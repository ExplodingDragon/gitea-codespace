// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import * as vscode from "vscode";
import { USBClient, type USBImportedDevice } from "./usb-client.js";
import {
  readSSHConnection,
  sshCommand,
  usbSourcePattern,
} from "./connection.js";

class USBItem extends vscode.TreeItem {
  constructor(readonly device: USBImportedDevice) {
    super(device.description, vscode.TreeItemCollapsibleState.None);
    this.description = `${device.busId || "?"} · ${vscode.l10n.t("VHCI port {0}", device.port)} · ${device.state}`;
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
  let statusReady = false;
  await vscode.commands.executeCommand(
    "setContext",
    "giteaCodespace.usbStatusReady",
    false,
  );
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
  const run = async (operation?: () => Promise<void>) => {
    if (busy) return;
    busy = true;
    await vscode.commands.executeCommand(
      "setContext",
      "giteaCodespace.usbBusy",
      true,
    );
    const errors: string[] = [];
    try {
      await operation?.();
    } catch (error) {
      errors.push(
        vscode.l10n.t(
          "USB operation failed: {0}",
          error instanceof Error ? error.message : String(error),
        ),
      );
    } finally {
      // A timeout does not prove that attach/detach failed. Read actual state.
      try {
        const status = await client.status();
        statusReady = true;
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
        statusReady = false;
        view.message = vscode.l10n.t(
          "USB status is out of date. Refresh to continue.",
        );
        errors.push(
          vscode.l10n.t(
            "USB status refresh failed: {0}",
            error instanceof Error ? error.message : String(error),
          ),
        );
      }
      await vscode.commands.executeCommand(
        "setContext",
        "giteaCodespace.usbStatusReady",
        statusReady,
      );
      busy = false;
      await vscode.commands.executeCommand(
        "setContext",
        "giteaCodespace.usbBusy",
        false,
      );
      if (errors.length > 0) {
        const detail = errors.join("\n");
        output.appendLine(detail);
        void vscode.window.showErrorMessage(detail);
      }
    }
  };
  context.subscriptions.push(
    changes,
    view,
    view.onDidChangeVisibility((event) => {
      if (event.visible) void run();
    }),
    vscode.commands.registerCommand("giteaCodespace.usb.refresh", () => run()),
    vscode.commands.registerCommand(
      "giteaCodespace.usb.copyTunnel",
      async () => {
        const connection = readSSHConnection(process.env);
        if (!connection) {
          void vscode.window.showWarningMessage(
            vscode.l10n.t(
              "SSH connection information is unavailable. Use the Codespace page in Gitea.",
            ),
          );
          return;
        }
        const source = await vscode.window.showInputBox({
          title: vscode.l10n.t("Copy USB tunnel command"),
          prompt: vscode.l10n.t("USB source name"),
          value: "workstation",
          validateInput: (value) =>
            usbSourcePattern.test(value)
              ? undefined
              : vscode.l10n.t(
                  "Use 1-32 lowercase letters, digits or hyphens, starting and ending with a letter or digit.",
                ),
        });
        if (source === undefined) return;
        await vscode.env.clipboard.writeText(sshCommand(connection, source));
        void vscode.window.showInformationMessage(
          vscode.l10n.t("Command copied. Run it on your local computer."),
        );
      },
    ),
    vscode.commands.registerCommand("giteaCodespace.usb.attach", () => {
      if (!statusReady) return;
      return run(async () => {
        const { devices, sources } = await client.list();
        const status = await client.status();
        const candidates = devices.filter(
          (device) =>
            status.sources.some((source) => source.name === device.source) &&
            !status.devices.some(
              (imported) =>
                imported.source === device.source &&
                imported.busId === device.busId,
            ),
        );
        const errors = sources
          .filter((source) => source.error)
          .map((source) => `${source.name}: ${source.error}`);
        if (errors.length > 0)
          void vscode.window.showWarningMessage(errors.join("\n"));
        if (candidates.length === 0) {
          if (errors.length === 0)
            void vscode.window.showInformationMessage(
              vscode.l10n.t("No USB devices to attach"),
            );
          return;
        }
        const selected = await vscode.window.showQuickPick(
          candidates.map((device) => ({
            label: device.description,
            description: `${device.source} / ${device.busId}`,
            device,
          })),
          { title: vscode.l10n.t("Attach USB device") },
        );
        if (selected)
          await client.attach(selected.device.busId, selected.device.source);
      });
    }),
    vscode.commands.registerCommand(
      "giteaCodespace.usb.detach",
      (item?: USBItem) => {
        if (!statusReady) return;
        return run(async () => {
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
            vscode.l10n.t(
              "Detach USB device {0} from {1} (VHCI port {2})? This interrupts applications using it. Unmount disks first.",
              device.description,
              device.source || vscode.l10n.t("Unidentified source"),
              device.port,
            ),
            { modal: true },
            vscode.l10n.t("Detach USB device"),
          );
          if (confirmed) await client.detach(device.port);
        });
      },
    ),
  );
  await vscode.commands.executeCommand(
    "setContext",
    "giteaCodespace.usbAvailable",
    true,
  );
  await run();
}
