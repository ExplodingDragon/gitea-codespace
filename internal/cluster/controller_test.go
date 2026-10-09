// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"gitea.dev/codespace/internal/devcontainerruntime"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, scheme.AddToScheme(s))
	require.NoError(t, api.AddToScheme(s))
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "CertificateList"}, &unstructured.UnstructuredList{})
	return s
}

func testEnvironment() api.EnvironmentConfiguration {
	return api.EnvironmentConfiguration{
		Isolation: "sysbox", RuntimeClassName: "sysbox-runc",
		StorageClassName: "local-path", Storage: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}, VolumeMode: corev1.PersistentVolumeFilesystem, AccessMode: corev1.ReadWriteOnce,
		Resources:     corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")}},
		GitSSHKeyType: "ed25519", DevContainer: devcontainerruntime.Configuration{},
	}
}

func testRuntime() api.RuntimeConfiguration {
	return api.RuntimeConfiguration{EnvironmentConfiguration: testEnvironment(), Image: "localhost/codespace@sha256:" + strings.Repeat("a", 64)}
}

func testSite(name, uid string) *api.GiteaSite {
	return &api.GiteaSite{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid), Generation: 1, Finalizers: []string{SiteFinalizer}},
		Spec: api.GiteaSiteSpec{DisplayName: name, URL: "https://gitea.example/", ManagerID: 1, Enabled: true, AcceptCreates: true,
			StartupConcurrency: 1, CleanupConcurrency: 16,
			Credential: api.ResourceReference{Name: "site-credential", UID: "credential-uid"}, Gateway: api.ResourceReference{Name: "gateway", UID: "gateway-uid"},
			Templates:       []api.ResourceReference{{Name: "default", UID: "template-uid"}},
			Quota:           corev1.ResourceList{corev1.ResourcePods: resource.MustParse("2"), corev1.ResourceRequestsCPU: resource.MustParse("2"), corev1.ResourceLimitsCPU: resource.MustParse("4"), corev1.ResourceRequestsMemory: resource.MustParse("2Gi"), corev1.ResourceLimitsMemory: resource.MustParse("4Gi"), corev1.ResourceRequestsStorage: resource.MustParse("10Gi"), corev1.ResourcePersistentVolumeClaims: resource.MustParse("2")},
			ContainerLimits: corev1.LimitRangeItem{Type: corev1.LimitTypeContainer},
		},
	}
}

func TestRuntimeAdmissionFailure(t *testing.T) {
	cs := &api.Codespace{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-example", UID: "runtime-uid"}, Spec: api.CodespaceSpec{Operation: api.Operation{Type: "create", Version: 1}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(cs).WithObjects(cs).Build()
	r := RuntimeReconciler{Client: c}
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cs), cs))
	_, err := r.condition(t.Context(), cs, "PodUnavailable", apierrors.NewTimeoutError("API request timed out", 1))
	require.NoError(t, err)
	require.Nil(t, cs.Status.Result)
	_, err = r.condition(t.Context(), cs, "PodUnavailable", apierrors.NewForbidden(corev1.Resource("pods"), "runtime", fmt.Errorf("exceeded quota")))
	require.NoError(t, err)
	require.NotNil(t, cs.Status.Result)
	require.EqualValues(t, 1, cs.Status.Result.Version)
	require.False(t, cs.Status.Result.Succeeded)
	require.Contains(t, cs.Status.Result.Message, "exceeded quota")
}

func TestRuntimeConfigurationRejectsDynamicResourceClaims(t *testing.T) {
	runtime := testRuntime()
	runtime.Resources.Claims = []corev1.ResourceClaim{{Name: "accelerator"}}
	require.EqualError(t, runtime.Validate(), "runtime dynamic resource claims are not supported")
}

func TestRuntimeConfigurationRequiresIsolationVolumeMode(t *testing.T) {
	runtime := testRuntime()
	runtime.Isolation = "kata"
	require.EqualError(t, runtime.Validate(), "kata isolation requires a raw Block volume so Docker data is mounted inside the guest")
	runtime.VolumeMode = corev1.PersistentVolumeBlock
	require.NoError(t, runtime.Validate())
	runtime.Isolation = "sysbox"
	require.EqualError(t, runtime.Validate(), "sysbox isolation requires a Filesystem volume")
}

