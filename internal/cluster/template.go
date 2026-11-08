// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"fmt"
	"time"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type TemplateReconciler struct {
	Client client.Client
}

func validateEnvironmentDependencies(ctx context.Context, reader client.Reader, configuration api.EnvironmentConfiguration) error {
	var runtime nodev1.RuntimeClass
	if configuration.Isolation != api.IsolationNative {
		if err := reader.Get(ctx, types.NamespacedName{Name: configuration.RuntimeClassName}, &runtime); err != nil {
			return fmt.Errorf("runtime class %q: %w", configuration.RuntimeClassName, err)
		}
		if runtime.Labels[api.RuntimeIsolationLabel] != configuration.Isolation {
			return fmt.Errorf("runtime class %q is not labeled for %s isolation", runtime.Name, configuration.Isolation)
		}
	}
	if configuration.Isolation == api.IsolationNative && len(configuration.Native.DevicePlugins) > 0 {
		var nodes corev1.NodeList
		if err := reader.List(ctx, &nodes); err != nil {
			return fmt.Errorf("list nodes for native device resources: %w", err)
		}
		declared := make(map[corev1.ResourceName]struct{}, len(configuration.Native.DevicePlugins))
		combinationAvailable := false
		for _, node := range nodes.Items {
			nodeSatisfiesAll := true
			for _, device := range configuration.Native.DevicePlugins {
				quantity := node.Status.Allocatable[device.ResourceName]
				if quantity.Value() >= device.Count {
					declared[device.ResourceName] = struct{}{}
				} else {
					nodeSatisfiesAll = false
				}
			}
			combinationAvailable = combinationAvailable || nodeSatisfiesAll
		}
		for _, device := range configuration.Native.DevicePlugins {
			if _, exists := declared[device.ResourceName]; !exists {
				return fmt.Errorf("no node has %d allocatable units of device resource %q", device.Count, device.ResourceName)
			}
		}
		if !combinationAvailable {
			return fmt.Errorf("no node can satisfy the native device resource requests together")
		}
	}
	var storage storagev1.StorageClass
	if err := reader.Get(ctx, types.NamespacedName{Name: configuration.StorageClassName}, &storage); err != nil {
		return fmt.Errorf("storage class %q: %w", configuration.StorageClassName, err)
	}
	return nil
}

func (r *TemplateReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var template api.EnvironmentTemplate
	if err := r.Client.Get(ctx, request.NamespacedName, &template); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	observedGeneration := template.Generation
	problem := template.Validate()
	if problem == nil {
		problem = validateEnvironmentDependencies(ctx, r.Client, template.Spec.Runtime)
	}
	err := updateTemplateStatus(ctx, r.Client, &template, func(current *api.EnvironmentTemplate) error {
		if current.Generation != observedGeneration {
			return errObservedResourceChanged
		}
		condition := metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "DependenciesReady", Message: "Runtime, storage and device resource dependencies are ready", ObservedGeneration: observedGeneration}
		if problem != nil {
			condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "DependencyUnavailable", problem.Error()
		}
		meta.SetStatusCondition(&current.Status.Conditions, condition)
		return nil
	})
	if err != nil {
		return reconcileStatusError(err)
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}
