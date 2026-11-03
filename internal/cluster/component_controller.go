// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"net/url"
	"strings"
	"time"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	configpkg "gitea.dev/codespace/internal/config"
	identitypkg "gitea.dev/codespace/internal/identity"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// ComponentReconciler materializes one managed Gateway or Cache. The custom
// resource UID is the component identity, so a same-name replacement receives
// new credentials and cannot reuse the previous component session.
type ComponentReconciler struct {
	Client              client.Client
	Identity            *IdentityAuthority
	ManagementNamespace string
	ManagerURL          string
	ManagerIdentity     string
	PlatformImage       string
	PlatformRevision    string
	ImagePullSecrets    []string
}

const (
	platformRevisionAnnotation            = "codespace.gitea.dev/platform-revision"
	componentCredentialRevisionAnnotation = "codespace.gitea.dev/credential-revision"
	componentReconcileInterval            = 5 * time.Minute
)

func (r *ComponentReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	if request.Namespace != "" {
		return ctrl.Result{}, nil
	}
	var component api.CodespaceComponent
	if err := r.Client.Get(ctx, request.NamespacedName, &component); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !component.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if component.UID == "" || r.PlatformImage == "" || r.PlatformRevision == "" {
		return ctrl.Result{}, fmt.Errorf("component UID, platform image and platform revision are required")
	}
	if err := component.Validate(); err != nil {
		return ctrl.Result{}, err
	}
	role := component.Spec.Role
	var gatewayConfig *configpkg.GatewayConfig
	var cacheConfig *configpkg.CacheConfig
	if role == "gateway" {
		config, err := gatewayComponentConfig(&component)
		if err != nil {
			return ctrl.Result{}, err
		}
		gatewayConfig = &config
	} else {
		config, err := cacheComponentConfig(&component)
		if err != nil {
			return ctrl.Result{}, err
		}
		cacheConfig = &config
	}
	credential, err := r.reconcileComponentCredential(ctx, &component, role)
	if err != nil {
		return ctrl.Result{}, err
	}
	identitySecret := component.Name + "-identity"
	identityURI := identitypkg.ComponentURI(role, string(component.UID))
	renewAfter, err := reconcileIdentitySecret(ctx, r.Client, r.Identity, &component, r.ManagementNamespace, identitySecret, role, identityURI,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, map[string]string{ComponentLabel: role, ComponentUIDLabel: string(component.UID)})
	if err != nil {
		return ctrl.Result{}, err
	}
	ports, err := r.reconcileService(ctx, &component, role)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cacheConfig != nil && cacheConfig.Storage.Driver == "filesystem" {
		if err := r.reconcileCacheVolume(ctx, &component); err != nil {
			return ctrl.Result{}, err
		}
	} else if cacheConfig != nil {
		if err := r.deleteOwned(ctx, &component, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: r.ManagementNamespace}}); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.reconcileDeployment(ctx, &component, role, identitySecret, credential, ports, cacheConfig); err != nil {
		return ctrl.Result{}, err
	}
	if role == "gateway" && component.Spec.Gateway.Parent.Name != "" {
		if err := r.reconcileGatewayRoutes(ctx, &component, ports, gatewayConfig); err != nil {
			return ctrl.Result{}, err
		}
	} else if role == "gateway" {
		for _, routeType := range []struct{ version, kind string }{{"v1", "HTTPRoute"}, {"v1alpha2", "TCPRoute"}} {
			route := &unstructured.Unstructured{}
			route.SetAPIVersion("gateway.networking.k8s.io/" + routeType.version)
			route.SetKind(routeType.kind)
			route.SetName(component.Name)
			route.SetNamespace(r.ManagementNamespace)
			if err := r.deleteOwned(ctx, &component, route); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	if err := r.reconcileNetworkPolicy(ctx, &component, role, ports); err != nil {
		return ctrl.Result{}, err
	}
	var deployment appsv1.Deployment
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.ManagementNamespace, Name: component.Name}, &deployment); err != nil {
		return ctrl.Result{}, err
	}
	available := deployment.Status.ReadyReplicas == 1
	condition := metav1.Condition{Type: "Available", Status: metav1.ConditionFalse, Reason: "DeploymentPending", Message: "Component workload is not ready", ObservedGeneration: component.Generation}
	if available {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionTrue, "DeploymentReady", "Component workload is ready"
	}
	changed := meta.SetStatusCondition(&component.Status.Conditions, condition)
	if component.Status.ObservedGeneration != component.Generation {
		component.Status.ObservedGeneration = component.Generation
		changed = true
	}
	if changed {
		if err := r.Client.Status().Update(ctx, &component); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: min(renewAfter, componentReconcileInterval)}, nil
}

