import type { Context } from '@deepseek-ai/cordis'
import { Remote, TypertRemoteService } from '@deepseek-ai/dsh-typert-protocol'
import type {} from '@deepseek-ai/dsh-commands'
import type {} from '@deepseek-ai/dsh-tools'
import { registerPortsCommand } from './commands.ts'
import { EndpointClient } from './endpoint.ts'
import { endpointTools } from './tools.ts'
import { USBClient } from './usb.ts'
import { registerUSBCommands } from './usb-commands.ts'
import type { Endpoint, SSHConnection, USBList, USBStatus } from './types.ts'
import { readSSHConnection } from './connection.ts'

export type * from './types.ts'

declare module '@deepseek-ai/cordis' {
  interface Context {
    /** Codespace runtime operations. */
    codespaceEndpoints: CodespaceEndpointService
  }
}

/** Host runtime service shared by the browser, direct commands, and model tools. */
export class CodespaceEndpointService extends TypertRemoteService {
  static inject = ['commands', 'tools']

  private readonly endpoints = new EndpointClient()
  private readonly usb = new USBClient()

  /** Install the service and its non-browser entry points. */
  constructor(ctx: Context) {
    super(ctx, 'codespaceEndpoints', { namespace: 'codespace' })
    registerPortsCommand(ctx, this.endpoints)
    for (const tool of endpointTools(this.endpoints)) ctx.tools.register(tool)
    registerUSBCommands(ctx, this.usb)
  }

  /** List the current runtime endpoint declarations. */
  @Remote
  list(): Promise<Endpoint[]> {
    return this.endpoints.list(this.ctx.invocation?.signal)
  }

  /** Add or replace a runtime endpoint, then return the authoritative set. */
  @Remote
  async publish(endpoint: Endpoint): Promise<Endpoint[]> {
    const signal = this.ctx.invocation?.signal
    await this.endpoints.publish(endpoint, signal)
    return this.endpoints.list(signal)
  }

  /** Remove a runtime endpoint, then return the authoritative set. */
  @Remote
  async unpublish(port: number): Promise<Endpoint[]> {
    const signal = this.ctx.invocation?.signal
    await this.endpoints.unpublish(port, signal)
    return this.endpoints.list(signal)
  }

  @Remote
  sshConnection(): SSHConnection | null {
    return readSSHConnection(process.env)
  }

  /** USB capability is fixed when the Runtime starts; tunnel state is separate. */
  @Remote
  usbAvailable(): boolean {
    return this.usb.available
  }

  @Remote
  usbList(): Promise<USBList> {
    return this.usb.list(this.ctx.invocation?.signal)
  }

  @Remote
  usbStatus(): Promise<USBStatus> {
    return this.usb.status(this.ctx.invocation?.signal)
  }

  @Remote
  usbAttach(busId: string, source: string): Promise<void> {
    return this.usb.attach(busId, source, this.ctx.invocation?.signal)
  }

  @Remote
  usbDetach(port: number): Promise<void> {
    return this.usb.detach(port, this.ctx.invocation?.signal)
  }
}

export default CodespaceEndpointService
