#!/bin/sh
# Copyright 2026 The Gitea Authors. All rights reserved.
# SPDX-License-Identifier: MIT

set -eu

os=${1:-}
arch=${2:-}
output=${3:-}
source_root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$source_root/version.env"
version=${CODE_SERVER_VERSION:-$CODE_SERVER_DEFAULT_VERSION}

fail() {
  echo "VS Code provider build: $*" >&2
  exit 1
}

[ "$os" = linux ] || fail "unsupported operating system: $os"
case $arch in amd64 | arm64) ;; *) fail "unsupported architecture: $arch" ;; esac
[ -n "$output" ] || fail "output directory is required"
case $version in "" | [!0-9]* | *[!0-9.]*) fail "invalid code-server version: $version" ;; esac
command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v tar >/dev/null 2>&1 || fail "tar is required"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM
asset="code-server-$version-$os-$arch.tar.gz"
curl -fsSL --proto '=https' --proto-redir '=https' --retry 3 --retry-all-errors \
  -o "$work/archive.tar.gz" "https://github.com/coder/code-server/releases/download/v$version/$asset" || fail "cannot download $asset"
root=$(tar -tzf "$work/archive.tar.gz" | sed -n '1{s|/.*||;p;}')
[ "$root" = "code-server-$version-$os-$arch" ] || fail "$asset has an unexpected top-level directory"

rm -rf "$output"
mkdir -p "$output/program" "$output/bin" "$output/lib" "$output/assets"
tar -xzf "$work/archive.tar.gz" --strip-components=1 -C "$output/program"
[ -x "$output/program/bin/code-server" ] || fail "$asset does not contain executable bin/code-server"
cp "$source_root/runtime/prepare" "$output/bin/prepare"
cp "$source_root/runtime/serve" "$output/bin/serve"
cp "$source_root/runtime/prepare.mjs" "$output/lib/prepare.mjs"
cp "$source_root/extension/dist/gitea-codespace.vsix" "$output/assets/gitea-codespace.vsix"
node -p "require(process.argv[1]).version" "$source_root/extension/package.json" >"$output/assets/gitea-codespace.version"
chmod 0555 "$output/bin/prepare" "$output/bin/serve"
printf '{"schemaVersion":1,"provider":"vscode","architecture":"%s","version":"%s"}\n' "$arch" "$version" >"$output/manifest.json"
