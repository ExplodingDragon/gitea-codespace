// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	identitypkg "gitea.dev/codespace/internal/identity"
	"github.com/stretchr/testify/require"
)

func TestServerCertificateServesAndReloadsServiceIdentity(t *testing.T) {
	const serviceDNSName = "cache.codespace-system.svc"
	startedAt := time.Now()
	authority, err := identitypkg.NewAuthority(startedAt)
	require.NoError(t, err)
	var currentTime atomic.Int64
	currentTime.Store(startedAt.UnixNano())
	directory := t.TempDir()
	writeCertificate := func() {
		leaf, err := authority.IssueServerCertificate(serviceDNSName, time.Now())
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(directory, "tls.crt"), leaf.Certificate, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(directory, "tls.key"), leaf.PrivateKey, 0o600))
	}
	writeCertificate()
	tlsConfig, err := serverCertificate(directory, func() time.Time { return time.Unix(0, currentTime.Load()) })
	require.NoError(t, err)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ready") }))
	server.TLS = tlsConfig
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(authority.RootPEM()))
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: serviceDNSName}, DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	request := func() string {
		response, err := client.Get(server.URL)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.NotEmpty(t, response.TLS.PeerCertificates)
		return response.TLS.PeerCertificates[0].SerialNumber.String()
	}
	firstSerial := request()
	writeCertificate()
	transport.CloseIdleConnections()
	require.Equal(t, firstSerial, request())
	currentTime.Add(int64(certificateRefreshInterval))
	transport.CloseIdleConnections()
	require.NotEqual(t, firstSerial, request())
}

func TestCertificateCacheCoalescesRefresh(t *testing.T) {
	startedAt := time.Now()
	var currentTime atomic.Int64
	currentTime.Store(startedAt.UnixNano())
	var loads atomic.Int32
	load := func() (*certificateSnapshot, error) {
		loads.Add(1)
		return &certificateSnapshot{certificate: tls.Certificate{Leaf: &x509.Certificate{NotBefore: startedAt.Add(-time.Hour), NotAfter: startedAt.Add(time.Hour)}}}, nil
	}
	cache, err := newCertificateCache(func() time.Time { return time.Unix(0, currentTime.Load()) }, load)
	require.NoError(t, err)
	for range 10 {
		_, err := cache.snapshot()
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, loads.Load())

	currentTime.Add(int64(certificateRefreshInterval))
	var wait sync.WaitGroup
	errors := make(chan error, 32)
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := cache.snapshot()
			errors <- err
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.EqualValues(t, 2, loads.Load())
}

func TestCertificateCacheRetainsValidSnapshotAfterRefreshFailure(t *testing.T) {
	startedAt := time.Now()
	var currentTime atomic.Int64
	currentTime.Store(startedAt.UnixNano())
	var loads atomic.Int32
	cache, err := newCertificateCache(func() time.Time { return time.Unix(0, currentTime.Load()) }, func() (*certificateSnapshot, error) {
		if loads.Add(1) > 1 {
			return nil, fmt.Errorf("projected Secret is incomplete")
		}
		return &certificateSnapshot{certificate: tls.Certificate{Leaf: &x509.Certificate{NotBefore: startedAt.Add(-time.Hour), NotAfter: startedAt.Add(90 * time.Second)}}}, nil
	})
	require.NoError(t, err)

	currentTime.Add(int64(certificateRefreshInterval))
	_, err = cache.snapshot()
	require.NoError(t, err)
	_, err = cache.snapshot()
	require.NoError(t, err)
	require.EqualValues(t, 2, loads.Load())

	currentTime.Add(int64(31 * time.Second))
	_, err = cache.snapshot()
	require.ErrorContains(t, err, "refresh certificate")
	require.EqualValues(t, 2, loads.Load())
}

func TestApplicationIdentityTLSAndRotation(t *testing.T) {
	startedAt := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: startedAt.Add(-time.Minute), NotAfter: startedAt.Add(time.Hour)}
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
	serverIdentity, clientIdentity := "spiffe://codespace/agent/site/runtime/pod-1", "spiffe://codespace/gateway/component-uid"
	issue(serverDirectory, serverIdentity)
	issue(clientDirectory, clientIdentity)
	var serverTime atomic.Int64
	serverTime.Store(startedAt.UnixNano())
	exactIdentity := func(expected string) IdentityCheck {
		return func(identity *url.URL) error {
			if identity.String() != expected {
				return fmt.Errorf("unexpected peer identity")
			}
			return nil
		}
	}
	serverCertificates, err := newFileCertificates(serverDirectory, func() time.Time { return time.Unix(0, serverTime.Load()) })
	require.NoError(t, err)
	serverTLS, err := serverCertificates.Server(exactIdentity(clientIdentity))
	require.NoError(t, err)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "authorized") }))
	server.EnableHTTP2, server.TLS = true, serverTLS
	server.StartTLS()
	defer server.Close()
	clientCertificates, err := NewFileCertificates(clientDirectory)
	require.NoError(t, err)
	clientTLS, err := clientCertificates.Client(exactIdentity(serverIdentity))
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
	serverTime.Add(int64(certificateRefreshInterval))
	_, err = client.Get(server.URL)
	require.ErrorContains(t, err, "unexpected peer identity")
}
