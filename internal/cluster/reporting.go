// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"sync/atomic"
	"time"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Slow result or metadata RPCs must not delay the site's lease renewal loop.
func (c *SiteCoordinator) reportSite(ctx context.Context, name string, uid types.UID, interval *atomic.Int64) {
	defer c.Samples.RetainSite(uid, nil)
	namespace, err := api.SiteNamespace(name, c.ManagementNamespace)
	if err != nil {
		return
	}
	logger := log.FromContext(ctx).WithValues("site", name)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var metadataAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		var site api.GiteaSite
		if err := c.Client.Get(ctx, types.NamespacedName{Name: name}, &site); err != nil {
			if ctx.Err() == nil {
				logger.Error(err, "Read site for runtime reporting")
			}
			continue
		}
		if site.UID != uid || !site.Spec.Enabled || !site.DeletionTimestamp.IsZero() {
			return
		}
		ready := meta.FindStatusCondition(site.Status.Conditions, "InfrastructureReady")
		if ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != site.Generation {
			continue
		}
		remote, err := siteManagerClient(ctx, c.Client, c.ManagementNamespace, &site)
		if err != nil {
			logger.Error(err, "Read Gitea reporting identity")
			continue
		}
		var rows api.CodespaceList
		if err := c.Client.List(ctx, &rows, client.InNamespace(namespace)); err != nil {
			if ctx.Err() == nil {
				logger.Error(err, "List runtimes for reporting")
			}
			continue
		}
		refresh := time.Since(metadataAt) >= time.Duration(interval.Load())
		c.Samples.RetainSite(uid, rows.Items)
		for i := range rows.Items {
			cs := &rows.Items[i]
			if cs.Spec.Site.UID != uid || !cs.Status.Bound {
				continue
			}
			pending := cs.Status.SettledOperationVersion < cs.Spec.Operation.Version && (cs.Status.Result != nil || meta.IsStatusConditionTrue(cs.Status.Conditions, "Stopped"))
			if !refresh && !pending {
				continue
			}
			if err := c.Flush(ctx, cs, remote); err != nil && ctx.Err() == nil {
				logger.Error(err, "Report runtime", "codespace", cs.Name)
			}
		}
		if refresh {
			metadataAt = time.Now()
		}
	}
}
