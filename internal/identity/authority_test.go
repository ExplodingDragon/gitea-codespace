// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package identity

import (
	"bytes"
	"crypto/x509"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAuthorityPersistsAndIssuesExactIdentity(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	authority, err := NewAuthority(now)
	require.NoError(t, err)
	data, err := authority.Data()
	require.NoError(t, err)
	reloaded, err := ParseAuthority(data, now)
	require.NoError(t, err)

	const workloadIdentity = "spiffe://codespace/agent/site/runtime/pod"
	usages := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}
	leaf, err := reloaded.Issue(workloadIdentity, usages, now)
	require.NoError(t, err)
	secretData := map[string][]byte{TLSCertificateKey: leaf.Certificate, TLSPrivateKeyKey: leaf.PrivateKey, RootCertificateKey: leaf.Root}
	renewAt, err := reloaded.ValidateLeaf(secretData, workloadIdentity, usages, now)
	require.NoError(t, err)
	require.Equal(t, leaf.NotAfter.Add(-LeafRenewBefore), renewAt)

	_, err = reloaded.ValidateLeaf(secretData, "spiffe://codespace/agent/site/runtime/other-pod", usages, now)
	require.ErrorContains(t, err, "URI")
	_, err = reloaded.ValidateLeaf(secretData, workloadIdentity, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, now)
	require.ErrorContains(t, err, "usages")
	_, err = reloaded.ValidateLeaf(secretData, workloadIdentity, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageCodeSigning}, now)
	require.ErrorContains(t, err, "usages")
}

func TestAuthorityIssuesExactServiceDNSName(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	authority, err := NewAuthority(now)
	require.NoError(t, err)
	const dnsName = "cache.codespace-system.svc"
	leaf, err := authority.IssueServerCertificate(dnsName, now)
	require.NoError(t, err)
	secretData := map[string][]byte{TLSCertificateKey: leaf.Certificate, TLSPrivateKeyKey: leaf.PrivateKey, RootCertificateKey: leaf.Root}
	renewAt, err := authority.ValidateServerCertificate(secretData, dnsName, now)
	require.NoError(t, err)
	require.Equal(t, leaf.NotAfter.Add(-LeafRenewBefore), renewAt)

	_, err = authority.ValidateServerCertificate(secretData, "other.codespace-system.svc", now)
	require.ErrorContains(t, err, "identity")
	_, err = authority.IssueServerCertificate("Cache.invalid", now)
	require.ErrorContains(t, err, "DNS")
	_, err = authority.ValidateLeaf(secretData, "spiffe://codespace/cache/component", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, now)
	require.ErrorContains(t, err, "URI")
}

func TestApplicationIdentityShapes(t *testing.T) {
	managerURI, err := ParseManagerURI(ManagerURI("namespace"))
	require.NoError(t, err)
	require.Equal(t, "spiffe://codespace/manager/namespace", managerURI.String())

	agentURI, err := url.Parse(AgentURI("site", "runtime", "pod"))
	require.NoError(t, err)
	agent, err := ParseAgentURI(agentURI)
	require.NoError(t, err)
	require.Equal(t, Agent{SiteUID: "site", RuntimeUID: "runtime", PodUID: "pod"}, agent)

	for _, role := range []string{"gateway", "cache"} {
		componentURI, err := url.Parse(ComponentURI(role, "component"))
		require.NoError(t, err)
		component, err := ParseComponentURI(componentURI)
		require.NoError(t, err)
		require.Equal(t, Component{Role: role, UID: "component"}, component)
	}
}

func TestIssuerRenewalPreservesRoot(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	authority, err := NewAuthority(now)
	require.NoError(t, err)
	before, err := authority.Data()
	require.NoError(t, err)
	require.False(t, authority.IssuerNeedsRenewal(now))

	renewed, err := authority.RenewIssuer(now.Add(IssuerLifetime - IssuerRenewBefore))
	require.NoError(t, err)
	after, err := renewed.Data()
	require.NoError(t, err)
	require.Equal(t, before[RootCertificateKey], after[RootCertificateKey])
	require.Equal(t, before[RootPrivateKeyKey], after[RootPrivateKeyKey])
	require.False(t, bytes.Equal(before[IssuerCertificateKey], after[IssuerCertificateKey]))
	require.False(t, bytes.Equal(before[IssuerPrivateKeyKey], after[IssuerPrivateKeyKey]))
}

func TestAuthorityRejectsCorruptPersistedMaterial(t *testing.T) {
	authority, err := NewAuthority(time.Now())
	require.NoError(t, err)
	data, err := authority.Data()
	require.NoError(t, err)
	data[IssuerPrivateKeyKey] = data[RootPrivateKeyKey]
	_, err = ParseAuthority(data, time.Now())
	require.ErrorContains(t, err, "do not match")
}
