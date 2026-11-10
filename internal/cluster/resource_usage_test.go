// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"testing"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRuntimeUsagePublication(t *testing.T) {
	cs := &api.Codespace{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-example", UID: "runtime-uid"}, Spec: api.CodespaceSpec{Site: api.ResourceReference{UID: "site-uid"}, RuntimeUUID: "933ef4a9-54c7-4f36-aad9-32ea72c4c986", Operation: api.Operation{Type: "create", Version: 1}}, Status: api.CodespaceStatus{Bound: true, Pod: api.ObservedResource{Name: "pod", UID: "pod-uid"}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(cs).WithObjects(cs).Build()
	control := &AgentControlServer{Client: c}
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cs), cs))
	report := &agentv1.AgentReport{OperationRversion: 1, Boot: &codespacev1.RuntimeBoot{OperationRversion: 1, Stage: codespacev1.RuntimeBootStage_RUNTIME_BOOT_STAGE_PREPARE_RUNTIME, StartedUnix: 1, LastUpdateUnix: 1}}
	require.NoError(t, control.saveReport(t.Context(), cs, report))
	version := cs.ResourceVersion
	usage := &codespacev1.RuntimeResourceUsage{ObservedUnix: time.Now().Unix(), Cpu: &codespacev1.RuntimeCPUUsage{UsedMillicores: 100}, Memory: &codespacev1.RuntimeMemoryUsage{UsedBytes: 1024}, Disk: &codespacev1.RuntimeDiskUsage{UsedBytes: 2048, LimitBytes: 4096}}
	control.Samples.Record(cs, usage)
	require.NoError(t, control.saveReport(t.Context(), cs, report))
	require.Equal(t, version, cs.ResourceVersion)
	require.EqualValues(t, 1, cs.Status.MetadataGeneration)
	operations := Operations{Client: c, Samples: &control.Samples}
	remote := &operationRemote{metadata: func(request *codespacev1.ReportRuntimeMetadataRequest) error {
		require.EqualValues(t, 1, request.MetadataGeneration)
		require.EqualValues(t, 100, request.Metadata.ResourceUsage.Cpu.UsedMillicores)
		require.Zero(t, request.Metadata.ResourceUsage.Cpu.LimitMillicores)
		require.Zero(t, request.Metadata.ResourceUsage.Memory.LimitBytes)
		request.Metadata.ResourceUsage.Cpu.UsedMillicores = 999
		return nil
	}}
	require.NoError(t, operations.Flush(t.Context(), cs, remote))
	require.NoError(t, operations.Flush(t.Context(), cs, remote))
	require.Equal(t, version, cs.ResourceVersion)

	usage.ObservedUnix++
	usage.Disk.LimitBytes = 0
	control.Samples.Record(cs, usage)
	require.EqualValues(t, 100, control.Samples.Load(cs).Cpu.UsedMillicores)
	usage.ObservedUnix -= 2
	usage.Disk.LimitBytes = 4096
	usage.Cpu.UsedMillicores = 200
	control.Samples.Record(cs, usage)
	require.EqualValues(t, 100, control.Samples.Load(cs).Cpu.UsedMillicores)
	cs.Status.Pod.UID = "replacement-pod"
	require.Nil(t, control.Samples.Load(cs))
	control.Samples.Record(cs, usage)
	cs.Status.Volume.UID = "replacement-volume"
	require.Nil(t, control.Samples.Load(cs))
	control.Samples.Record(cs, usage)
	control.Samples.RetainSite(cs.Spec.Site.UID, nil)
	require.Nil(t, control.Samples.Load(cs))
}

