#!/bin/sh
# Copyright 2026 The Gitea Authors. All rights reserved.
# SPDX-License-Identifier: MIT

set -eu

os=${1:-}
arch=${2:-}
output=${3:-}
version=${CODE_SERVER_VERSION:-}

fail() {
  echo "prepare-web-ide.sh: $*" >&2
  exit 1
}

[ "$os" = linux ] || fail "unsupported operating system: $os"
case $arch in
amd64 | arm64) ;;
*) fail "unsupported architecture: $arch" ;;
esac
[ -n "$output" ] || fail "output directory is required"
case $version in
"") fail "CODE_SERVER_VERSION is required" ;;
[!0-9]* | *[!0-9.]*) fail "invalid code-server version: $version" ;;
esac
command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v tar >/dev/null 2>&1 || fail "tar is required"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

asset="code-server-$version-$os-$arch.tar.gz"
url="https://github.com/coder/code-server/releases/download/v$version/$asset"
curl -fsSL --proto '=https' --proto-redir '=https' --retry 3 --retry-all-errors \
  -o "$work/archive.tar.gz" "$url" || fail "cannot download $asset"

root=$(tar -tzf "$work/archive.tar.gz" | sed -n '1{s|/.*||;p;}')
[ "$root" = "code-server-$version-$os-$arch" ] || fail "$asset has an unexpected top-level directory"
mkdir -p "$work/program"
tar -xzf "$work/archive.tar.gz" --strip-components=1 -C "$work/program"
[ -x "$work/program/bin/code-server" ] || fail "$asset does not contain executable bin/code-server"

mkdir -p "$output"
rm -rf "$output/web-ide"
mv "$work/program" "$output/web-ide"
tar -czf "$output/web-ide-platform.tar.gz" -C "$output" web-ide
metadata="$output/.web-ide.env.$$"
printf 'CODE_SERVER_VERSION=%s\n' "$version" >"$metadata"
mv "$metadata" "$output/web-ide.env"
printf 'Prepared code-server %s for %s/%s\n' "$version" "$os" "$arch"
