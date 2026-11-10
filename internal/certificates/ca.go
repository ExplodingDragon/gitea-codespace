// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package certificates parses and normalizes certificate bundles used for
// outbound TLS trust.
package certificates

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"strings"
)

const (
	// OutboundBundlePath is the Runtime-wide trust bundle consumed by outbound TLS clients.
	OutboundBundlePath = "/run/codespace/ca/bundle.pem"
	// ProxyCABundlePath is the fixed optional proxy trust input projected by Kubernetes.
	ProxyCABundlePath = ProxyCADirectory + "/ca.crt"
	// ProxyCADirectory is the parent mount used for the optional proxy trust input.
	ProxyCADirectory = "/run/codespace/proxy"
)

// ParseCABundle parses every certificate in a configured PEM bundle and removes duplicates.
func ParseCABundle(bundle string) (map[string]*x509.Certificate, error) {
	return parseCABundle(bundle, false)
}

// ParseSystemCABundle accepts the certificate comments emitted by system trust tools.
func ParseSystemCABundle(bundle string) (map[string]*x509.Certificate, error) {
	return parseCABundle(bundle, true)
}

func parseCABundle(bundle string, allowComments bool) (map[string]*x509.Certificate, error) {
	certificates := map[string]*x509.Certificate{}
	rest := []byte(strings.TrimSpace(bundle))
	for len(rest) > 0 {
		start := bytes.Index(rest, []byte("-----BEGIN "))
		if start < 0 {
			if allowComments && onlyComments(string(rest)) {
				break
			}
			return nil, fmt.Errorf("CA bundle contains data outside a PEM certificate")
		}
		if leading := strings.TrimSpace(string(rest[:start])); leading != "" && (!allowComments || !onlyComments(leading)) {
			return nil, fmt.Errorf("CA bundle contains data outside a PEM certificate")
		}
		block, remaining := pem.Decode(rest[start:])
		if block == nil {
			return nil, fmt.Errorf("CA bundle contains data outside a PEM certificate")
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, fmt.Errorf("CA bundle contains a non-certificate PEM block")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse CA certificate: %w", err)
		}
		fingerprint := sha256.Sum256(certificate.Raw)
		certificates[hex.EncodeToString(fingerprint[:])] = certificate
		rest = []byte(strings.TrimSpace(string(remaining)))
	}
	if len(certificates) == 0 {
		return nil, fmt.Errorf("CA bundle contains no certificates")
	}
	return certificates, nil
}

func onlyComments(value string) bool {
	for line := range strings.SplitSeq(value, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			return false
		}
	}
	return true
}
