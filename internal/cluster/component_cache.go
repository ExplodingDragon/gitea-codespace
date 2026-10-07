// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	agentv1 "gitea.dev/codespace-proto-go/agent/v1"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	componentv1 "gitea.dev/codespace-proto-go/component/v1"
	cachepkg "gitea.dev/codespace/internal/cache"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	componentpkg "gitea.dev/codespace/internal/component"
	configpkg "gitea.dev/codespace/internal/config"
	"github.com/google/uuid"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	cacheCredentialTTL = time.Hour
	cacheOwnerTTL      = 15 * time.Second
	cacheGCTTL         = 10 * time.Minute
)

type cacheMaterial struct {
	component *corev1.ConfigMap
	config    configpkg.CacheConfig
	secret    *corev1.Secret
}

func (s *ComponentServer) runtimeCache(ctx context.Context, cs *api.Codespace, operation *codespacev1.OperationPayload) (*agentv1.RuntimeCache, error) {
	create := operation.GetCreate()
	if create == nil || create.Repository == nil || create.GitIdentity == nil {
		return nil, nil
	}
	var site api.GiteaSite
	if err := s.Client.Get(ctx, types.NamespacedName{Name: cs.Spec.Site.Name}, &site); err != nil {
		return nil, err
	}
	if site.UID != cs.Spec.Site.UID || !site.Spec.Enabled {
		return nil, fmt.Errorf("site identity is no longer current")
	}
	for _, reference := range site.Spec.Caches {
		material, err := s.cacheByReference(ctx, reference)
		if err != nil || !material.config.Enabled {
			continue
		}
		available, err := s.cacheAvailable(ctx, material)
		if err != nil || !available {
			continue
		}
		namespace := cachepkg.Namespace(material.secret.Data["registryKey"], site.Spec.ManagerID, create.Repository.RepositoryId, create.GitIdentity.UserId)
		token, err := cachepkg.SignCredential(material.secret.Data["tokenKey"], material.component.Name, namespace, configpkg.CachePolicy{Public: true, Build: true}, time.Now().Add(cacheCredentialTTL))
		if err != nil {
			return nil, err
		}
		port, err := componentListenPort(material.config.Listen)
		if err != nil {
			return nil, fmt.Errorf("cache listen address is invalid: %w", err)
		}
		registryURL := fmt.Sprintf("http://%s.%s.svc:%d", material.component.Name, s.ManagementNamespace, port)
		result := &agentv1.RuntimeCache{
			BuildRegistry: registryURL + "/cache/" + namespace,
			Mirrors:       make(map[string]string, len(material.config.Upstreams)),
			Credentials:   make(map[string]*agentv1.RegistryCredential, 1),
		}
		for upstream := range material.config.Upstreams {
			result.Mirrors[upstream] = registryURL + "/mirror/" + upstream
		}
		address := strings.TrimPrefix(registryURL, "http://")
		if slash := strings.IndexByte(address, '/'); slash >= 0 {
			address = address[:slash]
		}
		result.Credentials[address] = &agentv1.RegistryCredential{Username: cachepkg.Username, Password: token}
		digest := sha256.New()
		_, _ = fmt.Fprintf(digest, "%s\x00%d\x00%d\x00%s\x00", site.UID, create.Repository.RepositoryId, create.GitIdentity.UserId, cs.Spec.Runtime.CodeServerVersion)
		if create.DevContainer != nil {
			encoded, _ := json.Marshal(create.DevContainer)
			_, _ = digest.Write(encoded)
		}
		result.BuildScope = fmt.Sprintf("%x", digest.Sum(nil))
		return result, nil
	}
	return nil, nil
}

func (s *ComponentServer) cacheAvailable(ctx context.Context, material *cacheMaterial) (bool, error) {
	var lease coordinationv1.Lease
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.ManagementNamespace, Name: "cache-owner-" + material.component.Name}, &lease); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	holder := strings.Split(ptr.Deref(lease.Spec.HolderIdentity, ""), "/")
	duration := time.Duration(ptr.Deref(lease.Spec.LeaseDurationSeconds, 0)) * time.Second
	return lease.Labels[ComponentUIDLabel] == string(material.component.UID) && metav1.IsControlledBy(&lease, material.component) && lease.Spec.RenewTime != nil && duration > 0 && time.Now().Before(lease.Spec.RenewTime.Add(duration)) && len(holder) == 4 && holder[0] == string(material.component.UID) && holder[1] != "" && holder[2] != "" && holder[3] == strconv.FormatInt(material.config.Revision, 10), nil
}

