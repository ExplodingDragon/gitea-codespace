// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import {mkdir, readdir, readFile, rename, writeFile} from 'node:fs/promises';
import {spawn} from 'node:child_process';
import {createHash} from 'node:crypto';
import path from 'node:path';
import {applyEdits, modify, parse, printParseErrorCode} from 'jsonc-parser';

const root = requiredEnv('CODESPACE_WEB_IDE_PROGRAM_DIR');
const data = requiredEnv('CODESPACE_WEB_IDE_DATA_DIR');
const request = JSON.parse(await readStdin());
if (request.protocolVersion !== 1) throw new Error(`unsupported Web IDE adapter protocol ${request.protocolVersion}`);
const template = vscodeOptions(request.templateOptions);
const repository = vscodeOptions(request.repositoryOptions);
const extensions = mergeExtensions(template.extensions, repository.extensions);
const executable = path.join(root, 'program/bin/code-server');
const userData = path.join(data, 'user-data');
const extensionDir = path.join(data, 'extensions');
await mkdir(path.join(userData, 'User'), {recursive: true});
await mkdir(extensionDir, {recursive: true});

const settingsPath = path.join(userData, 'User/settings.json');
let previousSettings = '';
try {
  previousSettings = await readFile(settingsPath, 'utf8');
} catch (error) {
  if (error.code !== 'ENOENT') throw error;
}
let settingsContent = previousSettings || `${JSON.stringify(request.initialize ? {...template.settings, ...repository.settings} : {}, null, 2)}\n`;
const errors = [];
const parsedSettings = parse(settingsContent, errors, {allowTrailingComma: true, allowEmptyContent: true});
const settings = parsedSettings === undefined ? {} : parsedSettings;
if (errors.length) throw new Error(`Invalid VS Code settings: ${printParseErrorCode(errors[0].error)}`);
if (!isObject(settings)) throw new Error('VS Code settings must be an object');
for (const [key, value] of Object.entries({'chat.disableAIFeatures': true, 'telemetry.telemetryLevel': 'off'})) {
  if (!Object.hasOwn(settings, key) || (key === 'telemetry.telemetryLevel' && settings[key] !== value)) {
    settingsContent = applyEdits(settingsContent, modify(settingsContent, [key], value, {
      formattingOptions: {insertSpaces: true, tabSize: 2},
    }));
  }
}
if (settingsContent !== previousSettings) await atomicWrite(settingsPath, settingsContent);

// An absent argv.json makes code-server select English before checking the browser.
try {
  await writeFile(path.join(userData, 'User/argv.json'), '{}\n', {flag: 'wx', mode: 0o600});
} catch (error) {
  if (error.code !== 'EEXIST') throw error;
}

const installArgs = ['--disable-telemetry', '--disable-update-check', '--user-data-dir', userData, '--extensions-dir', extensionDir];
if (request.initialize && extensions.length) {
  await run(executable, [...installArgs, ...extensions.flatMap((extension) => ['--install-extension', extension])]);
}

const assets = path.join(root, 'assets');
const bundledVersion = await readFile(path.join(assets, 'extensions.sha256'), 'utf8');
const versionMarker = path.join(data, '.bundled-extensions-version');
let installedVersion = '';
try {
  installedVersion = await readFile(versionMarker, 'utf8');
} catch (error) {
  if (error.code !== 'ENOENT') throw error;
}
if (installedVersion !== bundledVersion) {
  const bundledExtensions = (await readdir(assets)).filter((name) => name.endsWith('.vsix')).sort();
  await run(executable, [
    ...installArgs,
    '--force',
    ...bundledExtensions.flatMap((extension) => ['--install-extension', path.join(assets, extension)]),
  ]);
  // code-server's CLI installs language packs without creating the index used by its web client.
  const languagePacks = {};
  const installedExtensions = JSON.parse(await readFile(path.join(extensionDir, 'extensions.json'), 'utf8'));
  for (const extension of installedExtensions) {
    const manifest = JSON.parse(await readFile(path.join(extension.location.path, 'package.json'), 'utf8'));
    for (const localization of manifest.contributes?.localizations ?? []) {
      const pack = languagePacks[localization.languageId] ??= {
        extensions: [], translations: {}, label: localization.localizedLanguageName ?? localization.languageName,
      };
      pack.extensions.push({extensionIdentifier: extension.identifier, version: manifest.version});
      for (const translation of localization.translations) {
        pack.translations[translation.id] = path.join(extension.location.path, translation.path);
      }
    }
  }
  for (const pack of Object.values(languagePacks)) {
    const hash = createHash('md5');
    for (const extension of pack.extensions) hash.update(extension.extensionIdentifier.uuid || extension.extensionIdentifier.id).update(extension.version);
    pack.hash = hash.digest('hex');
  }
  await atomicWrite(path.join(userData, 'languagepacks.json'), `${JSON.stringify(languagePacks)}\n`);
  await atomicWrite(versionMarker, bundledVersion);
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
