set -euo pipefail

write_result() {
  result_outcome="${1:-done}"
  result_tmp_path="${CODESPACE_RESULT}.tmp.$$"
  umask 177
  printf '{"outcome":"%s","stage":"stop-environment"}\n' "$result_outcome" > "$result_tmp_path"
  chmod 600 "$result_tmp_path"
  mv "$result_tmp_path" "$CODESPACE_RESULT"
}

container_id="${CODESPACE_DEVCONTAINER_ID:-}"
if [ -n "$container_id" ] && docker inspect "$container_id" >/dev/null 2>&1; then
  if [ "$(docker inspect --format '{{.State.Running}}' "$container_id")" = "true" ]; then
    if ! docker stop "$container_id"; then
      if [ "$(docker inspect --format '{{.State.Running}}' "$container_id" 2>/dev/null || printf false)" = "true" ]; then
        write_result recoverable_failed
        exit 0
      fi
    fi
  fi
  printf 'CODESPACE_DEVCONTAINER_ID=%s\n' "$container_id" >> "$CODESPACE_ENV"
fi
write_result done
