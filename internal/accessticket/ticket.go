// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package accessticket signs the short-lived authorization passed from Manager
// through Gateway to one current Runtime Agent.
package accessticket

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	agentv1 "gitea.dev/codespace-proto-go/agent/v1"
	"google.golang.org/protobuf/proto"
)

const (
	ProtocolVersion = 1
	MaxLifetime     = 2 * time.Minute
)

func Sign(privateKey ed25519.PrivateKey, claims *agentv1.AccessTicketClaims, now time.Time) (string, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("access signing key is invalid")
	}
	if claims == nil {
		return "", fmt.Errorf("access ticket claims are required")
	}
	claims = proto.Clone(claims).(*agentv1.AccessTicketClaims)
	claims.ProtocolVersion = ProtocolVersion
	if len(claims.Nonce) == 0 {
		claims.Nonce = make([]byte, 16)
		if _, err := rand.Read(claims.Nonce); err != nil {
			return "", err
		}
	}
	if err := validate(claims, now); err != nil {
		return "", err
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(claims)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)), nil
}

func Verify(publicKey ed25519.PublicKey, token string, now time.Time) (*agentv1.AccessTicketClaims, error) {
	if len(publicKey) != ed25519.PublicKeySize || len(token) > 4096 {
		return nil, fmt.Errorf("access ticket or verification key is invalid")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("access ticket encoding is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(payload) > 2048 {
		return nil, fmt.Errorf("access ticket payload is invalid")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !ed25519.Verify(publicKey, payload, signature) {
		return nil, fmt.Errorf("access ticket signature is invalid")
	}
	claims := &agentv1.AccessTicketClaims{}
	if err := proto.Unmarshal(payload, claims); err != nil {
		return nil, fmt.Errorf("access ticket payload is invalid")
	}
	if err := validate(claims, now); err != nil {
		return nil, err
	}
	return claims, nil
}

func validate(claims *agentv1.AccessTicketClaims, now time.Time) error {
	if claims.ProtocolVersion != ProtocolVersion || claims.SiteUid == "" || claims.ResourceUid == "" || claims.RuntimeUuid == "" || claims.PodUid == "" || claims.TargetVersion <= 0 || claims.Capability == agentv1.AccessCapability_ACCESS_CAPABILITY_UNSPECIFIED || len(claims.Nonce) != 16 {
		return fmt.Errorf("access ticket identity is incomplete")
	}
	expires := time.Unix(claims.ExpiresUnix, 0)
	if !now.Before(expires) || expires.After(now.Add(MaxLifetime)) {
		return fmt.Errorf("access ticket lifetime is invalid")
	}
	if claims.Capability == agentv1.AccessCapability_ACCESS_CAPABILITY_ENDPOINT {
		if claims.EndpointId == "" {
			return fmt.Errorf("endpoint access ticket has no endpoint")
		}
	} else if claims.EndpointId != "" {
		return fmt.Errorf("non-endpoint access ticket contains an endpoint")
	}
	return nil
}
