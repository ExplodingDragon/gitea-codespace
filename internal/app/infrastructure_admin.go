// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/internal/controlplane"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"
)

//go:embed templates/manager_admin.tmpl
var managerAdminTemplateSource string

var managerAdminTemplate = template.Must(template.New("manager-admin").Parse(managerAdminTemplateSource))

type managerAdminPageData struct {
	Sites   []AdminSite
	Message string
	Error   string
	Config  string
}

func verifyManagerIdentity(ctx context.Context, opts UpsertAdminSiteOptions) error {
	giteaURL, err := normalizeGiteaURL(opts.GiteaURL)
	if err != nil {
		return err
	}
	if opts.ManagerID <= 0 || strings.TrimSpace(opts.ManagerSecret) == "" {
		return fmt.Errorf("manager identity is incomplete")
	}
	client := controlplane.NewManagerServiceClient(
		&http.Client{Timeout: 15 * time.Second},
		managerServiceBaseURL(giteaURL),
		opts.ManagerID,
		strings.TrimSpace(opts.ManagerSecret),
		0,
	)
	response, err := client.CheckManager(ctx, connect.NewRequest(&codespacev1.CheckManagerRequest{
		ProtocolVersion: controlplane.ProtocolVersion,
	}))
	if err != nil {
		return fmt.Errorf("verify manager identity: %w", err)
	}
	verifiedURL, err := normalizeGiteaURL(response.Msg.GetGiteaWebUrl())
	if err != nil {
		return fmt.Errorf("verify manager identity response: %w", err)
	}
	if verifiedURL != giteaURL {
		return fmt.Errorf("manager identity belongs to %s", verifiedURL)
	}
	return nil
}

func resolveAdminSiteSecret(ctx context.Context, store managerInfrastructureStore, opts UpsertAdminSiteOptions) (UpsertAdminSiteOptions, error) {
	if strings.TrimSpace(opts.ManagerSecret) != "" || opts.ID <= 0 {
		return opts, nil
	}
	site, err := store.LoadSite(ctx, opts.ID)
	if err != nil {
		return UpsertAdminSiteOptions{}, err
	}
	opts.ManagerSecret = site.ManagerSecret
	return opts, nil
}

func writeManagerAdminPage(ctx context.Context, writer http.ResponseWriter, store managerInfrastructureStore, message, errorMessage string) {
	sites, err := store.ListSites(ctx)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	config, err := store.LoadConfigOnly(ctx)
	if err != nil {
		if !errors.Is(err, errInfrastructureStateEmpty) {
			errorMessage = "Stored runtime configuration could not be loaded. Review and save the replacement configuration."
		}
		config = DefaultConfig()
	}
	configYAML, err := yaml.Marshal(config)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	if err := managerAdminTemplate.Execute(writer, managerAdminPageData{Sites: sites, Message: message, Error: errorMessage, Config: string(configYAML)}); err != nil {
		return
	}
}

func parseAdminRuntimeConfig(request *http.Request) (Config, error) {
	if err := request.ParseForm(); err != nil {
		return Config{}, fmt.Errorf("parse runtime configuration form: %w", err)
	}
	return decodeAdminRuntimeConfig(strings.NewReader(request.FormValue("config")))
}

func decodeAdminRuntimeConfig(reader io.Reader) (Config, error) {
	config := DefaultConfig()
	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode runtime configuration: %w", err)
	}
	config.applyDefaults()
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func authorizeInfrastructureAdminBrowser(writer http.ResponseWriter, request *http.Request, token string) bool {
	_, password, ok := request.BasicAuth()
	if !ok || subtle.ConstantTimeCompare([]byte(password), []byte(token)) != 1 {
		writer.Header().Set("WWW-Authenticate", `Basic realm="Gitea Codespace Manager"`)
		http.Error(writer, "authentication required", http.StatusUnauthorized)
		return false
	}
	return true
}

