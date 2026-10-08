// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestKubernetesE2EComponentWorkloads(t *testing.T) {
	if os.Getenv("CODESPACE_TEST_KUBERNETES_COMPONENTS") != "1" {
		t.Skip("requires the deployed Manager and manually imported platform image")
	}
	config, err := ctrl.GetConfig()
	require.NoError(t, err)
	c, err := client.New(config, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	const namespace = "codespace-system"
	server := &AdminServer{Client: c, Namespace: namespace}
	suffix := uuid.NewString()[:8]

	gatewayConfig := configpkg.DefaultGatewayConfig()
	gatewayConfig.HTTP.PublicURL = "https://gateway-" + suffix + ".example.test"
	gatewayConfig.SSH.PublicAddr = "gateway-" + suffix + ".example.test:22"
	gatewaySpec, err := json.Marshal(adminComponentSpec{Role: "gateway", DisplayName: "E2E gateway", Gateway: &gatewayConfig})
	require.NoError(t, err)
	gateway := &corev1.ConfigMap{}
	gateway.Name = "gateway-" + suffix
	require.NoError(t, server.saveComponent(ctx, gateway, adminWrite{Name: gateway.Name, Spec: gatewaySpec}, true))

	cacheConfig := configpkg.CacheConfig{
		Enabled: true, Listen: ":5000", PublicURL: "http://cache-" + suffix + ".example.test", MaxSize: "512MiB",
		Storage: configpkg.CacheStorageConfig{Driver: "filesystem", Path: "/var/lib/codespace-cache"},
	}
	cacheSpec, err := json.Marshal(adminComponentSpec{Role: "cache", DisplayName: "E2E cache", Cache: &cacheConfig})
	require.NoError(t, err)
	cache := &corev1.ConfigMap{}
	cache.Name = "cache-" + suffix
	require.NoError(t, server.saveComponent(ctx, cache, adminWrite{Name: cache.Name, Spec: cacheSpec}, true))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for _, component := range []*corev1.ConfigMap{gateway, cache} {
			var current corev1.ConfigMap
			if err := c.Get(cleanup, types.NamespacedName{Namespace: namespace, Name: component.Name}, &current); err == nil {
				require.NoError(t, client.IgnoreNotFound(c.Delete(cleanup, &current, client.Preconditions{UID: &current.UID})))
			} else {
				require.True(t, apierrors.IsNotFound(err), err)
			}
		}
		require.Eventually(t, func() bool {
			return apierrors.IsNotFound(c.Get(cleanup, types.NamespacedName{Namespace: namespace, Name: "cache-owner-" + cache.Name}, &coordinationv1.Lease{}))
		}, 30*time.Second, time.Second)
		for _, component := range []*corev1.ConfigMap{gateway, cache} {
			require.Eventually(t, func() bool {
				return apierrors.IsNotFound(c.Get(cleanup, types.NamespacedName{Namespace: namespace, Name: component.Name + "-identity"}, &corev1.Secret{}))
			}, 30*time.Second, time.Second)
		}
	})

	for _, component := range []*corev1.ConfigMap{gateway, cache} {
		require.Eventually(t, func() bool {
			var deployment appsv1.Deployment
			if c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: component.Name}, &deployment) != nil {
				return false
			}
			return deployment.Status.ReadyReplicas == 1 && deployment.Status.AvailableReplicas == 1
		}, 2*time.Minute, time.Second)
		var service corev1.Service
		require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: component.Name}, &service))
		require.Equal(t, string(component.UID), service.Spec.Selector[ComponentUIDLabel])
		var identity corev1.Secret
		require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: component.Name + "-identity"}, &identity))
		require.True(t, metav1.IsControlledBy(&identity, component))
	}

	require.Eventually(t, func() bool {
		var lease coordinationv1.Lease
		if c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "cache-owner-" + cache.Name}, &lease) != nil || lease.Spec.RenewTime == nil {
			return false
		}
		return time.Since(lease.Spec.RenewTime.Time) < cacheOwnerTTL
	}, time.Minute, time.Second)

	items, err := server.listResources(ctx, "components")
	require.NoError(t, err)
	available := make(map[string]bool, 2)
	for _, item := range items {
		if item.Name == gateway.Name || item.Name == cache.Name {
			available[item.Name] = item.Status.(adminComponentStatus).Available
		}
	}
	require.Equal(t, map[string]bool{gateway.Name: true, cache.Name: true}, available)
}
