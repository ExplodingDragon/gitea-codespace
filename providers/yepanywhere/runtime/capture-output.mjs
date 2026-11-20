// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT
import {spawn} from 'node:child_process';
import {writeFile} from 'node:fs/promises';
import {constants} from 'node:os';
import path from 'node:path';
import {StringDecoder} from 'node:string_decoder';

const root = process.env.CODESPACE_WEB_IDE_PROGRAM_DIR;
const maxOutputBytes = 8 * 1024;
const tail = [];
let tailBytes = 0;
const child = spawn(process.execPath, [path.join(root, 'lib/launch.mjs')], {stdio: ['ignore', 'pipe', 'pipe']});
// Yep's logger writes errors to stdout too. Decode each stream independently.
for (const name of ['stdout', 'stderr']) {
  const decoder = new StringDecoder('utf8');
  let pending = '';
  let discarding = false;
  // Keep complete lines only: truncating a credential could bypass redaction.
  const capture = (text) => {
    const parts = text.replaceAll('\0', '\ufffd').split('\n');
    for (const [index, part] of parts.entries()) {
      if (!discarding) {
        pending += part;
        if (Buffer.byteLength(pending) + 1 > maxOutputBytes) {
          pending = '';
          discarding = true;
        }
      }
      if (index === parts.length - 1) break;
      const line = discarding ? `[${name} line omitted: exceeds 8 KiB]\n` : `${pending}\n`;
      tail.push(line);
      tailBytes += Buffer.byteLength(line);
      while (tailBytes > maxOutputBytes) tailBytes -= Buffer.byteLength(tail.shift());
      pending = '';
      discarding = false;
    }
  };
  child[name].on('data', (chunk) => {
    process[name].write(chunk);
    capture(decoder.write(chunk));
  });
  child[name].on('end', () => {
    capture(decoder.end());
    if (pending || discarding) capture('\n');
  });
}
let stopping = false;
for (const signal of ['SIGHUP', 'SIGINT', 'SIGTERM']) {
  process.on(signal, () => {
    stopping = true;
    child.kill(signal);
  });
}
let startError;
child.once('error', (error) => { startError = error; });
const {code, signal} = await new Promise((resolve) => child.once('close', (code, signal) => resolve({code, signal})));
process.exitCode = signal ? 128 + constants.signals[signal] : code ?? 1;
if (!stopping) {
  const reason = startError ? `could not start: ${startError.message}` : `exited with ${signal || `code ${code}`}`;
  await writeFile(process.argv[2], `Yep Anywhere ${reason}\n${tail.join('')}`, {mode: 0o600});
}
