// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"testing"
	"time"
	"uuid"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
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
	suffix := uuid.NewV4().String()[:8]

	gateway := &api.CodespaceComponent{ObjectMeta: metav1.ObjectMeta{Name: "gateway-" + suffix}}
	gatewaySpec := testGatewayComponent(gateway.Name, "").Spec
	gatewaySpec.DisplayName = "E2E gateway"
	gatewaySpec.Gateway.EndpointBaseURL = "https://gateway-" + suffix + ".example.test"
	gatewaySpec.Gateway.SSHExternalAddress = "gateway-" + suffix + ".example.test:22"
	require.NoError(t, server.saveComponent(cluster.ctx, gateway, adminComponentWrite{adminWriteIdentity: adminWriteIdentity{Name: gateway.Name}, Spec: gatewaySpec}, true))

	cache := &api.CodespaceComponent{ObjectMeta: metav1.ObjectMeta{Name: "cache-" + suffix}}
	cacheSpec := testCacheComponent(cache.Name, "").Spec
	cacheSpec.DisplayName, cacheSpec.Cache.MaxSize = "E2E cache", "512MiB"
	require.NoError(t, server.saveComponent(cluster.ctx, cache, adminComponentWrite{adminWriteIdentity: adminWriteIdentity{Name: cache.Name}, Spec: cacheSpec}, true))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for _, component := range []*api.CodespaceComponent{gateway, cache} {
			var current api.CodespaceComponent
			if err := cluster.Get(cleanup, types.NamespacedName{Name: component.Name}, &current); err == nil {
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
		for _, component := range []*api.CodespaceComponent{gateway, cache} {
			assert.Eventually(t, func() bool {
				return apierrors.IsNotFound(cluster.Get(cleanup, types.NamespacedName{Namespace: namespace, Name: component.Name + "-identity"}, &corev1.Secret{}))
			}, 30*time.Second, time.Second)
			assert.Eventually(t, func() bool {
				return apierrors.IsNotFound(cluster.Get(cleanup, client.ObjectKeyFromObject(component), &api.CodespaceComponent{}))
			}, 30*time.Second, time.Second)
		}
	})

	for _, component := range []*api.CodespaceComponent{gateway, cache} {
		desired := int32(1)
		if component.Spec.Gateway != nil {
			desired = component.Spec.Gateway.Replicas
		}
		require.Eventually(t, func() bool {
			var deployment appsv1.Deployment
			if cluster.Get(cluster.ctx, types.NamespacedName{Namespace: namespace, Name: component.Name}, &deployment) != nil {
				return false
			}
			return deployment.Status.ReadyReplicas == desired && deployment.Status.AvailableReplicas == desired
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
