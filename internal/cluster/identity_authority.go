// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"sync"
	"time"

	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	identitypkg "gitea.dev/codespace/internal/identity"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var errIdentityTrustMaterial = errors.New("internal identity trust material is unavailable")

const (
	identityCALabel   = "codespace.gitea.dev/internal-ca"
	identityRoleLabel = "codespace.gitea.dev/identity-role"
)

// IdentityAuthority persists one installation trust root and provides the
// current in-memory Manager certificate. Kubernetes resourceVersion is the
// serialization boundary for concurrent Manager replicas.
type IdentityAuthority struct {
	client          client.Client
	namespace       string
	secretName      string
	managerIdentity string

	mu          sync.RWMutex
	authority   *identitypkg.Authority
	managerLeaf tls.Certificate
}

func LoadIdentityAuthority(ctx context.Context, kubeClient client.Client, namespace, secretName string) (*IdentityAuthority, error) {
	if namespace == "" || secretName == "" {
		return nil, fmt.Errorf("management namespace and internal CA Secret name are required")
	}
	var ns corev1.Namespace
	if err := kubeClient.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
		return nil, fmt.Errorf("read management namespace identity: %w", err)
	}
	if ns.UID == "" {
		return nil, fmt.Errorf("management namespace has no assigned UID")
	}
	authority := &IdentityAuthority{
		client: kubeClient, namespace: namespace, secretName: secretName,
		managerIdentity: identitypkg.ManagerURI(string(ns.UID)),
	}
	if err := authority.sync(ctx, true); err != nil {
		return nil, err
	}
	return authority, nil
}

func (a *IdentityAuthority) ManagerIdentity() string { return a.managerIdentity }

// Start keeps all Manager replicas on the persisted issuer and refreshes their
// independent in-memory server certificates.
func (a *IdentityAuthority) Start(ctx context.Context) error {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := a.sync(ctx, false); err != nil {
				if errors.Is(err, errIdentityTrustMaterial) {
					return err
				}
				log.FromContext(ctx).Error(err, "synchronize internal certificate authority")
			}
		}
	}
}

func (*IdentityAuthority) NeedLeaderElection() bool { return false }

func (a *IdentityAuthority) sync(ctx context.Context, allowCreate bool) error {
	var persisted *identitypkg.Authority
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var secret corev1.Secret
		err := a.client.Get(ctx, types.NamespacedName{Namespace: a.namespace, Name: a.secretName}, &secret)
		if apierrors.IsNotFound(err) {
			if !allowCreate {
				return fmt.Errorf("%w: internal CA Secret %s/%s disappeared; restore it from backup", errIdentityTrustMaterial, a.namespace, a.secretName)
			}
			hasState, err := hasPersistedIdentityState(ctx, a.client, a.namespace)
			if err != nil {
				return err
			}
			if hasState {
				return fmt.Errorf("%w: internal CA Secret %s/%s is missing while managed state exists; restore it from backup", errIdentityTrustMaterial, a.namespace, a.secretName)
			}
			created, err := identitypkg.NewAuthority(time.Now())
			if err != nil {
				return err
			}
			data, err := created.Data()
			if err != nil {
				return err
			}
			secret = corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: a.secretName, Namespace: a.namespace, Labels: map[string]string{identityCALabel: "true"}},
				Type:       corev1.SecretTypeOpaque, Data: data,
			}
			if err := a.client.Create(ctx, &secret); err != nil {
				if apierrors.IsAlreadyExists(err) {
					return apierrors.NewConflict(corev1.Resource("secrets"), a.secretName, err)
				}
				return err
			}
			persisted = created
			return nil
		}
		if err != nil {
			return err
		}
		loaded, err := identitypkg.ParseAuthority(secret.Data, time.Now())
		if err != nil {
			return fmt.Errorf("%w: validate internal CA Secret %s/%s: %v", errIdentityTrustMaterial, a.namespace, a.secretName, err)
		}
		if loaded.IssuerNeedsRenewal(time.Now()) {
			loaded, err = loaded.RenewIssuer(time.Now())
			if err != nil {
				return err
			}
			secret.Data, err = loaded.Data()
			if err != nil {
				return err
			}
			if err := a.client.Update(ctx, &secret); err != nil {
				return err
			}
		}
		persisted = loaded
		return nil
	})
	if err != nil {
		return err
	}
	if persisted == nil {
		return fmt.Errorf("internal certificate authority was not loaded")
	}
	leaf, err := persisted.Issue(a.managerIdentity, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, time.Now())
	if err != nil {
		return fmt.Errorf("issue Manager identity: %w", err)
	}
	pair, err := tls.X509KeyPair(leaf.Certificate, leaf.PrivateKey)
	if err != nil {
		return fmt.Errorf("load generated Manager identity: %w", err)
	}
	a.mu.Lock()
	a.authority = persisted
	a.managerLeaf = pair
	a.mu.Unlock()
	remaining := time.Until(persisted.RootNotAfter())
	if remaining < 180*24*time.Hour {
		log.FromContext(ctx).Info("internal root CA is approaching expiration", "notAfter", persisted.RootNotAfter(), "remaining", remaining)
	}
	return nil
}

func hasPersistedIdentityState(ctx context.Context, kubeClient client.Client, namespace string) (bool, error) {
	var sites api.GiteaSiteList
	if err := kubeClient.List(ctx, &sites, client.Limit(1)); err != nil {
		return false, err
	}
	if len(sites.Items) != 0 {
		return true, nil
	}
	var runtimes api.CodespaceList
	if err := kubeClient.List(ctx, &runtimes, client.Limit(1)); err != nil {
		return false, err
	}
	if len(runtimes.Items) != 0 {
		return true, nil
	}
	var components corev1.ConfigMapList
	if err := kubeClient.List(ctx, &components, client.InNamespace(namespace), client.HasLabels{ComponentLabel}, client.Limit(1)); err != nil {
		return false, err
	}
	if len(components.Items) != 0 {
		return true, nil
	}
	var identities corev1.SecretList
	if err := kubeClient.List(ctx, &identities, client.HasLabels{identityRoleLabel}, client.Limit(1)); err != nil {
		return false, err
	}
	return len(identities.Items) != 0, nil
}

// Load implements transport.CertificateSource for Manager's in-memory server
// identity.
func (a *IdentityAuthority) Load() (tls.Certificate, *x509.CertPool, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.authority == nil || len(a.managerLeaf.Certificate) == 0 {
		return tls.Certificate{}, nil, fmt.Errorf("Manager identity is not initialized")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(a.authority.RootPEM()) {
		return tls.Certificate{}, nil, fmt.Errorf("internal CA has no root certificate")
	}
	return a.managerLeaf, roots, nil
}

func (a *IdentityAuthority) issue(rawIdentity string, usages []x509.ExtKeyUsage) (*identitypkg.Leaf, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.authority == nil {
		return nil, fmt.Errorf("internal certificate authority is not initialized")
	}
	return a.authority.Issue(rawIdentity, usages, time.Now())
}

func (a *IdentityAuthority) validate(data map[string][]byte, rawIdentity string, usages []x509.ExtKeyUsage) (time.Time, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.authority == nil {
		return time.Time{}, fmt.Errorf("internal certificate authority is not initialized")
	}
	return a.authority.ValidateLeaf(data, rawIdentity, usages, time.Now())
}
