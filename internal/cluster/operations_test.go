// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type operationRemote struct {
	codespacev1connect.ManagerServiceClient
	bind     func(*codespacev1.BindRuntimeIdentityRequest) (*codespacev1.BindRuntimeIdentityResponse, error)
	final    func(*codespacev1.FinalizeOperationRequest) (*codespacev1.FinalizeOperationResponse, error)
	metadata func(*codespacev1.ReportRuntimeMetadataRequest) error
}

func (r *operationRemote) BindRuntimeIdentity(_ context.Context, request *connect.Request[codespacev1.BindRuntimeIdentityRequest]) (*connect.Response[codespacev1.BindRuntimeIdentityResponse], error) {
	response, err := r.bind(request.Msg)
	return connect.NewResponse(response), err
}

func (r *operationRemote) FinalizeOperation(_ context.Context, request *connect.Request[codespacev1.FinalizeOperationRequest]) (*connect.Response[codespacev1.FinalizeOperationResponse], error) {
	response, err := r.final(request.Msg)
	return connect.NewResponse(response), err
}

func (r *operationRemote) ReportRuntimeMetadata(_ context.Context, request *connect.Request[codespacev1.ReportRuntimeMetadataRequest]) (*connect.Response[codespacev1.ReportRuntimeMetadataResponse], error) {
	return connect.NewResponse(&codespacev1.ReportRuntimeMetadataResponse{}), r.metadata(request.Msg)
}

func TestOperationBindingRecovery(t *testing.T) {
	site := testSite("example", "site-uid")
	site.Status.NamespaceUID = "namespace-uid"
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "codespace-example", UID: site.Status.NamespaceUID, Labels: map[string]string{SiteUIDLabel: string(site.UID)}}}
	template := &api.EnvironmentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "template-uid", Generation: 1}, Spec: api.EnvironmentTemplateSpec{Tag: "standard", Runtime: testEnvironment()}}
	meta.SetStatusCondition(&template.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Verified", ObservedGeneration: 1})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&api.Codespace{}).WithObjects(template, namespace).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			object.SetUID("runtime-uid")
			return c.Create(ctx, object, opts...)
		}}).Build()
	o := Operations{Client: c, Authority: &ExecutionAuthority{}, ManagementNamespace: "codespace-system", PlatformImage: testRuntime().Image}
	key := types.NamespacedName{Namespace: "codespace-example", Name: "codespace-1"}
	var allocated string
	attempts := 0
	remote := &operationRemote{bind: func(request *codespacev1.BindRuntimeIdentityRequest) (*codespacev1.BindRuntimeIdentityResponse, error) {
		var cs api.Codespace
		require.NoError(t, c.Get(t.Context(), key, &cs))
		require.Equal(t, cs.Spec.RuntimeUUID, request.RuntimeUuid)
		attempts++
		if attempts == 1 {
			allocated = request.RuntimeUuid
			return nil, fmt.Errorf("binding response lost")
		}
		require.Equal(t, allocated, request.RuntimeUuid)
		return &codespacev1.BindRuntimeIdentityResponse{RuntimeUuid: allocated}, nil
	}}
	invalid := &codespacev1.OperationPayload{CodespaceId: 2, OperationRversion: 1, Command: &codespacev1.OperationPayload_Create{Create: &codespacev1.CreateOperationPayload{EnvironmentTag: "standard"}}}
	require.ErrorContains(t, o.Accept(t.Context(), site, remote, invalid, time.Now()), "invalid effective Runtime settings")
	require.True(t, apierrors.IsNotFound(c.Get(t.Context(), types.NamespacedName{Namespace: "codespace-example", Name: "codespace-2"}, &api.Codespace{})))

	input := &codespacev1.OperationPayload{CodespaceId: 1, OperationRversion: 1, LeaseValidForMilliseconds: 60000, Command: &codespacev1.OperationPayload_Create{Create: &codespacev1.CreateOperationPayload{EnvironmentTag: "standard", RuntimeSettings: &codespacev1.EffectiveCodespaceRuntimeSettings{}}}}
	require.ErrorContains(t, o.Accept(t.Context(), site, remote, input, time.Now()), "binding response lost")
	var cs api.Codespace
	require.NoError(t, c.Get(t.Context(), key, &cs))
	require.False(t, cs.Status.Bound)
	require.Zero(t, o.Authority.Remaining(cs.UID, 1))
	// Recovery uses the persisted UUID even if the original payload had none.
	require.NoError(t, o.Accept(t.Context(), site, remote, input, time.Now()))
	require.NoError(t, c.Get(t.Context(), key, &cs))
	require.True(t, cs.Status.Bound)
	require.Positive(t, o.Authority.Remaining(cs.UID, 1))
	saved := &codespacev1.OperationPayload{}
	require.NoError(t, protojson.Unmarshal(cs.Spec.Operation.Payload.Raw, saved))
	require.Zero(t, saved.LeaseValidForMilliseconds)
	require.Empty(t, input.RuntimeUuid)
	cs.Status.RecoveryAction = "stop"
	require.NoError(t, c.Status().Update(t.Context(), &cs))
	require.NoError(t, o.Accept(t.Context(), site, remote, input, time.Now()))
	require.NoError(t, c.Get(t.Context(), key, &cs))
	require.Equal(t, "stop", cs.Status.RecoveryAction)
	require.Zero(t, o.Authority.Remaining(cs.UID, 1))
	changed := proto.Clone(input).(*codespacev1.OperationPayload)
	changed.GetCreate().EnvironmentTag = "another"
	require.ErrorContains(t, o.Accept(t.Context(), site, remote, changed, time.Now()), "changed an already persisted operation")
	abort := &codespacev1.OperationPayload{CodespaceId: 1, RuntimeUuid: allocated, OperationRversion: 1, Command: &codespacev1.OperationPayload_AbortCreate{AbortCreate: &codespacev1.AbortCreateOperationPayload{}}}
	require.NoError(t, o.Accept(t.Context(), site, remote, abort, time.Now()))
	require.Zero(t, o.Authority.Remaining(cs.UID, 1))
	require.ErrorContains(t, o.Accept(t.Context(), site, remote, input, time.Now()), "changed an already persisted operation")
	resume := &codespacev1.OperationPayload{CodespaceId: 1, RuntimeUuid: allocated, OperationRversion: 2, LeaseValidForMilliseconds: 60000, Command: &codespacev1.OperationPayload_Resume{Resume: &codespacev1.ResumeOperationPayload{RuntimeSettings: &codespacev1.EffectiveCodespaceRuntimeSettings{}}}}
	require.NoError(t, o.Accept(t.Context(), site, remote, resume, time.Now()))
	require.NoError(t, c.Get(t.Context(), key, &cs))
	require.Empty(t, cs.Status.RecoveryAction)
}

