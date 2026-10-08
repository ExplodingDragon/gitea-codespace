// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	configpkg "gitea.dev/codespace/internal/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestAdminSessionLifecycle(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	tokenFile := filepath.Join(t.TempDir(), "token")
	token := strings.Repeat("a", 64)
	require.NoError(t, os.WriteFile(tokenFile, []byte(token), 0o600))
	admin, err := NewAdminServer(c, "codespace-system", "https://Manager.Example.COM:443/", tokenFile)
	require.NoError(t, err)
	require.Equal(t, "https://manager.example.com", admin.PublicURL.String())
	request := func(server *AdminServer, method, path, origin, csrf string, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", csrf)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		return w
	}
	require.Equal(t, http.StatusForbidden, request(admin, "POST", "/api/admin/login", "https://attacker.example", "", nil, `{"token":"`+token+`"}`).Code)
	login := request(admin, "POST", "/api/admin/login", admin.PublicURL.String(), "", nil, `{"token":"`+token+`"}`)
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	cookies := login.Result().Cookies()
	require.Len(t, cookies, 1)
	require.True(t, cookies[0].Secure)
	require.True(t, cookies[0].HttpOnly)
	var session map[string]string
	require.NoError(t, json.Unmarshal(login.Body.Bytes(), &session))
	var secrets corev1.SecretList
	require.NoError(t, c.List(t.Context(), &secrets))
	require.Len(t, secrets.Items, 1)
	stored, err := json.Marshal(secrets.Items)
	require.NoError(t, err)
	require.NotContains(t, string(stored), cookies[0].Value)

	other, err := NewAdminServer(c, admin.Namespace, admin.PublicURL.String(), tokenFile)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, request(other, "GET", "/api/admin/session", "", "", cookies[0], "").Code)
	require.Equal(t, http.StatusForbidden, request(other, "DELETE", "/api/admin/session", admin.PublicURL.String(), "incorrect", cookies[0], "").Code)
	require.Equal(t, http.StatusNoContent, request(other, "DELETE", "/api/admin/session", admin.PublicURL.String(), session["csrf"], cookies[0], "").Code)
	require.Equal(t, http.StatusUnauthorized, request(admin, "GET", "/api/admin/session", "", "", cookies[0], "").Code)

	login = request(admin, "POST", "/api/admin/login", admin.PublicURL.String(), "", nil, `{"token":"`+token+`"}`)
	require.Equal(t, http.StatusOK, login.Code)
	require.NoError(t, os.WriteFile(tokenFile, []byte(strings.Repeat("b", 64)), 0o600))
	require.Equal(t, http.StatusUnauthorized, request(other, "GET", "/api/admin/session", "", "", login.Result().Cookies()[0], "").Code)
	require.NoError(t, other.pruneSessions(t.Context()))
	require.NoError(t, c.List(t.Context(), &secrets))
	require.Empty(t, secrets.Items)
}

