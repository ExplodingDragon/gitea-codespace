// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {once} from 'node:events';
import {mkdir, mkdtemp, readFile, rm, writeFile} from 'node:fs/promises';
import {createServer} from 'node:http';
import os from 'node:os';
import path from 'node:path';
import {createInterface} from 'node:readline';
import {pathToFileURL} from 'node:url';

const root = path.resolve(process.argv[2]);
const sdk = pathToFileURL(`${root}/program/app/dist/sdk/`);
const {codespaceEnvironment, codespaceCodexArguments} = await import(new URL('codespace-connections.js', sdk));
const {ClaudeProvider} = await import(new URL('providers/claude.js', sdk));
const {resolveLocalClaudeCodeExecutable} = await import(new URL('providers/claude-executable.js', sdk));
const home = await mkdtemp(path.join(os.tmpdir(), 'yep-connections-smoke-'));
const piAuthPath = path.join(home, '.pi/agent/auth.json');
const piAuth = JSON.stringify({codespace: {type: 'api_key', key: 'previous-pi-key'}});
await mkdir(path.dirname(piAuthPath), {recursive: true});
await writeFile(piAuthPath, piAuth);
const received = [];
const api = createServer(async (request, response) => {
  const chunks = [];
  for await (const chunk of request) chunks.push(chunk);
  received.push({url: request.url, headers: request.headers, body: Buffer.concat(chunks).toString()});
  // A terminal API error exercises the real request path without generating tokens.
  response.writeHead(400, {'content-type': 'application/json'});
  response.end(JSON.stringify({error: {type: 'invalid_request_error', message: 'Codespace local API smoke test'}}));
});
api.listen(0, '127.0.0.1');
await once(api, 'listening');
const apiUrl = `http://127.0.0.1:${api.address().port}`;
const claudeSettingsPath = path.join(home, '.claude/settings.json');
const claudeSettings = JSON.stringify({env: {
  ANTHROPIC_BASE_URL: `${apiUrl}/old-claude`, ANTHROPIC_API_KEY: 'previous-claude-key',
  ANTHROPIC_AUTH_TOKEN: 'previous-claude-bearer',
}});
await mkdir(path.dirname(claudeSettingsPath), {recursive: true});
await writeFile(claudeSettingsPath, claudeSettings);
const keyName = 'CODESPACE_SMOKE_KEY';
process.env[keyName] = 'local-test-key';
// Isolated HOME and environment keep this test independent of developer logins.
const inherited = {HOME: home, PATH: '/usr/bin:/bin', LANG: 'C.UTF-8',
  OPENAI_API_KEY: 'unrelated-openai-key', ANTHROPIC_API_KEY: 'unrelated-anthropic-key',
  ANTHROPIC_AUTH_TOKEN: 'unrelated-bearer-token',
  XDG_CONFIG_HOME: path.join(home, 'config'), XDG_DATA_HOME: path.join(home, 'data'),
  XDG_CACHE_HOME: path.join(home, 'cache'), XDG_STATE_HOME: path.join(home, 'state'),
  OPENCODE_DISABLE_MODELS_FETCH: 'true', OPENCODE_DISABLE_AUTOUPDATE: 'true',
  CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: '1'};

