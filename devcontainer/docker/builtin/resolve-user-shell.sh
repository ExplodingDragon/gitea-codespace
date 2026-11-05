#!/bin/sh

set -eu

user=${1%%:*}
shell=

if command -v getent >/dev/null 2>&1; then
  entry=$(getent passwd "$user" 2>/dev/null || true)
  if [ -n "$entry" ]; then
    shell=${entry##*:}
  fi
fi

if [ -z "$shell" ] && [ -r /etc/passwd ]; then
  while IFS=: read -r name _ uid _ _ _ candidate; do
    if [ "$name" = "$user" ] || [ "$uid" = "$user" ]; then
      shell=$candidate
      break
    fi
  done </etc/passwd
fi

if [ -z "$shell" ]; then
  shell=/bin/sh
fi

case "$shell" in
/*) ;;
*)
  echo "remote user login shell is not an absolute path" >&2
  exit 1
  ;;
esac

if [ ! -x "$shell" ]; then
  echo "remote user login shell is not executable: $shell" >&2
  exit 1
fi

printf '%s' "$shell"
