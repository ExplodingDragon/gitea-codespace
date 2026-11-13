// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { registerHooks } from "node:module";
import { resolve } from "node:path";
import { it } from "node:test";
import type * as vscode from "vscode";
import { parseUSBResponse, USBClient } from "../src/usb-client.js";
import { readSSHConnection, sshCommand } from "../src/connection.js";

it("generates local SSH commands from only the workspace connection fields", () => {
  const environment = {
    CODESPACE_SSH_HOST: "ssh.example.com",
    CODESPACE_SSH_PORT: "2222",
    CODESPACE_SSH_USER: "cs-933ef4a9-54c7-4f36-aad9-32ea72c4c986",
    GITEA_TOKEN: "private",
  };
  const connection = readSSHConnection(environment)!;
  assert.deepEqual(connection, {
    host: "ssh.example.com",
    port: 2222,
    user: environment.CODESPACE_SSH_USER,
  });
  assert.equal(
    sshCommand(connection),
    `ssh -p 2222 -l ${connection.user} ssh.example.com`,
  );
  assert.equal(
    sshCommand(connection, "lab"),
    `ssh -NT -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -R usbip.lab:3240:127.0.0.1:3240 -p 2222 -l ${connection.user} ssh.example.com`,
  );
  assert.throws(() => sshCommand(connection, "lab; echo injected"));
  assert.equal(readSSHConnection({}), null);
  assert.equal(
    readSSHConnection({
      ...environment,
      CODESPACE_SSH_HOST: "-oProxyCommand=bad",
    }),
    null,
  );
});

it("reads imported devices independently of forwarding, including VHCI port zero", () => {
  const device = {
    source: "",
    busId: "1-1",
    description: "CDC ACM",
    port: 0,
    state: "Port in Use",
  };
  assert.deepEqual(
    parseUSBResponse(
      JSON.stringify({ version: 1, sources: [], devices: [device] }),
      true,
    ),
    {
      sources: [],
      devices: [device],
    },
  );
  assert.throws(() =>
    parseUSBResponse(
      '{"version":1,"sources":[{"name":"lab"}],"devices":[{"source":"lab","busId":"1-1","description":"CDC ACM"}]}',
      true,
    ),
  );
  assert.throws(() => parseUSBResponse('{"version":2,"devices":[]}', false));
  for (const name of [123, null, undefined]) {
    assert.throws(() =>
      parseUSBResponse(
        JSON.stringify({ version: 1, sources: [{ name }], devices: [] }),
        false,
      ),
    );
  }
});

it("activates capability discovery and gates every USB entry point", async () => {
  const manifest = JSON.parse(
    await readFile(resolve(__dirname, "../package.json"), "utf8"),
  );
  assert.ok(manifest.activationEvents.includes("onStartupFinished"));
  for (const command of manifest.contributes.commands.filter(
    (item: { command: string }) =>
      item.command.startsWith("giteaCodespace.usb."),
  )) {
    assert.match(command.enablement, /giteaCodespace.usbAvailable/);
    if (
      command.command.endsWith(".attach") ||
      command.command.endsWith(".detach")
    )
      assert.match(command.enablement, /giteaCodespace.usbStatusReady/);
    assert.equal(
      manifest.contributes.menus.commandPalette.find(
        (item: { command: string }) => item.command === command.command,
      ).when,
      "giteaCodespace.usbAvailable",
    );
  }
  assert.equal(
    manifest.contributes.views.explorer.find(
      (item: { id: string }) => item.id === "giteaCodespace.usb",
    ).when,
    "giteaCodespace.usbAvailable",
  );
});

