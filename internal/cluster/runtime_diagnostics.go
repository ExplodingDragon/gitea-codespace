// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"gitea.dev/codespace/internal/logbuffer"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const diagnosticLogBytes = 64 * 1024

// Diagnostics are process-local observations, not lifecycle state. Losing a
// snapshot or its deduplication cache never changes resource cleanup.
type runtimeDiagnostics struct {
	Reader   client.Reader
	Pods     coreclient.CoreV1Interface
	Logs     *operationLogs
	mu       sync.Mutex
	observed map[types.NamespacedName]*diagnosticObservation
}

type diagnosticObservation struct {
	uid      types.UID
	stages   map[string][32]byte
	redactor *logbuffer.Redactor
}

func (d *runtimeDiagnostics) forget(key types.NamespacedName) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.observed, key)
}

func (d *runtimeDiagnostics) observation(cs *api.Codespace) *diagnosticObservation {
	key := client.ObjectKeyFromObject(cs)
	if d.observed == nil {
		d.observed = make(map[types.NamespacedName]*diagnosticObservation)
	}
	entry := d.observed[key]
	if entry == nil || entry.uid != cs.UID {
		entry = &diagnosticObservation{uid: cs.UID, stages: make(map[string][32]byte)}
		d.observed[key] = entry
	}
	return entry
}

func (d *runtimeDiagnostics) credentials(cs *api.Codespace, values []string) {
	if d == nil || !cs.Spec.Runtime.Debug {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.observation(cs).redactor = logbuffer.NewRedactor(values)
}

func (d *runtimeDiagnostics) emit(ctx context.Context, cs *api.Codespace, stage string, lines []string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	var redactor *logbuffer.Redactor
	if entry := d.observed[client.ObjectKeyFromObject(cs)]; entry != nil && entry.uid == cs.UID {
		redactor = entry.redactor
	}
	d.mu.Unlock()
	logger := log.FromContext(ctx).WithValues("codespace", cs.Name, "namespace", cs.Namespace, "runtime", cs.Spec.RuntimeUUID, "operation", cs.Spec.Operation.Type, "version", cs.Spec.Operation.Version, "stage", stage)
	batch := make([]*codespacev1.LogLine, 0, len(lines))
	for _, line := range lines {
		message := redactor.Redact(line)
		logger.Info("Runtime diagnostic", "detail", message)
		batch = append(batch, &codespacev1.LogLine{TimestampUnixNano: time.Now().UnixNano(), Message: "[" + stage + "] " + message})
	}
	// Append only supplements the existing operation sender; a diagnostic must
	// never reopen an operation that Gitea has already completed.
	_ = d.Logs.Append(cs, fmt.Sprintf("diagnostic-%s-%x", stage, sha256.Sum256([]byte(strings.Join(lines, "\n")))), batch)
}

func (d *runtimeDiagnostics) capture(ctx context.Context, cs *api.Codespace, pod *corev1.Pod, stage string) {
	if d == nil || !cs.Spec.Runtime.Debug {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var podError error
	if pod == nil && cs.Status.Pod.Name != "" {
		pod = &corev1.Pod{}
		podError = d.Reader.Get(ctx, types.NamespacedName{Namespace: cs.Namespace, Name: cs.Status.Pod.Name}, pod)
		if podError != nil {
			pod = nil
		}
	}
	if pod != nil && (!metav1.IsControlledBy(pod, cs) || (cs.Status.Pod.UID != "" && pod.UID != cs.Status.Pod.UID)) {
		pod, podError = nil, fmt.Errorf("pod identity no longer matches the runtime")
	}
	lines := []string{fmt.Sprintf("operation=%s version=%d recovery=%s", cs.Spec.Operation.Type, cs.Spec.Operation.Version, cs.Status.RecoveryAction)}
	add := func(name string, value any) {
		data, err := json.Marshal(value)
		if err == nil {
			lines = append(lines, name+"="+string(data))
		}
	}
	add("result", cs.Status.Result)
	add("conditions", cs.Status.Conditions)
	if podError != nil {
		lines = append(lines, "Pod status unavailable: "+podError.Error())
	}
	if pod != nil {
		add("pod", struct {
			Name              string          `json:"name"`
			UID               types.UID       `json:"uid"`
			Node              string          `json:"node"`
			RuntimeClass      *string         `json:"runtimeClass"`
			Phase             corev1.PodPhase `json:"phase"`
			Reason            string          `json:"reason"`
			Message           string          `json:"message"`
			DeletionTimestamp *metav1.Time    `json:"deletionTimestamp"`
			Finalizers        []string        `json:"finalizers"`
		}{pod.Name, pod.UID, pod.Spec.NodeName, pod.Spec.RuntimeClassName, pod.Status.Phase, pod.Status.Reason, pod.Status.Message, pod.DeletionTimestamp, pod.Finalizers})
		for _, condition := range pod.Status.Conditions {
			add("podCondition", condition)
		}
		for _, containers := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
			for _, container := range containers {
				add("resources/"+container.Name, container.Resources)
			}
		}
		for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
			for _, status := range statuses {
				add("container/"+status.Name, status)
			}
		}
	}
	fingerprint := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	d.mu.Lock()
	entry := d.observation(cs)
	duplicate := entry.stages[stage] == fingerprint
	entry.stages[stage] = fingerprint
	d.mu.Unlock()
	if duplicate {
		return
	}
	defer func() { d.emit(ctx, cs, stage, lines) }()
	objects := []corev1.ObjectReference{}
	if pod != nil {
		objects = append(objects, corev1.ObjectReference{Kind: "Pod", Name: pod.Name, UID: pod.UID})
	}
	if cs.Status.Volume.Name != "" && ctx.Err() == nil {
		var pvc corev1.PersistentVolumeClaim
		if err := d.Reader.Get(ctx, types.NamespacedName{Namespace: cs.Namespace, Name: cs.Status.Volume.Name}, &pvc); err != nil {
			lines = append(lines, "PVC status unavailable: "+err.Error())
		} else if pvc.UID == cs.Status.Volume.UID && pvc.Labels[RuntimeUIDLabel] == string(cs.UID) {
			add("pvc", struct {
				Name         string                             `json:"name"`
				UID          types.UID                          `json:"uid"`
				StorageClass *string                            `json:"storageClass"`
				Status       corev1.PersistentVolumeClaimStatus `json:"status"`
			}{pvc.Name, pvc.UID, pvc.Spec.StorageClassName, pvc.Status})
			objects = append(objects, corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: pvc.Name, UID: pvc.UID})
		}
	}
	var events []corev1.Event
	for _, object := range objects {
		if ctx.Err() != nil {
			break
		}
		var list corev1.EventList
		if err := d.Reader.List(ctx, &list, client.InNamespace(cs.Namespace), client.MatchingFields{"involvedObject.uid": string(object.UID)}); err != nil {
			lines = append(lines, object.Kind+" events unavailable: "+err.Error())
		} else {
			events = append(events, list.Items...)
		}
	}
	slices.SortFunc(events, func(a, b corev1.Event) int { return eventTime(b).Compare(eventTime(a)) })
	if len(events) > 20 {
		events = events[:20]
		lines = append(lines, "events limited to latest 20")
	}
	for _, event := range events {
		lines = append(lines, fmt.Sprintf("event %s %s/%s %s %s count=%d: %s", eventTime(event).Format(time.RFC3339Nano), event.InvolvedObject.Kind, event.InvolvedObject.Name, event.Type, event.Reason, event.Count, event.Message))
	}
	if pod != nil && pod.Spec.NodeName != "" && ctx.Err() == nil {
		var node corev1.Node
		if err := d.Reader.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, &node); err != nil {
			lines = append(lines, "Node conditions unavailable: "+err.Error())
		} else {
			for _, condition := range node.Status.Conditions {
				add("nodeCondition", condition)
			}
		}
	}
	if pod != nil && d.Pods != nil {
		lines = append(lines, d.podLogs(ctx, pod)...)
	}
	if ctx.Err() != nil {
		lines = append(lines, "diagnostic collection interrupted: "+ctx.Err().Error())
	}
}

