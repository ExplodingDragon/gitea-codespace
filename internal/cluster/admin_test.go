// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	admin, err := NewAdminServer(c, "codespace-system", tokenFile)
	require.NoError(t, err)
	request := func(server *AdminServer, method, path, origin, csrf string, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Origin", origin)
		r.Header.Set("Accept-Language", "zh-CN")
		r.Header.Set("X-CSRF-Token", csrf)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		return w
	}
	login := request(admin, "POST", "/api/admin/login", "https://manager.example.com", "", nil, `{"token":"`+token+`"}`)
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

	other, err := NewAdminServer(c, admin.Namespace, tokenFile)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, request(other, "GET", "/api/admin/session", "", "", cookies[0], "").Code)
	require.Equal(t, http.StatusForbidden, request(other, "DELETE", "/api/admin/session", "http://127.0.0.1:18080", "incorrect", cookies[0], "").Code)
	require.Equal(t, http.StatusNoContent, request(other, "DELETE", "/api/admin/session", "http://127.0.0.1:18080", session["csrf"], cookies[0], "").Code)
	require.Equal(t, http.StatusUnauthorized, request(admin, "GET", "/api/admin/session", "", "", cookies[0], "").Code)

	login = request(admin, "POST", "/api/admin/login", "http://127.0.0.1:18080", "", nil, `{"token":"`+token+`"}`)
	require.Equal(t, http.StatusOK, login.Code)
	require.False(t, login.Result().Cookies()[0].Secure)
	require.NoError(t, os.WriteFile(tokenFile, []byte(strings.Repeat("b", 64)), 0o600))
	require.Equal(t, http.StatusUnauthorized, request(other, "GET", "/api/admin/session", "", "", login.Result().Cookies()[0], "").Code)
	require.NoError(t, other.pruneSessions(t.Context()))
	require.NoError(t, c.List(t.Context(), &secrets))
	require.Empty(t, secrets.Items)
}

