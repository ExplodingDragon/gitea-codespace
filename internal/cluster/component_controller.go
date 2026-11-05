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
	policyv1 "k8s.io/api/policy/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
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
	gatewaySessionKey                     = "sessionKey"
)

func gatewayHostKeySecretName(componentName string) string {
	return componentName + "-ssh-host-key"
}

func cacheRegistryTLSSecretName(componentName string) string {
	return componentName + "-registry-tls"
}

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
	var cacheConfig *configpkg.CacheConfig
	if role == "gateway" {
		if _, err := gatewayComponentConfig(&component); err != nil {
			return ctrl.Result{}, err
		}
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
	registryTLSSecret := ""
	if role == "cache" {
		registryTLSSecret = cacheRegistryTLSSecretName(component.Name)
		dnsName := component.Name + "." + r.ManagementNamespace + ".svc"
		registryRenewAfter, err := reconcileServerCertificateSecret(ctx, r.Client, r.Identity, &component, r.ManagementNamespace, registryTLSSecret, dnsName,
			map[string]string{ComponentLabel: role, ComponentUIDLabel: string(component.UID)})
		if err != nil {
			return ctrl.Result{}, err
		}
		renewAfter = min(renewAfter, registryRenewAfter)
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
	if err := r.reconcileDeployment(ctx, &component, role, identitySecret, registryTLSSecret, credential, ports, cacheConfig); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileGatewayDisruptionBudget(ctx, &component); err != nil {
		return ctrl.Result{}, err
	}
	var exposureConditions []metav1.Condition
	if role == "gateway" {
		var err error
		exposureConditions, err = r.reconcileGatewayExposure(ctx, &component)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.reconcileNetworkPolicy(ctx, &component, role, ports); err != nil {
		return ctrl.Result{}, err
	}
	var deployment appsv1.Deployment
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.ManagementNamespace, Name: component.Name}, &deployment); err != nil {
		return ctrl.Result{}, err
	}
	desired := ptr.Deref(deployment.Spec.Replicas, int32(1))
	available := role == "gateway" && deployment.Status.ReadyReplicas > 0 || role == "cache" && deployment.Status.ReadyReplicas == 1
	condition := metav1.Condition{Type: "Available", Status: metav1.ConditionFalse, Reason: "DeploymentPending", Message: fmt.Sprintf("Component workload has %d of %d replicas ready", deployment.Status.ReadyReplicas, desired), ObservedGeneration: component.Generation}
	if available {
		condition.Status, condition.Reason = metav1.ConditionTrue, "DeploymentReady"
		if deployment.Status.ReadyReplicas < desired {
			condition.Reason = "DeploymentDegraded"
		}
		condition.Message = fmt.Sprintf("Component workload has %d of %d replicas ready", deployment.Status.ReadyReplicas, desired)
	}
	observedGeneration := component.Generation
	err = updateComponentStatus(ctx, r.Client, &component, func(current *api.CodespaceComponent) error {
		if current.Generation != observedGeneration {
			return errObservedResourceChanged
		}
		meta.SetStatusCondition(&current.Status.Conditions, condition)
		for _, exposure := range exposureConditions {
			meta.SetStatusCondition(&current.Status.Conditions, exposure)
		}
		current.Status.ObservedGeneration = observedGeneration
		return nil
	})
	if err != nil {
		return reconcileStatusError(err)
	}
	return ctrl.Result{RequeueAfter: min(renewAfter, componentReconcileInterval)}, nil
}

func (r *ComponentReconciler) reconcileComponentCredential(ctx context.Context, component *api.CodespaceComponent, role string) (*corev1.Secret, error) {
	name := component.Name + "-cache"
	if role == "gateway" {
		name = gatewayHostKeySecretName(component.Name)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.ManagementNamespace}}
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(secret), secret)
	if err == nil {
		if !metav1.IsControlledBy(secret, component) || secret.Labels[ComponentUIDLabel] != string(component.UID) {
			return nil, fmt.Errorf("component credential ownership conflicts")
		}
		if role != "gateway" {
			previous := secret.DeepCopy().Data
			secret.Data = updateProxySecretData(secret.Data, component.Spec.Cache.Proxy)
			if !apiequality.Semantic.DeepEqual(previous, secret.Data) {
				if err := r.Client.Update(ctx, secret); err != nil {
					return nil, err
				}
			}
			return secret, nil
		}
		if len(secret.Data[gatewaySessionKey]) == 32 {
			return secret, nil
		}
		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}
		sessionKey := make([]byte, 32)
		if _, err := rand.Read(sessionKey); err != nil {
			return nil, err
		}
		secret.Data[gatewaySessionKey] = sessionKey
		if err := r.Client.Update(ctx, secret); err != nil {
			return nil, err
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
		sessionKey := make([]byte, 32)
		if _, err := rand.Read(sessionKey); err != nil {
			return nil, err
		}
		secret.Data[gatewaySessionKey] = sessionKey
	} else {
		registryKey, tokenKey := make([]byte, 32), make([]byte, 32)
		if _, err := rand.Read(registryKey); err != nil {
			return nil, err
		}
		if _, err := rand.Read(tokenKey); err != nil {
			return nil, err
		}
		secret.Data = map[string][]byte{"registryKey": registryKey, "tokenKey": tokenKey, "s3AccessKey": {}, "s3SecretKey": {}}
		secret.Data = updateProxySecretData(secret.Data, component.Spec.Cache.Proxy)
	}
	if err := controllerutil.SetControllerReference(component, secret, r.Client.Scheme()); err != nil {
		return nil, err
	}
	if err := r.Client.Create(ctx, secret); err != nil {
		return nil, err
	}
	return secret, nil
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

