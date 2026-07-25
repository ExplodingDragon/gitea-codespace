// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

const builtinInitScript = `
set -eu
write_result() {
  tmp="${CODESPACE_RESULT}.tmp.$$"
  umask 177
  printf '{"outcome":"done","stage":"initialize-system"}\n' > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$CODESPACE_RESULT"
}
cleanup_policy_rc() {
  if [ "${policy_rc_created:-}" = "1" ]; then
    rm -f /usr/sbin/policy-rc.d
  fi
}
trap cleanup_policy_rc EXIT
install_missing() {
  missing=""
  for command in git ssh sudo flock getent useradd groupadd; do
    if ! command -v "$command" >/dev/null 2>&1; then
      missing="$missing $command"
    fi
  done
  if [ -z "$missing" ]; then
    return 0
  fi
  if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    if [ ! -e /usr/sbin/policy-rc.d ]; then
      printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d
      chmod 0755 /usr/sbin/policy-rc.d
      policy_rc_created=1
    fi
    apt-get update
    apt-get install -y --no-install-recommends ca-certificates curl git openssh-client sudo util-linux passwd python3
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y --setopt=install_weak_deps=False ca-certificates curl git openssh-clients sudo util-linux shadow-utils python3
  elif command -v pacman >/dev/null 2>&1; then
    pacman -Sy --noconfirm ca-certificates curl git openssh sudo util-linux shadow python
  else
    exit 20
  fi
}
install_missing
if getent group codespace >/dev/null 2>&1; then
  if [ "$(getent group codespace | cut -d: -f3)" != "1000" ]; then
    exit 21
  fi
else
  groupadd -g 1000 codespace
fi
if id -u codespace >/dev/null 2>&1; then
  if [ "$(id -u codespace)" != "1000" ] || [ "$(id -g codespace)" != "1000" ]; then
    exit 22
  fi
else
  useradd -m -u 1000 -g 1000 -s /bin/bash codespace
fi
passwd -l codespace >/dev/null 2>&1 || true
	mkdir -p /workspaces /var/lib/gitea-codespace /var/lib/gitea-codespace/git /var/lib/gitea-codespace/runtime /var/lib/gitea-codespace/results
chown codespace:codespace /workspaces
chmod 0755 /workspaces
	cat >/usr/local/bin/gitea-codespace-git-credential <<'EOF'
#!/bin/sh
set -eu
while IFS= read -r line; do
  [ -n "$line" ] || break
done
printf 'username=codespace\n'
printf 'password=%s\n\n' "$(cat /var/lib/gitea-codespace/gitea-token)"
	EOF
	chmod 0755 /usr/local/bin/gitea-codespace-git-credential
	cat >/usr/local/bin/gitea-codespace-endpoint <<'EOF'
	#!/usr/bin/env python3
	import json
	import os
	import sys

	manifest = "/var/lib/gitea-codespace/runtime/endpoints.json"
	if len(sys.argv) < 2 or sys.argv[1] not in ("set", "delete"):
	    print("usage: gitea-codespace-endpoint set <id> <label> <http|https> <port> [--public] | delete <id>", file=sys.stderr)
	    sys.exit(2)
	try:
	    with open(manifest, "r", encoding="utf-8") as handle:
	        data = json.load(handle)
	except FileNotFoundError:
	    data = {"version": 1, "endpoints": []}
	endpoints = [entry for entry in data.get("endpoints", []) if entry.get("endpoint_id") != sys.argv[2]]
	if sys.argv[1] == "set":
	    if len(sys.argv) not in (6, 7):
	        sys.exit(2)
	    public = len(sys.argv) == 7 and sys.argv[6] == "--public"
	    endpoints.append({
	        "endpoint_id": sys.argv[2],
	        "label": sys.argv[3],
	        "upstream_scheme": sys.argv[4],
	        "upstream_port": int(sys.argv[5]),
	        "public": public,
	    })
	data = {"version": 1, "endpoints": endpoints}
	tmp = manifest + ".tmp." + str(os.getpid())
	with open(tmp, "w", encoding="utf-8") as handle:
	    json.dump(data, handle, separators=(",", ":"), sort_keys=True)
	    handle.write("\n")
	    handle.flush()
	    os.fsync(handle.fileno())
	os.replace(tmp, manifest)
	EOF
	chmod 0755 /usr/local/bin/gitea-codespace-endpoint
printf 'codespace ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/gitea-codespace
chmod 0440 /etc/sudoers.d/gitea-codespace
if command -v visudo >/dev/null 2>&1; then
  visudo -cf /etc/sudoers.d/gitea-codespace >/dev/null
fi
printf '%s\n' 'CODESPACE_CREDENTIAL_UID=1000' 'CODESPACE_CREDENTIAL_GID=1000' >> "$CODESPACE_ENV"
write_result
`

