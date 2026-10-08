// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	RuntimeFinalizer        = "codespace.gitea.dev/runtime-cleanup"
	WriterFinalizer         = "codespace.gitea.dev/writer-stopped"
	WriterStoppedAnnotation = "codespace.gitea.dev/confirmed-stopped-pod-uid"
)

type RuntimeReconciler struct {
	Client              client.Client
	Authority           *ExecutionAuthority
	ManagementNamespace string
	ManagerURL          string
	IdentityIssuer      string
	ImagePullSecrets    []string
}

func (r *RuntimeReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var cs api.Codespace
	if err := r.Client.Get(ctx, request.NamespacedName, &cs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if err := cs.Validate(r.ManagementNamespace); err != nil {
		return r.condition(ctx, &cs, "InvalidConfiguration", err)
	}
	var site api.GiteaSite
	if err := r.Client.Get(ctx, types.NamespacedName{Name: cs.Spec.Site.Name}, &site); err != nil {
		return r.condition(ctx, &cs, "SiteUnavailable", err)
	}
	var ns corev1.Namespace
	if err := r.Client.Get(ctx, types.NamespacedName{Name: cs.Namespace}, &ns); err != nil {
		return r.condition(ctx, &cs, "NamespaceUnavailable", err)
	}
	if site.UID != cs.Spec.Site.UID || ns.UID != site.Status.NamespaceUID || ns.Labels[SiteUIDLabel] != string(site.UID) {
		return r.condition(ctx, &cs, "OwnershipConflict", fmt.Errorf("runtime site or namespace identity changed"))
	}
	if !controllerutil.ContainsFinalizer(&cs, RuntimeFinalizer) {
		if !cs.DeletionTimestamp.IsZero() {
			return ctrl.Result{}, nil
		}
		controllerutil.AddFinalizer(&cs, RuntimeFinalizer)
		if err := r.Client.Update(ctx, &cs); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	if !cs.Status.Bound && cs.Status.RecoveryAction != "delete" {
		return r.condition(ctx, &cs, "AwaitingBinding", fmt.Errorf("gitea has not confirmed the runtime identity"))
	}
	if cs.Spec.Operation.Type == "delete" || cs.Status.RecoveryAction == "delete" || !cs.DeletionTimestamp.IsZero() {
		if cs.Spec.Operation.Type != "delete" && cs.Status.RecoveryAction != "delete" {
			return r.condition(ctx, &cs, "CleanupAuthorizationRequired", fmt.Errorf("gitea delete authorization is required before removing runtime data"))
		}
		return r.stopOrDelete(ctx, &cs, true)
	}
	if cs.Spec.Operation.Type == "stop" || cs.Spec.Operation.Type == "abort_create" || cs.Spec.Operation.Type == "abort_resume" || cs.Status.RecoveryAction == "stop" {
		return r.stopOrDelete(ctx, &cs, false)
	}
	if cs.Status.Result != nil && cs.Status.Result.Version == cs.Spec.Operation.Version && !cs.Status.Result.Succeeded {
		return r.stopOrDelete(ctx, &cs, false)
	}
	if cs.Status.SettledOperationVersion < cs.Spec.Operation.Version && (r.Authority == nil || r.Authority.Remaining(cs.UID, cs.Spec.Operation.Version) <= 0) {
		return r.condition(ctx, &cs, "AwaitingExecutionLease", fmt.Errorf("current Leader has not renewed this operation with Gitea"))
	}
	if err := r.ensureVolume(ctx, &cs); err != nil {
		return r.condition(ctx, &cs, "VolumeUnavailable", err)
	}
	if cs.Status.Pod.Name == "" {
		meta.RemoveStatusCondition(&cs.Status.Conditions, "Stopped")
		cs.Status.Pod.Name = "runtime-" + uuid.NewString()
		cs.Status.IdentitySecretName = cs.Status.Pod.Name + "-identity"
		if err := r.updateStatus(ctx, &cs); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	var pod corev1.Pod
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: cs.Namespace, Name: cs.Status.Pod.Name}, &pod)
	if apierrors.IsNotFound(err) {
		if cs.Status.Pod.UID != "" {
			r.Authority.Revoke(cs.UID)
			cs.Status.Target = nil
			cs.Status.Boot = nil
			if cs.Annotations[WriterStoppedAnnotation] == string(cs.Status.Pod.UID) {
				stoppedPodUID := cs.Status.Pod.UID
				cs.Status.StoppedPodUID = stoppedPodUID
				if cs.Status.SettledOperationVersion < cs.Spec.Operation.Version {
					cs.Status.Result = &api.OperationResult{Version: cs.Spec.Operation.Version, Message: "previous runtime Pod disappeared before operation confirmation"}
				} else {
					cs.Status.RecoveryAction = "stop"
				}
				if err := r.updateStatus(ctx, &cs); err != nil {
					return ctrl.Result{}, err
				}
				if err := r.consumeWriterStoppedConfirmation(ctx, &cs, stoppedPodUID); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: time.Millisecond}, nil
			}
			return r.condition(ctx, &cs, "RecoveryRequired", fmt.Errorf("previous runtime Pod disappeared; verify its writer stopped before replacing it"))
		}
		created, err := runtimePod(&cs, r.ManagerURL, r.ImagePullSecrets)
		if err != nil {
			return r.condition(ctx, &cs, "InvalidRuntimeConfiguration", err)
		}
		pod = *created
		if err := r.Client.Create(ctx, &pod); err != nil {
			return r.condition(ctx, &cs, "PodUnavailable", err)
		}
	} else if err != nil {
		return ctrl.Result{}, err
	}
	if !metav1.IsControlledBy(&pod, &cs) || (cs.Status.Pod.UID != "" && cs.Status.Pod.UID != pod.UID) {
		return r.condition(ctx, &cs, "OwnershipConflict", fmt.Errorf("runtime Pod UID or owner does not match"))
	}
	if !pod.DeletionTimestamp.IsZero() {
		return r.condition(ctx, &cs, "PodTerminating", fmt.Errorf("waiting for the previous runtime writer to stop"))
	}
	cs.Status.Pod.UID = pod.UID
	cs.Status.LastNodeName = pod.Spec.NodeName
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		r.Authority.Revoke(cs.UID)
		cs.Status.Target = nil
		cs.Status.Boot = nil
		if cs.Status.SettledOperationVersion < cs.Spec.Operation.Version {
			cs.Status.Result = &api.OperationResult{Version: cs.Spec.Operation.Version, Message: "runtime Pod exited before operation confirmation"}
		} else {
			cs.Status.RecoveryAction = "stop"
		}
		if err := r.updateStatus(ctx, &cs); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	if err := r.ensureIdentity(ctx, &cs, &pod); err != nil {
		return r.condition(ctx, &cs, "IdentityUnavailable", err)
	}
	return r.condition(ctx, &cs, "AgentPending", nil)
}

