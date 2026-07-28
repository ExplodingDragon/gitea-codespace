set -euo pipefail

write_result() {
  result_outcome="${1:-done}"
  result_tmp_path="${CODESPACE_RESULT}.tmp.$$"
  umask 177
  printf '{"outcome":"%s","stage":"start-environment"}\n' "$result_outcome" > "$result_tmp_path"
  chmod 600 "$result_tmp_path"
  mv "$result_tmp_path" "$CODESPACE_RESULT"
}

fail_recoverable() {
  [ "$#" -eq 0 ] || printf '%s\n' "$*" >&2
  write_result recoverable_failed
  exit 0
}

fail_unrecoverable() {
  [ "$#" -eq 0 ] || printf '%s\n' "$*" >&2
  write_result unrecoverable_failed
  exit 0
}

codespace_user="${CODESPACE_USER:-codespace}"
codespace_home="$(getent passwd "$codespace_user" | cut -d: -f6)"
[ -n "$codespace_home" ] || fail_unrecoverable "Codespace user home is unavailable"
runtime_dir="${CODESPACE_RUNTIME_DIR:-/var/lib/gitea-codespace}"
runtime_seed_dir="${CODESPACE_RUNTIME_SEED_DIR:-$runtime_dir/seed}"
token_file="${CODESPACE_GITEA_TOKEN_FILE:-$runtime_dir/gitea-token}"
private_key_file="${CODESPACE_GIT_SSH_PRIVATE_KEY:-$runtime_dir/git/id_ed25519}"
public_key_file="${CODESPACE_GIT_SSH_PUBLIC_KEY:-$runtime_dir/git/id_ed25519.pub}"
known_hosts_file="${CODESPACE_GIT_SSH_KNOWN_HOSTS:-$runtime_dir/git/known_hosts}"
git_credential_helper="$runtime_dir/bin/gitea-codespace-git-credential"
git_ssh_helper="$runtime_dir/bin/gitea-codespace-git-ssh"
devcontainer_dir="$runtime_dir/devcontainer"
code_server_feature='{"ghcr.io/coder/devcontainer-features/code-server:2.0.0":{"version":"4.121.0","auth":"none","host":"0.0.0.0","port":"13337","disableTelemetry":true,"disableUpdateCheck":true,"workspace":"${containerWorkspaceFolder}"}}'
code_server_port=13337

for seed_file in "$runtime_seed_dir/gitea-token" "$runtime_seed_dir/id_ed25519" "$runtime_seed_dir/id_ed25519.pub"; do
  [ -s "$seed_file" ] || fail_unrecoverable "missing runtime seed file: $seed_file"
done
[ -f "$runtime_seed_dir/known_hosts" ] || fail_unrecoverable "missing runtime seed known_hosts file"
codespace_uid="$(id -u "$codespace_user")" || fail_unrecoverable "Codespace user is unavailable"
codespace_gid="$(id -g "$codespace_user")" || fail_unrecoverable "Codespace group is unavailable"
install -d -m 0700 -o "$codespace_uid" -g "$codespace_gid" "$runtime_dir/git" "$devcontainer_dir"
install -m 0600 -o "$codespace_uid" -g "$codespace_gid" "$runtime_seed_dir/gitea-token" "$token_file"
install -m 0600 -o "$codespace_uid" -g "$codespace_gid" "$runtime_seed_dir/id_ed25519" "$private_key_file"
install -m 0644 -o "$codespace_uid" -g "$codespace_gid" "$runtime_seed_dir/id_ed25519.pub" "$public_key_file"
install -m 0600 -o "$codespace_uid" -g "$codespace_gid" "$runtime_seed_dir/known_hosts" "$known_hosts_file"

workspace="${CODESPACE_WORKSPACE_DIR:-${CODESPACE_WORKSPACES_DIR}/${CODESPACE_REPO_NAME:-repo}}"
[ -d "$workspace/.git" ] || fail_unrecoverable "workspace is not a git repository: $workspace"
remote_url="$(sudo -u "$codespace_user" git -C "$workspace" remote get-url origin)" || fail_unrecoverable "read workspace origin failed"
case "$remote_url" in
  http://*|https://*)
    sudo -u "$codespace_user" git config --global credential.helper "!$git_credential_helper" || fail_unrecoverable "configure global Git credential helper failed"
    sudo -u "$codespace_user" git -C "$workspace" config credential.helper "!$git_credential_helper" || fail_unrecoverable "configure workspace Git credential helper failed"
    ;;
  *)
    [ -s "$known_hosts_file" ] || fail_unrecoverable "Git SSH known_hosts is empty"
    sudo -u "$codespace_user" git -C "$workspace" config core.sshCommand "$git_ssh_helper" || fail_unrecoverable "configure workspace Git SSH helper failed"
    ;;
esac

if ! docker info >/dev/null 2>&1; then
  if command -v dockerd >/dev/null 2>&1; then
    nohup dockerd >/var/log/gitea-codespace-dockerd.log 2>&1 &
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      docker info >/dev/null 2>&1 && break
      sleep 1
    done
  fi
fi
docker info >/dev/null 2>&1 || fail_recoverable "docker daemon is unavailable"

