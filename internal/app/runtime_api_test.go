// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/manager"
	"gitea.dev/codespace/internal/provisioner"
	"golang.org/x/crypto/ssh"
)

func TestRuntimeAPIEndpointCRUDRequiresRuntimeToken(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	store := NewCodespaceStateStore(stateDir)
	routes := newGatewayRouteStore()
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	token := "runtime-token"
	if err := store.SaveRuntimeCredential(codespaceUUID, token); err != nil {
		t.Fatalf("save runtime credential: %v", err)
	}
	handler := newRuntimeAPIHandler(newProcessHealth(), newRuntimeAPIService(store, routes, nil, nil))

	noAuth := httptest.NewRecorder()
	handler.ServeHTTP(noAuth, httptest.NewRequest(http.MethodGet, runtimeEndpointAPIPrefix+"app-3000", nil))
	if noAuth.Code != http.StatusUnauthorized {
		t.Fatalf("no auth status = %d body=%s", noAuth.Code, noAuth.Body.String())
	}

	create := runtimeAPIRequest(t, http.MethodPost, runtimeEndpointAPIPrefix+"app-3000", token, `{
		"label": "App 3000",
		"upstream_scheme": "http",
		"upstream_port": 3000,
		"public": true
	}`)
	create.RemoteAddr = "10.0.0.12:45678"
	createResponse := httptest.NewRecorder()
	handler.ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", createResponse.Code, createResponse.Body.String())
	}
	route, ok := routes.Get(codespaceUUID, "app-3000")
	if !ok || route.upstreamHost != "10.0.0.12:3000" || !route.public {
		t.Fatalf("route after create = %#v ok=%v", route, ok)
	}

	conflict := runtimeAPIRequest(t, http.MethodPost, runtimeEndpointAPIPrefix+"app-3000", token, `{
		"label": "Other",
		"upstream_scheme": "http",
		"upstream_port": 3000,
		"public": true
	}`)
	conflict.RemoteAddr = "10.0.0.12:45678"
	conflictResponse := httptest.NewRecorder()
	handler.ServeHTTP(conflictResponse, conflict)
	if conflictResponse.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d body=%s", conflictResponse.Code, conflictResponse.Body.String())
	}

	get := runtimeAPIRequest(t, http.MethodGet, runtimeEndpointAPIPrefix+"app-3000", token, "")
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusOK || !strings.Contains(getResponse.Body.String(), `"upstream_port":3000`) {
		t.Fatalf("get status=%d body=%s", getResponse.Code, getResponse.Body.String())
	}

	deleteRequest := runtimeAPIRequest(t, http.MethodDelete, runtimeEndpointAPIPrefix+"app-3000", token, "")
	deleteResponse := httptest.NewRecorder()
	handler.ServeHTTP(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body=%s", deleteResponse.Code, deleteResponse.Body.String())
	}
	if _, ok := routes.Get(codespaceUUID, "app-3000"); ok {
		t.Fatalf("route still exists after delete")
	}
}

func TestRuntimeAPIEndpointRejectsWorkspacePublic(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	store := NewCodespaceStateStore(stateDir)
	token := "runtime-token"
	if err := store.SaveRuntimeCredential("11111111-1111-4111-8111-111111111111", token); err != nil {
		t.Fatalf("save runtime credential: %v", err)
	}
	handler := newRuntimeAPIHandler(newProcessHealth(), newRuntimeAPIService(store, newGatewayRouteStore(), nil, nil))
	request := runtimeAPIRequest(t, http.MethodPost, runtimeEndpointAPIPrefix+"workspace", token, `{
		"label": "Workspace",
		"upstream_scheme": "http",
		"upstream_port": 8080,
		"public": true
	}`)
	request.RemoteAddr = "10.0.0.12:45678"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("workspace public status = %d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"category":"invalid_request"`) {
		t.Fatalf("workspace public body = %s", response.Body.String())
	}
}

