#!/bin/sh
# Copyright 2026 The Gitea Authors. All rights reserved.
# SPDX-License-Identifier: MIT

set -eu

output=${CODESPACE_WEB_IDE_OUTPUT:-/output}
expected_provider=${CODESPACE_WEB_IDE_EXPECTED_PROVIDER:-}
staging="$output/.provider-$$"

test -n "$expected_provider" || {
  echo "CODESPACE_WEB_IDE_EXPECTED_PROVIDER is required" >&2
  exit 1
}
test ! -e "$output/current" || {
  echo "Web IDE provider output is not empty" >&2
  exit 1
}

trap 'rm -rf "$staging"' EXIT INT TERM
mkdir -p "$staging"
cp -a /provider/. "$staging/"
test -f "$staging/manifest.json"
test -x "$staging/bin/prepare"
test -x "$staging/bin/serve"
grep -Fq "\"provider\":\"$expected_provider\"" "$staging/manifest.json" || {
  echo "Web IDE provider image does not match $expected_provider" >&2
  exit 1
}
mv "$staging" "$output/current"
trap - EXIT INT TERM
