import type { Context } from '@deepseek-ai/cordis'
import type { CommandResult } from '@deepseek-ai/dsh-commands'
import type { EndpointClient } from './endpoint.ts'
import type { Endpoint, EndpointVisibility } from './types.ts'

const USAGE = 'Usage: /ports [list | publish <port> <authenticated|public> <label> | remove <port>]'

/** Parsed `/ports` operation. */
export type PortsCommand =
  | { kind: 'list' }
  | { kind: 'publish'; endpoint: Endpoint }
  | { kind: 'remove'; port: number }

/** Parse the complete argument tail of `/ports`. */
export function parsePortsCommand(rawInput: string): PortsCommand {
  const input = rawInput.trim()
  if (input === '' || input === 'list') return { kind: 'list' }

  const remove = /^remove\s+(\d+)$/u.exec(input)
  if (remove !== null) return { kind: 'remove', port: parsePort(remove[1]) }

  const publish = /^publish\s+(\d+)\s+(authenticated|public)\s+(.+)$/u.exec(input)
  if (publish !== null) {
    return {
      kind: 'publish',
      endpoint: {
        port: parsePort(publish[1]),
        visibility: publish[2] as EndpointVisibility,
        label: publish[3]?.trim() ?? '',
      },
    }
  }
  throw new Error(USAGE)
}

/** Run one parsed command through the shared endpoint client. */
export async function executePortsCommand(
  endpointClient: EndpointClient,
  rawInput: string,
  signal?: AbortSignal,
): Promise<CommandResult> {
  try {
    const command = parsePortsCommand(rawInput)
    switch (command.kind) {
      case 'list':
        return { kind: 'success', text: formatEndpoints(await endpointClient.list(signal)) }
      case 'publish': {
        const endpoint = await endpointClient.publish(command.endpoint, signal)
        return { kind: 'success', text: `Published ${formatEndpoint(endpoint)}.` }
      }
      case 'remove':
        await endpointClient.unpublish(command.port, signal)
        return { kind: 'success', text: `Unpublished port ${command.port}.` }
    }
  } catch (error: unknown) {
    return { kind: 'error', text: error instanceof Error ? error.message : String(error) }
  }
}

/** Register the human-facing `/ports` command. */
export function registerPortsCommand(ctx: Context, endpointClient: EndpointClient): void {
  ctx.commands.register({
    name: 'ports',
    description: 'List and manage ports published from this Codespace',
    input: { hint: '[list | publish <port> <authenticated|public> <label> | remove <port>]' },
    handler: invocation => executePortsCommand(endpointClient, invocation.rawInput, invocation.signal),
  })
}

/** Render the endpoint set for direct human and model output. */
export function formatEndpoints(endpoints: readonly Endpoint[]): string {
  if (endpoints.length === 0) return 'No ports are published.'
  return endpoints.map(formatEndpoint).join('\n')
}

function formatEndpoint(endpoint: Endpoint): string {
  return `${endpoint.port} (${endpoint.visibility}): ${endpoint.label}`
}

function parsePort(value: string | undefined): number {
  const port = Number(value)
  if (!Number.isSafeInteger(port) || port < 1 || port > 65535) {
    throw new Error('Port must be an integer between 1 and 65535')
  }
  return port
}
