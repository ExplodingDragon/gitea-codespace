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
codespace_user="${CODESPACE_USER:-codespace}"

git_user() {
  if [ -n "${GIT_SSH_COMMAND:-}" ]; then
    sudo -u "$codespace_user" env GIT_SSH_COMMAND="$GIT_SSH_COMMAND" git "$@"
  else
    sudo -u "$codespace_user" git "$@"
  fi
}

ensure_git_ssh() {
  [ -f /var/lib/gitea-codespace/git/id_ed25519 ] || exit 33
  [ -s /var/lib/gitea-codespace/git/known_hosts ] || exit 33
  export GIT_SSH_COMMAND='ssh -i /var/lib/gitea-codespace/git/id_ed25519 -o IdentitiesOnly=yes -o UserKnownHostsFile=/var/lib/gitea-codespace/git/known_hosts -o StrictHostKeyChecking=yes'
}

workspace="${CODESPACE_WORKSPACES_DIR}/${CODESPACE_REPO_NAME:-repo}"
if [ "$CODESPACE_SCRIPT_PHASE" = "prepare" ]; then
  if [ -z "$repo_url" ]; then
    exit 30
  fi
  mkdir -p "$CODESPACE_WORKSPACES_DIR"
  chown "$codespace_user:$codespace_user" "$CODESPACE_WORKSPACES_DIR" || chown "$codespace_user" "$CODESPACE_WORKSPACES_DIR"
  if [ -n "$GITEA_REPO_CLONE_HTTP_URL" ]; then
    git_user config --global credential.helper '!/usr/local/bin/gitea-codespace-git-credential'
  fi
  if [ -n "${GITEA_GIT_USER_NAME:-}" ]; then
    git_user config --global user.name "$GITEA_GIT_USER_NAME"
  fi
  if [ -n "${GITEA_GIT_USER_EMAIL:-}" ]; then
    git_user config --global user.email "$GITEA_GIT_USER_EMAIL"
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
  if [ -n "${GITEA_GIT_USER_NAME:-}" ]; then
    git_user -C "$workspace" config user.name "$GITEA_GIT_USER_NAME"
  fi
  if [ -n "${GITEA_GIT_USER_EMAIL:-}" ]; then
    git_user -C "$workspace" config user.email "$GITEA_GIT_USER_EMAIL"
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
