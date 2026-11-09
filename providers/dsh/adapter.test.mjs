// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import assert from 'node:assert/strict';
import {mkdtemp, mkdir, readFile, rm, writeFile} from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {spawn} from 'node:child_process';
import test from 'node:test';

test('DeepSeek Harness adapter prepares merged settings and plugins once', async (t) => {
  const schema = JSON.stringify({'x-cordis': {
    complete: true,
    entries: [
      {id: 'shell', path: '/0', status: 'schema'},
      {id: 'ui-theme', path: '/1', status: 'schema'},
    ],
    diagnostics: [],
  }});
  const {root, data, log} = await createFixture(t, schema);
  await runAdapter(
    path.join(import.meta.dirname, 'runtime/prepare.mjs'),
    {
      protocolVersion: 1,
      initialize: true,
      templateOptions: {
        plugins: ['template.plugin'],
        settings: {
          shell: {timeout: 30_000},
          'ui-theme': {preference: 'dark', fontSize: 14},
        },
      },
      repositoryOptions: {
        plugins: ['repository.plugin'],
        settings: {'ui-theme': {preference: 'light', fontSize: 16}},
      },
    },
    root,
    data,
    log,
    schema,
  );
  assert.deepEqual(JSON.parse(await readFile(path.join(data, 'profiles/web/cordis.patch.yml'), 'utf8')), [
    {id: 'shell', config: {timeout: 30_000}},
    {id: 'ui-theme', config: {preference: 'light', fontSize: 16}},
  ]);
  const initialCommands = await readFile(log, 'utf8');
  assert.match(initialCommands, /plugin --profile web add -- template\.plugin repository\.plugin/);
  assert.match(initialCommands, /--profile web --dump-config-schema/);

  await runAdapter(
    path.join(import.meta.dirname, 'runtime/prepare.mjs'),
    {
      protocolVersion: 1,
      initialize: true,
      templateOptions: {settings: {shell: {timeout: 60_000}}},
      repositoryOptions: {},
    },
    root,
    data,
    log,
    schema,
  );
  assert.equal(await readFile(log, 'utf8'), initialCommands);
  assert.deepEqual(JSON.parse(await readFile(path.join(data, 'profiles/web/cordis.patch.yml'), 'utf8')), [
    {id: 'shell', config: {timeout: 30_000}},
    {id: 'ui-theme', config: {preference: 'light', fontSize: 16}},
  ]);
});

test('DeepSeek Harness adapter skips a setting without a profile entry', async (t) => {
  const schema = JSON.stringify({'x-cordis': {
    complete: true,
    entries: [{id: 'shell', path: '/0', status: 'schema'}],
    diagnostics: [{level: 'warning', message: 'patch: entry "missing" not found'}],
  }});
  const {root, data, log} = await createFixture(t, schema);
  const stderr = await runAdapter(
    path.join(import.meta.dirname, 'runtime/prepare.mjs'),
    {
      protocolVersion: 1,
      initialize: true,
      templateOptions: {settings: {shell: {timeout: 30_000}, missing: {enabled: true}}},
      repositoryOptions: {},
    },
    root,
    data,
    log,
    schema,
  );
  assert.match(stderr, /warning: DeepSeek Harness setting "missing" was skipped because the profile has no matching top-level entry/);
  assert.deepEqual(JSON.parse(await readFile(path.join(data, 'profiles/web/cordis.patch.yml'), 'utf8')), [
    {id: 'shell', config: {timeout: 30_000}},
  ]);
  assert.equal(await readFile(path.join(data, '.codespace-initialized'), 'utf8'), '1\n');
});

test('DeepSeek Harness starts in and registers the current workspace', async (t) => {
  const {root, data, log} = await createFixture(t, '');
  const workspace = path.join(path.dirname(root), 'workspace');
  await mkdir(workspace);

  await new Promise((resolve, reject) => {
    const child = spawn(path.join(import.meta.dirname, 'runtime/serve'), {
      env: {
        ...process.env,
        CODESPACE_WEB_IDE_PROGRAM_DIR: root,
        CODESPACE_WEB_IDE_DATA_DIR: data,
        CODESPACE_WEB_IDE_ADDRESS: '127.0.0.1:3080',
        CODESPACE_WEB_IDE_WORKSPACE: workspace,
        CODESPACE_TEST_LOG: log,
      },
      stdio: ['ignore', 'ignore', 'pipe'],
    });
    const stderr = [];
    child.stderr.on('data', (chunk) => stderr.push(chunk));
    child.once('error', reject);
    child.once('exit', (code, signal) => code === 0
      ? resolve()
      : reject(new Error(Buffer.concat(stderr).toString('utf8') || `serve exited with ${signal ?? code}`)));
  });

  assert.equal(
    await readFile(log, 'utf8'),
    `cwd=${workspace} args=web --workspace ${workspace} --trusted-proxy --no-open --host 127.0.0.1 --port 3080\n`,
  );
});

async function createFixture(t, schema) {
  const directory = await mkdtemp(path.join(os.tmpdir(), 'codespace-dsh-adapter-'));
  t.after(() => rm(directory, {recursive: true, force: true}));
  const root = path.join(directory, 'program');
  const data = path.join(directory, 'data');
  const log = path.join(directory, 'commands');
  await mkdir(path.join(root, 'program/bin'), {recursive: true});
  await writeFile(
    path.join(root, 'program/bin/dsh'),
    '#!/bin/sh\nprintf \'cwd=%s args=%s\\n\' "$PWD" "$*" >>"$CODESPACE_TEST_LOG"\nmkdir -p "$DSH_HOME/profiles/web"\ncase "$*" in *--dump-config-schema*) printf \'%s\\n\' "$CODESPACE_TEST_SCHEMA";; esac\n',
    {mode: 0o755},
  );
  return {root, data, log};
}

async function runAdapter(script, request, root, data, log, schema) {
  return new Promise((resolve, reject) => {
    const stderr = [];
    const child = spawn(process.execPath, [script], {
      env: {
        ...process.env,
        CODESPACE_WEB_IDE_PROGRAM_DIR: root,
        CODESPACE_WEB_IDE_DATA_DIR: data,
        CODESPACE_TEST_LOG: log,
        CODESPACE_TEST_SCHEMA: schema,
      },
      stdio: ['pipe', 'inherit', 'pipe'],
    });
    child.stderr.on('data', (chunk) => stderr.push(chunk));
    child.once('error', reject);
    child.once('exit', (code, signal) => code === 0
      ? resolve(Buffer.concat(stderr).toString('utf8'))
      : reject(new Error(Buffer.concat(stderr).toString('utf8') || `${script} exited with ${signal ?? code}`)));
    child.stdin.end(JSON.stringify(request));
  });
}
