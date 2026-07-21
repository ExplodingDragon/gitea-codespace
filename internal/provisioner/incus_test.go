// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
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
