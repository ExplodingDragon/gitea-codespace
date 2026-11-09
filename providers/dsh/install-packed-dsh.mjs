// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import {execFileSync, spawnSync} from 'node:child_process';
import {mkdirSync, readdirSync, writeFileSync} from 'node:fs';
import {resolve} from 'node:path';
import {fileURLToPath, pathToFileURL} from 'node:url';

const [output, ...sources] = process.argv.slice(2);
if (!output || sources.length === 0) {
  throw new Error('usage: install-packed-dsh.mjs <output> <tarball-directory>...');
}

function acceptsPlatform(value, current) {
  if (value === undefined) return true;
  if (!Array.isArray(value) || !value.every((entry) => typeof entry === 'string')) {
    throw new Error('npm platform constraints must be arrays of strings');
  }
  let accepted = value.every((entry) => entry.startsWith('!'));
  for (const entry of value) {
    if (entry === `!${current}`) return false;
    if (entry === current || (entry === 'any' && value.length === 1)) accepted = true;
  }
  return accepted;
}

const packed = new Map();
const currentLibc = process.report.getReport().header.glibcVersionRuntime === undefined ? 'musl' : 'glibc';
for (const source of sources) {
  for (const filename of readdirSync(source).filter((name) => name.endsWith('.tgz')).sort()) {
    const tarball = resolve(source, filename);
    const manifest = JSON.parse(execFileSync('tar', ['-xOf', tarball, 'package/package.json'], {encoding: 'utf8'}));
    if (typeof manifest.name !== 'string' || typeof manifest.version !== 'string') {
      throw new Error(`${tarball} has no package name or version`);
    }
    if (packed.has(manifest.name)) {
      throw new Error(`duplicate packed package ${manifest.name}`);
    }
    packed.set(manifest.name, {
      compatible: acceptsPlatform(manifest.os, process.platform)
        && acceptsPlatform(manifest.cpu, process.arch)
        && acceptsPlatform(manifest.libc, currentLibc),
      manifest,
      url: pathToFileURL(tarball).href,
    });
  }
}

const selected = new Set();
const queue = ['@deepseek-ai/dsh'];
for (const name of queue) {
  if (selected.has(name)) continue;
  const entry = packed.get(name);
  if (entry === undefined) throw new Error(`packed DSH dependency ${name} is missing`);
  selected.add(name);
  for (const section of ['dependencies', 'optionalDependencies', 'peerDependencies']) {
    for (const dependency of Object.keys(entry.manifest[section] ?? {})) {
      if (packed.has(dependency)) queue.push(dependency);
    }
  }
}

const dependencies = {};
const optionalDependencies = {};
for (const name of [...selected].sort()) {
  const entry = packed.get(name);
  (entry.compatible ? dependencies : optionalDependencies)[name] = entry.url;
}

mkdirSync(output, {recursive: true});
const subprocessTarball = dependencies['@deepseek-ai/dsh-subprocess-local'];
if (subprocessTarball === undefined) {
  throw new Error('packed DSH distribution has no subprocess implementation');
}
writeFileSync(resolve(output, 'package.json'), `${JSON.stringify({
  name: 'gitea-codespace-dsh',
  private: true,
  dependencies,
  optionalDependencies,
  allowScripts: {
    [`file:${fileURLToPath(subprocessTarball)}`]: true,
    '@google/genai': true,
    esbuild: true,
    koffi: true,
    'node-pty': true,
    protobufjs: true,
  },
}, null, 2)}\n`);

const install = spawnSync('npm', [
  'install',
  '--package-lock=false',
  '--no-audit',
  '--no-fund',
  '--include=optional',
], {cwd: output, stdio: 'inherit'});
if (install.error) throw install.error;
if (install.status !== 0) process.exit(install.status ?? 1);
