// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestKubernetesE2EResourceOwnership(t *testing.T) {
	if os.Getenv("CODESPACE_TEST_KUBERNETES") != "1" {
		t.Skip("set CODESPACE_TEST_KUBERNETES=1 with a test-cluster kubeconfig and installed CRDs")
	}
	config, err := ctrl.GetConfig()
	require.NoError(t, err)
	c, err := client.New(config, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	name := "e2e-" + uuid.NewString()
	site := testSite(name, "")
	site.ResourceVersion, site.Generation, site.Finalizers = "", 0, nil
	site.Spec.Enabled = false
	require.NoError(t, c.Create(ctx, site))
	namespace := "codespace-" + name
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		var codespaces api.CodespaceList
		if err := c.List(cleanup, &codespaces, client.InNamespace(namespace)); err == nil {
			for i := range codespaces.Items {
				codespace := &codespaces.Items[i]
				codespace.Finalizers = nil
				require.NoError(t, c.Update(cleanup, codespace))
				require.NoError(t, client.IgnoreNotFound(c.Delete(cleanup, codespace, client.Preconditions{UID: &codespace.UID})))
			}
		}
		var current api.GiteaSite
		if err := c.Get(cleanup, client.ObjectKeyFromObject(site), &current); err == nil && current.UID == site.UID {
			current.Finalizers = nil
			require.NoError(t, c.Update(cleanup, &current))
			require.NoError(t, client.IgnoreNotFound(c.Delete(cleanup, &current, client.Preconditions{UID: &site.UID})))
		}
		var ns corev1.Namespace
		if err := c.Get(cleanup, types.NamespacedName{Name: namespace}, &ns); err == nil && ns.Labels[SiteUIDLabel] == string(site.UID) {
			require.NoError(t, c.Delete(cleanup, &ns, client.Preconditions{UID: &ns.UID}))
		}
	})
	r := &SiteReconciler{Client: c, ManagementNamespace: "codespace-system"}
	require.Eventually(t, func() bool {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(site)}); err != nil {
			return false
		}
		return c.Get(ctx, client.ObjectKeyFromObject(site), site) == nil && site.Status.NamespaceUID != ""
	}, 10*time.Second, 100*time.Millisecond)
	var ns corev1.Namespace
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: namespace}, &ns))
	require.Equal(t, ns.UID, site.Status.NamespaceUID)
	require.Equal(t, string(site.UID), ns.Labels[SiteUIDLabel])
	var quota corev1.ResourceQuota
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "codespace"}, &quota))
	require.Equal(t, site.Spec.Quota, quota.Spec.Hard)
	stale := site.DeepCopy()
	site.Spec.DisplayName = "Updated site"
	require.NoError(t, c.Update(ctx, site))
	stale.Spec.DisplayName = "Stale edit"
	require.True(t, apierrors.IsConflict(c.Update(ctx, stale)))
	changedIdentity := site.DeepCopy()
	changedIdentity.Spec.ManagerID++
	require.True(t, apierrors.IsInvalid(c.Update(ctx, changedIdentity)))
	// Mutating a copy must not change another reader's quota snapshot.
	copy := site.DeepCopy()
	delete(copy.Spec.Quota, corev1.ResourcePods)
	require.Contains(t, site.Spec.Quota, corev1.ResourcePods)
	wrong := &api.GiteaSite{ObjectMeta: metav1.ObjectMeta{Name: name + "-other", UID: "another-site"}, Spec: site.Spec}
	require.Error(t, r.ensureNamespace(ctx, wrong, namespace))

	cs := &api.Codespace{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: namespace},
		Spec:       api.CodespaceSpec{Site: api.ResourceReference{Name: site.Name, UID: site.UID}, CodespaceID: 1, RuntimeUUID: uuid.NewString(), EnvironmentTag: "standard", Runtime: testRuntime(), Operation: api.Operation{Version: 1, Type: "stop", Payload: runtime.RawExtension{Raw: []byte(`{}`)}}},
	}
	require.NoError(t, c.Create(ctx, cs))
	changed := cs.DeepCopy()
	changed.Spec.Runtime.GitSSHKeyType = "rsa-4096"
	require.True(t, apierrors.IsInvalid(c.Update(ctx, changed)))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "writer", Namespace: namespace, Finalizers: []string{WriterFinalizer}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cs, api.GroupVersion.WithKind("Codespace"))}},
		Spec:       corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: ptr.To(false), TerminationGracePeriodSeconds: ptr.To(int64(1)), Containers: []corev1.Container{{Name: "runtime", Image: "docker.io/library/busybox:1.37.0", ImagePullPolicy: corev1.PullNever, Command: []string{"sleep", "3600"}, Resources: testRuntime().Resources}}},
	}
	require.NoError(t, c.Create(ctx, pod))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		var current corev1.Pod
		if err := c.Get(cleanup, client.ObjectKeyFromObject(pod), &current); err == nil && current.UID == pod.UID {
			current.Finalizers = nil
			require.NoError(t, c.Update(cleanup, &current))
		}
	})
	require.Eventually(t, func() bool {
		return c.Get(ctx, client.ObjectKeyFromObject(pod), pod) == nil && pod.Status.Phase == corev1.PodRunning
	}, 30*time.Second, 200*time.Millisecond)
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.Get(ctx, client.ObjectKeyFromObject(cs), cs); err != nil {
			return err
		}
		cs.Status.Pod = api.ObservedResource{Name: pod.Name, UID: pod.UID}
		return c.Status().Update(ctx, cs)
	}))
	runtimeController := &RuntimeReconciler{Client: c}
	require.Eventually(t, func() bool {
		if err := c.Get(ctx, client.ObjectKeyFromObject(cs), cs); err != nil {
			return false
		}
		_, err := runtimeController.stopOrDelete(ctx, cs, false)
		return err == nil && cs.Status.StoppedPodUID == pod.UID && cs.Status.Pod.Name == ""
	}, 30*time.Second, 200*time.Millisecond)
	require.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{})))

	template := &api.EnvironmentTemplate{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: api.EnvironmentTemplateSpec{Tag: "standard", Runtime: testEnvironment()}}
	require.NoError(t, c.Create(ctx, template))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		require.NoError(t, client.IgnoreNotFound(c.Delete(cleanup, template, client.Preconditions{UID: &template.UID})))
	})
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.Get(ctx, client.ObjectKeyFromObject(template), template); err != nil {
			return err
		}
		template.Status.VerifiedGeneration = template.Generation
		template.Status.Verification = "resource ownership E2E"
		meta.SetStatusCondition(&template.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Verified", ObservedGeneration: template.Generation})
		return c.Status().Update(ctx, template)
	}))
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.Get(ctx, client.ObjectKeyFromObject(site), site); err != nil {
			return err
		}
		site.Spec.Templates = []api.ResourceReference{{Name: template.Name, UID: template.UID}}
		return c.Update(ctx, site)
	}))
	remote := &operationRemote{bind: func(request *codespacev1.BindRuntimeIdentityRequest) (*codespacev1.BindRuntimeIdentityResponse, error) {
		return &codespacev1.BindRuntimeIdentityResponse{RuntimeUuid: request.RuntimeUuid}, nil
	}, final: func(request *codespacev1.FinalizeOperationRequest) (*codespacev1.FinalizeOperationResponse, error) {
		require.Equal(t, codespacev1.FinalStatus_FINAL_STATUS_FAILED, request.Status)
		return &codespacev1.FinalizeOperationResponse{}, nil
	}}
	operations := Operations{Client: c, Authority: &ExecutionAuthority{}, ManagementNamespace: "codespace-system", PlatformImage: testRuntime().Image}
	require.NoError(t, operations.Accept(ctx, site, remote, &codespacev1.OperationPayload{CodespaceId: 2, OperationRversion: 1, LeaseValidForMilliseconds: 60000, Command: &codespacev1.OperationPayload_Create{Create: &codespacev1.CreateOperationPayload{EnvironmentTag: "standard", RuntimeSettings: &codespacev1.EffectiveCodespaceRuntimeSettings{}}}}, time.Now()))
	var allocated api.Codespace
	key := types.NamespacedName{Namespace: namespace, Name: "codespace-2"}
	require.NoError(t, c.Get(ctx, key, &allocated))
	require.True(t, allocated.Status.Bound)
	require.Positive(t, operations.Authority.Remaining(allocated.UID, 1))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		var current api.Codespace
		if c.Get(cleanup, key, &current) == nil && current.UID == allocated.UID {
			current.Finalizers = nil
			require.NoError(t, c.Update(cleanup, &current))
			require.NoError(t, client.IgnoreNotFound(c.Delete(cleanup, &current, client.Preconditions{UID: &allocated.UID})))
		}
	})
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.Get(ctx, key, &allocated); err != nil {
			return err
		}
		allocated.Status.Result = &api.OperationResult{Version: 1, Message: "bootstrap failed"}
		return c.Status().Update(ctx, &allocated)
	}))
	runtimeController.ManagementNamespace = "codespace-system"
	_, err = runtimeController.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, key, &allocated))
	require.NoError(t, operations.Flush(ctx, &allocated, remote))
	require.NoError(t, c.Get(ctx, key, &allocated))
	require.EqualValues(t, 1, allocated.Status.SettledOperationVersion)
	require.Equal(t, "delete", allocated.Status.RecoveryAction)
	require.Eventually(t, func() bool {
		_, _ = runtimeController.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		return apierrors.IsNotFound(c.Get(ctx, key, &api.Codespace{}))
	}, 30*time.Second, 200*time.Millisecond)
}

