// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// A workqueue serializes each runtime without blocking other runtimes or lease renewal.
func (c *SiteCoordinator) reportSite(ctx context.Context, name string, uid types.UID, interval *atomic.Int64) {
	defer c.Samples.RetainSite(uid, nil)
	namespace, err := api.SiteNamespace(name, c.ManagementNamespace)
	if err != nil {
		return
	}
	logger := log.FromContext(ctx).WithValues("site", name)
	queue := workqueue.NewTyped[types.NamespacedName]()
	var workers sync.WaitGroup
	var reported sync.Map
	for range 4 {
		workers.Go(func() {
			for {
				key, shutdown := queue.Get()
				if shutdown {
					return
				}
				err := func() error {
					defer queue.Done(key)
					var cs api.Codespace
					if err := c.Client.Get(ctx, key, &cs); err != nil {
						return client.IgnoreNotFound(err)
					}
					if cs.Spec.Site.UID != uid {
						return nil
					}
					var site api.GiteaSite
					if err := c.Client.Get(ctx, types.NamespacedName{Name: name}, &site); err != nil {
						return client.IgnoreNotFound(err)
					}
					ready := meta.FindStatusCondition(site.Status.Conditions, "InfrastructureReady")
					if site.UID != uid || !site.Spec.Enabled || !site.DeletionTimestamp.IsZero() || ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != site.Generation {
						return nil
					}
					remote, err := c.Connections.ForSite(ctx, &site)
					if err != nil {
						return err
					}
					generation := cs.Status.MetadataGeneration
					if err := c.Flush(ctx, &cs, remote); err != nil {
						return err
					}
					reported.Store(cs.UID, generation)
					return nil
				}()
				if err != nil && ctx.Err() == nil {
					logger.Error(err, "Report runtime", "codespace", key.Name)
				}
			}
		})
	}
	defer func() { queue.ShutDown(); workers.Wait() }()
	var changes <-chan struct{}
	if c.Changes != nil {
		var unsubscribe func()
		changes, unsubscribe = c.Changes.Subscribe(uid)
		defer unsubscribe()
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var metadataAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-changes:
		}
		refresh := time.Since(metadataAt) >= time.Duration(interval.Load())
		var observed api.CodespaceList
		if err := c.ObservationReader.List(ctx, &observed, client.InNamespace(namespace)); err != nil {
			if ctx.Err() == nil {
				logger.Error(err, "Observe runtimes for reporting")
			}
			continue
		}
		current := make(map[types.UID]bool, len(observed.Items))
		for i := range observed.Items {
			cs := &observed.Items[i]
			if cs.Spec.Site.UID != uid {
				continue
			}
			current[cs.UID] = true
			if c.Logs != nil {
				stage, message, err := managerProgress(ctx, c.ObservationReader, cs)
				if err != nil && ctx.Err() == nil {
					logger.Error(err, "Observe runtime progress", "codespace", cs.Name)
				} else if stage != "" {
					_ = c.Logs.Append(cs, stage, []*codespacev1.LogLine{{TimestampUnixNano: time.Now().UnixNano(), Message: message}})
				}
			}
			generation, _ := reported.Load(cs.UID)
			pending := cs.Status.SettledOperationVersion < cs.Spec.Operation.Version && (cs.Status.Result != nil || meta.IsStatusConditionTrue(cs.Status.Conditions, "Stopped"))
			if refresh || pending || generation != cs.Status.MetadataGeneration {
				queue.Add(client.ObjectKeyFromObject(cs))
			}
		}
		reported.Range(func(key, _ any) bool {
			if !current[key.(types.UID)] {
				reported.Delete(key)
			}
			return true
		})
		c.Samples.RetainSite(uid, observed.Items)
		if refresh {
			metadataAt = time.Now()
		}
	}
}
