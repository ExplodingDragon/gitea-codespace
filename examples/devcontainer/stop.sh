set -eu

write_result() {
  tmp="${CODESPACE_RESULT}.tmp.$$"
  umask 177
  printf '{"outcome":"done","stage":"stop-environment"}\n' > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$CODESPACE_RESULT"
}

container_id="${DEVCONTAINER_EXAMPLE_CONTAINER_ID:-}"
if [ -z "$container_id" ] && [ -n "${CODESPACE_WORKSPACE_DIR:-}" ]; then
  container_id="$(docker ps -a --filter "label=devcontainer.local_folder=${CODESPACE_WORKSPACE_DIR}" --format '{{.ID}}' | head -n 1)"
fi
if [ -n "$container_id" ]; then
  docker stop "$container_id" >/dev/null 2>&1 || true
  printf 'DEVCONTAINER_EXAMPLE_CONTAINER_ID=%s\n' "$container_id" >> "$CODESPACE_ENV"
fi
write_result
