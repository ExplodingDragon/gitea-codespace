// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"gitea.dev/codespace-proto-go/agent/v1/agentv1connect"
	agentpkg "gitea.dev/codespace/internal/agent"
	"gitea.dev/codespace/internal/devcontainerruntime"
	"gitea.dev/codespace/internal/runtimeendpoint"
	"gitea.dev/codespace/internal/transport"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

// NewCommand creates the internal Runtime Pod Agent command.
func NewCommand() *cobra.Command {
	var worker bool
	command := &cobra.Command{
		Use:    "agent",
		Short:  "Run the Codespace Runtime Pod agent",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(command *cobra.Command, _ []string) error {
			if os.Getpid() == 1 && !worker {
				return agentpkg.Supervise(command.Context(), os.Args[0], []string{"agent", "--worker"}, os.Stdin, os.Stdout, os.Stderr)
			}
			if !worker {
				return fmt.Errorf("agent worker must be started by the PID 1 supervisor")
			}
			return runWorker(command.Context(), command.ErrOrStderr())
		},
	}
	command.Flags().BoolVar(&worker, "worker", false, "run the supervised Agent worker")
	_ = command.Flags().MarkHidden("worker")
	return command
}

func runWorker(parent context.Context, diagnostics io.Writer) (returnErr error) {
	managerURL := strings.TrimSpace(os.Getenv("CODESPACE_MANAGER_URL"))
	endpoint, err := url.Parse(managerURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return fmt.Errorf("CODESPACE_MANAGER_URL must be an HTTPS origin")
	}
	identity := agentpkg.VolumeIdentity{
		SiteUID: strings.TrimSpace(os.Getenv("CODESPACE_SITE_UID")), ResourceUID: strings.TrimSpace(os.Getenv("CODESPACE_RESOURCE_UID")), RuntimeUUID: strings.TrimSpace(os.Getenv("CODESPACE_RUNTIME_UUID")),
	}
	podUID := strings.TrimSpace(os.Getenv("CODESPACE_POD_UID"))
	if podUID == "" {
		return fmt.Errorf("CODESPACE_POD_UID is required")
	}
	var devContainer devcontainerruntime.Configuration
	if err := json.Unmarshal([]byte(os.Getenv("CODESPACE_DEVCONTAINER_CONFIGURATION")), &devContainer); err != nil {
		return fmt.Errorf("decode CODESPACE_DEVCONTAINER_CONFIGURATION: %w", err)
	}
	if err := devContainer.Validate(); err != nil {
		return fmt.Errorf("CODESPACE_DEVCONTAINER_CONFIGURATION: %w", err)
	}
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if device := strings.TrimSpace(os.Getenv("CODESPACE_DATA_DEVICE")); device != "" {
		unmount, err := agentpkg.MountDataVolume(ctx, device, "/var/lib/codespace")
		if err != nil {
			return err
		}
		defer func() { returnErr = errors.Join(returnErr, unmount()) }()
	}
	journal, err := agentpkg.OpenJournal("/var/lib/codespace", identity)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, journal.Close()) }()
	if err := os.MkdirAll(filepath.Dir(strings.TrimPrefix(agentpkg.DockerHost, "unix://")), 0o700); err != nil {
		return err
	}
	if err := os.Setenv("DOCKER_HOST", agentpkg.DockerHost); err != nil {
		return err
	}
	tlsConfig, err := (transport.Certificates{Directory: "/run/identity"}).Client(func(identity *url.URL) error {
		parts := strings.Split(strings.TrimPrefix(identity.Path, "/"), "/")
		if len(parts) != 2 || parts[0] != "manager" || parts[1] == "" {
			return fmt.Errorf("manager identity is invalid")
		}
		return nil
	})
	if err != nil {
		return err
	}
	httpClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 10 * time.Second,
	}}
	defer httpClient.CloseIdleConnections()
	remote := agentv1connect.NewAgentControlServiceClient(httpClient, strings.TrimRight(managerURL, "/"), connect.WithGRPC())
	runtime := &agentpkg.Runtime{Journal: journal, PodUID: podUID, DockerDirectory: "/var/lib/codespace", Diagnostics: diagnostics, DevContainer: devContainer}
	defer func() { returnErr = errors.Join(returnErr, runtime.Close()) }()
	endpointListener, endpointServer, err := prepareEndpointServer(ctx, runtime)
	if err != nil {
		return err
	}
	defer func() {
		_ = endpointListener.Close()
		_ = os.Remove(runtimeendpoint.AgentSocketPath)
	}()
	sampler := &agentpkg.ResourceSampler{}
	client := &agentpkg.ControlClient{Journal: journal, Remote: remote, Execute: runtime.Execute, Sample: sampler.Sample, TargetChanges: runtime.TargetChanges(), CurrentTarget: runtime.CurrentAccessTarget}
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error { return runAccessServer(groupCtx, runtime) })
	group.Go(func() error { return serveEndpointServer(groupCtx, endpointListener, endpointServer) })
	group.Go(func() error { return client.Run(groupCtx) })
	err = group.Wait()
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return nil
	}
	return err
}

func prepareEndpointServer(ctx context.Context, runtime *agentpkg.Runtime) (net.Listener, *http.Server, error) {
	if err := os.MkdirAll(filepath.Dir(runtimeendpoint.AgentSocketPath), 0o700); err != nil {
		return nil, nil, err
	}
	if info, err := os.Lstat(runtimeendpoint.AgentSocketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, nil, fmt.Errorf("runtime endpoint socket path is occupied by a non-socket")
		}
		if err := os.Remove(runtimeendpoint.AgentSocketPath); err != nil {
			return nil, nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", runtimeendpoint.AgentSocketPath)
	if err != nil {
		return nil, nil, err
	}
	if err := os.Chmod(runtimeendpoint.AgentSocketPath, 0o666); err != nil {
		_ = listener.Close()
		_ = os.Remove(runtimeendpoint.AgentSocketPath)
		return nil, nil, err
	}
	path, handler := agentv1connect.NewRuntimeEndpointServiceHandler(&agentpkg.RuntimeEndpointServer{Runtime: runtime})
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 16 * 1024}
	return listener, server, nil
}

func serveEndpointServer(ctx context.Context, listener net.Listener, server *http.Server) error {
	server.BaseContext = func(net.Listener) context.Context { return ctx }
	stop := context.AfterFunc(ctx, func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	})
	defer stop()
	err := server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func runAccessServer(ctx context.Context, runtime *agentpkg.Runtime) error {
	tlsConfig, err := (transport.Certificates{Directory: "/run/identity"}).Server(func(identity *url.URL) error {
		parts := strings.Split(strings.TrimPrefix(identity.Path, "/"), "/")
		if len(parts) != 3 || parts[0] != "gateway" || parts[1] == "" || parts[2] == "" {
			return fmt.Errorf("gateway identity is invalid")
		}
		return nil
	})
	if err != nil {
		return err
	}
	path, handler := agentpkg.NewAccessServer(runtime).Handler()
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := &http.Server{Handler: mux, TLSConfig: tlsConfig, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 5 * time.Minute, MaxHeaderBytes: 32 * 1024}
	address := strings.TrimSpace(os.Getenv("CODESPACE_AGENT_ACCESS_ADDRESS"))
	if address == "" {
		address = ":8444"
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	server.BaseContext = func(net.Listener) context.Context { return ctx }
	stop := context.AfterFunc(ctx, func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	})
	defer stop()
	err = server.ServeTLS(listener, "", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