func (r *ComponentReconciler) reconcileGatewayExposure(ctx context.Context, component *api.CodespaceComponent) ([]metav1.Condition, error) {
	gateway := component.Spec.Gateway
	httpCondition := metav1.Condition{Type: "HTTPExposureConfigured", Status: metav1.ConditionFalse, Reason: "ReconcilePending", Message: "HTTP exposure is pending", ObservedGeneration: component.Generation}
	sshCondition := metav1.Condition{Type: "SSHExposureConfigured", Status: metav1.ConditionFalse, Reason: "ReconcilePending", Message: "SSH exposure is pending", ObservedGeneration: component.Generation}

	ingress := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: r.ManagementNamespace}}
	if gateway.HTTPExposure.Mode == "external" {
		if err := r.deleteOwned(ctx, component, ingress); err != nil {
			return nil, err
		}
		httpCondition.Status, httpCondition.Reason, httpCondition.Message = metav1.ConditionTrue, "ExternalManagement", "HTTP exposure uses the component Service"
	} else {
		ready, err := r.reconcileGatewayIngress(ctx, component, ingress)
		if err != nil {
			return nil, err
		}
		if ready {
			httpCondition.Status, httpCondition.Reason, httpCondition.Message = metav1.ConditionTrue, "IngressConfigured", "HTTP Ingress is configured"
		} else {
			httpCondition.Reason, httpCondition.Message = "TLSSecretPending", "HTTP Ingress is waiting for its TLS Secret"
		}
	}

	loadBalancer := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: component.Name + "-ssh", Namespace: r.ManagementNamespace}}
	if gateway.SSHExposure.Mode == "external" {
		if err := r.deleteOwned(ctx, component, loadBalancer); err != nil {
			return nil, err
		}
		sshCondition.Status, sshCondition.Reason, sshCondition.Message = metav1.ConditionTrue, "ExternalManagement", "SSH exposure uses the component Service"
	} else {
		if err := r.reconcileGatewaySSHLoadBalancer(ctx, component, loadBalancer); err != nil {
			return nil, err
		}
		sshCondition.Status, sshCondition.Reason, sshCondition.Message = metav1.ConditionTrue, "LoadBalancerConfigured", "SSH LoadBalancer is configured"
	}
	return []metav1.Condition{httpCondition, sshCondition}, nil
}

func (r *ComponentReconciler) reconcileGatewayIngress(ctx context.Context, component *api.CodespaceComponent, ingress *networkingv1.Ingress) (bool, error) {
	spec := component.Spec.Gateway
	parsed, err := url.Parse(spec.EndpointBaseURL)
	if err != nil {
		return false, err
	}
	if err := gatewayIngressClassAvailable(ctx, r.Client, spec.HTTPExposure.Ingress.ClassName); err != nil {
		return false, err
	}
	host := "*." + strings.ToLower(parsed.Hostname())
	tlsSecret := ""
	annotations := map[string]string{}
	switch tls := spec.HTTPExposure.Ingress.TLS; tls.Source {
	case "secret":
		tlsSecret = tls.SecretName
	case "certManager":
		if err := gatewayIssuerAvailable(ctx, r.Client, r.ManagementNamespace, tls.Issuer); err != nil {
			return false, err
		}
		tlsSecret = component.Name + "-tls"
		if tls.Issuer.Kind == "Issuer" {
			annotations["cert-manager.io/issuer"] = tls.Issuer.Name
		} else {
			annotations["cert-manager.io/cluster-issuer"] = tls.Issuer.Name
		}
	}
	pathType := networkingv1.PathTypePrefix
	_, err = controllerutil.CreateOrPatch(ctx, r.Client, ingress, func() error {
		if err := controllerutil.SetControllerReference(component, ingress, r.Client.Scheme()); err != nil {
			return err
		}
		ingress.Labels = map[string]string{ComponentLabel: "gateway", ComponentUIDLabel: string(component.UID)}
		ingress.Annotations = annotations
		ingress.Spec = networkingv1.IngressSpec{
			IngressClassName: ptr.To(spec.HTTPExposure.Ingress.ClassName),
			Rules: []networkingv1.IngressRule{{Host: host, IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
				Path: "/", PathType: &pathType, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: component.Name, Port: networkingv1.ServiceBackendPort{Name: "http"}}},
			}}}}}},
		}
		if tlsSecret != "" {
			ingress.Spec.TLS = []networkingv1.IngressTLS{{Hosts: []string{host}, SecretName: tlsSecret}}
		}
		return nil
	})
	if err != nil || tlsSecret == "" {
		return err == nil, err
	}
	return gatewayTLSSecretReady(ctx, r.Client, r.ManagementNamespace, tlsSecret)
}

