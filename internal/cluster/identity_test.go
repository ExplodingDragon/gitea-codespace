// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"crypto/x509"
	"testing"
	"time"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	identitypkg "gitea.dev/codespace/internal/identity"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestIdentityAuthority(t *testing.T) *IdentityAuthority {
	t.Helper()
	authority, err := identitypkg.NewAuthority(time.Now())
	require.NoError(t, err)
	return &IdentityAuthority{authority: authority}
}

func TestIdentityAuthorityBootstrapsAndReloads(t *testing.T) {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "codespace-system", UID: "namespace-uid"}}
	kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(namespace).Build()
	first, err := LoadIdentityAuthority(t.Context(), kubeClient, namespace.Name, "internal-ca")
	require.NoError(t, err)
	require.Equal(t, "spiffe://codespace/manager/namespace-uid", first.ManagerIdentity())
	second, err := LoadIdentityAuthority(t.Context(), kubeClient, namespace.Name, "internal-ca")
	require.NoError(t, err)

	firstCertificate, firstRoots, err := first.Load()
	require.NoError(t, err)
	secondCertificate, secondRoots, err := second.Load()
	require.NoError(t, err)
	require.NotEqual(t, firstCertificate.Certificate[0], secondCertificate.Certificate[0])
	require.True(t, firstRoots.Equal(secondRoots))

	var secret corev1.Secret
	require.NoError(t, kubeClient.Get(t.Context(), types.NamespacedName{Namespace: namespace.Name, Name: "internal-ca"}, &secret))
	require.Equal(t, "true", secret.Labels[identityCALabel])
	_, err = identitypkg.ParseAuthority(secret.Data, time.Now())
	require.NoError(t, err)
}

func TestIdentityAuthorityDoesNotReplaceMissingTrustRoot(t *testing.T) {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "codespace-system", UID: "namespace-uid"}}
	site := &api.GiteaSite{ObjectMeta: metav1.ObjectMeta{Name: "existing", UID: "site-uid"}}
	kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(namespace, site).Build()
	_, err := LoadIdentityAuthority(t.Context(), kubeClient, namespace.Name, "missing-ca")
	require.ErrorContains(t, err, "managed state exists")
}

func TestReconcileIdentitySecretReplacesInvalidOwnedMaterial(t *testing.T) {
	authority := newTestIdentityAuthority(t)
	owner := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "site", UID: "pod-uid"}}
	kubeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(owner).Build()
	const workloadIdentity = "spiffe://codespace/agent/site/runtime/pod-uid"
	renewAfter, err := reconcileIdentitySecret(t.Context(), kubeClient, authority, owner, owner.Namespace, "runtime-identity", "agent", workloadIdentity,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, nil)
	require.NoError(t, err)
	require.Positive(t, renewAfter)

	var secret corev1.Secret
	require.NoError(t, kubeClient.Get(t.Context(), types.NamespacedName{Namespace: owner.Namespace, Name: "runtime-identity"}, &secret))
	secret.Data[identitypkg.TLSCertificateKey] = []byte("invalid")
	require.NoError(t, kubeClient.Update(t.Context(), &secret))
	_, err = reconcileIdentitySecret(t.Context(), kubeClient, authority, owner, owner.Namespace, secret.Name, "agent", workloadIdentity,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, nil)
	require.NoError(t, err)
	require.NoError(t, kubeClient.Get(t.Context(), types.NamespacedName{Namespace: owner.Namespace, Name: secret.Name}, &secret))
	_, err = authority.validate(secret.Data, workloadIdentity, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth})
	require.NoError(t, err)
}
