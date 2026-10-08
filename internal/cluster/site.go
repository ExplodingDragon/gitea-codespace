// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	SiteUIDLabel         = "codespace.gitea.dev/site-uid"
	RuntimeUIDLabel      = "codespace.gitea.dev/runtime-uid"
	ComponentLabel       = "app.kubernetes.io/component"
	SiteFinalizer        = "codespace.gitea.dev/site-cleanup"
	imagePullSecretLabel = "codespace.gitea.dev/image-pull-secret"
)

type SiteReconciler struct {
	Client              client.Client
	ManagementNamespace string
	ImagePullSecrets    []string
	HTTPClient          *http.Client
}

func (r *SiteReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var site api.GiteaSite
	if err := r.Client.Get(ctx, request.NamespacedName, &site); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	namespace, err := api.SiteNamespace(site.Name, r.ManagementNamespace)
	if err != nil {
		return r.condition(ctx, &site, "InvalidConfiguration", err)
	}
	if !site.DeletionTimestamp.IsZero() {
		return r.deleteSite(ctx, &site, namespace)
	}
	if err := site.Validate(r.ManagementNamespace); err != nil {
		return r.condition(ctx, &site, "InvalidConfiguration", err)
	}
	if !controllerutil.ContainsFinalizer(&site, SiteFinalizer) {
		controllerutil.AddFinalizer(&site, SiteFinalizer)
		if err := r.Client.Update(ctx, &site); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	if err := r.ensureNamespace(ctx, &site, namespace); err != nil {
		return r.condition(ctx, &site, "NamespaceUnavailable", err)
	}
	if err := r.ensureSiteResources(ctx, &site, namespace); err != nil {
		return r.condition(ctx, &site, "ResourcesUnavailable", err)
	}
	if err := bindSiteCredential(ctx, r.Client, r.ManagementNamespace, &site); err != nil {
		return r.condition(ctx, &site, "CredentialUnavailable", err)
	}
	if !site.Spec.Enabled {
		return r.condition(ctx, &site, "Disabled", fmt.Errorf("site is disabled"))
	}
	var credential corev1.Secret
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.ManagementNamespace, Name: site.Spec.Credential.Name}, &credential); err != nil {
		return r.condition(ctx, &site, "CredentialUnavailable", fmt.Errorf("read site credential: %w", err))
	}
	if credential.UID != site.Spec.Credential.UID || credential.Labels[SiteUIDLabel] != string(site.UID) || len(credential.Data["managerSecret"]) == 0 {
		return r.condition(ctx, &site, "CredentialUnavailable", fmt.Errorf("site credential ownership or value is invalid"))
	}
	check := connect.NewRequest(&codespacev1.CheckManagerRequest{ProtocolVersion: 1})
	check.Header().Set("x-codespace-manager-id", fmt.Sprint(site.Spec.ManagerID))
	check.Header().Set("x-codespace-manager-secret", string(credential.Data["managerSecret"]))
	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	// Manager credentials in custom headers must never follow redirects.
	rpcClient := *httpClient
	rpcClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := codespacev1connect.NewManagerServiceClient(&rpcClient, strings.TrimRight(site.Spec.URL, "/")+"/api/codespace").CheckManager(ctx, check)
	if err != nil {
		return r.condition(ctx, &site, "IdentityUnavailable", fmt.Errorf("gitea identity check failed: %w", err))
	}
	canonical := strings.TrimRight(response.Msg.GiteaWebUrl, "/")
	canonicalURL, err := url.Parse(canonical)
	if err != nil || (canonicalURL.Scheme != "https" && canonicalURL.Scheme != "http") || canonicalURL.Hostname() == "" || canonicalURL.User != nil || canonicalURL.RawQuery != "" || canonicalURL.Fragment != "" {
		return r.condition(ctx, &site, "IdentityUnavailable", fmt.Errorf("gitea returned an invalid canonical URL"))
	}
	var sites api.GiteaSiteList
	if err := r.Client.List(ctx, &sites); err != nil {
		return ctrl.Result{}, err
	}
	for _, other := range sites.Items {
		if other.UID != site.UID && other.Spec.ManagerID == site.Spec.ManagerID && (strings.TrimRight(other.Status.CanonicalURL, "/") == canonical || strings.TrimRight(other.Spec.URL, "/") == canonical) {
			return r.condition(ctx, &site, "IdentityConflict", fmt.Errorf("gitea Manager identity is already referenced by site %s", other.Name))
		}
	}
	if site.Status.CanonicalURL != "" && site.Status.CanonicalURL != canonical {
		return r.condition(ctx, &site, "IdentityConflict", fmt.Errorf("canonical Gitea URL changed; review the site identity before continuing"))
	}
	site.Status.CanonicalURL = canonical
	return r.condition(ctx, &site, "IdentityVerified", nil)
}

