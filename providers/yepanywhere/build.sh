#!/bin/sh
# Copyright 2026 The Gitea Authors. All rights reserved.
# SPDX-License-Identifier: MIT
set -eu
os=${1:-}
arch=${2:-}
output=${3:-}
mode=${4:-bundle}
source_root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$source_root/version.env"
node_version=$(cat "$source_root/.node-version")
fail() {
  echo "Yep Anywhere provider build: $*" >&2
  exit 1
}
[ "$os" = linux ] || fail "unsupported operating system: $os"
case $arch in
amd64)
  node_arch=x64
  node_sha256=$YEP_NODE_SHA256_AMD64
  ;;
arm64)
  node_arch=arm64
  node_sha256=$YEP_NODE_SHA256_ARM64
  ;;
*) fail "unsupported architecture: $arch" ;;
esac
[ -n "$output" ] || fail "output directory is required"
case $mode in
bundle | check) ;;
*) fail "unsupported build mode: $mode" ;;
esac
for command in curl git node pnpm sha256sum tar; do command -v "$command" >/dev/null 2>&1 || fail "$command is required"; done
[ "$(node -p process.arch)" = "$node_arch" ] || fail "build on the target $node_arch architecture"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
git clone --branch "v$YEP_VERSION" --depth 1 https://github.com/kzahel/yepanywhere.git "$work/source"
[ "$(git -C "$work/source" rev-parse HEAD)" = "$YEP_COMMIT" ] || fail "upstream tag does not match the pinned commit"
git -C "$work/source" apply "$source_root/codespace-integration.patch"
node "$source_root/integrate.mjs" "$work/source"
(
  cd "$work/source"
  [ "$(node -p "require('./package.json').packageManager")" = "pnpm@$YEP_PNPM_VERSION" ] || fail "unexpected upstream pnpm version"
  CI=1 pnpm --filter yep-anywhere --filter '@yep-anywhere/server...' --filter '@yep-anywhere/client...' install --frozen-lockfile
  pnpm --filter @yep-anywhere/shared build
  node scripts/biome.cjs check packages/shared/src/codespace*.ts packages/server/src/routes/codespace*.ts packages/server/src/sdk/codespace*.ts \
    packages/client/src/pages/CodespacePage.tsx packages/client/src/pages/CodespacePage.module.css \
    packages/server/test/codespace packages/client/src/pages/__tests__/Codespace*.test.tsx
  pnpm --filter @yep-anywhere/server exec vitest run test/codespace
  pnpm --filter @yep-anywhere/client exec vitest run src/pages/__tests__/Codespace
  pnpm run build:bundle
  # deploy uses the upstream lockfile, including its dependency patches.
  pnpm --filter @yep-anywhere/server deploy --prod --legacy "$work/deploy"
)
[ "$mode" = bundle ] || output="$work/check"
node_asset="node-v$node_version-linux-$node_arch.tar.xz"
curl -fsSL --proto '=https' --proto-redir '=https' --retry 3 --retry-all-errors \
  -o "$work/$node_asset" "https://nodejs.org/dist/v$node_version/$node_asset"
printf '%s  %s\n' "$node_sha256" "$work/$node_asset" | sha256sum -c - >/dev/null
rm -rf "$output"
mkdir -p "$output/program/node" "$output/program/app" "$output/program/tools" "$output/bin" "$output/lib" "$output/licenses"
tar -xJf "$work/$node_asset" --strip-components=1 -C "$output/program/node"
cp -R "$work/source/dist/npm-package/." "$output/program/app/"
cp -R "$work/deploy/node_modules" "$output/program/app/node_modules"
# pnpm's workspace self-reference must point to the packaged app, not the checkout.
ln -sfn ../../../.. "$output/program/app/node_modules/.pnpm/node_modules/@yep-anywhere/server"
cp "$work/source/LICENSE" "$output/licenses/Yep-Anywhere-LICENSE"
cp "$source_root/tools/licenses/"* "$output/licenses/"
cp "$source_root/tools/package.json" "$source_root/tools/pnpm-lock.yaml" "$output/program/tools/"
CI=1 pnpm --dir "$output/program/tools" install --prod --frozen-lockfile --ignore-scripts
node "$source_root/tools/bundle.mjs" "$output/program/tools"
cp "$source_root/runtime/prepare" "$source_root/runtime/serve" "$output/bin/"
cp "$source_root/runtime/prepare.mjs" "$source_root/runtime/launch.mjs" "$output/lib/"
chmod 0555 "$output/bin/prepare" "$output/bin/serve"
printf '{"schemaVersion":1,"provider":"yepanywhere","architecture":"%s","version":"%s"}\n' "$arch" "$YEP_VERSION" >"$output/manifest.json"
chmod -R a+rX "$output"
"$output/program/node/bin/node" "$output/program/app/dist/cli.js" --version
"$output/program/node/bin/node" "$source_root/tools/smoke.mjs" "$output"
"$output/program/node/bin/node" "$source_root/tools/connections-smoke.mjs" "$output"