func (r *RuntimeReconciler) updateStatus(ctx context.Context, cs *api.Codespace) error {
	if err := api.ValidateObjectSize(cs); err != nil {
		return err
	}
	var current api.Codespace
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(cs), &current); err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(current.Status, cs.Status) {
		return nil
	}
	return r.Client.Status().Update(ctx, cs)
}

func (r *RuntimeReconciler) consumeWriterStoppedConfirmation(ctx context.Context, cs *api.Codespace, podUID types.UID) error {
	if podUID == "" || cs.Status.StoppedPodUID != podUID || cs.Annotations[WriterStoppedAnnotation] != string(podUID) {
		return nil
	}
	delete(cs.Annotations, WriterStoppedAnnotation)
	return r.Client.Update(ctx, cs)
}

func (r *RuntimeReconciler) condition(ctx context.Context, cs *api.Codespace, reason string, problem error) (ctrl.Result, error) {
	condition := metav1.Condition{Type: "InfrastructureReady", Status: metav1.ConditionTrue, Reason: reason, Message: "Runtime resources are prepared; Agent readiness is verified separately", ObservedGeneration: cs.Generation}
	if problem != nil {
		condition.Status = metav1.ConditionFalse
		condition.Message = problem.Error()
		preparation := reason == "VolumeUnavailable" || reason == "PodUnavailable" || reason == "IdentityUnavailable"
		pending := cs.Status.SettledOperationVersion < cs.Spec.Operation.Version && (cs.Status.Result == nil || cs.Status.Result.Version != cs.Spec.Operation.Version)
		if preparation && pending && (apierrors.IsForbidden(problem) || apierrors.IsInvalid(problem)) {
			message := []rune(problem.Error())
			if len(message) > 1024 {
				message = message[:1024]
			}
			cs.Status.Result = &api.OperationResult{Version: cs.Spec.Operation.Version, Message: string(message)}
		}
	}
	meta.SetStatusCondition(&cs.Status.Conditions, condition)
	if err := r.updateStatus(ctx, cs); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *RuntimeReconciler) ensureVolume(ctx context.Context, cs *api.Codespace) error {
	name := "data-" + cs.Spec.RuntimeUUID
	var volume corev1.PersistentVolumeClaim
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: cs.Namespace, Name: name}, &volume)
	if apierrors.IsNotFound(err) {
		if cs.Status.Volume.UID != "" {
			return fmt.Errorf("runtime data volume disappeared; refusing to replace user data with an empty volume")
		}
		volume = corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cs.Namespace, Labels: map[string]string{SiteUIDLabel: string(cs.Spec.Site.UID), RuntimeUIDLabel: string(cs.UID)}},
			Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{cs.Spec.Runtime.AccessMode}, VolumeMode: ptr.To(cs.Spec.Runtime.VolumeMode), StorageClassName: ptr.To(cs.Spec.Runtime.StorageClassName), Resources: corev1.VolumeResourceRequirements{Requests: cs.Spec.Runtime.Storage}},
		}
		// Explicit deletion authorization, not garbage collection, releases user data.
		if err := r.Client.Create(ctx, &volume); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if volume.Labels[RuntimeUIDLabel] != string(cs.UID) || volume.Labels[SiteUIDLabel] != string(cs.Spec.Site.UID) || (cs.Status.Volume.UID != "" && cs.Status.Volume.UID != volume.UID) || !volume.DeletionTimestamp.IsZero() {
		return fmt.Errorf("runtime volume ownership or lifecycle changed")
	}
	cs.Status.Volume = api.ObservedResource{Name: volume.Name, UID: volume.UID}
	return nil
}