func TestResourceSamplesSelectsSourcesAndCombinesDiskAvailability(t *testing.T) {
	now := time.Now().Unix()
	cs := &api.Codespace{ObjectMeta: metav1.ObjectMeta{UID: "runtime-uid"}, Spec: api.CodespaceSpec{Site: api.ResourceReference{UID: "site-uid"}, Operation: api.Operation{Version: 1}}, Status: api.CodespaceStatus{Pod: api.ObservedResource{UID: "pod-uid"}, Volume: api.ObservedResource{UID: "volume-uid"}}}
	samples := new(ResourceSamples)
	agent := &codespacev1.RuntimeResourceUsage{
		ObservedUnix: now,
		Cpu:          &codespacev1.RuntimeCPUUsage{UsedMillicores: 100, LimitMillicores: 500},
		Memory:       &codespacev1.RuntimeMemoryUsage{UsedBytes: 1000, LimitBytes: 2000},
		Disk:         &codespacev1.RuntimeDiskUsage{UsedBytes: 100, LimitBytes: 1000},
	}
	kubernetes := &codespacev1.RuntimeResourceUsage{
		ObservedUnix: now,
		Cpu:          &codespacev1.RuntimeCPUUsage{UsedMillicores: 200, LimitMillicores: 400},
		Memory:       &codespacev1.RuntimeMemoryUsage{UsedBytes: 2000, LimitBytes: 3000},
		Disk:         &codespacev1.RuntimeDiskUsage{UsedBytes: 600, LimitBytes: 1000},
	}
	samples.Record(cs, agent)
	samples.RecordKubernetes(cs, kubernetes)
	samples.RecordPodLimits(cs, 750, 4000)

	usage := samples.Load(cs)
	require.NotNil(t, usage)
	// Kubernetes wins for CPU/memory, while the lower disk available space wins.
	require.EqualValues(t, 200, usage.Cpu.UsedMillicores)
	require.EqualValues(t, 2000, usage.Memory.UsedBytes)
	require.EqualValues(t, 600, usage.Disk.UsedBytes)
	require.EqualValues(t, 750, usage.Cpu.LimitMillicores)
	require.EqualValues(t, 4000, usage.Memory.LimitBytes)
}

func TestResourceSamplesFallsBackPerMetricAndExpiresPublishedSnapshot(t *testing.T) {
	now := time.Now().Unix()
	cs := &api.Codespace{ObjectMeta: metav1.ObjectMeta{UID: "runtime-uid"}, Spec: api.CodespaceSpec{Site: api.ResourceReference{UID: "site-uid"}, Operation: api.Operation{Version: 1}}, Status: api.CodespaceStatus{Pod: api.ObservedResource{UID: "pod-uid"}, Volume: api.ObservedResource{UID: "volume-uid"}}}
	samples := new(ResourceSamples)
	samples.Record(cs, &codespacev1.RuntimeResourceUsage{
		ObservedUnix: now - 10,
		Cpu:          &codespacev1.RuntimeCPUUsage{UsedMillicores: 100},
		Memory:       &codespacev1.RuntimeMemoryUsage{UsedBytes: 1000},
		Disk:         &codespacev1.RuntimeDiskUsage{UsedBytes: 100, LimitBytes: 1000},
	})
	require.NotNil(t, samples.Load(cs))

	// A Kubernetes CPU-only sample must not make missing Kubernetes metrics erase
	// fresh Agent samples; each metric falls back independently.
	samples.Record(cs, &codespacev1.RuntimeResourceUsage{ObservedUnix: now, Memory: &codespacev1.RuntimeMemoryUsage{UsedBytes: 1000}, Disk: &codespacev1.RuntimeDiskUsage{UsedBytes: 100, LimitBytes: 1000}})
	samples.RecordKubernetes(cs, &codespacev1.RuntimeResourceUsage{ObservedUnix: now, Cpu: &codespacev1.RuntimeCPUUsage{UsedMillicores: 250}})
	samples.RecordPodLimits(cs, 0, 0)
	usage := samples.Load(cs)
	require.NotNil(t, usage)
	require.EqualValues(t, 250, usage.Cpu.UsedMillicores)
	require.EqualValues(t, 1000, usage.Memory.UsedBytes)

	samples.mu.Lock()
	sample := samples.samples[cs.UID]
	stale := now - int64(resourceSampleLifetime/time.Second) - 1
	sample.agent.memory.observed = stale
	sample.agent.disk.observed = stale
	sample.kubernetes.cpu.observed = stale
	sample.published.ObservedUnix = stale
	samples.samples[cs.UID] = sample
	samples.mu.Unlock()
	require.Nil(t, samples.Load(cs))
}

