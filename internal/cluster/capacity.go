// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"fmt"
	"slices"
	"strings"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// capacity limits claims; Kubernetes admission remains authoritative for resources.
func (c *SiteCoordinator) capacity(ctx context.Context, site *api.GiteaSite, rows []api.Codespace, tags []string, request *codespacev1.FetchOperationsRequest) error {
	startup, cleanup := site.Spec.StartupConcurrency, site.Spec.CleanupConcurrency
	for _, cs := range rows {
		if !cs.DeletionTimestamp.IsZero() || cs.Status.RecoveryAction != "" {
			cleanup--
		} else if cs.Status.SettledOperationVersion < cs.Spec.Operation.Version {
			switch cs.Spec.Operation.Type {
			case "create", "resume":
				startup--
			default:
				cleanup--
			}
		}
	}
	request.CleanupCapacityAvailable = max(0, cleanup)
	if startup <= 0 {
		return nil
	}
	namespace, err := api.SiteNamespace(site.Name, c.ManagementNamespace)
	if err != nil {
		return err
	}
	var quotas corev1.ResourceQuotaList
	if err := c.Client.List(ctx, &quotas, client.InNamespace(namespace)); err != nil {
		return err
	}
	var managed bool
	for _, quota := range quotas.Items {
		if quota.Name == "codespace" {
			managed = quota.Labels[SiteUIDLabel] == string(site.UID) && quota.DeletionTimestamp.IsZero()
		}
		if !equality.Semantic.DeepEqual(quota.Spec.Hard, quota.Status.Hard) {
			return fmt.Errorf("quota %s has not observed its current limits", quota.Name)
		}
		for name := range quota.Status.Hard {
			if _, ok := quota.Status.Used[name]; !ok {
				return fmt.Errorf("quota %s has not observed usage for %s", quota.Name, name)
			}
		}
	}
	if !managed {
		return fmt.Errorf("site resource quota is unavailable")
	}
	var accepted []string
	if site.Spec.AcceptCreates {
		for _, ref := range site.Spec.Templates {
			var template api.EnvironmentTemplate
			if err := c.Client.Get(ctx, types.NamespacedName{Name: ref.Name}, &template); err != nil {
				return err
			}
			if template.UID != ref.UID || !template.DeletionTimestamp.IsZero() {
				return fmt.Errorf("environment identity changed")
			}
			if !slices.Contains(tags, template.Spec.Tag) {
				continue
			}
			demand := corev1.ResourceList{
				corev1.ResourcePods: resource.MustParse("1"), corev1.ResourcePersistentVolumeClaims: resource.MustParse("1"),
				corev1.ResourceRequestsStorage:         template.Spec.Runtime.Storage[corev1.ResourceStorage],
				"count/codespaces.codespace.gitea.dev": resource.MustParse("1"),
			}
			for name, quantity := range template.Spec.Runtime.Resources.Requests {
				demand[corev1.ResourceName("requests."+string(name))] = quantity
			}
			for name, quantity := range template.Spec.Runtime.Resources.Limits {
				if !strings.Contains(string(name), "/") {
					demand[corev1.ResourceName("limits."+string(name))] = quantity
				}
			}
			fits := true
			for _, quota := range quotas.Items {
				// Scoped quotas are enforced by admission; their applicability is not
				// duplicated here as a second Kubernetes scheduler.
				if len(quota.Spec.Scopes) != 0 || quota.Spec.ScopeSelector != nil {
					continue
				}
				for name, required := range demand {
					if hard, exists := quota.Status.Hard[name]; exists {
						remaining := hard.DeepCopy()
						remaining.Sub(quota.Status.Used[name])
						if remaining.Cmp(required) < 0 {
							fits = false
						}
					}
				}
			}
			if fits {
				accepted = append(accepted, template.Spec.Tag)
			}
		}
	}
	request.StartupCapacityAvailable = startup
	request.AcceptedOperationTypes = []codespacev1.AcceptedOperationType{codespacev1.AcceptedOperationType_ACCEPTED_OPERATION_TYPE_RESUME}
	if len(accepted) != 0 {
		request.AcceptedCreateTags = accepted
		request.AcceptedOperationTypes = append(request.AcceptedOperationTypes, codespacev1.AcceptedOperationType_ACCEPTED_OPERATION_TYPE_CREATE)
	}
	return nil
}
