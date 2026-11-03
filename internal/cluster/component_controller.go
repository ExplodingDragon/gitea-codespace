// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	configpkg "gitea.dev/codespace/internal/config"
	identitypkg "gitea.dev/codespace/internal/identity"
	dockerunits "github.com/docker/go-units"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// ComponentReconciler materializes one managed Gateway or Cache configuration.
// ConfigMap UID is the authority boundary, so a same-name replacement receives
// a new certificate identity and cannot reuse the previous component session.
type ComponentReconciler struct {
	Client                 client.Client
	Identity               *IdentityAuthority
	ManagementNamespace    string
	ManagerURL             string
	ManagerIdentity        string
	PlatformImage          string
	PlatformRevision       string
	ImagePullSecrets       []string
	GatewayParentName      string
	GatewayParentNamespace string
	GatewayHTTPSectionName string
	GatewaySSHSectionName  string
}

const componentFinalizer = "codespace.gitea.dev/component-cleanup"

const platformRevisionAnnotation = "codespace.gitea.dev/platform-revision"

func (r *ComponentReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	if request.Namespace != r.ManagementNamespace {
		return ctrl.Result{}, nil
	}
	var component corev1.ConfigMap
	if err := r.Client.Get(ctx, request.NamespacedName, &component); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	role := component.Labels[ComponentLabel]
	if role != "gateway" && role != "cache" {
		return ctrl.Result{}, nil
	}
	if !component.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&component, componentFinalizer) {
			return ctrl.Result{}, nil
		}
		deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: component.Namespace}}
		deploymentExists := false
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(deployment), deployment); err == nil {
			if !metav1.IsControlledBy(deployment, &component) {
				return ctrl.Result{}, fmt.Errorf("component Deployment ownership conflicts")
			}
			deploymentExists = true
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		identity := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: component.Name + "-identity", Namespace: component.Namespace}}
		identityExists := false
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(identity), identity); err == nil {
			if !metav1.IsControlledBy(identity, &component) {
				return ctrl.Result{}, fmt.Errorf("component identity Secret ownership conflicts")
			}
			identityExists = true
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if deploymentExists {
			if err := r.Client.Delete(ctx, deployment); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		if identityExists {
			if err := r.Client.Delete(ctx, identity); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		controllerutil.RemoveFinalizer(&component, componentFinalizer)
		return ctrl.Result{}, r.Client.Update(ctx, &component)
	}
	if !controllerutil.ContainsFinalizer(&component, componentFinalizer) {
		controllerutil.AddFinalizer(&component, componentFinalizer)
		if err := r.Client.Update(ctx, &component); err != nil {
			return ctrl.Result{}, err
		}
	}
	if component.UID == "" || r.PlatformImage == "" || r.PlatformRevision == "" {
		return ctrl.Result{}, fmt.Errorf("component UID, platform image and platform revision are required")
	}
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
	if err := bindComponentCredential(ctx, r.Client, &component); err != nil {
		return ctrl.Result{}, fmt.Errorf("bind component credential: %w", err)
	}
	identitySecret := component.Name + "-identity"
	identityURI := identitypkg.ComponentURI(role, string(component.UID))
	renewAfter, err := reconcileIdentitySecret(ctx, r.Client, r.Identity, &component, identitySecret, role, identityURI,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, map[string]string{ComponentLabel: role, ComponentUIDLabel: string(component.UID)})
	if err != nil {
		return ctrl.Result{}, err
	}
	ports, err := r.reconcileService(ctx, &component, role, gatewayConfig, cacheConfig)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cacheConfig != nil && cacheConfig.Storage.Driver == "filesystem" {
		if err := r.reconcileCacheVolume(ctx, &component, cacheConfig.MaxSize); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.reconcileDeployment(ctx, &component, role, identitySecret, ports, cacheConfig); err != nil {
		return ctrl.Result{}, err
	}
	if role == "gateway" && r.GatewayParentName != "" {
		if err := r.reconcileGatewayRoutes(ctx, &component, ports, gatewayConfig); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.reconcileNetworkPolicy(ctx, &component, role, ports); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: renewAfter}, nil
}

func (r *ComponentReconciler) reconcileGatewayRoutes(ctx context.Context, component *corev1.ConfigMap, ports []int32, config *configpkg.GatewayConfig) error {
	if len(ports) != 2 {
		return fmt.Errorf("gateway routes require HTTP and SSH Service ports")
	}
	publicURL, err := url.Parse(config.HTTP.PublicURL)
	if err != nil || publicURL.Hostname() == "" {
		return fmt.Errorf("gateway public URL is invalid")
	}
	parentNamespace := r.GatewayParentNamespace
	if parentNamespace == "" {
		parentNamespace = r.ManagementNamespace
	}
	parentRef := map[string]any{
		"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": r.GatewayParentName, "namespace": parentNamespace,
	}
	if r.GatewayHTTPSectionName != "" {
		parentRef["sectionName"] = r.GatewayHTTPSectionName
	}
	host := strings.ToLower(publicURL.Hostname())
	httpRoute := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
		"metadata": map[string]any{"name": component.Name, "namespace": component.Namespace},
		"spec": map[string]any{
			"parentRefs": []any{parentRef}, "hostnames": []any{host, "*." + host},
			"rules": []any{map[string]any{"backendRefs": []any{map[string]any{"group": "", "kind": "Service", "name": component.Name, "port": int64(ports[0])}}}},
		},
	}}
	if err := r.reconcileRoute(ctx, component, httpRoute); err != nil {
		return err
	}
	if r.GatewaySSHSectionName == "" {
		return nil
	}
	sshParentRef := map[string]any{
		"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": r.GatewayParentName, "namespace": parentNamespace, "sectionName": r.GatewaySSHSectionName,
	}
	tcpRoute := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1alpha2", "kind": "TCPRoute",
		"metadata": map[string]any{"name": component.Name, "namespace": component.Namespace},
		"spec": map[string]any{
			"parentRefs": []any{sshParentRef},
			"rules":      []any{map[string]any{"backendRefs": []any{map[string]any{"group": "", "kind": "Service", "name": component.Name, "port": int64(ports[1])}}}},
		},
	}}
	return r.reconcileRoute(ctx, component, tcpRoute)
}

