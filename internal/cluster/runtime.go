// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"sort"
	"time"
	"uuid"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	identitypkg "gitea.dev/codespace/internal/identity"
	"gitea.dev/codespace/internal/runtimeplatform"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	RuntimeFinalizer = "codespace.gitea.dev/runtime-cleanup"
	WriterFinalizer  = "codespace.gitea.dev/writer-stopped"
)

type RuntimeReconciler struct {
	Client              client.Client
	Authority           *ExecutionAuthority
	Identity            *IdentityAuthority
	ManagementNamespace string
	ManagerURL          string
	ManagerIdentity     string
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
	if runtimeStopRequested(&cs) {
		return r.stopOrDelete(ctx, &cs, false)
	}
	if cs.Status.SettledOperationVersion < cs.Spec.Operation.Version && (r.Authority == nil || r.Authority.Remaining(cs.UID, cs.Spec.Operation.Version) <= 0) {
		return r.condition(ctx, &cs, "AwaitingExecutionLease", fmt.Errorf("current Leader has not renewed this operation with Gitea"))
	}
	if cs.Spec.Runtime.Proxy != nil {
		var proxySecret corev1.Secret
		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: cs.Namespace, Name: runtimeProxySecretName(cs.Name)}, &proxySecret); err != nil {
			return r.condition(ctx, &cs, "ProxyUnavailable", err)
		}
		if !metav1.IsControlledBy(&proxySecret, &cs) || proxySecret.Labels[RuntimeUIDLabel] != string(cs.UID) || !proxySecret.DeletionTimestamp.IsZero() {
			return r.condition(ctx, &cs, "OwnershipConflict", fmt.Errorf("runtime proxy Secret ownership changed"))
		}
	}
	if err := r.ensureVolume(ctx, &cs); err != nil {
		return r.condition(ctx, &cs, "VolumeUnavailable", err)
	}
	if cs.Status.Pod.Name == "" {
		operationVersion, operationType := cs.Spec.Operation.Version, cs.Spec.Operation.Type
		podName := "runtime-" + uuid.NewV4().String()
		err := updateCodespaceStatus(ctx, r.Client, &cs, func(current *api.Codespace) error {
			if current.Spec.Operation.Version != operationVersion || current.Spec.Operation.Type != operationType {
				return errObservedResourceChanged
			}
			if current.Status.Pod.Name != "" {
				return nil
			}
			meta.RemoveStatusCondition(&current.Status.Conditions, "Stopped")
			current.Status.Pod.Name = podName
			return nil
		})
		if err != nil {
			return reconcileStatusError(err)
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	var pod corev1.Pod
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: cs.Namespace, Name: cs.Status.Pod.Name}, &pod)
	if apierrors.IsNotFound(err) {
		if cs.Status.Pod.UID != "" {
			if r.Authority != nil {
				r.Authority.RevokeVersion(cs.UID, cs.Spec.Operation.Version)
			}
			stoppedPodUID, operationVersion, operationType := cs.Status.Pod.UID, cs.Spec.Operation.Version, cs.Spec.Operation.Type
			err := updateCodespaceStatus(ctx, r.Client, &cs, func(current *api.Codespace) error {
				if current.Spec.Operation.Version != operationVersion || current.Spec.Operation.Type != operationType || current.Status.Pod.UID != stoppedPodUID {
					return errObservedResourceChanged
				}
				current.Status.Target = nil
				current.Status.Boot = nil
				current.Status.StoppedPodUID = stoppedPodUID
				if current.Status.SettledOperationVersion < operationVersion {
					current.Status.Result = &api.OperationResult{Version: operationVersion, Message: "runtime Pod disappeared before operation completion"}
				} else {
					current.Status.RecoveryAction = "stop"
				}
				return nil
			})
			if err != nil {
				return reconcileStatusError(err)
			}
			return ctrl.Result{RequeueAfter: time.Millisecond}, nil
		}
		created, err := runtimePod(&cs, r.ManagerURL, r.ManagerIdentity, r.ImagePullSecrets)
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
	operationVersion, operationType, podName, podUID, nodeName := cs.Spec.Operation.Version, cs.Spec.Operation.Type, cs.Status.Pod.Name, pod.UID, pod.Spec.NodeName
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		r.Authority.RevokeVersion(cs.UID, operationVersion)
		err := updateCodespaceStatus(ctx, r.Client, &cs, func(current *api.Codespace) error {
			if !runtimePodStatusIsCurrent(current, operationVersion, operationType, podName, podUID) {
				return errObservedResourceChanged
			}
			current.Status.Pod.UID = podUID
			current.Status.LastNodeName = nodeName
			current.Status.Target = nil
			current.Status.Boot = nil
			if current.Status.SettledOperationVersion < operationVersion {
				current.Status.Result = &api.OperationResult{Version: operationVersion, Message: "runtime Pod exited before operation confirmation"}
			} else {
				current.Status.RecoveryAction = "stop"
			}
			return nil
		})
		if err != nil {
			return reconcileStatusError(err)
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	err = updateCodespaceStatus(ctx, r.Client, &cs, func(current *api.Codespace) error {
		if !runtimePodStatusIsCurrent(current, operationVersion, operationType, podName, podUID) {
			return errObservedResourceChanged
		}
		current.Status.Pod.UID = podUID
		current.Status.LastNodeName = nodeName
		return nil
	})
	if err != nil {
		return reconcileStatusError(err)
	}
	if err := r.ensureIdentity(ctx, &cs, &pod); err != nil {
		return r.condition(ctx, &cs, "IdentityUnavailable", err)
	}
	return r.condition(ctx, &cs, "ResourcesPrepared", nil)
}

func runtimePodStatusIsCurrent(cs *api.Codespace, operationVersion int64, operationType, podName string, podUID types.UID) bool {
	return cs.Spec.Operation.Version == operationVersion && cs.Spec.Operation.Type == operationType && cs.Status.Pod.Name == podName &&
		(cs.Status.Pod.UID == "" || cs.Status.Pod.UID == podUID)
}

func runtimeStopRequested(runtime *api.Codespace) bool {
	if runtime.Status.RecoveryAction == "stop" {
		return true
	}
	switch runtime.Spec.Operation.Type {
	case "stop", "abort_create", "abort_resume":
		return true
	}
	return runtime.Status.Result != nil && runtime.Status.Result.Version == runtime.Spec.Operation.Version && !runtime.Status.Result.Succeeded
}

func (r *RuntimeReconciler) condition(ctx context.Context, cs *api.Codespace, reason string, problem error) (ctrl.Result, error) {
	observedGeneration, operationVersion, operationType := cs.Generation, cs.Spec.Operation.Version, cs.Spec.Operation.Type
	condition := metav1.Condition{Type: "InfrastructureReady", Status: metav1.ConditionTrue, Reason: reason, Message: "Runtime resources are prepared; Agent readiness is verified separately", ObservedGeneration: observedGeneration}
	if problem != nil {
		condition.Status = metav1.ConditionFalse
		condition.Message = problem.Error()
	}
	err := updateCodespaceStatus(ctx, r.Client, cs, func(current *api.Codespace) error {
		if current.Generation != observedGeneration || current.Spec.Operation.Version != operationVersion || current.Spec.Operation.Type != operationType {
			return errObservedResourceChanged
		}
		if problem != nil {
			preparation := reason == "VolumeUnavailable" || reason == "PodUnavailable" || reason == "IdentityUnavailable"
			pending := current.Status.SettledOperationVersion < operationVersion && (current.Status.Result == nil || current.Status.Result.Version != operationVersion)
			if preparation && pending && (apierrors.IsForbidden(problem) || apierrors.IsInvalid(problem)) {
				message := []rune(problem.Error())
				if len(message) > 1024 {
					message = message[:1024]
				}
				current.Status.Result = &api.OperationResult{Version: operationVersion, Message: string(message)}
			}
		}
		meta.SetStatusCondition(&current.Status.Conditions, condition)
		return nil
	})
	if err != nil {
		return reconcileStatusError(err)
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
			Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{cs.Spec.Runtime.AccessMode}, VolumeMode: ptr.To(cs.Spec.Runtime.StorageVolumeMode()), StorageClassName: ptr.To(cs.Spec.Runtime.StorageClassName), Resources: corev1.VolumeResourceRequirements{Requests: cs.Spec.Runtime.Storage}},
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
	operationVersion, operationType := cs.Spec.Operation.Version, cs.Spec.Operation.Type
	observed := api.ObservedResource{Name: volume.Name, UID: volume.UID}
	return updateCodespaceStatus(ctx, r.Client, cs, func(current *api.Codespace) error {
		if current.Spec.Operation.Version != operationVersion || current.Spec.Operation.Type != operationType || (current.Status.Volume.UID != "" && current.Status.Volume.UID != volume.UID) {
			return errObservedResourceChanged
		}
		current.Status.Volume = observed
		return nil
	})
}

func runtimePod(cs *api.Codespace, managerURL, managerIdentity string, pullSecrets []string) (*corev1.Pod, error) {
	runtime := cs.Spec.Runtime
	pullPolicy, err := platformImagePullPolicy(runtime.Image)
	if err != nil {
		return nil, err
	}
	devContainer, err := json.Marshal(runtime.DevContainer)
	if err != nil {
		return nil, fmt.Errorf("encode Dev Container injection configuration: %w", err)
	}
	nativeConfiguration, err := json.Marshal(runtime.Native)
	if err != nil {
		return nil, fmt.Errorf("encode native runtime configuration: %w", err)
	}
	volumeMounts := []corev1.VolumeMount{{Name: "ephemeral", MountPath: "/run/codespace"}, {Name: "identity", MountPath: "/run/identity", ReadOnly: true}}
	var volumeDevices []corev1.VolumeDevice
	var dataDevice string
	if runtime.StorageVolumeMode() == corev1.PersistentVolumeBlock {
		dataDevice = "/dev/codespace-data"
		volumeDevices = []corev1.VolumeDevice{{Name: "data", DevicePath: dataDevice}}
	} else {
		volumeMounts = append([]corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/codespace"}}, volumeMounts...)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: cs.Status.Pod.Name, Namespace: cs.Namespace, Finalizers: []string{WriterFinalizer}, Labels: map[string]string{SiteUIDLabel: string(cs.Spec.Site.UID), RuntimeUIDLabel: string(cs.UID), ComponentLabel: "runtime"}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cs, api.GroupVersion.WithKind("Codespace"))}},
		Spec: corev1.PodSpec{
			ServiceAccountName: "codespace", AutomountServiceAccountToken: ptr.To(false),
			ImagePullSecrets: localObjectReferences(pullSecrets),
			RestartPolicy:    corev1.RestartPolicyNever, TerminationGracePeriodSeconds: ptr.To(int64(60)), EnableServiceLinks: ptr.To(false),
			Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: cs.Status.Volume.Name}}},
				{Name: "ephemeral", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: ptr.To(resource.MustParse("64Mi"))}}},
				{Name: "identity", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: runtimeIdentitySecretName(cs), DefaultMode: ptr.To(int32(0o400)), Items: []corev1.KeyToPath{{Key: "tls.crt", Path: "tls.crt"}, {Key: "tls.key", Path: "tls.key"}, {Key: "ca.crt", Path: "ca.crt"}}}}},
			},
			Containers: []corev1.Container{{
				Name: "runtime", Image: runtime.Image, ImagePullPolicy: pullPolicy,
				Command:   []string{"/usr/local/bin/gitea-codespace", "agent"},
				Ports:     []corev1.ContainerPort{{Name: "agent-access", ContainerPort: 8444, Protocol: corev1.ProtocolTCP}},
				Resources: runtime.Resources,
				Env: []corev1.EnvVar{
					{Name: "CODESPACE_MANAGER_URL", Value: managerURL},
					{Name: "CODESPACE_MANAGER_IDENTITY", Value: managerIdentity},
					{Name: "CODESPACE_RUNTIME_UUID", Value: cs.Spec.RuntimeUUID},
					{Name: "CODESPACE_SITE_UID", Value: string(cs.Spec.Site.UID)},
					{Name: "CODESPACE_RESOURCE_UID", Value: string(cs.UID)},
					{Name: "CODESPACE_POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
					{Name: "CODESPACE_DATA_DEVICE", Value: dataDevice},
					{Name: "CODESPACE_DEVCONTAINER_CONFIGURATION", Value: string(devContainer)},
					{Name: "CODESPACE_NATIVE_CONFIGURATION", Value: string(nativeConfiguration)},
					{Name: "DOCKER_CONFIG", Value: "/run/codespace/docker-config"},
				},
				VolumeMounts:  volumeMounts,
				VolumeDevices: volumeDevices,
			}},
		},
	}
	if runtime.Isolation != api.IsolationNative {
		pod.Spec.RuntimeClassName = &runtime.RuntimeClassName
	}
	if runtime.Proxy != nil {
		secretName := runtimeProxySecretName(cs.Name)
		pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, proxyEnvironmentFromSecret(secretName)...)
		if runtime.Proxy.CABundle != "" {
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "proxy-ca", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: secretName, DefaultMode: ptr.To(int32(0o444)), Items: []corev1.KeyToPath{{Key: proxyCAKey, Path: proxyCAKey}},
			}}})
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "proxy-ca", MountPath: "/run/codespace/proxy", ReadOnly: true})
			pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{Name: "CODESPACE_PROXY_CA_FILE", Value: "/run/codespace/proxy/" + proxyCAKey})
		}
	}
	if runtime.Isolation == api.IsolationKata {
		pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
	} else if runtime.Isolation == api.IsolationSysbox {
		pod.Spec.HostUsers = ptr.To(false)
		pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: ptr.To(int64(0)), RunAsGroup: ptr.To(int64(0))}
	} else {
		configureNativeRuntimePod(pod, runtime.Native)
	}
	return pod, nil
}

