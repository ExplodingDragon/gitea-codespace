#!/usr/bin/env bash

set -euo pipefail

if ! command -v code-server >/dev/null 2>&1; then
	echo "code-server is not installed by the platform Feature" >&2
	exit 1
fi

settings_dir="${XDG_DATA_HOME:-$HOME/.local/share}/code-server/User"
mkdir -p "$settings_dir"
umask 077
cat >"$settings_dir/settings.json"

pid_file=/var/lib/gitea-codespace/runtime/code-server.pid
running=false
if [[ -r "$pid_file" ]]; then
	pid="$(<"$pid_file")"
	if kill -0 "$pid" 2>/dev/null && grep -a -q code-server "/proc/$pid/cmdline" 2>/dev/null; then
		running=true
	fi
fi

if [[ "$running" != true ]]; then
	nohup code-server \
		--auth none \
		--bind-addr "0.0.0.0:$GITEA_WEB_IDE_PORT" \
		--disable-telemetry \
		--disable-update-check \
		"$GITEA_WORKSPACE" \
		>/var/lib/gitea-codespace/runtime/code-server.log 2>&1 </dev/null &
	printf '%s\n' "$!" >"$pid_file"
fi
