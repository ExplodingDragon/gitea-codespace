set -eu

write_result() {
  result_tmp_path="${CODESPACE_RESULT}.tmp.$$"
  umask 177
  printf '{"outcome":"%s","stage":"initialize-system"}\n' "$1" > "$result_tmp_path"
  chmod 600 "$result_tmp_path"
  mv "$result_tmp_path" "$CODESPACE_RESULT"
}

log_error() {
  printf '%s\n' "$*" >&2
}

fail_recoverable() {
  if [ "$#" -gt 0 ]; then
    log_error "$*"
  fi
  write_result recoverable_failed
  exit 0
}

fail_unrecoverable() {
  if [ "$#" -gt 0 ]; then
    log_error "$*"
  fi
  write_result unrecoverable_failed
  exit 0
}

install_missing() {
  missing_commands=""
  for command in bash curl git ssh ssh-keygen sudo flock getent useradd groupadd python3 npm docker; do
    if ! command -v "$command" >/dev/null 2>&1; then
      missing_commands="$missing_commands $command"
    fi
  done
  if [ -z "$missing_commands" ]; then
    return 0
  fi
  if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update || fail_recoverable
    apt-get install -y bash ca-certificates curl git openssh-client sudo util-linux passwd python3 npm docker.io || fail_recoverable
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y bash ca-certificates curl git openssh-clients sudo util-linux shadow-utils python3 npm docker || fail_recoverable
  elif command -v pacman >/dev/null 2>&1; then
    pacman -Sy --noconfirm bash ca-certificates curl git openssh sudo util-linux shadow python npm docker || fail_recoverable
  else
    fail_unrecoverable "no supported package manager found for installing devcontainer dependencies"
  fi
}

install_missing

if getent group codespace >/dev/null 2>&1; then
  if [ "$(getent group codespace | cut -d: -f3)" != "1000" ]; then
    fail_unrecoverable "existing codespace group does not use gid 1000"
  fi
else
  groupadd -g 1000 codespace || fail_unrecoverable "create codespace group failed"
fi

if id -u codespace >/dev/null 2>&1; then
  if [ "$(id -u codespace)" != "1000" ] || [ "$(id -g codespace)" != "1000" ]; then
    fail_unrecoverable "existing codespace user does not use uid/gid 1000"
  fi
else
  useradd -m -u 1000 -g 1000 -s /bin/bash codespace || fail_unrecoverable "create codespace user failed"
fi

if getent group docker >/dev/null 2>&1; then
  usermod -aG docker codespace || fail_recoverable "add codespace user to docker group failed"
fi

passwd -l codespace >/dev/null 2>&1 || true
mkdir -p /workspaces /var/lib/gitea-codespace /var/lib/gitea-codespace/git /var/lib/gitea-codespace/results /var/lib/gitea-codespace/devcontainer
chown codespace:codespace /workspaces
chmod 0755 /workspaces

runtime_dir="${CODESPACE_RUNTIME_DIR:-/var/lib/gitea-codespace}"
runtime_seed_dir="${CODESPACE_RUNTIME_SEED_DIR:-$runtime_dir/seed}"
token_file="${CODESPACE_GITEA_TOKEN_FILE:-$runtime_dir/gitea-token}"
private_key_file="${CODESPACE_GIT_SSH_PRIVATE_KEY:-$runtime_dir/git/id_ed25519}"
public_key_file="${CODESPACE_GIT_SSH_PUBLIC_KEY:-$runtime_dir/git/id_ed25519.pub}"
known_hosts_file="${CODESPACE_GIT_SSH_KNOWN_HOSTS:-$runtime_dir/git/known_hosts}"
seed_token="$runtime_seed_dir/gitea-token"
seed_private_key="$runtime_seed_dir/id_ed25519"
seed_public_key="$runtime_seed_dir/id_ed25519.pub"
seed_known_hosts="$runtime_seed_dir/known_hosts"
for seed_file in "$seed_token" "$seed_private_key" "$seed_public_key"; do
  [ -s "$seed_file" ] || fail_unrecoverable "missing runtime seed file: $seed_file"
done
[ -f "$seed_known_hosts" ] || fail_unrecoverable "missing runtime seed known_hosts file: $seed_known_hosts"
install -d -m 0755 -o 0 -g 0 "$runtime_dir"
install -d -m 0700 -o codespace -g codespace "$runtime_dir/git" "$runtime_dir/results" "$runtime_dir/devcontainer"
install -m 0600 -o codespace -g codespace "$seed_token" "$token_file"
install -m 0600 -o codespace -g codespace "$seed_private_key" "$private_key_file"
install -m 0644 -o codespace -g codespace "$seed_public_key" "$public_key_file"
install -m 0600 -o codespace -g codespace "$seed_known_hosts" "$known_hosts_file"