case "${GITEA_DEVCONTAINER_SOURCE:-}" in
  platform_default)
    [ -n "${GITEA_DEVCONTAINER_DEFAULT_IMAGE:-}" ] || fail_unrecoverable "platform default Dev Container image is empty"
    config_file="${GITEA_DEVCONTAINER_DEFAULT_CONFIG:-$runtime_dir/runtime/devcontainer.json}"
    GITEA_DEVCONTAINER_CONFIG_FILE="$config_file" python3 - <<'PY'
import json
import os

path = os.environ["GITEA_DEVCONTAINER_CONFIG_FILE"]
with open(path, "w", encoding="utf-8") as handle:
    json.dump({
        "name": "Gitea Codespace",
        "image": os.environ["GITEA_DEVCONTAINER_DEFAULT_IMAGE"],
        "privileged": True,
        "runArgs": ["--network=host"],
    }, handle)
    handle.write("\n")
PY
    chmod 0644 "$config_file"
    ;;
  repository)
    [ -n "${GITEA_DEVCONTAINER_PATH:-}" ] || fail_unrecoverable "repository Dev Container path is empty"
    config_file="$workspace/$GITEA_DEVCONTAINER_PATH"
    [ -f "$config_file" ] || fail_unrecoverable "selected Dev Container configuration is missing: $GITEA_DEVCONTAINER_PATH"
    committed_sha256="$(sudo -u "$codespace_user" git -C "$workspace" show "$GITEA_DEVCONTAINER_COMMIT_SHA:$GITEA_DEVCONTAINER_PATH" | sha256sum | cut -d' ' -f1)" || fail_unrecoverable "read selected Dev Container configuration from its commit failed"
    [ "$committed_sha256" = "${GITEA_DEVCONTAINER_CONTENT_SHA256:-}" ] || fail_unrecoverable "committed Dev Container configuration digest does not match the create request"
    actual_sha256="$(sha256sum "$config_file" | cut -d' ' -f1)"
    [ "$actual_sha256" = "${GITEA_DEVCONTAINER_CONTENT_SHA256:-}" ] || fail_unrecoverable "selected Dev Container configuration digest does not match the create request"
    ;;
  *)
    fail_unrecoverable "Dev Container configuration source is invalid"
    ;;
esac

container_seed_dir="$devcontainer_dir/credentials"
install -d -m 0755 -o 0 -g 0 "$container_seed_dir" "$container_seed_dir/git"
install -m 0444 -o 0 -g 0 "$token_file" "$container_seed_dir/gitea-token"
install -m 0444 -o 0 -g 0 "$private_key_file" "$container_seed_dir/git/id_ed25519"
install -m 0444 -o 0 -g 0 "$public_key_file" "$container_seed_dir/git/id_ed25519.pub"
install -m 0444 -o 0 -g 0 "$known_hosts_file" "$container_seed_dir/git/known_hosts"

up_output="$devcontainer_dir/up-output.jsonl"
rm -f "$up_output"
if ! sudo -u "$codespace_user" env HOME="$codespace_home" devcontainer up \
  --workspace-folder "$workspace" \
  --config "$config_file" \
  --mount "type=bind,source=$container_seed_dir/gitea-token,target=$runtime_dir/gitea-token" \
  --mount "type=bind,source=$container_seed_dir/git,target=$runtime_dir/git" \
  --mount "type=bind,source=$runtime_dir/bin,target=$runtime_dir/bin" \
  --mount "type=bind,source=$runtime_dir/runtime,target=$runtime_dir/runtime" \
  --additional-features "$code_server_feature" \
  | tee "$up_output"; then
  fail_recoverable "Dev Container startup failed"
fi

read_devcontainer_result() {
  python3 - "$up_output" "$1" <<'PY'
import json
import sys

with open(sys.argv[1], "r", encoding="utf-8") as handle:
    lines = handle.readlines()
for line in reversed(lines):
    try:
        value = json.loads(line)
    except json.JSONDecodeError:
        continue
    if value.get("outcome") == "success":
        result = value.get(sys.argv[2], "")
        if isinstance(result, str):
            print(result)
            sys.exit(0)
sys.exit(1)
PY
}

