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

const resourceSampleLifetime = 120 * time.Second

type resourceMetricSample struct {
	used     int64
	limit    int64
	observed int64
}

type resourceMetrics struct {
	cpu, memory, disk *resourceMetricSample
}

type resourceLimitsSample struct {
	cpu      int64
	memory   int64
	observed int64
}

type resourceSample struct {
	site, pod, volume types.UID
	operation         int64

	agent, kubernetes resourceMetrics
	kubernetesLimits  *resourceLimitsSample

	published *codespacev1.RuntimeResourceUsage
}

// ResourceSamples keeps replaceable observations out of Kubernetes status writes.
// Samples from each source remain separate so a temporary collection failure does
// not overwrite a still-fresh fallback metric.
type ResourceSamples struct {
	mu      sync.Mutex
	samples map[types.UID]resourceSample
}

// Record stores an Agent sample. The wire message may contain any subset of the
// three metrics; missing metrics retain their previous source sample.
func (s *ResourceSamples) Record(cs *api.Codespace, usage *codespacev1.RuntimeResourceUsage) {
	s.record(cs, usage, false)
}

// RecordKubernetes stores a Kubernetes sample. CPU, memory and disk may be
// recorded in separate calls because their source timestamps differ.
func (s *ResourceSamples) RecordKubernetes(cs *api.Codespace, usage *codespacev1.RuntimeResourceUsage) {
	s.record(cs, usage, true)
}

func (s *ResourceSamples) record(cs *api.Codespace, usage *codespacev1.RuntimeResourceUsage, kubernetes bool) {
	if s == nil || cs == nil || usage == nil || cs.Status.Pod.UID == "" || !validResourceTimestamp(usage.ObservedUnix, time.Now().Unix()) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sample := s.currentLocked(cs)
	source := &sample.agent
	if kubernetes {
		source = &sample.kubernetes
	}
	observed := usage.ObservedUnix
	if cpu := usage.Cpu; cpu != nil && cpu.UsedMillicores >= 0 && cpu.LimitMillicores >= 0 && (source.cpu == nil || observed > source.cpu.observed) {
		source.cpu = &resourceMetricSample{used: cpu.UsedMillicores, limit: cpu.LimitMillicores, observed: observed}
	}
	if memory := usage.Memory; memory != nil && memory.UsedBytes >= 0 && memory.LimitBytes >= 0 && (source.memory == nil || observed > source.memory.observed) {
		source.memory = &resourceMetricSample{used: memory.UsedBytes, limit: memory.LimitBytes, observed: observed}
	}
	if disk := usage.Disk; disk != nil && disk.UsedBytes >= 0 && disk.LimitBytes > 0 && disk.UsedBytes <= disk.LimitBytes && (source.disk == nil || observed > source.disk.observed) {
		source.disk = &resourceMetricSample{used: disk.UsedBytes, limit: disk.LimitBytes, observed: observed}
	}
	s.samples[cs.UID] = sample
}

// RecordPodLimits stores the current Runtime Pod limits. A zero limit means the
// Pod was read successfully and that resource is explicitly unlimited.
func (s *ResourceSamples) RecordPodLimits(cs *api.Codespace, cpuLimit, memoryLimit int64) {
	if s == nil || cs == nil || cs.Status.Pod.UID == "" || cpuLimit < 0 || memoryLimit < 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sample := s.currentLocked(cs)
	sample.kubernetesLimits = &resourceLimitsSample{cpu: cpuLimit, memory: memoryLimit, observed: time.Now().Unix()}
	s.samples[cs.UID] = sample
}

// Load returns the newest complete snapshot compatible with the existing wire
// contract. A source switch can produce an older group timestamp; in that case
// the previous complete snapshot remains visible while it is still fresh.
func (s *ResourceSamples) Load(cs *api.Codespace) *codespacev1.RuntimeResourceUsage {
	if s == nil || cs == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sample, ok := s.samples[cs.UID]
	if !ok || !sampleMatches(sample, cs) {
		delete(s.samples, cs.UID)
		return nil
	}
	now := time.Now().Unix()
	if usage := sample.snapshot(now); usage != nil && (sample.published == nil || usage.ObservedUnix > sample.published.ObservedUnix) {
		sample.published = usage
		s.samples[cs.UID] = sample
	}
	if sample.published != nil && validResourceTimestamp(sample.published.ObservedUnix, now) {
		return proto.Clone(sample.published).(*codespacev1.RuntimeResourceUsage)
	}
	return nil
}

