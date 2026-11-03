// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	identitypkg "gitea.dev/codespace/internal/identity"
	"gitea.dev/codespace/internal/transport"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ServerOptions struct {
	ManagerOptions
	AdminAddress     string
	AdminPublicURL   string
	AdminTokenFile   string
	AgentAddress     string
	ComponentAddress string
}

// Run starts the native controllers and listeners as one cancellation boundary.
func Run(ctx context.Context, config *rest.Config, options ServerOptions) error {
	mgr, err := NewManager(config, options.ManagerOptions)
	if err != nil {
		return err
	}
	componentTLS, err := transport.Server(mgr.Identity, func(identity *url.URL) error {
		if _, err := identitypkg.ParseComponentURI(identity); err != nil {
			return fmt.Errorf("gateway or Cache identity is required")
		}
		return nil
	})
	if err != nil {
		return err
	}
	reader, err := client.New(config, client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		return err
	}
	admin, err := NewAdminServer(reader, options.Namespace, options.AdminPublicURL, options.AdminTokenFile)
	if err != nil {
		return err
	}
	tlsConfig, err := transport.Server(mgr.Identity, func(identity *url.URL) error {
		if _, err := identitypkg.ParseAgentURI(identity); err != nil {
			return fmt.Errorf("agent identity is required")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := mgr.Add(admin); err != nil {
		return err
	}
	path, control := mgr.AgentControl.Handler()
	controlMux := http.NewServeMux()
	controlMux.Handle(path, mgr.Leadership.Handler(control))
	componentPath, componentHandler := mgr.Components.Handler()
	componentMux := http.NewServeMux()
	componentMux.Handle(componentPath, mgr.Leadership.Handler(componentIdentityHandler(componentHandler)))
	for _, server := range []*httpServer{
		{Server: &http.Server{Addr: options.AdminAddress, Handler: mgr.Leadership.Handler(admin.Handler()), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 32 * 1024}},
		{Server: &http.Server{Addr: options.AgentAddress, Handler: controlMux, TLSConfig: tlsConfig, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 32 * 1024}},
		{Server: &http.Server{Addr: options.ComponentAddress, Handler: componentMux, TLSConfig: componentTLS, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 32 * 1024}},
	} {
		if server.Addr == "" {
			return fmt.Errorf("admin, Agent and component listen addresses are required")
		}
		if err := mgr.Add(server); err != nil {
			return err
		}
	}
	return mgr.Start(ctx)
}

type httpServer struct{ *http.Server }

func (*httpServer) NeedLeaderElection() bool { return false }

func (s *httpServer) Start(ctx context.Context) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.Addr)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	s.BaseContext = func(net.Listener) context.Context { return ctx }
	drained := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(drained)
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.Shutdown(shutdown); err != nil {
			_ = s.Close()
		}
	})
	defer func() {
		if !stop() {
			<-drained
		}
	}()
	if s.TLSConfig == nil {
		err = s.Serve(listener)
	} else {
		// ServeTLS enables net/http's HTTP/2 support for bidirectional RPC streams.
		s.TLSConfig.MinVersion = tls.VersionTLS13
		err = s.ServeTLS(listener, "", "")
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