func TestOperationFinalRequiresReadyAndStoppedFacts(t *testing.T) {
	cs := &api.Codespace{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-example", UID: "runtime-uid", Generation: 2}, Spec: api.CodespaceSpec{RuntimeUUID: "933ef4a9-54c7-4f36-aad9-32ea72c4c986", Operation: api.Operation{Type: "create", Version: 1}}, Status: api.CodespaceStatus{Bound: true, Result: &api.OperationResult{Version: 1, Succeeded: true}}}
	cs.Status.Target = &api.AgentTarget{Version: 1, Ready: true, PrimaryContainerID: "container"}
	cs.Status.Boot = &api.RuntimeBoot{OperationVersion: 1, Stage: "ready", StartedUnix: 1, LastUpdateUnix: 2}
	cs.Status.MetadataGeneration = 1
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(cs).WithObjects(cs).Build()
	o := Operations{Client: c, Authority: &ExecutionAuthority{}}
	var events []string
	finalAttempts := 0
	remote := &operationRemote{
		metadata: func(request *codespacev1.ReportRuntimeMetadataRequest) error {
			events = append(events, "metadata")
			var current api.Codespace
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cs), &current))
			require.Equal(t, current.Status.MetadataGeneration, request.MetadataGeneration)
			require.EqualValues(t, 1, request.MetadataGeneration)
			return nil
		},
		final: func(request *codespacev1.FinalizeOperationRequest) (*codespacev1.FinalizeOperationResponse, error) {
			events = append(events, "final")
			finalAttempts++
			if finalAttempts == 1 {
				return nil, fmt.Errorf("final response lost")
			}
			return &codespacev1.FinalizeOperationResponse{}, nil
		},
	}
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cs), cs))
	require.ErrorContains(t, o.Flush(t.Context(), cs, remote), "final response lost")
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cs), cs))
	require.Zero(t, cs.Status.SettledOperationVersion)
	require.NoError(t, o.Flush(t.Context(), cs, remote))
	require.Equal(t, []string{"metadata", "final", "metadata", "final"}, events)
	require.EqualValues(t, 1, cs.Status.SettledOperationVersion)

	cs.Spec.Operation = api.Operation{Type: "stop", Version: 2}
	require.NoError(t, c.Update(t.Context(), cs))
	cs.Status.Result = &api.OperationResult{Version: 2, Succeeded: true}
	require.NoError(t, c.Status().Update(t.Context(), cs))
	require.NoError(t, o.Flush(t.Context(), cs, remote))
	require.Equal(t, 2, finalAttempts)
	meta.SetStatusCondition(&cs.Status.Conditions, metav1.Condition{Type: "Stopped", Status: metav1.ConditionTrue, Reason: "ResourcesStopped", ObservedGeneration: cs.Generation})
	require.NoError(t, c.Status().Update(t.Context(), cs))
	require.NoError(t, o.Flush(t.Context(), cs, remote))
	require.Equal(t, 3, finalAttempts)
}

