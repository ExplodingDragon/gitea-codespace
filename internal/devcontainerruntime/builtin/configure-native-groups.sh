#!/bin/sh
set -eu

user=${1%%:*}
requested_user=$1
shift
if [ "$user" != "$requested_user" ] && [ "$#" -gt 0 ]; then
  printf 'Device groups require remoteUser to be a username or UID without an explicit group: %s\n' "$requested_user" >&2
  exit 1
fi
name=$(awk -F: -v user="$user" '$1 == user || $3 == user { print $1; exit }' /etc/passwd)
if [ -z "$name" ] && [ "$#" -gt 0 ]; then
  printf 'Cannot resolve remote user %s for device groups\n' "$user" >&2
  exit 1
fi

# Named entries let Docker exec resolve the current groups as well as PID 1.
# Replace only platform-owned entries when a resumed Pod gets a new allocation.
temporary=$(mktemp /etc/.codespace-groups.XXXXXX)
trap 'rm -f "$temporary"' EXIT
awk -F: '$1 !~ /^codespace-device-[0-9]+$/ { print }' /etc/group >"$temporary"
for gid; do
  if [ "$gid" != 0 ]; then
    printf 'codespace-device-%s:x:%s:%s\n' "$gid" "$gid" "$name" >>"$temporary"
  fi
done
chmod 644 "$temporary"
mv "$temporary" /etc/group
