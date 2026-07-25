set -eu

write_result() {
  tmp="${CODESPACE_RESULT}.tmp.$$"
  umask 177
  printf '{"outcome":"%s","stage":"initialize-system"}\n' "$1" > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$CODESPACE_RESULT"
}

fail_recoverable() {
  write_result recoverable_failed
  exit 0
}

fail_unrecoverable() {
  write_result unrecoverable_failed
  exit 0
}

install_missing() {
  missing=""
  for command in curl git ssh ssh-keygen sudo flock getent useradd groupadd python3 npm docker; do
    if ! command -v "$command" >/dev/null 2>&1; then
      missing="$missing $command"
    fi
  done
  if [ -z "$missing" ]; then
    return 0
  fi
  if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update || fail_recoverable
    apt-get install -y ca-certificates curl git openssh-client sudo util-linux passwd python3 npm docker.io || fail_recoverable
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y ca-certificates curl git openssh-clients sudo util-linux shadow-utils python3 npm docker || fail_recoverable
  elif command -v pacman >/dev/null 2>&1; then
    pacman -Sy --noconfirm ca-certificates curl git openssh sudo util-linux shadow python npm docker || fail_recoverable
  else
    fail_unrecoverable
  fi
}

install_missing

if getent group codespace >/dev/null 2>&1; then
  if [ "$(getent group codespace | cut -d: -f3)" != "1000" ]; then
    fail_unrecoverable
  fi
else
  groupadd -g 1000 codespace || fail_unrecoverable
fi

if id -u codespace >/dev/null 2>&1; then
  if [ "$(id -u codespace)" != "1000" ] || [ "$(id -g codespace)" != "1000" ]; then
    fail_unrecoverable
  fi
else
  useradd -m -u 1000 -g 1000 -s /bin/bash codespace || fail_unrecoverable
fi

if getent group docker >/dev/null 2>&1; then
  usermod -aG docker codespace || fail_recoverable
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
  [ -s "$seed_file" ] || fail_unrecoverable
done
[ -f "$seed_known_hosts" ] || fail_unrecoverable
install -d -m 0755 -o 0 -g 0 "$runtime_dir"
install -d -m 0700 -o codespace -g codespace "$runtime_dir/git" "$runtime_dir/results" "$runtime_dir/devcontainer"
install -m 0600 -o codespace -g codespace "$seed_token" "$token_file"
install -m 0600 -o codespace -g codespace "$seed_private_key" "$private_key_file"
install -m 0644 -o codespace -g codespace "$seed_public_key" "$public_key_file"
install -m 0600 -o codespace -g codespace "$seed_known_hosts" "$known_hosts_file"

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

printf 'codespace ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/gitea-codespace
chmod 0440 /etc/sudoers.d/gitea-codespace
if command -v visudo >/dev/null 2>&1; then
  visudo -cf /etc/sudoers.d/gitea-codespace >/dev/null || fail_unrecoverable
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
docker info >/dev/null 2>&1 || fail_recoverable

cli_version="${DEVCONTAINER_EXAMPLE_CLI_VERSION:-0.75.0}"
if ! command -v devcontainer >/dev/null 2>&1; then
  npm install -g "@devcontainers/cli@${cli_version}" || fail_recoverable
fi

git_user() {
  if [ -n "${GIT_SSH_COMMAND:-}" ]; then
    sudo -u codespace env GIT_SSH_COMMAND="$GIT_SSH_COMMAND" git "$@"
  else
    sudo -u codespace git "$@"
  fi
}

ensure_git_ssh() {
  [ -f "$private_key_file" ] || return 1
  [ -s "$known_hosts_file" ] || return 1
  export GIT_SSH_COMMAND="ssh -i $private_key_file -o IdentitiesOnly=yes -o UserKnownHostsFile=$known_hosts_file -o StrictHostKeyChecking=yes"
}

