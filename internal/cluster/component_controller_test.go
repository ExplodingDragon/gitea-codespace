// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	gateway := testGatewayComponent("gateway", "gateway-uid")
	cache := testCacheComponent("cache", "cache-uid")
	parent := &unstructured.Unstructured{}
	parent.SetAPIVersion("gateway.networking.k8s.io/v1")
	parent.SetKind("Gateway")
	parent.SetName(gateway.Spec.Gateway.Parent.Name)
	parent.SetNamespace(gateway.Spec.Gateway.Parent.Namespace)
	parent.Object["spec"] = map[string]any{"listeners": []any{
		map[string]any{"name": "https", "protocol": "HTTPS"},
		map[string]any{"name": "ssh", "protocol": "TCP"},
	}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(gateway, cache).WithObjects(gateway, cache, parent).Build()
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
		probe := deployment.Spec.Template.Spec.Containers[0].ReadinessProbe
		if component.Spec.Role == "gateway" {
			require.Equal(t, "/api/readyz", probe.HTTPGet.Path)
			require.Equal(t, deployment.Spec.Template.Spec.Containers[0].Ports[0].Name, probe.HTTPGet.Port.StrVal)
		} else {
			require.Equal(t, deployment.Spec.Template.Spec.Containers[0].Ports[0].Name, probe.TCPSocket.Port.StrVal)
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
	}

	httpRoute := &unstructured.Unstructured{}
	httpRoute.SetAPIVersion("gateway.networking.k8s.io/v1")
	httpRoute.SetKind("HTTPRoute")
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: gateway.Name}, httpRoute))
	hostnames, found, err := unstructured.NestedStringSlice(httpRoute.Object, "spec", "hostnames")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, []string{"gateway.example.com", "*.gateway.example.com"}, hostnames)
	require.True(t, metav1.IsControlledBy(httpRoute, gateway))

	tcpRoute := &unstructured.Unstructured{}
	tcpRoute.SetAPIVersion("gateway.networking.k8s.io/v1alpha2")
	tcpRoute.SetKind("TCPRoute")
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: gateway.Name}, tcpRoute))
	require.True(t, metav1.IsControlledBy(tcpRoute, gateway))

	var claim corev1.PersistentVolumeClaim
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cache.Name}, &claim))
	require.Equal(t, resource.MustParse("1Gi"), claim.Spec.Resources.Requests[corev1.ResourceStorage])

	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(gateway), gateway))
	gateway.Spec.Gateway.Parent = api.GatewayParent{}
	require.NoError(t, c.Update(ctx, gateway))
	_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gateway)})
	require.NoError(t, err)
	require.True(t, apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: gateway.Name}, httpRoute)))
	require.True(t, apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: gateway.Name}, tcpRoute)))

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