func (s *ComponentServer) cacheByUID(ctx context.Context, uid string) (*cacheMaterial, error) {
	var components corev1.ConfigMapList
	if err := s.Client.List(ctx, &components, client.InNamespace(s.ManagementNamespace)); err != nil {
		return nil, err
	}
	for i := range components.Items {
		component := &components.Items[i]
		if string(component.UID) == uid && component.Labels[ComponentLabel] == "cache" && component.DeletionTimestamp.IsZero() {
			return s.cacheMaterial(ctx, component)
		}
	}
	return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("cache component identity is no longer configured"))
}

func (s *ComponentServer) cacheByReference(ctx context.Context, reference api.ResourceReference) (*cacheMaterial, error) {
	var component corev1.ConfigMap
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.ManagementNamespace, Name: reference.Name}, &component); err != nil {
		return nil, err
	}
	if component.UID != reference.UID || component.Labels[ComponentLabel] != "cache" || !component.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("cache reference is no longer current")
	}
	return s.cacheMaterial(ctx, &component)
}

func (s *ComponentServer) cacheMaterial(ctx context.Context, component *corev1.ConfigMap) (*cacheMaterial, error) {
	var config configpkg.CacheConfig
	decoder := json.NewDecoder(strings.NewReader(component.Data["config"]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("cache configuration is invalid")
	}
	config.ID = component.Name
	secretName, secretUID := component.Data["secretName"], component.Data["secretUID"]
	var secret corev1.Secret
	if secretName == "" || secretUID == "" || s.Client.Get(ctx, types.NamespacedName{Namespace: s.ManagementNamespace, Name: secretName}, &secret) != nil || string(secret.UID) != secretUID || secret.Labels[ComponentUIDLabel] != string(component.UID) || !secret.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("cache Secret identity is invalid")
	}
	if len(secret.Data["registryKey"]) < 32 || len(secret.Data["tokenKey"]) < 32 {
		return nil, fmt.Errorf("cache signing material is invalid")
	}
	if config.Storage.Driver == "s3" {
		config.Storage.S3.AccessKey = string(secret.Data["s3AccessKey"])
		config.Storage.S3.SecretKey = string(secret.Data["s3SecretKey"])
	}
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("cache configuration is invalid: %w", err)
	}
	return &cacheMaterial{component: component, config: config, secret: &secret}, nil
}

func (s *ComponentServer) cacheForRequest(ctx context.Context, cacheID string) (componentIdentity, *cacheMaterial, error) {
	identity, err := componentIdentityFromContext(ctx, "cache")
	if err != nil {
		return componentIdentity{}, nil, err
	}
	material, err := s.cacheByUID(ctx, identity.UID)
	if err != nil {
		return componentIdentity{}, nil, err
	}
	if cacheID != material.component.Name {
		return componentIdentity{}, nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("cache identity does not allow this resource"))
	}
	return identity, material, nil
}

func cacheOwnerHolder(identity componentIdentity, sessionID string, revision int64) string {
	return strings.Join([]string{identity.UID, identity.Instance, sessionID, strconv.FormatInt(revision, 10)}, "/")
}

func (s *ComponentServer) claimCache(ctx context.Context, identity componentIdentity, material *cacheMaterial, sessionID string) (*coordinationv1.Lease, error) {
	now := metav1.NowMicro()
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "cache-owner-" + material.component.Name, Namespace: s.ManagementNamespace}}
	err := s.Client.Get(ctx, client.ObjectKeyFromObject(lease), lease)
	if apierrors.IsNotFound(err) {
		lease.Labels = map[string]string{ComponentUIDLabel: identity.UID}
		lease.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(material.component, corev1.SchemeGroupVersion.WithKind("ConfigMap"))}
		lease.Spec = coordinationv1.LeaseSpec{HolderIdentity: ptr.To(cacheOwnerHolder(identity, sessionID, material.config.Revision)), LeaseDurationSeconds: ptr.To(int32(cacheOwnerTTL / time.Second)), AcquireTime: &now, RenewTime: &now}
		err = s.Client.Create(ctx, lease)
	} else if err == nil {
		active := lease.Spec.RenewTime != nil && time.Now().Before(lease.Spec.RenewTime.Add(time.Duration(ptr.Deref(lease.Spec.LeaseDurationSeconds, 0))*time.Second))
		if active {
			return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("cache storage already has an active writer"))
		}
		lease.Labels = map[string]string{ComponentUIDLabel: identity.UID}
		lease.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(material.component, corev1.SchemeGroupVersion.WithKind("ConfigMap"))}
		lease.Spec = coordinationv1.LeaseSpec{HolderIdentity: ptr.To(cacheOwnerHolder(identity, sessionID, material.config.Revision)), LeaseDurationSeconds: ptr.To(int32(cacheOwnerTTL / time.Second)), AcquireTime: &now, RenewTime: &now}
		err = s.Client.Update(ctx, lease)
	}
	return lease, err
}

