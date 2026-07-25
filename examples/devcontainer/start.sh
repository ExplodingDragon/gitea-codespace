set -eu

write_result() {
  tmp="${CODESPACE_RESULT}.tmp.$$"
  umask 177
  printf '{"outcome":"%s","stage":"%s"}\n' "$1" "$2" > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$CODESPACE_RESULT"
}

fail_recoverable() {
  write_result recoverable_failed "$1"
  exit 0
}

fail_unrecoverable() {
  write_result unrecoverable_failed "$1"
  exit 0
}

git_user() {
  if [ -n "${GIT_SSH_COMMAND:-}" ]; then
    sudo -u codespace env GIT_SSH_COMMAND="$GIT_SSH_COMMAND" git "$@"
  else
    sudo -u codespace git "$@"
  fi
}

ensure_git_ssh() {
  [ -f /var/lib/gitea-codespace/git/id_ed25519 ] || fail_recoverable prepare-workspace
  [ -s /var/lib/gitea-codespace/git/known_hosts ] || fail_recoverable prepare-workspace
  export GIT_SSH_COMMAND='ssh -i /var/lib/gitea-codespace/git/id_ed25519 -o IdentitiesOnly=yes -o UserKnownHostsFile=/var/lib/gitea-codespace/git/known_hosts -o StrictHostKeyChecking=yes'
}

container_id_from_workspace() {
  docker ps -a --filter "label=devcontainer.local_folder=${1}" --format '{{.ID}}' | head -n 1
}

activate_container() {
  container_id="${DEVCONTAINER_EXAMPLE_CONTAINER_ID:-}"
  if [ -z "$container_id" ]; then
    container_id="$(container_id_from_workspace "$CODESPACE_WORKSPACE_DIR")"
  fi
  [ -n "$container_id" ] || fail_unrecoverable start-environment
  docker start "$container_id" >/dev/null || fail_recoverable start-environment
  printf '%s\n' \
    "DEVCONTAINER_EXAMPLE_CONTAINER_ID=${container_id}" \
    >> "$CODESPACE_ENV"
  write_result done start-environment
}

repo_url="${GITEA_REPO_CLONE_HTTP_URL:-}"
repo_ssh_url="${GITEA_REPO_CLONE_SSH_URL:-}"
if [ "${GITEA_GIT_PROTOCOL:-http}" = "ssh" ] && [ -n "$repo_ssh_url" ]; then
  repo_url="$repo_ssh_url"
fi

if [ "$CODESPACE_SCRIPT_PHASE" = "prepare" ]; then
  [ -n "$repo_url" ] || fail_unrecoverable prepare-workspace
  workspace="${CODESPACE_WORKSPACES_DIR}/${CODESPACE_REPO_NAME:-repo}"
  mkdir -p "$CODESPACE_WORKSPACES_DIR"
  chown codespace:codespace "$CODESPACE_WORKSPACES_DIR"
  if [ -n "$GITEA_REPO_CLONE_HTTP_URL" ]; then
    git_user config --global credential.helper '!/usr/local/bin/gitea-codespace-git-credential'
  fi
  if [ "$repo_url" = "$repo_ssh_url" ] && [ -n "$repo_url" ]; then
    ensure_git_ssh
  fi
  if [ ! -d "$workspace/.git" ]; then
    git_user clone "$repo_url" "$workspace" || fail_recoverable prepare-workspace
  fi
  git_user -C "$workspace" remote set-url origin "$repo_url" || fail_recoverable prepare-workspace
  if [ -n "$GITEA_REPO_CLONE_HTTP_URL" ]; then
    git_user -C "$workspace" config credential.helper '!/usr/local/bin/gitea-codespace-git-credential'
  fi
  if [ -n "$GITEA_COMMIT_SHA" ]; then
    if [ -n "$GITEA_START_REF" ]; then
      git_user -C "$workspace" fetch origin "$GITEA_START_REF" --tags --prune || fail_recoverable prepare-workspace
    else
      git_user -C "$workspace" fetch --all --tags --prune || fail_recoverable prepare-workspace
    fi
    git_user -C "$workspace" checkout --detach "$GITEA_COMMIT_SHA" || fail_recoverable prepare-workspace
    current="$(git_user -C "$workspace" rev-parse HEAD)"
    [ "$current" = "$GITEA_COMMIT_SHA" ] || fail_unrecoverable prepare-workspace
  fi
  sudo -u codespace devcontainer up --workspace-folder "$workspace" || fail_recoverable prepare-workspace
  container_id="$(container_id_from_workspace "$workspace")"
  [ -n "$container_id" ] || fail_recoverable prepare-workspace
  printf '%s\n' \
    "CODESPACE_WORKSPACE_DIR=${workspace}" \
    "DEVCONTAINER_EXAMPLE_CONTAINER_ID=${container_id}" \
    >> "$CODESPACE_ENV"
  write_result done prepare-workspace
  exit 0
fi

if [ "$CODESPACE_SCRIPT_PHASE" = "activate" ]; then
  activate_container
  exit 0
fi

fail_unrecoverable start-environment
