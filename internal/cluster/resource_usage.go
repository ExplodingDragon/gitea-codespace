// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"sync"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/types"
)

type resourceSample struct {
	site, pod types.UID
	operation int64
	usage     *codespacev1.RuntimeResourceUsage
}

// ResourceSamples keeps replaceable observations out of Kubernetes status writes.
type ResourceSamples struct {
	mu      sync.Mutex
	samples map[types.UID]resourceSample
}

func (s *ResourceSamples) Record(cs *api.Codespace, usage *codespacev1.RuntimeResourceUsage) {
	if s == nil || usage == nil || usage.Cpu == nil || usage.Memory == nil || usage.Disk == nil || cs.Status.Pod.UID == "" {
		return
	}
	now := time.Now().Unix()
	if usage.ObservedUnix < now-120 || usage.ObservedUnix > now+30 || usage.Cpu.UsedMillicores < 0 || usage.Cpu.LimitMillicores < 0 || usage.Memory.UsedBytes < 0 || usage.Memory.LimitBytes < 0 || usage.Disk.UsedBytes < 0 || usage.Disk.LimitBytes < 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.samples[cs.UID]
	if previous.pod == cs.Status.Pod.UID && previous.operation == cs.Spec.Operation.Version && previous.usage != nil && previous.usage.ObservedUnix >= usage.ObservedUnix {
		return
	}
	if s.samples == nil {
		s.samples = make(map[types.UID]resourceSample)
	}
	s.samples[cs.UID] = resourceSample{site: cs.Spec.Site.UID, pod: cs.Status.Pod.UID, operation: cs.Spec.Operation.Version, usage: proto.Clone(usage).(*codespacev1.RuntimeResourceUsage)}
}

func (s *ResourceSamples) Load(cs *api.Codespace) *codespacev1.RuntimeResourceUsage {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sample, ok := s.samples[cs.UID]
	if !ok {
		return nil
	}
	if sample.pod != cs.Status.Pod.UID || sample.operation != cs.Spec.Operation.Version || sample.usage.ObservedUnix < time.Now().Unix()-120 {
		delete(s.samples, cs.UID)
		return nil
	}
	return proto.Clone(sample.usage).(*codespacev1.RuntimeResourceUsage)
}

func (s *ResourceSamples) RetainSite(site types.UID, rows []api.Codespace) {
	if s == nil {
		return
	}
	active := make(map[types.UID]bool, len(rows))
	for _, cs := range rows {
		active[cs.UID] = cs.DeletionTimestamp.IsZero() && cs.Status.Pod.UID != ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for uid, sample := range s.samples {
		if sample.site == site && (!active[uid] || sample.usage.ObservedUnix < time.Now().Unix()-120) {
			delete(s.samples, uid)
		}
	}
}
