// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"testing"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCoordinationUpdates(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		object client.Object
		change func(client.Object)
		wake   bool
	}{
		{"inventory counter", &api.GiteaSite{}, func(o client.Object) { o.(*api.GiteaSite).Status.InventoryGeneration++ }, false},
		{"site enabled state", &api.GiteaSite{}, func(o client.Object) { o.(*api.GiteaSite).Spec.Enabled = true }, true},
		{"metadata progress", &api.Codespace{}, func(o client.Object) {
			s := &o.(*api.Codespace).Status
			s.MetadataGeneration++
			s.Boot = &api.RuntimeBoot{}
		}, false},
		{"target changed", &api.Codespace{}, func(o client.Object) { o.(*api.Codespace).Status.Target = &api.AgentTarget{Version: 2, Ready: true} }, true},
		{"result ready", &api.Codespace{}, func(o client.Object) {
			o.(*api.Codespace).Status.Result = &api.OperationResult{Version: 1, Succeeded: true}
		}, true},
		{"pod address", &corev1.Pod{}, func(o client.Object) { o.(*corev1.Pod).Status.PodIP = "10.0.0.2" }, true},
		{"deletion", &corev1.Pod{}, func(o client.Object) {
			now := metav1.Now()
			o.SetDeletionTimestamp(&now)
		}, true},
		{"cache configuration", &corev1.ConfigMap{}, func(o client.Object) { o.(*corev1.ConfigMap).Data = map[string]string{"config": "new"} }, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			current := testCase.object.DeepCopyObject().(client.Object)
			current.SetResourceVersion("2")
			testCase.change(current)
			require.Equal(t, testCase.wake, coordinationUpdateChanged(testCase.object, current))
		})
	}
}
