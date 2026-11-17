// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"sync"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	kcache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

func watchCoordinationChanges(mgr manager.Manager, componentChanges, siteChanges *ChangeNotifier) error {
	notify := func(value any) {
		if tombstone, ok := value.(kcache.DeletedFinalStateUnknown); ok {
			value = tombstone.Obj
		}
		object, ok := value.(client.Object)
		if !ok {
			return
		}
		if component, ok := object.(*api.CodespaceComponent); ok {
			componentChanges.Notify(component.UID)
			return
		}
		if uid := object.GetLabels()[ComponentUIDLabel]; uid != "" {
			componentChanges.Notify(types.UID(uid))
		}
		var site *api.GiteaSite
		if current, ok := object.(*api.GiteaSite); ok {
			site = current
			siteChanges.Notify("")
		} else {
			uid := object.GetLabels()[SiteUIDLabel]
			if runtime, ok := object.(*api.Codespace); ok {
				uid = string(runtime.Spec.Site.UID)
			}
			if uid == "" {
				return
			}
			var sites api.GiteaSiteList
			if err := mgr.GetClient().List(context.Background(), &sites, client.MatchingFields{IdentityUIDIndex: uid}); err != nil || len(sites.Items) != 1 {
				return
			}
			site = &sites.Items[0]
		}
		siteChanges.Notify(site.UID)
		componentChanges.Notify(site.Spec.Gateway.UID)
		for _, cache := range site.Spec.Caches {
			componentChanges.Notify(cache.UID)
		}
	}
	for _, object := range []client.Object{&api.GiteaSite{}, &api.Codespace{}, &api.CodespaceComponent{}, &corev1.Pod{}, &corev1.Secret{}} {
		informer, err := mgr.GetCache().GetInformer(context.Background(), object)
		if err != nil {
			return err
		}
		if _, err := informer.AddEventHandler(kcache.ResourceEventHandlerFuncs{
			AddFunc: notify,
			UpdateFunc: func(previous, current any) {
				if coordinationUpdateChanged(previous, current) {
					notify(previous)
					notify(current)
				}
			},
			DeleteFunc: notify,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ChangeNotifier coalesces Kubernetes watch events for connected components.
// Subscribers always rebuild an authoritative snapshot, so event payloads are
// deliberately unnecessary.
type ChangeNotifier struct {
	mu          sync.Mutex
	subscribers map[chan struct{}]types.UID
}

func (n *ChangeNotifier) Subscribe(uid types.UID) (<-chan struct{}, func()) {
	channel := make(chan struct{}, 1)
	n.mu.Lock()
	if n.subscribers == nil {
		n.subscribers = make(map[chan struct{}]types.UID)
	}
	n.subscribers[channel] = uid
	n.mu.Unlock()
	return channel, func() {
		n.mu.Lock()
		delete(n.subscribers, channel)
		n.mu.Unlock()
	}
}

func (n *ChangeNotifier) Notify(uid types.UID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for channel, scope := range n.subscribers {
		if scope != uid {
			continue
		}
		select {
		case channel <- struct{}{}:
		default:
		}
	}
}

// Reporting counters and boot progress do not change routes or operation scheduling.
func coordinationUpdateChanged(previous, current any) bool {
	old, oldOK := previous.(client.Object)
	next, nextOK := current.(client.Object)
	if !oldOK || !nextOK {
		return true
	}
	if old.GetUID() != next.GetUID() || !equality.Semantic.DeepEqual(old.GetDeletionTimestamp(), next.GetDeletionTimestamp()) ||
		!equality.Semantic.DeepEqual(old.GetOwnerReferences(), next.GetOwnerReferences()) || !equality.Semantic.DeepEqual(old.GetLabels(), next.GetLabels()) {
		return true
	}
	switch old := old.(type) {
	case *api.GiteaSite:
		next, ok := next.(*api.GiteaSite)
		if !ok {
			return true
		}
		oldStatus, nextStatus := old.Status, next.Status
		oldStatus.InventoryGeneration, nextStatus.InventoryGeneration = 0, 0
		return old.Generation != next.Generation || !equality.Semantic.DeepEqual(old.Spec, next.Spec) || !equality.Semantic.DeepEqual(oldStatus, nextStatus)
	case *api.Codespace:
		next, ok := next.(*api.Codespace)
		if !ok {
			return true
		}
		oldStatus, nextStatus := old.Status, next.Status
		oldStatus.MetadataGeneration, nextStatus.MetadataGeneration = 0, 0
		oldStatus.Boot, nextStatus.Boot = nil, nil
		return !equality.Semantic.DeepEqual(old.Spec, next.Spec) || !equality.Semantic.DeepEqual(oldStatus, nextStatus)
	case *corev1.Pod:
		next, ok := next.(*corev1.Pod)
		if !ok {
			return true
		}
		return old.Status.Phase != next.Status.Phase || old.Status.PodIP != next.Status.PodIP || !equality.Semantic.DeepEqual(old.Status.PodIPs, next.Status.PodIPs)
	case *api.CodespaceComponent:
		next, ok := next.(*api.CodespaceComponent)
		return !ok || old.Generation != next.Generation || !equality.Semantic.DeepEqual(old.Spec, next.Spec)
	default:
		return true
	}
}