func (r *SiteReconciler) condition(ctx context.Context, site *api.GiteaSite, reason string, problem error) (ctrl.Result, error) {
	condition := metav1.Condition{Type: "InfrastructureReady", Status: metav1.ConditionTrue, Reason: reason, Message: "Site resources and Gitea identity are verified", ObservedGeneration: site.Generation}
	if problem != nil {
		condition.Status = metav1.ConditionFalse
		condition.Message = problem.Error()
	}
	meta.SetStatusCondition(&site.Status.Conditions, condition)
	if err := api.ValidateObjectSize(site); err != nil {
		return ctrl.Result{}, err
	}
	var current api.GiteaSite
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(site), &current); err != nil {
		return ctrl.Result{}, err
	}
	if equality.Semantic.DeepEqual(current.Status, site.Status) {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if err := r.Client.Status().Update(ctx, site); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *SiteReconciler) ensureNamespace(ctx context.Context, site *api.GiteaSite, name string) error {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(namespace), namespace)
	if apierrors.IsNotFound(err) {
		if site.Status.NamespaceUID != "" {
			return fmt.Errorf("previous site namespace disappeared; explicit recovery is required")
		}
		namespace.Labels = map[string]string{SiteUIDLabel: string(site.UID)}
		if err = r.Client.Create(ctx, namespace); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if namespace.Labels[SiteUIDLabel] != string(site.UID) || (site.Status.NamespaceUID != "" && site.Status.NamespaceUID != namespace.UID) || !namespace.DeletionTimestamp.IsZero() {
		return fmt.Errorf("namespace %s ownership or lifecycle conflicts with site", name)
	}
	site.Status.NamespaceUID = namespace.UID
	return nil
}