func TestKubernetesE2EAgentCertificate(t *testing.T) {
	if os.Getenv("CODESPACE_TEST_KUBERNETES") != "1" {
		t.Skip("set CODESPACE_TEST_KUBERNETES=1 with installed CRDs, cert-manager and the imported BusyBox test image")
	}
	config, err := ctrl.GetConfig()
	require.NoError(t, err)
	c, err := client.New(config, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	name := "identity-" + uuid.NewString()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	require.NoError(t, c.Create(ctx, ns))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		require.NoError(t, client.IgnoreNotFound(c.Delete(cleanup, ns, client.Preconditions{UID: &ns.UID})))
	})
	issuer := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1", "kind": "ClusterIssuer",
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"selfSigned": map[string]any{}},
	}}
	require.NoError(t, c.Create(ctx, issuer))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		uid := issuer.GetUID()
		require.NoError(t, client.IgnoreNotFound(c.Delete(cleanup, issuer, client.Preconditions{UID: &uid})))
	})
	cs := &api.Codespace{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: ns.Name, UID: "codespace-uid"},
		Spec:       api.CodespaceSpec{Site: api.ResourceReference{Name: "test", UID: "site-uid"}},
		Status:     api.CodespaceStatus{IdentitySecretName: "runtime-identity"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "identity", Namespace: ns.Name},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: ptr.To(false), RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{Name: "fixture", Image: "docker.io/library/busybox:1.37.0", ImagePullPolicy: corev1.PullNever, Command: []string{"sleep", "3600"}, VolumeMounts: []corev1.VolumeMount{{Name: "identity", MountPath: "/run/identity", ReadOnly: true}}}},
			Volumes:    []corev1.Volume{{Name: "identity", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: cs.Status.IdentitySecretName, Items: []corev1.KeyToPath{{Key: "tls.crt", Path: "tls.crt"}, {Key: "tls.key", Path: "tls.key"}, {Key: "ca.crt", Path: "ca.crt"}}}}}},
		},
	}
	require.NoError(t, c.Create(ctx, pod))
	require.NotEmpty(t, pod.UID)
	r := &RuntimeReconciler{Client: c, IdentityIssuer: name}
	require.NoError(t, r.ensureIdentity(ctx, cs, pod))
	var secret corev1.Secret
	require.Eventually(t, func() bool {
		return c.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cs.Status.IdentitySecretName}, &secret) == nil && len(secret.Data["tls.crt"]) > 0
	}, time.Minute, 200*time.Millisecond)
	block, _ := pem.Decode(secret.Data["tls.crt"])
	require.NotNil(t, block)
	certificate, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	require.Len(t, certificate.URIs, 1)
	require.Equal(t, "spiffe://codespace/agent/site-uid/codespace-uid/"+string(pod.UID), certificate.URIs[0].String())
	require.NoError(t, r.ensureIdentity(ctx, cs, pod))
	require.Eventually(t, func() bool {
		return c.Get(ctx, client.ObjectKeyFromObject(pod), pod) == nil && pod.Status.Phase == corev1.PodRunning
	}, time.Minute, 200*time.Millisecond)
	// A Pod replacement must receive a different identity, even at the same name.
	replacement := pod.DeepCopy()
	replacement.UID = "replacement-uid"
	require.Error(t, r.ensureIdentity(ctx, cs, replacement))
	require.NoError(t, c.Delete(ctx, pod, client.Preconditions{UID: &pod.UID}))
	require.Eventually(t, func() bool {
		return apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(&secret), &corev1.Secret{}))
	}, time.Minute, 200*time.Millisecond)
}
