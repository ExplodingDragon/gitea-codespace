// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

var errLeadershipLost = errors.New("deployment leadership lost")

// deploymentLeadership belongs to one process attempt, never to a reusable node name.
// Its session must stay alive until normal execution shutdown has completed.
type deploymentLeadership struct {
	client   *clientv3.Client
	session  *concurrency.Session
	key      string
	identity string
	revision int64
}

func (l *deploymentLeadership) comparisons() []clientv3.Cmp {
	return []clientv3.Cmp{
		clientv3.Compare(clientv3.CreateRevision(l.key), "=", l.revision),
		clientv3.Compare(clientv3.Value(l.key), "=", l.identity),
		clientv3.Compare(clientv3.LeaseValue(l.key), "=", int64(l.session.Lease())),
	}
}

func (l *deploymentLeadership) close(ctx context.Context) error {
	// Delete only this term. A delayed shutdown must not delete a successor's key.
	_, err := l.client.Txn(ctx).If(l.comparisons()...).Then(clientv3.OpDelete(l.key)).Commit()
	l.session.Orphan()
	_, revokeErr := l.client.Revoke(ctx, l.session.Lease())
	return errors.Join(err, revokeErr)
}

// waitForLeadership uses a single leased key. Reading before watching also handles
// a lost transaction response without assuming that the competing write failed.
func (s *etcdInfrastructureStore) waitForLeadership(ctx context.Context, nodeID string) (*deploymentLeadership, error) {
	key := s.key("leader")
	identity := nodeID + "/" + uuid.NewString()
	for ctx.Err() == nil {
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		response, err := s.client.Get(readCtx, key)
		cancel()
		if err != nil {
			if err := waitElectionRetry(ctx); err != nil {
				return nil, err
			}
			continue
		}
		if len(response.Kvs) != 0 {
			watchCtx, cancelWatch := context.WithCancel(clientv3.WithRequireLeader(ctx))
			watch := s.client.Watch(watchCtx, key, clientv3.WithRev(response.Header.Revision+1))
			watchFailed := false
		watchLeader:
			for event := range watch {
				if event.Err() != nil {
					watchFailed = true
					break
				}
				for _, change := range event.Events {
					if change.Type == clientv3.EventTypeDelete {
						break watchLeader
					}
				}
			}
			cancelWatch()
			if watchFailed {
				if err := waitElectionRetry(ctx); err != nil {
					return nil, err
				}
			}
			continue
		}

		// Candidates allocate a lease only when there is no observed leader.
		session, err := concurrency.NewSession(s.client, concurrency.WithTTL(15), concurrency.WithContext(ctx))
		if err != nil {
			if err := waitElectionRetry(ctx); err != nil {
				return nil, err
			}
			continue
		}
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		claimed, claimErr := s.client.Txn(writeCtx).
			If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
			Then(clientv3.OpPut(key, identity, clientv3.WithLease(session.Lease()))).Commit()
		cancel()
		var revision int64
		if claimErr == nil && claimed.Succeeded {
			revision = claimed.Header.Revision
		} else if claimErr != nil {
			readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			observed, readErr := s.client.Get(readCtx, key)
			cancel()
			if readErr == nil && len(observed.Kvs) == 1 &&
				string(observed.Kvs[0].Value) == identity && observed.Kvs[0].Lease == int64(session.Lease()) {
				revision = observed.Kvs[0].CreateRevision
			}
		}
		if revision > 0 && session.Ctx().Err() == nil && ctx.Err() == nil {
			return &deploymentLeadership{client: s.client, session: session, key: key, identity: identity, revision: revision}, nil
		}
		session.Orphan()
		revokeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = s.client.Revoke(revokeCtx, session.Lease())
		cancel()
		if claimErr != nil {
			if err := waitElectionRetry(ctx); err != nil {
				return nil, err
			}
		}
	}
	return nil, ctx.Err()
}

func waitElectionRetry(ctx context.Context) error {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// commit applies execution writes only while this term still owns the leader key.
func (l *deploymentLeadership) commit(ctx context.Context, comparisons []clientv3.Cmp, operations ...clientv3.Op) error {
	if err := l.session.Ctx().Err(); err != nil {
		return errLeadershipLost
	}
	response, err := l.client.Txn(ctx).If(append(l.comparisons(), comparisons...)...).Then(operations...).Commit()
	if err != nil {
		return fmt.Errorf("commit deployment execution state: %w", err)
	}
	if !response.Succeeded {
		return fmt.Errorf("execution ownership or state revision changed: %w", errLeadershipLost)
	}
	return nil
}
