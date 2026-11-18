// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestReportingIsolatesSlowRuntimeAndPreservesNewGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		site := testSite("example", "site-uid")
		site.Status.CanonicalURL = site.Spec.URL
		site.Status.Conditions = []metav1.Condition{{Type: "InfrastructureReady", Status: metav1.ConditionTrue, ObservedGeneration: site.Generation}}
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&api.Codespace{}).WithObjects(site).Build()
		slow := &api.Codespace{
			ObjectMeta: metav1.ObjectMeta{Name: "slow", Namespace: "codespace-example", UID: "slow"},
			Spec:       api.CodespaceSpec{Site: api.ResourceReference{UID: site.UID}, CodespaceID: 1, Operation: api.Operation{Type: "create", Version: 1}},
			Status:     api.CodespaceStatus{MetadataGeneration: 1, Boot: &api.RuntimeBoot{OperationVersion: 1, Stage: "prepare-runtime", StartedUnix: 1, LastUpdateUnix: 1}},
		}
		require.NoError(t, c.Create(t.Context(), slow))
		var mu sync.Mutex
		reports := map[int64][]int64{}
		release := make(chan struct{})
		remote := &operationRemote{metadata: func(request *codespacev1.ReportRuntimeMetadataRequest) error {
			mu.Lock()
			reports[request.CodespaceId] = append(reports[request.CodespaceId], request.MetadataGeneration)
			mu.Unlock()
			if request.CodespaceId == 1 && request.MetadataGeneration == 1 {
				<-release
			}
			return nil
		}}
		connections := newSiteConnectionStore(c, "codespace-system")
		connections.entries[site.UID] = &siteConnection{client: remote, fingerprint: siteConnectionFingerprint(site), expires: time.Now().Add(time.Minute)}
		coordinator := &SiteCoordinator{Operations: Operations{Client: c, Connections: connections, ManagementNamespace: "codespace-system"}, ObservationReader: c}
		var interval atomic.Int64
		interval.Store(int64(30 * time.Second))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); coordinator.reportSite(ctx, site.Name, site.UID, &interval) }()
		defer func() { cancel(); close(release); <-done }()
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal(t, map[int64][]int64{1: {1}}, reports)
		fast := slow.DeepCopy()
		fast.Name, fast.UID, fast.ResourceVersion, fast.Spec.CodespaceID = "fast", "fast", "", 2
		require.NoError(t, c.Create(ctx, fast))
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(slow), slow))
		slow.Status.MetadataGeneration = 2
		require.NoError(t, c.Status().Update(ctx, slow))
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal(t, map[int64][]int64{1: {1}, 2: {1}}, reports)
		release <- struct{}{}
		synctest.Wait()
		require.Equal(t, []int64{1, 2}, reports[1])
		require.Equal(t, []int64{1}, reports[2])
	})
}

func TestReportingObservesCacheBeforeReadingLiveState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		site := testSite("example", "site-uid")
		site.Spec.Enabled = false
		reads := 0
		live := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(site).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				reads++
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
		observations := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
		coordinator := &SiteCoordinator{Operations: Operations{Client: live, ManagementNamespace: "codespace-system"}, ObservationReader: observations}
		var interval atomic.Int64
		interval.Store(int64(30 * time.Second))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); coordinator.reportSite(ctx, site.Name, site.UID, &interval) }()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		require.Zero(t, reads)
		cs := &api.Codespace{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-example"}, Spec: api.CodespaceSpec{Site: api.ResourceReference{Name: site.Name, UID: site.UID}}, Status: api.CodespaceStatus{}}
		require.NoError(t, observations.Create(ctx, cs))
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal(t, 1, reads)
		cancel()
		<-done
	})
}
