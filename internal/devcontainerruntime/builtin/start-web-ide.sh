#!/bin/sh

set -eu

entrypoint=/usr/local/bin/code-server-entrypoint
if [ ! -x "$entrypoint" ]; then
  echo "code-server Feature entrypoint is not installed" >&2
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
launcher_log="$runtime_dir/code-server-entrypoint.log"

is_expected_process() {
  candidate=$1
  if ! kill -0 "$candidate" 2>/dev/null || [ ! -r "/proc/$candidate/cmdline" ]; then
    return 1
  fi
  cmdline=$(tr '\0' '\n' <"/proc/$candidate/cmdline" 2>/dev/null) || return 1
  [ "$(readlink "/proc/$candidate/cwd" 2>/dev/null || true)" = "$GITEA_WORKSPACE" ] &&
    printf '%s\n' "$cmdline" | grep -Fxq -- "--bind-addr" &&
    printf '%s\n' "$cmdline" | grep -Fxq -- "0.0.0.0:$GITEA_WEB_IDE_PORT"
}

find_expected_process() {
  for path in /proc/[0-9]*/cmdline; do
    candidate=${path#/proc/}
    candidate=${candidate%/cmdline}
    if is_expected_process "$candidate"; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  return 1
}

running=false
if [ -r "$pid_file" ]; then
  pid=$(cat "$pid_file")
  case "$pid" in
  '' | *[!0-9]*) pid= ;;
  esac
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    if is_expected_process "$pid"; then
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
  : >"$launcher_log"
  nohup "$entrypoint" >"$launcher_log" 2>&1 </dev/null &
  launcher_pid=$!
  attempts=0
  pid=
  while [ "$attempts" -lt 100 ]; do
    pid=$(find_expected_process || true)
    if [ -n "$pid" ]; then
      break
    fi
    if ! kill -0 "$launcher_pid" 2>/dev/null; then
      break
    fi
    sleep 0.1
    attempts=$((attempts + 1))
  done
  if [ -z "$pid" ]; then
    kill "$launcher_pid" 2>/dev/null || true
    cat "$launcher_log" >&2
    echo "code-server Feature entrypoint did not start the expected process" >&2
    exit 1
  fi
  printf '%s\n' "$pid" >"$pid_file"
fi
