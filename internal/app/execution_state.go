// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// executionStateStore stores recovery checkpoints under the current leader term.
// Binding ciphertext to its key prevents moving a valid record to another runtime.
type executionStateStore struct {
	store  *etcdInfrastructureStore
	leader *deploymentLeadership
	siteID int64
}

func (s *executionStateStore) list(prefix string) ([]string, error) {
	ctx, cancel := context.WithTimeout(s.leader.session.Ctx(), 5*time.Second)
	defer cancel()
	response, err := s.store.client.Get(ctx, s.key(prefix), clientv3.WithPrefix(), clientv3.WithKeysOnly())
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(response.Kvs))
	for _, item := range response.Kvs {
		names = append(names, strings.TrimPrefix(string(item.Key), s.key("")))
	}
	return names, nil
}

func (s *executionStateStore) key(name string) string {
	return s.store.key("execution/" + strconv.FormatInt(s.siteID, 10) + "/" + name)
}

func (s *executionStateStore) load(name string) ([]byte, int64, error) {
	ctx, cancel := context.WithTimeout(s.leader.session.Ctx(), 5*time.Second)
	defer cancel()
	key := s.key(name)
	response, err := s.store.client.Get(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	if len(response.Kvs) == 0 {
		return nil, 0, fs.ErrNotExist
	}
	codec := s.store.secret
	codec.aad = append(append([]byte(nil), codec.aad...), []byte(key)...)
	content, err := codec.decrypt(string(response.Kvs[0].Value))
	return []byte(content), response.Kvs[0].ModRevision, err
}

func (s *executionStateStore) save(name string, content []byte, revision int64) error {
	key := s.key(name)
	codec := s.store.secret
	codec.aad = append(append([]byte(nil), codec.aad...), []byte(key)...)
	encrypted, err := codec.encrypt(string(content))
	if err != nil {
		return err
	}
	// Leave room for the transaction and key below etcd's default request limit.
	if len(encrypted) > 1024*1024 {
		return fmt.Errorf("execution checkpoint exceeds 1 MiB")
	}
	ctx, cancel := context.WithTimeout(s.leader.session.Ctx(), 5*time.Second)
	defer cancel()
	return s.leader.commit(ctx, []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(key), "=", revision)}, clientv3.OpPut(key, encrypted))
}

func (s *executionStateStore) remove(name string, revision int64) error {
	key := s.key(name)
	ctx, cancel := context.WithTimeout(s.leader.session.Ctx(), 5*time.Second)
	defer cancel()
	return s.leader.commit(ctx, []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(key), "=", revision)}, clientv3.OpDelete(key))
}