func (r *ComponentReconciler) reconcileComponentCredential(ctx context.Context, component *api.CodespaceComponent, role string) (*corev1.Secret, error) {
	name := component.Name + "-cache"
	if role == "gateway" {
		name = component.Name + "-ssh-host-key"
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.ManagementNamespace}}
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(secret), secret)
	if err == nil {
		if !metav1.IsControlledBy(secret, component) || secret.Labels[ComponentUIDLabel] != string(component.UID) {
			return nil, fmt.Errorf("component credential ownership conflicts")
		}
		return secret, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	secret.Labels = map[string]string{ComponentUIDLabel: string(component.UID)}
	if role == "gateway" {
		generated, err := newGatewayHostKeySecret(name, r.ManagementNamespace)
		if err != nil {
			return nil, err
		}
		secret.Data = generated.Data
	} else {
		registryKey, tokenKey := make([]byte, 32), make([]byte, 32)
		if _, err := rand.Read(registryKey); err != nil {
			return nil, err
		}
		if _, err := rand.Read(tokenKey); err != nil {
			return nil, err
		}
		secret.Data = map[string][]byte{"registryKey": registryKey, "tokenKey": tokenKey, "s3AccessKey": {}, "s3SecretKey": {}}
	}
	if err := controllerutil.SetControllerReference(component, secret, r.Client.Scheme()); err != nil {
		return nil, err
	}
	if err := r.Client.Create(ctx, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

func (r *ComponentReconciler) reconcileGatewayRoutes(ctx context.Context, component *api.CodespaceComponent, ports []int32, config *configpkg.GatewayConfig) error {
	if len(ports) != 2 {
		return fmt.Errorf("gateway routes require HTTP and SSH Service ports")
	}
	publicURL, err := url.Parse(config.HTTP.PublicURL)
	if err != nil || publicURL.Hostname() == "" {
		return fmt.Errorf("gateway public URL is invalid")
	}
	parent := component.Spec.Gateway.Parent
	if err := validateGatewayParent(ctx, r.Client, parent); err != nil {
		return err
	}
	parentRef := map[string]any{
		"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": parent.Name, "namespace": parent.Namespace,
	}
	parentRef["sectionName"] = parent.HTTPSectionName
	host := strings.ToLower(publicURL.Hostname())
	httpRoute := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
		"metadata": map[string]any{"name": component.Name, "namespace": r.ManagementNamespace},
		"spec": map[string]any{
			"parentRefs": []any{parentRef}, "hostnames": []any{host, "*." + host},
			"rules": []any{map[string]any{"backendRefs": []any{map[string]any{"group": "", "kind": "Service", "name": component.Name, "port": int64(ports[0])}}}},
		},
	}}
	if err := r.reconcileRoute(ctx, component, httpRoute); err != nil {
		return err
	}
	if parent.SSHSectionName == "" {
		tcpRoute := &unstructured.Unstructured{}
		tcpRoute.SetAPIVersion("gateway.networking.k8s.io/v1alpha2")
		tcpRoute.SetKind("TCPRoute")
		tcpRoute.SetName(component.Name)
		tcpRoute.SetNamespace(r.ManagementNamespace)
		return r.deleteOwned(ctx, component, tcpRoute)
	}
	sshParentRef := map[string]any{
		"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": parent.Name, "namespace": parent.Namespace, "sectionName": parent.SSHSectionName,
	}
	tcpRoute := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1alpha2", "kind": "TCPRoute",
		"metadata": map[string]any{"name": component.Name, "namespace": r.ManagementNamespace},
		"spec": map[string]any{
			"parentRefs": []any{sshParentRef},
			"rules":      []any{map[string]any{"backendRefs": []any{map[string]any{"group": "", "kind": "Service", "name": component.Name, "port": int64(ports[1])}}}},
		},
	}}
	return r.reconcileRoute(ctx, component, tcpRoute)
}

func validateGatewayParent(ctx context.Context, reader client.Reader, parent api.GatewayParent) error {
	parentResource := &unstructured.Unstructured{}
	parentResource.SetAPIVersion("gateway.networking.k8s.io/v1")
	parentResource.SetKind("Gateway")
	if err := reader.Get(ctx, types.NamespacedName{Namespace: parent.Namespace, Name: parent.Name}, parentResource); err != nil {
		return fmt.Errorf("get parent Gateway: %w", err)
	}
	listeners, found, err := unstructured.NestedSlice(parentResource.Object, "spec", "listeners")
	if err != nil || !found {
		return fmt.Errorf("parent Gateway has no listeners")
	}
	required := map[string]map[string]bool{parent.HTTPSectionName: {"HTTP": true, "HTTPS": true}}
	if parent.SSHSectionName != "" {
		required[parent.SSHSectionName] = map[string]bool{"TCP": true}
	}
	for _, raw := range listeners {
		listener, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := listener["name"].(string)
		protocol, _ := listener["protocol"].(string)
		if protocols, ok := required[name]; ok && protocols[protocol] {
			delete(required, name)
		}
	}
	for name := range required {
		return fmt.Errorf("parent Gateway listener %s is missing or uses an incompatible protocol", name)
	}
	return nil
}

