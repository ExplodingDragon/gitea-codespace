// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/x509"
	"fmt"
	"strings"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const IdentityUIDIndex = "codespace.identity.uid"

// ResolveAgent checks current resource ownership after TLS verifies the certificate.
func ResolveAgent(ctx context.Context, index, reader client.Reader, certificate *x509.Certificate) (*api.Codespace, *corev1.Pod, error) {
	if index == nil || reader == nil {
		return nil, nil, fmt.Errorf("agent identity index and live reader are required")
	}
	if certificate == nil || len(certificate.URIs) != 1 {
		return nil, nil, fmt.Errorf("agent certificate requires one application identity")
	}
	identity := certificate.URIs[0]
	parts := strings.Split(strings.TrimPrefix(identity.Path, "/"), "/")
	if identity.Scheme != "spiffe" || identity.Host != "codespace" || identity.User != nil || identity.RawQuery != "" || identity.Fragment != "" || identity.RawPath != "" || len(parts) != 4 || parts[0] != "agent" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return nil, nil, fmt.Errorf("invalid Agent application identity")
	}
	var sites api.GiteaSiteList
	if err := index.List(ctx, &sites, client.MatchingFields{IdentityUIDIndex: parts[1]}); err != nil {
		return nil, nil, err
	}
	if len(sites.Items) != 1 {
		return nil, nil, fmt.Errorf("agent site identity is no longer available")
	}
	site := &sites.Items[0]
	// The index only locates names. Authorization uses live objects so a stale
	// informer cannot extend the life of a deleted or replaced identity.
	if err := reader.Get(ctx, client.ObjectKeyFromObject(site), site); err != nil {
		return nil, nil, err
	}
	if site.UID != types.UID(parts[1]) || !site.DeletionTimestamp.IsZero() {
		return nil, nil, fmt.Errorf("agent site identity changed")
	}
	var namespace corev1.Namespace
	if err := reader.Get(ctx, types.NamespacedName{Name: "codespace-" + site.Name}, &namespace); err != nil {
		return nil, nil, err
	}
	if namespace.UID != site.Status.NamespaceUID || namespace.Labels[SiteUIDLabel] != string(site.UID) {
		return nil, nil, fmt.Errorf("agent namespace identity changed")
	}
	var runtimes api.CodespaceList
	if err := index.List(ctx, &runtimes, client.InNamespace(namespace.Name), client.MatchingFields{IdentityUIDIndex: parts[2]}); err != nil {
		return nil, nil, err
	}
	if len(runtimes.Items) != 1 {
		return nil, nil, fmt.Errorf("agent runtime identity is no longer available")
	}
	cs := &runtimes.Items[0]
	if err := reader.Get(ctx, client.ObjectKeyFromObject(cs), cs); err != nil {
		return nil, nil, err
	}
	if cs.UID != types.UID(parts[2]) || cs.Spec.Site.UID != site.UID || cs.Spec.Site.Name != site.Name || !cs.Status.Bound || cs.Status.Pod.UID != types.UID(parts[3]) {
		return nil, nil, fmt.Errorf("agent runtime or Pod identity changed")
	}
	var pod corev1.Pod
	if err := reader.Get(ctx, types.NamespacedName{Namespace: cs.Namespace, Name: cs.Status.Pod.Name}, &pod); err != nil {
		return nil, nil, err
	}
	if pod.UID != cs.Status.Pod.UID || !metav1.IsControlledBy(&pod, cs) {
		return nil, nil, fmt.Errorf("agent Pod ownership changed")
	}
	return cs, &pod, nil
}