configure_workspace_credentials() {
  workspace="$1"
  remote_url="$(git_user -C "$workspace" remote get-url origin)" || return 1
  case "$remote_url" in
    http://*|https://*)
      [ -n "${GITEA_REPO_CLONE_HTTP_URL:-}" ] || return 1
      git_user config --global credential.helper '!/usr/local/bin/gitea-codespace-git-credential' || return 1
      git_user -C "$workspace" config credential.helper '!/usr/local/bin/gitea-codespace-git-credential' || return 1
      ;;
    *)
      ensure_git_ssh || return 1
      ;;
  esac
}

restore_existing_workspace() {
  workspace="$1"
  [ -d "$workspace/.git" ] || return 1
  configure_workspace_credentials "$workspace" || return 1
  if [ -n "${GITEA_COMMIT_SHA:-}" ]; then
    current="$(git_user -C "$workspace" rev-parse HEAD)" || return 1
    [ "$current" = "$GITEA_COMMIT_SHA" ] || return 1
  fi
}

prepare_workspace_from_repo() {
  repo_url="$1"
  workspace="$2"
  temp_workspace="${CODESPACE_WORKSPACES_DIR}/.gitea-create-${CODESPACE_UUID}"
  rm -rf "$temp_workspace"
  if [ -n "${GITEA_REPO_CLONE_HTTP_URL:-}" ]; then
    git_user config --global credential.helper '!/usr/local/bin/gitea-codespace-git-credential' || return 1
  fi
  if [ "$repo_url" = "${GITEA_REPO_CLONE_SSH_URL:-}" ] && [ -n "$repo_url" ]; then
    ensure_git_ssh || return 1
  fi
  git_user clone "$repo_url" "$temp_workspace" || return 1
  git_user -C "$temp_workspace" remote set-url origin "$repo_url" || return 1
  configure_workspace_credentials "$temp_workspace" || return 1
  if [ -n "${GITEA_COMMIT_SHA:-}" ]; then
    if [ -n "${GITEA_START_REF:-}" ]; then
      git_user -C "$temp_workspace" fetch origin "$GITEA_START_REF" --tags --prune || return 1
    else
      git_user -C "$temp_workspace" fetch --all --tags --prune || return 1
    fi
    git_user -C "$temp_workspace" checkout --detach "$GITEA_COMMIT_SHA" || return 1
    current="$(git_user -C "$temp_workspace" rev-parse HEAD)" || return 1
    [ "$current" = "$GITEA_COMMIT_SHA" ] || return 1
  fi
  [ ! -e "$workspace" ] || return 1
  mv "$temp_workspace" "$workspace" || return 1
}

if [ "${CODESPACE_OPERATION:-}" = "create" ]; then
  repo_url="${GITEA_REPO_CLONE_HTTP_URL:-}"
  fallback_url="${GITEA_REPO_CLONE_SSH_URL:-}"
  if [ "${GITEA_GIT_PROTOCOL:-http}" = "ssh" ] && [ -n "${GITEA_REPO_CLONE_SSH_URL:-}" ]; then
    repo_url="$GITEA_REPO_CLONE_SSH_URL"
    fallback_url="${GITEA_REPO_CLONE_HTTP_URL:-}"
  fi
  [ -n "$repo_url" ] || fail_unrecoverable
  workspace="${CODESPACE_WORKSPACES_DIR}/${GITEA_REPO_NAME:-repo}"
  if [ -d "$workspace/.git" ]; then
    restore_existing_workspace "$workspace" || fail_unrecoverable
  elif ! prepare_workspace_from_repo "$repo_url" "$workspace"; then
    if [ -n "$fallback_url" ] && [ "$fallback_url" != "$repo_url" ]; then
      prepare_workspace_from_repo "$fallback_url" "$workspace" || fail_unrecoverable
    else
      fail_unrecoverable
    fi
  fi
  printf '%s\n' \
    "CODESPACE_WORKSPACE_DIR=${workspace}" \
    >> "$CODESPACE_ENV"
fi

printf '%s\n' \
  'CODESPACE_CREDENTIAL_UID=1000' \
  'CODESPACE_CREDENTIAL_GID=1000' \
  "DEVCONTAINER_EXAMPLE_CLI_VERSION=${cli_version}" \
  >> "$CODESPACE_ENV"
write_result done