func configureNativeRuntimePod(pod *corev1.Pod, configuration *runtimeplatform.NativeConfiguration) {
	container := &pod.Spec.Containers[0]
	container.Resources = *container.Resources.DeepCopy()
	if container.Resources.Requests == nil {
		container.Resources.Requests = corev1.ResourceList{}
	}
	if container.Resources.Limits == nil {
		container.Resources.Limits = corev1.ResourceList{}
	}
	capabilities := []corev1.Capability{
		"AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "MKNOD", "NET_ADMIN", "NET_BIND_SERVICE",
		"NET_RAW", "SETFCAP", "SETGID", "SETPCAP", "SETUID", "SYS_ADMIN", "SYS_CHROOT", "SYS_RESOURCE",
	}
	seenCapabilities := make(map[corev1.Capability]struct{}, len(capabilities)+len(configuration.RuntimeCapAdd))
	for _, capability := range capabilities {
		seenCapabilities[capability] = struct{}{}
	}
	for _, capability := range configuration.RuntimeCapAdd {
		if _, exists := seenCapabilities[capability]; !exists {
			capabilities = append(capabilities, capability)
			seenCapabilities[capability] = struct{}{}
		}
	}
	container.SecurityContext = &corev1.SecurityContext{
		RunAsUser: ptr.To(int64(0)), RunAsGroup: ptr.To(int64(0)), AllowPrivilegeEscalation: ptr.To(true),
		Capabilities:    &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: capabilities},
		SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
		AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
	}
	hostPaths := map[string]bool{}
	usesCDI := false
	for _, device := range configuration.DevicePlugins {
		quantity := *resource.NewQuantity(device.Count, resource.DecimalSI)
		container.Resources.Requests[device.ResourceName] = quantity
		container.Resources.Limits[device.ResourceName] = quantity
		usesCDI = usesCDI || device.CDI != nil
		for _, mapping := range device.HostAccess.Mounts {
			if current, exists := hostPaths[mapping.Source]; !exists || current && !mapping.ReadOnly {
				hostPaths[mapping.Source] = mapping.ReadOnly
			}
		}
	}
	paths := make([]string, 0, len(hostPaths))
	for path := range hostPaths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for index, path := range paths {
		name := fmt.Sprintf("native-path-%d", index)
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: path}}})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: name, MountPath: path, ReadOnly: hostPaths[path]})
	}
	if usesCDI {
		for index, path := range []string{"/etc/cdi", "/var/run/cdi"} {
			name := fmt.Sprintf("cdi-specs-%d", index)
			directory := corev1.HostPathDirectoryOrCreate
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: path, Type: &directory}}})
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: name, MountPath: path, ReadOnly: true})
		}
	}
}

