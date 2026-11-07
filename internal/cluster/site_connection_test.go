// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestSiteConnectionStoreReusesConnectionWithinTTL(t *testing.T) {
	site := testSite("example", "site-uid")
	site.Status.CanonicalURL = "https://gitea.example.com"
	credential := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: site.Spec.Credential.Name, Namespace: "codespace-system", UID: site.Spec.Credential.UID, Labels: map[string]string{SiteUIDLabel: string(site.UID)}},
		Data:       map[string][]byte{"managerSecret": []byte("secret")},
	}
	var reads atomic.Int32
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(credential).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if _, ok := object.(*corev1.Secret); ok {
				reads.Add(1)
			}
			return c.Get(ctx, key, object, options...)
		},
	}).Build()
	store := newSiteConnectionStore(c, credential.Namespace)
	t.Cleanup(store.Close)

	var wg sync.WaitGroup
	errors := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.ForSite(t.Context(), site)
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, reads.Load())

	store.mu.Lock()
	store.entries[site.UID].expires = time.Now().Add(-time.Second)
	store.mu.Unlock()
	_, err := store.ForSite(t.Context(), site)
	require.NoError(t, err)
	require.EqualValues(t, 2, reads.Load())

	site.Spec.ManagerID++
	_, err = store.ForSite(t.Context(), site)
	require.NoError(t, err)
	require.EqualValues(t, 3, reads.Load())
}

func TestSiteConnectionRequiresOwnedCredential(t *testing.T) {
	site := testSite("example", "site-uid")
	site.Status.CanonicalURL = "https://gitea.example.com"
	credential := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: site.Spec.Credential.Name, Namespace: "codespace-system", UID: site.Spec.Credential.UID, Labels: map[string]string{SiteUIDLabel: "another-site"}},
		Data:       map[string][]byte{"managerSecret": []byte("secret")},
	}
	store := newSiteConnectionStore(fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(credential).Build(), credential.Namespace)
	t.Cleanup(store.Close)

	_, err := store.ForSite(t.Context(), site)
	require.ErrorContains(t, err, "credential ownership or value is invalid")
}
