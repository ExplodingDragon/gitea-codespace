#!/usr/bin/env bash

set -euo pipefail

if ! command -v code-server >/dev/null 2>&1; then
	echo "code-server is not installed by the platform Feature" >&2
	exit 1
fi

settings_dir="${XDG_DATA_HOME:-$HOME/.local/share}/code-server/User"
mkdir -p "$settings_dir"
if [[ "${GITEA_WEB_IDE_INITIALIZE:-false}" == true ]]; then
	umask 077
	cat >"$settings_dir/settings.json"
fi

pid_file=/var/lib/gitea-codespace/runtime/code-server.pid
log_file=/var/lib/gitea-codespace/runtime/code-server.log
running=false
if [[ -r "$pid_file" ]]; then
	pid="$(<"$pid_file")"
	if [[ "$pid" =~ ^[0-9]+$ ]] && kill -0 "$pid" 2>/dev/null && [[ -r "/proc/$pid/cmdline" ]]; then
		cmdline="$(tr '\0' '\n' <"/proc/$pid/cmdline")"
		if grep -Eq '(^|/)code-server$' <<<"$cmdline" &&
			grep -Fxq -- "--bind-addr" <<<"$cmdline" &&
			grep -Fxq -- "0.0.0.0:$GITEA_WEB_IDE_PORT" <<<"$cmdline" &&
			grep -Fxq -- "$GITEA_WORKSPACE" <<<"$cmdline"; then
			running=true
		else
			kill "$pid" 2>/dev/null || true
			for _ in {1..20}; do
				if ! kill -0 "$pid" 2>/dev/null; then
					break
				fi
				sleep 0.1
			done
			if kill -0 "$pid" 2>/dev/null; then
				echo "previous code-server process $pid did not stop" >&2
				exit 1
			fi
		fi
	fi
fi

if [[ "$running" != true ]]; then
	nohup code-server \
		--auth none \
		--bind-addr "0.0.0.0:$GITEA_WEB_IDE_PORT" \
		--disable-telemetry \
		--disable-update-check \
		"$GITEA_WORKSPACE" \
		>"$log_file" 2>&1 </dev/null &
	printf '%s\n' "$!" >"$pid_file"
fi