func TestRuntimeAPIEndpointRejectsTrailingJSON(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	store := NewCodespaceStateStore(stateDir)
	token := "runtime-token"
	if err := store.SaveRuntimeCredential("11111111-1111-4111-8111-111111111111", token); err != nil {
		t.Fatalf("save runtime credential: %v", err)
	}
	handler := newRuntimeAPIHandler(newProcessHealth(), newRuntimeAPIService(store, newGatewayRouteStore(), nil, nil))
	request := runtimeAPIRequest(t, http.MethodPost, runtimeEndpointAPIPrefix+"app-3000", token, `{
		"label": "App 3000",
		"upstream_scheme": "http",
		"upstream_port": 3000,
		"public": false
	} {}`)
	request.RemoteAddr = "10.0.0.12:45678"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("trailing json status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestRuntimeAPIEndpointRejectsMissingPublic(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	store := NewCodespaceStateStore(stateDir)
	token := "runtime-token"
	if err := store.SaveRuntimeCredential("11111111-1111-4111-8111-111111111111", token); err != nil {
		t.Fatalf("save runtime credential: %v", err)
	}
	handler := newRuntimeAPIHandler(newProcessHealth(), newRuntimeAPIService(store, newGatewayRouteStore(), nil, nil))
	request := runtimeAPIRequest(t, http.MethodPost, runtimeEndpointAPIPrefix+"app-3000", token, `{
		"label": "App 3000",
		"upstream_scheme": "http",
		"upstream_port": 3000
	}`)
	request.RemoteAddr = "10.0.0.12:45678"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing public status = %d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"category":"invalid_request"`) {
		t.Fatalf("missing public body = %s", response.Body.String())
	}
}

func TestRuntimeAPIEndpointRequiresJSONContentType(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	store := NewCodespaceStateStore(stateDir)
	token := "runtime-token"
	if err := store.SaveRuntimeCredential("11111111-1111-4111-8111-111111111111", token); err != nil {
		t.Fatalf("save runtime credential: %v", err)
	}
	handler := newRuntimeAPIHandler(newProcessHealth(), newRuntimeAPIService(store, newGatewayRouteStore(), nil, nil))
	request := runtimeAPIRequest(t, http.MethodPost, runtimeEndpointAPIPrefix+"app-3000", token, `{
		"label": "App 3000",
		"upstream_scheme": "http",
		"upstream_port": 3000,
		"public": false
	}`)
	request.Header.Set("Content-Type", "text/plain")
	request.RemoteAddr = "10.0.0.12:45678"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("content type status = %d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"category":"invalid_request"`) {
		t.Fatalf("content type body = %s", response.Body.String())
	}
}

func TestRuntimeAPIEndpointRejectsInvalidRequestShape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{
			name: "unknown upstream host",
			body: `{
				"label": "App 3000",
				"upstream_scheme": "http",
				"upstream_host": "127.0.0.1",
				"upstream_port": 3000,
				"public": false
			}`,
		},
		{
			name: "invalid upstream scheme",
			body: `{
				"label": "App 3000",
				"upstream_scheme": "ftp",
				"upstream_port": 3000,
				"public": false
			}`,
		},
		{
			name: "zero upstream port",
			body: `{
				"label": "App 3000",
				"upstream_scheme": "http",
				"upstream_port": 0,
				"public": false
			}`,
		},
		{
			name: "too large upstream port",
			body: `{
				"label": "App 3000",
				"upstream_scheme": "http",
				"upstream_port": 65536,
				"public": false
			}`,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			stateDir := t.TempDir()
			store := NewCodespaceStateStore(stateDir)
			routes := newGatewayRouteStore()
			token := "runtime-token"
			if err := store.SaveRuntimeCredential("11111111-1111-4111-8111-111111111111", token); err != nil {
				t.Fatalf("save runtime credential: %v", err)
			}
			handler := newRuntimeAPIHandler(newProcessHealth(), newRuntimeAPIService(store, routes, nil, nil))
			request := runtimeAPIRequest(t, http.MethodPost, runtimeEndpointAPIPrefix+"app-3000", token, test.body)
			request.RemoteAddr = "10.0.0.12:45678"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid request status = %d body=%s", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), `"category":"invalid_request"`) {
				t.Fatalf("invalid request body = %s", response.Body.String())
			}
			if _, ok := routes.Get("11111111-1111-4111-8111-111111111111", "app-3000"); ok {
				t.Fatalf("route was stored for invalid request")
			}
		})
	}
}