func runtimePod(cs *api.Codespace, managerURL string, pullSecrets []string) (*corev1.Pod, error) {
	runtime := cs.Spec.Runtime
	devContainer, err := json.Marshal(runtime.DevContainer)
	if err != nil {
		return nil, fmt.Errorf("encode Dev Container injection configuration: %w", err)
	}
	volumeMounts := []corev1.VolumeMount{{Name: "ephemeral", MountPath: "/run/codespace"}, {Name: "identity", MountPath: "/run/identity", ReadOnly: true}}
	var volumeDevices []corev1.VolumeDevice
	var dataDevice string
	if runtime.VolumeMode == corev1.PersistentVolumeBlock {
		dataDevice = "/dev/codespace-data"
		volumeDevices = []corev1.VolumeDevice{{Name: "data", DevicePath: dataDevice}}
	} else {
		volumeMounts = append([]corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/codespace"}}, volumeMounts...)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: cs.Status.Pod.Name, Namespace: cs.Namespace, Finalizers: []string{WriterFinalizer}, Labels: map[string]string{SiteUIDLabel: string(cs.Spec.Site.UID), RuntimeUIDLabel: string(cs.UID), ComponentLabel: "runtime"}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cs, api.GroupVersion.WithKind("Codespace"))}},
		Spec: corev1.PodSpec{
			RuntimeClassName: &runtime.RuntimeClassName, ServiceAccountName: "codespace", AutomountServiceAccountToken: ptr.To(false),
			ImagePullSecrets: localObjectReferences(pullSecrets),
			RestartPolicy:    corev1.RestartPolicyNever, TerminationGracePeriodSeconds: ptr.To(int64(60)), EnableServiceLinks: ptr.To(false),
			Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: cs.Status.Volume.Name}}},
				{Name: "ephemeral", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: ptr.To(resource.MustParse("64Mi"))}}},
				{Name: "identity", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: cs.Status.IdentitySecretName, DefaultMode: ptr.To(int32(0o400)), Items: []corev1.KeyToPath{{Key: "tls.crt", Path: "tls.crt"}, {Key: "tls.key", Path: "tls.key"}, {Key: "ca.crt", Path: "ca.crt"}}}}},
			},
			Containers: []corev1.Container{{
				Name: "runtime", Image: runtime.Image, ImagePullPolicy: corev1.PullIfNotPresent,
				Command:   []string{"/usr/local/bin/gitea-codespace", "agent"},
				Ports:     []corev1.ContainerPort{{Name: "agent-access", ContainerPort: 8444, Protocol: corev1.ProtocolTCP}},
				Resources: runtime.Resources,
				Env: []corev1.EnvVar{
					{Name: "CODESPACE_MANAGER_URL", Value: managerURL},
					{Name: "CODESPACE_RUNTIME_UUID", Value: cs.Spec.RuntimeUUID},
					{Name: "CODESPACE_SITE_UID", Value: string(cs.Spec.Site.UID)},
					{Name: "CODESPACE_RESOURCE_UID", Value: string(cs.UID)},
					{Name: "CODESPACE_POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
					{Name: "CODESPACE_DATA_DEVICE", Value: dataDevice},
					{Name: "CODESPACE_DEVCONTAINER_CONFIGURATION", Value: string(devContainer)},
				},
				VolumeMounts:  volumeMounts,
				VolumeDevices: volumeDevices,
			}},
		},
	}
	if runtime.Isolation == "kata" {
		pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
	} else {
		pod.Spec.HostUsers = ptr.To(false)
		pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: ptr.To(int64(0)), RunAsGroup: ptr.To(int64(0))}
	}
	return pod, nil
}

