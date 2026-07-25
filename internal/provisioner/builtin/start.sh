set -eu

write_result() {
  outcome="${1:-done}"
  tmp="${CODESPACE_RESULT}.tmp.$$"
  umask 177
  printf '{"outcome":"%s","stage":"start-environment"}\n' "$outcome" > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$CODESPACE_RESULT"
}

fail_unrecoverable() {
  write_result unrecoverable_failed
  exit 0
}

codespace_user="${CODESPACE_USER:-codespace}"
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
codespace_uid="$(id -u "$codespace_user")" || fail_unrecoverable
codespace_gid="$(id -g "$codespace_user")" || fail_unrecoverable
install -d -m 0700 -o "$codespace_uid" -g "$codespace_gid" "$runtime_dir/git"
install -m 0600 -o "$codespace_uid" -g "$codespace_gid" "$seed_token" "$token_file"
install -m 0600 -o "$codespace_uid" -g "$codespace_gid" "$seed_private_key" "$private_key_file"
install -m 0644 -o "$codespace_uid" -g "$codespace_gid" "$seed_public_key" "$public_key_file"
install -m 0600 -o "$codespace_uid" -g "$codespace_gid" "$seed_known_hosts" "$known_hosts_file"

git_user() {
  if [ -n "${GIT_SSH_COMMAND:-}" ]; then
    sudo -u "$codespace_user" env GIT_SSH_COMMAND="$GIT_SSH_COMMAND" git "$@"
  else
    sudo -u "$codespace_user" git "$@"
  fi
}

ensure_git_ssh() {
  [ -f "$private_key_file" ] || return 1
  [ -s "$known_hosts_file" ] || return 1
  export GIT_SSH_COMMAND="ssh -i $private_key_file -o IdentitiesOnly=yes -o UserKnownHostsFile=$known_hosts_file -o StrictHostKeyChecking=yes"
}

workspace="${CODESPACE_WORKSPACE_DIR:-${CODESPACE_WORKSPACES_DIR}/${CODESPACE_REPO_NAME:-repo}}"
[ -d "$workspace/.git" ] || fail_unrecoverable
remote_url="$(git_user -C "$workspace" remote get-url origin)" || fail_unrecoverable
case "$remote_url" in
  http://*|https://*)
    git_user config --global credential.helper '!/usr/local/bin/gitea-codespace-git-credential' || fail_unrecoverable
    git_user -C "$workspace" config credential.helper '!/usr/local/bin/gitea-codespace-git-credential' || fail_unrecoverable
    ;;
  *)
    ensure_git_ssh || fail_unrecoverable
    git_user -C "$workspace" config core.sshCommand "ssh -i $private_key_file -o IdentitiesOnly=yes -o UserKnownHostsFile=$known_hosts_file -o StrictHostKeyChecking=yes" || fail_unrecoverable
    ;;
esac

printf 'CODESPACE_WORKSPACE_DIR=%s\n' "$workspace" >> "$CODESPACE_ENV"
write_result done
