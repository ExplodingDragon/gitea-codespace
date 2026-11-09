// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import {mkdir, readFile, rename, writeFile} from 'node:fs/promises';
import {spawn} from 'node:child_process';
import path from 'node:path';

const root = requiredEnv('CODESPACE_WEB_IDE_PROGRAM_DIR');
const data = requiredEnv('CODESPACE_WEB_IDE_DATA_DIR');
const request = JSON.parse(await readStdin());
if (request.protocolVersion !== 1) throw new Error(`unsupported Web IDE adapter protocol ${request.protocolVersion}`);
const template = vscodeOptions(request.templateOptions);
const repository = vscodeOptions(request.repositoryOptions);
const settings = {...template.settings, ...repository.settings};
const extensions = mergeExtensions(template.extensions, repository.extensions);
const executable = path.join(root, 'program/bin/code-server');
const userData = path.join(data, 'user-data');
const extensionDir = path.join(data, 'extensions');
await mkdir(path.join(userData, 'User'), {recursive: true});
await mkdir(extensionDir, {recursive: true});

if (request.initialize) {
  const settingsPath = path.join(userData, 'User/settings.json');
  try {
    await readFile(settingsPath);
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
    await atomicWrite(settingsPath, `${JSON.stringify(settings, null, 2)}\n`);
  }
  for (const extension of extensions) {
    await run(executable, ['--user-data-dir', userData, '--extensions-dir', extensionDir, '--install-extension', extension]);
  }
}

const bundledVersion = (await readFile(path.join(root, 'assets/gitea-codespace.version'), 'utf8')).trim();
const versionMarker = path.join(data, '.gitea-codespace-vsix-version');
let installedVersion = '';
try {
  installedVersion = (await readFile(versionMarker, 'utf8')).trim();
} catch (error) {
  if (error.code !== 'ENOENT') throw error;
}
if (installedVersion !== bundledVersion) {
  await run(executable, [
    '--user-data-dir', userData,
    '--extensions-dir', extensionDir,
    '--force',
    '--install-extension', path.join(root, 'assets/gitea-codespace.vsix'),
  ]);
  await atomicWrite(versionMarker, `${bundledVersion}\n`);
}

function vscodeOptions(value) {
  if (value === undefined || value === null) return {settings: {}, extensions: []};
  if (!isObject(value)) throw new Error('VS Code Web IDE options must be an object');
  const settings = value.settings ?? {};
  const extensions = value.extensions ?? [];
  if (!isObject(settings)) throw new Error('VS Code settings must be an object');
  if (!Array.isArray(extensions) || extensions.some((item) => typeof item !== 'string' || item.trim() === '')) {
    throw new Error('VS Code extensions must be an array of non-empty strings');
  }
  return {settings, extensions: extensions.map((item) => item.trim())};
}

function mergeExtensions(template, repository) {
  const result = [];
  const positions = new Map();
  for (const extension of [...template, ...repository]) {
    const identity = extension.split('@', 1)[0].toLowerCase();
    if (positions.has(identity)) result[positions.get(identity)] = extension;
    else {
      positions.set(identity, result.length);
      result.push(extension);
    }
  }
  return result;
}

function run(command, args) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {stdio: ['ignore', 'inherit', 'inherit']});
    child.once('error', reject);
    child.once('exit', (code, signal) => code === 0 ? resolve() : reject(new Error(`${command} exited with ${signal ?? code}`)));
  });
}

async function atomicWrite(target, content) {
  const temporary = `${target}.codespace-${process.pid}`;
  await writeFile(temporary, content, {mode: 0o600});
  await rename(temporary, target);
}

async function readStdin() {
  const chunks = [];
  let size = 0;
  for await (const chunk of process.stdin) {
    size += chunk.length;
    if (size > 1024 * 1024) throw new Error('Web IDE options exceed 1 MiB');
    chunks.push(chunk);
  }
  return Buffer.concat(chunks).toString('utf8');
}

function requiredEnv(name) {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
}

function isObject(value) {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