func (s *ComponentServer) renewCache(ctx context.Context, identity componentIdentity, material *cacheMaterial, sessionID string, status *componentv1.CacheStatus) error {
	var lease coordinationv1.Lease
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.ManagementNamespace, Name: "cache-owner-" + material.component.Name}, &lease); err != nil {
		return err
	}
	holder := strings.Split(ptr.Deref(lease.Spec.HolderIdentity, ""), "/")
	if lease.Labels[ComponentUIDLabel] != identity.UID || len(holder) != 4 || holder[0] != identity.UID || holder[1] != identity.Instance || holder[2] != sessionID {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("cache owner lease is no longer current"))
	}
	now := metav1.NowMicro()
	lease.Spec.HolderIdentity = ptr.To(cacheOwnerHolder(identity, sessionID, material.config.Revision))
	lease.Spec.RenewTime = &now
	if status != nil {
		if lease.Annotations == nil {
			lease.Annotations = make(map[string]string)
		}
		lease.Annotations["codespace.gitea.dev/cache-bytes"] = strconv.FormatInt(status.CacheBytes, 10)
		lease.Annotations["codespace.gitea.dev/mirror-bytes"] = strconv.FormatInt(status.MirrorBytes, 10)
		lease.Annotations["codespace.gitea.dev/cleanup-result"] = status.CleanupResult
	}
	return s.Client.Update(ctx, &lease)
}

func (s *ComponentServer) releaseCache(ctx context.Context, identity componentIdentity, cacheID, sessionID string) {
	var lease coordinationv1.Lease
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.ManagementNamespace, Name: "cache-owner-" + cacheID}, &lease); err != nil {
		return
	}
	holder := strings.Split(ptr.Deref(lease.Spec.HolderIdentity, ""), "/")
	if lease.Labels[ComponentUIDLabel] == identity.UID && len(holder) == 4 && holder[1] == identity.Instance && holder[2] == sessionID {
		_ = s.Client.Delete(ctx, &lease, client.Preconditions{UID: &lease.UID, ResourceVersion: &lease.ResourceVersion})
	}
}

func (s *ComponentServer) beginCacheGC(ctx context.Context, identity componentIdentity, material *cacheMaterial, sessionID string) (*componentv1.CacheGCGrant, error) {
	grantID, now := uuid.NewString(), metav1.NowMicro()
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "cache-gc-" + material.component.Name, Namespace: s.ManagementNamespace, Labels: map[string]string{ComponentUIDLabel: identity.UID}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(material.component, corev1.SchemeGroupVersion.WithKind("ConfigMap"))}}, Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To(sessionID + "/" + grantID), LeaseDurationSeconds: ptr.To(int32(cacheGCTTL / time.Second)), AcquireTime: &now, RenewTime: &now}}
	if err := s.Client.Create(ctx, lease); apierrors.IsAlreadyExists(err) {
		var current coordinationv1.Lease
		if err := s.Client.Get(ctx, client.ObjectKeyFromObject(lease), &current); err != nil {
			return nil, err
		}
		active := current.Spec.RenewTime != nil && time.Now().Before(current.Spec.RenewTime.Add(time.Duration(ptr.Deref(current.Spec.LeaseDurationSeconds, 0))*time.Second))
		if active {
			return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("cache cleanup is already active"))
		}
		current.Labels, current.OwnerReferences, current.Spec = lease.Labels, lease.OwnerReferences, lease.Spec
		if err := s.Client.Update(ctx, &current); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return &componentv1.CacheGCGrant{CacheId: material.component.Name, Id: grantID, ConfigRevision: material.config.Revision, ExpiresAtUnixMilliseconds: now.Add(cacheGCTTL).UnixMilli()}, nil
}

