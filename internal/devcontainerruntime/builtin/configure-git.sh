#!/usr/bin/env bash

set -euo pipefail

if command -v git >/dev/null 2>&1; then
	git config --global user.name "$GITEA_GIT_USER_NAME"
	git config --global user.email "$GITEA_GIT_USER_EMAIL"
	git config --global --add safe.directory "$GITEA_WORKSPACE"

	if [[ -x /var/lib/gitea-codespace/bin/gitea-codespace-git-credential ]]; then
		git config --global credential.helper "!/var/lib/gitea-codespace/bin/gitea-codespace-git-credential"
	fi

	if [[ -x /var/lib/gitea-codespace/bin/gitea-codespace-git-ssh ]]; then
		git config --global core.sshCommand "/var/lib/gitea-codespace/bin/gitea-codespace-git-ssh"
	fi
	exit 0
fi

quote_git_config_value() {
	local value=$1
	value=${value//\\/\\\\}
	value=${value//\"/\\\"}
	value=${value//$'\n'/\\n}
	printf '"%s"' "$value"
}

{
	printf '\n[user]\n\tname = %s\n' "$(quote_git_config_value "$GITEA_GIT_USER_NAME")"
	printf '\temail = %s\n' "$(quote_git_config_value "$GITEA_GIT_USER_EMAIL")"
	printf '[safe]\n\tdirectory = %s\n' "$(quote_git_config_value "$GITEA_WORKSPACE")"
	if [[ -x /var/lib/gitea-codespace/bin/gitea-codespace-git-credential ]]; then
		printf '[credential]\n\thelper =\n\thelper = %s\n' '"!/var/lib/gitea-codespace/bin/gitea-codespace-git-credential"'
	fi
	if [[ -x /var/lib/gitea-codespace/bin/gitea-codespace-git-ssh ]]; then
		printf '[core]\n\tsshCommand = %s\n' '"/var/lib/gitea-codespace/bin/gitea-codespace-git-ssh"'
	fi
} >>"$HOME/.gitconfig"