func TestRuntimeAPIEndpointRejectsSourceBindingMismatch(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	store := NewCodespaceStateStore(stateDir)
	routes := newGatewayRouteStore()
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	token := "runtime-token"
	if err := store.SaveRuntimeCredential(codespaceUUID, token); err != nil {
		t.Fatalf("save runtime credential: %v", err)
	}
	resolver := runtimeSourceResolverFunc(func(ctx context.Context, sourceIP string) (provisioner.RuntimeSource, bool, error) {
		if sourceIP != "10.0.0.12" {
			t.Fatalf("source ip = %q", sourceIP)
		}
		return provisioner.RuntimeSource{CodespaceUUID: "22222222-2222-4222-8222-222222222222", InstanceName: "cs-other"}, true, nil
	})
	handler := newRuntimeAPIHandler(newProcessHealth(), newRuntimeAPIService(store, routes, nil, resolver))
	request := runtimeAPIRequest(t, http.MethodPost, runtimeEndpointAPIPrefix+"app-3000", token, `{
		"label": "App 3000",
		"upstream_scheme": "http",
		"upstream_port": 3000,
		"public": false
	}`)
	request.RemoteAddr = "10.0.0.12:45678"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("source binding status = %d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"category":"runtime_binding_mismatch"`) {
		t.Fatalf("source binding body = %s", response.Body.String())
	}
	if _, ok := routes.Get(codespaceUUID, "app-3000"); ok {
		t.Fatalf("route was stored for binding mismatch")
	}
}

func TestRuntimeAPIEndpointReportsRuntimeMetadata(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	store := NewCodespaceStateStore(stateDir)
	routes := newGatewayRouteStore()
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	token := "runtime-token"
	if err := store.SaveRuntimeCredential(codespaceUUID, token); err != nil {
		t.Fatalf("save runtime credential: %v", err)
	}
	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: 1,
		InternalSSH: manager.RuntimeMetadataInternalSSH{
			Host:               "10.0.0.12",
			Port:               2222,
			User:               "codespace",
			AuthMode:           "publickey",
			HostKeyFingerprint: "SHA256:test",
		},
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 7,
			Stage:             "ready",
			StartedUnix:       10,
			LastUpdateUnix:    11,
		},
	}); err != nil {
		t.Fatalf("save runtime metadata snapshot: %v", err)
	}
	service := &gatewayManagerService{}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	handler := newRuntimeAPIHandler(newProcessHealth(), newRuntimeAPIService(store, routes, controlPlane, nil))

	request := runtimeAPIRequest(t, http.MethodPost, runtimeEndpointAPIPrefix+"app-3000", token, `{
		"label": "App 3000",
		"upstream_scheme": "http",
		"upstream_port": 3000,
		"public": true
	}`)
	request.RemoteAddr = "10.0.0.12:45678"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", response.Code, response.Body.String())
	}
	if service.metadataRequest.GetCodespaceUuid() != codespaceUUID ||
		service.metadataRequest.GetMetadataGeneration() != 2 {
		t.Fatalf("metadata request = %#v", service.metadataRequest)
	}
	var metadata struct {
		Endpoints []struct {
			EndpointID string `json:"endpoint_id"`
			Label      string `json:"label"`
			Public     bool   `json:"public"`
		} `json:"endpoints"`
		Boot struct {
			Stage string `json:"stage"`
		} `json:"boot"`
	}
	if err := json.Unmarshal([]byte(service.metadataRequest.GetMetadataJson()), &metadata); err != nil {
		t.Fatalf("decode metadata json: %v", err)
	}
	if len(metadata.Endpoints) != 1 ||
		metadata.Endpoints[0].EndpointID != "app-3000" ||
		metadata.Endpoints[0].Label != "App 3000" ||
		!metadata.Endpoints[0].Public ||
		metadata.Boot.Stage != "ready" {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestRuntimeAPIEndpointKeepsLocalSuccessWhenMetadataReportFails(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	store := NewCodespaceStateStore(stateDir)
	routes := newGatewayRouteStore()
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	token := "runtime-token"
	if err := store.SaveRuntimeCredential(codespaceUUID, token); err != nil {
		t.Fatalf("save runtime credential: %v", err)
	}
	saveRuntimeMetadataSnapshotForTest(t, store, codespaceUUID, 1)
	service := &gatewayManagerService{metadataErr: errors.New("metadata temporarily unavailable")}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	handler := newRuntimeAPIHandler(newProcessHealth(), newRuntimeAPIService(store, routes, controlPlane, nil))

	request := runtimeAPIRequest(t, http.MethodPost, runtimeEndpointAPIPrefix+"app-3000", token, `{
		"label": "App 3000",
		"upstream_scheme": "http",
		"upstream_port": 3000,
		"public": true
	}`)
	request.RemoteAddr = "10.0.0.12:45678"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", response.Code, response.Body.String())
	}
	if service.metadataRequest == nil || service.metadataRequest.GetMetadataGeneration() != 2 {
		t.Fatalf("metadata report was not attempted with generation 2: %#v", service.metadataRequest)
	}
	route, ok := routes.Get(codespaceUUID, "app-3000")
	if !ok || route.upstreamHost != "10.0.0.12:3000" {
		t.Fatalf("route after metadata failure = %#v ok=%v", route, ok)
	}
}

