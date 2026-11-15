// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT
import {mkdir, readFile, realpath, stat, writeFile} from 'node:fs/promises';
import path from 'node:path';
import {pathToFileURL} from 'node:url';

const root = process.env.CODESPACE_WEB_IDE_PROGRAM_DIR;
const dataDir = process.env.CODESPACE_WEB_IDE_DATA_DIR;
const workspace = process.env.CODESPACE_WEB_IDE_WORKSPACE;
if (!root || !dataDir || !workspace) throw new Error('Web IDE program, data and workspace directories are required');
const chunks = [];
let size = 0;
for await (const chunk of process.stdin) {
  size += chunk.length;
  if (size > 1024 * 1024) throw new Error('Web IDE options exceed 1 MiB');
  chunks.push(chunk);
}
const request = JSON.parse(Buffer.concat(chunks).toString('utf8'));
if (request.protocolVersion !== 1) throw new Error(`unsupported Web IDE adapter protocol ${request.protocolVersion}`);
if (!(await stat(workspace)).isDirectory()) throw new Error('Yep Anywhere workspace must be a directory');
await mkdir(dataDir, {recursive: true});
process.env.YEP_DATA_DIR = dataDir;
const app = pathToFileURL(`${path.join(root, 'program/app/dist')}/`);
const {CODESPACE_PROVIDERS} = await import(new URL('../bundled/@yep-anywhere/shared/dist/codespace.js', app));
const {parseCodespaceConnections} = await import(new URL('../bundled/@yep-anywhere/shared/dist/codespace-connections.js', app));
const {openDiscoveryDatabase} = await import(new URL('storage/discovery-sqlite.js', app));
openDiscoveryDatabase({dataDir}).close();

const marker = path.join(dataDir, '.codespace-initialized');
let initialized = false;
try {
  await readFile(marker);
  initialized = true;
} catch (error) {
  if (error.code !== 'ENOENT') throw error;
}
if (!initialized) {
  const template = parseOptions(request.templateOptions);
  const repository = parseOptions(request.repositoryOptions);
  const {ServerSettingsService} = await import(new URL('services/ServerSettingsService.js', app));
  const settings = new ServerSettingsService({dataDir});
  await settings.initialize();
  await settings.updateSettings({...template.settings, ...repository.settings, codexUpdatePolicy: 'off'});
  const connections = {...template.connections, ...repository.connections};
  await writeFile(path.join(dataDir, 'codespace-options.json'), `${JSON.stringify({providers: repository.providers ?? template.providers ?? CODESPACE_PROVIDERS, connections})}\n`, {mode: 0o600});
  await writeFile(marker, '1\n', {mode: 0o600});
}

const {ProjectMetadataService} = await import(new URL('metadata/ProjectMetadataService.js', app));
const {encodeProjectId, canonicalizeProjectPath} = await import(new URL('projects/paths.js', app));
const projects = new ProjectMetadataService({dataDir});
await projects.initialize();
const projectPath = canonicalizeProjectPath(await realpath(workspace));
const id = encodeProjectId(projectPath);
if (!projects.getAllProjects()[id]) await projects.addProject(id, projectPath);

function parseOptions(value) {
  if (value === undefined || value === null) return {};
  if (typeof value !== 'object' || Array.isArray(value)) throw new Error('Yep Anywhere options must be an object');
  for (const key of Object.keys(value)) {
    if (!['providers', 'settings', 'connections'].includes(key)) throw new Error(`unsupported Yep Anywhere option: ${key}`);
  }
  if (value.providers !== undefined && (!Array.isArray(value.providers) || value.providers.length === 0 ||
      new Set(value.providers).size !== value.providers.length || value.providers.some((item) => typeof item !== 'string'))) {
    throw new Error('Yep Anywhere providers must be a non-empty array of unique backend IDs');
  }
  for (const provider of value.providers ?? []) {
    if (!CODESPACE_PROVIDERS.includes(provider)) throw new Error(`unsupported Yep Anywhere backend: ${provider}; supported backends: ${CODESPACE_PROVIDERS.join(', ')}`);
  }
  if (value.settings !== undefined && (value.settings === null || typeof value.settings !== 'object' || Array.isArray(value.settings))) {
    throw new Error('Yep Anywhere settings must be an object');
  }
  for (const name of ['allowedHosts', 'codexUpdatePolicy', 'publicSharesEnabled', 'limitedUsersEnabled']) {
    if (Object.hasOwn(value.settings ?? {}, name)) throw new Error(`Yep Anywhere setting ${name} is managed by the Provider`);
  }
  return {...value, connections: parseCodespaceConnections(value.connections)};
}
