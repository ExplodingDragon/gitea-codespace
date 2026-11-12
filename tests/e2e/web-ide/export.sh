#!/bin/sh
# Copyright 2026 The Gitea Authors. All rights reserved.
# SPDX-License-Identifier: MIT

set -eu

output=${CODESPACE_WEB_IDE_OUTPUT:-/output}
expected=${CODESPACE_WEB_IDE_EXPECTED_PROVIDER:-}
staging="$output/.provider-$$"

test "$expected" = e2e
test ! -e "$output/current"
trap 'rm -rf "$staging"' EXIT INT TERM
mkdir -p "$staging"
cp -a /provider/. "$staging/"
test -f "$staging/manifest.json"
test -x "$staging/bin/prepare"
test -x "$staging/bin/serve"
mv "$staging" "$output/current"
trap - EXIT INT TERM