func TestAdminClusterOptionsAndTemplateDependencies(t *testing.T) {
	defaultClass := "true"
	runtimeClass := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "sysbox-runc", Labels: map[string]string{api.RuntimeIsolationLabel: api.IsolationSysbox}}, Handler: "sysbox-runc"}
	unmanagedRuntime := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "runc"}, Handler: "runc"}
	storageClass := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "local-path", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": defaultClass}}, Provisioner: "rancher.io/local-path"}
	ingressClass := &networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "nginx", Annotations: map[string]string{"ingressclass.kubernetes.io/is-default-class": "true"}}, Spec: networkingv1.IngressClassSpec{Controller: "k8s.io/ingress-nginx"}}
	tlsSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "wildcard-tls", Namespace: "codespace-system"}, Type: corev1.SecretTypeTLS, Data: map[string][]byte{corev1.TLSCertKey: []byte("certificate"), corev1.TLSPrivateKeyKey: []byte("private-key")}}
	issuer := &unstructured.Unstructured{}
	issuer.SetAPIVersion("cert-manager.io/v1")
	issuer.SetKind("Issuer")
	issuer.SetName("letsencrypt")
	issuer.SetNamespace("codespace-system")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(runtimeClass, unmanagedRuntime, storageClass, ingressClass, tlsSecret, issuer).Build()
	server := &AdminServer{Client: c, Namespace: "codespace-system"}

	options, err := server.clusterOptions(t.Context())
	require.NoError(t, err)
	require.Equal(t, []adminRuntimeClass{{Name: "sysbox-runc", Handler: "sysbox-runc", Isolation: api.IsolationSysbox}}, options.RuntimeClasses)
	require.Equal(t, []adminStorageClass{{Name: "local-path", Provisioner: "rancher.io/local-path", Default: true}}, options.StorageClasses)
	require.Equal(t, []adminIngressClass{{Name: "nginx", Controller: "k8s.io/ingress-nginx", Default: true}}, options.IngressClasses)
	require.Equal(t, []string{"wildcard-tls"}, options.TLSSecrets)
	require.Equal(t, []adminIssuer{{Kind: "Issuer", Name: "letsencrypt"}}, options.Issuers)
	w := httptest.NewRecorder()
	server.handleResources(w, httptest.NewRequest(http.MethodGet, "/api/admin/cluster-options", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"isolation":"sysbox"`)

	template := &api.EnvironmentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	require.NoError(t, server.saveTemplate(t.Context(), template, adminTemplateWrite{Spec: api.EnvironmentTemplateSpec{Tag: "standard", Runtime: testEnvironment()}}, true))
	template.Spec.Runtime.Isolation = api.IsolationKata
	require.ErrorContains(t, validateEnvironmentDependencies(t.Context(), c, template.Spec.Runtime), "not labeled for kata isolation")

	cache := testCacheComponent("cache", "")
	emptyServer := &AdminServer{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), Namespace: "codespace-system"}
	require.ErrorContains(t, emptyServer.saveComponent(t.Context(), cache, adminComponentWrite{Spec: cache.Spec}, true), "requires an available StorageClass")
	cache.Spec.Cache.Storage.StorageClassName = "missing"
	require.ErrorContains(t, server.saveComponent(t.Context(), cache, adminComponentWrite{Spec: cache.Spec}, true), "storage class \"missing\" is unavailable")
	cache.Spec.Cache.Storage.StorageClassName = "local-path"
	require.NoError(t, server.saveComponent(t.Context(), cache, adminComponentWrite{Spec: cache.Spec}, true))
}

func TestAdminSiteRegistrationAndConcurrentEdit(t *testing.T) {
	ctx := t.Context()
	registration := &adminRegistrationService{}
	rpcServer := connect.NewServer()
	codespacev1connect.RegisterManagerServiceHandler(rpcServer, registration)
	rpcHandler := http.NewServeMux()
	connecthttp.Mount(rpcHandler, rpcServer)
	path := "/" + codespacev1connect.ManagerServiceName + "/"
	mux := http.NewServeMux()
	mux.Handle("/api/codespace"+path, http.StripPrefix("/api/codespace", rpcHandler))
	gitea := httptest.NewTLSServer(mux)
	t.Cleanup(gitea.Close)
	registration.webURL = "https://gitea.example.com"

	template := &api.EnvironmentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "template-uid"}, Spec: api.EnvironmentTemplateSpec{Tag: "standard", Runtime: testEnvironment()}}
	gateway := testGatewayComponent("gateway", "gateway-uid")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(template, gateway).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
		object.SetUID(types.UID(uuid.NewV4().String()))
		object.SetCreationTimestamp(metav1.Now())
		return c.Create(ctx, object, opts...)
	}}).Build()
	server := &AdminServer{Client: c, Namespace: "codespace-system", RegistrationHTTPClient: gitea.Client()}
	site := testSite("example", "")
	site.Spec.URL = gitea.URL
	site.Spec.CABundle = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: gitea.Certificate().Raw}))
	site.ResourceVersion, site.Spec.Credential = "", api.ResourceReference{}
	spec := adminSiteSpec{
		DisplayName: site.Spec.DisplayName, URL: site.Spec.URL, CABundle: site.Spec.CABundle, Enabled: site.Spec.Enabled, AcceptCreates: site.Spec.AcceptCreates,
		StartupConcurrency: site.Spec.StartupConcurrency, CleanupConcurrency: site.Spec.CleanupConcurrency,
		Templates: site.Spec.Templates, Gateway: site.Spec.Gateway, Caches: site.Spec.Caches, Quota: site.Spec.Quota,
		Upstreams: site.Spec.Upstreams,
	}
	input := adminSiteWrite{adminWriteIdentity: adminWriteIdentity{Name: site.Name}, Spec: spec, RegistrationToken: strings.Repeat("t", 40)}
	invalid := site.DeepCopy()
	invalid.Name = "invalid"
	invalidInput := input
	invalidInput.Name = invalid.Name
	invalidInput.Spec.Templates = []api.ResourceReference{{Name: "missing", UID: "missing-uid"}}
	require.Error(t, server.saveSite(ctx, invalid, invalidInput, true))
	require.Zero(t, registration.calls)
	require.NoError(t, server.saveSite(ctx, site, input, true))
	require.Equal(t, 1, registration.calls)
	require.EqualValues(t, 9007199254740993, site.Spec.ManagerID)
	require.Equal(t, "example", site.Spec.DisplayName)
	require.Equal(t, gitea.URL, site.Spec.URL)
	require.Equal(t, strings.Repeat("t", 40), registration.token)
	require.Equal(t, "example", registration.name)
	require.Equal(t, spec, newAdminSiteSpec(site.Spec))
	var credential corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: site.Spec.Credential.Name}, &credential))
	require.Equal(t, string(site.UID), credential.Labels[SiteUIDLabel])
	require.Equal(t, site.UID, credential.OwnerReferences[0].UID)
	generatedInput := input
	generatedInput.Name = ""
	generatedInput.Spec.DisplayName = "Generated site"
	encoded, err := json.Marshal(generatedInput)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	server.handleResources(w, httptest.NewRequest(http.MethodPost, "/api/admin/sites", bytes.NewReader(encoded)))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var created adminResource
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	require.Regexp(t, `^site-[0-9a-f-]{36}$`, created.Name)
	require.NotEmpty(t, created.UID)
	require.NotEmpty(t, created.ResourceVersion)
	var generated api.GiteaSite
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: created.Name}, &generated))
	require.Equal(t, "Generated site", generated.Spec.DisplayName)

	oldRef := site.Spec.Credential
	oldRevision := site.ResourceVersion
	spec.DisplayName = "Updated site"
	input.UID, input.ResourceVersion, input.Spec, input.RegistrationToken = site.UID, site.ResourceVersion, spec, ""
	encoded, err = json.Marshal(input)
	require.NoError(t, err)
	w = httptest.NewRecorder()
	server.handleResources(w, httptest.NewRequest("PUT", "/api/admin/sites/example", bytes.NewReader(encoded)))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = httptest.NewRecorder()
	server.handleResources(w, httptest.NewRequest("PUT", "/api/admin/sites/example", bytes.NewReader(encoded)))
	require.Equal(t, http.StatusConflict, w.Code)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(site), site))
	require.NotEqual(t, oldRevision, site.ResourceVersion)
	require.Equal(t, oldRef, site.Spec.Credential)
	items, err := server.listResources(ctx, "sites")
	require.NoError(t, err)
	encoded, err = json.Marshal(items)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "managerID")
	require.NotContains(t, string(encoded), "credential")
	require.NotContains(t, string(encoded), strings.Repeat("s", 40))

	// A controller can finish the ownership binding after a lost HTTP response.
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: site.Spec.Credential.Name}, &credential))
	credential.Labels, credential.OwnerReferences = map[string]string{pendingSiteLabel: site.Name}, nil
	require.NoError(t, c.Update(ctx, &credential))
	require.NoError(t, bindSiteCredential(ctx, c, server.Namespace, site))
	old := corev1.Secret{ObjectMeta: metav1.ObjectMeta{GenerateName: "site-credential-", Namespace: server.Namespace, Labels: map[string]string{pendingSiteLabel: "missing-site"}}}
	require.NoError(t, c.Create(ctx, &old))
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

type adminRegistrationService struct {
	codespacev1connect.UnimplementedManagerServiceHandler
	webURL string
	token  string
	name   string
	calls  int
}

func (s *adminRegistrationService) RegisterManager(_ context.Context, request *codespacev1.RegisterManagerRequest) (*codespacev1.RegisterManagerResponse, error) {
	s.calls++
	s.token = request.GetToken()
	s.name = request.GetName()
	return &codespacev1.RegisterManagerResponse{
		ManagerId: 9007199254740993, ManagerSecret: strings.Repeat("s", 40), ManagerName: request.Name, GiteaWebUrl: s.webURL,
	}, nil
}

func (s *adminRegistrationService) CheckManager(context.Context, *codespacev1.CheckManagerRequest) (*codespacev1.CheckManagerResponse, error) {
	return &codespacev1.CheckManagerResponse{GiteaWebUrl: s.webURL, ManagerName: s.name}, nil
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
	server.handleResources(w, httptest.NewRequest(http.MethodPost, "/api/admin/runtimes/codespace-example/runtime/confirm-writer-stopped", bytes.NewReader(body)))
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cs), cs))
	require.Equal(t, "pod-uid", cs.Annotations[WriterStoppedAnnotation])

	w = httptest.NewRecorder()
	server.handleResources(w, httptest.NewRequest(http.MethodPost, "/api/admin/runtimes/codespace-example/runtime/confirm-writer-stopped", bytes.NewReader(body)))
	require.Equal(t, http.StatusConflict, w.Code)
}

func TestAdminDeletesRuntimeOnlyAfterDisablingSite(t *testing.T) {
	site := testSite("example", "site-uid")
	cs := &api.Codespace{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-example", UID: "runtime-uid"},
		Spec:       api.CodespaceSpec{Site: api.ResourceReference{Name: site.Name, UID: site.UID}},
		Status:     api.CodespaceStatus{Pod: api.ObservedResource{Name: "pod", UID: "pod-uid"}, Target: &api.AgentTarget{Ready: true}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(cs).WithObjects(site, cs).Build()
	s := &AdminServer{Client: c, Namespace: "codespace-system"}
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cs), cs))
	body, err := json.Marshal(adminRuntimeRecovery{UID: cs.UID, ResourceVersion: cs.ResourceVersion, PodUID: cs.Status.Pod.UID})
	require.NoError(t, err)
	remove := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleResources(w, httptest.NewRequest(http.MethodDelete, "/api/admin/runtimes/"+cs.Namespace+"/"+cs.Name, bytes.NewReader(body)))
		return w
	}
	require.Equal(t, http.StatusConflict, remove().Code)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(site), site))
	site.Spec.Enabled = false
	require.NoError(t, c.Update(t.Context(), site))
	w := remove()
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	require.Equal(t, http.StatusConflict, remove().Code)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cs), cs))
	require.Equal(t, "delete", cs.Status.RecoveryAction)
	require.Nil(t, cs.Status.Target)
}

func TestAdminComponentCredentialsFollowCommittedReferences(t *testing.T) {
	ctx := t.Context()
	namespace := "codespace-system"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
		object.SetUID(types.UID(uuid.NewV4().String()))
		object.SetCreationTimestamp(metav1.Now())
		object.SetGeneration(1)
		return c.Create(ctx, object, opts...)
	}}).Build()
	server := &AdminServer{Client: c, Namespace: namespace}

	gatewaySpec := testGatewayComponent("gateway", "").Spec
	gateway := &api.CodespaceComponent{ObjectMeta: metav1.ObjectMeta{Name: "gateway"}}
	require.NoError(t, server.saveComponent(ctx, gateway, adminComponentWrite{adminWriteIdentity: adminWriteIdentity{Name: gateway.Name}, Spec: gatewaySpec}, true))
	gatewaySecret := testComponentCredential(gateway, nil)
	generated, err := newGatewayHostKeySecret(gatewaySecret.Name, namespace)
	require.NoError(t, err)
	gatewaySecret.Data = generated.Data
	gatewaySecret.Data[gatewaySessionKey] = []byte("0123456789abcdef0123456789abcdef")
	require.NoError(t, c.Create(ctx, gatewaySecret))
	originalHostKey := append([]byte(nil), gatewaySecret.Data["hostKey"]...)
	originalSessionKey := append([]byte(nil), gatewaySecret.Data[gatewaySessionKey]...)
	rotation, err := json.Marshal(adminWriteIdentity{Name: gateway.Name, UID: gateway.UID, ResourceVersion: gateway.ResourceVersion})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	server.handleResources(w, httptest.NewRequest(http.MethodPost, "/api/admin/components/gateway/rotate-ssh-host-key", bytes.NewReader(rotation)))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(gatewaySecret), gatewaySecret))
	require.Equal(t, gateway.UID, gatewaySecret.OwnerReferences[0].UID)
	require.NotEqual(t, originalHostKey, gatewaySecret.Data["hostKey"])
	require.Equal(t, originalSessionKey, gatewaySecret.Data[gatewaySessionKey])

	cacheSpec := testCacheComponent("cache", "").Spec
	cacheSpec.Cache.Storage = api.CacheStorage{Driver: "s3", S3: api.CacheS3Storage{Endpoint: "https://s3.example.com", Region: "test", Bucket: "codespace"}}
	cache := &api.CodespaceComponent{ObjectMeta: metav1.ObjectMeta{Name: "cache"}}
	require.NoError(t, server.saveComponent(ctx, cache, adminComponentWrite{adminWriteIdentity: adminWriteIdentity{Name: cache.Name}, Spec: cacheSpec, S3AccessKey: "initial-access", S3SecretKey: "initial-secret"}, true))
	var cacheSecret corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cache.Name + "-cache"}, &cacheSecret))
	require.Equal(t, []byte("initial-access"), cacheSecret.Data["s3AccessKey"])
	require.NotEmpty(t, cacheSecret.Data["registryKey"])
	originalRegistryKey := append([]byte(nil), cacheSecret.Data["registryKey"]...)
	for _, component := range []*api.CodespaceComponent{gateway, cache} {
		deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: component.Name, Namespace: namespace, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(component, api.GroupVersion.WithKind("CodespaceComponent"))}}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1}}
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
	require.NotContains(t, string(encoded), string(originalHostKey))
	require.NotContains(t, string(encoded), "PRIVATE KEY")
	require.ErrorContains(t, server.checkDeletion(ctx, cache), "disable the cache")
	disabled := cache.DeepCopy()
	disabled.Spec.Cache.Enabled = false
	require.ErrorContains(t, server.checkDeletion(ctx, disabled), "active writer lease")
	expired := metav1.NewMicroTime(time.Now().Add(-time.Minute))
	cacheLease.Spec.RenewTime = &expired
	require.NoError(t, c.Update(ctx, cacheLease))
	require.NoError(t, server.checkDeletion(ctx, disabled))

	cacheSpec.DisplayName = "Updated cache"
	require.NoError(t, server.saveComponent(ctx, cache, adminComponentWrite{adminWriteIdentity: adminWriteIdentity{Name: cache.Name, UID: cache.UID, ResourceVersion: cache.ResourceVersion}, Spec: cacheSpec, S3AccessKey: "rotated-access", S3SecretKey: "rotated-secret"}, false))
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cache.Name + "-cache"}, &cacheSecret))
	require.Equal(t, originalRegistryKey, cacheSecret.Data["registryKey"])
	require.Equal(t, []byte("rotated-access"), cacheSecret.Data["s3AccessKey"])
	require.Equal(t, string(cache.UID), cacheSecret.Labels[ComponentUIDLabel])
}
