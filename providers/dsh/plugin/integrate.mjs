// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import {readFile, writeFile} from 'node:fs/promises';
import path from 'node:path';

const root = process.argv[2];
if (!root) throw new Error('usage: integrate.mjs <DeepSeek-Harness-root>');

for (const face of ['host', 'client']) {
  const filename = path.join(root, `tsconfig.${face}.json`);
  const content = await readFile(filename, 'utf8');
  const reference = `    { "path": "./packages/codespace/codespace/tsconfig.${face}.json" },\n`;
  if (content.includes(reference.trim())) throw new Error(`Codespace ${face} project is already registered`);
  const updated = content.replace(/("references"\s*:\s*\[\s*\n)/u, `$1${reference}`);
  if (updated === content) throw new Error(`Cannot find references in ${filename}`);
  await writeFile(filename, updated);
}
