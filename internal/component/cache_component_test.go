// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package component

import (
	"context"
	"io"
	"testing"
	"testing/synctest"
	"time"

	cachepkg "gitea.dev/codespace/internal/cache"
	componentv1 "gitea.dev/codespace/internal/rpc/component/v1"
	"github.com/stretchr/testify/require"
)

type cacheTestStream struct {
	requests  chan *componentv1.CacheControlRequest
	responses chan *componentv1.CacheControlResponse
	closed    bool
}

func (s *cacheTestStream) Send(request *componentv1.CacheControlRequest) error {
	s.requests <- request
	return nil
}

func (s *cacheTestStream) Receive() (*componentv1.CacheControlResponse, error) {
	response, ok := <-s.responses
	if !ok {
		return nil, io.EOF
	}
	return response, nil
}

func (s *cacheTestStream) Close() error {
	s.closed = true
	return nil
}

func TestCacheMaintenanceAcknowledgmentAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stream := &cacheTestStream{
			requests: make(chan *componentv1.CacheControlRequest, 1), responses: make(chan *componentv1.CacheControlResponse, 1),
		}
		client := &CacheComponentClient{
			stream: stream, cacheID: "cache", sessionID: "session", permitUntil: time.Now().Add(time.Minute),
			ready: make(chan struct{}), waiters: make(map[uint64]chan *componentv1.CacheControlResponse),
		}
		go client.receive(t.Context(), stream, "")
		owner := cachepkg.Owner{CacheID: "cache", ID: "session", ConfigRevision: 1}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := client.BeginCacheGC(ctx, owner)
			done <- err
		}()
		request := <-stream.requests
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		synctest.Wait()
		require.Empty(t, client.waiters)

		// A late acknowledgment cannot complete the next maintenance request.
		stream.responses <- &componentv1.CacheControlResponse{ProtocolVersion: 1, Sequence: request.Sequence, PermitValidForMilliseconds: 15000}
		grant := cachepkg.GCGrant{CacheID: "cache", ID: "grant", ConfigRevision: 1}
		go func() { done <- client.CompleteCacheGC(t.Context(), grant) }()
		request = <-stream.requests
		synctest.Wait()
		require.Empty(t, done)
		require.Equal(t, grant.ID, request.GetCompleteGc().GrantId)
		stream.responses <- &componentv1.CacheControlResponse{ProtocolVersion: 1, Sequence: request.Sequence, PermitValidForMilliseconds: 15000}
		require.NoError(t, <-done)

		var issued cachepkg.GCGrant
		go func() {
			var err error
			issued, err = client.BeginCacheGC(t.Context(), owner)
			done <- err
		}()
		request = <-stream.requests
		stream.responses <- &componentv1.CacheControlResponse{
			ProtocolVersion: 1, Sequence: request.Sequence, PermitValidForMilliseconds: 15000,
			GcGrant: &componentv1.CacheGCGrant{CacheId: "cache", Id: "next-grant", ConfigRevision: 1, ExpiresAtUnixMilliseconds: time.Now().Add(time.Minute).UnixMilli()},
		}
		require.NoError(t, <-done)
		require.Equal(t, "next-grant", issued.ID)

		go func() { done <- client.CompleteCacheGC(t.Context(), issued) }()
		<-stream.requests
		require.ErrorIs(t, <-done, context.DeadlineExceeded)
		require.Empty(t, client.waiters)

		go func() { done <- client.CompleteCacheGC(t.Context(), issued) }()
		<-stream.requests
		close(stream.responses)
		require.Error(t, <-done)
		synctest.Wait()
		require.Empty(t, client.waiters)
		require.True(t, stream.closed)
	})
}
