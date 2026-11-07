// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"gitea.dev/codespace/internal/controlplane"
	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const siteConnectionTTL = time.Minute

type siteConnection struct {
	client      codespacev1connect.ManagerServiceClient
	transport   *http.Transport
	fingerprint [sha256.Size]byte
	expires     time.Time
}

func (c *siteConnection) close() {
	if c != nil && c.transport != nil {
		c.transport.CloseIdleConnections()
	}
}

// siteConnectionStore reuses authenticated Gitea transports across all
// runtime control paths while allowing credential and CA changes to converge.
type siteConnectionStore struct {
	client              client.Client
	managementNamespace string
	mu                  sync.Mutex
	entries             map[types.UID]*siteConnection
	builds              singleflight.Group
}

func newSiteConnectionStore(c client.Client, managementNamespace string) *siteConnectionStore {
	return &siteConnectionStore{client: c, managementNamespace: managementNamespace, entries: make(map[types.UID]*siteConnection)}
}

func (*siteConnectionStore) NeedLeaderElection() bool { return false }

func (s *siteConnectionStore) Start(ctx context.Context) error {
	<-ctx.Done()
	s.Close()
	return nil
}

func (s *siteConnectionStore) ForSite(ctx context.Context, site *api.GiteaSite) (codespacev1connect.ManagerServiceClient, error) {
	if s == nil || s.client == nil || site == nil {
		return nil, fmt.Errorf("site connection store is unavailable")
	}
	if site.Status.CanonicalURL == "" {
		return nil, fmt.Errorf("site identity is not verified")
	}
	fingerprint := siteConnectionFingerprint(site)
	for {
		if connection := s.load(site.UID, fingerprint, time.Now()); connection != nil {
			return connection.client, nil
		}
		result, err, _ := s.builds.Do(string(site.UID), func() (any, error) {
			if connection := s.load(site.UID, fingerprint, time.Now()); connection != nil {
				return connection, nil
			}
			connection, err := newSiteManagerConnection(ctx, s.client, s.managementNamespace, site, nil)
			if err != nil {
				return nil, err
			}
			connection.fingerprint = fingerprint
			connection.expires = time.Now().Add(siteConnectionTTL)
			s.mu.Lock()
			previous := s.entries[site.UID]
			s.entries[site.UID] = connection
			s.mu.Unlock()
			if previous != connection {
				previous.close()
			}
			return connection, nil
		})
		if err != nil {
			return nil, err
		}
		connection := result.(*siteConnection)
		if connection.fingerprint == fingerprint && time.Now().Before(connection.expires) {
			return connection.client, nil
		}
	}
}

func (s *siteConnectionStore) load(uid types.UID, fingerprint [sha256.Size]byte, now time.Time) *siteConnection {
	s.mu.Lock()
	connection := s.entries[uid]
	if connection != nil && (connection.fingerprint != fingerprint || !now.Before(connection.expires)) {
		delete(s.entries, uid)
	}
	s.mu.Unlock()
	if connection == nil || connection.fingerprint != fingerprint || !now.Before(connection.expires) {
		connection.close()
		return nil
	}
	return connection
}

func (s *siteConnectionStore) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	entries := s.entries
	s.entries = make(map[types.UID]*siteConnection)
	s.mu.Unlock()
	for _, connection := range entries {
		connection.close()
	}
}

func siteConnectionFingerprint(site *api.GiteaSite) [sha256.Size]byte {
	value := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s\x00%s\x00%s", site.UID, site.Spec.URL, site.Status.CanonicalURL, site.Spec.ManagerID, site.Spec.Credential.Name, site.Spec.Credential.UID, site.Spec.CABundle)
	return sha256.Sum256([]byte(value))
}

func newSiteManagerConnection(ctx context.Context, c client.Client, managementNamespace string, site *api.GiteaSite, base *http.Client) (*siteConnection, error) {
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: managementNamespace, Name: site.Spec.Credential.Name}, &secret); err != nil {
		return nil, err
	}
	if secret.UID != site.Spec.Credential.UID || secret.Labels[SiteUIDLabel] != string(site.UID) || len(secret.Data["managerSecret"]) == 0 || !secret.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("site credential ownership or value is invalid")
	}
	httpClient, err := giteaHTTPClient(base, site.Spec.CABundle)
	if err != nil {
		return nil, err
	}
	connection := &siteConnection{
		client: controlplane.NewManagerServiceClient(httpClient, strings.TrimRight(site.Spec.URL, "/")+"/api/codespace", site.Spec.ManagerID, string(secret.Data["managerSecret"])),
	}
	if site.Spec.CABundle != "" {
		connection.transport, _ = httpClient.Transport.(*http.Transport)
	}
	return connection, nil
}