container_id="$(read_devcontainer_result containerId)" || fail_recoverable "Dev Container CLI did not return a container ID"
remote_user="$(read_devcontainer_result remoteUser)" || fail_recoverable "Dev Container CLI did not return a remote user"
remote_workdir="$(read_devcontainer_result remoteWorkspaceFolder)" || fail_recoverable "Dev Container CLI did not return a workspace path"
[ -n "$container_id" ] && [ -n "$remote_user" ] || fail_recoverable "Dev Container CLI returned an incomplete runtime target"
case "$remote_workdir" in
  /*) ;;
  *) fail_recoverable "Dev Container CLI returned a non-absolute workspace path" ;;
esac
docker inspect "$container_id" >/dev/null 2>&1 || fail_recoverable "Dev Container is unavailable after startup"
container_runtime="$(docker inspect --format '{{.HostConfig.Privileged}} {{.HostConfig.NetworkMode}}' "$container_id")" || fail_recoverable "inspect Dev Container runtime settings failed"
[ "$container_runtime" = "true host" ] || fail_unrecoverable "Dev Container must be privileged and use host networking"

docker exec --user "$remote_user" \
  --env "GITEA_GIT_USER_NAME=${GITEA_GIT_USER_NAME:-}" \
  --env "GITEA_GIT_USER_EMAIL=${GITEA_GIT_USER_EMAIL:-}" \
  --workdir "$remote_workdir" "$container_id" /bin/sh -c '
    set -eu
    git config --global user.name "$GITEA_GIT_USER_NAME"
    git config --global user.email "$GITEA_GIT_USER_EMAIL"
    git config --global credential.helper "!/var/lib/gitea-codespace/bin/gitea-codespace-git-credential"
    git config credential.helper "!/var/lib/gitea-codespace/bin/gitea-codespace-git-credential"
    git config core.sshCommand "/var/lib/gitea-codespace/bin/gitea-codespace-git-ssh"
  ' || fail_recoverable "configure Git inside Dev Container failed"

editor_marker="$devcontainer_dir/code-server-$container_id.initialized"
if [ ! -f "$editor_marker" ]; then
  merged_config="$devcontainer_dir/merged-configuration.json"
  if ! sudo -u "$codespace_user" env HOME="$codespace_home" devcontainer read-configuration \
    --workspace-folder "$workspace" \
    --config "$config_file" \
    --container-id "$container_id" \
    --additional-features "$code_server_feature" \
    --include-merged-configuration \
    > "$merged_config"; then
    fail_recoverable "read merged Dev Container configuration failed"
  fi

  editor_settings="$devcontainer_dir/code-server-settings.json"
  editor_extensions="$devcontainer_dir/code-server-extensions.txt"
  if ! GITEA_DEVCONTAINER_MERGED_CONFIG="$merged_config" \
    GITEA_CODE_SERVER_SETTINGS="$editor_settings" \
    GITEA_CODE_SERVER_EXTENSIONS="$editor_extensions" \
    python3 - <<'PY'
import json
import os

with open(os.environ["GITEA_DEVCONTAINER_MERGED_CONFIG"], "r", encoding="utf-8") as handle:
    result = json.load(handle)
merged = result.get("mergedConfiguration") or {}
vscode = (merged.get("customizations") or {}).get("vscode") or {}
settings = vscode.get("settings") or {}
extensions = vscode.get("extensions") or []
if not isinstance(settings, dict):
    raise SystemExit("customizations.vscode.settings must be an object")
if not isinstance(extensions, list) or any(not isinstance(value, str) or not value.strip() for value in extensions):
    raise SystemExit("customizations.vscode.extensions must contain non-empty strings")
with open(os.environ["GITEA_CODE_SERVER_SETTINGS"], "w", encoding="utf-8") as handle:
    json.dump(settings, handle, ensure_ascii=False, separators=(",", ":"), sort_keys=True)
    handle.write("\n")
with open(os.environ["GITEA_CODE_SERVER_EXTENSIONS"], "w", encoding="utf-8") as handle:
    for extension in dict.fromkeys(value.strip() for value in extensions):
        handle.write(extension + "\n")
PY
  then
    fail_unrecoverable "Dev Container VS Code customizations are invalid"
  fi

  if ! docker exec --interactive --user "$remote_user" "$container_id" /bin/sh -c '
    set -eu
    settings_dir="${XDG_DATA_HOME:-$HOME/.local/share}/code-server/User"
    mkdir -p "$settings_dir"
    umask 077
    cat > "$settings_dir/settings.json"
  ' < "$editor_settings"; then
    fail_recoverable "configure code-server settings failed"
  fi

  while IFS= read -r extension; do
    [ -n "$extension" ] || continue
    if ! docker exec --user "$remote_user" "$container_id" code-server --install-extension "$extension"; then
      printf 'warning: code-server extension installation failed: %s\n' "$extension" >&2
    fi
  done < "$editor_extensions"
  : > "$editor_marker"
  chmod 0600 "$editor_marker"
fi

editor_ready=0
for _ in $(seq 1 60); do
  if curl --fail --silent --show-error --max-time 2 "http://127.0.0.1:$code_server_port/healthz" >/dev/null; then
    editor_ready=1
    break
  fi
  sleep 1
done
if [ "$editor_ready" != "1" ]; then
  docker exec "$container_id" /bin/sh -c 'test ! -f /tmp/code-server.log || cat /tmp/code-server.log' >&2 || true
  fail_recoverable "code-server health check failed"
fi

printf '%s\n' \
  "CODESPACE_WORKSPACE_DIR=$workspace" \
  "CODESPACE_DEVCONTAINER_ID=$container_id" \
  "CODESPACE_DEVCONTAINER_USER=$remote_user" \
  "CODESPACE_DEVCONTAINER_WORKDIR=$remote_workdir" \
  "CODESPACE_DEVCONTAINER_CONFIG=$config_file" \
  "CODESPACE_WEB_IDE_PORT=$code_server_port" \
  >> "$CODESPACE_ENV"
write_result done
