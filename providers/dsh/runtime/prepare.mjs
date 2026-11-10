// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import {mkdir, readFile, rename, rm, writeFile} from 'node:fs/promises';
import {spawn} from 'node:child_process';
import path from 'node:path';

const providerOwnedEntries = new Set(['codespace', 'connection', 'web-runtime', 'web-startup', 'webserver']);
const root = requiredEnv('CODESPACE_WEB_IDE_PROGRAM_DIR');
const home = requiredEnv('CODESPACE_WEB_IDE_DATA_DIR');
const request = JSON.parse(await readStdin());
if (request.protocolVersion !== 1) throw new Error(`unsupported Web IDE adapter protocol ${request.protocolVersion}`);
if (!request.initialize) process.exit(0);
const marker = path.join(home, '.codespace-initialized');
try {
  await readFile(marker);
  process.exit(0);
} catch (error) {
  if (error.code !== 'ENOENT') throw error;
}

const template = dshOptions(request.templateOptions);
const repository = dshOptions(request.repositoryOptions);
const plugins = [...new Set([...template.plugins, ...repository.plugins])];
const settings = new Map([...template.settings, ...repository.settings]);
let profileEntries = [...settings]
  .sort(([left], [right]) => left < right ? -1 : left > right ? 1 : 0)
  .map(([id, config]) => ({id, config}));

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
  const profilePath = path.join(stagedProfile, 'cordis.patch.yml');
  await writeFile(profilePath, `${JSON.stringify(profileEntries, null, 2)}\n`, {mode: 0o600});
  if (settings.size > 0) {
    const acceptedSettings = await validateProfileSettings(executable, environment, settings);
    if (acceptedSettings.size !== settings.size) {
      profileEntries = profileEntries.filter(({id}) => acceptedSettings.has(id));
      await writeFile(profilePath, `${JSON.stringify(profileEntries, null, 2)}\n`, {mode: 0o600});
    }
  }
  await mkdir(path.join(home, 'profiles'), {recursive: true});
  const profile = path.join(home, 'profiles/web');
  await rm(profile, {recursive: true, force: true});
  await rename(stagedProfile, profile);
  await writeFile(marker, '1\n', {mode: 0o600});
} finally {
  await rm(staging, {recursive: true, force: true});
}

function dshOptions(value) {
  if (value === undefined || value === null) return {plugins: [], settings: []};
  if (!isObject(value)) throw new Error('DeepSeek Harness Web IDE options must be an object');
  const unknown = Object.keys(value).find((key) => key !== 'plugins' && key !== 'settings');
  if (unknown) throw new Error(`DeepSeek Harness option ${JSON.stringify(unknown)} is not supported`);
  const plugins = value.plugins ?? [];
  const settings = value.settings ?? {};
  if (!Array.isArray(plugins) || plugins.some((item) => typeof item !== 'string' || item.trim() === '')) {
    throw new Error('DeepSeek Harness plugins must be an array of non-empty strings');
  }
  if (plugins.length > 128) throw new Error('DeepSeek Harness plugins cannot contain more than 128 entries');
  if (!isObject(settings)) throw new Error('DeepSeek Harness settings must be an object');
  const entries = Object.entries(settings);
  if (entries.length > 128) throw new Error('DeepSeek Harness settings cannot contain more than 128 entries');
  for (const [id, config] of entries) {
    if (id.trim() === '' || id.trim() !== id) throw new Error('DeepSeek Harness setting IDs must be non-empty and cannot have surrounding whitespace');
    if (!isObject(config)) throw new Error(`DeepSeek Harness setting ${JSON.stringify(id)} must be an object`);
    if (providerOwnedEntries.has(id)) throw new Error(`DeepSeek Harness setting ${JSON.stringify(id)} is managed by the Web IDE provider`);
  }
  return {plugins: plugins.map((item) => item.trim()), settings: entries};
}

async function validateProfileSettings(executable, environment, settings) {
  const result = await capture(executable, ['--profile', 'web', '--dump-config-schema'], environment);
  if (result.signal) throw new Error(`DeepSeek Harness profile inspection exited with ${result.signal}`);
  if (result.code !== 0 && result.code !== 1) {
    throw new Error(`DeepSeek Harness profile inspection exited with ${result.code}${result.stderr.trim() ? `: ${result.stderr.trim()}` : ''}`);
  }
  let document;
  try {
    document = JSON.parse(result.stdout);
  } catch {
    throw new Error(`DeepSeek Harness could not inspect the generated profile${result.stderr.trim() ? `: ${result.stderr.trim()}` : ''}`);
  }
  const metadata = document['x-cordis'];
  if (!isObject(metadata) || !Array.isArray(metadata.entries) || !Array.isArray(metadata.diagnostics)) {
    throw new Error('DeepSeek Harness returned an invalid profile schema');
  }
  const accepted = new Set();
  for (const id of settings.keys()) {
    const unmatched = metadata.diagnostics.find((diagnostic) =>
      isObject(diagnostic) && diagnostic.message === `patch: entry ${JSON.stringify(id)} not found`);
    const entry = metadata.entries.find((candidate) =>
      isObject(candidate) && candidate.id === id && typeof candidate.path === 'string' && /^\/\d+$/.test(candidate.path));
    if (unmatched || !entry) {
      process.stderr.write(`warning: DeepSeek Harness setting ${JSON.stringify(id)} was skipped because the profile has no matching top-level entry\n`);
      continue;
    }
    const diagnostic = metadata.diagnostics.find((candidate) =>
      isObject(candidate) && candidate.level === 'error' && typeof candidate.path === 'string' &&
      (candidate.path === entry.path || candidate.path.startsWith(`${entry.path}/`)));
    if (entry.status === 'error' || diagnostic) {
      throw new Error(`DeepSeek Harness setting ${JSON.stringify(id)} is invalid${diagnostic?.message ? `: ${diagnostic.message}` : ''}`);
    }
    accepted.add(id);
  }
  return accepted;
}

function run(command, args, env, showOutput = true) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {env, stdio: ['ignore', showOutput ? 'inherit' : 'ignore', 'inherit']});
    child.once('error', reject);
    child.once('exit', (code, signal) => code === 0 ? resolve() : reject(new Error(`${command} exited with ${signal ?? code}`)));
  });
}

function capture(command, args, env) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {env, stdio: ['ignore', 'pipe', 'pipe']});
    const stdout = [];
    const stderr = [];
    child.stdout.on('data', (chunk) => stdout.push(chunk));
    child.stderr.on('data', (chunk) => stderr.push(chunk));
    child.once('error', reject);
    child.once('exit', (code, signal) => resolve({
      code,
      signal,
      stdout: Buffer.concat(stdout).toString('utf8'),
      stderr: Buffer.concat(stderr).toString('utf8'),
    }));
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
