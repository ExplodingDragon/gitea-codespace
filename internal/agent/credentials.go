// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/devcontainer"
	"gitea.dev/codespace/internal/controlplane"
	"gitea.dev/codespace/internal/devcontainerruntime"
	"gitea.dev/codespace/internal/runtimeplatform"
	"golang.org/x/crypto/ssh"
)

var secretNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func (r *Runtime) prepareAccess(access *codespacev1.RuntimeAccessBundle, keyType string, owner devcontainer.HostUser) (map[string]string, error) {
	if access == nil || strings.TrimSpace(access.GiteaToken) == "" {
		return nil, fmt.Errorf("gitea access token is empty")
	}
	giteaURL, err := controlplane.CanonicalGiteaURL(access.GiteaServerUrl)
	if err != nil {
		return nil, fmt.Errorf("gitea server URL: %w", err)
	}
	secrets, err := runtimeAccessEnvironment(access.Secrets, giteaURL, access.GiteaToken, r.SSHEnvironment)
	if err != nil {
		return nil, err
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
	if err := os.Chmod(runtimeRunDirectory, 0o711); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(runtimeGitDirectory, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chown(runtimeGitDirectory, int(owner.UID), int(owner.GID)); err != nil {
		return nil, err
	}
	if err := os.Chmod(runtimeGitDirectory, 0o700); err != nil {
		return nil, err
	}
	files := []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{runtimeGiteaTokenPath, []byte(access.GiteaToken), 0o600},
		{runtimeGitDirectory + "/id", privateKey, 0o600},
		{runtimeGitDirectory + "/id.pub", ssh.MarshalAuthorizedKey(signer.PublicKey()), 0o644},
		{runtimeGitDirectory + "/known_hosts", []byte(knownHosts), 0o600},
	}
	for _, file := range files {
		if err := replaceFileAtomically(file.path, file.data, file.mode, &owner); err != nil {
			return nil, err
		}
	}
	if err := writeAccessEnvironment(devcontainerruntime.RuntimeEnvironmentPath, secrets, owner); err != nil {
		return nil, err
	}
	return secrets, nil
}

func runtimeAccessEnvironment(items []*codespacev1.RuntimeSecretEnvironmentVariable, giteaURL, token string, sshEnvironment map[string]string) (map[string]string, error) {
	if strings.ContainsRune(giteaURL, 0) || strings.ContainsRune(token, 0) {
		return nil, fmt.Errorf("runtime Gitea credentials contain NUL")
	}
	secrets := make(map[string]string, len(items)+2)
	for _, item := range items {
		if item == nil || !secretNamePattern.MatchString(item.Name) || strings.ContainsRune(item.Value, 0) {
			return nil, fmt.Errorf("runtime secret is invalid")
		}
		if _, exists := secrets[item.Name]; exists {
			return nil, fmt.Errorf("runtime secret name is duplicated")
		}
		secrets[item.Name] = item.Value
	}
	secrets["GITEA_INSTANCE_URL"] = giteaURL
	secrets["GITEA_TOKEN"] = token
	for _, name := range runtimeplatform.SSHEnvironmentNames {
		secrets[name] = sshEnvironment[name]
	}
	return secrets, nil
}

func writeAccessEnvironment(path string, environment map[string]string, owner devcontainer.HostUser) error {
	var shell, systemd strings.Builder
	escape := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "$", "\\$", "`", "\\`")
	for _, name := range slices.Sorted(maps.Keys(environment)) {
		value := environment[name]
		if !utf8.ValidString(value) {
			return fmt.Errorf("runtime environment variable %s is not valid UTF-8", name)
		}
		for _, char := range value {
			if char == 0 || char == '\uFEFF' || (char >= '\uFDD0' && char <= '\uFDEF') || char&0xFFFF >= 0xFFFE {
				return fmt.Errorf("runtime environment variable %s contains a character unsupported by systemd EnvironmentFile", name)
			}
		}
		_, _ = fmt.Fprintf(&shell, "export %s='%s'\n", name, strings.ReplaceAll(value, "'", "'\\''"))
		// EnvironmentFile keeps literal newlines inside double quotes.
		_, _ = fmt.Fprintf(&systemd, "%s=\"%s\"\n", name, escape.Replace(value))
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o711); err != nil {
		return fmt.Errorf("prepare runtime authorization environment: %w", err)
	}
	files := []struct {
		name    string
		content string
		mode    os.FileMode
		owner   *devcontainer.HostUser
	}{
		{filepath.Base(path), shell.String(), 0o600, &owner},
		{filepath.Base(devcontainerruntime.ContainerSystemdEnvironmentPath), systemd.String(), 0o600, nil},
		{filepath.Base(devcontainerruntime.RuntimeSystemdDropInPath), "[Service]\nEnvironmentFile=" + devcontainerruntime.ContainerSystemdEnvironmentPath + "\n", 0o644, nil},
	}
	// The Agent runs as root; only the shell file belongs to remoteUser.
	for _, file := range files {
		if err := replaceFileAtomically(filepath.Join(directory, file.name), []byte(file.content), file.mode, file.owner); err != nil {
			return fmt.Errorf("write runtime authorization environment %s: %w", file.name, err)
		}
	}
	return nil
}

func gitCredentialCommand(tokenPath string) string {
	return `!f() { while IFS= read -r line; do [ -n "$line" ] || break; done; printf 'username=codespace\npassword=%s\n\n' "$(cat ` + tokenPath + `)"; }; f`
}

func gitSSHCommand(directory string) string {
	return "ssh -i " + directory + "/id -o IdentitiesOnly=yes -o UserKnownHostsFile=" + directory + "/known_hosts -o StrictHostKeyChecking=yes"
}
