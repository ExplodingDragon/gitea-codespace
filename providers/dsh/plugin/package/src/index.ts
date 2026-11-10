import type { Context } from '@deepseek-ai/cordis'
import { Remote, TypertRemoteService } from '@deepseek-ai/dsh-typert-protocol'
import type {} from '@deepseek-ai/dsh-commands'
import type {} from '@deepseek-ai/dsh-tools'
import { registerPortsCommand } from './commands.ts'
import { EndpointClient } from './endpoint.ts'
import { endpointTools } from './tools.ts'
import type { Endpoint } from './types.ts'

export type * from './types.ts'

declare module '@deepseek-ai/cordis' {
  interface Context {
    /** Codespace runtime endpoint operations. */
    codespaceEndpoints: CodespaceEndpointService
  }
}

/** Host endpoint service shared by the browser, direct commands, and model tools. */
export class CodespaceEndpointService extends TypertRemoteService {
  static inject = ['commands', 'tools']

  private readonly endpoints = new EndpointClient()

  /** Install the service and its non-browser entry points. */
  constructor(ctx: Context) {
    super(ctx, 'codespaceEndpoints', { namespace: 'codespace' })
    registerPortsCommand(ctx, this.endpoints)
    for (const tool of endpointTools(this.endpoints)) ctx.tools.register(tool)
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
}

export default CodespaceEndpointService