func (s *ResourceSamples) currentLocked(cs *api.Codespace) resourceSample {
	if s.samples == nil {
		s.samples = make(map[types.UID]resourceSample)
	}
	sample := s.samples[cs.UID]
	if !sampleMatches(sample, cs) {
		sample = resourceSample{site: cs.Spec.Site.UID, pod: cs.Status.Pod.UID, volume: cs.Status.Volume.UID, operation: cs.Spec.Operation.Version}
	}
	return sample
}

func sampleMatches(sample resourceSample, cs *api.Codespace) bool {
	return sample.site == cs.Spec.Site.UID && sample.pod == cs.Status.Pod.UID && sample.volume == cs.Status.Volume.UID && sample.operation == cs.Spec.Operation.Version
}

func (s resourceSample) snapshot(now int64) *codespacev1.RuntimeResourceUsage {
	cpu := selectFreshMetric(s.kubernetes.cpu, s.agent.cpu, now)
	memory := selectFreshMetric(s.kubernetes.memory, s.agent.memory, now)
	disk := s.diskSnapshot(now)
	if cpu == nil || memory == nil || disk == nil {
		return nil
	}
	limits := s.kubernetesLimits
	if limits == nil || !validResourceTimestamp(limits.observed, now) {
		agentCPU := freshMetric(s.agent.cpu, now)
		agentMemory := freshMetric(s.agent.memory, now)
		if agentCPU == nil || agentMemory == nil {
			return nil
		}
		limits = &resourceLimitsSample{cpu: agentCPU.limit, memory: agentMemory.limit, observed: min(agentCPU.observed, agentMemory.observed)}
	}
	return &codespacev1.RuntimeResourceUsage{
		Cpu:          &codespacev1.RuntimeCPUUsage{UsedMillicores: cpu.used, LimitMillicores: limits.cpu},
		Memory:       &codespacev1.RuntimeMemoryUsage{UsedBytes: memory.used, LimitBytes: limits.memory},
		Disk:         &codespacev1.RuntimeDiskUsage{UsedBytes: disk.used, LimitBytes: disk.limit},
		ObservedUnix: min(cpu.observed, memory.observed, disk.observed, limits.observed),
	}
}

func (s resourceSample) diskSnapshot(now int64) *resourceMetricSample {
	agent := freshMetric(s.agent.disk, now)
	kubernetes := freshMetric(s.kubernetes.disk, now)
	if agent == nil {
		return kubernetes
	}
	if kubernetes == nil {
		return agent
	}
	available := min(agent.limit-agent.used, kubernetes.limit-kubernetes.used)
	return &resourceMetricSample{
		used:     agent.limit - available,
		limit:    agent.limit,
		observed: min(agent.observed, kubernetes.observed),
	}
}

func selectFreshMetric(preferred, fallback *resourceMetricSample, now int64) *resourceMetricSample {
	if metric := freshMetric(preferred, now); metric != nil {
		return metric
	}
	return freshMetric(fallback, now)
}

func freshMetric(metric *resourceMetricSample, now int64) *resourceMetricSample {
	if metric == nil || !validResourceTimestamp(metric.observed, now) {
		return nil
	}
	return metric
}

func validResourceTimestamp(observed, now int64) bool {
	return observed > 0 && observed <= now+30 && observed > now-int64(resourceSampleLifetime/time.Second)
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
	now := time.Now().Unix()
	for uid, sample := range s.samples {
		if sample.site != site {
			continue
		}
		fresh := sample.kubernetesLimits != nil && validResourceTimestamp(sample.kubernetesLimits.observed, now)
		for _, metric := range []*resourceMetricSample{sample.agent.cpu, sample.kubernetes.cpu, sample.agent.memory, sample.kubernetes.memory, sample.agent.disk, sample.kubernetes.disk} {
			if freshMetric(metric, now) != nil {
				fresh = true
				break
			}
		}
		if !active[uid] || !fresh {
			delete(s.samples, uid)
		}
	}
}