const builtinStartScript = `
set -eu
write_result() {
  tmp="${CODESPACE_RESULT}.tmp.$$"
  umask 177
  printf '{"outcome":"done","stage":"%s"}\n' "$1" > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$CODESPACE_RESULT"
}
repo_url="$GITEA_REPO_CLONE_HTTP_URL"
if [ "$GITEA_GIT_PROTOCOL" = "ssh" ] && [ -n "$GITEA_REPO_CLONE_SSH_URL" ]; then
  repo_url="$GITEA_REPO_CLONE_SSH_URL"
fi
git_user() {
  if [ -n "${GIT_SSH_COMMAND:-}" ]; then
    sudo -u codespace env GIT_SSH_COMMAND="$GIT_SSH_COMMAND" git "$@"
  else
    sudo -u codespace git "$@"
  fi
}
ensure_git_ssh() {
  install -d -m 700 -o codespace -g codespace /var/lib/gitea-codespace/git
	  if [ ! -f /var/lib/gitea-codespace/git/id_ed25519 ]; then
	    sudo -u codespace ssh-keygen -t ed25519 -N '' -f /var/lib/gitea-codespace/git/id_ed25519 >/dev/null
	  fi
	  [ -s /var/lib/gitea-codespace/git/known_hosts ] || exit 33
	  export GIT_SSH_COMMAND='ssh -i /var/lib/gitea-codespace/git/id_ed25519 -o IdentitiesOnly=yes -o UserKnownHostsFile=/var/lib/gitea-codespace/git/known_hosts -o StrictHostKeyChecking=yes'
	}
workspace="${CODESPACE_WORKSPACES_DIR}/${CODESPACE_REPO_NAME:-repo}"
if [ "$CODESPACE_SCRIPT_PHASE" = "prepare" ]; then
  if [ -z "$repo_url" ]; then
    exit 30
  fi
  mkdir -p "$CODESPACE_WORKSPACES_DIR"
  chown codespace:codespace "$CODESPACE_WORKSPACES_DIR"
  if [ -n "$GITEA_REPO_CLONE_HTTP_URL" ]; then
    git_user config --global credential.helper '!/usr/local/bin/gitea-codespace-git-credential'
  fi
  if [ "$repo_url" = "$GITEA_REPO_CLONE_SSH_URL" ] && [ -n "$repo_url" ]; then
    ensure_git_ssh
  fi
  if [ ! -d "$workspace/.git" ]; then
    git_user clone "$repo_url" "$workspace"
  fi
  git_user -C "$workspace" remote set-url origin "$repo_url"
  if [ -n "$GITEA_REPO_CLONE_HTTP_URL" ]; then
    git_user -C "$workspace" config credential.helper '!/usr/local/bin/gitea-codespace-git-credential'
  fi
  if [ -n "$GITEA_COMMIT_SHA" ]; then
    if [ -n "$GITEA_START_REF" ]; then
      git_user -C "$workspace" fetch origin "$GITEA_START_REF" --tags --prune
    else
      git_user -C "$workspace" fetch --all --tags --prune
    fi
    git_user -C "$workspace" checkout --detach "$GITEA_COMMIT_SHA"
    current="$(git_user -C "$workspace" rev-parse HEAD)"
    if [ "$current" != "$GITEA_COMMIT_SHA" ]; then
      exit 31
    fi
  fi
  printf 'CODESPACE_WORKSPACE_DIR=%s\n' "$workspace" >> "$CODESPACE_ENV"
  write_result prepare-workspace
  exit 0
fi
if [ "$CODESPACE_SCRIPT_PHASE" = "activate" ]; then
  write_result start-environment
  exit 0
fi
exit 32
`

const builtinResumeScript = `
set -eu
write_result() {
  tmp="${CODESPACE_RESULT}.tmp.$$"
  umask 177
  printf '{"outcome":"done","stage":"%s"}\n' "$1" > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$CODESPACE_RESULT"
}
if [ "$CODESPACE_SCRIPT_PHASE" = "prepare" ]; then
  if [ -z "${CODESPACE_WORKSPACE_DIR:-}" ] || [ ! -d "$CODESPACE_WORKSPACE_DIR/.git" ]; then
    exit 40
  fi
  remote_url="$(sudo -u codespace git -C "$CODESPACE_WORKSPACE_DIR" remote get-url origin)"
  case "$remote_url" in
    http://*|https://*)
      sudo -u codespace git config --global credential.helper '!/usr/local/bin/gitea-codespace-git-credential'
      sudo -u codespace git -C "$CODESPACE_WORKSPACE_DIR" config credential.helper '!/usr/local/bin/gitea-codespace-git-credential'
      ;;
    *)
      install -d -m 700 -o codespace -g codespace /var/lib/gitea-codespace/git
      if [ ! -f /var/lib/gitea-codespace/git/id_ed25519 ]; then
        exit 42
      fi
	      [ -s /var/lib/gitea-codespace/git/known_hosts ] || exit 43
	      sudo -u codespace git -C "$CODESPACE_WORKSPACE_DIR" config core.sshCommand 'ssh -i /var/lib/gitea-codespace/git/id_ed25519 -o IdentitiesOnly=yes -o UserKnownHostsFile=/var/lib/gitea-codespace/git/known_hosts -o StrictHostKeyChecking=yes'
      ;;
  esac
  printf 'CODESPACE_WORKSPACE_DIR=%s\n' "$CODESPACE_WORKSPACE_DIR" >> "$CODESPACE_ENV"
  write_result prepare-workspace
  exit 0
fi
if [ "$CODESPACE_SCRIPT_PHASE" = "activate" ]; then
  write_result start-environment
  exit 0
fi
exit 41
`
