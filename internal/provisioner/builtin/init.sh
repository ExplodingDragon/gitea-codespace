set -euo pipefail

write_result() {
  result_outcome="${1:-done}"
  result_tmp_path="${CODESPACE_RESULT}.tmp.$$"
  umask 177
  printf '{"outcome":"%s","stage":"initialize-system"}\n' "$result_outcome" > "$result_tmp_path"
  chmod 600 "$result_tmp_path"
  mv "$result_tmp_path" "$CODESPACE_RESULT"
}

log_error() {
  printf '%s\n' "$*" >&2
}

fail_unrecoverable() {
  if [ "$#" -gt 0 ]; then
    log_error "$*"
  fi
  write_result unrecoverable_failed
  exit 0
}

cleanup_policy_rc() {
  if [ "${policy_rc_created:-}" = "1" ]; then
    rm -f /usr/sbin/policy-rc.d
  fi
}

trap cleanup_policy_rc EXIT

configure_apt_mirrors() {
  apt_mirror=""
  apt_security_mirror=""
  if [ -r /etc/os-release ]; then
    . /etc/os-release
    case "${ID:-}" in
      debian)
        apt_mirror="https://mirrors.tuna.tsinghua.edu.cn/debian"
        apt_security_mirror="https://mirrors.tuna.tsinghua.edu.cn/debian-security"
        ;;
      ubuntu)
        apt_mirror="https://mirrors.tuna.tsinghua.edu.cn/ubuntu"
        apt_security_mirror="$apt_mirror"
        ;;
    esac
  fi
  [ -n "$apt_mirror" ] || return 0
  for file in /etc/apt/sources.list /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources; do
    [ -f "$file" ] || continue
    [ -f "${file}.gitea-codespace.bak" ] || cp "$file" "${file}.gitea-codespace.bak"
    sed -i \
      -e "s|https\?://deb.debian.org/debian-security|$apt_security_mirror|g" \
      -e "s|https\?://security.debian.org/debian-security|$apt_security_mirror|g" \
      -e "s|https\?://deb.debian.org/debian|$apt_mirror|g" \
      -e "s|https\?://archive.ubuntu.com/ubuntu|$apt_mirror|g" \
      -e "s|https\?://security.ubuntu.com/ubuntu|$apt_security_mirror|g" \
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
  pacman_mirror='Server = https://mirrors.tuna.tsinghua.edu.cn/archlinux/$repo/os/$arch'
  if [ -f /etc/pacman.d/mirrorlist ] && ! grep -Fq "$pacman_mirror" /etc/pacman.d/mirrorlist; then
    [ -f /etc/pacman.d/mirrorlist.gitea-codespace.bak ] || cp /etc/pacman.d/mirrorlist /etc/pacman.d/mirrorlist.gitea-codespace.bak
    pacman_tmp_path="/etc/pacman.d/mirrorlist.tmp.$$"
    printf '%s\n' "$pacman_mirror" > "$pacman_tmp_path"
    cat /etc/pacman.d/mirrorlist >> "$pacman_tmp_path"
    mv "$pacman_tmp_path" /etc/pacman.d/mirrorlist
  fi
}

install_missing() {
  missing_commands=""
  for command in bash git ssh sudo flock getent useradd usermod groupadd python3 curl tar xz docker sha256sum; do
    if ! command -v "$command" >/dev/null 2>&1; then
      missing_commands="$missing_commands $command"
    fi
  done
  if [ -z "$missing_commands" ]; then
    return 0
  fi
  if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    configure_apt_mirrors
    if [ ! -e /usr/sbin/policy-rc.d ]; then
      printf '#!/bin/bash\nexit 101\n' > /usr/sbin/policy-rc.d
      chmod 0755 /usr/sbin/policy-rc.d
      policy_rc_created=1
    fi
    apt-get update
    apt-get install -y --no-install-recommends bash ca-certificates curl git openssh-client sudo util-linux passwd python3 tar xz-utils docker.io coreutils
  elif command -v dnf >/dev/null 2>&1; then
    configure_dnf_mirrors
    dnf install -y --setopt=install_weak_deps=False bash ca-certificates curl git openssh-clients sudo util-linux shadow-utils python3 tar xz docker coreutils
  elif command -v pacman >/dev/null 2>&1; then
    configure_pacman_mirrors
    pacman -Sy --noconfirm bash ca-certificates curl git openssh sudo util-linux shadow python tar xz docker coreutils
  else
    fail_unrecoverable "no supported package manager found for installing runtime dependencies"
  fi
}

install_missing

codespace_user="${CODESPACE_USER:-codespace}"
case "$codespace_user" in
  ""|[0-9]*|-*|*[!abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-]*)
    fail_unrecoverable "invalid codespace user: $codespace_user"
    ;;
