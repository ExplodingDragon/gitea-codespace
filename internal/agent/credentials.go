// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/devcontainer"
	"golang.org/x/crypto/ssh"
)

var secretNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func (r *Runtime) prepareAccess(access *codespacev1.RuntimeAccessBundle, keyType string, owner devcontainer.HostUser) (map[string]string, error) {
	if access == nil || strings.TrimSpace(access.GiteaToken) == "" {
		return nil, fmt.Errorf("gitea access token is empty")
	}
	serverURL, err := url.Parse(strings.TrimSpace(access.GiteaServerUrl))
	if err != nil || (serverURL.Scheme != "http" && serverURL.Scheme != "https") || serverURL.Host == "" || serverURL.User != nil || serverURL.RawQuery != "" || serverURL.Fragment != "" {
		return nil, fmt.Errorf("gitea server URL is invalid")
	}
	privateKey, err := r.Journal.GitSSHPrivateKey(keyType)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	knownHosts := strings.Join(access.GetGitSshTrust().GetKnownHostsLines(), "\n")
	if knownHosts != "" {
		knownHosts += "\n"
		for remaining := []byte(knownHosts); len(remaining) > 0; {
			_, _, _, _, rest, parseErr := ssh.ParseKnownHosts(remaining)
			if parseErr != nil {
				return nil, fmt.Errorf("git SSH known_hosts is invalid: %w", parseErr)
			}
			remaining = rest
		}
	}
	for _, directory := range []string{runtimeRunDirectory + "/git", runtimeRunDirectory + "/bin", runtimeVolume + "/runtime"} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, err
		}
	}
	files := []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{runtimeRunDirectory + "/gitea-token", []byte(access.GiteaToken), 0o600},
		{runtimeRunDirectory + "/git/id", privateKey, 0o600},
		{runtimeRunDirectory + "/git/id.pub", ssh.MarshalAuthorizedKey(signer.PublicKey()), 0o644},
		{runtimeRunDirectory + "/git/known_hosts", []byte(knownHosts), 0o600},
		{runtimeRunDirectory + "/bin/gitea-codespace-git-credential", []byte(gitCredentialHelper), 0o755},
		{runtimeRunDirectory + "/bin/gitea-codespace-git-ssh", []byte(gitSSHHelper), 0o755},
	}
	for _, file := range files {
		if err := writeCredential(file.path, file.data, file.mode, owner); err != nil {
			return nil, err
		}
	}
	secrets := make(map[string]string, len(access.Secrets))
	for _, item := range access.Secrets {
		if item == nil || !secretNamePattern.MatchString(item.Name) || strings.ContainsRune(item.Value, 0) {
			return nil, fmt.Errorf("runtime secret is invalid")
		}
		if _, exists := secrets[item.Name]; exists {
			return nil, fmt.Errorf("runtime secret name is duplicated")
		}
		secrets[item.Name] = item.Value
	}
	return secrets, nil
}

func writeCredential(path string, data []byte, mode os.FileMode, owner devcontainer.HostUser) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".credential-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chown(int(owner.UID), int(owner.GID)); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

const gitCredentialHelper = `#!/bin/sh
set -eu
while IFS= read -r line; do [ -n "$line" ] || break; done
printf 'username=codespace\npassword=%s\n\n' "$(cat /var/lib/gitea-codespace/gitea-token)"
`

const gitSSHHelper = `#!/bin/sh
set -eu
exec ssh -i /var/lib/gitea-codespace/git/id -o IdentitiesOnly=yes -o UserKnownHostsFile=/var/lib/gitea-codespace/git/known_hosts -o StrictHostKeyChecking=yes "$@"
`
