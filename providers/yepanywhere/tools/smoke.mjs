// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT
import assert from 'node:assert/strict';
import {execFile, spawn} from 'node:child_process';
import {once} from 'node:events';
import {mkdtemp, readFile, rm} from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {createInterface} from 'node:readline';
import {pathToFileURL} from 'node:url';
import {promisify} from 'node:util';

const root = path.resolve(process.argv[2]);
const home = await mkdtemp(path.join(os.tmpdir(), 'yep-tools-smoke-'));
const {dependencies: versions} = JSON.parse(await readFile(path.join(root, 'program/tools/package.json'), 'utf8'));
// An empty PATH verifies that each program uses the bundled runtime.
const env = {HOME: home, PATH: '', LANG: 'C.UTF-8',
  XDG_CONFIG_HOME: path.join(home, 'config'), XDG_DATA_HOME: path.join(home, 'data'),
  XDG_CACHE_HOME: path.join(home, 'cache'), XDG_STATE_HOME: path.join(home, 'state'),
  OPENCODE_DISABLE_MODELS_FETCH: 'true', OPENCODE_DISABLE_AUTOUPDATE: 'true'};
try {
  for (const [name, pkg] of [['codex', '@openai/codex'], ['opencode', 'opencode-ai'], ['pi', '@earendil-works/pi-coding-agent']]) {
    const executable = path.join(root, 'program/tools/bin', name);
    const {stdout} = await promisify(execFile)(executable, ['--version'], {env, cwd: home, timeout: 30_000});
    assert.ok(stdout.includes(versions[pkg]), `${name}: unexpected version ${stdout}`);
    console.log(`${name}: ${stdout.trim()}`);
    if (name === 'codex') {
      const {configureCliInstallations} = await import(pathToFileURL(path.join(root, 'program/app/dist/sdk/cli-installation-runtime.js')));
      // The installation probe owns its local API, not the workspace connection.
      process.env.OPENAI_API_KEY = 'unused-workspace-test-key';
      process.env.CODESPACE_YEP_CONNECTIONS = JSON.stringify({codex: {
        baseUrl: 'http://127.0.0.1:1/unused', apiKeyEnv: 'OPENAI_API_KEY', models: [],
      }});
      const installations = configureCliInstallations(home, () => ({}), () => ({}));
      const launch = await installations.resolve('codex', executable);
      assert.equal(launch.command, executable);
      console.log('codex: isolated installation execution verified');
      continue;
    }
    const args = name === 'pi' ? ['--mode', 'rpc', '--no-session'] : ['serve', '--hostname', '127.0.0.1', '--port', '0'];
    const child = spawn(executable, args, {env, cwd: home, detached: true, stdio: ['pipe', 'pipe', 'pipe']});
    const exited = once(child, 'exit');
    let output = '';
    child.stderr.on('data', (chunk) => { output += chunk; });
    const timer = setTimeout(() => process.kill(-child.pid, 'SIGKILL'), 30_000);
    try {
      if (name === 'pi') child.stdin.write(`${JSON.stringify({id: 'smoke', type: 'get_state'})}\n`);
      let ready = false;
      for await (const line of createInterface({input: child.stdout})) {
        output += `${line}\n`;
        if (name === 'opencode') {
          const address = line.match(/http:\/\/127\.0\.0\.1:\d+/)?.[0];
          if (!address) continue;
          const response = await fetch(`${address}/global/health`);
          assert.equal(response.status, 200);
          assert.equal((await response.json()).healthy, true);
        } else {
          let message;
          try { message = JSON.parse(line); } catch { continue; }
          if (message.id !== 'smoke') continue;
          assert.equal(message.success, true, line);
        }
        ready = true;
        break;
      }
      assert.ok(ready, `${name}: protocol startup failed\n${output}`);
    } finally {
      if (child.exitCode === null && child.signalCode === null) process.kill(-child.pid, 'SIGTERM');
      await exited;
      clearTimeout(timer);
    }
  }
} finally {
  await rm(home, {recursive: true, force: true});
}