func (r *SiteReconciler) ensureSiteResources(ctx context.Context, site *api.GiteaSite, namespace string) error {
	metadata := metav1.ObjectMeta{Name: "codespace", Namespace: namespace, Labels: map[string]string{SiteUIDLabel: string(site.UID)}}
	quota := &corev1.ResourceQuota{ObjectMeta: metadata, Spec: corev1.ResourceQuotaSpec{Hard: site.Spec.Quota}}
	limits := &corev1.LimitRange{ObjectMeta: metadata, Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{site.Spec.ContainerLimits}}}
	account := &corev1.ServiceAccount{ObjectMeta: metadata, AutomountServiceAccountToken: ptr.To(false)}
	policy := siteNetworkPolicy(site, namespace, r.ManagementNamespace)
	for _, object := range []client.Object{quota, limits, account, policy} {
		current := object.DeepCopyObject().(client.Object)
		err := r.Client.Get(ctx, client.ObjectKeyFromObject(object), current)
		if apierrors.IsNotFound(err) {
			if err := r.Client.Create(ctx, object); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if current.GetLabels()[SiteUIDLabel] != string(site.UID) {
			return fmt.Errorf("resource %s %s has another owner", object.GetObjectKind().GroupVersionKind().Kind, object.GetName())
		}
		before := current.DeepCopyObject().(client.Object)
		switch desired := object.(type) {
		case *corev1.ResourceQuota:
			current.(*corev1.ResourceQuota).Spec = desired.Spec
		case *corev1.LimitRange:
			current.(*corev1.LimitRange).Spec = desired.Spec
		case *corev1.ServiceAccount:
			current.(*corev1.ServiceAccount).AutomountServiceAccountToken = desired.AutomountServiceAccountToken
		case *networkingv1.NetworkPolicy:
			current.(*networkingv1.NetworkPolicy).Spec = desired.Spec
		}
		if equality.Semantic.DeepEqual(before, current) {
			continue
		}
		if err := r.Client.Update(ctx, current); err != nil {
			return err
		}
	}
	desiredPullSecrets := make(map[string]struct{}, len(r.ImagePullSecrets))
	for _, name := range r.ImagePullSecrets {
		desiredPullSecrets[name] = struct{}{}
		var source corev1.Secret
		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.ManagementNamespace, Name: name}, &source); err != nil {
			return fmt.Errorf("read image pull Secret %s: %w", name, err)
		}
		if source.Type != corev1.SecretTypeDockerConfigJson && source.Type != corev1.SecretTypeDockercfg {
			return fmt.Errorf("image pull Secret %s has unsupported type %s", name, source.Type)
		}
		copiedSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, copiedSecret, func() error {
			if owner := copiedSecret.Labels[SiteUIDLabel]; owner != "" && owner != string(site.UID) {
				return fmt.Errorf("image pull Secret %s has another owner", name)
			}
			copiedSecret.Labels = map[string]string{SiteUIDLabel: string(site.UID), imagePullSecretLabel: "true"}
			copiedSecret.Type = source.Type
			copiedSecret.Data = source.DeepCopy().Data
			return nil
		})
		if err != nil {
			return err
		}
	}
	var copiedSecrets corev1.SecretList
	if err := r.Client.List(ctx, &copiedSecrets, client.InNamespace(namespace), client.MatchingLabels{SiteUIDLabel: string(site.UID), imagePullSecretLabel: "true"}); err != nil {
		return err
	}
	for i := range copiedSecrets.Items {
		copiedSecret := &copiedSecrets.Items[i]
		if _, wanted := desiredPullSecrets[copiedSecret.Name]; wanted {
			continue
		}
		if err := r.Client.Delete(ctx, copiedSecret, client.Preconditions{UID: &copiedSecret.UID}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func siteNetworkPolicy(site *api.GiteaSite, namespace, managementNamespace string) *networkingv1.NetworkPolicy {
	management := &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": managementNamespace}}
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "codespace", Namespace: namespace, Labels: map[string]string{SiteUIDLabel: string(site.UID)}},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  []networkingv1.NetworkPolicyPeer{{NamespaceSelector: management, PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{ComponentLabel: "gateway"}}}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(8444))}},
			}},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: management, PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{ComponentLabel: "manager"}}}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(8443))}},
			}, {
				To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: management, PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{ComponentLabel: "cache"}}}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromString("service"))}},
			}, {
				To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}}, PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}}}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolUDP), Port: ptr.To(intstr.FromInt32(53))}, {Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(53))}},
			}},
		},
	}
	for _, target := range site.Spec.Upstreams {
		rule := networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: target.CIDR}}}}
		for _, port := range target.Ports {
			rule.Ports = append(rule.Ports, networkingv1.NetworkPolicyPort{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(port))})
		}
		policy.Spec.Egress = append(policy.Spec.Egress, rule)
	}
	return policy
}

func (r *SiteReconciler) deleteSite(ctx context.Context, site *api.GiteaSite, namespace string) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(site, SiteFinalizer) {
		return ctrl.Result{}, nil
	}
	var codespaces api.CodespaceList
	if err := r.Client.List(ctx, &codespaces, client.InNamespace(namespace)); err != nil {
		return ctrl.Result{}, err
	}
	if len(codespaces.Items) != 0 {
		return r.condition(ctx, site, "CleanupRequired", fmt.Errorf("site still contains %d codespaces", len(codespaces.Items)))
	}
	var ns corev1.Namespace
	err := r.Client.Get(ctx, types.NamespacedName{Name: namespace}, &ns)
	if err == nil {
		if ns.UID != site.Status.NamespaceUID || ns.Labels[SiteUIDLabel] != string(site.UID) {
			return r.condition(ctx, site, "NamespaceConflict", fmt.Errorf("namespace UID does not match site cleanup authorization"))
		}
		// A vanished CR alone is not authorization to destroy a retained volume.
		var volumes corev1.PersistentVolumeClaimList
		if err := r.Client.List(ctx, &volumes, client.InNamespace(namespace)); err != nil {
			return ctrl.Result{}, err
		}
		if len(volumes.Items) != 0 {
			return r.condition(ctx, site, "CleanupRequired", fmt.Errorf("site still contains persistent volumes"))
		}
		var pods corev1.PodList
		if err := r.Client.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
			return ctrl.Result{}, err
		}
		if len(pods.Items) != 0 {
			return r.condition(ctx, site, "CleanupRequired", fmt.Errorf("site still contains runtime Pods"))
		}
		if ns.DeletionTimestamp.IsZero() {
			err = r.Client.Delete(ctx, &ns, client.Preconditions{UID: &ns.UID})
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(site, SiteFinalizer)
	return ctrl.Result{}, r.Client.Update(ctx, site)
}
