#!/bin/sh
# Copyright 2026 The Gitea Authors. All rights reserved.
# SPDX-License-Identifier: MIT

set -e
. /var/lib/gitea-codespace/environment/env.sh
export GITEA_CLI_NO_UPDATE_NOTIFIER=1
exec /usr/local/libexec/gitea-codespace/gt "$@"
