#!/bin/sh
# Copyright 2026 The Gitea Authors. All rights reserved.
# SPDX-License-Identifier: MIT

set -eu

output=${CODESPACE_WEB_IDE_OUTPUT:-/output}
staging="$output/.provider-$$"

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
mv "$staging" "$output/current"
trap - EXIT INT TERM
