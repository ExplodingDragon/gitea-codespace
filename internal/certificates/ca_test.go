// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package certificates

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseCABundle(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})

	parsed, err := ParseCABundle(string(append(certificate, certificate...)))
	require.NoError(t, err)
	require.Len(t, parsed, 1)

	_, err = ParseCABundle(string(certificate) + "unexpected")
	require.ErrorContains(t, err, "outside a PEM certificate")
	_, err = ParseCABundle("")
	require.ErrorContains(t, err, "no certificates")
}

func TestParseSystemCABundleAllowsCertificateComments(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})

	parsed, err := ParseSystemCABundle("# system certificate\n" + string(certificate) + "# end\n")
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	_, err = ParseCABundle("# configured certificate\n" + string(certificate))
	require.ErrorContains(t, err, "outside")
}
