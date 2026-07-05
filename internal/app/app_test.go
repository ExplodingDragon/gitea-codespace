// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"gitea.dev/codespace/internal/controlplane"
	"gitea.dev/codespace/internal/manager"
	"gitea.dev/codespace/internal/provisioner"
	"gitea.dev/codespace/internal/store"
)

func TestReferenceCodespaceFlow(t *testing.T) {
	t.Parallel()

	memoryStore := store.New()
	if err := memoryStore.EnsureRegistrationToken("bootstrap-local-token", "bootstrap", 24*time.Hour); err != nil {
		t.Fatalf("ensure registration token: %v", err)
	}

	config := DefaultConfig()
	config.Server.PublicBaseURL = "http://127.0.0.1:18080"
	config.Manager.Name = "test-manager"
	config.Manager.Version = "0.1.0"
	config.Manager.PollInterval = Duration(50 * time.Millisecond)
	config.Manager.PingInterval = Duration(50 * time.Millisecond)
	config.Manager.HTTPTimeout = Duration(5 * time.Second)
	controlPlaneService := controlplane.New(memoryStore)
	server := httptest.NewServer(newHTTPHandler(config, memoryStore, controlPlaneService))
	defer server.Close()

	config.Server.PublicBaseURL = server.URL
	config.Gitea.URL = server.URL
	config.Manager.GatewayURL = server.URL
	registeredManager, managerToken, err := memoryStore.RegisterManager(
		"bootstrap-local-token",
		config.Manager.Name,
		config.Manager.GatewayURL,
		config.Manager.Version,
		buildCapabilities(config),
	)
	if err != nil {
		t.Fatalf("register manager: %v", err)
	}
	config.Manager.ID = registeredManager.ID
	config.Manager.UUID = registeredManager.UUID
	config.Manager.Token = managerToken
	httpClient := &http.Client{Timeout: 5 * time.Second}
	embeddedManager := manager.New(
		manager.AgentConfig{
			BaseURL:       config.Gitea.URL,
			ManagerUUID:   config.Manager.UUID,
			ManagerToken:  config.Manager.Token,
			Name:          config.Manager.Name,
			GatewayURL:    server.URL,
			Version:       config.Manager.Version,
			PollInterval:  config.Manager.PollInterval.ToStdlib(),
			PingInterval:  config.Manager.PingInterval.ToStdlib(),
			FetchCapacity: config.Manager.FetchCapacity,
			RuntimeAPIURL: server.URL + "/api/runtime",
			Capabilities:  buildCapabilities(config),
		},
		httpClient,
		provisioner.NewDummy(),
		memoryStore,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	managerDone := make(chan error, 1)
	go func() {
		managerDone <- embeddedManager.Run(ctx)
	}()

	waitForManagerRegistration(t, memoryStore)

	createPayload := map[string]any{
		"owner":           "dragon",
		"repo_name":       "remote-demo",
		"user_id":         100,
		"repo_id":         200,
		"ref_type":        "branch",
		"ref_name":        "main",
		"instance_type":   "container",
		"image":           "images:debian/12",
		"resource_preset": "small",
	}
	createResponse := postJSON(t, server.URL+"/api/codespace", createPayload, nil)
	defer createResponse.Body.Close()
	if createResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create codespace status = %d", createResponse.StatusCode)
	}

	var createBody struct {
		Codespace struct {
			ID string `json:"id"`
		} `json:"codespace"`
	}
	if err := json.NewDecoder(createResponse.Body).Decode(&createBody); err != nil {
		t.Fatalf("decode create codespace response: %v", err)
	}
	if createBody.Codespace.ID == "" {
		t.Fatalf("codespace id is empty")
	}

	waitForCodespaceRunning(t, memoryStore, createBody.Codespace.ID)

	codespace, err := memoryStore.GetCodespace(createBody.Codespace.ID)
	if err != nil {
		t.Fatalf("get codespace from store: %v", err)
	}
	if codespace.Runtime == nil || codespace.Runtime.Token == "" {
		t.Fatalf("runtime token not prepared")
	}

	runtimeResponse := postJSON(t, server.URL+"/api/runtime/ports", map[string]any{
		"name":        "web",
		"port":        3000,
		"protocol":    "http",
		"visibility":  "private",
		"description": "dev server",
	}, map[string]string{
		"Authorization": "Bearer " + codespace.Runtime.Token,
	})
	defer runtimeResponse.Body.Close()
	if runtimeResponse.StatusCode != http.StatusCreated {
		t.Fatalf("runtime port status = %d", runtimeResponse.StatusCode)
	}

	redirectClient := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	openRequest, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/codespace/%s/open", server.URL, createBody.Codespace.ID), nil)
	if err != nil {
		t.Fatalf("new open request: %v", err)
	}
	openResponse, err := redirectClient.Do(openRequest)
	if err != nil {
		t.Fatalf("do open request: %v", err)
	}
	defer openResponse.Body.Close()
	if openResponse.StatusCode != http.StatusFound {
		t.Fatalf("open codespace status = %d", openResponse.StatusCode)
	}
	location := openResponse.Header.Get("Location")
	if location == "" {
		t.Fatalf("open codespace redirect missing location")
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("new cookie jar: %v", err)
	}
	sessionClient := &http.Client{
		Timeout: 5 * time.Second,
		Jar:     jar,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	sessionResponse, err := sessionClient.Get(location)
	if err != nil {
		t.Fatalf("follow open redirect: %v", err)
	}
	defer sessionResponse.Body.Close()
	if sessionResponse.StatusCode != http.StatusFound {
		t.Fatalf("gateway open status = %d", sessionResponse.StatusCode)
	}
	codespacePageURL := server.URL + sessionResponse.Header.Get("Location")

	codespaceResponse, err := sessionClient.Get(codespacePageURL)
	if err != nil {
		t.Fatalf("load codespace page: %v", err)
	}
	defer codespaceResponse.Body.Close()
	if codespaceResponse.StatusCode != http.StatusOK {
		t.Fatalf("codespace page status = %d", codespaceResponse.StatusCode)
	}

	previewResponse, err := sessionClient.Get(fmt.Sprintf("%s/p/%s/web/", server.URL, createBody.Codespace.ID))
	if err != nil {
		t.Fatalf("load preview page: %v", err)
	}
	defer previewResponse.Body.Close()
	if previewResponse.StatusCode != http.StatusOK {
		t.Fatalf("preview page status = %d", previewResponse.StatusCode)
	}

	cancel()
	select {
	case err := <-managerDone:
		if err != nil {
			t.Fatalf("manager returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("manager did not stop")
	}
}

func postJSON(t *testing.T, url string, payload any, headers map[string]string) *http.Response {
	t.Helper()

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal json payload: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new post request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post json request: %v", err)
	}
	return response
}

func waitForCodespaceRunning(t *testing.T, memoryStore *store.MemoryStore, codespaceID string) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		codespace, err := memoryStore.GetCodespace(codespaceID)
		if err == nil && codespace.Status.String() == "CODESPACE_STATUS_RUNNING" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("codespace %s did not reach running state", codespaceID)
}

func waitForManagerRegistration(t *testing.T, memoryStore *store.MemoryStore) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(memoryStore.ListManagers()) > 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("manager did not register")
}
