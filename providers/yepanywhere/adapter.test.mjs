// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {once} from 'node:events';
import {mkdtemp, mkdir, readFile, rm, symlink, writeFile} from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {createInterface} from 'node:readline';
import {fileURLToPath} from 'node:url';
import test from 'node:test';

const runtime = fileURLToPath(new URL('runtime/', import.meta.url));

async function fixture(t) {
  const root = await mkdtemp(path.join(os.tmpdir(), 'yep-provider-test-'));
  t.after(() => rm(root, {recursive: true, force: true}));
  const app = path.join(root, 'program/app');
  for (const directory of ['program/node/bin', 'data', 'workspace', 'lib', 'program/app/dist/storage', 'program/app/dist/services', 'program/app/dist/metadata', 'program/app/dist/projects']) {
    await mkdir(path.join(root, directory), {recursive: true});
  }
  await writeFile(path.join(app, 'package.json'), '{"type":"module"}');
  await symlink(process.execPath, path.join(root, 'program/node/bin/node'));
  const env = {...process.env, CODESPACE_WEB_IDE_PROGRAM_DIR: root, CODESPACE_WEB_IDE_DATA_DIR: path.join(root, 'data'),
    CODESPACE_WEB_IDE_WORKSPACE: path.join(root, 'workspace'), CODESPACE_WEB_IDE_ADDRESS: '127.0.0.1:13337'};
  return {root, app, env};
}

function run(file, env, request) {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [file], {env, stdio: ['pipe', 'pipe', 'pipe']});
    const output = [];
    child.stdout.on('data', (chunk) => output.push(chunk));
    child.stderr.on('data', (chunk) => output.push(chunk));
    child.once('error', reject);
    child.once('exit', (code) => resolve({code, output: Buffer.concat(output).toString('utf8')}));
    child.stdin.end(JSON.stringify(request));
  });
}

test('prepare initializes options once and registers the canonical workspace on resume', async (t) => {
  const {app, env} = await fixture(t);
  await writeFile(path.join(app, 'dist/storage/discovery-sqlite.js'), 'export function openDiscoveryDatabase() { return {close() {}}; }');
  await writeFile(path.join(app, 'dist/services/ServerSettingsService.js'), `
    import {writeFile} from 'node:fs/promises';
    export class ServerSettingsService {
      constructor({dataDir}) { this.path = dataDir + '/settings.json'; }
      async initialize() {}
      async updateSettings(value) { await writeFile(this.path, JSON.stringify(value)); }
    }
  `);
  await writeFile(path.join(app, 'dist/metadata/ProjectMetadataService.js'), `
    import {readFile, writeFile} from 'node:fs/promises';
    export class ProjectMetadataService {
      constructor({dataDir}) { this.path = dataDir + '/project.json'; this.projects = {}; }
      async initialize() { try { this.projects = JSON.parse(await readFile(this.path)); } catch (e) { if (e.code !== 'ENOENT') throw e; } }
      getAllProjects() { return this.projects; }
      async addProject(id, path) { this.projects[id] = {path}; await writeFile(this.path, JSON.stringify(this.projects)); }
    }
  `);
  await writeFile(path.join(app, 'dist/projects/paths.js'), 'export const encodeProjectId = value => value; export const canonicalizeProjectPath = value => value;');
  const request = {protocolVersion: 1, initialize: true, templateOptions: {providers: ['claude'], settings: {publicSharesEnabled: false}}, repositoryOptions: {providers: ['codex'], settings: {publicSharesEnabled: true}}};
  const first = await run(path.join(runtime, 'prepare.mjs'), env, request);
  assert.equal(first.code, 0, first.output);
  const data = env.CODESPACE_WEB_IDE_DATA_DIR;
  assert.deepEqual(JSON.parse(await readFile(path.join(data, 'codespace-options.json'))), {providers: ['codex']});
  assert.deepEqual(JSON.parse(await readFile(path.join(data, 'settings.json'))), {publicSharesEnabled: true, codexUpdatePolicy: 'off'});
  await writeFile(path.join(data, 'settings.json'), '{"userChanged":true}');
  const second = await run(path.join(runtime, 'prepare.mjs'), env, {...request, initialize: false});
  assert.equal(second.code, 0, second.output);
  assert.equal(await readFile(path.join(data, 'settings.json'), 'utf8'), '{"userChanged":true}');
  assert.equal(Object.keys(JSON.parse(await readFile(path.join(data, 'project.json')))).length, 1);
});

test('serve inherits user tools and secrets, restarts successful exits, and stops on termination', {timeout: 10_000}, async (t) => {
  const {root, app, env} = await fixture(t);
  await writeFile(path.join(root, 'data/codespace-options.json'), '{"providers":["codex"]}');
  await writeFile(path.join(app, 'dist/index.js'), `
    import {writeFile} from 'node:fs/promises';
    await writeFile(process.env.CODESPACE_WEB_IDE_DATA_DIR + '/launch.json', JSON.stringify(Object.fromEntries(
      ['HOME','PATH','TEST_SECRET','HTTPS_PROXY','NODE_EXTRA_CA_CERTS','NODE_ENV','HOST','PORT','AUTH_DISABLED','ENABLED_PROVIDERS'].map(key => [key,process.env[key]])
    )));
    console.log('started');
  `);
  Object.assign(env, {TEST_SECRET: 'line one\nline two', HTTPS_PROXY: 'http://proxy.invalid:8080', HOME: path.join(root, 'home'), HOST: '0.0.0.0', PORT: '9999', AUTH_DISABLED: 'false'});
  await symlink(path.join(runtime, 'launch.mjs'), path.join(root, 'lib/launch.mjs'));
  const supervisor = spawn(path.join(runtime, 'serve'), [], {env, stdio: ['ignore', 'pipe', 'inherit']});
  const exited = once(supervisor, 'exit');
  t.after(async () => {
    supervisor.kill('SIGTERM');
    await exited;
  });
  let starts = 0;
  for await (const line of createInterface({input: supervisor.stdout})) {
    if (line === 'started' && ++starts === 2) break;
  }
  assert.equal(starts, 2, 'a completed app process must be restarted');
  supervisor.kill('SIGTERM');
  assert.deepEqual(await exited, [0, null]);
  const actual = JSON.parse(await readFile(path.join(root, 'data/launch.json')));
  assert.equal(actual.HOME, env.HOME);
  assert.equal(actual.PATH, env.PATH);
  assert.equal(actual.TEST_SECRET, env.TEST_SECRET);
  assert.equal(actual.HTTPS_PROXY, env.HTTPS_PROXY);
  assert.equal(actual.NODE_ENV, 'production');
  assert.equal(actual.HOST, '127.0.0.1');
  assert.equal(actual.PORT, '13337');
  assert.equal(actual.AUTH_DISABLED, 'true');
  assert.equal(actual.ENABLED_PROVIDERS, 'codex');
});
