#!/bin/sh
# Copyright 2026 The Gitea Authors. All rights reserved.
# SPDX-License-Identifier: MIT

set -eu

os=${1:-}
arch=${2:-}
output=${3:-}
download_url=https://dl.gitea.com/gitea-cli

fail() {
  echo "prepare-gitea-cli.sh: $*" >&2
  exit 1
}

download() {
  curl -fsSL --proto '=https' --proto-redir '=https' --retry 3 --retry-all-errors "$@"
}

[ "$os" = linux ] || fail "unsupported operating system: $os"
case $arch in
amd64) elf_machine=3e00 ;;
arm64) elf_machine=b700 ;;
*) fail "unsupported architecture: $arch" ;;
esac
[ -n "$output" ] || fail "output directory is required"
command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v od >/dev/null 2>&1 || fail "od is required"
command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is required"
command -v xz >/dev/null 2>&1 || fail "xz is required"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

version=${GITEA_CLI_VERSION:-}
if [ -z "$version" ]; then
  latest_asset="gitea-cli-latest-$os-$arch.xz"
  resolved_url=$(download -w '%{url_effective}' -o "$work/archive.xz" "$download_url/latest/$latest_asset") || fail "cannot download the latest release"
  resolved_asset=${resolved_url##*/}
  version=${resolved_asset#"gitea-cli-"}
  version=${version%"-$os-$arch.xz"}
else
  version=${version#v}
fi
case $version in
"" | [!A-Za-z0-9]* | *[!A-Za-z0-9._-]*) fail "invalid release version: $version" ;;
esac

asset="gitea-cli-$version-$os-$arch.xz"
if [ ! -f "$work/archive.xz" ]; then
  download -o "$work/archive.xz" "$download_url/$version/$asset" || fail "cannot download $asset"
fi
download -o "$work/$asset.sha256" "$download_url/$version/$asset.sha256" || fail "cannot download the checksum for $asset"
expected=$(awk 'NR == 1 {print $1}' "$work/$asset.sha256")
printf '%s\n' "$expected" | grep -Eq '^[0-9a-f]{64}$' || fail "invalid checksum for $asset"
actual=$(sha256sum "$work/archive.xz" | awk '{print $1}')
[ "$actual" = "$expected" ] || fail "checksum mismatch for $asset"

xz -dc "$work/archive.xz" >"$work/gt"
[ "$(od -An -tx1 -N4 "$work/gt" | tr -d ' \n')" = 7f454c46 ] || fail "$asset is not an ELF binary"
[ "$(od -An -tx1 -j18 -N2 "$work/gt" | tr -d ' \n')" = "$elf_machine" ] || fail "$asset does not match $arch"
chmod 0555 "$work/gt"

host_arch=$(uname -m)
case $host_arch in
x86_64) host_arch=amd64 ;;
aarch64) host_arch=arm64 ;;
esac
if [ "$host_arch" = "$arch" ]; then
  case $("$work/gt" version) in
  *"$version"*) ;;
  *) fail "downloaded binary does not report version $version" ;;
  esac
fi

binary_digest=$(sha256sum "$work/gt" | awk '{print $1}')
mkdir -p "$output"
mv "$work/gt" "$output/gt"
metadata="$output/.gitea-cli.env.$$"
printf 'GITEA_CLI_VERSION=%s\nGITEA_CLI_SHA256=%s\n' "$version" "$binary_digest" >"$metadata"
mv "$metadata" "$output/gitea-cli.env"
printf 'Prepared Gitea CLI %s for %s/%s (%s)\n' "$version" "$os" "$arch" "$binary_digest"
