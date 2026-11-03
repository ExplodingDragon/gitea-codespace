// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package identity creates and verifies the private certificate hierarchy used
// by Codespace components inside one Kubernetes installation.
package identity

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	RootCertificateKey   = "ca.crt"
	RootPrivateKeyKey    = "ca.key"
	IssuerCertificateKey = "issuer.crt"
	IssuerPrivateKeyKey  = "issuer.key"
	TLSCertificateKey    = "tls.crt"
	TLSPrivateKeyKey     = "tls.key"

	RootLifetime      = 10 * 365 * 24 * time.Hour
	IssuerLifetime    = 365 * 24 * time.Hour
	IssuerRenewBefore = 30 * 24 * time.Hour
	LeafLifetime      = 24 * time.Hour
	LeafRenewBefore   = 8 * time.Hour
	ClockSkew         = 5 * time.Minute
)

// Authority is the validated root and current issuing certificate. Its
// certificate state is immutable; rotation returns a new Authority.
type Authority struct {
	root       *x509.Certificate
	rootKey    crypto.Signer
	issuer     *x509.Certificate
	issuerKey  crypto.Signer
	rootPEM    []byte
	rootKeyPEM []byte
}

// Leaf contains the Kubernetes Secret data for one workload identity.
type Leaf struct {
	Certificate []byte
	PrivateKey  []byte
	Root        []byte
	NotAfter    time.Time
}

// Agent identifies one Runtime Pod within its Gitea site and Codespace.
type Agent struct {
	SiteUID, RuntimeUID, PodUID string
}

// Component identifies one Gateway or Cache deployment.
type Component struct {
	Role, UID string
}

// NewAuthority creates a new installation root and its first issuing CA.
func NewAuthority(now time.Time) (*Authority, error) {
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate identity root key: %w", err)
	}
	rootTemplate, err := caTemplate("Codespace internal root", now.Add(-ClockSkew), now.Add(RootLifetime))
	if err != nil {
		return nil, err
	}
	rootTemplate.MaxPathLen = 1
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, rootKey.Public(), rootKey)
	if err != nil {
		return nil, fmt.Errorf("create identity root certificate: %w", err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		return nil, fmt.Errorf("parse generated identity root certificate: %w", err)
	}
	rootPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})
	rootKeyPEM, err := marshalPrivateKey(rootKey)
	if err != nil {
		return nil, err
	}
	return newIssuer(root, rootKey, rootPEM, rootKeyPEM, now)
}

// ParseAuthority validates persisted CA material before it is used. Invalid
// material is never repaired implicitly because that would create a new trust
// domain while workloads still trust the previous one.
func ParseAuthority(data map[string][]byte, now time.Time) (*Authority, error) {
	root, err := parseCertificate(data[RootCertificateKey], RootCertificateKey)
	if err != nil {
		return nil, err
	}
	rootKey, err := parsePrivateKey(data[RootPrivateKeyKey], RootPrivateKeyKey)
	if err != nil {
		return nil, err
	}
	issuer, err := parseCertificate(data[IssuerCertificateKey], IssuerCertificateKey)
	if err != nil {
		return nil, err
	}
	issuerKey, err := parsePrivateKey(data[IssuerPrivateKeyKey], IssuerPrivateKeyKey)
	if err != nil {
		return nil, err
	}
	if !root.IsCA || !issuer.IsCA || root.KeyUsage&x509.KeyUsageCertSign == 0 || issuer.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, fmt.Errorf("persisted identity certificates must be certificate authorities")
	}
	if err := root.CheckSignatureFrom(root); err != nil {
		return nil, fmt.Errorf("verify identity root certificate: %w", err)
	}
	if err := issuer.CheckSignatureFrom(root); err != nil {
		return nil, fmt.Errorf("verify identity issuer certificate: %w", err)
	}
	if !publicKeysEqual(root.PublicKey, rootKey.Public()) || !publicKeysEqual(issuer.PublicKey, issuerKey.Public()) {
		return nil, fmt.Errorf("persisted identity certificate and private key do not match")
	}
	if now.Before(root.NotBefore) || !now.Before(root.NotAfter) {
		return nil, fmt.Errorf("identity root certificate is not currently valid")
	}
	return &Authority{
		root: root, rootKey: rootKey, issuer: issuer, issuerKey: issuerKey,
		rootPEM: bytes.Clone(data[RootCertificateKey]), rootKeyPEM: bytes.Clone(data[RootPrivateKeyKey]),
	}, nil
}

// Data returns an independent copy suitable for a Kubernetes Secret.
func (a *Authority) Data() (map[string][]byte, error) {
	issuerKey, err := marshalPrivateKey(a.issuerKey)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		RootCertificateKey:   bytes.Clone(a.rootPEM),
		RootPrivateKeyKey:    bytes.Clone(a.rootKeyPEM),
		IssuerCertificateKey: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.issuer.Raw}),
		IssuerPrivateKeyKey:  issuerKey,
	}, nil
}

