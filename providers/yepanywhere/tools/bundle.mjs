// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT
import {chmod, copyFile, mkdir, realpath, symlink} from 'node:fs/promises';
import {createRequire} from 'node:module';
import path from 'node:path';

const tools = path.resolve(process.argv[2]);
const require = createRequire(path.join(tools, 'package.json'));
const bin = path.join(tools, 'bin');
await mkdir(bin, {recursive: true});
for (const name of ['codex', 'pi']) {
  await copyFile(new URL('launch', import.meta.url), path.join(bin, name));
  await chmod(path.join(bin, name), 0o555);
}
// Use the baseline x64 build so the image also works on CPUs without AVX2.
const opencode = createRequire(require.resolve('opencode-ai/package.json'));
const nativePackage = process.arch === 'x64' ? 'opencode-linux-x64-baseline' : 'opencode-linux-arm64';
const native = await realpath(path.join(path.dirname(opencode.resolve(`${nativePackage}/package.json`)), 'bin/opencode'));
await symlink(path.relative(bin, native), path.join(bin, 'opencode'));
