// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"context"
	"fmt"
	"sync"
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
func (p *DummyProvisioner) CreateOrStart(ctx context.Context, spec InstanceSpec) (*Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if spec.Name == "" {
		return nil, fmt.Errorf("instance name is empty")
	}

	instance, ok := p.instances[spec.Name]
	if !ok {
		instance = &Instance{
			CodespaceUUID:          spec.CodespaceUUID,
			Name:                   spec.Name,
			RuntimeState:           RuntimeStateRunning,
			RepoFullName:           spec.RepoFullName,
			RepoTag:                spec.RepoTag,
			InternalSSHHost:        "127.0.0.1",
			InternalSSHPort:        22,
			InternalSSHUser:        "root",
			InternalSSHAuthMode:    "publickey",
			InternalSSHFingerprint: "SHA256:dummy",
		}
		p.instances[instance.Name] = instance
	}
	instance.Workdir = "/codespace/" + repoDirName(spec.RepoFullName)
	instance.RuntimeState = RuntimeStateRunning

	copyValue := *instance
	return &copyValue, nil
}

// StartExisting starts one existing instance.
func (p *DummyProvisioner) StartExisting(ctx context.Context, spec InstanceSpec) (*Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if spec.Name == "" {
		return nil, fmt.Errorf("instance name is empty")
	}
	instance, ok := p.instances[spec.Name]
	if !ok {
		return nil, fmt.Errorf("instance %s does not exist", spec.Name)
	}
	instance.Workdir = "/codespace/" + repoDirName(instance.RepoFullName)
	instance.RuntimeState = RuntimeStateRunning

	copyValue := *instance
	return &copyValue, nil
}

// ListInstances returns all local dummy instances.
func (p *DummyProvisioner) ListInstances(ctx context.Context) ([]*Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	instances := make([]*Instance, 0, len(p.instances))
	for _, instance := range p.instances {
		copyValue := *instance
		instances = append(instances, &copyValue)
	}
	return instances, nil
}

// WriteCredentials simulates writing runtime credential files.
func (p *DummyProvisioner) WriteCredentials(ctx context.Context, instanceName string, request CredentialRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if instanceName == "" {
		return fmt.Errorf("instance name is empty")
	}
	if request.CodespaceUUID == "" {
		return fmt.Errorf("codespace uuid is empty")
	}
	if request.GiteaToken == "" {
		return fmt.Errorf("gitea token is empty")
	}
	if request.RuntimeToken == "" {
		return fmt.Errorf("runtime token is empty")
	}
	return nil
}

// Bootstrap simulates one bootstrap run.
func (p *DummyProvisioner) Bootstrap(ctx context.Context, instanceName string, request BootstrapRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if instanceName == "" {
		return fmt.Errorf("instance name is empty")
	}
	if request.CodespaceUUID == "" {
		return fmt.Errorf("codespace uuid is empty")
	}
	if request.Workdir == "" {
		return fmt.Errorf("workdir is empty")
	}
	return nil
}

// Stop marks one instance as stopped.
func (p *DummyProvisioner) Stop(ctx context.Context, instanceName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.instances[instanceName]; !ok {
		return nil
	}
	p.instances[instanceName].RuntimeState = RuntimeStateStopped
	return nil
}

// Delete deletes one instance.
func (p *DummyProvisioner) Delete(ctx context.Context, instanceName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.instances, instanceName)
	return nil
}
