// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"sync"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ChangeNotifier coalesces Kubernetes watch events for connected components.
// Subscribers always rebuild an authoritative snapshot, so event payloads are
// deliberately unnecessary.
type ChangeNotifier struct {
	mu          sync.Mutex
	subscribers map[chan struct{}]struct{}
}

func (n *ChangeNotifier) Subscribe() (<-chan struct{}, func()) {
	channel := make(chan struct{}, 1)
	n.mu.Lock()
	if n.subscribers == nil {
		n.subscribers = make(map[chan struct{}]struct{})
	}
	n.subscribers[channel] = struct{}{}
	n.mu.Unlock()
	return channel, func() {
		n.mu.Lock()
		delete(n.subscribers, channel)
		n.mu.Unlock()
	}
}

func (n *ChangeNotifier) Notify() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for channel := range n.subscribers {
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
	case *corev1.ConfigMap:
		next, ok := next.(*corev1.ConfigMap)
		return !ok || !equality.Semantic.DeepEqual(old.Data, next.Data) || !equality.Semantic.DeepEqual(old.BinaryData, next.BinaryData)
	default:
		return true
	}
}
