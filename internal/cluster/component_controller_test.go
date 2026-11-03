// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestComponentReconcilerMaterializesGatewayAndCache(t *testing.T) {
	ctx := t.Context()
	namespace := "codespace-system"
	gatewayConfig := configpkg.DefaultGatewayConfig()
	gatewayConfig.HTTP.Listen = ":8080"
	gatewayConfig.HTTP.PublicURL = "https://gateway.example.com"
	gatewayJSON, err := json.Marshal(gatewayConfig)
	require.NoError(t, err)
	gateway := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: namespace, UID: "gateway-uid", Labels: map[string]string{ComponentLabel: "gateway"}}, Data: map[string]string{
		"config": string(gatewayJSON), "sshHostKeySecret": "gateway-key", "sshHostKeySecretUID": "gateway-key-uid",
	}}
	gatewayKey := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gateway-key", Namespace: namespace, UID: "gateway-key-uid", Labels: map[string]string{pendingComponentLabel: gateway.Name}}, Data: map[string][]byte{"hostKey": []byte("test")}}
	cacheConfig := configpkg.CacheConfig{Name: "Build cache", Revision: 1, Enabled: true, Listen: ":5000", PublicURL: "https://cache.example.com", Storage: configpkg.CacheStorageConfig{Driver: "filesystem", Path: "/var/lib/cache"}, MaxSize: "1GiB", MaxAge: configpkg.Duration(24 * time.Hour), GCInterval: configpkg.Duration(time.Hour), Upstreams: map[string]configpkg.RuntimeCacheUpstreamConfig{"docker.io": {Allow: []string{"library/*"}}}}
	encoded, err := json.Marshal(cacheConfig)
	require.NoError(t, err)
	cache := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: namespace, UID: "cache-uid", Labels: map[string]string{ComponentLabel: "cache"}}, Data: map[string]string{
		"config": string(encoded), "secretName": "cache-key", "secretUID": "cache-key-uid",
	}}
	cacheKey := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cache-key", Namespace: namespace, UID: "cache-key-uid", Labels: map[string]string{pendingComponentLabel: cache.Name}}, Data: map[string][]byte{"registryKey": make([]byte, 32), "tokenKey": make([]byte, 32)}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(gateway, gatewayKey, cache, cacheKey).Build()
	reconciler := &ComponentReconciler{
		Client: c, Identity: newTestIdentityAuthority(t), ManagementNamespace: namespace, ManagerURL: "https://manager.codespace-system.svc:8443", ManagerIdentity: "spiffe://codespace/manager/namespace-uid", PlatformImage: "localhost/codespace@sha256:" + strings.Repeat("a", 64),
		PlatformRevision:  "sha-test",
		ImagePullSecrets:  []string{"registry"},
		GatewayParentName: "public", GatewayHTTPSectionName: "https", GatewaySSHSectionName: "ssh",
	}

	for _, component := range []*corev1.ConfigMap{gateway, cache} {
		result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(component)})
		require.NoError(t, err)
		require.NotZero(t, result.RequeueAfter)

		var service corev1.Service
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(component), component))
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(component), &service))
		require.Equal(t, string(component.UID), service.Spec.Selector[ComponentUIDLabel])

		var deployment appsv1.Deployment
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(component), &deployment))
		require.NotNil(t, deployment.Spec.Template.Spec.AutomountServiceAccountToken)
		require.False(t, *deployment.Spec.Template.Spec.AutomountServiceAccountToken)
		require.True(t, *deployment.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem)
		require.Equal(t, corev1.PullIfNotPresent, deployment.Spec.Template.Spec.Containers[0].ImagePullPolicy)
		require.Equal(t, []corev1.LocalObjectReference{{Name: "registry"}}, deployment.Spec.Template.Spec.ImagePullSecrets)
		require.Equal(t, string(component.UID), deployment.Spec.Template.Labels[ComponentUIDLabel])
		require.Equal(t, "sha-test", deployment.Spec.Template.Annotations[platformRevisionAnnotation])
		probe := deployment.Spec.Template.Spec.Containers[0].ReadinessProbe
		if component.Labels[ComponentLabel] == "gateway" {
			require.Equal(t, "/api/readyz", probe.HTTPGet.Path)
			require.Equal(t, deployment.Spec.Template.Spec.Containers[0].Ports[0].Name, probe.HTTPGet.Port.StrVal)
		} else {
			require.Equal(t, deployment.Spec.Template.Spec.Containers[0].Ports[0].Name, probe.TCPSocket.Port.StrVal)
		}

		var policy networkingv1.NetworkPolicy
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(component), &policy))
		require.Equal(t, string(component.UID), policy.Spec.PodSelector.MatchLabels[ComponentUIDLabel])

		identity := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: component.Name + "-identity", Namespace: component.Namespace}}
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(identity), identity))
		require.True(t, metav1.IsControlledBy(identity, component))
		block, _ := pem.Decode(identity.Data["tls.crt"])
		require.NotNil(t, block)
		certificate, err := x509.ParseCertificate(block.Bytes)
		require.NoError(t, err)
		require.Equal(t, "spiffe://codespace/"+component.Labels[ComponentLabel]+"/"+string(component.UID), certificate.URIs[0].String())
	}

	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: gatewayKey.Name}, gatewayKey))
	require.Equal(t, string(gateway.UID), gatewayKey.Labels[ComponentUIDLabel])
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cacheKey.Name}, cacheKey))
	require.Equal(t, string(cache.UID), cacheKey.Labels[ComponentUIDLabel])

	httpRoute := &unstructured.Unstructured{}
	httpRoute.SetAPIVersion("gateway.networking.k8s.io/v1")
	httpRoute.SetKind("HTTPRoute")
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(gateway), httpRoute))
	hostnames, found, err := unstructured.NestedStringSlice(httpRoute.Object, "spec", "hostnames")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, []string{"gateway.example.com", "*.gateway.example.com"}, hostnames)
	require.True(t, metav1.IsControlledBy(httpRoute, gateway))

	tcpRoute := &unstructured.Unstructured{}
	tcpRoute.SetAPIVersion("gateway.networking.k8s.io/v1alpha2")
	tcpRoute.SetKind("TCPRoute")
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(gateway), tcpRoute))
	require.True(t, metav1.IsControlledBy(tcpRoute, gateway))

	var claim corev1.PersistentVolumeClaim
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cache.Name}, &claim))
	require.Equal(t, resource.MustParse("1Gi"), claim.Spec.Resources.Requests[corev1.ResourceStorage])
}