func TestManagerRequiresDigestPinnedPlatformImage(t *testing.T) {
	_, err := NewManager(nil, ManagerOptions{
		Namespace: "codespace-system", ManagerURL: "https://manager.example.test", ComponentURL: "https://component.example.test",
		PlatformImage: "registry.example.test/codespace:latest", IdentityIssuer: "codespace-identity",
	})
	require.EqualError(t, err, "platform image must be pinned by digest")
}

func TestSiteOwnershipAndCleanup(t *testing.T) {
	ctx := t.Context()
	site := testSite("example", "site-uid")
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "codespace-example", UID: "other-ns", Labels: map[string]string{SiteUIDLabel: "other-site"}}}
	pullSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: "codespace-system"}, Type: corev1.SecretTypeDockerConfigJson, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&api.GiteaSite{}).WithObjects(site, ns, pullSecret).Build()
	r := &SiteReconciler{Client: c, ManagementNamespace: "codespace-system", ImagePullSecrets: []string{pullSecret.Name}}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(site)})
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(site), site))
	require.Equal(t, "NamespaceUnavailable", meta.FindStatusCondition(site.Status.Conditions, "InfrastructureReady").Reason)
	require.NoError(t, c.Delete(ctx, ns))
	ns.UID, ns.ResourceVersion, ns.Labels = "owned-ns", "", map[string]string{SiteUIDLabel: string(site.UID)}
	require.NoError(t, c.Create(ctx, ns))
	require.NoError(t, r.ensureNamespace(ctx, site, ns.Name))
	require.NoError(t, r.ensureSiteResources(ctx, site, ns.Name))
	require.NoError(t, c.Status().Update(ctx, site))
	var account corev1.ServiceAccount
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "codespace", Namespace: ns.Name}, &account))
	accountVersion := account.ResourceVersion
	require.NoError(t, r.ensureSiteResources(ctx, site, ns.Name))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(&account), &account))
	require.Equal(t, accountVersion, account.ResourceVersion)
	require.NotNil(t, account.AutomountServiceAccountToken)
	require.False(t, *account.AutomountServiceAccountToken)
	var copiedPullSecret corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: pullSecret.Name, Namespace: ns.Name}, &copiedPullSecret))
	require.Equal(t, string(site.UID), copiedPullSecret.Labels[SiteUIDLabel])
	require.Equal(t, "true", copiedPullSecret.Labels[imagePullSecretLabel])
	require.Equal(t, pullSecret.Data, copiedPullSecret.Data)
	r.ImagePullSecrets = nil
	require.NoError(t, r.ensureSiteResources(ctx, site, ns.Name))
	require.True(t, apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Name: pullSecret.Name, Namespace: ns.Name}, &copiedPullSecret)))
	var policy networkingv1.NetworkPolicy
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "codespace", Namespace: ns.Name}, &policy))
	require.Len(t, policy.Spec.PolicyTypes, 2)
	require.Equal(t, "gateway", policy.Spec.Ingress[0].From[0].PodSelector.MatchLabels[ComponentLabel])
	volume := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "retained", Namespace: ns.Name}}
	require.NoError(t, c.Create(ctx, volume))
	require.NoError(t, c.Delete(ctx, site))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(site), site))
	_, err = r.deleteSite(ctx, site, ns.Name)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(site), site))
	require.Equal(t, "CleanupRequired", meta.FindStatusCondition(site.Status.Conditions, "InfrastructureReady").Reason)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(ns), ns))
	require.True(t, ns.DeletionTimestamp.IsZero())
}

