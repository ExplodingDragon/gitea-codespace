// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import assert from 'node:assert/strict';
import {mkdtemp, mkdir, readFile, rm, writeFile} from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {spawn} from 'node:child_process';
import test from 'node:test';
import {parse} from 'jsonc-parser';

const inheritedGallery = {serviceUrl: 'https://inherited.example/api'};

test('VS Code adapter prepares editor defaults and bundled extensions while preserving user configuration', async (t) => {
  const directory = await mkdtemp(path.join(os.tmpdir(), 'codespace-vscode-adapter-'));
  t.after(() => rm(directory, {recursive: true, force: true}));
  const root = path.join(directory, 'program');
  const data = path.join(directory, 'data');
  const log = path.join(directory, 'commands');
  await mkdir(path.join(root, 'program/bin'), {recursive: true});
  await mkdir(path.join(root, 'assets'), {recursive: true});
  await writeFile(path.join(root, 'program/bin/code-server'), '#!/bin/sh\nprintf \'%s\\n\' "$*" >>"$CODESPACE_TEST_LOG"\nprintf \'%s\\n\' "$EXTENSIONS_GALLERY" >>"$CODESPACE_TEST_LOG.gallery"\n', {mode: 0o755});
  await writeFile(path.join(root, 'assets/gitea-codespace.vsix'), 'extension');
  await writeFile(path.join(root, 'assets/language-pack.vsix'), 'language pack');
  await writeFile(path.join(root, 'assets/extensions.sha256'), 'bundle-version-1\n');
  const languagePack = path.join(data, 'extensions/language-pack');
  await mkdir(languagePack, {recursive: true});
  await writeFile(path.join(data, 'extensions/extensions.json'), JSON.stringify([{identifier: {id: 'example.language-pack'}, location: {path: languagePack}}]));
  await writeFile(path.join(languagePack, 'package.json'), JSON.stringify({version: '1.0.0', contributes: {
    localizations: [{languageId: 'fr', translations: [{id: 'vscode', path: 'translations/main.json'}]}],
  }}));
  const script = path.join(import.meta.dirname, 'runtime/prepare.mjs');
  const templateGallery = {
    serviceUrl: 'https://template.example/api',
    resourceUrlTemplate: 'https://template.example/assets/{publisher}/{name}/{version}/{path}',
  };
  const repositoryGallery = {serviceUrl: 'https://repository.example/api'};
  const galleryPath = path.join(data, 'extensions-gallery.json');
  await runAdapter(
    script,
    {
      protocolVersion: 1,
      initialize: true,
      templateOptions: {settings: {'editor.tabSize': 4}, extensions: ['example.tool@1'], extensionsGallery: templateGallery},
      repositoryOptions: {settings: {'editor.tabSize': 2}, extensions: ['example.tool@2', 'example.other'], extensionsGallery: repositoryGallery},
    },
    root,
    data,
    log,
  );
  const settingsPath = path.join(data, 'user-data/User/settings.json');
  const localePath = path.join(data, 'user-data/User/argv.json');
  assert.deepEqual(JSON.parse(await readFile(settingsPath, 'utf8')), {
    'editor.tabSize': 2,
    'chat.disableAIFeatures': true,
    'telemetry.telemetryLevel': 'off',
  });
  assert.deepEqual(JSON.parse(await readFile(localePath, 'utf8')), {});
  const languagePacks = JSON.parse(await readFile(path.join(data, 'user-data/languagepacks.json'), 'utf8'));
  assert.equal(languagePacks.fr.translations.vscode, path.join(languagePack, 'translations/main.json'));
  const commands = await readFile(log, 'utf8');
  assert.match(commands, /--install-extension example\.tool@2/);
  assert.match(commands, /--install-extension example\.other/);
  assert.match(commands, /--force --install-extension .*gitea-codespace\.vsix/);
  assert.match(commands, /--install-extension .*language-pack\.vsix/);
  for (const command of commands.trim().split('\n')) assert.match(command, /^--disable-telemetry --disable-update-check /);
  assert.deepEqual(JSON.parse(await readFile(galleryPath, 'utf8')), repositoryGallery);
  assert.deepEqual((await readFile(`${log}.gallery`, 'utf8')).trim().split('\n').map((line) => JSON.parse(line)), [repositoryGallery, repositoryGallery]);

  await writeFile(settingsPath, '{\n  // User preferences\n  "editor.tabSize": 8,\n  "chat.disableAIFeatures": false,\n  "telemetry.telemetryLevel": "all",\n}\n');
  await writeFile(localePath, '{"locale":"fr"}\n');
  const resume = {
    protocolVersion: 1, initialize: false,
    templateOptions: {extensionsGallery: templateGallery},
    repositoryOptions: {settings: {'editor.tabSize': 2}},
  };
  await runAdapter(script, resume, root, data, log);
  assert.deepEqual(JSON.parse(await readFile(galleryPath, 'utf8')), templateGallery);
  const resumedSettings = await readFile(settingsPath, 'utf8');
  assert.match(resumedSettings, /\/\/ User preferences/);
  assert.deepEqual(parse(resumedSettings), {
    'editor.tabSize': 8,
    'chat.disableAIFeatures': false,
    'telemetry.telemetryLevel': 'off',
  });
  assert.deepEqual(JSON.parse(await readFile(localePath, 'utf8')), {locale: 'fr'});
  assert.equal(await readFile(log, 'utf8'), commands);

  await writeFile(path.join(root, 'assets/extensions.sha256'), 'bundle-version-2\n');
  await runAdapter(script, {...resume, templateOptions: {}}, root, data, log);
  await assert.rejects(readFile(galleryPath), {code: 'ENOENT'});
  assert.deepEqual(JSON.parse((await readFile(`${log}.gallery`, 'utf8')).trim().split('\n').at(-1)), inheritedGallery);
  assert.equal((await readFile(log, 'utf8')).trim().split('\n').length, 3);
  assert.equal(await readFile(path.join(data, '.bundled-extensions-version'), 'utf8'), 'bundle-version-2\n');
});

