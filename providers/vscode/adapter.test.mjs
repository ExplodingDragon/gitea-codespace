// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import assert from 'node:assert/strict';
import {mkdtemp, mkdir, readFile, writeFile} from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {spawn} from 'node:child_process';
import test from 'node:test';

test('VS Code adapter merges repository options and installs the Codespace extension', async () => {
  const directory = await mkdtemp(path.join(os.tmpdir(), 'codespace-vscode-adapter-'));
  const root = path.join(directory, 'program');
  const data = path.join(directory, 'data');
  const log = path.join(directory, 'commands');
  await mkdir(path.join(root, 'program/bin'), {recursive: true});
  await mkdir(path.join(root, 'assets'), {recursive: true});
  await writeFile(path.join(root, 'program/bin/code-server'), '#!/bin/sh\nprintf \'%s\\n\' "$*" >>"$CODESPACE_TEST_LOG"\n', {mode: 0o755});
  await writeFile(path.join(root, 'assets/gitea-codespace.vsix'), 'extension');
  await writeFile(path.join(root, 'assets/gitea-codespace.version'), '1.2.3\n');
  await runAdapter(
    path.join(import.meta.dirname, 'runtime/prepare.mjs'),
    {
      protocolVersion: 1,
      initialize: true,
      templateOptions: {settings: {'editor.tabSize': 4}, extensions: ['example.tool@1']},
      repositoryOptions: {settings: {'editor.tabSize': 2}, extensions: ['example.tool@2', 'example.other']},
    },
    root,
    data,
    log,
  );
  assert.deepEqual(JSON.parse(await readFile(path.join(data, 'user-data/User/settings.json'), 'utf8')), {'editor.tabSize': 2});
  const commands = await readFile(log, 'utf8');
  assert.match(commands, /--install-extension example\.tool@2/);
  assert.match(commands, /--install-extension example\.other/);
  assert.match(commands, /--force --install-extension .*gitea-codespace\.vsix/);
});

async function runAdapter(script, request, root, data, log) {
  await new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [script], {
      env: {...process.env, CODESPACE_WEB_IDE_PROGRAM_DIR: root, CODESPACE_WEB_IDE_DATA_DIR: data, CODESPACE_TEST_LOG: log},
      stdio: ['pipe', 'inherit', 'inherit'],
    });
    child.once('error', reject);
    child.once('exit', (code, signal) => code === 0 ? resolve() : reject(new Error(`${script} exited with ${signal ?? code}`)));
    child.stdin.end(JSON.stringify(request));
  });
}
