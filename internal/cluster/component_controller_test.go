// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"gitea.dev/codespace/internal/healthcheck"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestComponentReconcilerMaterializesGatewayAndCache(t *testing.T) {
	ctx := t.Context()
	namespace := "codespace-system"
	gateway := testGatewayComponent("gateway", "gateway-uid")
	gateway.Spec.Gateway.HTTPExposure = api.GatewayHTTPExposure{Mode: "ingress", Ingress: &api.GatewayIngressExposure{ClassName: "nginx", TLS: api.GatewayIngressTLS{Source: "secret", SecretName: "wildcard-tls"}}}
	gateway.Spec.Gateway.SSHExposure = api.GatewaySSHExposure{Mode: "loadBalancer"}
	cache := testCacheComponent("cache", "cache-uid")
	ingressClass := &networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "nginx"}, Spec: networkingv1.IngressClassSpec{Controller: "k8s.io/ingress-nginx"}}
	tlsSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "wildcard-tls", Namespace: namespace}, Type: corev1.SecretTypeTLS, Data: map[string][]byte{corev1.TLSCertKey: []byte("certificate"), corev1.TLSPrivateKeyKey: []byte("private-key")}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(gateway, cache).WithObjects(gateway, cache, ingressClass, tlsSecret).Build()
	reconciler := &ComponentReconciler{
		Client: c, Identity: newTestIdentityAuthority(t), ManagementNamespace: namespace, ManagerURL: "https://manager.codespace-system.svc:8443", ManagerIdentity: "spiffe://codespace/manager/namespace-uid", PlatformImage: "localhost/codespace@sha256:" + strings.Repeat("a", 64),
		PlatformRevision: "sha-test", ImagePullSecrets: []string{"registry"},
	}

	for _, component := range []*api.CodespaceComponent{gateway, cache} {
		result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(component)})
		require.NoError(t, err)
		require.Equal(t, componentReconcileInterval, result.RequeueAfter)

		key := types.NamespacedName{Namespace: namespace, Name: component.Name}
		var service corev1.Service
		require.NoError(t, c.Get(ctx, key, &service))
		require.Equal(t, string(component.UID), service.Spec.Selector[ComponentUIDLabel])
		if component.Spec.Role == "gateway" {
			require.Equal(t, corev1.ServiceTypeClusterIP, service.Spec.Type)
			require.Equal(t, []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromString("http"), AppProtocol: ptr.To("http")}, {Name: "ssh", Port: 22, TargetPort: intstr.FromString("ssh")}}, service.Spec.Ports)
		} else {
			require.Equal(t, []corev1.ServicePort{{Name: "https", Port: 443, TargetPort: intstr.FromString("https"), AppProtocol: ptr.To("https")}}, service.Spec.Ports)
		}

		var deployment appsv1.Deployment
		require.NoError(t, c.Get(ctx, key, &deployment))
		require.NotNil(t, deployment.Spec.Template.Spec.AutomountServiceAccountToken)
		require.False(t, *deployment.Spec.Template.Spec.AutomountServiceAccountToken)
		require.True(t, *deployment.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem)
		require.Equal(t, component.Spec.Resources, deployment.Spec.Template.Spec.Containers[0].Resources)
		require.Equal(t, corev1.PullIfNotPresent, deployment.Spec.Template.Spec.Containers[0].ImagePullPolicy)
		require.Equal(t, []corev1.LocalObjectReference{{Name: "registry"}}, deployment.Spec.Template.Spec.ImagePullSecrets)
		require.Equal(t, string(component.UID), deployment.Spec.Template.Labels[ComponentUIDLabel])
		require.Equal(t, "sha-test", deployment.Spec.Template.Annotations[platformRevisionAnnotation])
		container := deployment.Spec.Template.Spec.Containers[0]
		require.Contains(t, container.Env, corev1.EnvVar{Name: healthcheck.AddressEnvironment, Value: healthcheck.DefaultAddress})
		require.Contains(t, container.Ports, corev1.ContainerPort{Name: "health", ContainerPort: healthcheck.DefaultPort})
		require.Equal(t, "/readyz", container.ReadinessProbe.HTTPGet.Path)
		require.Equal(t, intstr.FromString("health"), container.ReadinessProbe.HTTPGet.Port)
		require.Equal(t, int32(2), container.ReadinessProbe.PeriodSeconds)
		require.Equal(t, "/healthz", container.LivenessProbe.HTTPGet.Path)
		require.Equal(t, intstr.FromString("health"), container.LivenessProbe.HTTPGet.Port)
		require.Equal(t, int32(10), container.LivenessProbe.PeriodSeconds)
		if component.Spec.Role == "gateway" {
			require.Equal(t, int32(2), ptr.Deref(deployment.Spec.Replicas, int32(0)))
			require.Equal(t, appsv1.RollingUpdateDeploymentStrategyType, deployment.Spec.Strategy.Type)
			require.Equal(t, intstr.FromInt(0), *deployment.Spec.Strategy.RollingUpdate.MaxUnavailable)
			require.Equal(t, intstr.FromInt(1), *deployment.Spec.Strategy.RollingUpdate.MaxSurge)
			require.Len(t, deployment.Spec.Template.Spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution, 1)
		} else {
			require.Contains(t, container.Ports, corev1.ContainerPort{Name: cacheHTTPSPortName, ContainerPort: 8443})
			require.Equal(t, int32(1), ptr.Deref(deployment.Spec.Replicas, int32(0)))
			require.Equal(t, appsv1.RecreateDeploymentStrategyType, deployment.Spec.Strategy.Type)
			require.Nil(t, deployment.Spec.Strategy.RollingUpdate)
		}

		var policy networkingv1.NetworkPolicy
		require.NoError(t, c.Get(ctx, key, &policy))
		require.Equal(t, string(component.UID), policy.Spec.PodSelector.MatchLabels[ComponentUIDLabel])

		identity := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: component.Name + "-identity", Namespace: namespace}}
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(identity), identity))
		require.True(t, metav1.IsControlledBy(identity, component))
		block, _ := pem.Decode(identity.Data["tls.crt"])
		require.NotNil(t, block)
		certificate, err := x509.ParseCertificate(block.Bytes)
		require.NoError(t, err)
		require.Equal(t, "spiffe://codespace/"+component.Spec.Role+"/"+string(component.UID), certificate.URIs[0].String())

		credentialName := component.Name + "-cache"
		if component.Spec.Role == "gateway" {
			credentialName = component.Name + "-ssh-host-key"
		}
		var credential corev1.Secret
		require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: credentialName}, &credential))
		require.True(t, metav1.IsControlledBy(&credential, component))
		require.Equal(t, string(component.UID), credential.Labels[ComponentUIDLabel])
		if component.Spec.Role == "cache" {
			var registryTLS corev1.Secret
			require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cacheRegistryTLSSecretName(component.Name)}, &registryTLS))
			require.True(t, metav1.IsControlledBy(&registryTLS, component))
			block, _ := pem.Decode(registryTLS.Data[corev1.TLSCertKey])
			require.NotNil(t, block)
			certificate, err := x509.ParseCertificate(block.Bytes)
			require.NoError(t, err)
			require.Equal(t, []string{component.Name + "." + namespace + ".svc"}, certificate.DNSNames)
			require.Empty(t, certificate.URIs)
			require.Equal(t, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, certificate.ExtKeyUsage)
			require.Contains(t, deployment.Spec.Template.Spec.Volumes, corev1.Volume{Name: "registry-tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: registryTLS.Name, DefaultMode: ptr.To(int32(0o440)),
				Items: []corev1.KeyToPath{{Key: "tls.crt", Path: "tls.crt"}, {Key: "tls.key", Path: "tls.key"}},
			}}})
		}
		if component.Spec.Role == "gateway" {
			require.Len(t, credential.Data[gatewaySessionKey], 32)
			var budget policyv1.PodDisruptionBudget
			require.NoError(t, c.Get(ctx, key, &budget))
			require.True(t, metav1.IsControlledBy(&budget, component))
			require.Equal(t, intstr.FromInt(1), *budget.Spec.MaxUnavailable)
		}
	}

	var ingress networkingv1.Ingress
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: gateway.Name}, &ingress))
	require.True(t, metav1.IsControlledBy(&ingress, gateway))
	require.Equal(t, "*.gateway.example.com", ingress.Spec.Rules[0].Host)
	require.Equal(t, "http", ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Name)
	require.Equal(t, []networkingv1.IngressTLS{{Hosts: []string{"*.gateway.example.com"}, SecretName: "wildcard-tls"}}, ingress.Spec.TLS)
	var sshService corev1.Service
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: gateway.Name + "-ssh"}, &sshService))
	require.Equal(t, corev1.ServiceTypeLoadBalancer, sshService.Spec.Type)
	require.Equal(t, corev1.ServiceExternalTrafficPolicyLocal, sshService.Spec.ExternalTrafficPolicy)
	require.Equal(t, []corev1.ServicePort{{Name: "ssh", Port: 22, TargetPort: intstr.FromString("ssh")}}, sshService.Spec.Ports)

	var claim corev1.PersistentVolumeClaim
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cache.Name}, &claim))
	require.Equal(t, resource.MustParse("1Gi"), claim.Spec.Resources.Requests[corev1.ResourceStorage])

	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(gateway), gateway))
	gateway.Spec.Gateway.HTTPExposure = api.GatewayHTTPExposure{Mode: "external"}
	gateway.Spec.Gateway.SSHExposure = api.GatewaySSHExposure{Mode: "external"}
	gateway.Spec.Gateway.Replicas = 1
	require.NoError(t, c.Update(ctx, gateway))
	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gateway)})
	require.NoError(t, err)
	require.True(t, apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: gateway.Name}, &ingress)))
	require.True(t, apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: gateway.Name + "-ssh"}, &sshService)))
	var budget policyv1.PodDisruptionBudget
	require.True(t, apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: gateway.Name}, &budget)))
	var gatewayDeployment appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: gateway.Name}, &gatewayDeployment))
	require.Equal(t, int32(1), ptr.Deref(gatewayDeployment.Spec.Replicas, int32(0)))
	require.Nil(t, gatewayDeployment.Spec.Template.Spec.Affinity)

	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(cache), cache))
	cache.Spec.Cache.Storage = api.CacheStorage{Driver: "s3", S3: api.CacheS3Storage{Region: "test", Bucket: "codespace"}}
	require.NoError(t, c.Update(ctx, cache))
	_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cache)})
	require.NoError(t, err)
	require.True(t, apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cache.Name}, &claim)))
}