func TestResourceSamplesRequiresObservedLimits(t *testing.T) {
	now := time.Now().Unix()
	cs := &api.Codespace{ObjectMeta: metav1.ObjectMeta{UID: "runtime-uid"}, Spec: api.CodespaceSpec{Site: api.ResourceReference{UID: "site-uid"}, Operation: api.Operation{Version: 1}}, Status: api.CodespaceStatus{Pod: api.ObservedResource{UID: "pod-uid"}, Volume: api.ObservedResource{UID: "volume-uid"}}}
	samples := new(ResourceSamples)
	samples.RecordKubernetes(cs, &codespacev1.RuntimeResourceUsage{
		ObservedUnix: now,
		Cpu:          &codespacev1.RuntimeCPUUsage{UsedMillicores: 200},
		Memory:       &codespacev1.RuntimeMemoryUsage{UsedBytes: 1000},
		Disk:         &codespacev1.RuntimeDiskUsage{UsedBytes: 100, LimitBytes: 1000},
	})
	require.Nil(t, samples.Load(cs), "Kubernetes usage without Pod limits or Agent fallback must not use zero as a limit")

	agent := &codespacev1.RuntimeResourceUsage{
		ObservedUnix: now,
		Cpu:          &codespacev1.RuntimeCPUUsage{UsedMillicores: 100, LimitMillicores: 500},
		Memory:       &codespacev1.RuntimeMemoryUsage{UsedBytes: 1000, LimitBytes: 2000},
		Disk:         &codespacev1.RuntimeDiskUsage{UsedBytes: 100, LimitBytes: 1000},
	}
	samples.Record(cs, agent)
	usage := samples.Load(cs)
	require.NotNil(t, usage)
	require.EqualValues(t, 200, usage.Cpu.UsedMillicores)
	require.EqualValues(t, 500, usage.Cpu.LimitMillicores)
	require.EqualValues(t, 2000, usage.Memory.LimitBytes)
}

func TestResourceSamplesPublicationOrder(t *testing.T) {
	now := time.Now().Unix()
	cs := &api.Codespace{ObjectMeta: metav1.ObjectMeta{UID: "runtime-uid"}, Status: api.CodespaceStatus{Pod: api.ObservedResource{UID: "pod-uid"}}}
	samples := new(ResourceSamples)
	agent := &codespacev1.RuntimeResourceUsage{ObservedUnix: now - 30, Cpu: &codespacev1.RuntimeCPUUsage{UsedMillicores: 100}, Memory: &codespacev1.RuntimeMemoryUsage{}, Disk: &codespacev1.RuntimeDiskUsage{LimitBytes: 1000}}
	samples.Record(cs, agent)
	require.EqualValues(t, 100, samples.Load(cs).Cpu.UsedMillicores)

	kubernetes := &codespacev1.RuntimeResourceUsage{ObservedUnix: now - 40, Cpu: &codespacev1.RuntimeCPUUsage{UsedMillicores: 200}}
	samples.RecordKubernetes(cs, kubernetes)
	require.EqualValues(t, 100, samples.Load(cs).Cpu.UsedMillicores, "wait for the source-switch timestamp to advance")
	kubernetes.ObservedUnix = now - 20
	samples.RecordKubernetes(cs, kubernetes)
	require.EqualValues(t, 100, samples.Load(cs).Cpu.UsedMillicores, "disk and limit observations still determine the snapshot time")
	agent.ObservedUnix = now
	samples.Record(cs, agent)
	usage := samples.Load(cs)
	require.EqualValues(t, 200, usage.Cpu.UsedMillicores)
	require.Equal(t, now-20, usage.ObservedUnix)

	kubernetes.ObservedUnix = now - 25
	kubernetes.Cpu.UsedMillicores = 900
	samples.RecordKubernetes(cs, kubernetes)
	sample := samples.samples[cs.UID]
	require.EqualValues(t, 200, sample.snapshot(now+95).Cpu.UsedMillicores, "the newer Kubernetes observation remains valid")
	require.EqualValues(t, 100, sample.snapshot(now+100).Cpu.UsedMillicores, "Kubernetes expiry falls back to the fresh Agent sample")
	require.Nil(t, sample.snapshot(now+120), "all source samples expire at the same boundary")
}

func TestResourceSamplesRetainSiteIsolation(t *testing.T) {
	now := time.Now().Unix()
	newCodespace := func(uid, site string) *api.Codespace {
		return &api.Codespace{ObjectMeta: metav1.ObjectMeta{UID: types.UID(uid)}, Spec: api.CodespaceSpec{Site: api.ResourceReference{UID: types.UID(site)}, Operation: api.Operation{Version: 1}}, Status: api.CodespaceStatus{Pod: api.ObservedResource{UID: types.UID(uid + "-pod")}, Volume: api.ObservedResource{UID: types.UID(uid + "-volume")}}}
	}
	usage := &codespacev1.RuntimeResourceUsage{ObservedUnix: now, Cpu: &codespacev1.RuntimeCPUUsage{}, Memory: &codespacev1.RuntimeMemoryUsage{}, Disk: &codespacev1.RuntimeDiskUsage{LimitBytes: 1000}}
	first, second := newCodespace("first", "site-a"), newCodespace("second", "site-b")
	samples := new(ResourceSamples)
	samples.Record(first, usage)
	samples.Record(second, usage)
	samples.RetainSite(types.UID("site-a"), []api.Codespace{*first})
	require.NotNil(t, samples.Load(second))
}
