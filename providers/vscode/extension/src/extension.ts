// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import * as vscode from "vscode";
import { registerPorts } from "./ports-view.js";
import { registerUSB } from "./usb-view.js";
import { readSSHConnection, sshCommand } from "./connection.js";
import { showOperationError } from "./errors.js";

export async function activate(
  context: vscode.ExtensionContext,
): Promise<void> {
  const output = vscode.window.createOutputChannel("Gitea Codespace");
  context.subscriptions.push(
    output,
    vscode.commands.registerCommand("giteaCodespace.ssh.copy", async () => {
      const connection = readSSHConnection(process.env);
      if (!connection) {
        void vscode.window.showWarningMessage(
          vscode.l10n.t(
            "SSH connection information is unavailable. Use the Codespace page in Gitea.",
          ),
        );
        return;
      }
      try {
        await vscode.env.clipboard.writeText(sshCommand(connection));
        void vscode.window.showInformationMessage(
          vscode.l10n.t("Command copied. Run it on your local computer."),
        );
      } catch (error) {
        showOperationError(
          output,
          vscode.l10n.t("Could not copy the SSH command"),
          error,
        );
      }
    }),
  );
  registerPorts(context, output);
  try {
    await registerUSB(context, output);
  } catch (error) {
    output.appendLine(`USB initialization failed: ${String(error)}`);
  }
}
