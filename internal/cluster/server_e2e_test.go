// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestKubernetesE2EManagerServing(t *testing.T) {
	if os.Getenv("CODESPACE_TEST_KUBERNETES") != "1" {
		t.Skip("requires the test cluster and installed CRDs")
	}
	config, err := ctrl.GetConfig()
	require.NoError(t, err)
	c, err := client.New(config, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "manager-" + uuid.NewString()}}
	require.NoError(t, c.Create(t.Context(), namespace))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		require.NoError(t, client.IgnoreNotFound(c.Delete(ctx, namespace, client.Preconditions{UID: &namespace.UID})))
	})
	directory := t.TempDir()
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	key, err := x509.MarshalPKCS8PrivateKey(fixture.TLS.Certificates[0].PrivateKey)
	require.NoError(t, err)
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fixture.TLS.Certificates[0].Certificate[0]})
	fixture.Close()
	token := strings.Repeat("test-administrator-", 4)
	for name, data := range map[string][]byte{"tls.crt": certificate, "ca.crt": certificate, "tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), "token": []byte(token)} {
		require.NoError(t, os.WriteFile(filepath.Join(directory, name), data, 0o600))
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	options := ServerOptions{ManagerOptions: ManagerOptions{Namespace: namespace.Name, ManagerURL: "https://manager.codespace-system.svc:8443", ComponentURL: "https://manager.codespace-system.svc:8445", ComponentImage: "localhost/codespace@sha256:" + strings.Repeat("a", 64), IdentityIssuer: "test-issuer", HealthAddress: "0"}, AdminAddress: address, AdminPublicURL: "http://" + address, AdminTokenFile: filepath.Join(directory, "token"), AgentAddress: "127.0.0.1:0", ComponentAddress: "127.0.0.1:0", CertificateDirectory: directory}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, config, options) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(20 * time.Second):
			t.Error("Manager did not stop")
		}
	})
	httpClient := &http.Client{Timeout: 3 * time.Second}
	t.Cleanup(httpClient.CloseIdleConnections)
	require.Eventually(t, func() bool {
		response, err := httpClient.Get(options.AdminPublicURL + "/api/admin/session")
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode == http.StatusUnauthorized
	}, 45*time.Second, 100*time.Millisecond)
	var lease coordinationv1.Lease
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Namespace: namespace.Name, Name: "codespace-manager"}, &lease))
	require.NotNil(t, lease.Spec.HolderIdentity)
	require.NotEmpty(t, *lease.Spec.HolderIdentity)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, options.AdminPublicURL+"/api/admin/login", strings.NewReader(`{"token":"`+token+`"}`))
	require.NoError(t, err)
	request.Header.Set("Origin", options.AdminPublicURL)
	response, err := httpClient.Do(request)
	require.NoError(t, err)
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode, string(data))
	var session map[string]string
	require.NoError(t, json.Unmarshal(data, &session))
	cookies := response.Cookies()
	require.Len(t, cookies, 1)
	var sessions corev1.SecretList
	require.NoError(t, c.List(t.Context(), &sessions, client.InNamespace(namespace.Name), client.MatchingLabels{adminSessionLabel: "true"}))
	require.Len(t, sessions.Items, 1)
	spec, err := json.Marshal(api.EnvironmentTemplateSpec{Tag: "e2e", Runtime: testRuntime()})
	require.NoError(t, err)
	body, err := json.Marshal(adminWrite{Name: namespace.Name, Spec: spec})
	require.NoError(t, err)
	request, err = http.NewRequestWithContext(t.Context(), http.MethodPost, options.AdminPublicURL+"/api/admin/environments", bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Origin", options.AdminPublicURL)
	request.Header.Set("X-CSRF-Token", session["csrf"])
	request.AddCookie(cookies[0])
	response, err = httpClient.Do(request)
	require.NoError(t, err)
	data, err = io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode, string(data))
	var template api.EnvironmentTemplate
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: namespace.Name}, &template))
	t.Cleanup(func() {
		ctx, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		require.NoError(t, client.IgnoreNotFound(c.Delete(ctx, &template, client.Preconditions{UID: &template.UID})))
	})
	require.Equal(t, "e2e", template.Spec.Tag)
	require.Eventually(t, func() bool {
		return c.Get(t.Context(), client.ObjectKeyFromObject(&template), &template) == nil && len(template.Status.Conditions) != 0
	}, 10*time.Second, 100*time.Millisecond)
}
