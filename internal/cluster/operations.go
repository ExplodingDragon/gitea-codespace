// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Operations serializes a site's Gitea replies within the elected Manager term.
type Operations struct {
	Client              client.Client
	Authority           *ExecutionAuthority
	Samples             *ResourceSamples
	Activity            *RuntimeActivityTracker
	ManagementNamespace string
	PlatformImage       string
}

// Accept persists identity and inputs before binding or granting execution.
func (o *Operations) Accept(ctx context.Context, site *api.GiteaSite, remote codespacev1connect.ManagerServiceClient, payload *codespacev1.OperationPayload, sent time.Time) error {
	if payload == nil || payload.CodespaceId <= 0 || payload.OperationRversion <= 0 || payload.LogOffset < 0 {
		return fmt.Errorf("invalid Gitea operation identity or offset")
	}
	var kind string
	var settings *codespacev1.EffectiveCodespaceRuntimeSettings
	switch command := payload.Command.(type) {
	case *codespacev1.OperationPayload_Create:
		kind = "create"
		if command.Create != nil {
			settings = command.Create.RuntimeSettings
		}
	case *codespacev1.OperationPayload_Resume:
		kind = "resume"
		if command.Resume != nil {
			settings = command.Resume.RuntimeSettings
		}
	case *codespacev1.OperationPayload_Stop:
		kind = "stop"
	case *codespacev1.OperationPayload_Delete:
		kind = "delete"
	case *codespacev1.OperationPayload_AbortCreate:
		kind = "abort_create"
	case *codespacev1.OperationPayload_AbortResume:
		kind = "abort_resume"
	default:
		return fmt.Errorf("unknown Gitea operation")
	}
	if (kind == "create" || kind == "resume") && validateRuntimeSettings(settings) != nil {
		return fmt.Errorf("%s operation has invalid effective Runtime settings", kind)
	}
	if kind != "create" && payload.RuntimeUuid == "" {
		return fmt.Errorf("operation requires the bound runtime UUID")
	}
	namespace, err := api.SiteNamespace(site.Name, o.ManagementNamespace)
	if err != nil {
		return err
	}
	var ns corev1.Namespace
	if err := o.Client.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
		return err
	}
	if ns.UID != site.Status.NamespaceUID || ns.Labels[SiteUIDLabel] != string(site.UID) || !ns.DeletionTimestamp.IsZero() {
		return fmt.Errorf("site namespace ownership changed")
	}
	key := types.NamespacedName{Namespace: namespace, Name: fmt.Sprintf("codespace-%d", payload.CodespaceId)}
	var cs api.Codespace
	err = o.Client.Get(ctx, key, &cs)
	isNew := apierrors.IsNotFound(err)
	if err != nil && !isNew {
		return err
	}
	if isNew {
		if payload.RuntimeUuid != "" {
			return fmt.Errorf("bound runtime record is missing; data recovery is required")
		}
		if kind != "create" || payload.GetCreate() == nil {
			return fmt.Errorf("%s requires an existing runtime record", kind)
		}
		var selected *api.EnvironmentTemplate
		for _, ref := range site.Spec.Templates {
			var template api.EnvironmentTemplate
			if err := o.Client.Get(ctx, types.NamespacedName{Name: ref.Name}, &template); err != nil {
				return err
			}
			if template.UID != ref.UID || !template.DeletionTimestamp.IsZero() {
				return fmt.Errorf("environment template identity changed")
			}
			if template.Spec.Tag != payload.GetCreate().EnvironmentTag {
				continue
			}
			ready := meta.FindStatusCondition(template.Status.Conditions, "Ready")
			if selected != nil || ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != template.Generation {
				return fmt.Errorf("create environment is ambiguous or not verified")
			}
			selected = &template
		}
		if selected == nil {
			return fmt.Errorf("create environment is not authorized by this site")
		}
		cs = api.Codespace{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{SiteUIDLabel: string(site.UID)}, Finalizers: []string{RuntimeFinalizer}},
			Spec: api.CodespaceSpec{
				Site: api.ResourceReference{Name: site.Name, UID: site.UID}, CodespaceID: payload.CodespaceId, RuntimeUUID: payload.RuntimeUuid, EnvironmentTag: selected.Spec.Tag,
				Runtime: api.RuntimeConfiguration{EnvironmentConfiguration: selected.Spec.Runtime, Image: o.PlatformImage},
			},
		}
		if cs.Spec.RuntimeUUID == "" {
			cs.Spec.RuntimeUUID = uuid.NewString()
		}
	} else {
		if cs.Spec.Site.UID != site.UID || cs.Spec.CodespaceID != payload.CodespaceId || !cs.DeletionTimestamp.IsZero() || cs.Status.RecoveryAction == "delete" {
			return fmt.Errorf("runtime ownership or deletion conflicts with operation")
		}
		if payload.OperationRversion < cs.Spec.Operation.Version || (payload.RuntimeUuid != "" && payload.RuntimeUuid != cs.Spec.RuntimeUUID) {
			return fmt.Errorf("gitea operation identity regressed or changed")
		}
	}
	input := proto.Clone(payload).(*codespacev1.OperationPayload)
	input.RuntimeUuid = cs.Spec.RuntimeUUID
	// Relative grants are valid only in this process; persisted input is not authority.
	input.LeaseValidForMilliseconds = 0
	encoded, err := protojson.Marshal(input)
	if err != nil {
		return err
	}
	operation := api.Operation{Type: kind, Version: input.OperationRversion, Payload: runtime.RawExtension{Raw: encoded}}
	newVersion := operation.Version > cs.Spec.Operation.Version
	changed := isNew || operation.Version != cs.Spec.Operation.Version || operation.Type != cs.Spec.Operation.Type
	if !isNew && operation.Version == cs.Spec.Operation.Version {
		previous := &codespacev1.OperationPayload{}
		if err := protojson.Unmarshal(cs.Spec.Operation.Payload.Raw, previous); err != nil {
			return fmt.Errorf("decode saved operation: %w", err)
		}
		previous.LogOffset = input.LogOffset
		aborting := (kind == "abort_create" && cs.Spec.Operation.Type == "create") || (kind == "abort_resume" && cs.Spec.Operation.Type == "resume")
		if !aborting && !proto.Equal(previous, input) {
			return fmt.Errorf("gitea changed an already persisted operation")
		}
	}
	if changed {
		o.Authority.Revoke(cs.UID)
		cs.Spec.Operation = operation
		if err := cs.Validate(o.ManagementNamespace); err != nil {
			return err
		}
		if isNew {
			err = o.Client.Create(ctx, &cs)
		} else {
			err = o.Client.Update(ctx, &cs)
		}
		if err != nil {
			return err
		}
	}
	if !cs.Status.Bound {
		if kind == "create" {
			if err := o.bind(ctx, &cs, remote); err != nil {
				return err
			}
		} else {
			if payload.RuntimeUuid != cs.Spec.RuntimeUUID {
				return fmt.Errorf("a subsequent operation must confirm the persisted runtime identity")
			}
			cs.Status.Bound = true
			if err := o.Client.Status().Update(ctx, &cs); err != nil {
				return err
			}
		}
	}
	if cs.Status.RecoveryAction == "stop" && newVersion {
		cs.Status.RecoveryAction = ""
		if err := o.Client.Status().Update(ctx, &cs); err != nil {
			return err
		}
	}
	if kind == "abort_create" || kind == "abort_resume" || cs.Status.RecoveryAction != "" {
		o.Authority.Revoke(cs.UID)
	} else if payload.LeaseValidForMilliseconds > 0 && payload.LeaseValidForMilliseconds <= int64((24*time.Hour)/time.Millisecond) && cs.Status.SettledOperationVersion < cs.Spec.Operation.Version {
		o.Authority.Grant(cs.UID, cs.Spec.Operation.Version, sent.Add(time.Duration(payload.LeaseValidForMilliseconds)*time.Millisecond))
	}
	if o.Activity != nil {
		if settings != nil {
			return o.Activity.ObserveSettings(string(site.UID), cs.Spec.RuntimeUUID, settings, time.Now())
		}
	}
	return nil
}

