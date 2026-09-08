// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

func (s *etcdInfrastructureStore) publishGatewayActivity(ctx context.Context, sessions *gatewaySessionRegistry) {
	key := s.key("gateway-activity/" + uuid.NewString())
	for ctx.Err() == nil {
		session, err := concurrency.NewSession(s.client, concurrency.WithTTL(15), concurrency.WithContext(ctx))
		if err != nil {
			if waitElectionRetry(ctx) != nil {
				return
			}
			continue
		}
		for ctx.Err() == nil && session.Ctx().Err() == nil {
			sessions.mu.Lock()
			cancels := sessions.dropExpiredSessionsLocked(time.Now())
			activity := make(map[string]int, len(sessions.sessionCountByCodespace))
			for id, count := range sessions.sessionCountByCodespace {
				activity[id] = count
			}
			for id, count := range sessions.anonymousLive {
				activity[id] += count
			}
			sessions.mu.Unlock()
			cancelGatewaySessions(cancels)
			data, err := json.Marshal(activity)
			if err != nil {
				break
			}
			writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err = s.client.Put(writeCtx, key, string(data), clientv3.WithLease(session.Lease()))
			cancel()
			if err != nil {
				for id := range activity {
					sessions.DeleteCodespace(id)
				}
				break
			}
			if waitElectionRetry(ctx) != nil {
				break
			}
		}
		session.Orphan()
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = s.client.Revoke(cleanup, session.Lease())
		cancel()
	}
}

// Unknown activity is treated as active: infrastructure failure must not stop work.
type sharedGatewayActivity struct {
	store     *etcdInfrastructureStore
	mu        sync.Mutex
	nextRead  time.Time
	activity  map[string]int
	available bool
}

func (s *sharedGatewayActivity) LiveSessions(runtimeUUID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Now().After(s.nextRead) {
		ctx, cancel := context.WithTimeout(s.store.client.Ctx(), 5*time.Second)
		response, err := s.store.client.Get(ctx, s.store.key("gateway-activity/"), clientv3.WithPrefix())
		cancel()
		s.available = err == nil && len(response.Kvs) > 0
		s.activity = make(map[string]int)
		if s.available {
			for _, item := range response.Kvs {
				var counts map[string]int
				if json.Unmarshal(item.Value, &counts) != nil {
					s.available = false
					break
				}
				for id, count := range counts {
					if count < 0 {
						s.available = false
						break
					}
					if count > 0 {
						s.activity[id] = 1
					}
				}
			}
		}
		s.nextRead = time.Now().Add(time.Second)
	}
	if !s.available {
		return 1
	}
	return s.activity[runtimeUUID]
}
