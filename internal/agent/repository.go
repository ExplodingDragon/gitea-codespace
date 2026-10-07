// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	"gitea.dev/codespace/devcontainer"
)

func repositoryWorkspace(repository *codespacev1.RepositoryCheckout) (string, error) {
	fullName := strings.Trim(strings.TrimSpace(repository.FullName), "/")
	parts := strings.Split(fullName, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(parts[1], `\\\x00`) {
		return "", fmt.Errorf("repository full name is invalid")
	}
	return filepath.Join(workspaceRoot, parts[1]), nil
}

func cloneRepository(ctx context.Context, operation *codespacev1.OperationPayload, repository *codespacev1.RepositoryCheckout, owner devcontainer.HostUser, stdout, stderr io.Writer) error {
	target, err := repositoryWorkspace(repository)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		return err
	}
	if err := os.Chown(workspaceRoot, int(owner.UID), int(owner.GID)); err != nil {
		return err
	}
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("workspace path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	urls := []string{strings.TrimSpace(repository.CloneHttpUrl), strings.TrimSpace(repository.CloneSshUrl)}
	if repository.PreferredProtocol == codespacev1.GitProtocol_GIT_PROTOCOL_SSH {
		urls[0], urls[1] = urls[1], urls[0]
	}
	var failures []error
	for index, cloneURL := range urls {
		if cloneURL == "" || (index == 1 && cloneURL == urls[0]) {
			continue
		}
		temporary := filepath.Join(workspaceRoot, ".create-"+operation.RuntimeUuid)
		if err := os.RemoveAll(temporary); err != nil {
			return err
		}
		if err := runGit(ctx, owner, "", stdout, stderr, cloneURL, "clone", "--no-checkout", cloneURL, temporary); err != nil {
			failures = append(failures, err)
			continue
		}
		if err := configureRepositoryCredentials(ctx, owner, temporary, cloneURL); err != nil {
			failures = append(failures, err)
			_ = os.RemoveAll(temporary)
			continue
		}
		if err := checkoutRepository(ctx, owner, temporary, repository, stdout, stderr); err != nil {
			failures = append(failures, err)
			_ = os.RemoveAll(temporary)
			continue
		}
		if err := os.Rename(temporary, target); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("clone repository: %w", errors.Join(failures...))
}

func configureRepositoryCredentials(ctx context.Context, owner devcontainer.HostUser, directory, cloneURL string) error {
	var key, value string
	if strings.HasPrefix(cloneURL, "http://") || strings.HasPrefix(cloneURL, "https://") {
		key, value = "credential.helper", "!"+runtimeRunDirectory+"/bin/gitea-codespace-git-credential"
	} else {
		key, value = "core.sshCommand", runtimeRunDirectory+"/bin/gitea-codespace-git-ssh"
	}
	command := exec.CommandContext(ctx, "git", "-C", directory, "config", key, value)
	configureGitCommand(command, owner, "")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("configure repository credentials: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func verifyWorkspace(ctx context.Context, owner devcontainer.HostUser, directory, commit string) error {
	command := exec.CommandContext(ctx, "git", "-C", directory, "rev-parse", "HEAD")
	configureGitCommand(command, owner, "")
	output, err := command.Output()
	if err != nil || strings.TrimSpace(string(output)) != strings.ToLower(commit) {
		return fmt.Errorf("workspace commit does not match create input")
	}
	return nil
}

func checkoutRepository(ctx context.Context, owner devcontainer.HostUser, directory string, repository *codespacev1.RepositoryCheckout, stdout, stderr io.Writer) error {
	ref, commit := strings.TrimSpace(repository.StartRef), strings.ToLower(repository.CommitSha)
	run := func(arguments ...string) error {
		return runGit(ctx, owner, directory, stdout, stderr, "", arguments...)
	}
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		branch := strings.TrimPrefix(ref, "refs/heads/")
		if branch == "" || strings.ContainsAny(branch, " ~^:?*[\\") {
			return fmt.Errorf("repository branch is invalid")
		}
		if err := run("fetch", "origin", "+"+ref+":refs/remotes/origin/"+branch, "--prune"); err != nil {
			return err
		}
		if err := run("checkout", "-B", branch, commit); err != nil {
			return err
		}
		if err := run("branch", "--set-upstream-to=origin/"+branch, branch); err != nil {
			return err
		}
	case strings.HasPrefix(ref, "refs/tags/"), strings.HasPrefix(ref, "refs/pull/") && strings.HasSuffix(ref, "/head"):
		if err := run("fetch", "origin", ref, "--prune"); err != nil {
			return err
		}
		if err := run("checkout", "--detach", commit); err != nil {
			return err
		}
	case ref == "":
		if err := run("fetch", "--all", "--tags", "--prune"); err != nil {
			return err
		}
		if err := run("checkout", "--detach", commit); err != nil {
			return err
		}
	default:
		return fmt.Errorf("repository start ref is invalid")
	}
	return verifyWorkspace(ctx, owner, directory, commit)
}

func runGit(ctx context.Context, owner devcontainer.HostUser, directory string, stdout, stderr io.Writer, cloneURL string, arguments ...string) error {
	if directory != "" {
		arguments = append([]string{"-C", directory}, arguments...)
	}
	command := exec.CommandContext(ctx, "git", arguments...)
	configureGitCommand(command, owner, cloneURL)
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("git %s failed: %w", arguments[0], err)
	}
	return nil
}

func configureGitCommand(command *exec.Cmd, owner devcontainer.HostUser, cloneURL string) {
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: owner.UID, Gid: owner.GID}}
	command.Env = append(os.Environ(), "HOME="+owner.Home)
	if strings.HasPrefix(cloneURL, "http://") || strings.HasPrefix(cloneURL, "https://") {
		command.Env = append(command.Env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=!"+runtimeRunDirectory+"/bin/gitea-codespace-git-credential")
	} else if cloneURL != "" {
		command.Env = append(command.Env, "GIT_SSH_COMMAND="+runtimeRunDirectory+"/bin/gitea-codespace-git-ssh")
	}
}