func TestRuntimeAPIEndpointGenerationExhaustionDoesNotCommitPartialRoute(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	store := NewCodespaceStateStore(stateDir)
	routes := newGatewayRouteStore()
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	token := "runtime-token"
	if err := store.SaveRuntimeCredential(codespaceUUID, token); err != nil {
		t.Fatalf("save runtime credential: %v", err)
	}
	oldRoute := gatewayEndpointRoute{
		codespaceUUID:  codespaceUUID,
		endpointID:     "app-3000",
		label:          "App 3000",
		upstreamScheme: "http",
		upstreamHost:   "10.0.0.12:3000",
	}
	if err := store.SaveEndpointRoute(oldRoute); err != nil {
		t.Fatalf("save endpoint route: %v", err)
	}
	if err := routes.Put(oldRoute); err != nil {
		t.Fatalf("put route: %v", err)
	}
	saveRuntimeMetadataSnapshotForTest(t, store, codespaceUUID, math.MaxInt64)
	handler := newRuntimeAPIHandler(newProcessHealth(), newRuntimeAPIService(store, routes, nil, nil))

	request := runtimeAPIRequest(t, http.MethodPut, runtimeEndpointAPIPrefix+"app-3000", token, `{
		"label": "Updated",
		"upstream_scheme": "http",
		"upstream_port": 3001,
		"public": false
	}`)
	request.RemoteAddr = "10.0.0.12:45678"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("generation exhaustion status = %d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"category":"runtime_unavailable"`) {
		t.Fatalf("generation exhaustion body = %s", response.Body.String())
	}
	route, ok := routes.Get(codespaceUUID, "app-3000")
	if !ok || route.label != "App 3000" || route.upstreamHost != "10.0.0.12:3000" {
		t.Fatalf("route after generation exhaustion = %#v ok=%v", route, ok)
	}
	generation, metadataJSON, ok, err := store.LoadRuntimeMetadataRequest(codespaceUUID)
	if err != nil {
		t.Fatalf("load runtime metadata request: %v", err)
	}
	if !ok || generation != math.MaxInt64 {
		t.Fatalf("metadata generation = %d ok=%v", generation, ok)
	}
	if strings.Contains(metadataJSON, "Updated") || strings.Contains(metadataJSON, "3001") {
		t.Fatalf("metadata changed after generation exhaustion: %s", metadataJSON)
	}
}

