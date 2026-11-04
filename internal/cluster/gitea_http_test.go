// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitea.dev/codespace/internal/identity"
	"github.com/stretchr/testify/require"
)

func TestGiteaHTTPClientWithCustomCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(server.Close)
	bundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})

	client, err := giteaHTTPClient(nil, string(bundle))
	require.NoError(t, err)
	response, err := client.Get(server.URL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())

	_, err = giteaHTTPClient(nil, "not a certificate")
	require.ErrorContains(t, err, "outside a PEM certificate")
}

func TestGiteaHTTPClientsKeepCustomCAsIsolated(t *testing.T) {
	now := time.Now()
	firstAuthority, err := identity.NewAuthority(now)
	require.NoError(t, err)
	secondAuthority, err := identity.NewAuthority(now)
	require.NoError(t, err)
	firstClient, err := giteaHTTPClient(nil, string(firstAuthority.RootPEM()))
	require.NoError(t, err)
	secondClient, err := giteaHTTPClient(nil, string(secondAuthority.RootPEM()))
	require.NoError(t, err)

	firstBlock, _ := pem.Decode(firstAuthority.RootPEM())
	firstCertificate, err := x509.ParseCertificate(firstBlock.Bytes)
	require.NoError(t, err)
	secondBlock, _ := pem.Decode(secondAuthority.RootPEM())
	secondCertificate, err := x509.ParseCertificate(secondBlock.Bytes)
	require.NoError(t, err)

	firstRoots := firstClient.Transport.(*http.Transport).TLSClientConfig.RootCAs
	secondRoots := secondClient.Transport.(*http.Transport).TLSClientConfig.RootCAs
	_, err = firstCertificate.Verify(x509.VerifyOptions{Roots: firstRoots})
	require.NoError(t, err)
	_, err = firstCertificate.Verify(x509.VerifyOptions{Roots: secondRoots})
	require.Error(t, err)
	_, err = secondCertificate.Verify(x509.VerifyOptions{Roots: secondRoots})
	require.NoError(t, err)
	_, err = secondCertificate.Verify(x509.VerifyOptions{Roots: firstRoots})
	require.Error(t, err)
}
