// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"net/http"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

type adminRuntimeDeletion struct {
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resourceVersion"`
	PodUID          types.UID `json:"podUID"`
}

type adminRuntimeStop struct {
	UID    types.UID `json:"uid"`
	PodUID types.UID `json:"podUID"`
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
	CanStop    bool                 `json:"canStop"`
	Stopping   bool                 `json:"stopping"`
	Stopped    bool                 `json:"stopped"`
	Deleting   bool                 `json:"deleting"`
	Conditions []metav1.Condition   `json:"conditions"`
}

func (s *AdminServer) deleteRuntime(w http.ResponseWriter, r *http.Request, namespace, name string) {
	if len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 {
		http.NotFound(w, r)
		return
	}
	var body adminRuntimeDeletion
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

func (s *AdminServer) stopRuntime(w http.ResponseWriter, r *http.Request, namespace, name string) {
	if r.Method != http.MethodPost || len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 {
		http.NotFound(w, r)
		return
	}
	var body adminRuntimeStop
	if !decodeAdminJSON(w, r, &body) {
		return
	}
	var runtime api.Codespace
	if err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: namespace, Name: name}, &runtime); err != nil {
		writeAdminError(w, r, err)
		return
	}
	if body.UID == "" || body.PodUID == "" || runtime.UID != body.UID || !runtime.DeletionTimestamp.IsZero() {
		writeAdminProblem(w, r, http.StatusConflict, "resource_conflict", "runtime identity changed; reload before stopping")
		return
	}
	if runtime.Spec.Operation.Type == "delete" || runtime.Status.RecoveryAction == "delete" {
		writeAdminProblem(w, r, http.StatusConflict, "resource_in_use", "runtime deletion is already in progress")
		return
	}
	if runtime.Status.Pod.UID == "" && runtime.Status.StoppedPodUID == body.PodUID {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if runtime.Status.Pod.UID != body.PodUID {
		writeAdminProblem(w, r, http.StatusConflict, "resource_conflict", "runtime identity changed; reload before stopping")
		return
	}
	if runtimeStopRequested(&runtime) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	operationVersion, operationType := runtime.Spec.Operation.Version, runtime.Spec.Operation.Type
	if err := updateCodespaceStatus(r.Context(), s.Client, &runtime, func(current *api.Codespace) error {
		if current.Spec.Operation.Version != operationVersion || current.Spec.Operation.Type != operationType || current.Status.Pod.UID != body.PodUID || !current.DeletionTimestamp.IsZero() {
			return errObservedResourceChanged
		}
		if current.Status.RecoveryAction == "delete" || current.Spec.Operation.Type == "delete" {
			return errObservedResourceChanged
		}
		current.Status.Target = nil
		if current.Status.SettledOperationVersion < operationVersion && (operationType == "create" || operationType == "resume") {
			current.Status.Result = &api.OperationResult{Version: operationVersion, Message: "runtime stopped by Manager administrator"}
		} else {
			current.Status.RecoveryAction = "stop"
		}
		return nil
	}); err != nil {
		writeAdminError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
