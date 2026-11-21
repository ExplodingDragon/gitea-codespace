// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import * as vscode from "vscode";

export function showOperationError(
  output: vscode.OutputChannel,
  message: string,
  error: unknown,
): void {
  output.appendLine(
    error instanceof Error ? error.stack || error.message : String(error),
  );
  const showOutput = vscode.l10n.t("Show output");
  void vscode.window.showErrorMessage(message, showOutput).then((selected) => {
    if (selected === showOutput) output.show(true);
  });
}
