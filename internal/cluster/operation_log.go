// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"fmt"
	"regexp"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var podWaitingReasonPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

// managerProgress reports resource facts using bounded, non-secret messages.
// The runtime controller remains responsible for resource convergence.
func managerProgress(ctx context.Context, reader client.Reader, cs *api.Codespace) (string, string, error) {
	if !cs.Status.Bound || cs.Status.SettledOperationVersion >= cs.Spec.Operation.Version || !cs.DeletionTimestamp.IsZero() || cs.Status.RecoveryAction != "" {
		return "", "", nil
	}
	switch cs.Spec.Operation.Type {
	case "stop", "delete", "abort_create", "abort_resume":
		if cs.Status.Pod.Name != "" {
			return "stopping", "[INFO] [kubernetes] Stopping runtime Pod", nil
		}
		if cs.Spec.Operation.Type == "delete" {
			return "deleting", "[INFO] [kubernetes] Removing runtime resources", nil
		}
		return "stopped", "[INFO] [kubernetes] Runtime process has stopped", nil
	}
	if (cs.Status.Result != nil && cs.Status.Result.Version == cs.Spec.Operation.Version) || (cs.Status.Boot != nil && cs.Status.Boot.OperationVersion == cs.Spec.Operation.Version) {
		return "", "", nil
	}
	if condition := meta.FindStatusCondition(cs.Status.Conditions, "InfrastructureReady"); condition != nil && condition.Status == metav1.ConditionFalse && condition.ObservedGeneration == cs.Generation {
		switch condition.Reason {
		case "InvalidConfiguration", "SiteUnavailable", "NamespaceUnavailable", "OwnershipConflict", "ProxyUnavailable", "VolumeUnavailable", "InvalidRuntimeConfiguration", "PodUnavailable", "PodTerminating", "IdentityUnavailable":
			return "infrastructure-" + condition.Reason, "[INFO] [kubernetes] Waiting for runtime infrastructure: " + condition.Reason, nil
		}
	}
	if cs.Status.Pod.Name != "" {
		var pod corev1.Pod
		err := reader.Get(ctx, types.NamespacedName{Namespace: cs.Namespace, Name: cs.Status.Pod.Name}, &pod)
		if err != nil && !apierrors.IsNotFound(err) {
			return "", "", err
		}
		if err == nil && pod.Labels[RuntimeUIDLabel] == string(cs.UID) {
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse && podWaitingReasonPattern.MatchString(condition.Reason) {
					return "scheduling-" + condition.Reason, "[INFO] [kubernetes] Waiting for Pod scheduling: " + condition.Reason, nil
				}
			}
			for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
				for _, status := range statuses {
					if waiting := status.State.Waiting; waiting != nil && podWaitingReasonPattern.MatchString(waiting.Reason) {
						return "container-" + status.Name + "-" + waiting.Reason, fmt.Sprintf("[INFO] [kubernetes] Waiting for container %s: %s", status.Name, waiting.Reason), nil
					}
				}
			}
			if pod.Status.Phase == corev1.PodRunning {
				return "pod-running", "[INFO] [kubernetes] Runtime Pod is running; waiting for Agent", nil
			}
			return "pod-created", "[INFO] [kubernetes] Runtime Pod has been created", nil
		}
		return "pod-creating", "[INFO] [kubernetes] Creating runtime Pod", nil
	}
	if cs.Status.Volume.Name != "" {
		var volume corev1.PersistentVolumeClaim
		err := reader.Get(ctx, types.NamespacedName{Namespace: cs.Namespace, Name: cs.Status.Volume.Name}, &volume)
		if err != nil && !apierrors.IsNotFound(err) {
			return "", "", err
		}
		if err == nil {
			if volume.Status.Phase == corev1.ClaimBound {
				return "volume-bound", "[INFO] [kubernetes] Workspace volume is bound", nil
			}
			return "volume-created", "[INFO] [kubernetes] Workspace volume has been created", nil
		}
		return "volume-creating", "[INFO] [kubernetes] Creating workspace volume", nil
	}
	return "", "", nil
}
