set -eu

write_result() {
  outcome="${1:-done}"
  tmp="${CODESPACE_RESULT}.tmp.$$"
  umask 177
  printf '{"outcome":"%s","stage":"stop-environment"}\n' "$outcome" > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$CODESPACE_RESULT"
}

write_result done
