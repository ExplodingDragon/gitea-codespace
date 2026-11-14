// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {copyFile, mkdir, mkdtemp, rm, writeFile} from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';

test('VS Code build version honors overrides and image labels describe the built bundle', async (t) => {
  const directory = await mkdtemp(path.join(os.tmpdir(), 'codespace-vscode-build-'));
  t.after(() => rm(directory, {recursive: true, force: true}));
  for (const file of ['Makefile', 'version.env']) {
    await copyFile(path.join(import.meta.dirname, file), path.join(directory, file));
  }
  for (const version of ['', '9.9.9']) {
    const options = {cwd: directory, env: {...process.env, CODE_SERVER_VERSION: version}, encoding: 'utf8'};
    const configured = execFileSync('sh', ['-c', '. ./version.env; printf "%s" "$CODE_SERVER_VERSION"'], options);
    const reported = execFileSync('make', ['--no-print-directory', 'version'], options).trim();
    assert.equal(reported, configured);
    if (version) assert.equal(reported, version);
    else assert.match(reported, /^\d+\.\d+\.\d+$/);
  }

  await mkdir(path.join(directory, '.tmp/amd64'), {recursive: true});
  await writeFile(path.join(directory, '.tmp/amd64/manifest.json'), JSON.stringify({version: '8.8.8'}));
  const command = execFileSync('make', ['--no-print-directory', '-o', 'bundle', 'image', 'ARCH=amd64', 'CONTAINER_TOOL=echo'], {
    cwd: directory,
    env: {...process.env, CODE_SERVER_VERSION: '9.9.9'},
    encoding: 'utf8',
  });
  assert.match(command, /--build-arg PROVIDER_VERSION=8\.8\.8(?:\s|$)/);
});
