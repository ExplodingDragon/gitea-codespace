// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	configpkg "gitea.dev/codespace/internal/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestKubernetesE2EComponentWorkloads(t *testing.T) {
	cluster := newE2ECluster(t, 3*time.Minute)
	namespace := cluster.managementNamespace
	server := &AdminServer{Client: cluster.Client, Namespace: namespace}
	suffix := uuid.NewString()[:8]

	gatewayConfig := configpkg.DefaultGatewayConfig()
	gatewayConfig.HTTP.PublicURL = "https://gateway-" + suffix + ".example.test"
	gatewayConfig.SSH.PublicAddr = "gateway-" + suffix + ".example.test:22"
	gatewaySpec, err := json.Marshal(adminComponentSpec{Role: "gateway", DisplayName: "E2E gateway", Gateway: &gatewayConfig})
	require.NoError(t, err)
	gateway := &corev1.ConfigMap{}
	gateway.Name = "gateway-" + suffix
	require.NoError(t, server.saveComponent(cluster.ctx, gateway, adminWrite{Name: gateway.Name, Spec: gatewaySpec}, true))

	cacheConfig := configpkg.CacheConfig{
		Enabled: true, Listen: ":5000", PublicURL: "http://cache-" + suffix + ".example.test", MaxSize: "512MiB",
		Storage: configpkg.CacheStorageConfig{Driver: "filesystem", Path: "/var/lib/codespace-cache"},
	}
	cacheSpec, err := json.Marshal(adminComponentSpec{Role: "cache", DisplayName: "E2E cache", Cache: &cacheConfig})
	require.NoError(t, err)
	cache := &corev1.ConfigMap{}
	cache.Name = "cache-" + suffix
	require.NoError(t, server.saveComponent(cluster.ctx, cache, adminWrite{Name: cache.Name, Spec: cacheSpec}, true))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for _, component := range []*corev1.ConfigMap{gateway, cache} {
			var current corev1.ConfigMap
			if err := cluster.Get(cleanup, types.NamespacedName{Namespace: namespace, Name: component.Name}, &current); err == nil {
				if err := client.IgnoreNotFound(cluster.Delete(cleanup, &current, client.Preconditions{UID: &current.UID})); err != nil {
					t.Errorf("delete E2E component %s: %v", component.Name, err)
				}
			} else if !apierrors.IsNotFound(err) {
				t.Errorf("read E2E component %s for cleanup: %v", component.Name, err)
			}
		}
		assert.Eventually(t, func() bool {
			return apierrors.IsNotFound(cluster.Get(cleanup, types.NamespacedName{Namespace: namespace, Name: "cache-owner-" + cache.Name}, &coordinationv1.Lease{}))
		}, 30*time.Second, time.Second)
		for _, component := range []*corev1.ConfigMap{gateway, cache} {
			assert.Eventually(t, func() bool {
				return apierrors.IsNotFound(cluster.Get(cleanup, types.NamespacedName{Namespace: namespace, Name: component.Name + "-identity"}, &corev1.Secret{}))
			}, 30*time.Second, time.Second)
		}
	})

	for _, component := range []*corev1.ConfigMap{gateway, cache} {
		require.Eventually(t, func() bool {
			var deployment appsv1.Deployment
			if cluster.Get(cluster.ctx, types.NamespacedName{Namespace: namespace, Name: component.Name}, &deployment) != nil {
				return false
			}
			return deployment.Status.ReadyReplicas == 1 && deployment.Status.AvailableReplicas == 1
		}, 2*time.Minute, time.Second)
		var service corev1.Service
		require.NoError(t, cluster.Get(cluster.ctx, types.NamespacedName{Namespace: namespace, Name: component.Name}, &service))
		require.Equal(t, string(component.UID), service.Spec.Selector[ComponentUIDLabel])
		var identity corev1.Secret
		require.NoError(t, cluster.Get(cluster.ctx, types.NamespacedName{Namespace: namespace, Name: component.Name + "-identity"}, &identity))
		require.True(t, metav1.IsControlledBy(&identity, component))
	}

	require.Eventually(t, func() bool {
		var lease coordinationv1.Lease
		if cluster.Get(cluster.ctx, types.NamespacedName{Namespace: namespace, Name: "cache-owner-" + cache.Name}, &lease) != nil || lease.Spec.RenewTime == nil {
			return false
		}
		return time.Since(lease.Spec.RenewTime.Time) < cacheOwnerTTL
	}, time.Minute, time.Second)

	items, err := server.listResources(cluster.ctx, "components")
	require.NoError(t, err)
	available := make(map[string]bool, 2)
	for _, item := range items {
		if item.Name == gateway.Name || item.Name == cache.Name {
			available[item.Name] = item.Status.(adminComponentStatus).Available
		}
	}
	require.Equal(t, map[string]bool{gateway.Name: true, cache.Name: true}, available)
}
