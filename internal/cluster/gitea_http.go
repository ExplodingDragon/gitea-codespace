// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"time"

	"gitea.dev/codespace/internal/certificates"
)

func giteaHTTPClient(base *http.Client, caBundle string) (*http.Client, error) {
	client := http.Client{Timeout: 15 * time.Second}
	if base != nil {
		client = *base
		if client.Timeout == 0 {
			client.Timeout = 15 * time.Second
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if caBundle == "" {
		return &client, nil
	}
	parsed, err := certificates.ParseCABundle(caBundle)
	if err != nil {
		return nil, err
	}
	var transport *http.Transport
	switch current := client.Transport.(type) {
	case nil:
		transport = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		transport = current.Clone()
	default:
		return nil, fmt.Errorf("custom CA requires an HTTP transport with TLS configuration")
	}
	rootCAs, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system CA pool: %w", err)
	}
	for _, certificate := range parsed {
		rootCAs.AddCert(certificate)
	}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	transport.TLSClientConfig.RootCAs = rootCAs
	client.Transport = transport
	return &client, nil
}
