#!/bin/sh

set -eu

configure_ca() {
  [ -n "${GITEA_URL:-}" ] || return 0
  if [ -s "${GITEA_CA_BUNDLE:-}" ]; then
    git config --global "http.$GITEA_URL/.sslCAInfo" "$GITEA_CA_BUNDLE"
  else
    git config --global --unset-all "http.$GITEA_URL/.sslCAInfo" 2>/dev/null || true
  fi
}

if command -v git >/dev/null 2>&1; then
  if [ "${GITEA_CONFIGURE_CA_ONLY:-false}" != true ]; then
    git config --global user.name "$GITEA_GIT_USER_NAME"
    git config --global user.email "$GITEA_GIT_USER_EMAIL"
    git config --global --add safe.directory "$GITEA_WORKSPACE"
    if [ -x /var/lib/gitea-codespace/bin/gitea-codespace-git-credential ]; then
      git config --global credential.helper "!/var/lib/gitea-codespace/bin/gitea-codespace-git-credential"
    fi
    if [ -x /var/lib/gitea-codespace/bin/gitea-codespace-git-ssh ]; then
      git config --global core.sshCommand "/var/lib/gitea-codespace/bin/gitea-codespace-git-ssh"
    fi
  fi
  configure_ca
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
  if [ -x /var/lib/gitea-codespace/bin/gitea-codespace-git-credential ]; then
    printf '[credential]\n\thelper =\n\thelper = %s\n' '"!/var/lib/gitea-codespace/bin/gitea-codespace-git-credential"'
  fi
  if [ -x /var/lib/gitea-codespace/bin/gitea-codespace-git-ssh ]; then
    printf '[core]\n\tsshCommand = %s\n' '"/var/lib/gitea-codespace/bin/gitea-codespace-git-ssh"'
  fi
  if [ -s "${GITEA_CA_BUNDLE:-}" ]; then
    printf '[http %s]\n\tsslCAInfo = %s\n' "$(quote_git_config_value "$GITEA_URL/")" "$(quote_git_config_value "$GITEA_CA_BUNDLE")"
  fi
} >>"$HOME/.gitconfig"