func TestComponentReconcilerChecksOwnershipBeforeDeletion(t *testing.T) {
	now := metav1.Now()
	component := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "cache", Namespace: "codespace-system", UID: "cache-uid", Labels: map[string]string{ComponentLabel: "cache"},
		Finalizers: []string{componentFinalizer}, DeletionTimestamp: &now,
	}}
	owner := *metav1.NewControllerRef(component, corev1.SchemeGroupVersion.WithKind("ConfigMap"))
	identity := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cache-identity", Namespace: component.Namespace}}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: component.Namespace, OwnerReferences: []metav1.OwnerReference{owner}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(component, identity, deployment).Build()
	reconciler := &ComponentReconciler{Client: c, ManagementNamespace: component.Namespace}

	_, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(component)})
	require.ErrorContains(t, err, "identity Secret ownership conflicts")
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(deployment), deployment))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(identity), identity))

	identity.OwnerReferences = []metav1.OwnerReference{owner}
	require.NoError(t, c.Update(t.Context(), identity))
	_, err = reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(component)})
	require.NoError(t, err)
	require.Error(t, c.Get(t.Context(), client.ObjectKeyFromObject(deployment), deployment))
	require.Error(t, c.Get(t.Context(), client.ObjectKeyFromObject(identity), identity))
}
