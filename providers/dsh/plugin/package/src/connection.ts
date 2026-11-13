import { z } from 'zod'
import type { SSHConnection } from './types.ts'

const connectionSchema = z.object({
  user: z.string().regex(/^cs-[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/iu),
  host: z.string().regex(/^[a-z0-9][a-z0-9.-]*$/iu),
  port: z.coerce.number().int().min(1).max(65535),
})

export const usbSourcePattern = /^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$/u

export function readSSHConnection(environment: Record<string, string | undefined>): SSHConnection | null {
  const result = connectionSchema.safeParse({
    user: environment.CODESPACE_SSH_USER, host: environment.CODESPACE_SSH_HOST, port: environment.CODESPACE_SSH_PORT,
  })
  return result.success ? result.data : null
}

export function sshCommand(connection: SSHConnection, source?: string): string {
  if (source !== undefined && !usbSourcePattern.test(source)) throw new Error('Invalid USB source name')
  const forwarding = source === undefined ? '' : `-NT -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -R usbip.${source}:3240:127.0.0.1:3240 `
  return `ssh ${forwarding}-p ${connection.port} -l ${connection.user} ${connection.host}`
}
