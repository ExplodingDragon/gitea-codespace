#!/usr/bin/env bash

set -euo pipefail

git config --global user.name "$GITEA_GIT_USER_NAME"
git config --global user.email "$GITEA_GIT_USER_EMAIL"
git config --global --add safe.directory "$GITEA_WORKSPACE"

if [[ -x /var/lib/gitea-codespace/bin/gitea-codespace-git-credential ]]; then
	git config --global credential.helper "!/var/lib/gitea-codespace/bin/gitea-codespace-git-credential"
fi

if [[ -x /var/lib/gitea-codespace/bin/gitea-codespace-git-ssh ]]; then
	git config --global core.sshCommand "/var/lib/gitea-codespace/bin/gitea-codespace-git-ssh"
fi
