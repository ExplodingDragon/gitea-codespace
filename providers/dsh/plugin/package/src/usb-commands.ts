import type { Context } from '@deepseek-ai/cordis'
import type { CommandResult } from '@deepseek-ai/dsh-commands'
import { defineTool, type ToolDefinition } from '@deepseek-ai/dsh-tools'
import type { USBClient } from './usb.ts'

const usage = 'Usage: /usb [sources | list [--source name] | status | attach <bus-id> [--source name] | detach <port>]'

export async function executeUSBCommand(client: USBClient, input: string, signal?: AbortSignal): Promise<CommandResult> {
  try {
    const args = input.trim().split(/\s+/u)
    const action = args[0] || 'status'
    if (args.length === 1 && (action === 'list' || action === 'status' || action === 'sources')) {
      return { kind: 'success', text: JSON.stringify(await client[action](signal), null, 2) }
    }
    if (args.length === 3 && action === 'list' && args[1] === '--source') {
      return { kind: 'success', text: JSON.stringify(await client.list(signal, args[2]), null, 2) }
    }
    if (action === 'attach' && (args.length === 2 || (args.length === 4 && args[2] === '--source'))) {
      await client.attach(args[1]!, args[3], signal)
      return { kind: 'success', text: `Attached USB device ${args[1]}.` }
    }
    if (args.length === 2 && action === 'detach' && /^\d+$/u.test(args[1]!)) {
      await client.detach(Number(args[1]), signal)
      return { kind: 'success', text: `Detached USB port ${args[1]}.` }
    }
    throw new Error(usage)
  } catch (error) {
    return { kind: 'error', text: error instanceof Error ? error.message : String(error) }
  }
}

const statusSchema = {
  type: 'object', additionalProperties: false,
  properties: {
    sources: { type: 'array', required: true, items: { type: 'object', additionalProperties: false, properties: { name: { type: 'string', required: true }, error: { type: 'string' } } } },
    devices: {
      type: 'array', required: true,
      items: {
        type: 'object', additionalProperties: false,
        properties: {
          busId: { type: 'string', required: true }, description: { type: 'string', required: true },
          source: { type: 'string', required: true },
          port: { type: 'integer' }, state: { type: 'string' },
        },
      },
    },
  },
} as const

/** Supported runtimes contribute all direct and model USB actions together. */
export function registerUSBCommands(ctx: Context, client: USBClient): void {
  if (!client.available) return
  ctx.commands.register({
    name: 'usb', description: 'Manage USB devices through named SSH reverse sources',
    input: { hint: '[sources | list [--source name] | status | attach <bus-id> [--source name] | detach <port>]' },
    handler: invocation => executeUSBCommand(client, invocation.rawInput, invocation.signal),
  })
  const tools: ToolDefinition[] = [
    ...(['sources', 'list', 'status'] as const).map(action => defineTool({
      name: `codespace_usb_${action}`,
      description: action === 'list'
        ? 'List exported USB devices grouped by registered SSH source, including individual source errors.'
        : action === 'sources' ? 'List registered SSH USB sources. Registration does not indicate remote usbipd health.'
        : 'Read imported USB devices with their source names and VHCI port numbers.',
      parameters: {},
      output: { schema: statusSchema, render: (_args, result) => [{ type: 'text', text: JSON.stringify(result) }] },
      isConcurrencySafe: () => true,
      execute: (_args, exec) => client[action](exec.signal),
    })),
    defineTool({
      name: 'codespace_usb_attach', description: 'Attach an exported USB device by source and bus ID. Query list first; after a timeout query status to determine the outcome.',
      parameters: { busId: { type: 'string', required: true, description: 'Remote USB bus ID from codespace_usb_list.' }, source: { type: 'string', required: true, description: 'Registered source name from codespace_usb_list.' } },
      output: { schema: { type: 'string' }, render: (_args, result) => [{ type: 'text', text: result }] },
      execute: async (args, exec) => { await client.attach(args.busId, args.source, exec.signal); return `Attached USB device ${args.source}/${args.busId}.` },
    }),
    defineTool({
      name: 'codespace_usb_detach', description: 'Detach an imported USB device by its VHCI port from status. This disconnects applications using it. Query status after a timeout instead of retrying.',
      parameters: { port: { type: 'integer', required: true, description: 'VHCI port from codespace_usb_status, including zero.' } },
      output: { schema: { type: 'string' }, render: (_args, result) => [{ type: 'text', text: result }] },
      execute: async (args, exec) => { await client.detach(args.port, exec.signal); return `Detached USB port ${args.port}.` },
    }),
  ]
  for (const tool of tools) ctx.tools.register(tool)
}