func (r *ComponentReconciler) deleteOwned(ctx context.Context, component *api.CodespaceComponent, object client.Object) error {
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(object), object)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(object, component) {
		return fmt.Errorf("component %T ownership conflicts", object)
	}
	return client.IgnoreNotFound(r.Client.Delete(ctx, object))
}

func (r *ComponentReconciler) reconcileRoute(ctx context.Context, component *api.CodespaceComponent, desired *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(desired.GroupVersionKind())
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		desired.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(component, api.GroupVersion.WithKind("CodespaceComponent"))})
		return r.Client.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(existing, component) {
		return fmt.Errorf("component %s ownership conflicts", desired.GetKind())
	}
	existing.Object["spec"] = desired.Object["spec"]
	return r.Client.Update(ctx, existing)
}

func (r *ComponentReconciler) reconcileService(ctx context.Context, component *api.CodespaceComponent, role string) ([]int32, error) {
	var ports []corev1.ServicePort
	var containerPorts []int32
	if role == "gateway" {
		containerPorts = []int32{gatewayHTTPPort, gatewaySSHPort}
		ports = []corev1.ServicePort{{Name: "http", Port: gatewayHTTPPort, TargetPort: intstr.FromInt32(gatewayHTTPPort)}, {Name: "ssh", Port: gatewaySSHPort, TargetPort: intstr.FromInt32(gatewaySSHPort)}}
	} else {
		containerPorts = []int32{cachePort}
		ports = []corev1.ServicePort{{Name: "registry", Port: cachePort, TargetPort: intstr.FromInt32(cachePort)}}
	}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: r.ManagementNamespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		if err := controllerutil.SetControllerReference(component, service, r.Client.Scheme()); err != nil {
			return err
		}
		service.Labels = map[string]string{ComponentLabel: role, ComponentUIDLabel: string(component.UID)}
		service.Spec.Selector = map[string]string{ComponentLabel: role, ComponentUIDLabel: string(component.UID)}
		service.Spec.Ports = ports
		return nil
	})
	return containerPorts, err
}

func (r *ComponentReconciler) reconcileCacheVolume(ctx context.Context, component *api.CodespaceComponent) error {
	storage := component.Spec.Cache.Storage
	volumeSize, err := resource.ParseQuantity(storage.VolumeSize)
	if err != nil || volumeSize.Sign() <= 0 {
		return fmt.Errorf("cache volume size must be a positive Kubernetes quantity")
	}
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: r.ManagementNamespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, claim, func() error {
		if err := controllerutil.SetControllerReference(component, claim, r.Client.Scheme()); err != nil {
			return err
		}
		claim.Labels = map[string]string{ComponentLabel: "cache", ComponentUIDLabel: string(component.UID)}
		if claim.Spec.Resources.Requests == nil {
			claim.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
			claim.Spec.Resources.Requests = corev1.ResourceList{}
			if storage.StorageClassName != "" {
				claim.Spec.StorageClassName = ptr.To(storage.StorageClassName)
			}
		}
		claim.Spec.Resources.Requests[corev1.ResourceStorage] = volumeSize
		return nil
	})
	return err
}

