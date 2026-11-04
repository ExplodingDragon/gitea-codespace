// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/x509"
	"fmt"
	"time"

	identitypkg "gitea.dev/codespace/internal/identity"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func reconcileIdentitySecret(ctx context.Context, kubeClient client.Client, authority *IdentityAuthority, owner client.Object, namespace, name, role, rawIdentity string, usages []x509.ExtKeyUsage, labels map[string]string) (time.Duration, error) {
	if authority == nil {
		return 0, fmt.Errorf("internal certificate authority is required")
	}
	return reconcileCertificateSecret(ctx, kubeClient, owner, namespace, name, role, labels,
		func(data map[string][]byte) (time.Time, error) { return authority.validate(data, rawIdentity, usages) },
		func() (*identitypkg.Leaf, error) { return authority.issue(rawIdentity, usages) },
	)
}

func reconcileServerCertificateSecret(ctx context.Context, kubeClient client.Client, authority *IdentityAuthority, owner client.Object, namespace, name, dnsName string, labels map[string]string) (time.Duration, error) {
	if authority == nil {
		return 0, fmt.Errorf("internal certificate authority is required")
	}
	return reconcileCertificateSecret(ctx, kubeClient, owner, namespace, name, "cache-registry", labels,
		func(data map[string][]byte) (time.Time, error) {
			return authority.validateServerCertificate(data, dnsName)
		},
		func() (*identitypkg.Leaf, error) { return authority.issueServerCertificate(dnsName) },
	)
}

func reconcileCertificateSecret(ctx context.Context, kubeClient client.Client, owner client.Object, namespace, name, role string, labels map[string]string, validate func(map[string][]byte) (time.Time, error), issue func() (*identitypkg.Leaf, error)) (time.Duration, error) {
	key := client.ObjectKey{Namespace: namespace, Name: name}
	var secret corev1.Secret
	err := kubeClient.Get(ctx, key, &secret)
	notFound := apierrors.IsNotFound(err)
	if err != nil && !notFound {
		return 0, err
	}
	if err == nil && !metav1.IsControlledBy(&secret, owner) {
		return 0, fmt.Errorf("certificate Secret %s does not belong to the current %s", name, owner.GetObjectKind().GroupVersionKind().Kind)
	}
	renewal, validationErr := validate(secret.Data)
	if err == nil && validationErr == nil && time.Until(renewal) > 0 {
		return time.Until(renewal), nil
	}
	leaf, err := issue()
	if err != nil {
		return 0, err
	}
	if notFound {
		secret = corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	}
	secret.Type = corev1.SecretTypeTLS
	secret.Data = map[string][]byte{
		identitypkg.TLSCertificateKey:  leaf.Certificate,
		identitypkg.TLSPrivateKeyKey:   leaf.PrivateKey,
		identitypkg.RootCertificateKey: leaf.Root,
	}
	secret.Labels = make(map[string]string, len(labels)+1)
	for key, value := range labels {
		secret.Labels[key] = value
	}
	secret.Labels[identityRoleLabel] = role
	if err := controllerutil.SetControllerReference(owner, &secret, kubeClient.Scheme()); err != nil {
		return 0, err
	}
	if notFound {
		err = kubeClient.Create(ctx, &secret)
	} else {
		err = kubeClient.Update(ctx, &secret)
	}
	if err != nil {
		return 0, err
	}
	return time.Until(leaf.NotAfter.Add(-identitypkg.LeafRenewBefore)), nil
}
