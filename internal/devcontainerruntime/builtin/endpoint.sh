#!/usr/bin/env bash
set -euo pipefail
exec /usr/local/libexec/gitea-codespace-runtime runtime endpoint "$@"
