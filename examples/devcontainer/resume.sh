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

container_id_from_workspace() {
  docker ps -a --filter "label=devcontainer.local_folder=${1}" --format '{{.ID}}' | head -n 1
}

restore_git_credentials() {
  remote_url="$(sudo -u codespace git -C "$CODESPACE_WORKSPACE_DIR" remote get-url origin)" || fail_unrecoverable prepare-workspace
  case "$remote_url" in
    http://*|https://*)
      sudo -u codespace git config --global credential.helper '!/usr/local/bin/gitea-codespace-git-credential'
      sudo -u codespace git -C "$CODESPACE_WORKSPACE_DIR" config credential.helper '!/usr/local/bin/gitea-codespace-git-credential'
      ;;
    *)
      [ -f /var/lib/gitea-codespace/git/id_ed25519 ] || fail_unrecoverable prepare-workspace
      [ -s /var/lib/gitea-codespace/git/known_hosts ] || fail_unrecoverable prepare-workspace
      sudo -u codespace git -C "$CODESPACE_WORKSPACE_DIR" config core.sshCommand 'ssh -i /var/lib/gitea-codespace/git/id_ed25519 -o IdentitiesOnly=yes -o UserKnownHostsFile=/var/lib/gitea-codespace/git/known_hosts -o StrictHostKeyChecking=yes'
      ;;
  esac
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

if [ "$CODESPACE_SCRIPT_PHASE" = "prepare" ]; then
  [ -n "${CODESPACE_WORKSPACE_DIR:-}" ] || fail_unrecoverable prepare-workspace
  [ -d "$CODESPACE_WORKSPACE_DIR/.git" ] || fail_unrecoverable prepare-workspace
  restore_git_credentials
  container_id="${DEVCONTAINER_EXAMPLE_CONTAINER_ID:-}"
  if [ -z "$container_id" ]; then
    container_id="$(container_id_from_workspace "$CODESPACE_WORKSPACE_DIR")"
  fi
  if [ -z "$container_id" ]; then
    sudo -u codespace devcontainer up --workspace-folder "$CODESPACE_WORKSPACE_DIR" || fail_recoverable prepare-workspace
    container_id="$(container_id_from_workspace "$CODESPACE_WORKSPACE_DIR")"
  else
    docker start "$container_id" >/dev/null || fail_recoverable prepare-workspace
  fi
  [ -n "$container_id" ] || fail_recoverable prepare-workspace
  printf '%s\n' \
    "CODESPACE_WORKSPACE_DIR=${CODESPACE_WORKSPACE_DIR}" \
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
