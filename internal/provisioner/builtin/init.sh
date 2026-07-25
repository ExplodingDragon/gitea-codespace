set -eu

write_result() {
  tmp="${CODESPACE_RESULT}.tmp.$$"
  umask 177
  printf '{"outcome":"done","stage":"initialize-system"}\n' > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$CODESPACE_RESULT"
}

cleanup_policy_rc() {
  if [ "${policy_rc_created:-}" = "1" ]; then
    rm -f /usr/sbin/policy-rc.d
  fi
}

trap cleanup_policy_rc EXIT

configure_apt_mirrors() {
  mirror=""
  security_mirror=""
  if [ -r /etc/os-release ]; then
    . /etc/os-release
    case "${ID:-}" in
      debian)
        mirror="https://mirrors.tuna.tsinghua.edu.cn/debian"
        security_mirror="https://mirrors.tuna.tsinghua.edu.cn/debian-security"
        ;;
      ubuntu)
        mirror="https://mirrors.tuna.tsinghua.edu.cn/ubuntu"
        security_mirror="$mirror"
        ;;
    esac
  fi
  [ -n "$mirror" ] || return 0
  for file in /etc/apt/sources.list /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources; do
    [ -f "$file" ] || continue
    [ -f "${file}.gitea-codespace.bak" ] || cp "$file" "${file}.gitea-codespace.bak"
    sed -i \
      -e "s|https\?://deb.debian.org/debian-security|$security_mirror|g" \
      -e "s|https\?://security.debian.org/debian-security|$security_mirror|g" \
      -e "s|https\?://deb.debian.org/debian|$mirror|g" \
      -e "s|https\?://archive.ubuntu.com/ubuntu|$mirror|g" \
      -e "s|https\?://security.ubuntu.com/ubuntu|$security_mirror|g" \
      "$file"
  done
}