func TestRuntimeProvisioningAndVolumeRetention(t *testing.T) {
	ctx := t.Context()
	site := testSite("example", "site-uid")
	site.Status.NamespaceUID = "namespace-uid"
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "codespace-example", UID: "namespace-uid", Labels: map[string]string{SiteUIDLabel: string(site.UID)}}}
	cs := &api.Codespace{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: ns.Name, UID: "codespace-uid", Finalizers: []string{RuntimeFinalizer}},
		Spec:       api.CodespaceSpec{Site: api.ResourceReference{Name: site.Name, UID: site.UID}, CodespaceID: 1, RuntimeUUID: "933ef4a9-54c7-4f36-aad9-32ea72c4c986", EnvironmentTag: "standard", Runtime: testRuntime(), Operation: api.Operation{Version: 1, Type: "create", Payload: runtime.RawExtension{Raw: []byte(`{}`)}}},
		Status:     api.CodespaceStatus{Bound: true},
	}
	unusable := cs.DeepCopy()
	unusable.Spec.Runtime.Image = "invalid-platform-image"
	require.Error(t, unusable.Validate("codespace-system"))
	unusable.Spec.Operation.Type = "delete"
	require.NoError(t, unusable.Validate("codespace-system"))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&api.GiteaSite{}, &api.Codespace{}).WithObjects(site, ns, cs).Build()
	r := &RuntimeReconciler{Client: c, ManagementNamespace: "codespace-system", ManagerURL: "https://manager.codespace-system.svc:8443", IdentityIssuer: "codespace-internal", ImagePullSecrets: []string{"registry"}}
	r.Authority = &ExecutionAuthority{}
	r.Authority.Grant(cs.UID, cs.Spec.Operation.Version, time.Now().Add(time.Minute))
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cs)}
	_, err := r.Reconcile(ctx, request)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, request.NamespacedName, cs))
	require.NotEmpty(t, cs.Status.Pod.Name)
	_, err = r.Reconcile(ctx, request)
	require.NoError(t, err)
	var pod corev1.Pod
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: cs.Status.Pod.Name, Namespace: cs.Namespace}, &pod))
	require.False(t, *pod.Spec.AutomountServiceAccountToken)
	require.False(t, *pod.Spec.HostUsers)
	require.Equal(t, []corev1.LocalObjectReference{{Name: "registry"}}, pod.Spec.ImagePullSecrets)
	require.Equal(t, corev1.StorageMediumMemory, pod.Spec.Volumes[1].EmptyDir.Medium)
	require.Equal(t, "agent", pod.Spec.Containers[0].Command[1])
	var volume corev1.PersistentVolumeClaim
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: cs.Status.Volume.Name, Namespace: cs.Namespace}, &volume))
	require.Empty(t, volume.OwnerReferences)
	require.Equal(t, corev1.PersistentVolumeFilesystem, *volume.Spec.VolumeMode)
	// The consumer is created even while the real provisioner may be waiting for it.
	require.NotEmpty(t, pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
	require.NoError(t, c.Get(ctx, request.NamespacedName, cs))
	cs.Spec.Operation = api.Operation{Version: 2, Type: "stop", Payload: runtime.RawExtension{Raw: []byte(`{}`)}}
	require.NoError(t, c.Update(ctx, cs))
	cs.Status.Result = &api.OperationResult{Version: 2, Succeeded: true}
	require.NoError(t, c.Status().Update(ctx, cs))
	for range 3 {
		_, err = r.Reconcile(ctx, request)
		require.NoError(t, err)
	}
	require.NoError(t, c.Get(ctx, request.NamespacedName, cs))
	require.True(t, meta.IsStatusConditionTrue(cs.Status.Conditions, "Stopped"))
	require.Equal(t, &api.OperationResult{Version: 2, Succeeded: true}, cs.Status.Result)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(&volume), &volume))
	previousPod := pod.Name
	cs.Spec.Operation = api.Operation{Version: 3, Type: "resume", Payload: runtime.RawExtension{Raw: []byte(`{}`)}}
	require.NoError(t, c.Update(ctx, cs))
	_, err = r.Reconcile(ctx, request)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, request.NamespacedName, cs))
	require.Empty(t, cs.Status.Pod.Name)
	require.Equal(t, "AwaitingExecutionLease", meta.FindStatusCondition(cs.Status.Conditions, "InfrastructureReady").Reason)
	r.Authority.Grant(cs.UID, cs.Spec.Operation.Version, time.Now().Add(time.Minute))
	_, err = r.Reconcile(ctx, request)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, request.NamespacedName, cs))
	require.NotEmpty(t, cs.Status.Pod.Name)
	require.NotEqual(t, previousPod, cs.Status.Pod.Name)
	require.Equal(t, volume.Name, cs.Status.Volume.Name)
	cs.Status.Result = &api.OperationResult{Version: 3, Message: "postStartCommand failed"}
	require.NoError(t, c.Status().Update(ctx, cs))
	_, err = r.Reconcile(ctx, request)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, request.NamespacedName, cs))
	require.True(t, meta.IsStatusConditionTrue(cs.Status.Conditions, "Stopped"))
	operations := Operations{Client: c, Authority: r.Authority}
	remote := &operationRemote{final: func(request *codespacev1.FinalizeOperationRequest) (*codespacev1.FinalizeOperationResponse, error) {
		require.Equal(t, codespacev1.OperationType_OPERATION_TYPE_RESUME, request.OperationType)
		require.Equal(t, codespacev1.FinalStatus_FINAL_STATUS_FAILED, request.Status)
		return &codespacev1.FinalizeOperationResponse{}, nil
	}}
	require.NoError(t, operations.Flush(ctx, cs, remote))
	_, err = r.Reconcile(ctx, request)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(&volume), &volume))
}