func (r *RuntimeReconciler) ensureIdentity(ctx context.Context, cs *api.Codespace, pod *corev1.Pod) error {
	if pod.UID == "" || cs.UID == "" || cs.Spec.Site.UID == "" {
		return fmt.Errorf("agent identity requires assigned Kubernetes UIDs")
	}
	identity := fmt.Sprintf("spiffe://codespace/agent/%s/%s/%s", cs.Spec.Site.UID, cs.UID, pod.UID)
	// Preassign ownership before cert-manager writes key material. Required keys
	// keep the Pod waiting while the Secret is still empty.
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: cs.Status.IdentitySecretName, Namespace: cs.Namespace}}
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(secret), secret)
	if apierrors.IsNotFound(err) {
		secret.Labels = map[string]string{RuntimeUIDLabel: string(cs.UID), SiteUIDLabel: string(cs.Spec.Site.UID)}
		secret.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}}
		if err := r.Client.Create(ctx, secret); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		owned := false
		for _, owner := range secret.OwnerReferences {
			owned = owned || (owner.APIVersion == "v1" && owner.Kind == "Pod" && owner.UID == pod.UID && owner.Name == pod.Name)
		}
		if !owned || secret.Labels[RuntimeUIDLabel] != string(cs.UID) || secret.Labels[SiteUIDLabel] != string(cs.Spec.Site.UID) || !secret.DeletionTimestamp.IsZero() {
			return fmt.Errorf("agent identity Secret does not belong to the current Pod")
		}
	}
	certificate := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
		"metadata": map[string]any{"name": cs.Status.IdentitySecretName, "namespace": cs.Namespace},
		"spec": map[string]any{
			"secretName": cs.Status.IdentitySecretName,
			"issuerRef":  map[string]any{"name": r.IdentityIssuer, "kind": "ClusterIssuer", "group": "cert-manager.io"},
			"uris":       []any{identity}, "usages": []any{"client auth", "server auth"},
			"duration": "24h", "renewBefore": "8h",
			"privateKey": map[string]any{"algorithm": "ECDSA", "size": int64(256), "rotationPolicy": "Always"},
		},
	}}
	certificate.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(pod, corev1.SchemeGroupVersion.WithKind("Pod"))})
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(certificate.GroupVersionKind())
	err = r.Client.Get(ctx, client.ObjectKeyFromObject(certificate), existing)
	if apierrors.IsNotFound(err) {
		return r.Client.Create(ctx, certificate)
	}
	if err != nil {
		return err
	}
	uris, _, err := unstructured.NestedStringSlice(existing.Object, "spec", "uris")
	if err != nil || !metav1.IsControlledBy(existing, pod) || len(uris) != 1 || uris[0] != identity {
		return fmt.Errorf("agent certificate does not belong to the current Pod")
	}
	for _, field := range []string{"secretName", "issuerRef", "usages", "privateKey"} {
		actual, _, err := unstructured.NestedFieldCopy(existing.Object, "spec", field)
		expected, _, _ := unstructured.NestedFieldCopy(certificate.Object, "spec", field)
		if err != nil || !equality.Semantic.DeepEqual(actual, expected) {
			return fmt.Errorf("agent certificate %s differs from the current identity configuration", field)
		}
	}
	return nil
}