configure_dnf_mirrors() {
  if [ -r /etc/os-release ]; then
    . /etc/os-release
  fi
  [ "${ID:-}" = "fedora" ] || return 0
  for file in /etc/yum.repos.d/*.repo; do
    [ -f "$file" ] || continue
    [ -f "${file}.gitea-codespace.bak" ] || cp "$file" "${file}.gitea-codespace.bak"
    sed -i \
      -e 's|^metalink=|#metalink=|' \
      -e 's|^mirrorlist=|#mirrorlist=|' \
      -e 's|^#baseurl=http://download.example/pub/fedora/linux|baseurl=https://mirrors.tuna.tsinghua.edu.cn/fedora|g' \
      -e 's|^baseurl=https\?://download.fedoraproject.org/pub/fedora/linux|baseurl=https://mirrors.tuna.tsinghua.edu.cn/fedora|g' \
      "$file"
  done
}

configure_pacman_mirrors() {
  mirror='Server = https://mirrors.tuna.tsinghua.edu.cn/archlinux/$repo/os/$arch'
  if [ -f /etc/pacman.d/mirrorlist ] && ! grep -Fq "$mirror" /etc/pacman.d/mirrorlist; then
    [ -f /etc/pacman.d/mirrorlist.gitea-codespace.bak ] || cp /etc/pacman.d/mirrorlist /etc/pacman.d/mirrorlist.gitea-codespace.bak
    tmp="/etc/pacman.d/mirrorlist.tmp.$$"
    printf '%s\n' "$mirror" > "$tmp"
    cat /etc/pacman.d/mirrorlist >> "$tmp"
    mv "$tmp" /etc/pacman.d/mirrorlist
  fi
}

install_missing() {
  missing=""
  for command in git ssh sudo flock getent useradd groupadd; do
    if ! command -v "$command" >/dev/null 2>&1; then
      missing="$missing $command"
    fi
  done
  if [ -z "$missing" ]; then
    return 0
  fi
  if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    configure_apt_mirrors
    if [ ! -e /usr/sbin/policy-rc.d ]; then
      printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d
      chmod 0755 /usr/sbin/policy-rc.d
      policy_rc_created=1
    fi
    apt-get update
    apt-get install -y --no-install-recommends ca-certificates curl git openssh-client sudo util-linux passwd python3
  elif command -v dnf >/dev/null 2>&1; then
    configure_dnf_mirrors
    dnf install -y --setopt=install_weak_deps=False ca-certificates curl git openssh-clients sudo util-linux shadow-utils python3
  elif command -v pacman >/dev/null 2>&1; then
    configure_pacman_mirrors
    pacman -Sy --noconfirm ca-certificates curl git openssh sudo util-linux shadow python
  else
    exit 20
  fi
}

install_missing

codespace_user="${CODESPACE_USER:-codespace}"
case "$codespace_user" in
  ""|[0-9]*|-*|*[!abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-]*)
    exit 23
    ;;
esac

if ! id -u "$codespace_user" >/dev/null 2>&1; then
  useradd -m -s /bin/bash "$codespace_user"
fi
codespace_uid="$(id -u "$codespace_user")"
codespace_gid="$(id -g "$codespace_user")"
if [ "$codespace_uid" = "0" ] || [ "$codespace_gid" = "0" ]; then
  exit 24
fi

passwd -l "$codespace_user" >/dev/null 2>&1 || true
mkdir -p /workspaces
chown "$codespace_uid:$codespace_gid" /workspaces
chmod 0755 /workspaces

runtime_dir="${CODESPACE_RUNTIME_DIR:-/var/lib/gitea-codespace}"
runtime_seed_dir="${CODESPACE_RUNTIME_SEED_DIR:-$runtime_dir/seed}"
token_file="${CODESPACE_GITEA_TOKEN_FILE:-$runtime_dir/gitea-token}"
private_key_file="${CODESPACE_GIT_SSH_PRIVATE_KEY:-$runtime_dir/git/id_ed25519}"
public_key_file="${CODESPACE_GIT_SSH_PUBLIC_KEY:-$runtime_dir/git/id_ed25519.pub}"
known_hosts_file="${CODESPACE_GIT_SSH_KNOWN_HOSTS:-$runtime_dir/git/known_hosts}"
seed_token="$runtime_seed_dir/gitea-token"
seed_private_key="$runtime_seed_dir/id_ed25519"
seed_public_key="$runtime_seed_dir/id_ed25519.pub"
seed_known_hosts="$runtime_seed_dir/known_hosts"

for seed_file in "$seed_token" "$seed_private_key" "$seed_public_key" "$seed_known_hosts"; do
  [ -s "$seed_file" ] || exit 25
done

install -d -m 0755 -o 0 -g 0 "$runtime_dir"
install -d -m 0700 -o "$codespace_uid" -g "$codespace_gid" "$runtime_dir/git" "$runtime_dir/runtime"
install -d -m 0700 -o 0 -g 0 "$runtime_dir/results"
install -m 0600 -o "$codespace_uid" -g "$codespace_gid" "$seed_token" "$token_file"
install -m 0600 -o "$codespace_uid" -g "$codespace_gid" "$seed_private_key" "$private_key_file"
install -m 0644 -o "$codespace_uid" -g "$codespace_gid" "$seed_public_key" "$public_key_file"
install -m 0600 -o "$codespace_uid" -g "$codespace_gid" "$seed_known_hosts" "$known_hosts_file"

cat >/usr/local/bin/gitea-codespace-git-credential <<'EOF'
#!/bin/sh
set -eu
while IFS= read -r line; do
  [ -n "$line" ] || break
done
printf 'username=codespace\n'
printf 'password=%s\n\n' "$(cat /var/lib/gitea-codespace/gitea-token)"
EOF
chmod 0755 /usr/local/bin/gitea-codespace-git-credential

cat >/usr/local/bin/gitea-codespace-endpoint <<'EOF'
#!/usr/bin/env python3
import json
import os
import sys

manifest = "/var/lib/gitea-codespace/runtime/endpoints.json"
if len(sys.argv) < 2 or sys.argv[1] not in ("set", "delete"):
    print("usage: gitea-codespace-endpoint set <id> <label> <http|https> <port> [--public] | delete <id>", file=sys.stderr)
    sys.exit(2)
try:
    with open(manifest, "r", encoding="utf-8") as handle:
        data = json.load(handle)
except FileNotFoundError:
    data = {"version": 1, "endpoints": []}
endpoints = [entry for entry in data.get("endpoints", []) if entry.get("endpoint_id") != sys.argv[2]]
if sys.argv[1] == "set":
    if len(sys.argv) not in (6, 7):
        sys.exit(2)
    public = len(sys.argv) == 7 and sys.argv[6] == "--public"
    endpoints.append({
        "endpoint_id": sys.argv[2],
        "label": sys.argv[3],
        "upstream_scheme": sys.argv[4],
        "upstream_port": int(sys.argv[5]),
        "public": public,
    })
data = {"version": 1, "endpoints": endpoints}
tmp = manifest + ".tmp." + str(os.getpid())
with open(tmp, "w", encoding="utf-8") as handle:
    json.dump(data, handle, separators=(",", ":"), sort_keys=True)
    handle.write("\n")
    handle.flush()
    os.fsync(handle.fileno())
os.replace(tmp, manifest)
EOF
chmod 0755 /usr/local/bin/gitea-codespace-endpoint

printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$codespace_user" > /etc/sudoers.d/gitea-codespace
chmod 0440 /etc/sudoers.d/gitea-codespace
if command -v visudo >/dev/null 2>&1; then
  visudo -cf /etc/sudoers.d/gitea-codespace >/dev/null
fi
printf 'CODESPACE_CREDENTIAL_UID=%s\nCODESPACE_CREDENTIAL_GID=%s\nCODESPACE_USER=%s\n' "$codespace_uid" "$codespace_gid" "$codespace_user" >> "$CODESPACE_ENV"
write_result