func TestRuntimeAPIEndpointLimitExceeded(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	token := "runtime-token"
	if err := store.SaveRuntimeCredential(codespaceUUID, token); err != nil {
		t.Fatalf("save runtime credential: %v", err)
	}
	for i := 0; i < maxCodespaceEndpoints; i++ {
		if err := store.SaveEndpointRoute(gatewayEndpointRoute{
			codespaceUUID:  codespaceUUID,
			endpointID:     endpointIDForTest(i),
			label:          "App",
			upstreamScheme: "http",
			upstreamHost:   "10.0.0.12:3000",
		}); err != nil {
			t.Fatalf("save endpoint %d: %v", i, err)
		}
	}
	handler := newRuntimeAPIHandler(newProcessHealth(), newRuntimeAPIService(store, newGatewayRouteStore(), nil, nil))
	request := runtimeAPIRequest(t, http.MethodPost, runtimeEndpointAPIPrefix+"extra", token, `{
		"label": "Extra",
		"upstream_scheme": "http",
		"upstream_port": 3001,
		"public": false
	}`)
	request.RemoteAddr = "10.0.0.12:45678"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("limit status = %d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"category":"endpoint_limit_exceeded"`) {
		t.Fatalf("limit body = %s", response.Body.String())
	}
}

func TestRuntimeAPIEndpointPutMissingAndDeleteMissing(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	store := NewCodespaceStateStore(stateDir)
	token := "runtime-token"
	if err := store.SaveRuntimeCredential("11111111-1111-4111-8111-111111111111", token); err != nil {
		t.Fatalf("save runtime credential: %v", err)
	}
	handler := newRuntimeAPIHandler(newProcessHealth(), newRuntimeAPIService(store, newGatewayRouteStore(), nil, nil))

	put := runtimeAPIRequest(t, http.MethodPut, runtimeEndpointAPIPrefix+"app-3000", token, `{
		"label": "App 3000",
		"upstream_scheme": "http",
		"upstream_port": 3000,
		"public": false
	}`)
	put.RemoteAddr = "10.0.0.12:45678"
	putResponse := httptest.NewRecorder()
	handler.ServeHTTP(putResponse, put)
	if putResponse.Code != http.StatusNotFound {
		t.Fatalf("put missing status = %d body=%s", putResponse.Code, putResponse.Body.String())
	}
	if !strings.Contains(putResponse.Body.String(), `"category":"endpoint_not_found"`) {
		t.Fatalf("put missing body = %s", putResponse.Body.String())
	}

	deleteRequest := runtimeAPIRequest(t, http.MethodDelete, runtimeEndpointAPIPrefix+"app-3000", token, "")
	deleteResponse := httptest.NewRecorder()
	handler.ServeHTTP(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("delete missing status = %d body=%s", deleteResponse.Code, deleteResponse.Body.String())
	}
	if deleteResponse.Body.Len() != 0 {
		t.Fatalf("delete missing body = %s", deleteResponse.Body.String())
	}
}

func TestRuntimeAPIEndpointRejectsActiveStop(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	token := "runtime-token"
	if err := store.SaveRuntimeCredential(codespaceUUID, token); err != nil {
		t.Fatalf("save runtime credential: %v", err)
	}
	if err := store.SaveActiveOperation(manager.OperationSnapshot{Payload: &codespacev1.OperationPayload{
		OperationRversion: 2,
		CodespaceUuid:     codespaceUUID,
		Command: &codespacev1.OperationPayload_Stop{
			Stop: &codespacev1.StopOperationPayload{},
		},
	}}); err != nil {
		t.Fatalf("save stop operation: %v", err)
	}
	handler := newRuntimeAPIHandler(newProcessHealth(), newRuntimeAPIService(store, newGatewayRouteStore(), nil, nil))

	request := runtimeAPIRequest(t, http.MethodGet, runtimeEndpointAPIPrefix+"app-3000", token, "")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("active stop endpoint status = %d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"category":"operation_conflict"`) {
		t.Fatalf("active stop endpoint body = %s", response.Body.String())
	}
}

