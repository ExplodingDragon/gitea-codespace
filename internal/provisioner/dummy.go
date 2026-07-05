// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"fmt"
	"sync"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
)

// DummyProvisioner simulates backend operations for tests.
type DummyProvisioner struct {
	mu        sync.Mutex
	instances map[string]*Instance
}

// NewDummy creates one dummy provisioner.
func NewDummy() *DummyProvisioner {
	return &DummyProvisioner{
		instances: make(map[string]*Instance),
	}
}

// CreateOrStart creates or starts one instance.
func (p *DummyProvisioner) CreateOrStart(spec InstanceSpec) (*Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if spec.Name == "" {
		return nil, fmt.Errorf("instance name is empty")
	}

	instance, ok := p.instances[spec.Name]
	if !ok {
		instance = &Instance{
			Name:         spec.Name,
			Image:        spec.Image,
			Type:         spec.Type,
			RepoFullName: spec.RepoFullName,
		}
		p.instances[instance.Name] = instance
	}
	instance.State = codespacev1.CodespaceStatus_CODESPACE_STATUS_RUNNING
	instance.Workdir = "/codespace/" + repoDirName(spec.RepoFullName)

	copyValue := *instance
	return &copyValue, nil
}

// StartExisting starts one existing instance.
func (p *DummyProvisioner) StartExisting(spec InstanceSpec) (*Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if spec.Name == "" {
		return nil, fmt.Errorf("instance name is empty")
	}
	instance, ok := p.instances[spec.Name]
	if !ok {
		return nil, fmt.Errorf("instance %s does not exist", spec.Name)
	}
	instance.State = codespacev1.CodespaceStatus_CODESPACE_STATUS_RUNNING
	instance.Workdir = "/codespace/" + repoDirName(instance.RepoFullName)

	copyValue := *instance
	return &copyValue, nil
}

// Bootstrap simulates one bootstrap run.
func (p *DummyProvisioner) Bootstrap(instanceName string, request BootstrapRequest) error {
	if instanceName == "" {
		return fmt.Errorf("instance name is empty")
	}
	if request.CodespaceID == "" {
		return fmt.Errorf("codespace id is empty")
	}
	if request.Workdir == "" {
		return fmt.Errorf("workdir is empty")
	}
	return nil
}

// Stop marks one instance as stopped.
func (p *DummyProvisioner) Stop(instanceName string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	instance, ok := p.instances[instanceName]
	if !ok {
		return nil
	}
	instance.State = codespacev1.CodespaceStatus_CODESPACE_STATUS_STOPPED
	return nil
}

// Delete deletes one instance.
func (p *DummyProvisioner) Delete(instanceName string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.instances, instanceName)
	return nil
}