esac

if ! id -u "$codespace_user" >/dev/null 2>&1; then
  useradd -m -s /bin/bash "$codespace_user"
fi
codespace_uid="$(id -u "$codespace_user")"
codespace_gid="$(id -g "$codespace_user")"
if [ "$codespace_uid" = "0" ] || [ "$codespace_gid" = "0" ]; then
  fail_unrecoverable "codespace user must not resolve to uid or gid 0: $codespace_user"
fi

if getent group docker >/dev/null 2>&1; then
  usermod -aG docker "$codespace_user" || fail_unrecoverable "add Codespace user to docker group failed"
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
runtime_bin_dir="$runtime_dir/bin"
git_credential_helper="$runtime_bin_dir/gitea-codespace-git-credential"
seed_token="$runtime_seed_dir/gitea-token"
seed_private_key="$runtime_seed_dir/id_ed25519"
seed_public_key="$runtime_seed_dir/id_ed25519.pub"
seed_known_hosts="$runtime_seed_dir/known_hosts"

for seed_file in "$seed_token" "$seed_private_key" "$seed_public_key"; do
  [ -s "$seed_file" ] || fail_unrecoverable "missing runtime seed file: $seed_file"
done
[ -f "$seed_known_hosts" ] || fail_unrecoverable "missing runtime seed known_hosts file: $seed_known_hosts"

install -d -m 0755 -o 0 -g 0 "$runtime_dir"
install -d -m 0755 -o 0 -g 0 "$runtime_bin_dir"
install -d -m 0700 -o "$codespace_uid" -g "$codespace_gid" "$runtime_dir/git" "$runtime_dir/runtime" "$runtime_dir/devcontainer"
install -d -m 0700 -o 0 -g 0 "$runtime_dir/results"
install -m 0600 -o "$codespace_uid" -g "$codespace_gid" "$seed_token" "$token_file"
install -m 0600 -o "$codespace_uid" -g "$codespace_gid" "$seed_private_key" "$private_key_file"
install -m 0644 -o "$codespace_uid" -g "$codespace_gid" "$seed_public_key" "$public_key_file"
install -m 0600 -o "$codespace_uid" -g "$codespace_gid" "$seed_known_hosts" "$known_hosts_file"

git_user() {
  if [ -n "${GIT_SSH_COMMAND:-}" ]; then
    sudo -u "$codespace_user" env GIT_SSH_COMMAND="$GIT_SSH_COMMAND" git "$@"
  else
    sudo -u "$codespace_user" git "$@"
  fi
}

ensure_git_ssh() {
  [ -f "$private_key_file" ] || { log_error "missing git ssh private key: $private_key_file"; return 1; }
  [ -s "$known_hosts_file" ] || { log_error "missing git ssh known_hosts: $known_hosts_file"; return 1; }
  export GIT_SSH_COMMAND="ssh -i $private_key_file -o IdentitiesOnly=yes -o UserKnownHostsFile=$known_hosts_file -o StrictHostKeyChecking=yes"
}

configure_git_credentials() {
  credential_repo_url="$1"
  if [ -n "${GITEA_REPO_CLONE_HTTP_URL:-}" ]; then
    git_user config --global credential.helper "!$git_credential_helper" || return 1
  fi
  if [ -n "${GITEA_GIT_USER_NAME:-}" ]; then
    git_user config --global user.name "$GITEA_GIT_USER_NAME" || return 1
  fi
  if [ -n "${GITEA_GIT_USER_EMAIL:-}" ]; then
    git_user config --global user.email "$GITEA_GIT_USER_EMAIL" || return 1
  fi
  if [ "$credential_repo_url" = "${GITEA_REPO_CLONE_SSH_URL:-}" ] && [ -n "$credential_repo_url" ]; then
    ensure_git_ssh || return 1
  fi
}

configure_workspace_credentials() {
  credential_workspace="$1"
  credential_remote_url="$(git_user -C "$credential_workspace" remote get-url origin)" || return 1
  configure_git_credentials "$credential_remote_url" || return 1
  case "$credential_remote_url" in
    http://*|https://*)
      [ -n "${GITEA_REPO_CLONE_HTTP_URL:-}" ] || return 1
      git_user -C "$credential_workspace" config credential.helper "!$git_credential_helper" || return 1
      ;;
    *)
      ensure_git_ssh || return 1
      ;;
  esac
  if [ -n "${GITEA_GIT_USER_NAME:-}" ]; then
    git_user -C "$credential_workspace" config user.name "$GITEA_GIT_USER_NAME" || return 1
  fi
  if [ -n "${GITEA_GIT_USER_EMAIL:-}" ]; then
    git_user -C "$credential_workspace" config user.email "$GITEA_GIT_USER_EMAIL" || return 1
  fi
}

