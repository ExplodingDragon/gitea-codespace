// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import assert from 'node:assert/strict';
import {mkdtemp, mkdir, readFile, writeFile} from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {spawn} from 'node:child_process';
import test from 'node:test';

test('DeepSeek Harness adapter appends repository plugins and patches', async () => {
  const directory = await mkdtemp(path.join(os.tmpdir(), 'codespace-dsh-adapter-'));
  const root = path.join(directory, 'program');
  const data = path.join(directory, 'data');
  const log = path.join(directory, 'commands');
  await mkdir(path.join(root, 'program/bin'), {recursive: true});
  await writeFile(
    path.join(root, 'program/bin/dsh'),
    '#!/bin/sh\nprintf \'%s\\n\' "$*" >>"$CODESPACE_TEST_LOG"\nmkdir -p "$DSH_HOME/profiles/web"\n',
    {mode: 0o755},
  );
  await runAdapter(
    path.join(import.meta.dirname, 'runtime/prepare.mjs'),
    {
      protocolVersion: 1,
      initialize: true,
      templateOptions: {plugins: ['template.plugin'], patches: [{id: 'template'}]},
      repositoryOptions: {plugins: ['repository.plugin'], patches: [{id: 'repository'}]},
    },
    root,
    data,
    log,
  );
  assert.deepEqual(JSON.parse(await readFile(path.join(data, 'profiles/web/cordis.patch.yml'), 'utf8')), [
    {id: 'template'},
    {id: 'repository'},
  ]);
  assert.match(await readFile(log, 'utf8'), /plugin --profile web add -- template\.plugin repository\.plugin/);
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
