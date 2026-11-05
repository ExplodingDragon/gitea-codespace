// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"errors"
	"testing"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestStatusUpdateRetriesPreserveIndependentWriters(t *testing.T) {
	t.Run("site", func(t *testing.T) {
		site := &api.GiteaSite{ObjectMeta: metav1.ObjectMeta{Name: "site", UID: "site-uid", Generation: 1}}
		kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(site).WithObjects(site).Build()
		require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(site), site))

		attempts := 0
		err := updateSiteStatus(t.Context(), kubeClient, site, func(current *api.GiteaSite) error {
			attempts++
			if attempts == 1 {
				var concurrent api.GiteaSite
				require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(site), &concurrent))
				concurrent.Status.InventoryGeneration = 7
				require.NoError(t, kubeClient.Status().Update(t.Context(), &concurrent))
			}
			current.Status.CanonicalURL = "https://gitea.example"
			meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{Type: "InfrastructureReady", Status: metav1.ConditionTrue, Reason: "Verified", ObservedGeneration: 1})
			return nil
		})
		require.NoError(t, err)
		require.Equal(t, 2, attempts)
		require.EqualValues(t, 7, site.Status.InventoryGeneration)
		require.Equal(t, "https://gitea.example", site.Status.CanonicalURL)
		require.True(t, meta.IsStatusConditionTrue(site.Status.Conditions, "InfrastructureReady"))
	})

	t.Run("codespace", func(t *testing.T) {
		codespace := &api.Codespace{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "site", UID: "runtime-uid"}}
		kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(codespace).WithObjects(codespace).Build()
		require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(codespace), codespace))

		attempts := 0
		err := updateCodespaceStatus(t.Context(), kubeClient, codespace, func(current *api.Codespace) error {
			attempts++
			if attempts == 1 {
				var concurrent api.Codespace
				require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(codespace), &concurrent))
				concurrent.Status.Target = &api.AgentTarget{Version: 3, Ready: true, PrimaryContainerID: "container"}
				require.NoError(t, kubeClient.Status().Update(t.Context(), &concurrent))
			}
			current.Status.Pod = api.ObservedResource{Name: "runtime-pod", UID: "pod-uid"}
			return nil
		})
		require.NoError(t, err)
		require.Equal(t, 2, attempts)
		require.Equal(t, types.UID("pod-uid"), codespace.Status.Pod.UID)
		require.EqualValues(t, 3, codespace.Status.Target.Version)
	})

	t.Run("template", func(t *testing.T) {
		template := &api.EnvironmentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "template-uid", Generation: 4}}
		kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(template).WithObjects(template).Build()
		require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(template), template))

		attempts := 0
		err := updateTemplateStatus(t.Context(), kubeClient, template, func(current *api.EnvironmentTemplate) error {
			attempts++
			if attempts == 1 {
				var concurrent api.EnvironmentTemplate
				require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(template), &concurrent))
				meta.SetStatusCondition(&concurrent.Status.Conditions, metav1.Condition{Type: "StorageReady", Status: metav1.ConditionTrue, Reason: "Available", ObservedGeneration: 4})
				require.NoError(t, kubeClient.Status().Update(t.Context(), &concurrent))
			}
			meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "DependenciesReady", ObservedGeneration: 4})
			return nil
		})
		require.NoError(t, err)
		require.Equal(t, 2, attempts)
		require.True(t, meta.IsStatusConditionTrue(template.Status.Conditions, "StorageReady"))
		require.True(t, meta.IsStatusConditionTrue(template.Status.Conditions, "Ready"))
	})
}

func TestStatusUpdateRejectsNewGeneration(t *testing.T) {
	site := &api.GiteaSite{ObjectMeta: metav1.ObjectMeta{Name: "site", UID: "site-uid", Generation: 1}, Spec: api.GiteaSiteSpec{DisplayName: "before"}}
	kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(site).WithObjects(site).Build()
	require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(site), site))

	attempts := 0
	err := updateSiteStatus(t.Context(), kubeClient, site, func(current *api.GiteaSite) error {
		attempts++
		if attempts == 1 {
			var concurrent api.GiteaSite
			require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(site), &concurrent))
			concurrent.Spec.DisplayName = "after"
			concurrent.Generation = 2
			require.NoError(t, kubeClient.Update(t.Context(), &concurrent))
		}
		if current.Generation != 1 {
			return errObservedResourceChanged
		}
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{Type: "InfrastructureReady", Status: metav1.ConditionTrue, Reason: "Verified", ObservedGeneration: 1})
		return nil
	})
	require.True(t, errors.Is(err, errObservedResourceChanged))
	require.Equal(t, 2, attempts)

	var current api.GiteaSite
	require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(site), &current))
	require.EqualValues(t, 2, current.Generation)
	require.Nil(t, meta.FindStatusCondition(current.Status.Conditions, "InfrastructureReady"))
}

func TestStatusUpdateRejectsDifferentOperationWithSameVersion(t *testing.T) {
	codespace := &api.Codespace{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "site", UID: "runtime-uid", Generation: 1},
		Spec:       api.CodespaceSpec{Operation: api.Operation{Type: "create", Version: 5}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(codespace).WithObjects(codespace).Build()
	require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(codespace), codespace))

	attempts := 0
	err := updateCodespaceStatus(t.Context(), kubeClient, codespace, func(current *api.Codespace) error {
		attempts++
		if attempts == 1 {
			var concurrent api.Codespace
			require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(codespace), &concurrent))
			concurrent.Spec.Operation.Type = "abort_create"
			concurrent.Generation = 2
			require.NoError(t, kubeClient.Update(t.Context(), &concurrent))
		}
		if current.Spec.Operation.Version != 5 || current.Spec.Operation.Type != "create" {
			return errObservedResourceChanged
		}
		current.Status.Result = &api.OperationResult{Version: 5, Succeeded: true}
		return nil
	})
	require.True(t, errors.Is(err, errObservedResourceChanged))
	require.Equal(t, 2, attempts)

	var current api.Codespace
	require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(codespace), &current))
	require.Equal(t, "abort_create", current.Spec.Operation.Type)
	require.Nil(t, current.Status.Result)
}

func TestComponentStatusRejectsNewGeneration(t *testing.T) {
	component := &api.CodespaceComponent{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", UID: "component-uid", Generation: 1},
		Spec:       api.CodespaceComponentSpec{Role: "gateway", DisplayName: "before"},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(component).WithObjects(component).Build()
	require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(component), component))

	attempts := 0
	err := updateComponentStatus(t.Context(), kubeClient, component, func(current *api.CodespaceComponent) error {
		attempts++
		if attempts == 1 {
			var concurrent api.CodespaceComponent
			require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(component), &concurrent))
			concurrent.Spec.DisplayName = "after"
			concurrent.Generation = 2
			require.NoError(t, kubeClient.Update(t.Context(), &concurrent))
		}
		if current.Generation != 1 {
			return errObservedResourceChanged
		}
		current.Status.ObservedGeneration = 1
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{Type: "Available", Status: metav1.ConditionTrue, Reason: "DeploymentReady", ObservedGeneration: 1})
		return nil
	})
	require.True(t, errors.Is(err, errObservedResourceChanged))
	require.Equal(t, 2, attempts)

	var current api.CodespaceComponent
	require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKeyFromObject(component), &current))
	require.EqualValues(t, 2, current.Generation)
	require.Zero(t, current.Status.ObservedGeneration)
	require.Nil(t, meta.FindStatusCondition(current.Status.Conditions, "Available"))
}