func (r *ComponentReconciler) reconcileGatewaySSHLoadBalancer(ctx context.Context, component *api.CodespaceComponent, service *corev1.Service) error {
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, service, func() error {
		if err := controllerutil.SetControllerReference(component, service, r.Client.Scheme()); err != nil {
			return err
		}
		service.Labels = map[string]string{ComponentLabel: "gateway", ComponentUIDLabel: string(component.UID)}
		service.Spec.Type = corev1.ServiceTypeLoadBalancer
		service.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
		service.Spec.Selector = map[string]string{ComponentLabel: "gateway", ComponentUIDLabel: string(component.UID)}
		service.Spec.Ports = []corev1.ServicePort{{Name: "ssh", Port: gatewaySSHServicePort, TargetPort: intstr.FromString("ssh")}}
		return nil
	})
	return err
}

func gatewayIngressClassAvailable(ctx context.Context, reader client.Reader, name string) error {
	var ingressClass networkingv1.IngressClass
	if err := reader.Get(ctx, types.NamespacedName{Name: name}, &ingressClass); err != nil {
		return fmt.Errorf("get IngressClass %q: %w", name, err)
	}
	return nil
}

func gatewayIssuerAvailable(ctx context.Context, reader client.Reader, namespace string, reference *api.GatewayIssuerReference) error {
	issuer := &unstructured.Unstructured{}
	issuer.SetAPIVersion("cert-manager.io/v1")
	issuer.SetKind(reference.Kind)
	key := types.NamespacedName{Name: reference.Name}
	if reference.Kind == "Issuer" {
		key.Namespace = namespace
	}
	if err := reader.Get(ctx, key, issuer); err != nil {
		return fmt.Errorf("get cert-manager %s %q: %w", reference.Kind, reference.Name, err)
	}
	return nil
}

func gatewayTLSSecretReady(ctx context.Context, reader client.Reader, namespace, name string) (bool, error) {
	var secret corev1.Secret
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &secret); apierrors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return secret.Type == corev1.SecretTypeTLS && len(secret.Data[corev1.TLSCertKey]) > 0 && len(secret.Data[corev1.TLSPrivateKeyKey]) > 0, nil
}

