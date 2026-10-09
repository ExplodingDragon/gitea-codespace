// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestKubernetesE2EManagerFailover(t *testing.T) {
	cluster := newE2ECluster(t, 3*time.Minute)
	workloadKey := types.NamespacedName{Namespace: cluster.managementNamespace, Name: requireE2EEnvironment(t, "CODESPACE_E2E_MANAGER_NAME")}
	leaseKey := types.NamespacedName{Namespace: cluster.managementNamespace, Name: "codespace-manager"}

	var deployment appsv1.Deployment
	require.NoError(t, cluster.Get(cluster.ctx, workloadKey, &deployment))
	require.NotNil(t, deployment.Spec.Replicas)
	require.GreaterOrEqual(t, *deployment.Spec.Replicas, int32(2))
	require.Eventually(t, func() bool {
		return cluster.Get(cluster.ctx, workloadKey, &deployment) == nil && deployment.Status.ReadyReplicas == *deployment.Spec.Replicas
	}, time.Minute, time.Second)

	var lease coordinationv1.Lease
	require.NoError(t, cluster.Get(cluster.ctx, leaseKey, &lease))
	require.NotNil(t, lease.Spec.HolderIdentity)
	leaderName, _, found := strings.Cut(*lease.Spec.HolderIdentity, "_")
	require.True(t, found)
	var leader corev1.Pod
	require.NoError(t, cluster.Get(cluster.ctx, types.NamespacedName{Namespace: workloadKey.Namespace, Name: leaderName}, &leader))
	require.Equal(t, "true", leader.Labels[managerLeaderLabel])
	oldUID := leader.UID
	require.NoError(t, cluster.Delete(cluster.ctx, &leader, client.Preconditions{UID: &oldUID}))

	var elected corev1.Pod
	require.Eventually(t, func() bool {
		if cluster.Get(cluster.ctx, leaseKey, &lease) != nil || lease.Spec.HolderIdentity == nil {
			return false
		}
		name, _, ok := strings.Cut(*lease.Spec.HolderIdentity, "_")
		if !ok || name == leaderName || cluster.Get(cluster.ctx, types.NamespacedName{Namespace: workloadKey.Namespace, Name: name}, &elected) != nil {
			return false
		}
		return elected.DeletionTimestamp.IsZero() && elected.Labels[managerLeaderLabel] == "true"
	}, 2*time.Minute, time.Second)
	require.Eventually(t, func() bool {
		if cluster.Get(cluster.ctx, workloadKey, &deployment) != nil || deployment.Status.ReadyReplicas != *deployment.Spec.Replicas {
			return false
		}
		var slices discoveryv1.EndpointSliceList
		if cluster.List(cluster.ctx, &slices, client.InNamespace(workloadKey.Namespace), client.MatchingLabels{discoveryv1.LabelServiceName: workloadKey.Name}) != nil {
			return false
		}
		readyAddresses := make([]string, 0, 1)
		for _, slice := range slices.Items {
			for _, endpoint := range slice.Endpoints {
				if endpoint.Conditions.Ready != nil && *endpoint.Conditions.Ready {
					readyAddresses = append(readyAddresses, endpoint.Addresses...)
				}
			}
		}
		return len(readyAddresses) == 1 && readyAddresses[0] == elected.Status.PodIP
	}, 2*time.Minute, time.Second)
}