func TestComponentReconcilerRejectsCredentialOwnershipConflict(t *testing.T) {
	component := testCacheComponent("cache", "cache-uid")
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: component.Name + "-cache", Namespace: "codespace-system"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(component, secret).Build()
	reconciler := &ComponentReconciler{Client: c, ManagementNamespace: secret.Namespace, PlatformImage: "localhost/codespace@sha256:" + strings.Repeat("a", 64), PlatformRevision: "test"}

	_, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(component)})
	require.ErrorContains(t, err, "credential ownership conflicts")
}

func TestComponentReconcilerRejectsCacheStorageClassChange(t *testing.T) {
	component := testCacheComponent("cache", "cache-uid")
	component.Spec.Cache.Storage.StorageClassName = "fast"
	claim := managedCacheVolume(component, "slow")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(claim).Build()
	reconciler := &ComponentReconciler{Client: c, ManagementNamespace: claim.Namespace}
	require.EqualError(t, reconciler.reconcileCacheVolume(t.Context(), component), `cache volume storage class is "slow", but configuration requests "fast"; storage class is immutable after PVC creation`)
}

func TestComponentReconcilerKeepsDefaultStorageClassOnExistingCacheVolume(t *testing.T) {
	component := testCacheComponent("cache", "cache-uid")
	claim := managedCacheVolume(component, "slow")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(claim).Build()
	reconciler := &ComponentReconciler{Client: c, ManagementNamespace: claim.Namespace}
	require.NoError(t, reconciler.reconcileCacheVolume(t.Context(), component))
	var updated corev1.PersistentVolumeClaim
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(claim), &updated))
	require.Equal(t, "slow", ptr.Deref(updated.Spec.StorageClassName, ""))
}