func (r *RuntimeReconciler) stopOrDelete(ctx context.Context, cs *api.Codespace, remove bool) (ctrl.Result, error) {
	// List by claim as well as owner: a second writer must not be missed by labels.
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods, client.InNamespace(cs.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		usesVolume := false
		for _, volume := range pod.Spec.Volumes {
			usesVolume = usesVolume || (volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == cs.Status.Volume.Name)
		}
		if !usesVolume && pod.Name != cs.Status.Pod.Name {
			continue
		}
		if !metav1.IsControlledBy(pod, cs) || pod.Name != cs.Status.Pod.Name || (cs.Status.Pod.UID != "" && pod.UID != cs.Status.Pod.UID) {
			return r.condition(ctx, cs, "WriterConflict", fmt.Errorf("another Pod references the runtime volume"))
		}
		if pod.DeletionTimestamp.IsZero() {
			if err := r.Client.Delete(ctx, pod, client.Preconditions{UID: &pod.UID}); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		// A ready Node and an absent Pod are not proof that its processes stopped.
		// Hold the Pod until kubelet reports termination, then persist that fact.
		stopped := cs.Annotations[WriterStoppedAnnotation] == string(pod.UID) || pod.Spec.NodeName == ""
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			stopped = len(pod.Status.ContainerStatuses) == len(pod.Spec.Containers)
			for _, status := range pod.Status.ContainerStatuses {
				stopped = stopped && status.State.Terminated != nil
			}
		}
		if !stopped {
			return r.condition(ctx, cs, "WriterUnconfirmed", fmt.Errorf("waiting for kubelet to confirm runtime process termination"))
		}
		if cs.Status.StoppedPodUID != pod.UID {
			cs.Status.StoppedPodUID = pod.UID
			if err := r.updateStatus(ctx, cs); err != nil {
				return ctrl.Result{}, err
			}
		}
		if err := r.consumeWriterStoppedConfirmation(ctx, cs, pod.UID); err != nil {
			return ctrl.Result{}, err
		}
		if controllerutil.RemoveFinalizer(pod, WriterFinalizer) {
			if err := r.Client.Update(ctx, pod); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	stoppedPodUID := cs.Status.Pod.UID
	if stoppedPodUID != "" && cs.Status.StoppedPodUID != stoppedPodUID {
		if cs.Annotations[WriterStoppedAnnotation] != string(stoppedPodUID) {
			return r.condition(ctx, cs, "WriterUnconfirmed", fmt.Errorf("previous runtime Pod disappeared without confirmed termination"))
		}
		cs.Status.StoppedPodUID = stoppedPodUID
	}
	if remove && cs.Status.Volume.Name != "" {
		var volume corev1.PersistentVolumeClaim
		err := r.Client.Get(ctx, types.NamespacedName{Namespace: cs.Namespace, Name: cs.Status.Volume.Name}, &volume)
		if err == nil {
			if volume.UID != cs.Status.Volume.UID || volume.Labels[RuntimeUIDLabel] != string(cs.UID) {
				return r.condition(ctx, cs, "OwnershipConflict", fmt.Errorf("PVC does not match cleanup authorization"))
			}
			if volume.DeletionTimestamp.IsZero() {
				err = r.Client.Delete(ctx, &volume, client.Preconditions{UID: &volume.UID})
			}
			if err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	if remove && !cs.DeletionTimestamp.IsZero() {
		controllerutil.RemoveFinalizer(cs, RuntimeFinalizer)
		return ctrl.Result{}, r.Client.Update(ctx, cs)
	}
	if remove && cs.Status.RecoveryAction == "delete" {
		if err := r.Client.Delete(ctx, cs, client.Preconditions{UID: &cs.UID}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	cs.Status.Target = nil
	cs.Status.Boot = nil
	cs.Status.Pod = api.ObservedResource{}
	cs.Status.IdentitySecretName = ""
	cs.Status.LastNodeName = ""
	meta.SetStatusCondition(&cs.Status.Conditions, metav1.Condition{Type: "Stopped", Status: metav1.ConditionTrue, Reason: "ResourcesStopped", Message: "Runtime Pod has stopped", ObservedGeneration: cs.Generation})
	if cs.Spec.Operation.Type == "stop" || cs.Spec.Operation.Type == "delete" {
		cs.Status.Result = &api.OperationResult{Version: cs.Spec.Operation.Version, Succeeded: true}
	}
	if err := r.updateStatus(ctx, cs); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.consumeWriterStoppedConfirmation(ctx, cs, stoppedPodUID)
}
