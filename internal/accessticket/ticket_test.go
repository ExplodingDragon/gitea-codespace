// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package accessticket

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	agentv1 "gitea.dev/codespace-proto-go/agent/v1"
	"github.com/stretchr/testify/require"
)

func TestTicketBindsCurrentAccessTarget(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	now := time.Unix(1000, 0)
	encoded, err := Sign(privateKey, &agentv1.AccessTicketClaims{
		SiteUid: "site", ResourceUid: "resource", RuntimeUuid: "runtime", PodUid: "pod", TargetVersion: 7,
		Capability: agentv1.AccessCapability_ACCESS_CAPABILITY_ENDPOINT, EndpointId: "workspace", ExpiresUnix: now.Add(time.Minute).Unix(),
	}, now)
	require.NoError(t, err)
	claims, err := Verify(publicKey, encoded, now.Add(30*time.Second))
	require.NoError(t, err)
	require.EqualValues(t, 7, claims.TargetVersion)
	require.Equal(t, "workspace", claims.EndpointId)

	_, err = Verify(publicKey, encoded, now.Add(time.Minute))
	require.ErrorContains(t, err, "lifetime")
	signature := strings.IndexByte(encoded, '.') + 1
	require.Positive(t, signature)
	replacement := byte('A')
	if encoded[signature] == replacement {
		replacement = 'B'
	}
	encoded = encoded[:signature] + string(replacement) + encoded[signature+1:]
	_, err = Verify(publicKey, encoded, now)
	require.ErrorContains(t, err, "signature")
}
