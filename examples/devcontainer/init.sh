set -eu

write_result() {
  tmp="${CODESPACE_RESULT}.tmp.$$"
  umask 177
  printf '{"outcome":"%s","stage":"initialize-system"}\n' "$1" > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$CODESPACE_RESULT"
}

fail_recoverable() {
  write_result recoverable_failed
  exit 0
}

fail_unrecoverable() {
  write_result unrecoverable_failed
  exit 0
}

install_missing() {
  missing=""
  for command in curl git ssh ssh-keygen sudo flock getent useradd groupadd python3 npm docker; do
    if ! command -v "$command" >/dev/null 2>&1; then
      missing="$missing $command"
    fi
  done
  if [ -z "$missing" ]; then
    return 0
  fi
  if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update || fail_recoverable
    apt-get install -y ca-certificates curl git openssh-client sudo util-linux passwd python3 npm docker.io || fail_recoverable
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y ca-certificates curl git openssh-clients sudo util-linux shadow-utils python3 npm docker || fail_recoverable
  elif command -v pacman >/dev/null 2>&1; then
    pacman -Sy --noconfirm ca-certificates curl git openssh sudo util-linux shadow python npm docker || fail_recoverable
  else
    fail_unrecoverable
  fi
}

install_missing

if getent group codespace >/dev/null 2>&1; then
  if [ "$(getent group codespace | cut -d: -f3)" != "1000" ]; then
    fail_unrecoverable
  fi
else
  groupadd -g 1000 codespace || fail_unrecoverable
fi

if id -u codespace >/dev/null 2>&1; then
  if [ "$(id -u codespace)" != "1000" ] || [ "$(id -g codespace)" != "1000" ]; then
    fail_unrecoverable
  fi
else
  useradd -m -u 1000 -g 1000 -s /bin/bash codespace || fail_unrecoverable
fi

if getent group docker >/dev/null 2>&1; then
  usermod -aG docker codespace || fail_recoverable
fi

passwd -l codespace >/dev/null 2>&1 || true
mkdir -p /workspaces /var/lib/gitea-codespace /var/lib/gitea-codespace/git /var/lib/gitea-codespace/results /var/lib/gitea-codespace/devcontainer
chown codespace:codespace /workspaces
chmod 0755 /workspaces

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

printf 'codespace ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/gitea-codespace
chmod 0440 /etc/sudoers.d/gitea-codespace
if command -v visudo >/dev/null 2>&1; then
  visudo -cf /etc/sudoers.d/gitea-codespace >/dev/null || fail_unrecoverable
fi

if ! docker info >/dev/null 2>&1; then
  if command -v dockerd >/dev/null 2>&1; then
    nohup dockerd >/var/log/gitea-codespace-dockerd.log 2>&1 &
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      docker info >/dev/null 2>&1 && break
      sleep 1
    done
  fi
fi
docker info >/dev/null 2>&1 || fail_recoverable

cli_version="${DEVCONTAINER_EXAMPLE_CLI_VERSION:-0.75.0}"
if ! command -v devcontainer >/dev/null 2>&1; then
  npm install -g "@devcontainers/cli@${cli_version}" || fail_recoverable
fi

printf '%s\n' \
  'CODESPACE_CREDENTIAL_UID=1000' \
  'CODESPACE_CREDENTIAL_GID=1000' \
  "DEVCONTAINER_EXAMPLE_CLI_VERSION=${cli_version}" \
  >> "$CODESPACE_ENV"
write_result done
