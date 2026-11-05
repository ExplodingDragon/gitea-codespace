#!/bin/sh

set -eu

credential_helper='!f() { while IFS= read -r line; do [ -n "$line" ] || break; done; printf "username=codespace\npassword=%s\n\n" "$(cat /var/lib/gitea-codespace/gitea-token)"; }; f'
ssh_command='ssh -i /var/lib/gitea-codespace/git/id -o IdentitiesOnly=yes -o UserKnownHostsFile=/var/lib/gitea-codespace/git/known_hosts -o StrictHostKeyChecking=yes'

if command -v git >/dev/null 2>&1; then
  if [ "${GITEA_CONFIGURE_CA_ONLY:-false}" != true ]; then
    git config --global user.name "$GITEA_GIT_USER_NAME"
    git config --global user.email "$GITEA_GIT_USER_EMAIL"
    git config --global --add safe.directory "$GITEA_WORKSPACE"
    if [ -r /var/lib/gitea-codespace/gitea-token ] && [ -n "${GITEA_URL:-}" ]; then
      git config --global "credential.$GITEA_URL.helper" "$credential_helper"
    fi
    if [ -r /var/lib/gitea-codespace/git/id ] && [ -r /var/lib/gitea-codespace/git/known_hosts ]; then
      git config --global core.sshCommand "$ssh_command"
    fi
  fi
  if [ -n "${GITEA_URL:-}" ]; then
    if [ -s "${GITEA_CA_BUNDLE:-}" ]; then
      git config --global "http.$GITEA_URL/.sslCAInfo" "$GITEA_CA_BUNDLE"
    else
      git config --global --unset-all "http.$GITEA_URL/.sslCAInfo" 2>/dev/null || true
    fi
  fi
  exit 0
fi

[ "${GITEA_CONFIGURE_CA_ONLY:-false}" != true ] || exit 0

quote_git_config_value() {
  case $1 in
  *'
'*)
    echo "Git configuration value contains a newline" >&2
    exit 1
    ;;
  esac
  printf '"%s"' "$(printf '%s' "$1" | sed 's/[\\"]/\\&/g')"
}

{
  printf '\n[user]\n\tname = %s\n' "$(quote_git_config_value "$GITEA_GIT_USER_NAME")"
  printf '\temail = %s\n' "$(quote_git_config_value "$GITEA_GIT_USER_EMAIL")"
  printf '[safe]\n\tdirectory = %s\n' "$(quote_git_config_value "$GITEA_WORKSPACE")"
  if [ -r /var/lib/gitea-codespace/gitea-token ] && [ -n "${GITEA_URL:-}" ]; then
    printf '[credential %s]\n\thelper =\n\thelper = %s\n' "$(quote_git_config_value "$GITEA_URL")" "$(quote_git_config_value "$credential_helper")"
  fi
  if [ -r /var/lib/gitea-codespace/git/id ] && [ -r /var/lib/gitea-codespace/git/known_hosts ]; then
    printf '[core]\n\tsshCommand = %s\n' "$(quote_git_config_value "$ssh_command")"
  fi
  if [ -s "${GITEA_CA_BUNDLE:-}" ]; then
    printf '[http %s]\n\tsslCAInfo = %s\n' "$(quote_git_config_value "$GITEA_URL/")" "$(quote_git_config_value "$GITEA_CA_BUNDLE")"
  fi
} >>"$HOME/.gitconfig"