func TestRuntimeAPIGitSSHKeyEnsuresCodespaceKey(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	token := "runtime-token"
	if err := store.SaveRuntimeCredential(codespaceUUID, token); err != nil {
		t.Fatalf("save runtime credential: %v", err)
	}
	saveRuntimeAPICreateOperation(t, store, codespaceUUID)
	publicKey, wireKey := newTestAuthorizedKey(t)
	service := &gatewayManagerService{
		ensureGitSSHKeyResponse: &codespacev1.EnsureCodespaceGitSSHKeyResponse{
			KnownHostsLines: []string{"gitea.example.com ssh-ed25519 AAAA"},
		},
	}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	handler := newRuntimeAPIHandler(newProcessHealth(), newRuntimeAPIService(store, newGatewayRouteStore(), controlPlane, nil))

	request := runtimeAPIRequest(t, http.MethodPut, runtimeGitSSHKeyAPIPath, token, `{"public_key":`+quoteJSONString(publicKey)+`}`)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("git ssh key status = %d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "known_hosts_lines") {
		t.Fatalf("git ssh key response = %s", response.Body.String())
	}
	if service.ensureGitSSHKeyRequest.GetProtocolVersion() != 1 ||
		service.ensureGitSSHKeyRequest.GetCodespaceUuid() != codespaceUUID ||
		string(service.ensureGitSSHKeyRequest.GetPublicKey()) != string(wireKey) {
		t.Fatalf("ensure git ssh key request = %#v", service.ensureGitSSHKeyRequest)
	}
}

func TestRuntimeAPIGitSSHKeyRejectsSourceBindingMismatch(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	token := "runtime-token"
	if err := store.SaveRuntimeCredential(codespaceUUID, token); err != nil {
		t.Fatalf("save runtime credential: %v", err)
	}
	saveRuntimeAPICreateOperation(t, store, codespaceUUID)
	service := &gatewayManagerService{}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	resolver := runtimeSourceResolverFunc(func(ctx context.Context, sourceIP string) (provisioner.RuntimeSource, bool, error) {
		return provisioner.RuntimeSource{}, false, nil
	})
	handler := newRuntimeAPIHandler(newProcessHealth(), newRuntimeAPIService(store, newGatewayRouteStore(), controlPlane, resolver))

	publicKey, _ := newTestAuthorizedKey(t)
	request := runtimeAPIRequest(t, http.MethodPut, runtimeGitSSHKeyAPIPath, token, `{"public_key":`+quoteJSONString(publicKey)+`}`)
	request.RemoteAddr = "10.0.0.12:45678"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("git ssh key source binding status = %d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"category":"runtime_binding_mismatch"`) {
		t.Fatalf("git ssh key source binding body = %s", response.Body.String())
	}
	if service.ensureGitSSHKeyRequest != nil {
		t.Fatalf("ensure git ssh key was called for binding mismatch")
	}
}

func TestRuntimeAPIGitSSHKeyRejectsInvalidPublicKey(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	store := NewCodespaceStateStore(stateDir)
	token := "runtime-token"
	if err := store.SaveRuntimeCredential("11111111-1111-4111-8111-111111111111", token); err != nil {
		t.Fatalf("save runtime credential: %v", err)
	}
	saveRuntimeAPICreateOperation(t, store, "11111111-1111-4111-8111-111111111111")
	service := &gatewayManagerService{}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	handler := newRuntimeAPIHandler(newProcessHealth(), newRuntimeAPIService(store, newGatewayRouteStore(), controlPlane, nil))

	request := runtimeAPIRequest(t, http.MethodPut, runtimeGitSSHKeyAPIPath, token, `{"public_key":"not-a-public-key"}`)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid git ssh key status = %d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"category":"invalid_request"`) {
		t.Fatalf("invalid git ssh key body = %s", response.Body.String())
	}
	if service.ensureGitSSHKeyRequest != nil {
		t.Fatalf("ensure git ssh key was called for invalid key")
	}
}

func TestRuntimeAPIGitSSHKeyRequiresActiveCreateOrResume(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	store := NewCodespaceStateStore(stateDir)
	token := "runtime-token"
	if err := store.SaveRuntimeCredential("11111111-1111-4111-8111-111111111111", token); err != nil {
		t.Fatalf("save runtime credential: %v", err)
	}
	service := &gatewayManagerService{}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	handler := newRuntimeAPIHandler(newProcessHealth(), newRuntimeAPIService(store, newGatewayRouteStore(), controlPlane, nil))

	publicKey, _ := newTestAuthorizedKey(t)
	request := runtimeAPIRequest(t, http.MethodPut, runtimeGitSSHKeyAPIPath, token, `{"public_key":`+quoteJSONString(publicKey)+`}`)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("git ssh key without active operation status = %d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"category":"operation_conflict"`) {
		t.Fatalf("git ssh key without active operation body = %s", response.Body.String())
	}
	if service.ensureGitSSHKeyRequest != nil {
		t.Fatalf("ensure git ssh key was called without active operation")
	}
}