func (r *RuntimeReconciler) ensureIdentity(ctx context.Context, cs *api.Codespace, pod *corev1.Pod) error {
	if pod.UID == "" || cs.UID == "" || cs.Spec.Site.UID == "" {
		return fmt.Errorf("agent identity requires assigned Kubernetes UIDs")
	}
	identity := identitypkg.AgentURI(string(cs.Spec.Site.UID), string(cs.UID), string(pod.UID))
	_, err := reconcileIdentitySecret(ctx, r.Client, r.Identity, pod, pod.Namespace, runtimeIdentitySecretName(cs), "agent", identity,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		map[string]string{RuntimeUIDLabel: string(cs.UID), SiteUIDLabel: string(cs.Spec.Site.UID)},
	)
	return err
}

func runtimeIdentitySecretName(cs *api.Codespace) string { return cs.Status.Pod.Name + "-identity" }

func (r *RuntimeReconciler) stopOrDelete(ctx context.Context, cs *api.Codespace, remove bool) (ctrl.Result, error) {
	if r.Authority != nil {
		r.Authority.Revoke(cs.UID)
	}
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
			if !controllerutil.ContainsFinalizer(pod, WriterFinalizer) {
				key, podUID := client.ObjectKeyFromObject(pod), pod.UID
				err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
					var current corev1.Pod
					if err := r.Client.Get(ctx, key, &current); err != nil {
						return err
					}
					if current.UID != podUID || !metav1.IsControlledBy(&current, cs) || !current.DeletionTimestamp.IsZero() {
						return errObservedResourceChanged
					}
					if controllerutil.AddFinalizer(&current, WriterFinalizer) {
						return r.Client.Update(ctx, &current)
					}
					return nil
				})
				if err != nil {
					return reconcileStatusError(err)
				}
				return ctrl.Result{RequeueAfter: time.Millisecond}, nil
			}
			if err := r.Client.Delete(ctx, pod, client.Preconditions{UID: &pod.UID}); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		// DeletionTimestamp alone does not prove that the Pod processes stopped.
		// Hold the Pod until kubelet reports termination, then persist that fact.
		stopped := pod.Spec.NodeName == ""
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
			podUID := pod.UID
			err := updateCodespaceStatus(ctx, r.Client, cs, func(current *api.Codespace) error {
				if current.Status.Pod.UID != "" && current.Status.Pod.UID != podUID {
					return errObservedResourceChanged
				}
				current.Status.StoppedPodUID = podUID
				return nil
			})
			if err != nil {
				return reconcileStatusError(err)
			}
		}
		if err := r.releaseWriterFinalizer(ctx, cs, pod); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	stoppedPodUID := cs.Status.Pod.UID
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
		return ctrl.Result{}, r.removeRuntimeFinalizer(ctx, cs)
	}
	if remove && cs.Status.RecoveryAction == "delete" {
		if err := r.Client.Delete(ctx, cs, client.Preconditions{UID: &cs.UID}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	observedGeneration, operationVersion, operationType := cs.Generation, cs.Spec.Operation.Version, cs.Spec.Operation.Type
	err := updateCodespaceStatus(ctx, r.Client, cs, func(current *api.Codespace) error {
		if current.Generation != observedGeneration || current.Spec.Operation.Version != operationVersion || current.Spec.Operation.Type != operationType || (stoppedPodUID != "" && current.Status.Pod.UID != stoppedPodUID) {
			return errObservedResourceChanged
		}
		if stoppedPodUID != "" {
			current.Status.StoppedPodUID = stoppedPodUID
		}
		current.Status.Target = nil
		current.Status.Boot = nil
		current.Status.Pod = api.ObservedResource{}
		current.Status.LastNodeName = ""
		meta.RemoveStatusCondition(&current.Status.Conditions, "InfrastructureReady")
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{Type: "Stopped", Status: metav1.ConditionTrue, Reason: "ResourcesStopped", Message: "Runtime Pod has stopped", ObservedGeneration: observedGeneration})
		if (operationType == "stop" || operationType == "delete") && (current.Status.Result == nil || current.Status.Result.Version != operationVersion) {
			current.Status.Result = &api.OperationResult{Version: operationVersion, Succeeded: true}
		}
		return nil
	})
	if err != nil {
		return reconcileStatusError(err)
	}
	return ctrl.Result{}, nil
}

func (r *RuntimeReconciler) releaseWriterFinalizer(ctx context.Context, cs *api.Codespace, observed *corev1.Pod) error {
	key, uid := client.ObjectKeyFromObject(observed), observed.UID
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current corev1.Pod
		if err := r.Client.Get(ctx, key, &current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.UID != uid || !metav1.IsControlledBy(&current, cs) {
			return errObservedResourceChanged
		}
		if !controllerutil.RemoveFinalizer(&current, WriterFinalizer) {
			return nil
		}
		return r.Client.Update(ctx, &current)
	})
}

func (r *RuntimeReconciler) removeRuntimeFinalizer(ctx context.Context, observed *api.Codespace) error {
	key, uid := client.ObjectKeyFromObject(observed), observed.UID
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current api.Codespace
		if err := r.Client.Get(ctx, key, &current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.UID != uid || current.DeletionTimestamp.IsZero() {
			return errObservedResourceChanged
		}
		if !controllerutil.RemoveFinalizer(&current, RuntimeFinalizer) {
			return nil
		}
		return r.Client.Update(ctx, &current)
	})
}