cat >/usr/local/bin/gitea-codespace-git-credential <<'EOF'
#!/bin/bash
set -eu
while IFS= read -r line; do
  [ -n "$line" ] || break
done
printf 'username=codespace\n'
printf 'password=%s\n\n' "$(cat /var/lib/gitea-codespace/gitea-token)"
EOF
chmod 0755 /usr/local/bin/gitea-codespace-git-credential

printf 'codespace ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/gitea-codespace
chmod 0440 /etc/sudoers.d/gitea-codespace
if command -v visudo >/dev/null 2>&1; then
  visudo -cf /etc/sudoers.d/gitea-codespace >/dev/null || fail_unrecoverable "validate sudoers file failed"
fi

if ! docker info >/dev/null 2>&1; then
  if command -v dockerd >/dev/null 2>&1; then
    nohup dockerd >/var/log/gitea-codespace-dockerd.log 2>&1 &
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      docker info >/dev/null 2>&1 && break
      sleep 1
    done
  fi
fi
docker info >/dev/null 2>&1 || fail_recoverable "docker daemon is not available"

cli_version="${DEVCONTAINER_EXAMPLE_CLI_VERSION:-0.75.0}"
if ! command -v devcontainer >/dev/null 2>&1; then
  npm install -g "@devcontainers/cli@${cli_version}" || fail_recoverable "install devcontainer CLI failed"
fi

git_user() {
  if [ -n "${GIT_SSH_COMMAND:-}" ]; then
    sudo -u codespace env GIT_SSH_COMMAND="$GIT_SSH_COMMAND" git "$@"
  else
    sudo -u codespace git "$@"
  fi
}

ensure_git_ssh() {
  [ -f "$private_key_file" ] || { log_error "missing git ssh private key: $private_key_file"; return 1; }
  [ -s "$known_hosts_file" ] || { log_error "missing git ssh known_hosts: $known_hosts_file"; return 1; }
  export GIT_SSH_COMMAND="ssh -i $private_key_file -o IdentitiesOnly=yes -o UserKnownHostsFile=$known_hosts_file -o StrictHostKeyChecking=yes"
}

configure_workspace_credentials() {
  credential_workspace="$1"
  credential_remote_url="$(git_user -C "$credential_workspace" remote get-url origin)" || return 1
  case "$credential_remote_url" in
    http://*|https://*)
      [ -n "${GITEA_REPO_CLONE_HTTP_URL:-}" ] || return 1
      git_user config --global credential.helper '!/usr/local/bin/gitea-codespace-git-credential' || return 1
      git_user -C "$credential_workspace" config credential.helper '!/usr/local/bin/gitea-codespace-git-credential' || return 1
      ;;
    *)
      ensure_git_ssh || return 1
      ;;
  esac
}

restore_existing_workspace() {
  existing_workspace="$1"
  [ -d "$existing_workspace/.git" ] || { log_error "workspace is not a git repository: $existing_workspace"; return 1; }
  configure_workspace_credentials "$existing_workspace" || { log_error "configure existing workspace credentials failed: $existing_workspace"; return 1; }
  if [ -n "${GITEA_COMMIT_SHA:-}" ]; then
    existing_head="$(git_user -C "$existing_workspace" rev-parse HEAD)" || { log_error "read existing workspace HEAD failed: $existing_workspace"; return 1; }
    [ "$existing_head" = "$GITEA_COMMIT_SHA" ] || { log_error "existing workspace commit mismatch: expected $GITEA_COMMIT_SHA got $existing_head"; return 1; }
  fi
}

prepare_workspace_target() {
  candidate_workspace="$1"
  if [ ! -e "$candidate_workspace" ]; then
    return 0
  fi
  if [ -d "$candidate_workspace/.git" ]; then
    log_error "workspace already contains a git repository: $candidate_workspace"
    return 1
  fi
  if [ -d "$candidate_workspace" ]; then
    if rmdir "$candidate_workspace" 2>/dev/null; then
      log_error "removed empty workspace directory before clone: $candidate_workspace"
      return 0
    fi
    log_error "workspace path already exists and is not empty or a git repository: $candidate_workspace"
    return 1
  fi
  log_error "workspace path already exists and is not a directory: $candidate_workspace"
  return 1
}

