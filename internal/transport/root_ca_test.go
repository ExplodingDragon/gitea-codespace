// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package transport

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtendDefaultRootCAs(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) { response.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "proxy-ca.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600))
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	require.NoError(t, ExtendDefaultRootCAs(path))
	response, err := http.Get(server.URL)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
}
