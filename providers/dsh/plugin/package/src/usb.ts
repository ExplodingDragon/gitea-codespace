import { z } from 'zod'
import { commandAvailable, runtimeCommand, type CommandRunner } from './cli.ts'
import { usbSourcePattern } from './connection.ts'
import type { USBList, USBStatus } from './types.ts'

const USB_CLI = '/usr/local/bin/gitea-codespace-usbip'
const busID = z.string().regex(/^[0-9]+-[0-9]+(?:\.[0-9]+)*$/u)
const port = z.number().int().min(0).max(65535)
const source = z.string().regex(usbSourcePattern)
const device = z.object({ source: source.or(z.literal('')), busId: z.string(), description: z.string() })
const listResponse = z.object({
  version: z.literal(1),
  sources: z.array(z.object({ name: source, error: z.string().optional() })
    .transform(({ name, error }) => ({ name, ...(error === undefined ? {} : { error }) }))),
  devices: z.array(device.extend({ source, busId: busID })),
})
const statusResponse = listResponse.extend({
  devices: z.array(device.extend({ port, state: z.string() })),
})

/** USB mutations and reads share one CLI boundary across all DSH entry points. */
export class USBClient {
  readonly available: boolean

  constructor(
    private readonly run: CommandRunner = runtimeCommand(USB_CLI, 35000),
    available = commandAvailable(USB_CLI),
  ) {
    this.available = available
  }

  async list(signal?: AbortSignal, name?: string): Promise<USBList> {
    const args = ['list', '--format', 'json']
    if (name) args.push('--source', source.parse(name))
    const result = listResponse.parse(JSON.parse((await this.execute(args, signal)).stdout))
    return { sources: result.sources, devices: result.devices }
  }

  async sources(signal?: AbortSignal): Promise<USBList> {
    const result = listResponse.parse(JSON.parse((await this.execute(['sources', '--format', 'json'], signal)).stdout))
    return { sources: result.sources, devices: result.devices }
  }

  async status(signal?: AbortSignal): Promise<USBStatus> {
    const result = statusResponse.parse(JSON.parse((await this.execute(['status', '--format', 'json'], signal)).stdout))
    return { sources: result.sources, devices: result.devices }
  }

  async attach(id: string, name: string | undefined, signal?: AbortSignal): Promise<void> {
    const args = ['attach', busID.parse(id)]
    if (name) args.push('--source', source.parse(name))
    await this.execute(args, signal)
  }

  async detach(value: number, signal?: AbortSignal): Promise<void> {
    await this.execute(['detach', String(port.parse(value))], signal)
  }

  private execute(args: string[], signal?: AbortSignal) {
    if (!this.available) throw new Error('USB management is unavailable in this runtime')
    return this.run(args, signal)
  }
}