func validateAdminBrowserMutation(request *http.Request) error {
	if request.Method == http.MethodGet || request.Method == http.MethodHead {
		return nil
	}
	origin := strings.TrimSpace(request.Header.Get("Origin"))
	if origin == "" {
		return fmt.Errorf("origin is required")
	}
	parsed, err := url.Parse(origin)
	if err != nil || !strings.EqualFold(parsed.Host, request.Host) {
		return fmt.Errorf("origin does not match the administration endpoint")
	}
	return nil
}

func parseAdminSiteForm(request *http.Request) (UpsertAdminSiteOptions, error) {
	if err := request.ParseForm(); err != nil {
		return UpsertAdminSiteOptions{}, fmt.Errorf("parse site form: %w", err)
	}
	var opts UpsertAdminSiteOptions
	var err error
	if value := strings.TrimSpace(request.FormValue("id")); value != "" {
		opts.ID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || opts.ID <= 0 {
			return UpsertAdminSiteOptions{}, fmt.Errorf("site id must be a positive integer")
		}
	}
	opts.GiteaURL = request.FormValue("gitea_url")
	opts.ManagerID, err = strconv.ParseInt(request.FormValue("manager_id"), 10, 64)
	if err != nil || opts.ManagerID <= 0 {
		return UpsertAdminSiteOptions{}, fmt.Errorf("manager id must be a positive integer")
	}
	opts.ManagerSecret = request.FormValue("manager_secret")
	opts.Enabled = request.FormValue("enabled") == "on"
	return opts, nil
}

func validateAdminListen(value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parsed, err := url.Parse("http://" + value)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("%s must be a host:port listener address", managerAdminListenEnv)
	}
	return nil
}

// runService owns the store until both the management server and workers stop.
func runService(ctx context.Context, output io.Writer) (resultErr error) {
	listen := strings.TrimSpace(os.Getenv(managerAdminListenEnv))
	if listen == "" {
		listen = "127.0.0.1:18080"
	}
	if err := validateAdminListen(listen); err != nil {
		return err
	}
	token := strings.TrimSpace(os.Getenv(managerAdminTokenEnv))
	if token == "" {
		return fmt.Errorf("%s is required", managerAdminTokenEnv)
	}
	store, err := openInfrastructureStore(strings.ToLower(strings.TrimSpace(os.Getenv(managerStateDriverEnv))))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen on manager administration address: %w", err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{
		Handler:           newInfrastructureAdminHandler(store, token),
		ReadHeaderTimeout: gatewayHTTPReadHeaderTime,
		MaxHeaderBytes:    gatewayHTTPMaxHeaderBytes,
		WriteTimeout:      5 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	adminDone := make(chan error, 1)
	go func() { adminDone <- server.Serve(listener) }()
	_, _ = fmt.Fprintf(output, "codespace manager admin listening on %s\n", listener.Addr())
	var config InfrastructureRuntimeConfig
	var loadErr error
	for {
		loadCtx, loadCancel := context.WithTimeout(ctx, 15*time.Second)
		config, loadErr = store.LoadRuntimeConfig(loadCtx)
		loadCancel()
		code := status.Code(loadErr)
		if ctx.Err() != nil || (!errors.Is(loadErr, context.DeadlineExceeded) && code != codes.Unavailable && code != codes.DeadlineExceeded) {
			break
		}
		log.Printf("waiting for deployment state: %v", loadErr)
		if waitElectionRetry(ctx) != nil {
			break
		}
	}
	var workerDone chan error
	if loadErr != nil {
		log.Printf("manager runtime is not configured or needs repair: %v; update administration settings and restart", loadErr)
	} else {
		config.store = store
		workerDone = make(chan error, 1)
		go func() { workerDone <- runManagerRuntime(ctx, output, config) }()
	}
	var etcdStopped <-chan struct{}
	if store.embedded != nil {
		etcdStopped = store.embedded.Server.StopNotify()
	}
	select {
	case <-ctx.Done():
	case <-etcdStopped:
		err = fmt.Errorf("embedded etcd stopped unexpectedly")
	case err = <-adminDone:
		adminDone = nil
	case err = <-workerDone:
		workerDone = nil
	}
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
		err = errors.Join(err, shutdownErr, server.Close())
	}
	if adminDone != nil {
		<-adminDone
	}
	if workerDone != nil {
		err = errors.Join(err, <-workerDone)
	}
	if err == http.ErrServerClosed || err == context.Canceled {
		return nil
	}
	return err
}