func TestKataRuntimePodUsesRawBlockVolume(t *testing.T) {
	runtime := testRuntime()
	runtime.Isolation = "kata"
	runtime.RuntimeClassName = "kata-qemu-runtime-rs"
	runtime.VolumeMode = corev1.PersistentVolumeBlock
	cs := &api.Codespace{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-example", UID: "runtime-uid"},
		Spec:       api.CodespaceSpec{Site: api.ResourceReference{Name: "example", UID: "site-uid"}, RuntimeUUID: "933ef4a9-54c7-4f36-aad9-32ea72c4c986", Runtime: runtime},
		Status:     api.CodespaceStatus{Pod: api.ObservedResource{Name: "runtime-pod"}, Volume: api.ObservedResource{Name: "runtime-data"}, IdentitySecretName: "runtime-identity"},
	}
	pod, err := runtimePod(cs, "https://manager.example", nil)
	require.NoError(t, err)
	container := pod.Spec.Containers[0]
	require.True(t, *container.SecurityContext.Privileged)
	require.Equal(t, []corev1.VolumeDevice{{Name: "data", DevicePath: "/dev/codespace-data"}}, container.VolumeDevices)
	require.NotContains(t, container.VolumeMounts, corev1.VolumeMount{Name: "data", MountPath: "/var/lib/codespace"})
	require.Contains(t, container.Env, corev1.EnvVar{Name: "CODESPACE_DATA_DEVICE", Value: "/dev/codespace-data"})
}

func TestExitedRuntimePreservesVolume(t *testing.T) {
	for _, settled := range []bool{false, true} {
		t.Run(fmt.Sprintf("settled=%t", settled), func(t *testing.T) {
			site := testSite("example", "site-uid")
			site.Status.NamespaceUID = "namespace-uid"
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "codespace-example", UID: site.Status.NamespaceUID, Labels: map[string]string{SiteUIDLabel: string(site.UID)}}}
			cs := &api.Codespace{
				ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: ns.Name, UID: "runtime-uid", Finalizers: []string{RuntimeFinalizer}},
				Spec:       api.CodespaceSpec{Site: api.ResourceReference{Name: site.Name, UID: site.UID}, CodespaceID: 1, RuntimeUUID: "933ef4a9-54c7-4f36-aad9-32ea72c4c986", EnvironmentTag: "standard", Runtime: testRuntime(), Operation: api.Operation{Type: "resume", Version: 3, Payload: runtime.RawExtension{Raw: []byte(`{}`)}}},
				Status:     api.CodespaceStatus{Bound: true, Pod: api.ObservedResource{Name: "pod", UID: "pod-uid"}, Volume: api.ObservedResource{Name: "data-933ef4a9-54c7-4f36-aad9-32ea72c4c986", UID: "volume-uid"}},
			}
			if settled {
				cs.Status.SettledOperationVersion = 3
			}
			pod, err := runtimePod(cs, "https://manager.example", nil)
			require.NoError(t, err)
			pod.UID, pod.Spec.NodeName, pod.Status.Phase = cs.Status.Pod.UID, "worker", corev1.PodFailed
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "runtime", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}}}
			volume := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: cs.Status.Volume.Name, Namespace: cs.Namespace, UID: cs.Status.Volume.UID, Labels: map[string]string{RuntimeUIDLabel: string(cs.UID), SiteUIDLabel: string(site.UID)}}}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(cs, pod).WithObjects(site, ns, cs, pod, volume).Build()
			r := &RuntimeReconciler{Client: c, Authority: &ExecutionAuthority{}, ManagementNamespace: "codespace-system"}
			r.Authority.Grant(cs.UID, 3, time.Now().Add(time.Minute))
			for range 4 {
				_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cs)})
				require.NoError(t, err)
			}
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cs), cs))
			require.True(t, meta.IsStatusConditionTrue(cs.Status.Conditions, "Stopped"))
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(volume), volume))
			if settled {
				require.Equal(t, "stop", cs.Status.RecoveryAction)
			} else {
				require.NotNil(t, cs.Status.Result)
				require.False(t, cs.Status.Result.Succeeded)
			}
		})
	}
}

