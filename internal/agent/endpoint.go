// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	"gitea.dev/codespace/internal/runtimeendpoint"
	"google.golang.org/protobuf/proto"
)

const endpointStatePath = "state/endpoints.pb"

type RuntimeEndpointServer struct {
	Runtime *Runtime
}

func (s *RuntimeEndpointServer) List(_ context.Context, request *connect.Request[agentv1.RuntimeEndpointServiceListRequest]) (*connect.Response[agentv1.RuntimeEndpointServiceListResponse], error) {
	if request.Msg.ProtocolVersion != 1 || s.Runtime == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid runtime endpoint request"))
	}
	endpoints, err := s.Runtime.listEndpoints()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentv1.RuntimeEndpointServiceListResponse{Endpoints: endpoints}), nil
}

func (s *RuntimeEndpointServer) Set(_ context.Context, request *connect.Request[agentv1.RuntimeEndpointServiceSetRequest]) (*connect.Response[agentv1.RuntimeEndpointServiceSetResponse], error) {
	if request.Msg.ProtocolVersion != 1 || request.Msg.Endpoint == nil || s.Runtime == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid runtime endpoint request"))
	}
	_, err := s.Runtime.setEndpoint(request.Msg.Endpoint)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&agentv1.RuntimeEndpointServiceSetResponse{}), nil
}

func (s *RuntimeEndpointServer) Delete(_ context.Context, request *connect.Request[agentv1.RuntimeEndpointServiceDeleteRequest]) (*connect.Response[agentv1.RuntimeEndpointServiceDeleteResponse], error) {
	if request.Msg.ProtocolVersion != 1 || s.Runtime == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid runtime endpoint request"))
	}
	_, err := s.Runtime.deleteEndpoint(request.Msg.EndpointId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&agentv1.RuntimeEndpointServiceDeleteResponse{}), nil
}

func (r *Runtime) TargetChanges() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.targetChanged == nil {
		r.targetChanged = make(chan struct{}, 1)
	}
	return r.targetChanged
}

func (r *Runtime) CurrentAccessTarget() *agentv1.AccessTarget {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.target == nil {
		return nil
	}
	return proto.Clone(r.target).(*agentv1.AccessTarget)
}

func (r *Runtime) listEndpoints() ([]*codespacev1.RuntimeEndpoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadEndpointsLocked(); err != nil {
		return nil, err
	}
	return cloneEndpoints(r.endpoints), nil
}