func (r *ComponentReconciler) reconcileRoute(ctx context.Context, component *corev1.ConfigMap, desired *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(desired.GroupVersionKind())
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		desired.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(component, corev1.SchemeGroupVersion.WithKind("ConfigMap"))})
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

func componentListenPort(value string) (int32, error) {
	_, raw, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return 0, err
	}
	port, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("listen port is invalid")
	}
	return int32(port), nil
}

func (r *ComponentReconciler) reconcileService(ctx context.Context, component *corev1.ConfigMap, role string, gateway *configpkg.GatewayConfig, cache *configpkg.CacheConfig) ([]int32, error) {
	ports := []corev1.ServicePort{}
	var containerPorts []int32
	if role == "gateway" {
		httpPort, err := componentListenPort(gateway.HTTP.Listen)
		if err != nil {
			return nil, fmt.Errorf("gateway HTTP listen address: %w", err)
		}
		sshPort, err := componentListenPort(gateway.SSH.Listen)
		if err != nil {
			return nil, fmt.Errorf("gateway SSH listen address: %w", err)
		}
		containerPorts = []int32{httpPort, sshPort}
		ports = []corev1.ServicePort{{Name: "http", Port: httpPort, TargetPort: intstr.FromInt32(httpPort)}, {Name: "ssh", Port: sshPort, TargetPort: intstr.FromInt32(sshPort)}}
	} else {
		port, err := componentListenPort(cache.Listen)
		if err != nil {
			return nil, fmt.Errorf("cache listen address: %w", err)
		}
		containerPorts = []int32{port}
		ports = []corev1.ServicePort{{Name: "registry", Port: port, TargetPort: intstr.FromInt32(port)}}
	}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: component.Namespace}}
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

func (r *ComponentReconciler) reconcileCacheVolume(ctx context.Context, component *corev1.ConfigMap, maxSize string) error {
	storageBytes := int64(10 << 30)
	if strings.TrimSpace(maxSize) != "" {
		parsed, err := dockerunits.RAMInBytes(maxSize)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("cache max size is invalid")
		}
		storageBytes = parsed
	}
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: component.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, claim, func() error {
		if err := controllerutil.SetControllerReference(component, claim, r.Client.Scheme()); err != nil {
			return err
		}
		claim.Labels = map[string]string{ComponentLabel: "cache", ComponentUIDLabel: string(component.UID)}
		if claim.Spec.Resources.Requests == nil {
			claim.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
			claim.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: *resource.NewQuantity(storageBytes, resource.BinarySI)}
		}
		return nil
	})
	return err
}

func (r *ComponentReconciler) reconcileDeployment(ctx context.Context, component *corev1.ConfigMap, role, identitySecret string, ports []int32, cache *configpkg.CacheConfig) error {
	pullPolicy, err := platformImagePullPolicy(r.PlatformImage)
	if err != nil {
		return err
	}
	labels := map[string]string{"app.kubernetes.io/name": "codespace", ComponentLabel: role, ComponentUIDLabel: string(component.UID)}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: component.Namespace}}
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
			Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")}},
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
			keySecret := component.Data["sshHostKeySecret"]
			if keySecret == "" {
				return fmt.Errorf("gateway SSH host key Secret is required")
			}
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "ssh-host-key", MountPath: "/var/run/codespace/gateway", ReadOnly: true})
			volumes = append(volumes, corev1.Volume{Name: "ssh-host-key", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: keySecret, DefaultMode: ptr.To(int32(0o440)),
				Items: []corev1.KeyToPath{{Key: "hostKey", Path: "ssh-host-key"}},
			}}})
		} else {
			secretName := component.Data["secretName"]
			if secretName == "" {
				return fmt.Errorf("cache Secret is required")
			}
			items := []corev1.KeyToPath{{Key: "registryKey", Path: "registry-key"}, {Key: "tokenKey", Path: "token-key"}}
			if cache.Storage.Driver == "s3" {
				items = append(items, corev1.KeyToPath{Key: "s3AccessKey", Path: "s3-access-key"}, corev1.KeyToPath{Key: "s3SecretKey", Path: "s3-secret-key"})
			}
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "cache-secret", MountPath: "/var/run/codespace/cache", ReadOnly: true})
			volumes = append(volumes, corev1.Volume{Name: "cache-secret", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secretName, DefaultMode: ptr.To(int32(0o440)), Items: items}}})
			if cache.Storage.Driver == "filesystem" {
				container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "storage", MountPath: cache.Storage.Path})
				volumes = append(volumes, corev1.Volume{Name: "storage", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: component.Name}}})
			}
		}
		deployment.Spec.Template = corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{platformRevisionAnnotation: r.PlatformRevision}}, Spec: corev1.PodSpec{
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

func (r *ComponentReconciler) reconcileNetworkPolicy(ctx context.Context, component *corev1.ConfigMap, role string, ports []int32) error {
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: component.Namespace}}
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
