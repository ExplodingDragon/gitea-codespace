// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"net"
	"reflect"
	"testing"

	"github.com/lxc/incus/v6/shared/api"
)

func TestIncusInstanceFromAPIRequiresManagerOwnership(t *testing.T) {
	t.Parallel()

	provisioner := &IncusProvisioner{managerID: "7"}
	instance, ok := provisioner.instanceFromAPI(api.Instance{
		Name:   "cs-11111111222243338444",
		Status: "Running",
		InstancePut: api.InstancePut{
			Config: map[string]string{
				incusConfigManagerID:     "7",
				incusConfigCodespaceUUID: "11111111-2222-4333-8444-555555555555",
				incusConfigTag:           "default",
			},
		},
	})
	if !ok {
		t.Fatalf("owned instance was not accepted")
	}
	if instance.CodespaceUUID != "11111111-2222-4333-8444-555555555555" ||
		instance.Name != "cs-11111111222243338444" ||
		instance.RuntimeState != RuntimeStateRunning ||
		instance.RepoTag != "default" {
		t.Fatalf("instance = %#v", instance)
	}
}

func TestIncusInstanceFromAPISkipsOtherManagers(t *testing.T) {
	t.Parallel()

	provisioner := &IncusProvisioner{managerID: "7"}
	_, ok := provisioner.instanceFromAPI(api.Instance{
		Name:   "cs-11111111222243338444",
		Status: "Running",
		InstancePut: api.InstancePut{
			Config: map[string]string{
				incusConfigManagerID:     "8",
				incusConfigCodespaceUUID: "11111111-2222-4333-8444-555555555555",
			},
		},
	})
	if ok {
		t.Fatalf("instance owned by another manager was accepted")
	}
}

func TestIncusInstanceFromAPISkipsMissingCodespaceUUID(t *testing.T) {
	t.Parallel()

	provisioner := &IncusProvisioner{managerID: "7"}
	_, ok := provisioner.instanceFromAPI(api.Instance{
		Name:   "cs-11111111222243338444",
		Status: "Running",
		InstancePut: api.InstancePut{
			Config: map[string]string{
				incusConfigManagerID: "7",
			},
		},
	})
	if ok {
		t.Fatalf("instance without codespace uuid was accepted")
	}
}

func TestBootstrapCredentialFilesUseFixedPathsAndModes(t *testing.T) {
	t.Parallel()

	files := bootstrapCredentialFiles(CredentialRequest{
		GiteaToken:   "gitea-token",
		RuntimeToken: "runtime-token",
	})
	got := make([]bootstrapCredentialFile, len(files))
	copy(got, files)
	want := []bootstrapCredentialFile{
		{
			path: runtimeCredentialDir,
			mode: runtimeCredentialDirMode,
			kind: "directory",
		},
		{
			path: runtimeGitCredentialDir,
			mode: runtimeCredentialDirMode,
			kind: "directory",
		},
		{
			path:    runtimeGiteaTokenFilePath,
			content: "gitea-token",
			mode:    runtimeCredentialFileMode,
			kind:    "file",
		},
		{
			path:    runtimeAPITokenFilePath,
			content: "runtime-token",
			mode:    runtimeCredentialFileMode,
			kind:    "file",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("credential files = %#v", got)
	}
}

func TestInstanceStateHasSourceIPUsesCommunicationInterface(t *testing.T) {
	t.Parallel()

	state := &api.InstanceState{
		Network: map[string]api.InstanceStateNetwork{
			"eth0": {
				Addresses: []api.InstanceStateNetworkAddress{
					{Family: "inet", Address: "10.0.0.12", Scope: "global"},
					{Family: "inet6", Address: "fe80::1", Scope: "link"},
				},
			},
			"eth1": {
				Addresses: []api.InstanceStateNetworkAddress{
					{Family: "inet", Address: "10.0.1.12", Scope: "global"},
				},
			},
		},
	}

	if !instanceStateHasSourceIP(state, net.ParseIP("10.0.0.12"), "eth0") {
		t.Fatalf("eth0 source address was not accepted")
	}
	if instanceStateHasSourceIP(state, net.ParseIP("10.0.1.12"), "eth0") {
		t.Fatalf("address from a different interface was accepted")
	}
	if instanceStateHasSourceIP(state, net.ParseIP("fe80::1"), "eth0") {
		t.Fatalf("link-local address was accepted")
	}
	if !instanceStateHasSourceIP(state, net.ParseIP("10.0.1.12"), "") {
		t.Fatalf("address from any interface was not accepted")
	}
}

func TestNetworkAddressHasSourceIPRejectsLocalAddresses(t *testing.T) {
	t.Parallel()

	network := api.InstanceStateNetwork{
		Addresses: []api.InstanceStateNetworkAddress{
			{Family: "inet", Address: "127.0.0.1", Scope: "local"},
			{Family: "inet", Address: "169.254.1.1", Scope: "global"},
			{Family: "inet6", Address: "::1", Scope: "local"},
		},
	}
	for _, sourceIP := range []string{"127.0.0.1", "169.254.1.1", "::1"} {
		if networkAddressHasSourceIP(network, net.ParseIP(sourceIP)) {
			t.Fatalf("local source address %s was accepted", sourceIP)
		}
	}
}

func TestValidateIncusServerAcceptsTrustedNonClusteredProject(t *testing.T) {
	t.Parallel()

	if err := validateIncusServer(&api.Server{
		ServerUntrusted: api.ServerUntrusted{
			Auth: "trusted",
		},
		Environment: api.ServerEnvironment{
			Server:          "incus",
			Project:         "codespace",
			ServerClustered: false,
		},
	}, "codespace"); err != nil {
		t.Fatalf("validate incus server: %v", err)
	}
}

func TestValidateIncusServerRejectsUnsupportedServer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		server  *api.Server
		project string
	}{
		{
			name: "untrusted",
			server: &api.Server{
				ServerUntrusted: api.ServerUntrusted{Auth: "untrusted"},
				Environment:     api.ServerEnvironment{Server: "incus", Project: "codespace"},
			},
			project: "codespace",
		},
		{
			name: "clustered",
			server: &api.Server{
				ServerUntrusted: api.ServerUntrusted{Auth: "trusted"},
				Environment: api.ServerEnvironment{
					Server:          "incus",
					Project:         "codespace",
					ServerClustered: true,
				},
			},
			project: "codespace",
		},
		{
			name: "wrong project",
			server: &api.Server{
				ServerUntrusted: api.ServerUntrusted{Auth: "trusted"},
				Environment:     api.ServerEnvironment{Server: "incus", Project: "default"},
			},
			project: "codespace",
		},
		{
			name: "public only",
			server: &api.Server{
				ServerUntrusted: api.ServerUntrusted{Auth: "trusted", Public: true},
				Environment:     api.ServerEnvironment{Server: "incus", Project: "codespace"},
			},
			project: "codespace",
		},
		{
			name: "not incus",
			server: &api.Server{
				ServerUntrusted: api.ServerUntrusted{Auth: "trusted"},
				Environment:     api.ServerEnvironment{Server: "lxd", Project: "codespace"},
			},
			project: "codespace",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if err := validateIncusServer(test.server, test.project); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}
}