prepare_workspace_from_repo() {
  clone_repo_url="$1"
  target_workspace="$2"
  clone_temp_workspace="${CODESPACE_WORKSPACES_DIR}/.gitea-create-${CODESPACE_UUID}"
  [ "$target_workspace" != "$clone_temp_workspace" ] || { log_error "workspace path conflicts with temporary clone path: $target_workspace"; return 1; }
  prepare_workspace_target "$target_workspace" || return 1
  rm -rf "$clone_temp_workspace" || { log_error "remove temporary workspace failed: $clone_temp_workspace"; return 1; }
  if [ -n "${GITEA_REPO_CLONE_HTTP_URL:-}" ]; then
    git_user config --global credential.helper '!/usr/local/bin/gitea-codespace-git-credential' || { log_error "configure git credential helper failed"; return 1; }
  fi
  if [ "$clone_repo_url" = "${GITEA_REPO_CLONE_SSH_URL:-}" ] && [ -n "$clone_repo_url" ]; then
    ensure_git_ssh || return 1
  fi
  git_user clone "$clone_repo_url" "$clone_temp_workspace" || { log_error "clone repository failed: $clone_repo_url"; return 1; }
  git_user -C "$clone_temp_workspace" remote set-url origin "$clone_repo_url" || { log_error "set workspace origin failed: $clone_repo_url"; return 1; }
  configure_workspace_credentials "$clone_temp_workspace" || { log_error "configure cloned workspace credentials failed: $clone_temp_workspace"; return 1; }
  if [ -n "${GITEA_COMMIT_SHA:-}" ]; then
    if [ -n "${GITEA_START_REF:-}" ]; then
      git_user -C "$clone_temp_workspace" fetch origin "$GITEA_START_REF" --tags --prune || { log_error "fetch start ref failed: $GITEA_START_REF"; return 1; }
    else
      git_user -C "$clone_temp_workspace" fetch --all --tags --prune || { log_error "fetch repository refs failed"; return 1; }
    fi
    git_user -C "$clone_temp_workspace" checkout --detach "$GITEA_COMMIT_SHA" || { log_error "checkout commit failed: $GITEA_COMMIT_SHA"; return 1; }
    cloned_head="$(git_user -C "$clone_temp_workspace" rev-parse HEAD)" || { log_error "read cloned workspace HEAD failed"; return 1; }
    [ "$cloned_head" = "$GITEA_COMMIT_SHA" ] || { log_error "cloned workspace commit mismatch: expected $GITEA_COMMIT_SHA got $cloned_head"; return 1; }
  fi
  prepare_workspace_target "$target_workspace" || return 1
  mv "$clone_temp_workspace" "$target_workspace" || { log_error "move prepared workspace into place failed: $clone_temp_workspace -> $target_workspace"; return 1; }
}

if [ "${CODESPACE_OPERATION:-}" = "create" ]; then
  create_repo_url="${GITEA_REPO_CLONE_HTTP_URL:-}"
  create_fallback_url="${GITEA_REPO_CLONE_SSH_URL:-}"
  if [ "${GITEA_GIT_PROTOCOL:-http}" = "ssh" ] && [ -n "${GITEA_REPO_CLONE_SSH_URL:-}" ]; then
    create_repo_url="$GITEA_REPO_CLONE_SSH_URL"
    create_fallback_url="${GITEA_REPO_CLONE_HTTP_URL:-}"
  fi
  [ -n "$create_repo_url" ] || fail_unrecoverable "repository clone URL is empty"
  create_workspace="${CODESPACE_WORKSPACES_DIR}/${GITEA_REPO_NAME:-repo}"
  if [ -d "$create_workspace/.git" ]; then
    restore_existing_workspace "$create_workspace" || fail_unrecoverable "existing workspace cannot be restored: $create_workspace"
  elif ! prepare_workspace_from_repo "$create_repo_url" "$create_workspace"; then
    if [ -n "$create_fallback_url" ] && [ "$create_fallback_url" != "$create_repo_url" ]; then
      prepare_workspace_from_repo "$create_fallback_url" "$create_workspace" || fail_unrecoverable "clone failed with primary and fallback repository URLs"
    else
      fail_unrecoverable "clone failed and no fallback repository URL is available"
    fi
  fi
  printf '%s\n' \
    "CODESPACE_WORKSPACE_DIR=${create_workspace}" \
    >> "$CODESPACE_ENV"
fi

printf '%s\n' \
  'CODESPACE_CREDENTIAL_UID=1000' \
  'CODESPACE_CREDENTIAL_GID=1000' \
  "DEVCONTAINER_EXAMPLE_CLI_VERSION=${cli_version}" \
  >> "$CODESPACE_ENV"
write_result done