test('VS Code restarts a clean exit and stops on termination', async (t) => {
  const directory = await mkdtemp(path.join(os.tmpdir(), 'codespace-vscode-serve-'));
  t.after(() => rm(directory, {recursive: true, force: true}));
  const root = path.join(directory, 'program');
  const data = path.join(directory, 'data');
  const workspace = path.join(directory, 'workspace');
  const log = path.join(directory, 'commands');
  const restartMarker = path.join(directory, 'restart-marker');
  await mkdir(path.join(root, 'program/bin'), {recursive: true});
  await mkdir(data);
  const gallery = {serviceUrl: 'https://extensions.example/api'};
  await writeFile(path.join(data, 'extensions-gallery.json'), `${JSON.stringify(gallery)}\n`);
  await mkdir(workspace);
  await writeFile(
    path.join(root, 'program/bin/code-server'),
    '#!/bin/sh\nprintf \'%s\\n\' "$*" >>"$CODESPACE_TEST_LOG"\nprintf \'%s\\n\' "$EXTENSIONS_GALLERY" >>"$CODESPACE_TEST_LOG.gallery"\nif [ ! -e "$CODESPACE_TEST_RESTART_MARKER" ]; then\n  : >"$CODESPACE_TEST_RESTART_MARKER"\n  exit 0\nfi\nkill -TERM "$PPID"\nexec sleep 5\n',
    {mode: 0o755},
  );

  await new Promise((resolve, reject) => {
    const child = spawn(path.join(import.meta.dirname, 'runtime/serve'), {
      env: {
        ...process.env,
        EXTENSIONS_GALLERY: JSON.stringify(inheritedGallery),
        CODESPACE_WEB_IDE_PROGRAM_DIR: root,
        CODESPACE_WEB_IDE_DATA_DIR: data,
        CODESPACE_WEB_IDE_ADDRESS: '127.0.0.1:3080',
        CODESPACE_WEB_IDE_WORKSPACE: workspace,
        CODESPACE_TEST_LOG: log,
        CODESPACE_TEST_RESTART_MARKER: restartMarker,
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

  const commands = await readFile(log, 'utf8');
  assert.equal(commands.trim().split('\n').length, 2);
  assert.match(commands, /--bind-addr 127\.0\.0\.1:3080/);
  assert.match(commands, /--disable-workspace-trust --disable-telemetry --disable-update-check/);
  assert.match(commands, new RegExp(`${workspace.replaceAll(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`, 'm'));
  assert.deepEqual((await readFile(`${log}.gallery`, 'utf8')).trim().split('\n').map((line) => JSON.parse(line)), [gallery, gallery]);
});

async function runAdapter(script, request, root, data, log) {
  await new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [script], {
      env: {...process.env, EXTENSIONS_GALLERY: JSON.stringify(inheritedGallery), CODESPACE_WEB_IDE_PROGRAM_DIR: root, CODESPACE_WEB_IDE_DATA_DIR: data, CODESPACE_TEST_LOG: log},
      stdio: ['pipe', 'inherit', 'inherit'],
    });
    child.once('error', reject);
    child.once('exit', (code, signal) => code === 0 ? resolve() : reject(new Error(`${script} exited with ${signal ?? code}`)));
    child.stdin.end(JSON.stringify(request));
  });
}
