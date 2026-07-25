set -eu
codespace_user="${CODESPACE_USER:-codespace}"

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
  remote_url="$(sudo -u "$codespace_user" git -C "$CODESPACE_WORKSPACE_DIR" remote get-url origin)"
  case "$remote_url" in
    http://*|https://*)
      sudo -u "$codespace_user" git config --global credential.helper '!/usr/local/bin/gitea-codespace-git-credential'
      sudo -u "$codespace_user" git -C "$CODESPACE_WORKSPACE_DIR" config credential.helper '!/usr/local/bin/gitea-codespace-git-credential'
      ;;
    *)
      if [ ! -f /var/lib/gitea-codespace/git/id_ed25519 ]; then
        exit 42
      fi
      [ -s /var/lib/gitea-codespace/git/known_hosts ] || exit 43
      sudo -u "$codespace_user" git -C "$CODESPACE_WORKSPACE_DIR" config core.sshCommand 'ssh -i /var/lib/gitea-codespace/git/id_ed25519 -o IdentitiesOnly=yes -o UserKnownHostsFile=/var/lib/gitea-codespace/git/known_hosts -o StrictHostKeyChecking=yes'
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
