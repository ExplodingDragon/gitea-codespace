// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT
import {readFile} from 'node:fs/promises';
import path from 'node:path';
import {pathToFileURL} from 'node:url';

const root = process.env.CODESPACE_WEB_IDE_PROGRAM_DIR;
const dataDir = process.env.CODESPACE_WEB_IDE_DATA_DIR;
const address = process.env.CODESPACE_WEB_IDE_ADDRESS;
if (!root || !dataDir || !address) throw new Error('Web IDE program, data and address are required');
const endpoint = new URL(`http://${address}`);
const options = JSON.parse(await readFile(path.join(dataDir, 'codespace-options.json'), 'utf8'));
const {CODESPACE_PROVIDERS} = await import(pathToFileURL(path.join(root, 'program/app/bundled/@yep-anywhere/shared/dist/codespace.js')));
if (!Array.isArray(options.providers) || !options.providers.length || options.providers.some((name) => !CODESPACE_PROVIDERS.includes(name))) {
  throw new Error('Invalid Yep Anywhere backend selection');
}
Object.assign(process.env, {
  NODE_ENV: 'production',
  YEP_DATA_DIR: dataDir,
  HOST: endpoint.hostname,
  PORT: endpoint.port,
  CLI_HOST_OVERRIDE: 'true',
  CLI_PORT_OVERRIDE: 'true',
  AUTH_DISABLED: 'true',
  YEP_PEER_API_ENABLED: 'false',
  ALLOWED_HOSTS: '*',
  HTTPS_SELF_SIGNED: 'false',
  OPEN_BROWSER: 'false',
  SERVE_FRONTEND: 'true',
  MAINTENANCE_PORT: '0',
  ENABLED_PROVIDERS: options.providers.join(','),
});
// The private Node runs the app; Features retain ownership of PATH and HOME.
await import(pathToFileURL(path.join(root, 'program/app/dist/index.js')));
