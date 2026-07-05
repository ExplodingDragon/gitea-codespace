// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	"gitea.dev/codespace/internal/controlplane"
	"gitea.dev/codespace/internal/manager"
	"gitea.dev/codespace/internal/provisioner"
	"gitea.dev/codespace/internal/store"
)

const sessionCookieName = "codespace_session"

// Run starts the reference control plane, gateway, runtime API, and embedded manager.
func Run(output io.Writer, configPath string) error {
	if output == nil {
		return fmt.Errorf("output is nil")
	}

	config, err := LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	return RunWithConfig(output, config)
}

// RunWithConfig starts the service stack with one in-memory config.
func RunWithConfig(output io.Writer, config Config) error {
	if output == nil {
		return fmt.Errorf("output is nil")
	}

	memoryStore := store.New()
	controlPlaneService := controlplane.New(memoryStore)
	handler := newHTTPHandler(config, memoryStore, controlPlaneService)
	server := &http.Server{
		Addr:              config.Server.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errorChannel := make(chan error, 2)
	go func() {
		fmt.Fprintf(output, "codespace listening on %s\n", config.Server.PublicBaseURL)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errorChannel <- fmt.Errorf("listen and serve: %w", err)
		}
	}()

	managerProvisioner, err := newProvisioner(config)
	if err != nil {
		return fmt.Errorf("create provisioner: %w", err)
	}

	embeddedManager := manager.New(
		manager.AgentConfig{
			BaseURL:       config.Gitea.URL,
			ManagerUUID:   config.Manager.UUID,
			ManagerToken:  config.Manager.Token,
			Name:          config.Manager.Name,
			GatewayURL:    config.Manager.GatewayURL,
			Version:       config.Manager.Version,
			PollInterval:  config.Manager.PollInterval.ToStdlib(),
			PingInterval:  config.Manager.PingInterval.ToStdlib(),
			FetchCapacity: config.Manager.FetchCapacity,
			RuntimeAPIURL: strings.TrimSuffix(config.Server.PublicBaseURL, "/") + "/api/runtime",
			Capabilities:  buildCapabilities(config),
		},
		&http.Client{Timeout: config.Manager.HTTPTimeout.ToStdlib()},
		managerProvisioner,
		memoryStore,
	)
	go func() {
		if err := embeddedManager.Run(ctx); err != nil {
			errorChannel <- fmt.Errorf("manager: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errorChannel:
		return err
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), config.Server.ShutdownTimeout.ToStdlib())
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		return fmt.Errorf("shutdown server: %w", err)
	}
	return nil
}

func newProvisioner(config Config) (provisioner.Provisioner, error) {
	switch strings.ToLower(strings.TrimSpace(config.Provisioner.Kind)) {
	case "dummy":
		return provisioner.NewDummy(), nil
	case "incus":
		return provisioner.NewIncus(provisioner.IncusConfig{
			Project:       config.Provisioner.Incus.Project,
			Remote:        config.Provisioner.Incus.Remote,
			UnixSocket:    config.Provisioner.Incus.UnixSocket,
			CodespaceRoot: config.Provisioner.CodespaceRoot,
			Bootstrap: provisioner.BootstrapConfig{
				Shell:   config.Provisioner.Bootstrap.Shell,
				HomeDir: config.Provisioner.Bootstrap.HomeDir,
				User:    config.Provisioner.Bootstrap.User,
				Group:   config.Provisioner.Bootstrap.Group,
			},
		})
	default:
		return nil, fmt.Errorf("unknown provisioner kind %q", config.Provisioner.Kind)
	}
}

