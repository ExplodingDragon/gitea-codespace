// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"gitea.dev/codespace/devcontainer"
)

func ensureRuntimeUser(name string, expected *devcontainer.HostUser) (devcontainer.HostUser, error) {
	account, err := user.Lookup(name)
	var unknownUser user.UnknownUserError
	if errors.As(err, &unknownUser) {
		arguments := []string{"--create-home", "--shell", "/bin/bash"}
		if expected != nil {
			if _, groupErr := user.LookupGroupId(strconv.FormatUint(uint64(expected.GID), 10)); groupErr != nil {
				var unknownGroup user.UnknownGroupIdError
				if !errors.As(groupErr, &unknownGroup) {
					return devcontainer.HostUser{}, groupErr
				}
				if output, createErr := exec.Command("groupadd", "--gid", strconv.FormatUint(uint64(expected.GID), 10), name).CombinedOutput(); createErr != nil {
					return devcontainer.HostUser{}, fmt.Errorf("create runtime group: %w: %s", createErr, strings.TrimSpace(string(output)))
				}
			}
			arguments = append(arguments, "--uid", strconv.FormatUint(uint64(expected.UID), 10), "--gid", strconv.FormatUint(uint64(expected.GID), 10))
		}
		arguments = append(arguments, name)
		if output, createErr := exec.Command("useradd", arguments...).CombinedOutput(); createErr != nil {
			return devcontainer.HostUser{}, fmt.Errorf("create runtime user: %w: %s", createErr, strings.TrimSpace(string(output)))
		}
		account, err = user.Lookup(name)
	}
	if err != nil {
		return devcontainer.HostUser{}, err
	}
	uid, uidErr := strconv.ParseUint(account.Uid, 10, 32)
	gid, gidErr := strconv.ParseUint(account.Gid, 10, 32)
	if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 || !filepath.IsAbs(account.HomeDir) {
		return devcontainer.HostUser{}, fmt.Errorf("runtime user identity is invalid")
	}
	result := devcontainer.HostUser{Name: name, UID: uint32(uid), GID: uint32(gid), Home: account.HomeDir}
	if expected != nil && result != *expected {
		return devcontainer.HostUser{}, fmt.Errorf("runtime user identity changed")
	}
	return result, nil
}

func runtimeUserName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var result strings.Builder
	separator := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			result.WriteRune(r)
			separator = false
		case r == '-' || r == '_':
			if !separator {
				result.WriteByte('-')
				separator = true
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r):
		default:
			if !separator {
				result.WriteByte('-')
				separator = true
			}
		}
	}
	name := strings.Trim(result.String(), "-_")
	if name == "" {
		name = "codespace"
	}
	if name[0] >= '0' && name[0] <= '9' || reservedRuntimeUser(name) {
		name = "u-" + name
	}
	if len(name) > 32 {
		name = strings.Trim(name[:32], "-_")
	}
	return name
}

func reservedRuntimeUser(name string) bool {
	switch name {
	case "root", "daemon", "bin", "sys", "sync", "games", "man", "lp", "mail", "news", "uucp", "proxy", "www-data", "backup", "list", "irc", "gnats", "nobody":
		return true
	default:
		return false
	}
}

func (r *Runtime) saveHostUser(hostUser devcontainer.HostUser) error {
	data, err := json.Marshal(hostUser)
	if err != nil {
		return err
	}
	return r.Journal.write("state/host-user.json", data)
}

func (r *Runtime) loadHostUser() (devcontainer.HostUser, error) {
	data, err := r.Journal.read("state/host-user.json", 16*1024)
	if err != nil {
		return devcontainer.HostUser{}, fmt.Errorf("read saved runtime user: %w", err)
	}
	var hostUser devcontainer.HostUser
	if err := json.Unmarshal(data, &hostUser); err != nil || hostUser.Name == "" || hostUser.UID == 0 || hostUser.GID == 0 || !filepath.IsAbs(hostUser.Home) {
		return devcontainer.HostUser{}, fmt.Errorf("saved runtime user is invalid")
	}
	return hostUser, nil
}
