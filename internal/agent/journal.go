// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

// VolumeIdentity binds recovery data to the Kubernetes objects that own the PVC.
type VolumeIdentity struct {
	SiteUID     string `json:"site_uid"`
	ResourceUID string `json:"resource_uid"`
	RuntimeUUID string `json:"runtime_uuid"`
}

type stageResult struct {
	Complete  bool `json:"complete"`
	Succeeded bool `json:"succeeded"`
}

type executionRecord struct {
	Identity VolumeIdentity         `json:"identity"`
	Version  int64                  `json:"operation_version"`
	Started  int64                  `json:"started_unix,omitempty"`
	Stages   map[string]stageResult `json:"stages,omitempty"`
}

// Journal keeps side-effect completion facts and the private key on one PVC.
// Its lock supplements, but does not replace, Kubernetes writer termination checks.
type Journal struct {
	mu     sync.Mutex
	root   *os.Root
	lock   *os.File
	record executionRecord
	active bool
}

func OpenJournal(directory string, identity VolumeIdentity) (_ *Journal, err error) {
	if identity.SiteUID == "" || identity.ResourceUID == "" || identity.RuntimeUUID == "" {
		return nil, fmt.Errorf("runtime volume identity is incomplete")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, fmt.Errorf("open runtime volume: %w", err)
	}
	j := &Journal{root: root}
	defer func() {
		if err != nil {
			_ = j.Close()
		}
	}()
	for _, name := range []string{"state", "identity", "logs", "workspaces"} {
		if err := root.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	j.lock, err = root.OpenFile("state/lock", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := j.lock.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("runtime writer lock is not a regular file")
	}
	if err := unix.Flock(int(j.lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, fmt.Errorf("runtime volume already has an active writer: %w", err)
	}
	data, err := j.read("state/execution.json", 1024*1024)
	if errors.Is(err, os.ErrNotExist) {
		// Missing bookkeeping cannot turn an existing Docker volume into a fresh
		// create: that would replay user commands whose completion is unknown.
		for _, name := range []string{"state", "identity", "logs", "workspaces", "docker", "containerd"} {
			directory, err := root.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			entries, readErr := directory.Readdirnames(2)
			closeErr := directory.Close()
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return nil, readErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			for _, entry := range entries {
				if name != "state" || entry != "lock" {
					return nil, fmt.Errorf("runtime execution record is missing from an existing volume")
				}
			}
		}
		j.record.Identity = identity
		if err := j.save(); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if err := json.Unmarshal(data, &j.record); err != nil {
		return nil, fmt.Errorf("decode runtime execution record: %w", err)
	} else if j.record.Identity != identity {
		return nil, fmt.Errorf("runtime volume belongs to another Codespace")
	}
	if j.record.Version < 0 || j.record.Started < 0 || len(j.record.Stages) > 64 || (j.record.Version == 0 && (j.record.Started != 0 || len(j.record.Stages) != 0)) || (j.record.Version > 0 && j.record.Started == 0) {
		return nil, fmt.Errorf("invalid runtime execution record")
	}
	for name, result := range j.record.Stages {
		if name == "" || len(name) > 128 || (!result.Complete && result.Succeeded) {
			return nil, fmt.Errorf("invalid runtime execution stage")
		}
	}
	if j.record.Stages == nil {
		j.record.Stages = make(map[string]stageResult)
	}
	return j, nil
}

// BeginOperation returns one stable origin for boot progress. It is persisted
// before side effects so a replacement Pod reports the same operation start.
func (j *Journal) BeginOperation(version int64, now int64) (int64, error) {
	if version <= 0 || now <= 0 {
		return 0, fmt.Errorf("operation start identity is invalid")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.active || j.record.Version > version {
		return 0, fmt.Errorf("operation start conflicts with execution history")
	}
	if j.record.Version < version {
		j.record.Version, j.record.Started = version, now
		j.record.Stages = make(map[string]stageResult)
		if err := j.save(); err != nil {
			return 0, err
		}
	}
	if j.record.Started <= 0 {
		return 0, fmt.Errorf("operation start is missing from execution history")
	}
	return j.record.Started, nil
}

func (j *Journal) Close() error {
	var err error
	if j.lock != nil {
		err = j.lock.Close()
	}
	return errors.Join(err, j.root.Close())
}

// RunStage records intent before invoking a side effect. An interrupted invocation
// is not replayed because its external result cannot be inferred from a missing ack.
func (j *Journal) RunStage(ctx context.Context, version int64, name string, run func(context.Context) error) error {
	if version <= 0 || name == "" || len(name) > 128 || run == nil {
		return fmt.Errorf("execution stage identity is invalid")
	}
	j.mu.Lock()
	if j.active {
		j.mu.Unlock()
		return fmt.Errorf("previous execution stage is still running")
	}
	if j.record.Version > version {
		j.mu.Unlock()
		return fmt.Errorf("execution operation version is stale")
	}
	if j.record.Version < version {
		j.record.Version = version
		j.record.Started = time.Now().Unix()
		j.record.Stages = make(map[string]stageResult)
	}
	if result, exists := j.record.Stages[name]; exists {
		j.mu.Unlock()
		if !result.Complete {
			return fmt.Errorf("execution stage %s has an unknown result after interruption", name)
		}
		if !result.Succeeded {
			return fmt.Errorf("execution stage %s previously failed", name)
		}
		return nil
	}
	if err := ctx.Err(); err != nil {
		j.mu.Unlock()
		return err
	}
	if len(j.record.Stages) >= 64 {
		j.mu.Unlock()
		return fmt.Errorf("operation contains too many execution stages")
	}
	j.record.Stages[name] = stageResult{}
	err := j.save()
	j.active = err == nil
	j.mu.Unlock()
	if err != nil {
		return err
	}
	runErr := run(ctx)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.active = false
	j.record.Stages[name] = stageResult{Complete: true, Succeeded: runErr == nil}
	if err := j.save(); err != nil {
		j.record.Stages[name] = stageResult{}
		return errors.Join(runErr, err)
	}
	return runErr
}

// GitSSHKey durably creates the private key before its public half can be reported.
// The public half is derived, so interruption cannot leave a mismatched key pair.
// allowCreate is granted only during first creation, never during resume.
func (j *Journal) GitSSHKey(keyType string, allowCreate bool) (ssh.Signer, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if keyType != "ed25519" && keyType != "rsa-4096" {
		return nil, fmt.Errorf("unsupported Git SSH key type %q", keyType)
	}
	data, err := j.read("identity/git-key", 32*1024)
	if errors.Is(err, os.ErrNotExist) {
		if !allowCreate {
			return nil, fmt.Errorf("persistent Git SSH key is missing; identity recovery is required")
		}
		var key crypto.PrivateKey
		if keyType == "ed25519" {
			_, key, err = ed25519.GenerateKey(rand.Reader)
		} else {
			key, err = rsa.GenerateKey(rand.Reader, 4096)
		}
		if err != nil {
			return nil, err
		}
		block, err := ssh.MarshalPrivateKey(key, "gitea-codespace")
		if err != nil {
			return nil, err
		}
		data = pem.EncodeToMemory(block)
		if err := j.write("identity/git-key", data); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	key, err := ssh.ParseRawPrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("read persistent Git SSH key: %w", err)
	}
	switch key := key.(type) {
	case *ed25519.PrivateKey:
		if keyType != "ed25519" {
			return nil, fmt.Errorf("persistent Git SSH key does not match the pinned algorithm")
		}
	case *rsa.PrivateKey:
		if keyType != "rsa-4096" || key.N.BitLen() != 4096 {
			return nil, fmt.Errorf("persistent Git SSH key does not match the pinned algorithm")
		}
	default:
		return nil, fmt.Errorf("persistent Git SSH key has an unsupported algorithm")
	}
	return ssh.NewSignerFromKey(key)
}

// GitSSHPrivateKey returns the validated private key encoding used by Git in
// the current Runtime Pod. Callers expose it only through the Pod tmpfs.
func (j *Journal) GitSSHPrivateKey(keyType string) ([]byte, error) {
	if _, err := j.GitSSHKey(keyType, false); err != nil {
		return nil, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	data, err := j.read("identity/git-key", 32*1024)
	return append([]byte(nil), data...), err
}

func (j *Journal) read(name string, limit int64) ([]byte, error) {
	file, err := j.root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > limit {
		return nil, fmt.Errorf("invalid runtime state file %s", name)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("runtime state file %s exceeds the size limit", name)
	}
	return data, err
}

func (j *Journal) save() error {
	data, err := json.Marshal(j.record)
	if err != nil {
		return err
	}
	return j.write("state/execution.json", data)
}

func (j *Journal) write(name string, data []byte) error {
	temporary := name + "." + uuid.NewString()
	file, err := j.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = j.root.Remove(temporary) }()
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err := j.root.Rename(temporary, name); err != nil {
		return err
	}
	parent, err := j.root.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}
