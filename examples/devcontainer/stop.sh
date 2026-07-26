set -eu

write_result() {
  result_tmp_path="${CODESPACE_RESULT}.tmp.$$"
  umask 177
  printf '{"outcome":"done","stage":"stop-environment"}\n' > "$result_tmp_path"
  chmod 600 "$result_tmp_path"
  mv "$result_tmp_path" "$CODESPACE_RESULT"
}

stop_container_id="${DEVCONTAINER_EXAMPLE_CONTAINER_ID:-}"
if [ -z "$stop_container_id" ] && [ -n "${CODESPACE_WORKSPACE_DIR:-}" ]; then
  stop_container_id="$(docker ps -a --filter "label=devcontainer.local_folder=${CODESPACE_WORKSPACE_DIR}" --format '{{.ID}}' | head -n 1)"
fi
if [ -n "$stop_container_id" ]; then
  docker stop "$stop_container_id" >/dev/null 2>&1 || true
  printf 'DEVCONTAINER_EXAMPLE_CONTAINER_ID=%s\n' "$stop_container_id" >> "$CODESPACE_ENV"
fi
write_result