func (r *ComponentReconciler) reconcileDeployment(ctx context.Context, component *api.CodespaceComponent, role, identitySecret string, credential *corev1.Secret, ports []int32, cache *configpkg.CacheConfig) error {
	pullPolicy, err := platformImagePullPolicy(r.PlatformImage)
	if err != nil {
		return err
	}
	labels := map[string]string{"app.kubernetes.io/name": "codespace", ComponentLabel: role, ComponentUIDLabel: string(component.UID)}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: r.ManagementNamespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		if err := controllerutil.SetControllerReference(component, deployment, r.Client.Scheme()); err != nil {
			return err
		}
		deployment.Labels = labels
		deployment.Spec.Replicas = ptr.To(int32(1))
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		args := []string{role}
		if role == "cache" {
			args = append(args, "--id", component.Name)
		}
		container := corev1.Container{
			Name: role, Image: r.PlatformImage, ImagePullPolicy: pullPolicy, Args: args,
			Env: []corev1.EnvVar{
				{Name: "GITEA_CODESPACE_MANAGER_URL", Value: r.ManagerURL},
				{Name: "GITEA_CODESPACE_MANAGER_IDENTITY", Value: r.ManagerIdentity},
				{Name: "GITEA_CODESPACE_CERTIFICATE_DIRECTORY", Value: "/var/run/codespace/identity"},
			},
			Resources:       component.Spec.Resources,
			SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
			VolumeMounts:    []corev1.VolumeMount{{Name: "identity", MountPath: "/var/run/codespace/identity", ReadOnly: true}},
		}
		for index, port := range ports {
			name := "service"
			if role == "gateway" {
				name = []string{"http", "ssh"}[index]
			}
			container.Ports = append(container.Ports, corev1.ContainerPort{Name: name, ContainerPort: port})
		}
		probe := corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString(container.Ports[0].Name)}}
		if role == "gateway" {
			probe = corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/api/readyz", Port: intstr.FromString(container.Ports[0].Name)}}
		}
		container.ReadinessProbe = &corev1.Probe{
			ProbeHandler:     probe,
			PeriodSeconds:    1,
			TimeoutSeconds:   1,
			FailureThreshold: 30,
			SuccessThreshold: 1,
		}
		volumes := []corev1.Volume{{Name: "identity", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: identitySecret, DefaultMode: ptr.To(int32(0o440))}}}}
		if role == "gateway" {
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "ssh-host-key", MountPath: "/var/run/codespace/gateway", ReadOnly: true})
			volumes = append(volumes, corev1.Volume{Name: "ssh-host-key", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: credential.Name, DefaultMode: ptr.To(int32(0o440)),
				Items: []corev1.KeyToPath{{Key: "hostKey", Path: "ssh-host-key"}},
			}}})
		} else {
			items := []corev1.KeyToPath{{Key: "registryKey", Path: "registry-key"}, {Key: "tokenKey", Path: "token-key"}}
			if cache.Storage.Driver == "s3" {
				items = append(items, corev1.KeyToPath{Key: "s3AccessKey", Path: "s3-access-key"}, corev1.KeyToPath{Key: "s3SecretKey", Path: "s3-secret-key"})
			}
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "cache-secret", MountPath: "/var/run/codespace/cache", ReadOnly: true})
			volumes = append(volumes, corev1.Volume{Name: "cache-secret", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: credential.Name, DefaultMode: ptr.To(int32(0o440)), Items: items}}})
			if cache.Storage.Driver == "filesystem" {
				container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "storage", MountPath: cache.Storage.Path})
				volumes = append(volumes, corev1.Volume{Name: "storage", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: component.Name}}})
			}
		}
		deployment.Spec.Template = corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{platformRevisionAnnotation: r.PlatformRevision, componentCredentialRevisionAnnotation: credential.ResourceVersion}}, Spec: corev1.PodSpec{
			AutomountServiceAccountToken: ptr.To(false), EnableServiceLinks: ptr.To(false), TerminationGracePeriodSeconds: ptr.To(int64(30)),
			ImagePullSecrets: localObjectReferences(r.ImagePullSecrets),
			SecurityContext:  &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), RunAsGroup: ptr.To(int64(65532)), FSGroup: ptr.To(int64(65532)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers:       []corev1.Container{container}, Volumes: volumes,
		}}
		return nil
	})
	return err
}

func localObjectReferences(names []string) []corev1.LocalObjectReference {
	references := make([]corev1.LocalObjectReference, len(names))
	for i, name := range names {
		references[i].Name = name
	}
	return references
}

func (r *ComponentReconciler) reconcileNetworkPolicy(ctx context.Context, component *api.CodespaceComponent, role string, ports []int32) error {
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: r.ManagementNamespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		if err := controllerutil.SetControllerReference(component, policy, r.Client.Scheme()); err != nil {
			return err
		}
		labels := map[string]string{ComponentLabel: role, ComponentUIDLabel: string(component.UID)}
		policy.Spec.PodSelector = metav1.LabelSelector{MatchLabels: labels}
		policy.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
		ingressPorts := make([]networkingv1.NetworkPolicyPort, 0, len(ports))
		for _, port := range ports {
			ingressPorts = append(ingressPorts, networkingv1.NetworkPolicyPort{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(port))})
		}
		rule := networkingv1.NetworkPolicyIngressRule{Ports: ingressPorts}
		if role == "cache" {
			rule.From = []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: SiteUIDLabel, Operator: metav1.LabelSelectorOpExists}}}}}
		}
		policy.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{rule}
		return nil
	})
	return err
}