func (r *ComponentReconciler) reconcileService(ctx context.Context, component *api.CodespaceComponent, role string) ([]int32, error) {
	var ports []corev1.ServicePort
	var containerPorts []int32
	if role == "gateway" {
		containerPorts = []int32{gatewayHTTPPort, gatewaySSHPort}
		http := "http"
		ports = []corev1.ServicePort{
			{Name: "http", Port: gatewayHTTPServicePort, TargetPort: intstr.FromString("http"), AppProtocol: &http},
			{Name: "ssh", Port: gatewaySSHServicePort, TargetPort: intstr.FromString("ssh")},
		}
	} else {
		containerPorts = []int32{cacheContainerPort}
		https := "https"
		ports = []corev1.ServicePort{{Name: "https", Port: cacheServicePort, TargetPort: intstr.FromString("https"), AppProtocol: &https}}
	}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: r.ManagementNamespace}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, service, func() error {
		if err := controllerutil.SetControllerReference(component, service, r.Client.Scheme()); err != nil {
			return err
		}
		service.Labels = map[string]string{ComponentLabel: role, ComponentUIDLabel: string(component.UID)}
		service.Spec.Selector = map[string]string{ComponentLabel: role, ComponentUIDLabel: string(component.UID)}
		service.Spec.Type = corev1.ServiceTypeClusterIP
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
	_, err = controllerutil.CreateOrPatch(ctx, r.Client, claim, func() error {
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

func (r *ComponentReconciler) reconcileDeployment(ctx context.Context, component *api.CodespaceComponent, role, identitySecret, registryTLSSecret string, credential *corev1.Secret, ports []int32, cache *configpkg.CacheConfig) error {
	pullPolicy, err := platformImagePullPolicy(r.PlatformImage)
	if err != nil {
		return err
	}
	labels := map[string]string{"app.kubernetes.io/name": "codespace", ComponentLabel: role, ComponentUIDLabel: string(component.UID)}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: r.ManagementNamespace}}
	_, err = controllerutil.CreateOrPatch(ctx, r.Client, deployment, func() error {
		if err := controllerutil.SetControllerReference(component, deployment, r.Client.Scheme()); err != nil {
			return err
		}
		deployment.Labels = labels
		deployment.Spec.Replicas = ptr.To(int32(1))
		deployment.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
		var affinity *corev1.Affinity
		if role == "gateway" {
			deployment.Spec.Replicas = ptr.To(component.Spec.Gateway.Replicas)
			deployment.Spec.Strategy = appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: ptr.To(intstr.FromInt(0)),
					MaxSurge:       ptr.To(intstr.FromInt(1)),
				},
			}
			if component.Spec.Gateway.Replicas > 1 {
				affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
						TopologyKey:   corev1.LabelHostname,
					},
				}}}}
			}
		}
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
		if role == "cache" && component.Spec.Cache.Proxy != nil {
			container.Env = append(container.Env, proxyEnvironmentFromSecret(credential.Name)...)
			if len(credential.Data[proxyCAKey]) != 0 {
				container.Env = append(container.Env, corev1.EnvVar{Name: "CODESPACE_PROXY_CA_FILE", Value: "/var/run/codespace/cache/" + proxyCAKey})
			}
		}
		for index, port := range ports {
			name := "https"
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
				Items: []corev1.KeyToPath{{Key: "hostKey", Path: "ssh-host-key"}, {Key: gatewaySessionKey, Path: "session-key"}},
			}}})
		} else {
			items := []corev1.KeyToPath{{Key: "registryKey", Path: "registry-key"}, {Key: "tokenKey", Path: "token-key"}}
			if cache.Storage.Driver == "s3" {
				items = append(items, corev1.KeyToPath{Key: "s3AccessKey", Path: "s3-access-key"}, corev1.KeyToPath{Key: "s3SecretKey", Path: "s3-secret-key"})
			}
			if len(credential.Data[proxyCAKey]) != 0 {
				items = append(items, corev1.KeyToPath{Key: proxyCAKey, Path: proxyCAKey})
			}
			container.VolumeMounts = append(container.VolumeMounts,
				corev1.VolumeMount{Name: "cache-secret", MountPath: "/var/run/codespace/cache", ReadOnly: true},
				corev1.VolumeMount{Name: "registry-tls", MountPath: "/var/run/codespace/registry-tls", ReadOnly: true},
			)
			volumes = append(volumes, corev1.Volume{Name: "cache-secret", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: credential.Name, DefaultMode: ptr.To(int32(0o440)), Items: items}}})
			volumes = append(volumes, corev1.Volume{Name: "registry-tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: registryTLSSecret, DefaultMode: ptr.To(int32(0o440)),
				Items: []corev1.KeyToPath{{Key: identitypkg.TLSCertificateKey, Path: "tls.crt"}, {Key: identitypkg.TLSPrivateKeyKey, Path: "tls.key"}},
			}}})
			if cache.Storage.Driver == "filesystem" {
				container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "storage", MountPath: cache.Storage.Path})
				volumes = append(volumes, corev1.Volume{Name: "storage", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: component.Name}}})
			}
		}
		deployment.Spec.Template = corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{platformRevisionAnnotation: r.PlatformRevision, componentCredentialRevisionAnnotation: credential.ResourceVersion}}, Spec: corev1.PodSpec{
			AutomountServiceAccountToken: ptr.To(false), EnableServiceLinks: ptr.To(false), TerminationGracePeriodSeconds: ptr.To(int64(30)),
			ImagePullSecrets: localObjectReferences(r.ImagePullSecrets), Affinity: affinity,
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), RunAsGroup: ptr.To(int64(65532)), FSGroup: ptr.To(int64(65532)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers:      []corev1.Container{container}, Volumes: volumes,
		}}
		return nil
	})
	return err
}

func (r *ComponentReconciler) reconcileGatewayDisruptionBudget(ctx context.Context, component *api.CodespaceComponent) error {
	budget := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: r.ManagementNamespace}}
	if component.Spec.Role != "gateway" || component.Spec.Gateway.Replicas <= 1 {
		return r.deleteOwned(ctx, component, budget)
	}
	labels := map[string]string{"app.kubernetes.io/name": "codespace", ComponentLabel: "gateway", ComponentUIDLabel: string(component.UID)}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, budget, func() error {
		if err := controllerutil.SetControllerReference(component, budget, r.Client.Scheme()); err != nil {
			return err
		}
		budget.Labels = labels
		budget.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		budget.Spec.MinAvailable = nil
		budget.Spec.MaxUnavailable = ptr.To(intstr.FromInt(1))
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
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, policy, func() error {
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