func newHTTPHandler(config Config, memoryStore *store.MemoryStore, controlPlaneService *controlplane.Service) http.Handler {
	mux := http.NewServeMux()
	path, handler := codespacev1connect.NewCodespaceServiceHandler(controlPlaneService)
	mux.Handle(path, handler)
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/" {
			http.NotFound(writer, request)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"name":   "codespace",
			"status": "ok",
		})
	})
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("/api/admin/managers", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"managers": memoryStore.ListManagers()})
	})
	mux.HandleFunc("/api/codespace", func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			writeJSON(writer, http.StatusOK, map[string]any{"codespace": memoryStore.ListCodespace()})
		case http.MethodPost:
			var payload store.CreateCodespaceInput
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				writeError(writer, http.StatusBadRequest, fmt.Errorf("decode codespace payload: %w", err))
				return
			}
			if payload.Owner == "" {
				payload.Owner = "demo"
			}
			if payload.RepoName == "" {
				payload.RepoName = "repo"
			}
			if payload.UserID == 0 {
				payload.UserID = 100
			}
			if payload.RepoID == 0 {
				payload.RepoID = 200
			}
			codespace, task, err := memoryStore.CreateCodespace(payload)
			if err != nil {
				writeError(writer, http.StatusBadRequest, err)
				return
			}
			writeJSON(writer, http.StatusCreated, map[string]any{
				"codespace": codespace,
				"task":      task,
			})
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/codespace/", func(writer http.ResponseWriter, request *http.Request) {
		handleCodespaceRoute(memoryStore, writer, request)
	})
	mux.HandleFunc("/open", func(writer http.ResponseWriter, request *http.Request) {
		ticket := request.URL.Query().Get("ticket")
		record, err := memoryStore.ValidateAccessTicket(ticket, "open")
		if err != nil {
			writeError(writer, http.StatusUnauthorized, err)
			return
		}
		session, err := memoryStore.CreateSession(record.CodespaceID, record.UserID, record.RepoID)
		if err != nil {
			writeError(writer, http.StatusInternalServerError, err)
			return
		}
		http.SetCookie(writer, &http.Cookie{
			Name:     sessionCookieName,
			Value:    session.Token,
			HttpOnly: true,
			Path:     "/",
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(writer, request, "/w/"+record.CodespaceID+"/", http.StatusFound)
	})
	mux.HandleFunc("/w/", func(writer http.ResponseWriter, request *http.Request) {
		codespaceID := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/w/"), "/")
		session, err := requireSession(memoryStore, request, codespaceID)
		if err != nil {
			writeError(writer, http.StatusUnauthorized, err)
			return
		}
		codespace, err := memoryStore.GetCodespace(codespaceID)
		if err != nil {
			writeError(writer, http.StatusNotFound, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"codespace": codespace,
			"session":   session,
			"ssh":       sshTarget(config, codespaceID),
		})
	})
	mux.HandleFunc("/p/", func(writer http.ResponseWriter, request *http.Request) {
		pathValue := strings.TrimPrefix(request.URL.Path, "/p/")
		parts := strings.Split(strings.TrimSuffix(pathValue, "/"), "/")
		if len(parts) != 2 {
			http.NotFound(writer, request)
			return
		}
		codespaceID := parts[0]
		portName := parts[1]
		if _, err := requireSession(memoryStore, request, codespaceID); err != nil {
			writeError(writer, http.StatusUnauthorized, err)
			return
		}
		port, err := memoryStore.FindPort(codespaceID, portName)
		if err != nil {
			writeError(writer, http.StatusNotFound, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"codespace_id": codespaceID,
			"port":         port,
		})
	})
	mux.HandleFunc("/api/runtime/context", func(writer http.ResponseWriter, request *http.Request) {
		token := bearerToken(request)
		runtimeContext, _, err := memoryStore.ValidateRuntimeToken(token)
		if err != nil {
			writeError(writer, http.StatusUnauthorized, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"codespace_id": runtimeContext.CodespaceID,
			"repo":         runtimeContext.Repo,
			"ref":          runtimeContext.Ref,
			"root":         runtimeContext.Root,
			"phase":        runtimeContext.Phase,
			"message":      runtimeContext.Message,
		})
	})
	mux.HandleFunc("/api/runtime/ports", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		token := bearerToken(request)
		runtimeContext, codespace, err := memoryStore.ValidateRuntimeToken(token)
		if err != nil {
			writeError(writer, http.StatusUnauthorized, err)
			return
		}
		var payload struct {
			Name        string `json:"name"`
			Port        int32  `json:"port"`
			Protocol    string `json:"protocol"`
			Visibility  string `json:"visibility"`
			Description string `json:"description"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("decode port payload: %w", err))
			return
		}
		port, err := memoryStore.UpsertRuntimePort(token, store.Port{
			Name:        payload.Name,
			Port:        payload.Port,
			Protocol:    defaultString(payload.Protocol, "http"),
			Visibility:  parseVisibility(payload.Visibility),
			Description: payload.Description,
			PublicURL:   requestBaseURL(request) + "/p/" + runtimeContext.CodespaceID + "/" + payload.Name + "/",
			Status:      codespacev1.PortStatus_PORT_STATUS_ACTIVE,
		})
		if err != nil {
			writeError(writer, http.StatusBadRequest, err)
			return
		}
		writeJSON(writer, http.StatusCreated, map[string]any{
			"name":   port.Name,
			"status": port.Status,
			"url":    port.PublicURL,
			"repo":   codespace.RepoFullName,
		})
	})
	mux.HandleFunc("/api/runtime/ports/", func(writer http.ResponseWriter, request *http.Request) {
		token := bearerToken(request)
		name := strings.TrimPrefix(request.URL.Path, "/api/runtime/ports/")
		if name == "" {
			http.NotFound(writer, request)
			return
		}
		switch request.Method {
		case http.MethodPatch:
			var payload struct {
				Visibility  string `json:"visibility"`
				Description string `json:"description"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				writeError(writer, http.StatusBadRequest, fmt.Errorf("decode port patch payload: %w", err))
				return
			}
			port, err := memoryStore.PatchRuntimePort(token, name, parseVisibility(payload.Visibility), payload.Description)
			if err != nil {
				writeError(writer, http.StatusBadRequest, err)
				return
			}
			writeJSON(writer, http.StatusOK, port)
		case http.MethodDelete:
			if err := memoryStore.DeleteRuntimePort(token, name); err != nil {
				writeError(writer, http.StatusBadRequest, err)
				return
			}
			writer.WriteHeader(http.StatusNoContent)
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/runtime/status", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		token := bearerToken(request)
		var payload struct {
			Phase   string `json:"phase"`
			Message string `json:"message"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			writeError(writer, http.StatusBadRequest, fmt.Errorf("decode runtime status payload: %w", err))
			return
		}
		value, err := memoryStore.UpdateRuntimeStatus(token, payload.Phase, payload.Message)
		if err != nil {
			writeError(writer, http.StatusUnauthorized, err)
			return
		}
		writeJSON(writer, http.StatusOK, value)
	})
	return loggingMiddleware(mux)
}

func handleCodespaceRoute(memoryStore *store.MemoryStore, writer http.ResponseWriter, request *http.Request) {
	trimmed := strings.TrimPrefix(request.URL.Path, "/api/codespace/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(writer, request)
		return
	}
	codespaceID := parts[0]
	if len(parts) == 1 {
		if request.Method != http.MethodGet {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		codespace, err := memoryStore.GetCodespace(codespaceID)
		if err != nil {
			writeError(writer, http.StatusNotFound, err)
			return
		}
		writeJSON(writer, http.StatusOK, codespace)
		return
	}
	action := parts[1]
	switch action {
	case "open":
		if request.Method != http.MethodGet {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		userID := int64(100)
		if headerValue := request.Header.Get("X-User-ID"); headerValue != "" {
			if value, err := strconv.ParseInt(headerValue, 10, 64); err == nil {
				userID = value
			}
		}
		ticket, err := memoryStore.IssueAccessTicket(codespaceID, userID, "open")
		if err != nil {
			writeError(writer, http.StatusBadRequest, err)
			return
		}
		http.Redirect(writer, request, requestBaseURL(request)+"/open?ticket="+ticket.Token, http.StatusFound)
	case "resume":
		queueCodespaceAction(memoryStore, writer, request, codespaceID, codespacev1.OperationType_OPERATION_TYPE_RESUME)
	case "stop":
		queueCodespaceAction(memoryStore, writer, request, codespaceID, codespacev1.OperationType_OPERATION_TYPE_STOP)
	case "delete":
		queueCodespaceAction(memoryStore, writer, request, codespaceID, codespacev1.OperationType_OPERATION_TYPE_DELETE)
	default:
		http.NotFound(writer, request)
	}
}

func queueCodespaceAction(
	memoryStore *store.MemoryStore,
	writer http.ResponseWriter,
	request *http.Request,
	codespaceID string,
	taskType codespacev1.OperationType,
) {
	if request.Method != http.MethodPost {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	task, err := memoryStore.QueueCodespaceAction(codespaceID, taskType)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, task)
}

func requireSession(memoryStore *store.MemoryStore, request *http.Request, codespaceID string) (*store.Session, error) {
	cookie, err := request.Cookie(sessionCookieName)
	if err != nil {
		return nil, fmt.Errorf("read session cookie: %w", err)
	}
	session, err := memoryStore.ValidateSession(cookie.Value, codespaceID)
	if err != nil {
		return nil, fmt.Errorf("validate session: %w", err)
	}
	return session, nil
}

func bearerToken(request *http.Request) string {
	return strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
}

func parseVisibility(value string) codespacev1.PortVisibility {
	switch strings.ToLower(value) {
	case "org":
		return codespacev1.PortVisibility_PORT_VISIBILITY_ORG
	case "public":
		return codespacev1.PortVisibility_PORT_VISIBILITY_PUBLIC
	default:
		return codespacev1.PortVisibility_PORT_VISIBILITY_PRIVATE
	}
}

func writeJSON(writer http.ResponseWriter, statusCode int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(statusCode)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		log.Printf("encode json response: %v", err)
	}
}

func writeError(writer http.ResponseWriter, statusCode int, err error) {
	writeJSON(writer, statusCode, map[string]any{
		"error": err.Error(),
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		log.Printf("%s %s", request.Method, request.URL.Path)
		next.ServeHTTP(writer, request)
	})
}

func defaultString(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func requestBaseURL(request *http.Request) string {
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	}
	if forwarded := request.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = forwarded
	}
	return scheme + "://" + request.Host
}

func sshTarget(config Config, codespaceID string) string {
	sshHost := strings.TrimSpace(config.Gateway.SSHHost)
	if sshHost == "" {
		if parsedURL, err := url.Parse(config.Server.PublicBaseURL); err == nil && parsedURL.Host != "" {
			sshHost = parsedURL.Hostname()
		}
	}
	if sshHost == "" {
		sshHost = "localhost"
	}
	if config.Gateway.SSHPort > 0 && config.Gateway.SSHPort != 22 {
		return fmt.Sprintf("ssh -p %d codespace-%s@%s", config.Gateway.SSHPort, codespaceID, sshHost)
	}
	return fmt.Sprintf("ssh codespace-%s@%s", codespaceID, sshHost)
}
