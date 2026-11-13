// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import * as vscode from "vscode";
import { registerUSB } from "./usb-view.js";
import { readSSHConnection, sshCommand } from "./connection.js";
import {
  EndpointClient,
  type Endpoint,
  type EndpointVisibility,
} from "./cli.js";

class PortItem extends vscode.TreeItem {
  constructor(readonly endpoint: Endpoint) {
    super(
      endpoint.label || String(endpoint.port),
      vscode.TreeItemCollapsibleState.None,
    );
    this.description = `${endpoint.port} · ${visibilityLabel(endpoint.visibility)}`;
    this.contextValue = "codespacePort";
    this.iconPath = new vscode.ThemeIcon(
      endpoint.visibility === "public" ? "globe" : "lock",
    );
    this.tooltip = `${endpoint.label || endpoint.port} (${endpoint.port})`;
  }
}

class PortsProvider implements vscode.TreeDataProvider<PortItem> {
  private readonly changes = new vscode.EventEmitter<
    PortItem | undefined | void
  >();
  readonly onDidChangeTreeData = this.changes.event;

  constructor(
    private readonly client: EndpointClient,
    private readonly reportError: (error: unknown) => void,
  ) {}

  refresh(): void {
    this.changes.fire();
  }

  getTreeItem(item: PortItem): vscode.TreeItem {
    return item;
  }

  async getChildren(): Promise<PortItem[]> {
    try {
      return (await this.client.list())
        .sort((a, b) => a.port - b.port)
        .map((endpoint) => new PortItem(endpoint));
    } catch (error) {
      this.reportError(error);
      return [];
    }
  }
}

export async function activate(
  context: vscode.ExtensionContext,
): Promise<void> {
  const output = vscode.window.createOutputChannel("Gitea Codespace");
  const connection = readSSHConnection(process.env);
  context.subscriptions.push(
    vscode.commands.registerCommand("giteaCodespace.ssh.copy", async () => {
      if (!connection) {
        void vscode.window.showWarningMessage(
          vscode.l10n.t(
            "SSH connection information is unavailable. Use the Codespace page in Gitea.",
          ),
        );
        return;
      }
      await vscode.env.clipboard.writeText(sshCommand(connection));
      void vscode.window.showInformationMessage(
        vscode.l10n.t("Command copied. Run it on your local computer."),
      );
    }),
  );
  const client = new EndpointClient((message) => output.appendLine(message));
  const reportError = (error: unknown) => {
    output.appendLine(
      error instanceof Error ? error.stack || error.message : String(error),
    );
    void vscode.window.showErrorMessage(
      vscode.l10n.t("Codespace port operation failed"),
    );
  };
  const provider = new PortsProvider(client, reportError);
  context.subscriptions.push(
    output,
    { dispose: () => client.dispose() },
    vscode.window.registerTreeDataProvider("giteaCodespace.ports", provider),
  );

  const run =
    <Arguments extends unknown[]>(
      operation: (...arguments_: Arguments) => Promise<void>,
    ) =>
    async (...arguments_: Arguments) => {
      try {
        await operation(...arguments_);
        provider.refresh();
      } catch (error) {
        reportError(error);
      }
    };

  context.subscriptions.push(
    vscode.commands.registerCommand("giteaCodespace.ports.refresh", () =>
      provider.refresh(),
    ),
    vscode.commands.registerCommand(
      "giteaCodespace.ports.add",
      run(async () => {
        const port = await readPort();
        if (port === undefined) return;
        const label = await vscode.window.showInputBox({
          title: vscode.l10n.t("Publish port"),
          prompt: vscode.l10n.t("Port label"),
          value: String(port),
          validateInput: validateLabel,
        });
        if (label === undefined) return;
        const visibility = await selectVisibility();
        if (!visibility) return;
        await client.set({ port, label: label.trim(), visibility });
      }),
    ),
    vscode.commands.registerCommand(
      "giteaCodespace.ports.rename",
      run(async (item?: PortItem) => {
        if (!item) return;
        const label = await vscode.window.showInputBox({
          title: vscode.l10n.t("Rename port"),
          prompt: vscode.l10n.t("Port label"),
          value: item.endpoint.label,
          validateInput: validateLabel,
        });
        if (label === undefined) return;
        await client.set({ ...item.endpoint, label: label.trim() });
      }),
    ),
    vscode.commands.registerCommand(
      "giteaCodespace.ports.visibility",
      run(async (item?: PortItem) => {
        if (!item) return;
        const visibility = await selectVisibility(item.endpoint.visibility);
        if (!visibility) return;
        await client.set({ ...item.endpoint, visibility });
      }),
    ),
    vscode.commands.registerCommand(
      "giteaCodespace.ports.remove",
      run(async (item?: PortItem) => {
        if (!item) return;
        const confirmed = await vscode.window.showWarningMessage(
          vscode.l10n.t(
            "Remove port {0}? This removes its Codespace publication and does not stop the application.",
            item.endpoint.port,
          ),
          { modal: true },
          vscode.l10n.t("Remove port"),
        );
        if (confirmed) await client.delete(item.endpoint.port);
      }),
    ),
  );

  try {
    await registerUSB(context, output);
  } catch (error) {
    output.appendLine(`USB initialization failed: ${String(error)}`);
  }
}

async function readPort(): Promise<number | undefined> {
  const value = await vscode.window.showInputBox({
    title: vscode.l10n.t("Publish port"),
    prompt: vscode.l10n.t("Port number"),
    validateInput: (input) => {
      const port = Number(input);
      return Number.isInteger(port) && port >= 1 && port <= 65535
        ? undefined
        : vscode.l10n.t("Enter a number between 1 and 65535");
    },
  });
  return value === undefined ? undefined : Number(value);
}

async function selectVisibility(
  current?: EndpointVisibility,
): Promise<EndpointVisibility | undefined> {
  const choices: Array<vscode.QuickPickItem & { value: EndpointVisibility }> = [
    {
      label: vscode.l10n.t("Authenticated"),
      value: "authenticated",
      picked: current === "authenticated",
    },
    {
      label: vscode.l10n.t("Public"),
      value: "public",
      picked: current === "public",
    },
  ];
  const selected = await vscode.window.showQuickPick(choices, {
    title: vscode.l10n.t("Change visibility"),
  });
  return selected?.value;
}

function visibilityLabel(visibility: EndpointVisibility): string {
  return visibility === "public"
    ? vscode.l10n.t("Public")
    : vscode.l10n.t("Authenticated");
}

function validateLabel(value: string): string | undefined {
  const length = new TextEncoder().encode(value.trim()).length;
  if (length < 1 || length > 128)
    return vscode.l10n.t("Enter a label between 1 and 128 bytes");
  return undefined;
}
