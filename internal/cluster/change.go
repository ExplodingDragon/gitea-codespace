// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import "sync"

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