func TestStartupFailureWaitsForStoppedWriter(t *testing.T) {
	for _, kind := range []string{"create", "resume", "abort_create", "abort_resume"} {
		t.Run(kind, func(t *testing.T) {
			cs := &api.Codespace{
				ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-example", UID: "runtime-uid", Generation: 2},
				Spec:       api.CodespaceSpec{RuntimeUUID: "933ef4a9-54c7-4f36-aad9-32ea72c4c986", Operation: api.Operation{Type: kind, Version: 3}},
				Status:     api.CodespaceStatus{Bound: true, Pod: api.ObservedResource{Name: "pod", UID: "pod-uid"}, Result: &api.OperationResult{Version: 3, Message: "startup failed"}},
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(cs).WithObjects(cs).Build()
			o := Operations{Client: c, Authority: &ExecutionAuthority{}}
			attempts := 0
			remote := &operationRemote{final: func(request *codespacev1.FinalizeOperationRequest) (*codespacev1.FinalizeOperationResponse, error) {
				attempts++
				require.Equal(t, codespacev1.FinalStatus_FINAL_STATUS_FAILED, request.Status)
				return &codespacev1.FinalizeOperationResponse{}, nil
			}}
			require.NoError(t, o.Flush(t.Context(), cs, remote))
			require.Zero(t, attempts)
			meta.SetStatusCondition(&cs.Status.Conditions, metav1.Condition{Type: "Stopped", Status: metav1.ConditionTrue, Reason: "ResourcesStopped", ObservedGeneration: cs.Generation})
			require.NoError(t, c.Status().Update(t.Context(), cs))
			require.NoError(t, o.Flush(t.Context(), cs, remote))
			require.Zero(t, attempts)
			cs.Status.Pod = api.ObservedResource{}
			require.NoError(t, c.Status().Update(t.Context(), cs))
			require.NoError(t, o.Flush(t.Context(), cs, remote))
			require.Equal(t, 1, attempts)
			require.EqualValues(t, 3, cs.Status.SettledOperationVersion)
			if kind == "create" || kind == "abort_create" {
				require.Equal(t, "delete", cs.Status.RecoveryAction)
			} else {
				require.Equal(t, "stop", cs.Status.RecoveryAction)
			}
		})
	}
}

func TestFinalResourceAbsentWaitsForInventory(t *testing.T) {
	cs := &api.Codespace{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-example", UID: "runtime-uid"},
		Spec:       api.CodespaceSpec{Operation: api.Operation{Type: "resume", Version: 2}},
		Status:     api.CodespaceStatus{Bound: true, Result: &api.OperationResult{Version: 2}, Volume: api.ObservedResource{Name: "data", UID: "volume-uid"}},
	}
	meta.SetStatusCondition(&cs.Status.Conditions, metav1.Condition{Type: "Stopped", Status: metav1.ConditionTrue, Reason: "ResourcesStopped"})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(cs).WithObjects(cs).Build()
	o := Operations{Client: c, Authority: &ExecutionAuthority{}}
	remote := &operationRemote{final: func(*codespacev1.FinalizeOperationRequest) (*codespacev1.FinalizeOperationResponse, error) {
		return &codespacev1.FinalizeOperationResponse{ResourceAbsent: true}, nil
	}}
	require.NoError(t, o.Flush(t.Context(), cs, remote))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cs), cs))
	require.Equal(t, "stop", cs.Status.RecoveryAction)
	require.EqualValues(t, 2, cs.Status.SettledOperationVersion)
	require.Equal(t, types.UID("volume-uid"), cs.Status.Volume.UID)
	require.NoError(t, o.Flush(t.Context(), cs, remote))
}