func (s *ComponentServer) completeCacheGC(ctx context.Context, identity componentIdentity, cacheID, sessionID, grantID string) error {
	var lease coordinationv1.Lease
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.ManagementNamespace, Name: "cache-gc-" + cacheID}, &lease); err != nil {
		return err
	}
	if lease.Labels[ComponentUIDLabel] != identity.UID || ptr.Deref(lease.Spec.HolderIdentity, "") != sessionID+"/"+grantID {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("cache cleanup grant is no longer current"))
	}
	return client.IgnoreNotFound(s.Client.Delete(ctx, &lease, client.Preconditions{UID: &lease.UID, ResourceVersion: &lease.ResourceVersion}))
}

func (s *ComponentServer) CacheControl(ctx context.Context, stream *connect.BidiStream[componentv1.CacheControlRequest, componentv1.CacheControlResponse]) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	if first.ProtocolVersion != 1 || first.CacheId == "" || first.SessionId == "" || first.Sequence == 0 {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("cache control identity is invalid"))
	}
	identity, material, err := s.cacheForRequest(ctx, first.CacheId)
	if err != nil {
		return err
	}
	if _, err := s.claimCache(ctx, identity, material, first.SessionId); err != nil {
		return err
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.releaseCache(releaseCtx, identity, first.CacheId, first.SessionId)
	}()

	requests := make(chan *componentv1.CacheControlRequest)
	receiveErrors := make(chan error, 1)
	go func() {
		for {
			request, err := stream.Receive()
			if err != nil {
				receiveErrors <- err
				return
			}
			select {
			case requests <- request:
			case <-ctx.Done():
				return
			}
		}
	}()
	changes := (<-chan struct{})(nil)
	unsubscribe := func() {}
	if s.CacheChanges != nil {
		changes, unsubscribe = s.CacheChanges.Subscribe()
	}
	defer unsubscribe()
	permitTicker := time.NewTicker(5 * time.Second)
	defer permitTicker.Stop()
	lifetime := time.NewTimer(45 * time.Minute)
	defer lifetime.Stop()
	sequence, lastRevision := first.Sequence, int64(-1)
	sendConfig := true
	for {
		if sendConfig {
			_, current, err := s.cacheForRequest(ctx, first.CacheId)
			if err != nil {
				return err
			}
			material = current
			if material.config.Revision != lastRevision {
				if err := stream.Send(&componentv1.CacheControlResponse{ProtocolVersion: 1, Sequence: sequence, PermitValidForMilliseconds: cacheOwnerTTL.Milliseconds(), Config: componentpkg.CacheConfigToProto(material.config)}); err != nil {
					return err
				}
				lastRevision = material.config.Revision
			}
			sendConfig = false
		}
		select {
		case <-ctx.Done():
			return connect.NewError(connect.CodeCanceled, ctx.Err())
		case err := <-receiveErrors:
			if err == io.EOF {
				return nil
			}
			return err
		case request := <-requests:
			if request.ProtocolVersion != 1 || request.CacheId != first.CacheId || request.SessionId != first.SessionId || request.Sequence <= sequence {
				return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("cache control sequence is invalid"))
			}
			sequence = request.Sequence
			_, material, err = s.cacheForRequest(ctx, first.CacheId)
			if err != nil || s.renewCache(ctx, identity, material, first.SessionId, request.Status) != nil {
				return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("cache owner lease is no longer current"))
			}
			response := &componentv1.CacheControlResponse{ProtocolVersion: 1, Sequence: sequence, PermitValidForMilliseconds: cacheOwnerTTL.Milliseconds()}
			switch maintenance := request.Maintenance.(type) {
			case *componentv1.CacheControlRequest_BeginGc:
				response.GcGrant, err = s.beginCacheGC(ctx, identity, material, first.SessionId)
			case *componentv1.CacheControlRequest_CompleteGc:
				err = s.completeCacheGC(ctx, identity, first.CacheId, first.SessionId, maintenance.CompleteGc.GrantId)
			}
			if err != nil {
				return err
			}
			if err := stream.Send(response); err != nil {
				return err
			}
		case <-changes:
			sendConfig = true
		case <-permitTicker.C:
			if err := stream.Send(&componentv1.CacheControlResponse{ProtocolVersion: 1, Sequence: sequence, PermitValidForMilliseconds: cacheOwnerTTL.Milliseconds()}); err != nil {
				return err
			}
		case <-lifetime.C:
			return nil
		}
	}
}
