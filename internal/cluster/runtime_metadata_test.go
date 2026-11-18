// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"testing"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRuntimeMetadataPublishesAccessAddresses(t *testing.T) {
	cs := &api.Codespace{
		ObjectMeta: metav1.ObjectMeta{UID: "933ef4a9-54c7-4f36-aad9-32ea72c4c986"},
		Spec:       api.CodespaceSpec{Site: api.ResourceReference{UID: "site"}, CodespaceID: 42},
		Status: api.CodespaceStatus{
			Access: api.RuntimeAccess{GatewayURL: "https://workspace.example:8443", SSHAddress: "ssh.example:2222"},
			Target: &api.AgentTarget{Ready: true, WorkspaceFolder: "/workspaces/a b", Endpoints: []api.AgentEndpoint{
				{ID: "workspace", Label: "Workspace", Port: 13337},
				{ID: "app", Label: "App", Port: 3000, Public: true},
			}},
		},
	}
	metadata, err := runtimeMetadata(cs)
	require.NoError(t, err)
	require.Equal(t, "https://933ef4a954c74f36aad932ea72c4c986.workspace.example:8443/", metadata.Endpoints[0].Url)
	require.Zero(t, metadata.Endpoints[0].Port)
	require.Equal(t, "https://app-933ef4a954c74f36aad932ea72c4c986.workspace.example:8443/", metadata.Endpoints[1].Url)
	require.Equal(t, "ssh://cs-933ef4a9-54c7-4f36-aad9-32ea72c4c986@ssh.example:2222", metadata.Ssh)
	require.Equal(t, "/workspaces/a b", metadata.WorkspaceFolder)
	cs.Status.Access = api.RuntimeAccess{GatewayURL: "http://new.example", SSHAddress: "[2001:db8::1]:22"}
	updated, err := runtimeMetadata(cs)
	require.NoError(t, err)
	require.Equal(t, "http://933ef4a954c74f36aad932ea72c4c986.new.example/", updated.Endpoints[0].Url)
	require.Equal(t, "ssh://cs-933ef4a9-54c7-4f36-aad9-32ea72c4c986@[2001:db8::1]:22", updated.Ssh)
}