try {
  const cases = [
    ['codex', 'openai-responses'], ['codex-oss', 'openai-responses'],
    ['claude', 'api-key'], ['claude', 'bearer'],
    ...['opencode', 'pi'].flatMap((backend) => ['openai-completions', 'openai-responses', 'anthropic-messages'].map((protocol) => [backend, protocol])),
    ...['codex', 'claude', 'opencode', 'pi'].map((backend) => [backend, 'no-auth']),
  ];
  for (const [backend, protocol] of cases) {
    const prefix = `/${backend}/${protocol}`;
    const apiProtocol = protocol === 'no-auth' ? 'openai-completions' : protocol;
    process.env.CODESPACE_YEP_CONNECTIONS = JSON.stringify({[backend]: {
      baseUrl: `${apiUrl}${prefix}`,
      ...(protocol !== 'no-auth' ? {apiKeyEnv: keyName} : {}),
      models: [{id: 'test-model'}],
      ...(backend === 'claude' ? {authMode: protocol === 'bearer' ? 'bearer' : 'api-key'} : {}),
      ...(['pi', 'opencode'].includes(backend) ? {api: apiProtocol} : {}),
    }});
    let command = path.join(root, 'program/tools/bin', backend === 'codex-oss' ? 'codex' : backend);
    let args;
    const env = codespaceEnvironment(backend, {...inherited, CODESPACE_YEP_CONNECTIONS: process.env.CODESPACE_YEP_CONNECTIONS});
    if (backend === 'codex') {
      args = ['app-server', '--listen', 'stdio://', ...codespaceCodexArguments(backend)];
    } else if (backend === 'codex-oss') {
      args = ['exec', ...codespaceCodexArguments(backend), '--skip-git-repo-check', '--json', 'Say hello'];
    } else if (backend === 'claude') {
      command = resolveLocalClaudeCodeExecutable();
      const claude = new ClaudeProvider();
      args = ['--print', '--model', 'test-model', '--settings', JSON.stringify(claude.getSettings()), 'Say hello'];
    } else if (backend === 'opencode') {
      args = ['serve', '--hostname', '127.0.0.1', '--port', '0'];
    } else {
      args = ['--mode', 'rpc', '--no-session', '--extension', path.join(root, 'program/app/dist/sdk/providers/pi-yep-anywhere-extension.mjs'), '--provider', 'codespace', '--model', 'test-model', '--api-key', protocol === 'no-auth' ? 'codespace-no-auth' : env[keyName]];
    }
    const child = spawn(command, args, {env, cwd: home, detached: true, stdio: ['pipe', 'pipe', 'pipe']});
    if (backend === 'codex-oss' || backend === 'claude') child.stdin.end();
    const exited = once(child, 'exit');
    let output = '';
    child.stdout.on('data', (chunk) => { output += chunk; });
    child.stderr.on('data', (chunk) => { output += chunk; });
    const timeout = setTimeout(() => process.kill(-child.pid, 'SIGKILL'), 30_000);
    try {
      if (backend === 'codex') {
        child.stdin.write(`${JSON.stringify({id: 1, method: 'initialize', params: {clientInfo: {name: 'codespace-smoke', version: '1.0.0'}}})}\n`);
        for await (const line of createInterface({input: child.stdout})) {
          let message;
          try { message = JSON.parse(line); } catch { continue; }
          if (message.id) assert.ok(!message.error, line);
          if (message.id === 1) {
            child.stdin.write(`${JSON.stringify({method: 'initialized'})}\n`);
            child.stdin.write(`${JSON.stringify({id: 2, method: 'thread/start', params: {model: 'test-model', cwd: home, approvalPolicy: 'never', sandbox: 'read-only', ephemeral: true}})}\n`);
          } else if (message.id === 2) {
            child.stdin.write(`${JSON.stringify({id: 3, method: 'turn/start', params: {threadId: message.result.thread.id, input: [{type: 'text', text: 'Say hello', text_elements: []}]}})}\n`);
          } else if (message.id === 3) break;
        }
      }
      if (backend === 'pi') child.stdin.write(`${JSON.stringify({type: 'prompt', message: 'Say hello'})}\n`);
      if (backend === 'opencode') {
        for await (const line of createInterface({input: child.stdout})) {
          const address = line.match(/http:\/\/127\.0\.0\.1:\d+/)?.[0];
          if (!address) continue;
          const session = await (await fetch(`${address}/session`, {method: 'POST', headers: {'content-type': 'application/json'}, body: '{}'})).json();
          assert.ok(session.id, JSON.stringify(session));
          const response = await fetch(`${address}/session/${session.id}/prompt_async`, {method: 'POST', headers: {'content-type': 'application/json'}, body: JSON.stringify({model: {providerID: 'codespace', modelID: 'test-model'}, parts: [{type: 'text', text: 'Say hello'}]})});
          assert.ok(response.ok, await response.text());
          break;
        }
      }
      const deadline = Date.now() + 25_000;
      while (!received.some((request) => request.url.startsWith(`${prefix}/`)) && child.exitCode === null && child.signalCode === null && Date.now() < deadline) {
        await new Promise((resolve) => setTimeout(resolve, 50));
      }
      const request = received.find((request) => request.url.startsWith(`${prefix}/`));
      assert.ok(request, `${backend}/${protocol}: no API request\n${output}`);
      const keyHeader = (backend === 'claude' && protocol !== 'bearer') || protocol === 'anthropic-messages';
      const token = protocol === 'no-auth' ? 'codespace-no-auth' : 'local-test-key';
      if (backend === 'codex' && protocol === 'no-auth') assert.equal(request.headers.authorization, undefined);
      else assert.equal(request.headers[keyHeader ? 'x-api-key' : 'authorization'], keyHeader ? token : `Bearer ${token}`);
      assert.match(request.url, backend.startsWith('codex') || apiProtocol === 'openai-responses' ? /\/responses$/ : backend === 'claude' || apiProtocol === 'anthropic-messages' ? /\/messages/ : /\/chat\/completions$/);
      assert.equal(JSON.parse(request.body).model, 'test-model');
      console.log(`${backend}/${protocol}: configured API address, credential and model verified`);
    } finally {
      if (child.exitCode === null && child.signalCode === null) process.kill(-child.pid, 'SIGTERM');
      await exited;
      clearTimeout(timeout);
    }
  }
  assert.equal(await readFile(piAuthPath, 'utf8'), piAuth);
  assert.equal(await readFile(claudeSettingsPath, 'utf8'), claudeSettings);
} finally {
  api.closeAllConnections();
  await new Promise((resolve, reject) => api.close((error) => error ? reject(error) : resolve()));
  await rm(home, {recursive: true, force: true});
}