it("filters imported devices and recovers from stale status without repeating mutations", async (t) => {
  const commands = new Map<string, (...args: unknown[]) => unknown>();
  const contexts = new Map<string, boolean>();
  const errors: string[] = [];
  const warnings: string[] = [];
  const picks: {
    device: { source: string; busId: string; port?: number };
  }[][] = [];
  let copied = "";
  const environment = { ...process.env };
  Object.assign(process.env, {
    CODESPACE_SSH_HOST: "ssh.example.com",
    CODESPACE_SSH_PORT: "2222",
    CODESPACE_SSH_USER: "cs-933ef4a9-54c7-4f36-aad9-32ea72c4c986",
  });
  t.after(() => {
    process.env = environment;
  });
  let confirm = false;
  let statusFails = false;
  let detachFails = false;
  const disposable = { dispose() {} };
  const view = {
    ...disposable,
    message: "",
    visible: false,
    onDidChangeVisibility: () => disposable,
  };
  let tree: vscode.TreeDataProvider<vscode.TreeItem>;
  const api = {
    env: {
      clipboard: {
        writeText: async (value: string) => {
          copied = value;
        },
      },
    },
    TreeItem: class {
      constructor(readonly label: string) {}
    },
    ThemeIcon: class {},
    TreeItemCollapsibleState: { None: 0, Expanded: 2 },
    EventEmitter: class {
      event = () => disposable;
      fire() {}
      dispose() {}
    },
    l10n: {
      t: (text: string, ...args: unknown[]) =>
        text.replace(/\{(\d+)\}/g, (_, index) => String(args[Number(index)])),
    },
    commands: {
      executeCommand: async (_command: string, key: string, value: boolean) => {
        contexts.set(key, value);
      },
      registerCommand: (
        key: string,
        handler: (...args: unknown[]) => unknown,
      ) => {
        commands.set(key, handler);
        return disposable;
      },
    },
    window: {
      createOutputChannel: () => ({ ...disposable, appendLine() {} }),
      registerTreeDataProvider: () => disposable,
      showInputBox: async (options: {
        value: string;
        validateInput: (value: string) => unknown;
      }) => {
        assert.equal(options.value, "workstation");
        assert.ok(options.validateInput("invalid source"));
        assert.equal(options.validateInput("lab"), undefined);
        return "lab";
      },
      createTreeView: (
        _id: string,
        options: { treeDataProvider: typeof tree },
      ) => {
        tree = options.treeDataProvider;
        return view;
      },
      showErrorMessage: async (text: string) => {
        errors.push(text);
      },
      showInformationMessage: async () => {},
      showWarningMessage: async (
        text: string,
        options?: { modal: boolean },
        choice?: string,
      ) => {
        warnings.push(text);
        return options?.modal && confirm ? choice : undefined;
      },
      showQuickPick: async (items: (typeof picks)[number]) => {
        picks.push(items);
        return items[0];
      },
    },
  };
  // Supply the extension host API while exercising the actual command handlers.
  const key = Symbol.for("codespace.test.vscode");
  Object.defineProperty(globalThis, key, { value: api, configurable: true });
  const hooks = registerHooks({
    resolve(specifier, context, next) {
      return specifier === "vscode"
        ? { url: "codespace-test:vscode", shortCircuit: true }
        : next(specifier, context);
    },
    load(url, context, next) {
      return url === "codespace-test:vscode"
        ? {
            format: "commonjs",
            source:
              'module.exports = globalThis[Symbol.for("codespace.test.vscode")]',
            shortCircuit: true,
          }
        : next(url, context);
    },
  });
  const subscriptions: vscode.Disposable[] = [];
  t.after(() => {
    subscriptions.forEach((item) => item.dispose());
    hooks.deregister();
    Reflect.deleteProperty(globalThis, key);
  });
  const device = {
    source: "workstation",
    busId: "1-3",
    description: "Disk",
    port: 0,
    state: "Port in Use",
  };
  t.mock.method(USBClient.prototype, "available", async () => true);
  t.mock.method(USBClient.prototype, "status", async () => {
    if (statusFails) throw new Error("Status unavailable");
    return {
      sources: [{ name: "workstation" }, { name: "lab" }],
      devices: [device],
    };
  });
  t.mock.method(USBClient.prototype, "list", async () => ({
    sources: [
      { name: "workstation" },
      { name: "lab" },
      { name: "offline", error: "Source unavailable" },
    ],
    devices: [device, { ...device, source: "lab" }],
  }));
  const attach = t.mock.method(USBClient.prototype, "attach", async () => {});
  const detach = t.mock.method(USBClient.prototype, "detach", async () => {
    if (detachFails) {
      statusFails = true;
      throw new Error("Detach timed out");
    }
  });
  const { registerUSB } = await import("../src/usb-view.js");
  await registerUSB(
    { subscriptions } as vscode.ExtensionContext,
    { appendLine() {} } as unknown as vscode.OutputChannel,
  );
  assert.equal(contexts.get("giteaCodespace.usbStatusReady"), true);
  await commands.get("giteaCodespace.usb.attach")!();
  assert.deepEqual(
    picks[0].map((item) => item.device.source),
    ["lab"],
  );
  assert.deepEqual(attach.mock.calls[0].arguments, ["1-3", "lab"]);
  assert.match(warnings[0], /offline: Source unavailable/);
  await commands.get("giteaCodespace.usb.detach")!();
  assert.equal(detach.mock.callCount(), 0);
  assert.match(warnings.at(-1)!, /workstation.*VHCI port 0/);
  confirm = true;
  detachFails = true;
  const previousItems = await tree!.getChildren!();
  await commands.get("giteaCodespace.usb.detach")!();
  assert.deepEqual(detach.mock.calls[0].arguments, [0]);
  assert.equal(errors.length, 1);
  assert.match(errors[0], /Detach timed out/);
  assert.match(errors[0], /Status unavailable/);
  assert.match(view.message, /out of date/);
  assert.equal(contexts.get("giteaCodespace.usbStatusReady"), false);
  await commands.get("giteaCodespace.usb.copyTunnel")!();
  assert.match(copied, /-R usbip.lab:3240:127.0.0.1:3240/);
  assert.match(
    copied,
    /-p 2222 -l cs-933ef4a9-54c7-4f36-aad9-32ea72c4c986 ssh.example.com$/,
  );
  assert.equal(await tree!.getChildren!(), previousItems);
  await commands.get("giteaCodespace.usb.detach")!();
  await commands.get("giteaCodespace.usb.attach")!();
  assert.equal(detach.mock.callCount(), 1);
  assert.equal(attach.mock.callCount(), 1);
  statusFails = false;
  await commands.get("giteaCodespace.usb.refresh")!();
  assert.equal(contexts.get("giteaCodespace.usbStatusReady"), true);
  commands.clear();
  t.mock.method(USBClient.prototype, "available", async () => false);
  const { activate } = await import("../src/extension.js");
  await activate({ subscriptions } as vscode.ExtensionContext);
  assert.equal(contexts.get("giteaCodespace.usbAvailable"), false);
  assert.ok(
    [...commands.keys()].every(
      (name) => !name.startsWith("giteaCodespace.usb."),
    ),
  );
  await commands.get("giteaCodespace.ssh.copy")!();
  assert.equal(
    copied,
    "ssh -p 2222 -l cs-933ef4a9-54c7-4f36-aad9-32ea72c4c986 ssh.example.com",
  );
});
