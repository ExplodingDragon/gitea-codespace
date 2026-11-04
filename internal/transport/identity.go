// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package transport provides the certificate-based identity boundary for components.
package transport

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const certificateRefreshInterval = time.Minute

type IdentityCheck func(*url.URL) error

// CertificateSource supplies the current leaf certificate and trust roots.
// File-backed workloads and the in-memory Manager identity use the same
// verification path while retaining their different refresh behavior.
type CertificateSource interface {
	Load() (tls.Certificate, *x509.CertPool, error)
}

// FileCertificates caches one kubelet-projected component identity. A single
// instance can supply both client and server TLS configurations.
type FileCertificates struct{ cache *certificateCache }

type certificateSnapshot struct {
	certificate tls.Certificate
	roots       *x509.CertPool
}

type certificateCache struct {
	now  func() time.Time
	load func() (*certificateSnapshot, error)

	current     atomic.Pointer[certificateSnapshot]
	nextRefresh atomic.Int64
	mu          sync.Mutex
	lastError   error
}

func newCertificateCache(now func() time.Time, load func() (*certificateSnapshot, error)) (*certificateCache, error) {
	cache := &certificateCache{now: now, load: load}
	loaded, err := load()
	if err != nil {
		return nil, err
	}
	current := now()
	if err := loaded.validAt(current); err != nil {
		return nil, err
	}
	cache.current.Store(loaded)
	cache.nextRefresh.Store(current.Add(certificateRefreshInterval).UnixNano())
	return cache, nil
}

func (c *certificateCache) snapshot() (*certificateSnapshot, error) {
	now := c.now()
	current := c.current.Load()
	if now.UnixNano() < c.nextRefresh.Load() && current.validAt(now) == nil {
		return current, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	now = c.now()
	current = c.current.Load()
	if now.UnixNano() < c.nextRefresh.Load() {
		if err := current.validAt(now); err == nil {
			return current, nil
		}
		if c.lastError != nil {
			return nil, c.lastError
		}
	}

	loaded, err := c.load()
	c.nextRefresh.Store(now.Add(certificateRefreshInterval).UnixNano())
	if err == nil {
		err = loaded.validAt(now)
	}
	if err == nil {
		c.current.Store(loaded)
		c.lastError = nil
		return loaded, nil
	}

	c.lastError = fmt.Errorf("refresh certificate: %w", err)
	log.Printf("TLS certificate refresh failed: %v", err)
	if current.validAt(now) == nil {
		return current, nil
	}
	return nil, c.lastError
}

func (s *certificateSnapshot) validAt(now time.Time) error {
	if s == nil || s.certificate.Leaf == nil {
		return fmt.Errorf("certificate has no parsed leaf")
	}
	if now.Before(s.certificate.Leaf.NotBefore) {
		return fmt.Errorf("certificate is not valid before %s", s.certificate.Leaf.NotBefore.UTC().Format(time.RFC3339))
	}
	if !now.Before(s.certificate.Leaf.NotAfter) {
		return fmt.Errorf("certificate expired at %s", s.certificate.Leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	return nil
}

func loadCertificate(directory, description string) (tls.Certificate, error) {
	pair, err := tls.LoadX509KeyPair(filepath.Join(directory, "tls.crt"), filepath.Join(directory, "tls.key"))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load %s certificate: %w", description, err)
	}
	if len(pair.Certificate) == 0 {
		return tls.Certificate{}, fmt.Errorf("%s certificate chain is empty", description)
	}
	pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse %s certificate: %w", description, err)
	}
	return pair, nil
}

func newFileCertificates(directory string, now func() time.Time) (*FileCertificates, error) {
	cache, err := newCertificateCache(now, func() (*certificateSnapshot, error) {
		pair, err := loadCertificate(directory, "component")
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(filepath.Join(directory, "ca.crt"))
		if err != nil {
			return nil, fmt.Errorf("load component CA: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("component CA has no certificates")
		}
		return &certificateSnapshot{certificate: pair, roots: roots}, nil
	})
	if err != nil {
		return nil, err
	}
	return &FileCertificates{cache: cache}, nil
}

// NewFileCertificates loads and caches one file-backed component identity.
func NewFileCertificates(directory string) (*FileCertificates, error) {
	return newFileCertificates(directory, time.Now)
}

// ServerCertificate returns one-way TLS configuration for an internal HTTPS
// service. New handshakes check the projected key pair at most once per minute,
// so Kubernetes Secret rotation does not require restarting the serving process.
func ServerCertificate(directory string) (*tls.Config, error) {
	return serverCertificate(directory, time.Now)
}

func serverCertificate(directory string, now func() time.Time) (*tls.Config, error) {
	cache, err := newCertificateCache(now, func() (*certificateSnapshot, error) {
		pair, err := loadCertificate(directory, "server")
		if err != nil {
			return nil, err
		}
		return &certificateSnapshot{certificate: pair}, nil
	})
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		current, err := cache.snapshot()
		if err != nil {
			return nil, err
		}
		return &current.certificate, nil
	}}, nil
}

func (c *FileCertificates) Load() (tls.Certificate, *x509.CertPool, error) {
	if c == nil || c.cache == nil {
		return tls.Certificate{}, nil, fmt.Errorf("component certificate source is not initialized")
	}
	current, err := c.cache.snapshot()
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	return current.certificate, current.roots, nil
}

func (c *FileCertificates) Server(check IdentityCheck) (*tls.Config, error) {
	return Server(c, check)
}

func Server(source CertificateSource, check IdentityCheck) (*tls.Config, error) {
	if check == nil {
		return nil, fmt.Errorf("peer identity check is required")
	}
	if _, _, err := source.Load(); err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		certificate, roots, err := source.Load()
		if err != nil {
			return nil, err
		}
		return &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}, Certificates: []tls.Certificate{certificate}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert,
			VerifyConnection: func(state tls.ConnectionState) error { return checkPeerIdentity(state.PeerCertificates, check) },
		}, nil
	}}, nil
}

func (c *FileCertificates) Client(check IdentityCheck) (*tls.Config, error) {
	return Client(c, check)
}

func Client(source CertificateSource, check IdentityCheck) (*tls.Config, error) {
	if check == nil {
		return nil, fmt.Errorf("peer identity check is required")
	}
	if _, _, err := source.Load(); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"},
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			certificate, _, err := source.Load()
			return &certificate, err
		},
		// Pod IPs are reusable. Verify the CA chain and application URI instead of DNS.
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return fmt.Errorf("peer did not present a certificate")
			}
			_, roots, err := source.Load()
			if err != nil {
				return err
			}
			intermediates := x509.NewCertPool()
			for _, certificate := range state.PeerCertificates[1:] {
				intermediates.AddCert(certificate)
			}
			if _, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
				return fmt.Errorf("verify component certificate chain: %w", err)
			}
			return checkPeerIdentity(state.PeerCertificates, check)
		},
	}, nil
}

func checkPeerIdentity(certificates []*x509.Certificate, check IdentityCheck) error {
	if len(certificates) == 0 || len(certificates[0].URIs) != 1 {
		return fmt.Errorf("peer must present exactly one application identity")
	}
	identity := certificates[0].URIs[0]
	if identity.Scheme != "spiffe" || identity.Host != "codespace" || identity.User != nil || identity.RawQuery != "" || identity.Fragment != "" || identity.RawPath != "" {
		return fmt.Errorf("invalid peer application identity")
	}
	return check(identity)
}
