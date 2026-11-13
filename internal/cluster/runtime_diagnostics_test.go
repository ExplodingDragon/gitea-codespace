// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/logbuffer"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRuntimeDiagnosticsUploadContainerLogsAndResourceFacts(t *testing.T) {
	var mu sync.Mutex
	var received []string
	remote := &operationLogRemote{update: func(_ context.Context, request *codespacev1.UpdateLogRequest) (*codespacev1.UpdateLogResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		for _, line := range request.Lines {
			if strings.ContainsAny(line.Message, "\r\n") || !utf8.ValidString(line.Message) || len(line.Message) > logbuffer.MaxLineBytes {
				return nil, connect.NewError(connect.CodeInvalidArgument, "invalid log line")
			}
			received = append(received, line.Message)
		}
		return &codespacev1.UpdateLogResponse{NextOffset: int64(len(received))}, nil
	}}
	logs, cs := testOperationLogs(t, remote)
	credential := "quoted\"\\secret\nsecond-line"
	cs.Name = "runtime"
	cs.Spec.Runtime.Debug = true
	cs.Status.Pod.Name, cs.Status.Pod.UID = "pod", "pod-uid"
	cs.Status.Volume.Name, cs.Status.Volume.UID = "data", "volume-uid"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: cs.Namespace, UID: cs.Status.Pod.UID, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cs, cs.GroupVersionKind())}},
		Spec:       corev1.PodSpec{NodeName: "worker"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Message: credential,
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "prepare", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				{Name: "interrupted", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}},
			},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "runtime", RestartCount: 1, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: cs.Namespace, UID: cs.Status.Volume.UID, Labels: map[string]string{RuntimeUIDLabel: string(cs.UID)}}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse}}}}
	event := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "scheduled", Namespace: cs.Namespace}, InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod.Name, UID: pod.UID}, Reason: "Scheduled", Message: "placed on worker\r\n" + credential + "\n" + strings.Repeat("x", logbuffer.MaxLineBytes) + "\nremaining event detail", LastTimestamp: metav1.Now()}
	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pod, pvc, node, event).WithIndex(&corev1.Event{}, "involvedObject.uid", func(object client.Object) []string {
		return []string{string(object.(*corev1.Event).InvolvedObject.UID)}
	}).Build()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("limitBytes") != "65536" || r.URL.Query().Get("timestamps") != "true" {
			http.Error(w, "missing bounds", 400)
			return
		}
		if r.URL.Query().Get("previous") == "true" {
			_, _ = fmt.Fprint(w, "partial-boundary\n"+strings.Repeat("old-output\n", diagnosticLogBytes/10)+"truncated-secret")
			return
		}
		if r.URL.Query().Get("container") == "interrupted" {
			w.Header().Set("Content-Length", "1000")
			_, _ = fmt.Fprint(w, "complete-before-disconnect\npartial-credential")
			return
		}
		_, _ = fmt.Fprintf(w, "2026-11-13T00:00:00Z %s actual output secret-value Authorization: Bearer private-token\n", r.URL.Query().Get("container"))
	}))
	defer server.Close()
	core, err := coreclient.NewForConfig(&rest.Config{Host: server.URL})
	require.NoError(t, err)
	d := &runtimeDiagnostics{Reader: reader, Pods: core, Logs: logs}
	d.credentials(cs, []string{"secret-value", credential})
	require.NoError(t, logs.Append(cs, "", nil))
	d.capture(t.Context(), cs, pod, "state")
	d.capture(t.Context(), cs, pod, "state")
	require.EqualValues(t, 4, calls.Load(), "unchanged observations do not fetch logs again")
	pod.Status.Phase = corev1.PodFailed
	d.capture(t.Context(), cs, pod, "state")
	require.EqualValues(t, 8, calls.Load())
	require.NoError(t, logs.Append(cs, "", []*codespacev1.LogLine{{TimestampUnixNano: 1, Message: "output after diagnostics"}}))
	require.Eventually(t, func() bool { return logs.Finish(cs) }, 5*time.Second, 10*time.Millisecond)
	mu.Lock()
	output := strings.Join(received, "\n")
	mu.Unlock()
	for _, expected := range []string{"prepare actual output [redacted]", "runtime actual output [redacted]", "previous=true", "log byte limit reached", "Scheduled", "MemoryPressure", "Bound", `"phase":"Failed"`, `"message":"[redacted]"`, "complete-before-disconnect", "logs incomplete", "remaining event detail", "output after diagnostics", "line omitted"} {
		require.Contains(t, output, expected)
	}
	require.NotContains(t, output, "secret-value")
	require.NotContains(t, output, "private-token")
	require.NotContains(t, output, "partial-boundary")
	require.NotContains(t, output, "partial-credential")
	d.forget(client.ObjectKeyFromObject(cs))
	require.Empty(t, d.observed)
}
