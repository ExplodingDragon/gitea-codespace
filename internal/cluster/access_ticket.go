// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"time"

	"gitea.dev/codespace/internal/accessticket"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	agentv1 "gitea.dev/codespace/internal/rpc/agent/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const accessSigningSecretName = "codespace-access-signing-key"

// AccessTickets owns the Manager-only signing key. Runtime Agents receive only
// PublicKey, while Gateway receives a ticket limited to one current target.
type AccessTickets struct{ privateKey ed25519.PrivateKey }

func LoadAccessTickets(ctx context.Context, c client.Client, namespace string) (*AccessTickets, error) {
	key := types.NamespacedName{Namespace: namespace, Name: accessSigningSecretName}
	var secret corev1.Secret
	err := c.Get(ctx, key, &secret)
	if apierrors.IsNotFound(err) {
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		secret = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{ComponentLabel: "manager"}},
			Immutable:  ptr.To(true), Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"seed": seed},
		}
		if err := c.Create(ctx, &secret); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return nil, err
			}
			if err := c.Get(ctx, key, &secret); err != nil {
				return nil, err
			}
		}
	} else if err != nil {
		return nil, err
	}
	seed := secret.Data["seed"]
	if len(seed) != ed25519.SeedSize || !secret.DeletionTimestamp.IsZero() || secret.Labels[ComponentLabel] != "manager" {
		return nil, fmt.Errorf("access signing Secret is invalid")
	}
	return &AccessTickets{privateKey: ed25519.NewKeyFromSeed(seed)}, nil
}

func (t *AccessTickets) PublicKey() []byte {
	if t == nil || len(t.privateKey) != ed25519.PrivateKeySize {
		return nil
	}
	return append([]byte(nil), t.privateKey.Public().(ed25519.PublicKey)...)
}

func (t *AccessTickets) Issue(cs *api.Codespace, capability agentv1.AccessCapability, endpointID string, lifetime time.Duration) (string, error) {
	if t == nil || cs == nil || cs.Status.Pod.UID == "" || cs.Status.Target == nil || !cs.Status.Target.Ready {
		return "", fmt.Errorf("runtime access target is unavailable")
	}
	if lifetime <= 0 || lifetime > accessticket.MaxLifetime {
		return "", fmt.Errorf("access ticket lifetime is invalid")
	}
	now := time.Now()
	return accessticket.Sign(t.privateKey, &agentv1.AccessTicketClaims{
		SiteUid: string(cs.Spec.Site.UID), ResourceUid: string(cs.UID), RuntimeUuid: cs.Spec.RuntimeUUID, PodUid: string(cs.Status.Pod.UID),
		TargetVersion: cs.Status.Target.Version, Capability: capability, EndpointId: endpointID, ExpiresUnix: now.Add(lifetime).Unix(),
	}, now)
}