func TestAdminSiteCredentialAndConcurrentEdit(t *testing.T) {
	ctx := t.Context()
	template := &api.EnvironmentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "template-uid"}, Spec: api.EnvironmentTemplateSpec{Tag: "standard", Runtime: testEnvironment()}}
	gateway := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "codespace-system", UID: "gateway-uid", Labels: map[string]string{ComponentLabel: "gateway"}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(template, gateway).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
		object.SetUID(types.UID(uuid.NewString()))
		object.SetCreationTimestamp(metav1.Now())
		return c.Create(ctx, object, opts...)
	}}).Build()
	server := &AdminServer{Client: c, Namespace: gateway.Namespace}
	site := testSite("example", "")
	site.ResourceVersion, site.Spec.Credential = "", api.ResourceReference{}
	spec := adminSiteSpec{GiteaSiteSpec: site.Spec, ManagerID: "9007199254740993"}
	raw, err := json.Marshal(spec)
	require.NoError(t, err)
	input := adminWrite{Name: site.Name, Spec: raw, ManagerSecret: strings.Repeat("secret", 8)}
	require.NoError(t, server.saveSite(ctx, site, input, true))
	require.EqualValues(t, 9007199254740993, site.Spec.ManagerID)
	var credential corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: site.Spec.Credential.Name}, &credential))
	require.Equal(t, string(site.UID), credential.Labels[SiteUIDLabel])
	require.Equal(t, site.UID, credential.OwnerReferences[0].UID)
	oldRef := site.Spec.Credential
	oldRevision := site.ResourceVersion
	spec.GiteaSiteSpec = site.Spec
	spec.DisplayName = "Updated site"
	raw, err = json.Marshal(spec)
	require.NoError(t, err)
	input.UID, input.ResourceVersion, input.Spec, input.ManagerSecret = site.UID, site.ResourceVersion, raw, strings.Repeat("updated", 8)
	encoded, err := json.Marshal(input)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	server.resources(w, httptest.NewRequest("PUT", "/api/admin/sites/example", bytes.NewReader(encoded)))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = httptest.NewRecorder()
	server.resources(w, httptest.NewRequest("PUT", "/api/admin/sites/example", bytes.NewReader(encoded)))
	require.Equal(t, http.StatusConflict, w.Code)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(site), site))
	require.NotEqual(t, oldRevision, site.ResourceVersion)
	require.NotEqual(t, oldRef, site.Spec.Credential)
	items, err := server.listResources(ctx, "sites")
	require.NoError(t, err)
	encoded, err = json.Marshal(items)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"managerID":"9007199254740993"`)
	require.NotContains(t, string(encoded), input.ManagerSecret)

	// A controller can finish the ownership binding after a lost HTTP response.
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: site.Spec.Credential.Name}, &credential))
	credential.Labels, credential.OwnerReferences = map[string]string{pendingSiteLabel: site.Name}, nil
	require.NoError(t, c.Update(ctx, &credential))
	require.NoError(t, bindSiteCredential(ctx, c, server.Namespace, site))
	var old corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: oldRef.Name}, &old))
	old.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
	require.NoError(t, c.Update(ctx, &old))
	require.NoError(t, server.pruneCredentials(ctx))
	require.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(&old), &old)))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(&credential), &credential))
	require.ErrorContains(t, server.checkDeletion(ctx, template), "selected by site")

	runtimeObject := &api.Codespace{ObjectMeta: metav1.ObjectMeta{Name: "workspace", Namespace: "codespace-example"}, Spec: api.CodespaceSpec{Operation: api.Operation{Payload: runtime.RawExtension{Raw: []byte(`{"sensitive":"private-value"}`)}}}}
	require.NoError(t, c.Create(ctx, runtimeObject))
	items, err = server.listResources(ctx, "runtimes")
	require.NoError(t, err)
	encoded, err = json.Marshal(items)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private-value")
}

func TestAdminConfirmsMissingRuntimeWriterByExactIdentity(t *testing.T) {
	cs := &api.Codespace{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-example", UID: "runtime-uid", ResourceVersion: "1"},
		Status: api.CodespaceStatus{
			Pod: api.ObservedResource{Name: "runtime-pod", UID: "pod-uid"},
			Conditions: []metav1.Condition{{
				Type: "InfrastructureReady", Status: metav1.ConditionFalse, Reason: "RecoveryRequired",
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(cs).WithObjects(cs).Build()
	server := &AdminServer{Client: c, Namespace: "codespace-system"}
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cs), cs))
	body, err := json.Marshal(adminRuntimeRecovery{UID: cs.UID, ResourceVersion: cs.ResourceVersion, PodUID: cs.Status.Pod.UID})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	server.resources(w, httptest.NewRequest(http.MethodPost, "/api/admin/runtimes/codespace-example/runtime/confirm-writer-stopped", bytes.NewReader(body)))
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cs), cs))
	require.Equal(t, "pod-uid", cs.Annotations[WriterStoppedAnnotation])

	w = httptest.NewRecorder()
	server.resources(w, httptest.NewRequest(http.MethodPost, "/api/admin/runtimes/codespace-example/runtime/confirm-writer-stopped", bytes.NewReader(body)))
	require.Equal(t, http.StatusConflict, w.Code)
}

func TestAdminComponentCredentialsFollowCommittedReferences(t *testing.T) {
	ctx := t.Context()
	namespace := "codespace-system"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
		object.SetUID(types.UID(uuid.NewString()))
		object.SetCreationTimestamp(metav1.Now())
		object.SetGeneration(1)
		return c.Create(ctx, object, opts...)
	}}).Build()
	server := &AdminServer{Client: c, Namespace: namespace}

	gatewayConfig := configpkg.DefaultGatewayConfig()
	gatewaySpec, err := json.Marshal(adminComponentSpec{Role: "gateway", DisplayName: "Primary gateway", Gateway: &gatewayConfig})
	require.NoError(t, err)
	gateway := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gateway"}}
	require.NoError(t, server.saveComponent(ctx, gateway, adminWrite{Name: gateway.Name, Spec: gatewaySpec}, true))
	var gatewaySecret corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: gateway.Data["sshHostKeySecret"]}, &gatewaySecret))
	require.Equal(t, gateway.UID, gatewaySecret.OwnerReferences[0].UID)
	require.Equal(t, string(gateway.UID), gatewaySecret.Labels[ComponentUIDLabel])
	require.NotEmpty(t, gatewaySecret.Data["hostKey"])
	originalGatewaySecret := gatewaySecret.DeepCopy()
	rotation, err := json.Marshal(adminWrite{Name: gateway.Name, UID: gateway.UID, ResourceVersion: gateway.ResourceVersion})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	server.resources(w, httptest.NewRequest(http.MethodPost, "/api/admin/components/gateway/rotate-ssh-host-key", bytes.NewReader(rotation)))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(gateway), gateway))
	require.NotEqual(t, originalGatewaySecret.Name, gateway.Data["sshHostKeySecret"])
	require.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(originalGatewaySecret), originalGatewaySecret)))
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: gateway.Data["sshHostKeySecret"]}, &gatewaySecret))
	require.True(t, ptr.Deref(gatewaySecret.Immutable, false))
	require.Equal(t, gateway.UID, gatewaySecret.OwnerReferences[0].UID)
	require.NotEqual(t, originalGatewaySecret.Data["hostKey"], gatewaySecret.Data["hostKey"])

	cacheConfig := configpkg.CacheConfig{
		Name: "Build cache", Enabled: true, Listen: ":5000", PublicURL: "https://cache.example.com", MaxSize: "1GiB", MaxAge: configpkg.Duration(24 * time.Hour), GCInterval: configpkg.Duration(time.Hour),
		Storage: configpkg.CacheStorageConfig{Driver: "s3", S3: configpkg.CacheS3Config{Endpoint: "https://s3.example.com", Region: "test", Bucket: "codespace"}},
	}
	cacheSpec, err := json.Marshal(adminComponentSpec{Role: "cache", DisplayName: "Build cache", Cache: &cacheConfig})
	require.NoError(t, err)
	cache := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cache"}}
	require.NoError(t, server.saveComponent(ctx, cache, adminWrite{Name: cache.Name, Spec: cacheSpec, S3AccessKey: "initial-access", S3SecretKey: "initial-secret"}, true))
	var persisted configpkg.CacheConfig
	require.NoError(t, json.Unmarshal([]byte(cache.Data["config"]), &persisted))
	require.EqualValues(t, 1, persisted.Revision)
	var original corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cache.Data["secretName"]}, &original))
	require.Equal(t, []byte("initial-access"), original.Data["s3AccessKey"])
	require.NotEmpty(t, original.Data["registryKey"])
	for _, component := range []*corev1.ConfigMap{gateway, cache} {
		deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: namespace, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(component, corev1.SchemeGroupVersion.WithKind("ConfigMap"))}}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1}}
		require.NoError(t, c.Create(ctx, deployment))
	}
	now := metav1.NowMicro()
	cacheLease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "cache-owner-" + cache.Name, Namespace: namespace, Labels: map[string]string{ComponentUIDLabel: string(cache.UID)}, Annotations: map[string]string{"codespace.gitea.dev/cache-bytes": "1024", "codespace.gitea.dev/mirror-bytes": "2048", "codespace.gitea.dev/cleanup-result": "complete"}}, Spec: coordinationv1.LeaseSpec{RenewTime: &now, LeaseDurationSeconds: ptr.To(int32(15))}}
	require.NoError(t, c.Create(ctx, cacheLease))

	items, err := server.listResources(ctx, "components")
	require.NoError(t, err)
	require.Len(t, items, 2)
	for _, item := range items {
		status := item.Status.(adminComponentStatus)
		require.True(t, status.Available)
		if item.Name == cache.Name {
			require.EqualValues(t, 1024, status.CacheBytes)
			require.EqualValues(t, 2048, status.MirrorBytes)
			require.Equal(t, "complete", status.CleanupResult)
			require.NotNil(t, status.LastHeartbeat)
		}
	}
	encoded, err := json.Marshal(items)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "initial-access")
	require.NotContains(t, string(encoded), "initial-secret")
	require.NotContains(t, string(encoded), string(gatewaySecret.Data["hostKey"]))
	require.NotContains(t, string(encoded), "PRIVATE KEY")

	cacheSpec, err = json.Marshal(adminComponentSpec{Role: "cache", DisplayName: "Updated cache", Cache: &cacheConfig})
	require.NoError(t, err)
	require.NoError(t, server.saveComponent(ctx, cache, adminWrite{Name: cache.Name, UID: cache.UID, ResourceVersion: cache.ResourceVersion, Spec: cacheSpec, S3AccessKey: "rotated-access", S3SecretKey: "rotated-secret"}, false))
	require.NoError(t, json.Unmarshal([]byte(cache.Data["config"]), &persisted))
	require.EqualValues(t, 2, persisted.Revision)
	require.NotEqual(t, original.Name, cache.Data["secretName"])
	var rotated corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cache.Data["secretName"]}, &rotated))
	require.Equal(t, original.Data["registryKey"], rotated.Data["registryKey"])
	require.Equal(t, original.Data["tokenKey"], rotated.Data["tokenKey"])
	require.Equal(t, []byte("rotated-access"), rotated.Data["s3AccessKey"])
	require.Equal(t, string(cache.UID), rotated.Labels[ComponentUIDLabel])

	// Reconciliation completes ownership binding when the configuration update
	// committed but the administration request ended before the final update.
	delete(rotated.Labels, ComponentUIDLabel)
	rotated.Labels[pendingComponentLabel], rotated.OwnerReferences = cache.Name, nil
	require.NoError(t, c.Update(ctx, &rotated))
	require.NoError(t, bindComponentCredential(ctx, c, cache))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(&rotated), &rotated))
	require.Equal(t, string(cache.UID), rotated.Labels[ComponentUIDLabel])

	original.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
	require.NoError(t, c.Update(ctx, &original))
	require.NoError(t, server.pruneCredentials(ctx))
	require.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(&original), &original)))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(&rotated), &rotated))
}
