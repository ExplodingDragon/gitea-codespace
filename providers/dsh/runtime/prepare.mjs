// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import {mkdir, readFile, rename, rm, writeFile} from 'node:fs/promises';
import {spawn} from 'node:child_process';
import path from 'node:path';

const root = requiredEnv('CODESPACE_WEB_IDE_PROGRAM_DIR');
const home = requiredEnv('CODESPACE_WEB_IDE_DATA_DIR');
const request = JSON.parse(await readStdin());
if (request.protocolVersion !== 1) throw new Error(`unsupported Web IDE adapter protocol ${request.protocolVersion}`);
if (!request.initialize) process.exit(0);
const template = dshOptions(request.templateOptions);
const repository = dshOptions(request.repositoryOptions);
const plugins = [...new Set([...template.plugins, ...repository.plugins])];
const patches = [...template.patches, ...repository.patches];
const marker = path.join(home, '.codespace-initialized');
try {
  await readFile(marker);
  process.exit(0);
} catch (error) {
  if (error.code !== 'ENOENT') throw error;
}

await mkdir(home, {recursive: true});
const staging = path.join(home, `.codespace-initialize-${process.pid}`);
await rm(staging, {recursive: true, force: true});
await mkdir(staging, {recursive: true});
const executable = path.join(root, 'program/bin/dsh');
const environment = {...process.env, DSH_HOME: staging};
try {
  await run(executable, ['--profile', 'web', '--dump-config'], environment, false);
  if (plugins.length > 0) await run(executable, ['plugin', '--profile', 'web', 'add', '--', ...plugins], environment);
  const stagedProfile = path.join(staging, 'profiles/web');
  await writeFile(path.join(stagedProfile, 'cordis.patch.yml'), `${JSON.stringify(patches, null, 2)}\n`, {mode: 0o600});
  await mkdir(path.join(home, 'profiles'), {recursive: true});
  const profile = path.join(home, 'profiles/web');
  await rm(profile, {recursive: true, force: true});
  await rename(stagedProfile, profile);
  await writeFile(marker, '1\n', {mode: 0o600});
} finally {
  await rm(staging, {recursive: true, force: true});
}

function dshOptions(value) {
  if (value === undefined || value === null) return {plugins: [], patches: []};
  if (!isObject(value)) throw new Error('DeepSeek Harness Web IDE options must be an object');
  const plugins = value.plugins ?? [];
  const patches = value.patches ?? [];
  if (!Array.isArray(plugins) || plugins.some((item) => typeof item !== 'string' || item.trim() === '')) {
    throw new Error('DeepSeek Harness plugins must be an array of non-empty strings');
  }
  if (!Array.isArray(patches)) throw new Error('DeepSeek Harness patches must be an array');
  return {plugins: plugins.map((item) => item.trim()), patches};
}

function run(command, args, env, showOutput = true) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {env, stdio: ['ignore', showOutput ? 'inherit' : 'ignore', 'inherit']});
    child.once('error', reject);
    child.once('exit', (code, signal) => code === 0 ? resolve() : reject(new Error(`${command} exited with ${signal ?? code}`)));
  });
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