func TestComponentReconcilerWaitsForCertManagerTLSSecret(t *testing.T) {
	component := testGatewayComponent("gateway", "gateway-uid")
	component.Spec.Gateway.HTTPExposure = api.GatewayHTTPExposure{Mode: "ingress", Ingress: &api.GatewayIngressExposure{
		ClassName: "nginx",
		TLS:       api.GatewayIngressTLS{Source: "certManager", Issuer: &api.GatewayIssuerReference{Kind: "ClusterIssuer", Name: "letsencrypt"}},
	}}
	ingressClass := &networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "nginx"}, Spec: networkingv1.IngressClassSpec{Controller: "k8s.io/ingress-nginx"}}
	issuer := &unstructured.Unstructured{}
	issuer.SetAPIVersion("cert-manager.io/v1")
	issuer.SetKind("ClusterIssuer")
	issuer.SetName("letsencrypt")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(component).WithObjects(component, ingressClass, issuer).Build()
	reconciler := &ComponentReconciler{
		Client: c, Identity: newTestIdentityAuthority(t), ManagementNamespace: "codespace-system",
		ManagerURL: "https://manager.codespace-system.svc:8443", ManagerIdentity: "spiffe://codespace/manager/namespace-uid",
		PlatformImage: "localhost/codespace@sha256:" + strings.Repeat("a", 64), PlatformRevision: "test",
	}

	_, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(component)})
	require.NoError(t, err)
	var ingress networkingv1.Ingress
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Namespace: "codespace-system", Name: component.Name}, &ingress))
	require.Equal(t, "letsencrypt", ingress.Annotations["cert-manager.io/cluster-issuer"])
	require.Equal(t, component.Name+"-tls", ingress.Spec.TLS[0].SecretName)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(component), component))
	condition := meta.FindStatusCondition(component.Status.Conditions, "HTTPExposureConfigured")
	require.NotNil(t, condition)
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.Equal(t, "TLSSecretPending", condition.Reason)
}
