#!/bin/sh

set -eu

if ! command -v code-server >/dev/null 2>&1; then
  echo "code-server is not installed by the platform Feature" >&2
  exit 1
fi

settings_dir="${XDG_DATA_HOME:-$HOME/.local/share}/code-server/User"
mkdir -p "$settings_dir"
if [ "${GITEA_WEB_IDE_INITIALIZE:-false}" = true ]; then
  umask 077
  cat >"$settings_dir/settings.json"
fi

runtime_dir="${XDG_RUNTIME_DIR:-$HOME/.cache}/gitea-codespace"
mkdir -p "$runtime_dir"
pid_file="$runtime_dir/code-server.pid"
log_file="$runtime_dir/code-server.log"
running=false
if [ -r "$pid_file" ]; then
  pid=$(cat "$pid_file")
  case "$pid" in
  '' | *[!0-9]*) pid= ;;
  esac
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && [ -r "/proc/$pid/cmdline" ]; then
    cmdline=$(tr '\0' '\n' <"/proc/$pid/cmdline")
    if printf '%s\n' "$cmdline" | grep -Eq '(^|/)code-server$' &&
      printf '%s\n' "$cmdline" | grep -Fxq -- "--bind-addr" &&
      printf '%s\n' "$cmdline" | grep -Fxq -- "0.0.0.0:$GITEA_WEB_IDE_PORT" &&
      printf '%s\n' "$cmdline" | grep -Fxq -- "$GITEA_WORKSPACE"; then
      running=true
    else
      kill "$pid" 2>/dev/null || true
      attempts=0
      while [ "$attempts" -lt 20 ] && kill -0 "$pid" 2>/dev/null; do
        sleep 0.1
        attempts=$((attempts + 1))
      done
      if kill -0 "$pid" 2>/dev/null; then
        echo "previous code-server process $pid did not stop" >&2
        exit 1
      fi
    fi
  fi
fi

if [ "$running" != true ]; then
  nohup code-server \
    --auth none \
    --bind-addr "0.0.0.0:$GITEA_WEB_IDE_PORT" \
    --disable-telemetry \
    --disable-update-check \
    "$GITEA_WORKSPACE" \
    >"$log_file" 2>&1 </dev/null &
  printf '%s\n' "$!" >"$pid_file"
fi
