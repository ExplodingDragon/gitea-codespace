// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"uuid"

	"connectrpc.com/connect/v2"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	componentv1 "gitea.dev/codespace/internal/rpc/component/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	cacheCredentialUntilAnnotation = "codespace.gitea.dev/cache-credentials-until"
	cacheLastCleanupAnnotation     = "codespace.gitea.dev/last-cleanup"
)

var errCacheCleanupNotDue = errors.New("cache cleanup is not due")

// Credential issuance and maintenance share one resourceVersion boundary. The
// idle Lease retains the expiry watermark so restarts cannot shorten protection.
func (s *ComponentServer) updateCacheGCLease(ctx context.Context, component *api.CodespaceComponent, update func(*coordinationv1.Lease) error) error {
	return retry.OnError(retry.DefaultRetry, func(err error) bool {
		return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err)
	}, func() error {
		lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "cache-gc-" + component.Name, Namespace: s.ManagementNamespace}}
		err := s.Client.Get(ctx, client.ObjectKeyFromObject(lease), lease)
		missing := apierrors.IsNotFound(err)
		if err != nil && !missing {
			return err
		}
		if missing {
			lease.Labels = map[string]string{ComponentUIDLabel: string(component.UID)}
			lease.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(component, api.GroupVersion.WithKind("CodespaceComponent"))}
		} else if lease.Labels[ComponentUIDLabel] != string(component.UID) || !metav1.IsControlledBy(lease, component) || !lease.DeletionTimestamp.IsZero() {
			return connect.NewError(connect.CodeFailedPrecondition, "cache maintenance Lease ownership changed")
		}
		if lease.Annotations == nil {
			lease.Annotations = make(map[string]string)
		}
		if err := update(lease); err != nil {
			return err
		}
		if missing {
			return s.Client.Create(ctx, lease)
		}
		return s.Client.Update(ctx, lease)
	})
}

func cacheCredentialDeadline(lease *coordinationv1.Lease) (time.Time, error) {
	value := lease.Annotations[cacheCredentialUntilAnnotation]
	if value == "" {
		return time.Time{}, nil
	}
	until, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("read cache credential expiry: %w", err)
	}
	return until, nil
}

func cacheLeaseDeadline(lease *coordinationv1.Lease) time.Time {
	if ptr.Deref(lease.Spec.HolderIdentity, "") == "" || lease.Spec.RenewTime == nil {
		return time.Time{}
	}
	return lease.Spec.RenewTime.Add(time.Duration(ptr.Deref(lease.Spec.LeaseDurationSeconds, 0)) * time.Second)
}

func (s *ComponentServer) beginCacheGC(ctx context.Context, identity componentIdentity, material *cacheMaterial, sessionID string) (*componentv1.CacheGCGrant, error) {
	var owner coordinationv1.Lease
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.ManagementNamespace, Name: cacheOwnerLeaseName(material.component.Name)}, &owner); err != nil {
		return nil, err
	}
	if ptr.Deref(owner.Spec.HolderIdentity, "") != cacheOwnerHolder(identity, sessionID, material.config.Revision) || !time.Now().Before(cacheLeaseDeadline(&owner)) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "cache owner lease is no longer current")
	}
	var grant *componentv1.CacheGCGrant
	err := s.updateCacheGCLease(ctx, material.component, func(lease *coordinationv1.Lease) error {
		until, err := cacheCredentialDeadline(lease)
		if err != nil {
			return err
		}
		now := metav1.NowMicro()
		deadline := cacheLeaseDeadline(lease)
		if !now.Time.Before(deadline) {
			if value := lease.Annotations[cacheLastCleanupAnnotation]; value != "" {
				completedAt, err := time.Parse(time.RFC3339Nano, value)
				if err != nil {
					return fmt.Errorf("read cache cleanup time: %w", err)
				}
				if now.Time.Before(completedAt.Add(material.config.GCInterval.ToStdlib())) {
					return errCacheCleanupNotDue
				}
			}
			readyAt := now.Time
			if until.After(readyAt) {
				readyAt = until
			}
			duration := readyAt.Sub(now.Time) + cacheGCTTL
			lease.Spec = coordinationv1.LeaseSpec{
				HolderIdentity: ptr.To(sessionID + "/" + uuid.NewV4().String()),
				AcquireTime:    &now, RenewTime: &now,
				LeaseDurationSeconds: ptr.To(int32((duration + time.Second - 1) / time.Second)),
			}
		} else {
			// A replacement owner resumes the frozen cutoff, not a new drain.
			holder, _, _ := strings.Cut(ptr.Deref(lease.Spec.HolderIdentity, ""), "/")
			if holder != sessionID {
				lease.Spec.HolderIdentity = ptr.To(sessionID + "/" + uuid.NewV4().String())
			}
		}
		if lease.Spec.AcquireTime == nil {
			return fmt.Errorf("cache maintenance Lease has no acquisition time")
		}
		readyAt := lease.Spec.AcquireTime.Time
		if until.After(readyAt) {
			readyAt = until
		}
		_, grantID, _ := strings.Cut(ptr.Deref(lease.Spec.HolderIdentity, ""), "/")
		grant = &componentv1.CacheGCGrant{
			CacheId: material.component.Name, Id: grantID, ConfigRevision: material.config.Revision,
			ReadyAtUnixMilliseconds: readyAt.UnixMilli(), ExpiresAtUnixMilliseconds: cacheLeaseDeadline(lease).UnixMilli(),
		}
		return nil
	})
	if errors.Is(err, errCacheCleanupNotDue) {
		return nil, nil
	}
	return grant, err
}

func (s *ComponentServer) completeCacheGC(ctx context.Context, component *api.CodespaceComponent, sessionID, grantID string) error {
	return s.updateCacheGCLease(ctx, component, func(lease *coordinationv1.Lease) error {
		if ptr.Deref(lease.Spec.HolderIdentity, "") != sessionID+"/"+grantID {
			return connect.NewError(connect.CodeFailedPrecondition, "cache cleanup grant is no longer current")
		}
		lease.Spec = coordinationv1.LeaseSpec{}
		lease.Annotations[cacheLastCleanupAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
		return nil
	})
}
