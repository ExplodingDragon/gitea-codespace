// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package component

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	identitypkg "gitea.dev/codespace/internal/identity"
	"gitea.dev/codespace/internal/rpc/component/v1/componentv1connect"
	"gitea.dev/codespace/internal/transport"
)

const componentRequestTimeout = 5 * time.Second

// ManagerConnection owns one component's authenticated connection to Manager.
// Gateway also reuses Certificates for its Agent mTLS client connections.
type ManagerConnection struct {
	Service      componentv1connect.ComponentServiceClient
	Certificates *transport.FileCertificates
	httpClient   *http.Client
}

// NewManagerConnection validates the component environment and constructs the
// shared mTLS state used by the Manager client and, for Gateway, Agent clients.
func NewManagerConnection() (*ManagerConnection, error) {
	address := strings.TrimSpace(os.Getenv("GITEA_CODESPACE_MANAGER_URL"))
	endpoint, err := url.Parse(address)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return nil, errors.New("GITEA_CODESPACE_MANAGER_URL must be a Manager root URL")
	}
	if endpoint.Scheme != "https" {
		return nil, errors.New("manager component API requires HTTPS")
	}
	certificateDirectory := strings.TrimSpace(os.Getenv("GITEA_CODESPACE_CERTIFICATE_DIRECTORY"))
	if certificateDirectory == "" {
		certificateDirectory = "/var/run/codespace/identity"
	}
	managerIdentity := strings.TrimSpace(os.Getenv("GITEA_CODESPACE_MANAGER_IDENTITY"))
	if _, err := identitypkg.ParseManagerURI(managerIdentity); err != nil {
		return nil, fmt.Errorf("GITEA_CODESPACE_MANAGER_IDENTITY: %w", err)
	}
	certificates, err := transport.NewFileCertificates(certificateDirectory)
	if err != nil {
		return nil, err
	}
	tlsConfig, err := certificates.Client(func(identity *url.URL) error {
		if identity.String() != managerIdentity {
			return fmt.Errorf("manager identity does not match this Codespace installation")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	httpTransport := http.DefaultTransport.(*http.Transport).Clone()
	httpTransport.ResponseHeaderTimeout = 30 * time.Second
	httpTransport.ForceAttemptHTTP2 = true
	httpTransport.TLSClientConfig = tlsConfig
	httpClient := &http.Client{
		Transport: httpTransport,
		// Component credentials are bound to the configured Manager endpoint.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	baseURL := strings.TrimRight(address, "/")
	service := componentv1connect.NewComponentServiceClient(connect.NewClient(connecthttp.NewTransport(httpClient, baseURL, connecthttp.WithGRPC())))
	return &ManagerConnection{Service: service, Certificates: certificates, httpClient: httpClient}, nil
}

// Close releases idle Manager connections.
func (c *ManagerConnection) Close() {
	if c != nil && c.httpClient != nil {
		c.httpClient.CloseIdleConnections()
	}
}
