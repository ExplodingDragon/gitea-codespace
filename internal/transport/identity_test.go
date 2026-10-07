// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestApplicationIdentityTLSAndRotation(t *testing.T) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	issue := func(directory, identity string) {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		uri, err := url.Parse(identity)
		require.NoError(t, err)
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
		require.NoError(t, err)
		leaf := &x509.Certificate{SerialNumber: serial, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
		require.NoError(t, err)
		encodedKey, err := x509.MarshalPKCS8PrivateKey(key)
		require.NoError(t, err)
		for name, value := range map[string][]byte{"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), "tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encodedKey}), "ca.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})} {
			require.NoError(t, os.WriteFile(filepath.Join(directory, name), value, 0o600))
		}
	}
	serverDirectory, clientDirectory := t.TempDir(), t.TempDir()
	serverIdentity, clientIdentity := "spiffe://codespace/agent/site/runtime/pod-1", "spiffe://codespace/gateway/component/pod"
	issue(serverDirectory, serverIdentity)
	issue(clientDirectory, clientIdentity)
	exactIdentity := func(expected string) IdentityCheck {
		return func(identity *url.URL) error {
			if identity.String() != expected {
				return fmt.Errorf("unexpected peer identity")
			}
			return nil
		}
	}
	serverTLS, err := (Certificates{Directory: serverDirectory}).Server(exactIdentity(clientIdentity))
	require.NoError(t, err)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "authorized") }))
	server.EnableHTTP2, server.TLS = true, serverTLS
	server.StartTLS()
	defer server.Close()
	clientTLS, err := (Certificates{Directory: clientDirectory}).Client(exactIdentity(serverIdentity))
	require.NoError(t, err)
	transport := &http.Transport{TLSClientConfig: clientTLS, ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, err := client.Get(server.URL)
	require.NoError(t, err)
	require.Equal(t, 2, response.ProtoMajor)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
	transport.CloseIdleConnections()
	issue(serverDirectory, "spiffe://codespace/agent/site/runtime/pod-2")
	_, err = client.Get(server.URL)
	require.ErrorContains(t, err, "unexpected peer identity")
}