func TestLeadershipCancelsRequests(t *testing.T) {
	leadership := &Leadership{}
	request := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
	response := httptest.NewRecorder()
	leadership.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("standby handled a business request") })).ServeHTTP(response, request)
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	term, cancel := context.WithCancel(t.Context())
	leadership.ctx = term
	entered, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		leadership.Handler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(entered)
			<-r.Context().Done()
		})).ServeHTTP(httptest.NewRecorder(), request)
	}()
	<-entered
	cancel()
	<-finished
}

func TestLeadershipSelectsOnlyCurrentManagerPod(t *testing.T) {
	labels := map[string]string{"app.kubernetes.io/name": "codespace", "app.kubernetes.io/component": "manager"}
	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "codespace-system", Labels: maps.Clone(labels)}}
	oldPod.Labels[managerLeaderLabel] = "true"
	currentPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "current", Namespace: "codespace-system", Labels: maps.Clone(labels)}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(oldPod, currentPod).Build()
	leadership := &Leadership{Client: c, Namespace: "codespace-system", PodName: currentPod.Name}
	require.NoError(t, leadership.selectLeaderPod(t.Context()))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(oldPod), oldPod))
	require.NotContains(t, oldPod.Labels, managerLeaderLabel)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(currentPod), currentPod))
	require.Equal(t, "true", currentPod.Labels[managerLeaderLabel])
}

func TestRuntimeDeletionWaitsForWriterTermination(t *testing.T) {
	cs := &api.Codespace{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-example", UID: "runtime-uid"},
		Spec:       api.CodespaceSpec{Site: api.ResourceReference{Name: "example", UID: "site-uid"}, Operation: api.Operation{Type: "stop", Version: 2}},
		Status:     api.CodespaceStatus{Pod: api.ObservedResource{Name: "runtime-pod", UID: "pod-uid"}, Volume: api.ObservedResource{Name: "runtime-data", UID: "volume-uid"}},
	}
	pod, err := runtimePod(cs, "https://manager.example", nil)
	require.NoError(t, err)
	pod.UID, pod.Spec.NodeName, pod.Status.Phase = cs.Status.Pod.UID, "worker", corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "runtime", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	volume := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: cs.Status.Volume.Name, Namespace: cs.Namespace, UID: cs.Status.Volume.UID, Labels: map[string]string{RuntimeUIDLabel: string(cs.UID)}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(cs, pod).WithObjects(cs, pod, volume).Build()
	r := &RuntimeReconciler{Client: c}
	ctx := t.Context()
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(cs), cs))
	// Stop initiates graceful termination even if the Agent cannot submit a result.
	_, err = r.stopOrDelete(ctx, cs, false)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(pod), pod))
	require.False(t, pod.DeletionTimestamp.IsZero())
	_, err = r.stopOrDelete(ctx, cs, false)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(cs), cs))
	require.Equal(t, "WriterUnconfirmed", meta.FindStatusCondition(cs.Status.Conditions, "InfrastructureReady").Reason)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(volume), volume))
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137}}
	require.NoError(t, c.Status().Update(ctx, pod))
	_, err = r.stopOrDelete(ctx, cs, false)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(cs), cs))
	require.Equal(t, pod.UID, cs.Status.StoppedPodUID)
	_, err = r.stopOrDelete(ctx, cs, false)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(cs), cs))
	require.True(t, meta.IsStatusConditionTrue(cs.Status.Conditions, "Stopped"))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(volume), volume))

	// A force-removed Pod without the persisted termination fact cannot authorize data deletion.
	cs.Spec.Operation = api.Operation{Type: "delete", Version: 3}
	require.NoError(t, c.Update(ctx, cs))
	cs.Status.Pod = api.ObservedResource{Name: "lost-pod", UID: "lost-uid"}
	require.NoError(t, c.Status().Update(ctx, cs))
	_, err = r.stopOrDelete(ctx, cs, true)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(cs), cs))
	require.Equal(t, "WriterUnconfirmed", meta.FindStatusCondition(cs.Status.Conditions, "InfrastructureReady").Reason)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(volume), volume))
}

