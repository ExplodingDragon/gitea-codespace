// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"errors"
	"time"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errObservedResourceChanged = errors.New("observed Kubernetes resource changed")

func reconcileStatusError(err error) (ctrl.Result, error) {
	if errors.Is(err, errObservedResourceChanged) {
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	return ctrl.Result{}, err
}

func retryStatusUpdate[Object any, Pointer interface {
	*Object
	client.Object
}](
	ctx context.Context,
	kubeClient client.Client,
	observed Pointer,
	status func(Pointer) any,
	mutate func(Pointer) error,
) error {
	key, uid := client.ObjectKeyFromObject(observed), observed.GetUID()
	var saved Pointer
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := Pointer(new(Object))
		if err := kubeClient.Get(ctx, key, current); err != nil {
			return err
		}
		if current.GetUID() != uid {
			return errObservedResourceChanged
		}
		before := current.DeepCopyObject().(Pointer)
		if err := mutate(current); err != nil {
			return err
		}
		if err := api.ValidateObjectSize(current); err != nil {
			return err
		}
		if !equality.Semantic.DeepEqual(status(before), status(current)) {
			if err := kubeClient.Status().Update(ctx, current); err != nil {
				return err
			}
		}
		saved = current
		return nil
	})
	if err == nil {
		*observed = *saved
	}
	return err
}

func updateSiteStatus(ctx context.Context, kubeClient client.Client, observed *api.GiteaSite, mutate func(*api.GiteaSite) error) error {
	return retryStatusUpdate[api.GiteaSite](ctx, kubeClient, observed,
		func(site *api.GiteaSite) any { return site.Status },
		mutate,
	)
}

func updateTemplateStatus(ctx context.Context, kubeClient client.Client, observed *api.EnvironmentTemplate, mutate func(*api.EnvironmentTemplate) error) error {
	return retryStatusUpdate[api.EnvironmentTemplate](ctx, kubeClient, observed,
		func(template *api.EnvironmentTemplate) any { return template.Status },
		mutate,
	)
}

func updateComponentStatus(ctx context.Context, kubeClient client.Client, observed *api.CodespaceComponent, mutate func(*api.CodespaceComponent) error) error {
	return retryStatusUpdate[api.CodespaceComponent](ctx, kubeClient, observed,
		func(component *api.CodespaceComponent) any { return component.Status },
		mutate,
	)
}

func updateCodespaceStatus(ctx context.Context, kubeClient client.Client, observed *api.Codespace, mutate func(*api.Codespace) error) error {
	return retryStatusUpdate[api.Codespace](ctx, kubeClient, observed,
		func(codespace *api.Codespace) any { return codespace.Status },
		mutate,
	)
}
