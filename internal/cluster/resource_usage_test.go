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
	usage := &codespacev1.RuntimeResourceUsage{ObservedUnix: time.Now().Unix(), Cpu: &codespacev1.RuntimeCPUUsage{UsedMillicores: 100}, Memory: &codespacev1.RuntimeMemoryUsage{UsedBytes: 1024}, Disk: &codespacev1.RuntimeDiskUsage{UsedBytes: 2048}}
	control.Samples.Record(cs, usage)
	require.NoError(t, control.saveReport(t.Context(), cs, report))
	require.Equal(t, version, cs.ResourceVersion)
	require.EqualValues(t, 1, cs.Status.MetadataGeneration)
	operations := Operations{Client: c, Samples: &control.Samples}
	remote := &operationRemote{metadata: func(request *codespacev1.ReportRuntimeMetadataRequest) error {
		require.EqualValues(t, 1, request.MetadataGeneration)
		require.EqualValues(t, 100, request.Metadata.ResourceUsage.Cpu.UsedMillicores)
		request.Metadata.ResourceUsage.Cpu.UsedMillicores = 999
		return nil
	}}
	require.NoError(t, operations.Flush(t.Context(), cs, remote))
	require.NoError(t, operations.Flush(t.Context(), cs, remote))
	require.Equal(t, version, cs.ResourceVersion)

	usage.ObservedUnix--
	usage.Cpu.UsedMillicores = 200
	control.Samples.Record(cs, usage)
	require.EqualValues(t, 100, control.Samples.Load(cs).Cpu.UsedMillicores)
	cs.Status.Pod.UID = "replacement-pod"
	require.Nil(t, control.Samples.Load(cs))
	control.Samples.Record(cs, usage)
	control.Samples.RetainSite(cs.Spec.Site.UID, nil)
	require.Nil(t, control.Samples.Load(cs))
}
