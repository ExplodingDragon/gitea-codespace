// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"encoding/json"
	"strings"
	"testing"

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
	gateway := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: namespace, UID: "gateway-uid", Labels: map[string]string{ComponentLabel: "gateway"}}, Data: map[string]string{
		"httpListen": ":8080", "sshListen": ":2222", "url": "https://gateway.example.com", "sshHostKeySecret": "gateway-key", "sshHostKeySecretUID": "gateway-key-uid",
	}}
	gatewayKey := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gateway-key", Namespace: namespace, UID: "gateway-key-uid", Labels: map[string]string{pendingComponentLabel: gateway.Name}}, Data: map[string][]byte{"hostKey": []byte("test")}}
	cacheConfig := configpkg.CacheConfig{Name: "Build cache", Listen: ":5000", Storage: configpkg.CacheStorageConfig{Driver: "filesystem", Path: "/var/lib/cache"}, MaxSize: "1GiB"}
	encoded, err := json.Marshal(cacheConfig)
	require.NoError(t, err)
	cache := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: namespace, UID: "cache-uid", Labels: map[string]string{ComponentLabel: "cache"}}, Data: map[string]string{
		"config": string(encoded), "secretName": "cache-key", "secretUID": "cache-key-uid",
	}}
	cacheKey := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cache-key", Namespace: namespace, UID: "cache-key-uid", Labels: map[string]string{pendingComponentLabel: cache.Name}}, Data: map[string][]byte{"registryKey": make([]byte, 32), "tokenKey": make([]byte, 32)}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(gateway, gatewayKey, cache, cacheKey).Build()
	reconciler := &ComponentReconciler{
		Client: c, ManagementNamespace: namespace, ManagerURL: "https://manager.codespace-system.svc:8443", IdentityIssuer: "codespace-internal", Image: "localhost/codespace@sha256:" + strings.Repeat("a", 64),
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
		require.Equal(t, string(component.UID), deployment.Spec.Template.Labels[ComponentUIDLabel])
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

		certificate := &unstructured.Unstructured{}
		certificate.SetAPIVersion("cert-manager.io/v1")
		certificate.SetKind("Certificate")
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(component), certificate))
		uris, found, err := unstructured.NestedStringSlice(certificate.Object, "spec", "uris")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, []string{"spiffe://codespace/" + component.Labels[ComponentLabel] + "/" + string(component.UID) + "/deployment"}, uris)

		identity := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: component.Name + "-identity", Namespace: component.Namespace}}
		require.NoError(t, c.Create(ctx, identity))
		result, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(component)})
		require.NoError(t, err)
		require.Zero(t, result.RequeueAfter)
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(identity), identity))
		require.True(t, metav1.IsControlledBy(identity, component))
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

func TestComponentReconcilerDeletesGeneratedIdentity(t *testing.T) {
	now := metav1.Now()
	component := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "cache", Namespace: "codespace-system", UID: "cache-uid", Labels: map[string]string{ComponentLabel: "cache"},
		Finalizers: []string{componentFinalizer}, DeletionTimestamp: &now,
	}}
	owner := *metav1.NewControllerRef(component, corev1.SchemeGroupVersion.WithKind("ConfigMap"))
	identity := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cache-identity", Namespace: component.Namespace}}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: component.Namespace, OwnerReferences: []metav1.OwnerReference{owner}}}
	certificate := &unstructured.Unstructured{}
	certificate.SetAPIVersion("cert-manager.io/v1")
	certificate.SetKind("Certificate")
	certificate.SetName(component.Name)
	certificate.SetNamespace(component.Namespace)
	certificate.SetOwnerReferences([]metav1.OwnerReference{owner})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(component, identity, deployment, certificate).Build()
	reconciler := &ComponentReconciler{Client: c, ManagementNamespace: component.Namespace}

	result, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(component)})
	require.NoError(t, err)
	require.NotZero(t, result.RequeueAfter)
	require.Error(t, c.Get(t.Context(), client.ObjectKeyFromObject(deployment), deployment))
	require.Error(t, c.Get(t.Context(), client.ObjectKeyFromObject(certificate), certificate))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(identity), identity))

	_, err = reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(component)})
	require.NoError(t, err)
	require.Error(t, c.Get(t.Context(), client.ObjectKeyFromObject(identity), identity))
}
