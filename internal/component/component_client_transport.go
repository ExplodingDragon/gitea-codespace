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

const ComponentRequestTimeout = 5 * time.Second

func NewComponentRPCClient() (componentv1connect.ComponentServiceClient, *http.Client, error) {
	address := strings.TrimSpace(os.Getenv("GITEA_CODESPACE_MANAGER_URL"))
	endpoint, err := url.Parse(address)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return nil, nil, errors.New("GITEA_CODESPACE_MANAGER_URL must be a Manager root URL")
	}
	if endpoint.Scheme != "https" {
		return nil, nil, errors.New("manager component API requires HTTPS")
	}
	certificateDirectory := strings.TrimSpace(os.Getenv("GITEA_CODESPACE_CERTIFICATE_DIRECTORY"))
	if certificateDirectory == "" {
		certificateDirectory = "/var/run/codespace/identity"
	}
	managerIdentity := strings.TrimSpace(os.Getenv("GITEA_CODESPACE_MANAGER_IDENTITY"))
	if _, err := identitypkg.ParseManagerURI(managerIdentity); err != nil {
		return nil, nil, fmt.Errorf("GITEA_CODESPACE_MANAGER_IDENTITY: %w", err)
	}
	tlsConfig, err := (transport.Certificates{Directory: certificateDirectory}).Client(func(identity *url.URL) error {
		if identity.String() != managerIdentity {
			return fmt.Errorf("manager identity does not match this Codespace installation")
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.ForceAttemptHTTP2 = true
	transport.TLSClientConfig = tlsConfig
	httpClient := &http.Client{
		Transport: transport,
		// Component credentials are bound to the configured Manager endpoint.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	baseURL := strings.TrimRight(address, "/")
	client := componentv1connect.NewComponentServiceClient(connect.NewClient(connecthttp.NewTransport(httpClient, baseURL, connecthttp.WithGRPC())))
	return client, httpClient, nil
}
