// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/embed"
)

func TestDeploymentEtcdQuorumRecovery(t *testing.T) {
	configs := make([]*embed.Config, 3)
	servers := make([]*embed.Etcd, 3)
	peers := make([]string, 3)
	for i := range configs {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		_ = listener.Close()
		peer := url.URL{Scheme: "http", Host: address}
		config := embed.NewConfig()
		config.Name = fmt.Sprintf("member%d", i)
		config.Dir = filepath.Join(t.TempDir(), "data")
		config.ListenPeerUrls = []url.URL{peer}
		config.AdvertisePeerUrls = []url.URL{peer}
		config.ListenClientUrls = []url.URL{{Scheme: "http", Host: "127.0.0.1:0"}}
		config.AdvertiseClientUrls = config.ListenClientUrls
		config.LogLevel = "error"
		configs[i] = config
		peers[i] = config.Name + "=" + peer.String()
	}
	t.Cleanup(func() {
		for _, server := range servers {
			if server != nil {
				server.Close()
			}
		}
	})
	for i, config := range configs {
		config.InitialCluster = strings.Join(peers, ",")
		server, err := embed.StartEtcd(config)
		if err != nil {
			t.Fatal(err)
		}
		servers[i] = server
	}
	endpoints := make([]string, len(servers))
	for i, server := range servers {
		select {
		case <-server.Server.ReadyNotify():
		case <-time.After(20 * time.Second):
			t.Fatal("etcd cluster did not become ready")
		}
		endpoints[i] = "http://" + server.Clients[0].Addr().String()
	}
	client, err := clientv3.New(clientv3.Config{Endpoints: endpoints, DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	store := &etcdInfrastructureStore{client: client, prefix: "/election-test"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	leader, err := store.waitForLeadership(ctx, "executor")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = leader.close(ctx) }()
	// Losing one etcd member preserves quorum and execution ownership.
	servers[0].Close()
	servers[0] = nil
	if err := leader.commit(ctx, nil, clientv3.OpPut(store.key("checkpoint"), "committed")); err != nil {
		t.Fatal(err)
	}
	servers[1].Close()
	servers[1] = nil
	writeCtx, stopWrite := context.WithTimeout(ctx, time.Second)
	err = leader.commit(writeCtx, nil, clientv3.OpPut(store.key("checkpoint"), "unconfirmed"))
	stopWrite()
	if err == nil {
		t.Fatal("execution write succeeded without etcd quorum")
	}
	// Restore the same member data, not a new empty cluster.
	servers[1], err = embed.StartEtcd(configs[1])
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-servers[1].Server.ReadyNotify():
	case <-ctx.Done():
		t.Fatal("etcd quorum was not restored")
	}
	client.SetEndpoints("http://"+servers[1].Clients[0].Addr().String(), endpoints[2])
	if err := leader.close(ctx); err != nil {
		t.Fatal(err)
	}
	replacement, err := store.waitForLeadership(ctx, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = replacement.close(ctx) }()
	if err := replacement.commit(ctx, nil, clientv3.OpPut(store.key("checkpoint"), "recovered")); err != nil {
		t.Fatal(err)
	}
	if err := leader.commit(ctx, nil, clientv3.OpPut(store.key("checkpoint"), "stale")); err == nil {
		t.Fatal("old execution term wrote after recovery")
	}
}
