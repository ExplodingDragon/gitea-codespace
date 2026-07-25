// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package app

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

const gatewaySSHHostKeyFileName = "gateway-ssh-host-key"

type gatewaySSHHostKey struct {
	signer            ssh.Signer
	algorithm         string
	fingerprintSHA256 string
	updatedUnix       int64
}

func loadOrCreateGatewaySSHHostKey(stateDir string) (gatewaySSHHostKey, error) {
	path, err := gatewaySSHKeyPath(stateDir, gatewaySSHHostKeyFileName)
	if err != nil {
		return gatewaySSHHostKey{}, err
	}
	content, err := loadOrCreateGatewaySSHKeyFile(path)
	if err != nil {
		return gatewaySSHHostKey{}, err
	}
	signer, err := ssh.ParsePrivateKey(content)
	if err != nil {
		return gatewaySSHHostKey{}, fmt.Errorf("parse gateway ssh host key %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return gatewaySSHHostKey{}, fmt.Errorf("stat gateway ssh host key %s: %w", path, err)
	}
	return gatewaySSHHostKey{
		signer:            signer,
		algorithm:         signer.PublicKey().Type(),
		fingerprintSHA256: ssh.FingerprintSHA256(signer.PublicKey()),
		updatedUnix:       info.ModTime().Unix(),
	}, nil
}

func gatewaySSHKeyPath(stateDir, name string) (string, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return "", fmt.Errorf("manager.state_dir is required")
	}
	return filepath.Join(stateDir, name), nil
}

func loadOrCreateGatewaySSHKeyFile(path string) ([]byte, error) {
	content, err := os.ReadFile(path)
	if err == nil {
		return content, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read gateway ssh key %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", filepath.Dir(path), err)
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate gateway ssh host key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(privateKey, "gitea-codespace")
	if err != nil {
		return nil, fmt.Errorf("marshal gateway ssh host key: %w", err)
	}
	content = pem.EncodeToMemory(block)
	if len(content) == 0 {
		return nil, fmt.Errorf("encode gateway ssh host key")
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return nil, fmt.Errorf("write gateway ssh host key %s: %w", path, err)
	}
	return content, nil
}