func (a *Authority) RootPEM() []byte { return bytes.Clone(a.rootPEM) }

func (a *Authority) RootNotAfter() time.Time { return a.root.NotAfter }

func (a *Authority) IssuerNeedsRenewal(now time.Time) bool {
	return now.Before(a.issuer.NotBefore) || !now.Before(a.issuer.NotAfter.Add(-IssuerRenewBefore))
}

// RenewIssuer preserves the installation root while replacing the online
// signing key.
func (a *Authority) RenewIssuer(now time.Time) (*Authority, error) {
	return newIssuer(a.root, a.rootKey, a.rootPEM, a.rootKeyPEM, now)
}

func newIssuer(root *x509.Certificate, rootKey crypto.Signer, rootPEM, rootKeyPEM []byte, now time.Time) (*Authority, error) {
	notAfter := minTime(now.Add(IssuerLifetime), root.NotAfter)
	if !notAfter.After(now.Add(LeafLifetime + ClockSkew)) {
		return nil, fmt.Errorf("identity root certificate expires too soon to renew the issuer")
	}
	issuerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate identity issuer key: %w", err)
	}
	issuerTemplate, err := caTemplate("Codespace internal issuer", now.Add(-ClockSkew), notAfter)
	if err != nil {
		return nil, err
	}
	issuerTemplate.MaxPathLenZero = true
	issuerDER, err := x509.CreateCertificate(rand.Reader, issuerTemplate, root, issuerKey.Public(), rootKey)
	if err != nil {
		return nil, fmt.Errorf("create identity issuer certificate: %w", err)
	}
	issuer, err := x509.ParseCertificate(issuerDER)
	if err != nil {
		return nil, fmt.Errorf("parse generated identity issuer certificate: %w", err)
	}
	return &Authority{
		root: root, rootKey: rootKey, issuer: issuer, issuerKey: issuerKey,
		rootPEM: bytes.Clone(rootPEM), rootKeyPEM: bytes.Clone(rootKeyPEM),
	}, nil
}

// Issue creates a short-lived certificate containing exactly one application
// identity URI and the requested TLS purposes.
func (a *Authority) Issue(rawIdentity string, usages []x509.ExtKeyUsage, now time.Time) (*Leaf, error) {
	identity, err := parseURI(rawIdentity)
	if err != nil {
		return nil, err
	}
	if len(usages) == 0 {
		return nil, fmt.Errorf("identity certificate requires at least one extended key usage")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate identity key: %w", err)
	}
	notAfter := minTime(now.Add(LeafLifetime), a.issuer.NotAfter)
	if !notAfter.After(now) {
		return nil, fmt.Errorf("identity issuer certificate has expired")
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: rawIdentity},
		NotBefore: now.Add(-ClockSkew), NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: slices.Clone(usages),
		BasicConstraintsValid: true, URIs: []*url.URL{identity},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, a.issuer, key.Public(), a.issuerKey)
	if err != nil {
		return nil, fmt.Errorf("create identity certificate: %w", err)
	}
	privateKey, err := marshalPrivateKey(key)
	if err != nil {
		return nil, err
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})
	certificate = append(certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.issuer.Raw})...)
	return &Leaf{Certificate: certificate, PrivateKey: privateKey, Root: a.RootPEM(), NotAfter: notAfter}, nil
}

// ValidateLeaf verifies persisted leaf material and returns its renewal time.
func (a *Authority) ValidateLeaf(data map[string][]byte, rawIdentity string, usages []x509.ExtKeyUsage, now time.Time) (time.Time, error) {
	pair, err := tls.X509KeyPair(data[TLSCertificateKey], data[TLSPrivateKeyKey])
	if err != nil {
		return time.Time{}, fmt.Errorf("load identity certificate pair: %w", err)
	}
	if !bytes.Equal(data[RootCertificateKey], a.rootPEM) {
		return time.Time{}, fmt.Errorf("identity certificate has a different trust root")
	}
	if len(pair.Certificate) < 2 {
		return time.Time{}, fmt.Errorf("identity certificate chain does not include its issuer")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return time.Time{}, fmt.Errorf("parse identity certificate: %w", err)
	}
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != rawIdentity {
		return time.Time{}, fmt.Errorf("identity certificate URI does not match its workload")
	}
	if !sameUsages(leaf.ExtKeyUsage, usages) {
		return time.Time{}, fmt.Errorf("identity certificate usages do not match its workload")
	}
	roots := x509.NewCertPool()
	roots.AddCert(a.root)
	intermediates := x509.NewCertPool()
	for _, encoded := range pair.Certificate[1:] {
		certificate, err := x509.ParseCertificate(encoded)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse identity certificate chain: %w", err)
		}
		intermediates.AddCert(certificate)
	}
	for _, usage := range usages {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
			return time.Time{}, fmt.Errorf("verify identity certificate: %w", err)
		}
	}
	return leaf.NotAfter.Add(-LeafRenewBefore), nil
}