func TestRuntimeRecoveryConfirmationUnblocksMissingWriter(t *testing.T) {
	cs := &api.Codespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "runtime", Namespace: "codespace-example", UID: "runtime-uid",
			Annotations: map[string]string{WriterStoppedAnnotation: "lost-pod-uid"},
		},
		Spec: api.CodespaceSpec{Operation: api.Operation{Type: "stop", Version: 3}},
		Status: api.CodespaceStatus{
			Pod:    api.ObservedResource{Name: "lost-pod", UID: "lost-pod-uid"},
			Volume: api.ObservedResource{Name: "runtime-data", UID: "volume-uid"},
		},
	}
	volume := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: cs.Status.Volume.Name, Namespace: cs.Namespace, UID: cs.Status.Volume.UID,
		Labels: map[string]string{RuntimeUIDLabel: string(cs.UID)},
	}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(cs).WithObjects(cs, volume).Build()
	r := &RuntimeReconciler{Client: c}
	_, err := r.stopOrDelete(t.Context(), cs, false)
	require.NoError(t, err)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cs), cs))
	require.True(t, meta.IsStatusConditionTrue(cs.Status.Conditions, "Stopped"))
	require.Equal(t, types.UID("lost-pod-uid"), cs.Status.StoppedPodUID)
	require.NotContains(t, cs.Annotations, WriterStoppedAnnotation)
}

func TestRuntimeCertificateOwnership(t *testing.T) {
	cs := &api.Codespace{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-example", UID: "runtime-uid"}, Spec: api.CodespaceSpec{Site: api.ResourceReference{Name: "example", UID: "site-uid"}}, Status: api.CodespaceStatus{IdentitySecretName: "pod-identity"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "runtime-pod", Namespace: cs.Namespace, UID: "pod-uid"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	r := &RuntimeReconciler{Client: c, IdentityIssuer: "codespace-internal"}
	require.NoError(t, r.ensureIdentity(t.Context(), cs, pod))
	require.NoError(t, r.ensureIdentity(t.Context(), cs, pod))
	var secret corev1.Secret
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Namespace: cs.Namespace, Name: cs.Status.IdentitySecretName}, &secret))
	require.Equal(t, pod.UID, secret.OwnerReferences[0].UID)
	certificate := &unstructured.Unstructured{}
	certificate.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"})
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(&secret), certificate))
	require.NoError(t, unstructured.SetNestedField(certificate.Object, "another-secret", "spec", "secretName"))
	require.NoError(t, c.Update(t.Context(), certificate))
	require.ErrorContains(t, r.ensureIdentity(t.Context(), cs, pod), "secretName")
}

func TestGiteaCredentialRedirect(t *testing.T) {
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	site := testSite("example", "site-uid")
	site.Spec.URL, site.Status.CanonicalURL = origin.URL, origin.URL
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: site.Spec.Credential.Name, Namespace: "codespace-system", UID: site.Spec.Credential.UID, Labels: map[string]string{SiteUIDLabel: string(site.UID)}}, Data: map[string][]byte{"managerSecret": []byte("secret")}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(site).WithObjects(site, secret).Build()
	r := &SiteReconciler{Client: c, ManagementNamespace: secret.Namespace}
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(site)})
	require.NoError(t, err)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(site), site))
	require.Equal(t, "IdentityUnavailable", meta.FindStatusCondition(site.Status.Conditions, "InfrastructureReady").Reason)
	control := &AgentControlServer{Client: c, ManagementNamespace: secret.Namespace}
	remote, err := control.gitea(t.Context(), &api.Codespace{Spec: api.CodespaceSpec{Site: api.ResourceReference{Name: site.Name, UID: site.UID}}})
	require.NoError(t, err)
	request := connect.NewRequest(&codespacev1.CheckManagerRequest{ProtocolVersion: 1})
	_, err = remote.CheckManager(t.Context(), request)
	require.Error(t, err)
	require.Zero(t, forwarded.Load())
}
