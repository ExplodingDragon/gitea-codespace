// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package transport provides the certificate-based identity boundary for components.
package transport

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

type IdentityCheck func(*url.URL) error

// Certificates reads kubelet-projected files at each handshake to pick up rotation.
type Certificates struct{ Directory string }

func (c Certificates) load() (tls.Certificate, *x509.CertPool, error) {
	pair, err := tls.LoadX509KeyPair(filepath.Join(c.Directory, "tls.crt"), filepath.Join(c.Directory, "tls.key"))
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("load component certificate: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(c.Directory, "ca.crt"))
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("load component CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		return tls.Certificate{}, nil, fmt.Errorf("component CA has no certificates")
	}
	return pair, roots, nil
}

func (c Certificates) Server(check IdentityCheck) (*tls.Config, error) {
	if check == nil {
		return nil, fmt.Errorf("peer identity check is required")
	}
	if _, _, err := c.load(); err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		certificate, roots, err := c.load()
		if err != nil {
			return nil, err
		}
		return &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}, Certificates: []tls.Certificate{certificate}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert,
			VerifyConnection: func(state tls.ConnectionState) error { return checkPeerIdentity(state.PeerCertificates, check) },
		}, nil
	}}, nil
}

func (c Certificates) Client(check IdentityCheck) (*tls.Config, error) {
	if check == nil {
		return nil, fmt.Errorf("peer identity check is required")
	}
	if _, _, err := c.load(); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"},
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			certificate, _, err := c.load()
			return &certificate, err
		},
		// Pod IPs are reusable. Verify the CA chain and application URI instead of DNS.
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return fmt.Errorf("peer did not present a certificate")
			}
			_, roots, err := c.load()
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
