#!/bin/sh
# Copyright 2026 The Gitea Authors. All rights reserved.
# SPDX-License-Identifier: MIT

set -eu

os=${1:-}
arch=${2:-}
output=${3:-}
source_root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$source_root/version.env"
version=$CODE_SERVER_VERSION

fail() {
  echo "VS Code provider build: $*" >&2
  exit 1
}

download() {
  curl -fsSL --proto '=https' --proto-redir '=https' --connect-timeout 30 --max-time 600 \
    --retry 8 --retry-all-errors --retry-max-time 900 \
    -o "$2" "$1" || fail "cannot download $1"
}

[ "$os" = linux ] || fail "unsupported operating system: $os"
case $arch in amd64 | arm64) ;; *) fail "unsupported architecture: $arch" ;; esac
[ -n "$output" ] || fail "output directory is required"
case $version in "" | [!0-9]* | *[!0-9.]*) fail "invalid code-server version: $version" ;; esac
command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v tar >/dev/null 2>&1 || fail "tar is required"
command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is required"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
asset="code-server-$version-$os-$arch.tar.gz"
download "https://github.com/coder/code-server/releases/download/v$version/$asset" "$work/archive.tar.gz"
root=$(tar -tzf "$work/archive.tar.gz" | sed -n '1{s|/.*||;p;}')
[ "$root" = "code-server-$version-$os-$arch" ] || fail "$asset has an unexpected top-level directory"

rm -rf "$output"
mkdir -p "$output/program" "$output/bin" "$output/lib" "$output/assets"
tar -xzf "$work/archive.tar.gz" --strip-components=1 -C "$output/program"
[ -x "$output/program/bin/code-server" ] || fail "$asset does not contain executable bin/code-server"
cp "$source_root/runtime/prepare" "$output/bin/prepare"
cp "$source_root/runtime/serve" "$output/bin/serve"
cp "$source_root/.tmp/prepare.mjs" "$output/lib/prepare.mjs"
cp "$source_root/node_modules/jsonc-parser/LICENSE.md" "$output/assets/jsonc-parser.LICENSE.md"
cp "$source_root/extension/dist/gitea-codespace.vsix" "$output/assets/gitea-codespace.vsix"
for language in cs de es fr it ja ko pl pt-BR ru tr zh-hans zh-hant; do
  extension="MS-CEINTL.vscode-language-pack-$language-$LANGUAGE_PACK_VERSION.vsix"
  download "https://open-vsx.org/api/MS-CEINTL/vscode-language-pack-$language/$LANGUAGE_PACK_VERSION/file/$extension" "$output/assets/$extension"
done
(
  cd "$output/assets"
  sha256sum ./*.vsix
) >"$output/assets/extensions.sha256"
chmod 0555 "$output/bin/prepare" "$output/bin/serve"
printf '{"schemaVersion":1,"provider":"vscode","architecture":"%s","version":"%s"}\n' "$arch" "$version" >"$output/manifest.json"
chmod -R a+rX "$output"