restore_existing_workspace() {
  existing_workspace="$1"
  [ -d "$existing_workspace/.git" ] || { log_error "workspace is not a git repository: $existing_workspace"; return 1; }
  configure_workspace_credentials "$existing_workspace" || { log_error "configure existing workspace credentials failed: $existing_workspace"; return 1; }
  if [ -n "${GITEA_COMMIT_SHA:-}" ]; then
    existing_head="$(git_user -C "$existing_workspace" rev-parse HEAD)" || { log_error "read existing workspace HEAD failed: $existing_workspace"; return 1; }
    [ "$existing_head" = "$GITEA_COMMIT_SHA" ] || { log_error "existing workspace commit mismatch: expected $GITEA_COMMIT_SHA got $existing_head"; return 1; }
  fi
}

prepare_workspace_target() {
  candidate_workspace="$1"
  if [ ! -e "$candidate_workspace" ]; then
    return 0
  fi
  if [ -d "$candidate_workspace/.git" ]; then
    log_error "workspace already contains a git repository: $candidate_workspace"
    return 1
  fi
  if [ -d "$candidate_workspace" ]; then
    if rmdir "$candidate_workspace" 2>/dev/null; then
      log_error "removed empty workspace directory before clone: $candidate_workspace"
      return 0
    fi
    log_error "workspace path already exists and is not empty or a git repository: $candidate_workspace"
    return 1
  fi
  log_error "workspace path already exists and is not a directory: $candidate_workspace"
  return 1
}

prepare_workspace_from_repo() {
  clone_repo_url="$1"
  target_workspace="$2"
  clone_temp_workspace="${CODESPACE_WORKSPACES_DIR}/.gitea-create-${CODESPACE_UUID}"
  [ "$target_workspace" != "$clone_temp_workspace" ] || { log_error "workspace path conflicts with temporary clone path: $target_workspace"; return 1; }
  prepare_workspace_target "$target_workspace" || return 1
  rm -rf "$clone_temp_workspace" || { log_error "remove temporary workspace failed: $clone_temp_workspace"; return 1; }
  configure_git_credentials "$clone_repo_url" || { log_error "configure git credentials failed for repository URL: $clone_repo_url"; return 1; }
  git_user clone "$clone_repo_url" "$clone_temp_workspace" || { log_error "clone repository failed: $clone_repo_url"; return 1; }
  git_user -C "$clone_temp_workspace" remote set-url origin "$clone_repo_url" || { log_error "set workspace origin failed: $clone_repo_url"; return 1; }
  configure_workspace_credentials "$clone_temp_workspace" || { log_error "configure cloned workspace credentials failed: $clone_temp_workspace"; return 1; }
  if [ -n "${GITEA_COMMIT_SHA:-}" ]; then
    if [ -n "${GITEA_START_REF:-}" ]; then
      git_user -C "$clone_temp_workspace" fetch origin "$GITEA_START_REF" --tags --prune || { log_error "fetch start ref failed: $GITEA_START_REF"; return 1; }
    else
      git_user -C "$clone_temp_workspace" fetch --all --tags --prune || { log_error "fetch repository refs failed"; return 1; }
    fi
    git_user -C "$clone_temp_workspace" checkout --detach "$GITEA_COMMIT_SHA" || { log_error "checkout commit failed: $GITEA_COMMIT_SHA"; return 1; }
    cloned_head="$(git_user -C "$clone_temp_workspace" rev-parse HEAD)" || { log_error "read cloned workspace HEAD failed"; return 1; }
    [ "$cloned_head" = "$GITEA_COMMIT_SHA" ] || { log_error "cloned workspace commit mismatch: expected $GITEA_COMMIT_SHA got $cloned_head"; return 1; }
  fi
  prepare_workspace_target "$target_workspace" || return 1
  mv "$clone_temp_workspace" "$target_workspace" || { log_error "move prepared workspace into place failed: $clone_temp_workspace -> $target_workspace"; return 1; }
}

cat >"$git_credential_helper" <<'EOF'
#!/bin/bash
set -eu
while IFS= read -r line; do
  [ -n "$line" ] || break
done
printf 'username=codespace\n'
printf 'password=%s\n\n' "$(cat /var/lib/gitea-codespace/gitea-token)"
EOF
chmod 0755 "$git_credential_helper"

