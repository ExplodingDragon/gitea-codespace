// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"testing"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"github.com/stretchr/testify/require"
)

func TestRuntimeEndpointStateUpdatesPublishedTarget(t *testing.T) {
	directory := t.TempDir()
	identity := VolumeIdentity{SiteUID: "site", ResourceUID: "resource", RuntimeUUID: "runtime"}
	journal, err := OpenJournal(directory, identity)
	require.NoError(t, err)
	runtime := &Runtime{Journal: journal}
	require.NoError(t, runtime.replaceConfiguredEndpoints([]*codespacev1.RuntimeEndpoint{{EndpointId: "port-3000", Label: "API", Port: 3000}}))
	target := accessTargetForContainer(1, "container", runtime.endpoints)
	runtime.setAccessState(target, nil)

	endpoints, err := runtime.setEndpoint(&codespacev1.RuntimeEndpoint{EndpointId: "port-8080", Label: "Preview", Public: true, Port: 8080})
	require.NoError(t, err)
	require.Len(t, endpoints, 2)
	updated := runtime.CurrentAccessTarget()
	require.EqualValues(t, 2, updated.Version)
	require.Len(t, updated.Endpoints, 3)
	require.True(t, updated.Endpoints[2].Endpoint.Public)

	require.NoError(t, journal.Close())
	journal, err = OpenJournal(directory, identity)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, journal.Close()) })
	recovered := &Runtime{Journal: journal}
	endpoints, err = recovered.listEndpoints()
	require.NoError(t, err)
	require.Len(t, endpoints, 2)
	require.EqualValues(t, 8080, endpoints[1].Port)
}
