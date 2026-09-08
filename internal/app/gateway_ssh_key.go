// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/crypto/ssh"
)

type gatewaySSHHostKey struct {
	signer            ssh.Signer
	algorithm         string
	fingerprintSHA256 string
	updatedUnix       int64
}

// LoadGatewaySSHHostKey shares one encrypted SSH identity across all gateway nodes.
func (s *etcdInfrastructureStore) LoadGatewaySSHHostKey(ctx context.Context) (gatewaySSHHostKey, error) {
	key := s.key("gateway-host-key")
	response, err := s.client.Get(ctx, key)
	if err != nil {
		return gatewaySSHHostKey{}, err
	}
	var record struct {
		PrivateKey  string `json:"private_key"`
		UpdatedUnix int64  `json:"updated_unix"`
	}
	if len(response.Kvs) == 0 {
		_, privateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return gatewaySSHHostKey{}, err
		}
		block, err := ssh.MarshalPrivateKey(privateKey, "gitea-codespace")
		if err != nil {
			return gatewaySSHHostKey{}, err
		}
		record.PrivateKey, err = s.secret.encrypt(string(pem.EncodeToMemory(block)))
		if err != nil {
			return gatewaySSHHostKey{}, err
		}
		record.UpdatedUnix = time.Now().Unix()
		encoded, err := json.Marshal(record)
		if err != nil {
			return gatewaySSHHostKey{}, err
		}
		result, err := s.client.Txn(ctx).
			If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
			Then(clientv3.OpPut(key, string(encoded))).
			Else(clientv3.OpGet(key)).Commit()
		if err != nil {
			return gatewaySSHHostKey{}, err
		}
		if !result.Succeeded {
			values := result.Responses[0].GetResponseRange().Kvs
			if len(values) != 1 {
				return gatewaySSHHostKey{}, fmt.Errorf("shared SSH host key disappeared")
			}
			if err := json.Unmarshal(values[0].Value, &record); err != nil {
				return gatewaySSHHostKey{}, err
			}
		}
	} else if err := json.Unmarshal(response.Kvs[0].Value, &record); err != nil {
		return gatewaySSHHostKey{}, err
	}
	content, err := s.secret.decrypt(record.PrivateKey)
	if err != nil {
		return gatewaySSHHostKey{}, err
	}
	signer, err := ssh.ParsePrivateKey([]byte(content))
	if err != nil {
		return gatewaySSHHostKey{}, fmt.Errorf("parse shared SSH host key: %w", err)
	}
	if record.UpdatedUnix <= 0 {
		return gatewaySSHHostKey{}, fmt.Errorf("shared SSH host key creation time is invalid")
	}
	return gatewaySSHHostKey{signer: signer, algorithm: signer.PublicKey().Type(),
		fingerprintSHA256: ssh.FingerprintSHA256(signer.PublicKey()), updatedUnix: record.UpdatedUnix}, nil
}