func (r *Runtime) setEndpoint(endpoint *codespacev1.RuntimeEndpoint) ([]*codespacev1.RuntimeEndpoint, error) {
	if err := validateRuntimeEndpoint(endpoint); err != nil {
		return nil, err
	}
	r.mu.Lock()
	if err := r.loadEndpointsLocked(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	updated := slices.DeleteFunc(cloneEndpoints(r.endpoints), func(item *codespacev1.RuntimeEndpoint) bool {
		return item.EndpointId == endpoint.EndpointId
	})
	updated = append(updated, proto.Clone(endpoint).(*codespacev1.RuntimeEndpoint))
	sortRuntimeEndpoints(updated)
	if len(updated) > runtimeendpoint.MaxDeclaredEndpointCount {
		r.mu.Unlock()
		return nil, fmt.Errorf("endpoint count exceeds %d", runtimeendpoint.MaxDeclaredEndpointCount)
	}
	if err := r.persistEndpointsLocked(updated); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	target, changed, err := r.updateTargetEndpointsLocked()
	result := cloneEndpoints(r.endpoints)
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if changed {
		r.notifyTargetChanged(target)
	}
	return result, nil
}

func (r *Runtime) deleteEndpoint(endpointID string) ([]*codespacev1.RuntimeEndpoint, error) {
	endpointID = strings.TrimSpace(endpointID)
	if endpointID == "" || endpointID == runtimeendpoint.WorkspaceEndpointID {
		return nil, fmt.Errorf("endpoint ID is invalid")
	}
	r.mu.Lock()
	if err := r.loadEndpointsLocked(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	updated := slices.DeleteFunc(cloneEndpoints(r.endpoints), func(item *codespacev1.RuntimeEndpoint) bool {
		return item.EndpointId == endpointID
	})
	if len(updated) == len(r.endpoints) {
		result := cloneEndpoints(r.endpoints)
		r.mu.Unlock()
		return result, nil
	}
	if err := r.persistEndpointsLocked(updated); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	target, changed, err := r.updateTargetEndpointsLocked()
	result := cloneEndpoints(r.endpoints)
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if changed {
		r.notifyTargetChanged(target)
	}
	return result, nil
}

func (r *Runtime) replaceConfiguredEndpoints(endpoints []*codespacev1.RuntimeEndpoint) error {
	for _, endpoint := range endpoints {
		if err := validateRuntimeEndpoint(endpoint); err != nil {
			return err
		}
	}
	if len(endpoints) > runtimeendpoint.MaxDeclaredEndpointCount {
		return fmt.Errorf("endpoint count exceeds %d", runtimeendpoint.MaxDeclaredEndpointCount)
	}
	endpoints = cloneEndpoints(endpoints)
	sortRuntimeEndpoints(endpoints)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.persistEndpointsLocked(endpoints)
}

func (r *Runtime) loadEndpointsLocked() error {
	if r.endpointsLoaded {
		return nil
	}
	data, err := r.Journal.read(endpointStatePath, 64*1024)
	if errors.Is(err, os.ErrNotExist) {
		r.endpointsLoaded = true
		return nil
	}
	if err != nil {
		return err
	}
	state := &agentv1.RuntimeEndpointServiceListResponse{}
	if err := proto.Unmarshal(data, state); err != nil || len(state.Endpoints) > runtimeendpoint.MaxDeclaredEndpointCount {
		return fmt.Errorf("runtime endpoint state is invalid")
	}
	for _, endpoint := range state.Endpoints {
		if err := validateRuntimeEndpoint(endpoint); err != nil {
			return fmt.Errorf("runtime endpoint state is invalid: %w", err)
		}
	}
	r.endpoints = cloneEndpoints(state.Endpoints)
	sortRuntimeEndpoints(r.endpoints)
	r.endpointsLoaded = true
	return nil
}

func (r *Runtime) persistEndpointsLocked(endpoints []*codespacev1.RuntimeEndpoint) error {
	data, err := proto.Marshal(&agentv1.RuntimeEndpointServiceListResponse{Endpoints: endpoints})
	if err != nil {
		return err
	}
	if err := r.Journal.write(endpointStatePath, data); err != nil {
		return err
	}
	r.endpoints = cloneEndpoints(endpoints)
	r.endpointsLoaded = true
	return nil
}

func (r *Runtime) updateTargetEndpointsLocked() (*agentv1.AccessTarget, bool, error) {
	if r.target == nil || !r.target.Ready || r.target.PrimaryContainerId == "" {
		return nil, false, nil
	}
	if r.target.Version == int64(^uint64(0)>>1) {
		return nil, false, fmt.Errorf("runtime endpoint target version is exhausted")
	}
	target := accessTargetForContainer(r.target.Version+1, r.target.PrimaryContainerId, r.endpoints)
	r.target = proto.Clone(target).(*agentv1.AccessTarget)
	return target, true, nil
}

func (r *Runtime) notifyTargetChanged(target *agentv1.AccessTarget) {
	r.mu.Lock()
	changed := r.targetChanged
	r.mu.Unlock()
	if changed == nil || target == nil {
		return
	}
	select {
	case changed <- struct{}{}:
	default:
	}
}

func validateRuntimeEndpoint(endpoint *codespacev1.RuntimeEndpoint) error {
	if endpoint == nil || endpoint.Port == 0 || endpoint.Port > 65535 || endpoint.EndpointId != runtimeendpoint.PortEndpointID(uint16(endpoint.Port)) {
		return fmt.Errorf("endpoint identity or port is invalid")
	}
	if err := runtimeendpoint.ValidateLabel(endpoint.Label); err != nil {
		return err
	}
	return nil
}

func sortRuntimeEndpoints(endpoints []*codespacev1.RuntimeEndpoint) {
	slices.SortFunc(endpoints, func(left, right *codespacev1.RuntimeEndpoint) int {
		if left.Port != right.Port {
			return int(left.Port) - int(right.Port)
		}
		return strings.Compare(left.EndpointId, right.EndpointId)
	})
}

func cloneEndpoints(endpoints []*codespacev1.RuntimeEndpoint) []*codespacev1.RuntimeEndpoint {
	result := make([]*codespacev1.RuntimeEndpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		result = append(result, proto.Clone(endpoint).(*codespacev1.RuntimeEndpoint))
	}
	return result
}
