import type { Context } from '@deepseek-ai/cordis'
import type { CommandResult } from '@deepseek-ai/dsh-commands'
import { defineTool, type ToolDefinition } from '@deepseek-ai/dsh-tools'
import type { USBClient } from './usb.ts'

const usage = 'Usage: /usb [sources | list [--source name] | status | attach <bus-id> [--source name] | detach <port>]'

export async function executeUSBCommand(client: USBClient, input: string, signal?: AbortSignal): Promise<CommandResult> {
  try {
    const args = input.trim().split(/\s+/u)
    const [command, value = '', option, source] = args
    const action = command || 'status'
    if (args.length === 1 && (action === 'list' || action === 'status' || action === 'sources')) {
      return { kind: 'success', text: JSON.stringify(await client[action](signal), null, 2) }
    }
    if (args.length === 3 && action === 'list' && value === '--source') {
      return { kind: 'success', text: JSON.stringify(await client.list(signal, option), null, 2) }
    }
    if (action === 'attach' && (args.length === 2 || (args.length === 4 && option === '--source'))) {
      await client.attach(value, source, signal)
      return { kind: 'success', text: `USB/IP import completed for ${source ? `${source}/` : ''}${value}. Device enumeration and application setup may still be pending.` }
    }
    if (args.length === 2 && action === 'detach' && /^\d+$/u.test(value)) {
      await client.detach(Number(value), signal)
      return { kind: 'success', text: `Detached USB port ${value}.` }
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
    ...(['sources', 'status'] as const).map(action => defineTool({
      name: `codespace_usb_${action}`,
      description: action === 'sources' ? 'List registered SSH USB sources. Registration does not indicate remote usbipd health.'
        : 'Read imported USB devices with their source names and VHCI port numbers.',
      parameters: {},
      output: { schema: statusSchema, render: (_args, result) => [{ type: 'text', text: JSON.stringify(result) }] },
      isConcurrencySafe: () => true,
      execute: (_args, exec) => client[action](exec.signal),
    })),
    defineTool({
      name: 'codespace_usb_list', description: 'Query remote exported USB devices, optionally from one SSH source. Check source errors separately from empty device lists; a listed device is not reserved for this Codespace.',
      parameters: { source: { type: 'string', description: 'Optional registered SSH source name.' } },
      output: { schema: statusSchema, render: (_args, result) => [{ type: 'text', text: JSON.stringify(result) }] },
      isConcurrencySafe: () => true,
      execute: (args, exec) => client.list(exec.signal, args.source),
    }),
    defineTool({
      name: 'codespace_usb_attach', description: 'Attach an exported USB device by source and bus ID. Query list first; after a timeout query status to determine the outcome.',
      parameters: { busId: { type: 'string', required: true, description: 'Remote USB bus ID from codespace_usb_list.' }, source: { type: 'string', required: true, description: 'Registered source name from codespace_usb_list.' } },
      output: { schema: { type: 'string' }, render: (_args, result) => [{ type: 'text', text: result }] },
      execute: async (args, exec) => { await client.attach(args.busId, args.source, exec.signal); return `USB/IP import completed for ${args.source}/${args.busId}. Device enumeration and application setup may still be pending.` },
    }),
    defineTool({
      name: 'codespace_usb_detach', description: 'Detach an imported USB device by its VHCI port from status. This disconnects applications using it; unmount disks first. Query status after a timeout instead of retrying.',
      parameters: { port: { type: 'integer', required: true, description: 'VHCI port from codespace_usb_status, including zero.' } },
      output: { schema: { type: 'string' }, render: (_args, result) => [{ type: 'text', text: result }] },
      execute: async (args, exec) => { await client.detach(args.port, exec.signal); return `Detached USB port ${args.port}.` },
    }),
  ]
  for (const tool of tools) ctx.tools.register(tool)
}