cat >"$runtime_bin_dir/gitea-codespace-git-ssh" <<'EOF'
#!/bin/bash
set -euo pipefail
seed_dir="/var/lib/gitea-codespace/git"
key_dir="${HOME}/.cache/gitea-codespace/git"
install -d -m 0700 "$key_dir"
install -m 0600 "$seed_dir/id_ed25519" "$key_dir/id_ed25519"
install -m 0600 "$seed_dir/known_hosts" "$key_dir/known_hosts"
exec ssh -i "$key_dir/id_ed25519" -o IdentitiesOnly=yes -o UserKnownHostsFile="$key_dir/known_hosts" -o StrictHostKeyChecking=yes "$@"
EOF
chmod 0755 "$runtime_bin_dir/gitea-codespace-git-ssh"

cat >"$runtime_bin_dir/gitea-codespace-endpoint" <<'EOF'
#!/usr/bin/env python3
import json
import os
import sys

manifest = "/var/lib/gitea-codespace/runtime/endpoints.json"
if len(sys.argv) < 2 or sys.argv[1] not in ("set", "delete"):
    print("usage: gitea-codespace-endpoint set <id> <label> <http|https> <port> [--public] | delete <id>", file=sys.stderr)
    sys.exit(2)
if len(sys.argv) < 3 or sys.argv[2] == "workspace":
    print("endpoint id 'workspace' is reserved for the Web IDE", file=sys.stderr)
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
chmod 0755 "$runtime_bin_dir/gitea-codespace-endpoint"

printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$codespace_user" > /etc/sudoers.d/gitea-codespace
chmod 0440 /etc/sudoers.d/gitea-codespace
if command -v visudo >/dev/null 2>&1; then
  visudo -cf /etc/sudoers.d/gitea-codespace >/dev/null
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
docker info >/dev/null 2>&1 || fail_unrecoverable "docker daemon is unavailable"

devcontainer_cli_version="0.88.0"
devcontainer_install_dir="/opt/gitea-codespace/devcontainers"
devcontainer_bin="$devcontainer_install_dir/bin/devcontainer"
if [ ! -x "$devcontainer_bin" ] || [ "$("$devcontainer_bin" --version)" != "$devcontainer_cli_version" ]; then
  install -d -m 0755 -o 0 -g 0 "$devcontainer_install_dir"
  devcontainer_installer="$runtime_dir/devcontainer-install.sh"
  curl -fL --retry 3 --retry-delay 2 \
    -o "$devcontainer_installer" \
    "https://raw.githubusercontent.com/devcontainers/cli/v$devcontainer_cli_version/scripts/install.sh" \
    || fail_unrecoverable "download Dev Container CLI installer failed"
  sh "$devcontainer_installer" \
    --prefix "$devcontainer_install_dir" \
    --version "$devcontainer_cli_version" \
    --node-version 20 \
    || fail_unrecoverable "install Dev Container CLI failed"
fi
ln -sfn "$devcontainer_bin" /usr/local/bin/devcontainer

if [ "${CODESPACE_OPERATION:-}" = "create" ]; then
  create_repo_url="${GITEA_REPO_CLONE_HTTP_URL:-}"
  create_fallback_url="${GITEA_REPO_CLONE_SSH_URL:-}"
  if [ "${GITEA_GIT_PROTOCOL:-http}" = "ssh" ] && [ -n "${GITEA_REPO_CLONE_SSH_URL:-}" ]; then
    create_repo_url="$GITEA_REPO_CLONE_SSH_URL"
    create_fallback_url="${GITEA_REPO_CLONE_HTTP_URL:-}"
  fi
  [ -n "$create_repo_url" ] || fail_unrecoverable "repository clone URL is empty"
  create_workspace="${CODESPACE_WORKSPACES_DIR}/${GITEA_REPO_NAME:-repo}"
  if [ -d "$create_workspace/.git" ]; then
    restore_existing_workspace "$create_workspace" || fail_unrecoverable "existing workspace cannot be restored: $create_workspace"
  elif ! prepare_workspace_from_repo "$create_repo_url" "$create_workspace"; then
    if [ -n "$create_fallback_url" ] && [ "$create_fallback_url" != "$create_repo_url" ]; then
      prepare_workspace_from_repo "$create_fallback_url" "$create_workspace" || fail_unrecoverable "clone failed with primary and fallback repository URLs"
    else
      fail_unrecoverable "clone failed and no fallback repository URL is available"
    fi
  fi
  printf 'CODESPACE_WORKSPACE_DIR=%s\n' "$create_workspace" >> "$CODESPACE_ENV"
fi

printf 'CODESPACE_CREDENTIAL_UID=%s\nCODESPACE_CREDENTIAL_GID=%s\nCODESPACE_USER=%s\n' "$codespace_uid" "$codespace_gid" "$codespace_user" >> "$CODESPACE_ENV"
write_result done