func newInfrastructureAdminHandler(store managerInfrastructureStore, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/" {
			http.NotFound(writer, request)
			return
		}
		if !authorizeInfrastructureAdminBrowser(writer, request, token) {
			return
		}
		if request.Method != http.MethodGet {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		message := ""
		switch {
		case request.URL.Query().Has("saved"):
			message = "Site saved. Restart the Manager process to apply this change."
		case request.URL.Query().Has("config-saved"):
			message = "Configuration saved. Restart the Manager process to apply this change."
		case request.URL.Query().Has("verified"):
			message = "Manager identity verified."
		case request.URL.Query().Has("deleted"):
			message = "Site deleted. Restart the Manager process to apply this change."
		}
		writeManagerAdminPage(request.Context(), writer, store, message, "")
	})
	mux.HandleFunc("/sites", func(writer http.ResponseWriter, request *http.Request) {
		if !authorizeInfrastructureAdminBrowser(writer, request, token) {
			return
		}
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := validateAdminBrowserMutation(request); err != nil {
			writeManagerAdminPage(request.Context(), writer, store, "", err.Error())
			return
		}
		opts, err := parseAdminSiteForm(request)
		if err == nil {
			opts, err = resolveAdminSiteSecret(request.Context(), store, opts)
		}
		if err == nil && opts.Enabled {
			err = verifyManagerIdentity(request.Context(), opts)
		}
		if err == nil {
			_, err = store.UpsertSite(request.Context(), opts)
		}
		if err != nil {
			writeManagerAdminPage(request.Context(), writer, store, "", err.Error())
			return
		}
		http.Redirect(writer, request, "/?saved=1", http.StatusSeeOther)
	})
	mux.HandleFunc("/config", func(writer http.ResponseWriter, request *http.Request) {
		if !authorizeInfrastructureAdminBrowser(writer, request, token) {
			return
		}
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := validateAdminBrowserMutation(request); err != nil {
			writeManagerAdminPage(request.Context(), writer, store, "", err.Error())
			return
		}
		config, err := parseAdminRuntimeConfig(request)
		if err == nil {
			err = store.SaveConfigOnly(request.Context(), config)
		}
		if err != nil {
			writeManagerAdminPage(request.Context(), writer, store, "", err.Error())
			return
		}
		http.Redirect(writer, request, "/?config-saved=1", http.StatusSeeOther)
	})
	mux.HandleFunc("/sites/", func(writer http.ResponseWriter, request *http.Request) {
		if !authorizeInfrastructureAdminBrowser(writer, request, token) {
			return
		}
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := validateAdminBrowserMutation(request); err != nil {
			writeManagerAdminPage(request.Context(), writer, store, "", err.Error())
			return
		}
		parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/sites/"), "/")
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || id <= 0 || len(parts) != 2 {
			writeManagerAdminPage(request.Context(), writer, store, "", "invalid site action")
			return
		}
		switch parts[1] {
		case "verify":
			site, loadErr := store.LoadSite(request.Context(), id)
			if loadErr == nil {
				loadErr = verifyManagerIdentity(request.Context(), UpsertAdminSiteOptions{
					GiteaURL: site.GiteaURL, ManagerID: site.ManagerID, ManagerSecret: site.ManagerSecret,
				})
			}
			if loadErr != nil {
				writeManagerAdminPage(request.Context(), writer, store, "", loadErr.Error())
				return
			}
			http.Redirect(writer, request, "/?verified=1", http.StatusSeeOther)
		case "delete":
			if err := store.DeleteSite(request.Context(), id); err != nil {
				writeManagerAdminPage(request.Context(), writer, store, "", err.Error())
				return
			}
			http.Redirect(writer, request, "/?deleted=1", http.StatusSeeOther)
		default:
			http.NotFound(writer, request)
		}
	})
	mux.HandleFunc("GET /api/state/snapshot", func(writer http.ResponseWriter, request *http.Request) {
		if !authorizeInfrastructureAdmin(writer, request, token) {
			return
		}
		// External clusters may contain unrelated namespaces; their operator owns backups.
		embedded, ok := store.(*etcdInfrastructureStore)
		if !ok || embedded.embedded == nil {
			http.Error(writer, "external etcd backups are managed by the cluster operator", http.StatusBadRequest)
			return
		}
		snapshot := embedded.embedded.Server.Backend().Snapshot()
		defer snapshot.Close()
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Header().Set("Content-Disposition", "attachment; filename=manager-etcd.snapshot")
		writer.Header().Set("Cache-Control", "no-store")
		// Match etcd's snapshot format: backend pages followed by their SHA256 digest.
		digest := sha256.New()
		if _, err := snapshot.WriteTo(io.MultiWriter(writer, digest)); err != nil {
			log.Printf("export manager etcd snapshot: %v", err)
			panic(http.ErrAbortHandler)
		}
		if _, err := writer.Write(digest.Sum(nil)); err != nil {
			panic(http.ErrAbortHandler)
		}
	})
	mux.HandleFunc("/api/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{"status": "pass"})
	})
	mux.HandleFunc("/api/sites", func(writer http.ResponseWriter, request *http.Request) {
		if !authorizeInfrastructureAdmin(writer, request, token) {
			return
		}
		switch request.Method {
		case http.MethodGet:
			sites, err := store.ListSites(request.Context())
			if err != nil {
				writeAdminError(writer, err)
				return
			}
			writeJSON(writer, http.StatusOK, map[string]any{"sites": sites})
		case http.MethodPost:
			var opts UpsertAdminSiteOptions
			decoder := json.NewDecoder(request.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&opts); err != nil {
				writeAdminError(writer, fmt.Errorf("decode site request: %w", err))
				return
			}
			opts, err := resolveAdminSiteSecret(request.Context(), store, opts)
			if err == nil && opts.Enabled {
				err = verifyManagerIdentity(request.Context(), opts)
			}
			var id int64
			if err == nil {
				id, err = store.UpsertSite(request.Context(), opts)
			}
			if err != nil {
				writeAdminError(writer, err)
				return
			}
			writeJSON(writer, http.StatusOK, map[string]any{"id": id})
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/sites/", func(writer http.ResponseWriter, request *http.Request) {
		if !authorizeInfrastructureAdmin(writer, request, token) {
			return
		}
		if request.Method != http.MethodDelete {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(request.URL.Path, "/api/sites/"), 10, 64)
		if err != nil {
			writeAdminError(writer, fmt.Errorf("site id must be a positive integer"))
			return
		}
		if err := store.DeleteSite(request.Context(), id); err != nil {
			writeAdminError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"status": "deleted"})
	})
	mux.HandleFunc("/api/config", func(writer http.ResponseWriter, request *http.Request) {
		if !authorizeInfrastructureAdmin(writer, request, token) {
			return
		}
		switch request.Method {
		case http.MethodGet:
			config, err := store.LoadConfigOnly(request.Context())
			if err != nil {
				writeAdminError(writer, err)
				return
			}
			data, err := yaml.Marshal(config)
			if err != nil {
				writeAdminError(writer, err)
				return
			}
			writer.Header().Set("Content-Type", "application/yaml")
			writer.Header().Set("Cache-Control", "no-store")
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(data)
		case http.MethodPut:
			config, err := decodeAdminRuntimeConfig(request.Body)
			if err != nil {
				writeAdminError(writer, err)
				return
			}
			if err := store.SaveConfigOnly(request.Context(), config); err != nil {
				writeAdminError(writer, err)
				return
			}
			writeJSON(writer, http.StatusOK, map[string]any{"status": "saved"})
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	return loggingMiddleware(mux)
}

func authorizeInfrastructureAdmin(writer http.ResponseWriter, request *http.Request, token string) bool {
	provided := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
		writeJSON(writer, http.StatusUnauthorized, map[string]any{"error": "admin token is required"})
		return false
	}
	return true
}

func writeAdminError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, errInfrastructureStateEmpty) {
		status = http.StatusNotFound
	}
	writeJSON(writer, status, map[string]any{"error": err.Error()})
}
