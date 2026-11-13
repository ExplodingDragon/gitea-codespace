// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

export interface SSHConnection {
  user: string;
  host: string;
  port: number;
}

export const usbSourcePattern = /^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$/;

export function readSSHConnection(
  environment: NodeJS.ProcessEnv,
): SSHConnection | null {
  const host = environment.CODESPACE_SSH_HOST ?? "";
  const user = environment.CODESPACE_SSH_USER ?? "";
  const port = Number(environment.CODESPACE_SSH_PORT);
  if (
    !/^[a-z0-9][a-z0-9.-]*$/i.test(host) ||
    !/^cs-[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i.test(user) ||
    !Number.isInteger(port) ||
    port < 1 ||
    port > 65535
  )
    return null;
  return { user, host, port };
}

export function sshCommand(connection: SSHConnection, source?: string): string {
  if (source !== undefined && !usbSourcePattern.test(source))
    throw new Error("Invalid USB source name");
  const forwarding =
    source === undefined
      ? ""
      : `-NT -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -R usbip.${source}:3240:127.0.0.1:3240 `;
  return `ssh ${forwarding}-p ${connection.port} -l ${connection.user} ${connection.host}`;
}
