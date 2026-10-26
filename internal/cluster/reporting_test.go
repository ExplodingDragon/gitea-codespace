// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

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
		cs := &api.Codespace{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "codespace-example"}, Spec: api.CodespaceSpec{Site: api.ResourceReference{Name: site.Name, UID: site.UID}}, Status: api.CodespaceStatus{Bound: true}}
		require.NoError(t, observations.Create(ctx, cs))
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal(t, 1, reads)
		select {
		case <-done:
		default:
			t.Fatal("reporting continued after the live site was disabled")
		}
	})
}