func (o *Operations) bind(ctx context.Context, cs *api.Codespace, remote codespacev1connect.ManagerServiceClient) error {
	if cs.Spec.Operation.Type != "create" || !cs.DeletionTimestamp.IsZero() {
		return fmt.Errorf("unbound runtime has no create operation")
	}
	response, err := remote.BindRuntimeIdentity(ctx, connect.NewRequest(&codespacev1.BindRuntimeIdentityRequest{ProtocolVersion: 1, CodespaceId: cs.Spec.CodespaceID, RuntimeUuid: cs.Spec.RuntimeUUID, OperationRversion: cs.Spec.Operation.Version}))
	if err != nil {
		return fmt.Errorf("bind persisted runtime identity: %w", err)
	}
	if response.Msg.RuntimeUuid != cs.Spec.RuntimeUUID {
		return fmt.Errorf("gitea binding conflicts with persisted runtime identity")
	}
	cs.Status.Bound = true
	return o.Client.Status().Update(ctx, cs)
}

// Flush publishes ready metadata before final and keeps acknowledgements durable.
func (o *Operations) Flush(ctx context.Context, cs *api.Codespace, remote codespacev1connect.ManagerServiceClient) error {
	if !cs.Status.Bound || !cs.DeletionTimestamp.IsZero() {
		return nil
	}
	if cs.Status.RecoveryAction != "" {
		return nil
	}
	kind := cs.Spec.Operation.Type
	startup := kind == "create" || kind == "resume"
	if startup && cs.Status.SettledOperationVersion < cs.Spec.Operation.Version && cs.Status.Result != nil && cs.Status.Result.Version == cs.Spec.Operation.Version && cs.Status.Result.Succeeded && (cs.Status.Target == nil || !cs.Status.Target.Ready || cs.Status.Boot == nil) {
		return fmt.Errorf("successful startup has no persisted ready target and metadata")
	}
	if startup && cs.Status.Boot != nil && (cs.Status.Result == nil || cs.Status.Result.Version != cs.Spec.Operation.Version || cs.Status.Result.Succeeded) {
		if cs.Status.Boot.OperationVersion == cs.Spec.Operation.Version {
			if cs.Status.MetadataGeneration <= 0 {
				return fmt.Errorf("persisted runtime metadata has no generation")
			}
			metadata, err := runtimeMetadata(cs)
			if err != nil {
				return fmt.Errorf("build runtime metadata: %w", err)
			}
			metadata.ResourceUsage = o.Samples.Load(cs)
			_, err = remote.ReportRuntimeMetadata(ctx, connect.NewRequest(&codespacev1.ReportRuntimeMetadataRequest{ProtocolVersion: 1, RuntimeUuid: cs.Spec.RuntimeUUID, MetadataGeneration: cs.Status.MetadataGeneration, Metadata: metadata}))
			if err != nil {
				return err
			}
		}
	}
	result := cs.Status.Result
	failedStartup := startup && result != nil && result.Version == cs.Spec.Operation.Version && !result.Succeeded
	if !startup || failedStartup {
		stopped := meta.FindStatusCondition(cs.Status.Conditions, "Stopped")
		if stopped == nil || stopped.Status != metav1.ConditionTrue || stopped.ObservedGeneration != cs.Generation || cs.Status.Pod.Name != "" {
			return nil
		}
		if result == nil || result.Version != cs.Spec.Operation.Version || (strings.HasPrefix(kind, "abort_") && result.Succeeded) {
			result = &api.OperationResult{Version: cs.Spec.Operation.Version, Succeeded: kind == "stop" || kind == "delete"}
			cs.Status.Result = result
			if err := o.Client.Status().Update(ctx, cs); err != nil {
				return err
			}
		}
	}
	if result == nil || result.Version != cs.Spec.Operation.Version || cs.Status.SettledOperationVersion >= result.Version {
		return nil
	}
	var operationType codespacev1.OperationType
	switch kind {
	case "create", "abort_create":
		operationType = codespacev1.OperationType_OPERATION_TYPE_CREATE
	case "resume", "abort_resume":
		operationType = codespacev1.OperationType_OPERATION_TYPE_RESUME
	case "stop":
		operationType = codespacev1.OperationType_OPERATION_TYPE_STOP
	case "delete":
		operationType = codespacev1.OperationType_OPERATION_TYPE_DELETE
	default:
		return fmt.Errorf("invalid saved operation type")
	}
	status := codespacev1.FinalStatus_FINAL_STATUS_FAILED
	if result.Succeeded {
		status = codespacev1.FinalStatus_FINAL_STATUS_DONE
	}
	response, err := remote.FinalizeOperation(ctx, connect.NewRequest(&codespacev1.FinalizeOperationRequest{ProtocolVersion: 1, RuntimeUuid: cs.Spec.RuntimeUUID, OperationRversion: result.Version, OperationType: operationType, Status: status}))
	if err != nil {
		return err
	}
	o.Authority.Revoke(cs.UID)
	cs.Status.SettledOperationVersion = result.Version
	if response.Msg.ResourceAbsent {
		// Only a complete inventory response authorizes cleanup of an absent resource.
		cs.Status.RecoveryAction = "stop"
		cs.Status.Target = nil
		cs.Status.Boot = nil
	} else if kind == "delete" || (!result.Succeeded && (kind == "create" || kind == "abort_create")) {
		cs.Status.RecoveryAction = "delete"
	} else if !result.Succeeded {
		// A failed resume returns to stopped and retains the existing workspace.
		cs.Status.RecoveryAction = "stop"
	}
	return o.Client.Status().Update(ctx, cs)
}
