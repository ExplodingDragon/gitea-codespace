// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"net/http"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

type adminRuntimeRecovery struct {
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resourceVersion"`
	PodUID          types.UID `json:"podUID"`
}

type adminRuntimeSpec struct {
	Site           api.ResourceReference `json:"site"`
	CodespaceID    string                `json:"codespaceID"`
	RuntimeUUID    string                `json:"runtimeUUID"`
	EnvironmentTag string                `json:"environmentTag"`
	Operation      string                `json:"operation"`
	Version        int64                 `json:"version"`
}

type adminRuntimeStatus struct {
	Bound      bool                 `json:"bound"`
	Pod        api.ObservedResource `json:"pod"`
	Volume     api.ObservedResource `json:"volume"`
	Ready      bool                 `json:"ready"`
	Conditions []metav1.Condition   `json:"conditions"`
}

func (s *AdminServer) deleteRuntime(w http.ResponseWriter, r *http.Request, namespace, name string) {
	if len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 {
		http.NotFound(w, r)
		return
	}
	var body adminRuntimeRecovery
	if !decodeAdminJSON(w, r, &body) {
		return
	}
	var runtime api.Codespace
	if err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: namespace, Name: name}, &runtime); err != nil {
		writeAdminError(w, r, err)
		return
	}
	if body.UID == "" || body.ResourceVersion == "" || runtime.UID != body.UID || runtime.ResourceVersion != body.ResourceVersion || runtime.Status.Pod.UID != body.PodUID {
		writeAdminProblem(w, r, http.StatusConflict, "resource_conflict", "runtime identity changed; reload before deleting")
		return
	}
	var site api.GiteaSite
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: runtime.Spec.Site.Name}, &site); err != nil {
		writeAdminError(w, r, err)
		return
	}
	expectedNamespace, err := api.SiteNamespace(site.Name, s.Namespace)
	if err != nil || site.UID != runtime.Spec.Site.UID || expectedNamespace != namespace || site.Spec.Enabled {
		writeAdminProblem(w, r, http.StatusConflict, "resource_in_use", "disable the owning site before cleaning up its runtime data")
		return
	}
	operationVersion, podUID := runtime.Spec.Operation.Version, runtime.Status.Pod.UID
	if err := updateCodespaceStatus(r.Context(), s.Client, &runtime, func(current *api.Codespace) error {
		if current.Spec.Operation.Version != operationVersion || current.Status.Pod.UID != podUID {
			return errObservedResourceChanged
		}
		current.Status.RecoveryAction = "delete"
		current.Status.Target = nil
		return nil
	}); err != nil {
		writeAdminError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *AdminServer) confirmRuntimeWriterStopped(w http.ResponseWriter, r *http.Request, namespace, name string) {
	if r.Method != http.MethodPost || len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 {
		http.NotFound(w, r)
		return
	}
	var body adminRuntimeRecovery
	if !decodeAdminJSON(w, r, &body) {
		return
	}
	var runtime api.Codespace
	if err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: namespace, Name: name}, &runtime); err != nil {
		writeAdminError(w, r, err)
		return
	}
	condition := meta.FindStatusCondition(runtime.Status.Conditions, "InfrastructureReady")
	if body.UID == "" || body.ResourceVersion == "" || body.PodUID == "" || runtime.UID != body.UID || runtime.ResourceVersion != body.ResourceVersion ||
		runtime.Status.Pod.UID != body.PodUID || !runtime.DeletionTimestamp.IsZero() || condition == nil || condition.Status != metav1.ConditionFalse ||
		(condition.Reason != "RecoveryRequired" && condition.Reason != "WriterUnconfirmed") {
		writeAdminProblem(w, r, http.StatusConflict, "resource_conflict", "runtime recovery state changed; reload before confirming")
		return
	}
	var pod corev1.Pod
	err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: namespace, Name: runtime.Status.Pod.Name}, &pod)
	if err == nil {
		if pod.UID != body.PodUID || !metav1.IsControlledBy(&pod, &runtime) || pod.DeletionTimestamp.IsZero() {
			writeAdminProblem(w, r, http.StatusConflict, "resource_conflict", "runtime Pod is still active or its identity changed")
			return
		}
	} else if !apierrors.IsNotFound(err) {
		writeAdminError(w, r, err)
		return
	}
	if runtime.Annotations == nil {
		runtime.Annotations = map[string]string{}
	}
	runtime.Annotations[WriterStoppedAnnotation] = string(body.PodUID)
	if err := s.Client.Update(r.Context(), &runtime); err != nil {
		writeAdminError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
