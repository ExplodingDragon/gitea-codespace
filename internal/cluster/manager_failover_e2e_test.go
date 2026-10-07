// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestKubernetesE2EManagerFailover(t *testing.T) {
	if os.Getenv("CODESPACE_TEST_KUBERNETES_HA") != "1" {
		t.Skip("requires the deployed two-replica Manager")
	}
	config, err := ctrl.GetConfig()
	require.NoError(t, err)
	c, err := client.New(config, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	key := types.NamespacedName{Namespace: "codespace-system", Name: "codespace-manager"}

	var deployment appsv1.Deployment
	require.NoError(t, c.Get(ctx, key, &deployment))
	require.NotNil(t, deployment.Spec.Replicas)
	require.GreaterOrEqual(t, *deployment.Spec.Replicas, int32(2))
	require.Eventually(t, func() bool {
		return c.Get(ctx, key, &deployment) == nil && deployment.Status.ReadyReplicas == *deployment.Spec.Replicas
	}, time.Minute, time.Second)

	var lease coordinationv1.Lease
	require.NoError(t, c.Get(ctx, key, &lease))
	require.NotNil(t, lease.Spec.HolderIdentity)
	leaderName, _, found := strings.Cut(*lease.Spec.HolderIdentity, "_")
	require.True(t, found)
	var leader corev1.Pod
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: leaderName}, &leader))
	require.Equal(t, "true", leader.Labels[managerLeaderLabel])
	oldUID := leader.UID
	require.NoError(t, c.Delete(ctx, &leader, client.Preconditions{UID: &oldUID}))

	var elected corev1.Pod
	require.Eventually(t, func() bool {
		if c.Get(ctx, key, &lease) != nil || lease.Spec.HolderIdentity == nil {
			return false
		}
		name, _, ok := strings.Cut(*lease.Spec.HolderIdentity, "_")
		if !ok || name == leaderName || c.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: name}, &elected) != nil {
			return false
		}
		return elected.DeletionTimestamp.IsZero() && elected.Labels[managerLeaderLabel] == "true"
	}, 2*time.Minute, time.Second)
	require.Eventually(t, func() bool {
		if c.Get(ctx, key, &deployment) != nil || deployment.Status.ReadyReplicas != *deployment.Spec.Replicas {
			return false
		}
		var slices discoveryv1.EndpointSliceList
		if c.List(ctx, &slices, client.InNamespace(key.Namespace), client.MatchingLabels{discoveryv1.LabelServiceName: key.Name}) != nil {
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