func parseURI(raw string) (*url.URL, error) {
	identity, err := url.Parse(raw)
	if err != nil || validateURI(identity) != nil {
		return nil, fmt.Errorf("invalid Codespace application identity")
	}
	return identity, nil
}

func ParseManagerURI(raw string) (*url.URL, error) {
	identity, err := parseURI(raw)
	if err != nil {
		return nil, err
	}
	if _, err := identityParts(identity, "manager", 1); err != nil {
		return nil, fmt.Errorf("invalid Codespace Manager identity")
	}
	return identity, nil
}

// ManagerURI returns the identity of the Manager installation in one
// Kubernetes management namespace.
func ManagerURI(namespaceUID string) string {
	return (&url.URL{Scheme: "spiffe", Host: "codespace", Path: "/manager/" + namespaceUID}).String()
}

// AgentURI returns the identity of one Runtime Pod.
func AgentURI(siteUID, runtimeUID, podUID string) string {
	return (&url.URL{Scheme: "spiffe", Host: "codespace", Path: "/agent/" + siteUID + "/" + runtimeUID + "/" + podUID}).String()
}

// ComponentURI returns the identity of one Gateway or Cache deployment.
func ComponentURI(role, componentUID string) string {
	return (&url.URL{Scheme: "spiffe", Host: "codespace", Path: "/" + role + "/" + componentUID}).String()
}

// ParseAgentURI returns the resource identities bound to an Agent certificate.
func ParseAgentURI(identity *url.URL) (Agent, error) {
	parts, err := identityParts(identity, "agent", 3)
	if err != nil {
		return Agent{}, fmt.Errorf("invalid Codespace Agent identity")
	}
	return Agent{SiteUID: parts[0], RuntimeUID: parts[1], PodUID: parts[2]}, nil
}

// ParseComponentURI returns the role and Kubernetes UID bound to a component
// certificate.
func ParseComponentURI(identity *url.URL) (Component, error) {
	if err := validateURI(identity); err != nil {
		return Component{}, fmt.Errorf("invalid Codespace component identity")
	}
	parts := strings.Split(strings.TrimPrefix(identity.Path, "/"), "/")
	if len(parts) != 2 || (parts[0] != "gateway" && parts[0] != "cache") || parts[1] == "" {
		return Component{}, fmt.Errorf("invalid Codespace component identity")
	}
	return Component{Role: parts[0], UID: parts[1]}, nil
}

func identityParts(identity *url.URL, role string, count int) ([]string, error) {
	if err := validateURI(identity); err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(identity.Path, "/"), "/")
	if len(parts) != count+1 || parts[0] != role {
		return nil, fmt.Errorf("unexpected identity path")
	}
	for _, part := range parts[1:] {
		if part == "" {
			return nil, fmt.Errorf("identity path contains an empty segment")
		}
	}
	return parts[1:], nil
}

func validateURI(identity *url.URL) error {
	if identity == nil || identity.Scheme != "spiffe" || identity.Host != "codespace" || identity.User != nil || identity.RawQuery != "" || identity.Fragment != "" || identity.RawPath != "" || !strings.HasPrefix(identity.Path, "/") {
		return fmt.Errorf("invalid Codespace application identity")
	}
	return nil
}

func caTemplate(commonName string, notBefore, notAfter time.Time) (*x509.Certificate, error) {
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	return &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: commonName},
		NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true, IsCA: true,
	}, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial number: %w", err)
	}
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	return serial, nil
}

func marshalPrivateKey(key crypto.Signer) ([]byte, error) {
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal identity private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), nil
}

func parsePrivateKey(data []byte, name string) (crypto.Signer, error) {
	block, rest := pem.Decode(data)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("decode persisted %s", name)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse persisted %s: %w", name, err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("persisted %s is not a signing key", name)
	}
	if ec, ok := signer.(*ecdsa.PrivateKey); !ok || ec.Curve != elliptic.P256() {
		return nil, fmt.Errorf("persisted %s must be an ECDSA P-256 key", name)
	}
	return signer, nil
}

func parseCertificate(data []byte, name string) (*x509.Certificate, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("decode persisted %s", name)
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse persisted %s: %w", name, err)
	}
	return certificate, nil
}

func publicKeysEqual(a, b any) bool {
	first, err := x509.MarshalPKIXPublicKey(a)
	if err != nil {
		return false
	}
	second, err := x509.MarshalPKIXPublicKey(b)
	return err == nil && bytes.Equal(first, second)
}

func sameUsages(actual, expected []x509.ExtKeyUsage) bool {
	actual = slices.Clone(actual)
	expected = slices.Clone(expected)
	slices.Sort(actual)
	slices.Sort(expected)
	return slices.Equal(actual, expected)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
