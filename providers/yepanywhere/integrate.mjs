// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT
import {cp, mkdir, readFile, writeFile} from 'node:fs/promises';
import path from 'node:path';
import {fileURLToPath} from 'node:url';

const source = process.argv[2];
if (!source) throw new Error('upstream source directory is required');
const integration = fileURLToPath(new URL('integration/', import.meta.url));
for (const [from, to] of [
  ['shared', 'packages/shared/src'],
  ['server', 'packages/server/src/routes'],
  ['sdk', 'packages/server/src/sdk'],
  ['client', 'packages/client/src/pages'],
  ['tests/server', 'packages/server/test/codespace'],
  ['tests/client', 'packages/client/src/pages/__tests__'],
]) {
  await mkdir(path.join(source, to), {recursive: true});
  await cp(path.join(integration, from), path.join(source, to), {recursive: true});
}
for (const locale of ['en', 'zh-CN']) {
  const file = path.join(source, `packages/client/src/i18n/${locale}.json`);
  const original = JSON.parse(await readFile(file, 'utf8'));
  const messages = JSON.parse(await readFile(path.join(integration, `${locale}.json`), 'utf8'));
  await writeFile(file, `${JSON.stringify({...original, ...messages}, null, 2)}\n`);
}
