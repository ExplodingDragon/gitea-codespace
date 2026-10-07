// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"fmt"
	"time"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type TemplateReconciler struct{ Client client.Client }

func (r *TemplateReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var template api.EnvironmentTemplate
	if err := r.Client.Get(ctx, request.NamespacedName, &template); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	previous := template.DeepCopy().Status
	condition := metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Attested", Message: "An administrator recorded validation for this Runtime and storage configuration", ObservedGeneration: template.Generation}
	problem := template.Validate()
	if problem == nil {
		var runtime nodev1.RuntimeClass
		problem = r.Client.Get(ctx, types.NamespacedName{Name: template.Spec.Runtime.RuntimeClassName}, &runtime)
	}
	if problem == nil {
		var storage storagev1.StorageClass
		problem = r.Client.Get(ctx, types.NamespacedName{Name: template.Spec.Runtime.StorageClassName}, &storage)
	}
	if problem != nil {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "DependencyUnavailable", problem.Error()
	} else if template.Status.VerifiedGeneration != template.Generation || template.Status.Verification == "" {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "VerificationRequired", "Record an administrator validation reference before publishing this template"
	}
	meta.SetStatusCondition(&template.Status.Conditions, condition)
	if err := api.ValidateObjectSize(&template); err != nil {
		return ctrl.Result{}, fmt.Errorf("template status: %w", err)
	}
	if equality.Semantic.DeepEqual(previous, template.Status) {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if err := r.Client.Status().Update(ctx, &template); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}
