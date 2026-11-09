#!/bin/sh
# Copyright 2026 The Gitea Authors. All rights reserved.
# SPDX-License-Identifier: MIT

set -eu

os=${1:-}
arch=${2:-}
output=${3:-}
source_root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$source_root/version.env"
dsh_version=${DSH_VERSION:-$DSH_DEFAULT_VERSION}
node_version=${DSH_NODE_VERSION:-$(cat "$source_root/.node-version")}
pnpm_version=${DSH_PNPM_VERSION:-$DSH_PNPM_DEFAULT_VERSION}

fail() {
  echo "DeepSeek Harness provider build: $*" >&2
  exit 1
}

[ "$os" = linux ] || fail "unsupported operating system: $os"
case $arch in
amd64)
  node_arch=x64
  node_sha256=46da9a098973ab7ba4fca76945581ecb2eaf468de347173897044382f10e0a0a
  ;;
arm64)
  node_arch=arm64
  node_sha256=33a6673b2c7bffeae9deec7f9f8b31aad9119b08f13d49b2ca3ee3bebfe8260f
  ;;
*) fail "unsupported architecture: $arch" ;;
esac
[ -n "$output" ] || fail "output directory is required"
[ -n "$dsh_version" ] || fail "DSH_VERSION is required"
[ -n "$node_version" ] || fail "DSH_NODE_VERSION is required"
[ -n "$pnpm_version" ] || fail "DSH_PNPM_VERSION is required"
for command in curl git node npm pnpm sha256sum tar; do command -v "$command" >/dev/null 2>&1 || fail "$command is required"; done
[ "$(node -p process.arch)" = "$node_arch" ] || fail "DeepSeek Harness must be built on the target $node_arch architecture"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM
dsh_ref="dsh-v$dsh_version"
dsh_commit=66491b05e3f05ad35bcdd4f6cd7ed26041d9e3db
git clone --branch "$dsh_ref" --depth 1 https://github.com/deepseek-ai/DeepSeek-Harness.git "$work/source" || fail "cannot clone DeepSeek Harness $dsh_ref"
[ "$(git -C "$work/source" rev-parse HEAD)" = "$dsh_commit" ] || fail "DeepSeek Harness tag does not match the pinned commit"
git -C "$work/source" apply --check "$source_root/codespace-integration.patch" || fail "DeepSeek Harness Codespace integration patch no longer applies"
git -C "$work/source" apply "$source_root/codespace-integration.patch"
(
  cd "$work/source"
  CI=1 pnpm install --frozen-lockfile
  [ "$(node -p "require('./package.json').packageManager")" = "pnpm@$pnpm_version" ] || fail "DeepSeek Harness requires a different pnpm version"
  pnpm exec vitest run packages/bundle/web-app/tests/startup.spec.ts packages/bundle/web-app/tests/web-app.spec.ts packages/client/connection/tests/browser-auth.host.spec.ts
  pnpm exec tsx scripts/build.ts --profile official --serial
  pnpm exec tsx scripts/release/pack.ts --family dsh --out "$work/pack" --concurrency 8
  pnpm exec tsx scripts/release/pack.ts --family vendor --out "$work/vendor" --concurrency 8
  pnpm --dir native/system run build:ts
  mkdir -p "$work/landlock"
  pnpm --dir native/system/packages/entry pack --pack-destination "$work/landlock"
)
node "$source_root/install-packed-dsh.mjs" "$work/app" "$work/pack" "$work/vendor" "$work/landlock" || fail "cannot install packed DeepSeek Harness"

node_asset="node-v$node_version-linux-$node_arch.tar.xz"
curl -fsSL --proto '=https' --proto-redir '=https' --retry 3 --retry-all-errors \
  -o "$work/$node_asset" "https://nodejs.org/dist/v$node_version/$node_asset" || fail "cannot download $node_asset"
printf '%s  %s\n' "$node_sha256" "$work/$node_asset" | sha256sum -c - >/dev/null || fail "$node_asset checksum does not match"

rm -rf "$output"
mkdir -p "$output/program/node" "$output/program/app" "$output/program/pnpm" "$output/program/bin" "$output/bin" "$output/lib" "$output/licenses"
tar -xJf "$work/$node_asset" --strip-components=1 -C "$output/program/node"
cp -R "$work/app/." "$output/program/app/"
cp -RL "$work/source/node_modules/pnpm/." "$output/program/pnpm/"
cp "$work/source/LICENSE" "$output/licenses/DeepSeek-Harness-LICENSE"
cat >"$output/program/bin/dsh" <<'EOF'
#!/bin/sh
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
PATH="$root/bin:$PATH"
export PATH
exec "$root/node/bin/node" "$root/app/node_modules/@deepseek-ai/dsh/lib/bin.js" "$@"
EOF
cat >"$output/program/bin/pnpm" <<'EOF'
#!/bin/sh
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
exec "$root/node/bin/node" "$root/pnpm/bin/pnpm.mjs" "$@"
EOF
cp "$source_root/runtime/prepare" "$output/bin/prepare"
cp "$source_root/runtime/serve" "$output/bin/serve"
cp "$source_root/runtime/prepare.mjs" "$output/lib/prepare.mjs"
chmod 0555 "$output/program/bin/dsh" "$output/program/bin/pnpm" "$output/bin/prepare" "$output/bin/serve"
help=$(DSH_HOME="$work/home" "$output/program/bin/dsh" web --help)
printf '%s\n' "$help" | grep -F -- '--trusted-proxy' >/dev/null || fail "trusted-proxy option is unavailable"
printf '%s\n' "$help" | grep -F -- '--workspace' >/dev/null || fail "workspace option is unavailable"
printf '{"schemaVersion":1,"provider":"dsh","architecture":"%s","version":"%s"}\n' "$arch" "$dsh_version" >"$output/manifest.json"
