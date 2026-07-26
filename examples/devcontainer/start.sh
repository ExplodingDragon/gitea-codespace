set -eu

write_result() {
  result_tmp_path="${CODESPACE_RESULT}.tmp.$$"
  umask 177
  printf '{"outcome":"%s","stage":"start-environment"}\n' "$1" > "$result_tmp_path"
  chmod 600 "$result_tmp_path"
  mv "$result_tmp_path" "$CODESPACE_RESULT"
}

fail_recoverable() {
  write_result recoverable_failed
  exit 0
}

fail_unrecoverable() {
  write_result unrecoverable_failed
  exit 0
}

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
install -d -m 0700 -o codespace -g codespace "$runtime_dir/git"
install -m 0600 -o codespace -g codespace "$seed_token" "$token_file"
install -m 0600 -o codespace -g codespace "$seed_private_key" "$private_key_file"
install -m 0644 -o codespace -g codespace "$seed_public_key" "$public_key_file"
install -m 0600 -o codespace -g codespace "$seed_known_hosts" "$known_hosts_file"

git_user() {
  if [ -n "${GIT_SSH_COMMAND:-}" ]; then
    sudo -u codespace env GIT_SSH_COMMAND="$GIT_SSH_COMMAND" git "$@"
  else
    sudo -u codespace git "$@"
  fi
}

ensure_git_ssh() {
  [ -f "$private_key_file" ] || fail_recoverable
  [ -s "$known_hosts_file" ] || fail_recoverable
  export GIT_SSH_COMMAND="ssh -i $private_key_file -o IdentitiesOnly=yes -o UserKnownHostsFile=$known_hosts_file -o StrictHostKeyChecking=yes"
}

container_id_from_workspace() {
  docker ps -a --filter "label=devcontainer.local_folder=${1}" --format '{{.ID}}' | head -n 1
}

ensure_docker() {
  if docker info >/dev/null 2>&1; then
    return 0
  fi
  if command -v dockerd >/dev/null 2>&1; then
    nohup dockerd >/var/log/gitea-codespace-dockerd.log 2>&1 &
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      docker info >/dev/null 2>&1 && return 0
      sleep 1
    done
  fi
  return 1
}

[ -n "${CODESPACE_WORKSPACE_DIR:-}" ] || fail_unrecoverable
[ -d "$CODESPACE_WORKSPACE_DIR/.git" ] || fail_unrecoverable
start_remote_url="$(git_user -C "$CODESPACE_WORKSPACE_DIR" remote get-url origin)" || fail_unrecoverable
case "$start_remote_url" in
  http://*|https://*)
    git_user config --global credential.helper '!/usr/local/bin/gitea-codespace-git-credential' || fail_unrecoverable
    git_user -C "$CODESPACE_WORKSPACE_DIR" config credential.helper '!/usr/local/bin/gitea-codespace-git-credential' || fail_unrecoverable
    ;;
  *)
    ensure_git_ssh
    git_user -C "$CODESPACE_WORKSPACE_DIR" config core.sshCommand "ssh -i $private_key_file -o IdentitiesOnly=yes -o UserKnownHostsFile=$known_hosts_file -o StrictHostKeyChecking=yes" || fail_unrecoverable
    ;;
esac

ensure_docker || fail_recoverable
devcontainer_container_id="${DEVCONTAINER_EXAMPLE_CONTAINER_ID:-}"
if [ -z "$devcontainer_container_id" ]; then
  devcontainer_container_id="$(container_id_from_workspace "$CODESPACE_WORKSPACE_DIR")"
fi
if [ -n "$devcontainer_container_id" ]; then
  docker start "$devcontainer_container_id" >/dev/null || fail_recoverable
else
  sudo -u codespace devcontainer up --workspace-folder "$CODESPACE_WORKSPACE_DIR" || fail_recoverable
  devcontainer_container_id="$(container_id_from_workspace "$CODESPACE_WORKSPACE_DIR")"
  [ -n "$devcontainer_container_id" ] || fail_recoverable
fi

printf '%s\n' \
  "CODESPACE_WORKSPACE_DIR=${CODESPACE_WORKSPACE_DIR}" \
  "DEVCONTAINER_EXAMPLE_CONTAINER_ID=${devcontainer_container_id}" \
  >> "$CODESPACE_ENV"
write_result done