func TestRuntimeAPIGitSSHKeyRequiresJSONContentType(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	store := NewCodespaceStateStore(stateDir)
	codespaceUUID := "11111111-1111-4111-8111-111111111111"
	token := "runtime-token"
	if err := store.SaveRuntimeCredential(codespaceUUID, token); err != nil {
		t.Fatalf("save runtime credential: %v", err)
	}
	saveRuntimeAPICreateOperation(t, store, codespaceUUID)
	service := &gatewayManagerService{}
	controlPlane, closeServer := newTestGatewayControlPlane(t, service)
	defer closeServer()
	handler := newRuntimeAPIHandler(newProcessHealth(), newRuntimeAPIService(store, newGatewayRouteStore(), controlPlane, nil))

	publicKey, _ := newTestAuthorizedKey(t)
	request := runtimeAPIRequest(t, http.MethodPut, runtimeGitSSHKeyAPIPath, token, `{"public_key":`+quoteJSONString(publicKey)+`}`)
	request.Header.Del("Content-Type")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("git ssh key content type status = %d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"category":"invalid_request"`) {
		t.Fatalf("git ssh key content type body = %s", response.Body.String())
	}
	if service.ensureGitSSHKeyRequest != nil {
		t.Fatalf("ensure git ssh key was called without JSON content type")
	}
}

func runtimeAPIRequest(t *testing.T, method, target, token, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

func saveRuntimeAPICreateOperation(t *testing.T, store *CodespaceStateStore, codespaceUUID string) {
	t.Helper()

	if err := store.SaveActiveOperation(manager.OperationSnapshot{Payload: &codespacev1.OperationPayload{
		OperationRversion: 1,
		CodespaceUuid:     codespaceUUID,
		Command: &codespacev1.OperationPayload_Create{
			Create: &codespacev1.CreateOperationPayload{},
		},
	}}); err != nil {
		t.Fatalf("save active operation: %v", err)
	}
}

func saveRuntimeMetadataSnapshotForTest(t *testing.T, store *CodespaceStateStore, codespaceUUID string, generation int64) {
	t.Helper()

	if err := store.SaveRuntimeMetadataSnapshot(manager.RuntimeMetadataSnapshot{
		CodespaceUUID:      codespaceUUID,
		MetadataGeneration: generation,
		InternalSSH: manager.RuntimeMetadataInternalSSH{
			Host:               "10.0.0.12",
			Port:               2222,
			User:               "codespace",
			AuthMode:           "publickey",
			HostKeyFingerprint: "SHA256:test",
		},
		Boot: manager.RuntimeMetadataBoot{
			OperationRVersion: 7,
			Stage:             "ready",
			StartedUnix:       10,
			LastUpdateUnix:    11,
		},
	}); err != nil {
		t.Fatalf("save runtime metadata snapshot: %v", err)
	}
}

func newTestAuthorizedKey(t *testing.T) (string, []byte) {
	t.Helper()

	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ssh key: %v", err)
	}
	sshPublicKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatalf("create ssh public key: %v", err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPublicKey))), sshPublicKey.Marshal()
}

func quoteJSONString(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}

type runtimeSourceResolverFunc func(context.Context, string) (provisioner.RuntimeSource, bool, error)

func (f runtimeSourceResolverFunc) ResolveRuntimeSource(
	ctx context.Context,
	sourceIP string,
) (provisioner.RuntimeSource, bool, error) {
	return f(ctx, sourceIP)
}