func (d *runtimeDiagnostics) podLogs(ctx context.Context, pod *corev1.Pod) []string {
	var lines []string
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, status := range statuses {
			for _, previous := range []bool{false, true} {
				if ctx.Err() != nil {
					return lines
				}
				if (previous && status.RestartCount == 0) || (!previous && status.State.Waiting != nil) {
					continue
				}
				label := fmt.Sprintf("pod=%s container=%s previous=%t", pod.Name, status.Name, previous)
				stream, err := d.Pods.Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: status.Name, Previous: previous, Timestamps: true, TailLines: ptr.To(int64(200)), LimitBytes: ptr.To(int64(diagnosticLogBytes))}).Stream(ctx)
				if err != nil {
					lines = append(lines, label+" logs unavailable: "+err.Error())
					continue
				}
				data, readErr := io.ReadAll(io.LimitReader(stream, diagnosticLogBytes))
				_ = stream.Close()
				lines = append(lines, label+" log tail (up to 200 lines / 64 KiB)")
				limited := len(data) == diagnosticLogBytes
				if limited {
					lines = append(lines, "log byte limit reached; boundary lines omitted")
					// A byte-limited tail may begin in the middle of a credential.
					if start := bytes.IndexByte(data, '\n'); start >= 0 {
						data = data[start+1:]
					} else {
						data = nil
					}
				}
				if limited || readErr != nil {
					// An interrupted read may also end in a partial credential.
					data = data[:bytes.LastIndexByte(data, '\n')+1]
				}
				for line := range strings.Lines(string(data)) {
					if line = strings.TrimSuffix(line, "\n"); line != "" {
						lines = append(lines, label+" "+line)
					}
				}
				if readErr != nil {
					lines = append(lines, label+" logs incomplete: "+readErr.Error())
				}
			}
		}
	}
	return lines
}

func eventTime(event corev1.Event) time.Time {
	if event.Series != nil {
		return event.Series.LastObservedTime.Time
	}
	if !event.LastTimestamp.IsZero() {
		return event.LastTimestamp.Time
	}
	if !event.EventTime.IsZero() {
		return event.EventTime.Time
	}
	return event.CreationTimestamp.Time
}
