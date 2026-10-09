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

	"connectrpc.com/connect"
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
	tlsConfig, err := (transport.Certificates{Directory: certificateDirectory}).Client(func(identity *url.URL) error {
		parts := strings.Split(strings.TrimPrefix(identity.Path, "/"), "/")
		if len(parts) != 2 || parts[0] != "manager" || parts[1] == "" {
			return fmt.Errorf("manager identity is invalid")
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
	client := componentv1connect.NewComponentServiceClient(httpClient, baseURL, connect.WithGRPC())
	return client, httpClient, nil
}
