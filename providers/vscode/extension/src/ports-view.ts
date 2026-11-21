// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import * as vscode from "vscode";
import {
  EndpointClient,
  type Endpoint,
  type EndpointVisibility,
} from "./cli.js";
import { showOperationError } from "./errors.js";

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

export function registerPorts(
  context: vscode.ExtensionContext,
  output: vscode.OutputChannel,
): void {
  const client = new EndpointClient((message) => output.appendLine(message));
  const changes = new vscode.EventEmitter<void>();
  let items: PortItem[] = [];
  let busy = false;
  let disposed = false;
  let loading: Promise<void> | undefined;
  const view = vscode.window.createTreeView<PortItem>("giteaCodespace.ports", {
    treeDataProvider: {
      onDidChangeTreeData: changes.event,
      getTreeItem: (item) => item,
      getChildren: () => items,
    },
  });
  view.message = vscode.l10n.t("Loading ports...");

  const load = (): Promise<void> => {
    if (loading) return loading;
    loading = (async () => {
      try {
        const endpoints = await client.list();
        if (disposed) return;
        items = endpoints
          .sort((a, b) => a.port - b.port)
          .map((endpoint) => new PortItem(endpoint));
        view.message = undefined;
        changes.fire();
      } catch (error) {
        if (disposed) return;
        view.message = vscode.l10n.t(
          "Port status is out of date. Refresh to continue.",
        );
        throw error;
      } finally {
        loading = undefined;
      }
    })();
    return loading;
  };

  const refresh = async (notify = false) => {
    if (busy || disposed) return;
    try {
      await load();
    } catch (error) {
      if (notify)
        showOperationError(
          output,
          vscode.l10n.t("Could not refresh ports"),
          error,
        );
      else output.appendLine(String(error));
    }
  };

  const selectEndpoint = async (
    port?: number,
  ): Promise<Endpoint | undefined> => {
    await load();
    if (port !== undefined) {
      const endpoint = items.find(
        (item) => item.endpoint.port === port,
      )?.endpoint;
      if (!endpoint)
        throw new Error(
          vscode.l10n.t(
            "Port {0} is no longer published. Refresh and select it again.",
            port,
          ),
        );
      return endpoint;
    }
    if (items.length === 0) {
      void vscode.window.showInformationMessage(
        vscode.l10n.t("No ports are published"),
      );
      return;
    }
    return (
      await vscode.window.showQuickPick(
        items.map(({ endpoint }) => ({
          label: endpoint.label,
          description: `${endpoint.port} · ${visibilityLabel(endpoint.visibility)}`,
          endpoint,
        })),
        { title: vscode.l10n.t("Select a port") },
      )
    )?.endpoint;
  };

  const run =
    (operation: (item?: PortItem) => Promise<void>) =>
    async (item?: PortItem) => {
      if (busy || disposed) return;
      busy = true;
      await vscode.commands.executeCommand(
        "setContext",
        "giteaCodespace.portsBusy",
        true,
      );
      try {
        await operation(item);
      } catch (error) {
        if (!disposed)
          showOperationError(
            output,
            vscode.l10n.t("Codespace port operation failed"),
            error,
          );
      } finally {
        // A failed reply does not prove the endpoint declaration was unchanged.
        try {
          await load();
        } catch (error) {
          output.appendLine(String(error));
        }
        busy = false;
        await vscode.commands.executeCommand(
          "setContext",
          "giteaCodespace.portsBusy",
          false,
        );
      }
    };

  context.subscriptions.push(
    {
      dispose() {
        disposed = true;
        client.dispose();
      },
    },
    changes,
    view,
    view.onDidChangeVisibility((event) => {
      if (event.visible) void refresh();
    }),
    vscode.window.onDidChangeWindowState((state) => {
      if (state.focused && view.visible) void refresh();
    }),
    vscode.commands.registerCommand("giteaCodespace.ports.refresh", () =>
      refresh(true),
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
        if (visibility)
          await client.set({ port, label: label.trim(), visibility });
      }),
    ),
    vscode.commands.registerCommand(
      "giteaCodespace.ports.remove",
      run(async (item) => {
        const endpoint = await selectEndpoint(item?.endpoint.port);
        if (!endpoint) return;
        const confirmed = await vscode.window.showWarningMessage(
          vscode.l10n.t(
            "Unpublish port {0}? This removes its Codespace publication and does not stop the application.",
            endpoint.port,
          ),
          { modal: true },
          vscode.l10n.t("Unpublish port"),
        );
        if (confirmed) await client.delete(endpoint.port);
      }),
    ),
  );
  for (const field of ["label", "visibility"] as const) {
    context.subscriptions.push(
      vscode.commands.registerCommand(
        `giteaCodespace.ports.${field === "label" ? "rename" : "visibility"}`,
        run(async (item) => {
          const endpoint = await selectEndpoint(item?.endpoint.port);
          if (!endpoint) return;
          const value =
            field === "label"
              ? await vscode.window.showInputBox({
                  title: vscode.l10n.t("Rename port"),
                  prompt: vscode.l10n.t("Port label"),
                  value: endpoint.label,
                  validateInput: validateLabel,
                })
              : await selectVisibility(endpoint.visibility);
          if (value === undefined) return;
          // Preserve fields that may have changed while the input was open.
          const current = await selectEndpoint(endpoint.port);
          if (current)
            await client.set(
              field === "label"
                ? { ...current, label: value.trim() }
                : { ...current, visibility: value as EndpointVisibility },
            );
        }),
      ),
    );
  }
  void refresh();
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
  const choices = (["authenticated", "public"] as const).map((value) => ({
    label: visibilityLabel(value),
    value,
    description: value === current ? vscode.l10n.t("Current") : undefined,
  }));
  choices.sort(
    (a, b) => Number(b.value === current) - Number(a.value === current),
  );
  return (
    await vscode.window.showQuickPick(choices, {
      title: vscode.l10n.t("Change visibility"),
    })
  )?.value;
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
