set -eu

user="$1"
uid="$2"
gid="$3"
user="${user%%:*}"
case "$user" in
  *[!0-9]*) entry="$(awk -F: -v user="$user" '$1 == user { print; exit }' /etc/passwd)" ;;
  *) entry="$(awk -F: -v uid="$user" '$3 == uid { print; exit }' /etc/passwd)" ;;
esac
[ -n "$entry" ] || { echo "remote user $user does not exist" >&2; exit 1; }

name="${entry%%:*}"
old_gid="$(printf '%s' "$entry" | cut -d: -f4)"
home="$(printf '%s' "$entry" | cut -d: -f6)"
uid_owner="$(awk -F: -v uid="$uid" '$3 == uid { print $1; exit }' /etc/passwd)"
[ -z "$uid_owner" ] || [ "$uid_owner" = "$name" ] || { echo "target uid is used by $uid_owner" >&2; exit 1; }

group="$(awk -F: -v gid="$old_gid" '$3 == gid { print $1; exit }' /etc/group)"
gid_owner="$(awk -F: -v gid="$gid" '$3 == gid { print $1; exit }' /etc/group)"
[ -z "$gid_owner" ] || [ "$gid_owner" = "$group" ] || { echo "target gid is used by $gid_owner" >&2; exit 1; }

awk -F: -v OFS=: -v name="$name" -v uid="$uid" -v gid="$gid" '$1 == name { $3=uid; $4=gid } { print }' /etc/passwd > /etc/passwd.devcontainer
cat /etc/passwd.devcontainer > /etc/passwd
rm /etc/passwd.devcontainer
if [ -n "$group" ]; then
  awk -F: -v OFS=: -v group="$group" -v gid="$gid" '$1 == group { $3=gid } { print }' /etc/group > /etc/group.devcontainer
  cat /etc/group.devcontainer > /etc/group
  rm /etc/group.devcontainer
fi
[ -z "$home" ] || chown -R "$uid:$gid" "$home"
