// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package certificates

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"strings"
)

// ParseCABundle parses every certificate in a PEM bundle and removes duplicates.
func ParseCABundle(bundle string) (map[string]*x509.Certificate, error) {
	certificates := map[string]*x509.Certificate{}
	rest := []byte(strings.TrimSpace(bundle))
	for len(rest) > 0 {
		block, remaining := pem.Decode(rest)
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
