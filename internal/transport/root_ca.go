// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package transport

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

// ExtendDefaultRootCAs adds a PEM bundle to the process-wide HTTP transport.
// Commands call it during startup, before constructing service clients.
func ExtendDefaultRootCAs(path string) error {
	bundle, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read additional CA bundle: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return fmt.Errorf("load system CA pool: %w", err)
	}
	if !roots.AppendCertsFromPEM(bundle) {
		return fmt.Errorf("additional CA bundle contains no certificates")
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return fmt.Errorf("default HTTP transport has an unexpected type")
	}
	configured := base.Clone()
	if configured.TLSClientConfig == nil {
		configured.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		configured.TLSClientConfig = configured.TLSClientConfig.Clone()
	}
	configured.TLSClientConfig.RootCAs = roots
	http.DefaultTransport = configured
	return nil
}
