// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import * as vscode from "vscode";
import { USBClient, type USBImportedDevice } from "./usb-client.js";

class USBItem extends vscode.TreeItem {
  constructor(readonly device: USBImportedDevice) {
    super(device.description, vscode.TreeItemCollapsibleState.None);
    this.description = `${device.busId || "?"} · ${device.state}`;
    this.tooltip = vscode.l10n.t(
      "USB port {0}: {1}",
      device.port,
      device.description,
    );
    this.contextValue = "codespaceUSBDevice";
    this.iconPath = new vscode.ThemeIcon("plug");
  }
}

class USBSourceItem extends vscode.TreeItem {
  constructor(
    name: string,
    readonly children: USBItem[],
  ) {
    super(
      name || vscode.l10n.t("Unidentified source"),
      vscode.TreeItemCollapsibleState.Expanded,
    );
    this.iconPath = new vscode.ThemeIcon("remote");
  }
}

export async function registerUSB(
  context: vscode.ExtensionContext,
  output: vscode.OutputChannel,
): Promise<void> {
  const client = new USBClient((message) => output.appendLine(message));
  context.subscriptions.push(client);
  await vscode.commands.executeCommand(
    "setContext",
    "giteaCodespace.usbAvailable",
    false,
  );
  if (!(await client.available())) return;

  const changes = new vscode.EventEmitter<void>();
  let items: USBSourceItem[] = [];
  let busy = false;
  const view = vscode.window.createTreeView<USBItem | USBSourceItem>(
    "giteaCodespace.usb",
    {
      treeDataProvider: {
        onDidChangeTreeData: changes.event,
        getTreeItem: (item) => item,
        getChildren: (item) =>
          item instanceof USBSourceItem ? item.children : item ? [] : items,
      },
    },
  );
  const reportError = (error: unknown) => {
    const detail = error instanceof Error ? error.message : String(error);
    output.appendLine(detail);
    void vscode.window.showErrorMessage(
      vscode.l10n.t("USB operation failed: {0}", detail),
    );
  };
  const run = async (operation?: () => Promise<void>) => {
    if (busy) return;
    busy = true;
    await vscode.commands.executeCommand(
      "setContext",
      "giteaCodespace.usbBusy",
      true,
    );
    try {
      await operation?.();
    } catch (error) {
      reportError(error);
    } finally {
      // A timeout does not prove that attach/detach failed. Read actual state.
      try {
        const status = await client.status();
        const sources = new Set([
          ...status.sources.map((source) => source.name),
          ...status.devices.map((device) => device.source),
        ]);
        items = [...sources].map(
          (source) =>
            new USBSourceItem(
              source,
              status.devices
                .filter((device) => device.source === source)
                .map((device) => new USBItem(device)),
            ),
        );
        view.message =
          status.sources.length > 0
            ? vscode.l10n.t("SSH USB forwarding is active")
            : vscode.l10n.t("SSH USB forwarding is not connected");
        changes.fire();
      } catch (error) {
        reportError(error);
      }
      busy = false;
      await vscode.commands.executeCommand(
        "setContext",
        "giteaCodespace.usbBusy",
        false,
      );
    }
  };
  context.subscriptions.push(
    changes,
    view,
    view.onDidChangeVisibility((event) => {
      if (event.visible) void run();
    }),
    vscode.commands.registerCommand("giteaCodespace.usb.refresh", () => run()),
    vscode.commands.registerCommand("giteaCodespace.usb.attach", () =>
      run(async () => {
        const { devices, sources } = await client.list();
        const errors = sources
          .filter((source) => source.error)
          .map((source) => `${source.name}: ${source.error}`);
        if (errors.length > 0)
          void vscode.window.showWarningMessage(errors.join("\n"));
        if (devices.length === 0) {
          void vscode.window.showInformationMessage(
            vscode.l10n.t("No exported USB devices"),
          );
          return;
        }
        const selected = await vscode.window.showQuickPick(
          devices.map((device) => ({
            label: device.description,
            description: `${device.source} / ${device.busId}`,
            device,
          })),
          { title: vscode.l10n.t("Attach USB device") },
        );
        if (selected)
          await client.attach(selected.device.busId, selected.device.source);
      }),
    ),
    vscode.commands.registerCommand(
      "giteaCodespace.usb.detach",
      (item?: USBItem) =>
        run(async () => {
          let device = item?.device;
          if (!device) {
            const status = await client.status();
            device = (
              await vscode.window.showQuickPick(
                status.devices.map((value) => ({
                  label: value.description,
                  description: `${value.source} / ${value.port}`,
                  device: value,
                })),
                { title: vscode.l10n.t("Detach USB device") },
              )
            )?.device;
          }
          if (!device) return;
          const confirmed = await vscode.window.showWarningMessage(
            vscode.l10n.t("Detach USB device {0}?", device.description),
            { modal: true },
            vscode.l10n.t("Detach USB device"),
          );
          if (confirmed) await client.detach(device.port);
        }),
    ),
  );
  await vscode.commands.executeCommand(
    "setContext",
    "giteaCodespace.usbAvailable",
    true,
  );
  if (view.visible) await run();
}
