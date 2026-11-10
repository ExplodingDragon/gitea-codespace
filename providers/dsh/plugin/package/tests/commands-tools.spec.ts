import { describe, expect, it, vi } from 'vitest'
import { executePortsCommand, parsePortsCommand } from '../src/commands.ts'
import { EndpointClient, type EndpointCommandRunner } from '../src/endpoint.ts'
import { endpointTools } from '../src/tools.ts'

function fixture(): { client: EndpointClient; runner: ReturnType<typeof vi.fn<EndpointCommandRunner>> } {
  const runner = vi.fn<EndpointCommandRunner>(async args => ({
    stdout: args[0] === 'list'
      ? JSON.stringify({ version: 1, endpoints: [{ port: 3000, label: 'App', visibility: 'authenticated' }] })
      : '',
    stderr: '',
  }))
  return { client: new EndpointClient(runner), runner }
}

describe('Codespace DSH commands and tools', () => {
  it('parses and executes the complete slash-command grammar', async () => {
    expect(parsePortsCommand('')).toEqual({ kind: 'list' })
    expect(parsePortsCommand(' publish 8080 public Documentation ')).toEqual({
      kind: 'publish', endpoint: { port: 8080, visibility: 'public', label: 'Documentation' },
    })
    expect(parsePortsCommand('remove 8080')).toEqual({ kind: 'remove', port: 8080 })
    expect(() => parsePortsCommand('publish 0 public Invalid')).toThrow(/Port/)

    const { client, runner } = fixture()
    await expect(executePortsCommand(client, 'list')).resolves.toEqual({
      kind: 'success', text: '3000 (authenticated): App',
    })
    await expect(executePortsCommand(client, 'publish 8080 public Docs')).resolves.toEqual({
      kind: 'success', text: 'Published 8080 (public): Docs.',
    })
    await expect(executePortsCommand(client, 'remove 8080')).resolves.toEqual({
      kind: 'success', text: 'Unpublished port 8080.',
    })
    expect(runner).toHaveBeenCalledTimes(3)
  })

  it('registers three structured model operations over the same client', async () => {
    const { client, runner } = fixture()
    const tools = endpointTools(client)
    expect(tools.map(tool => tool.name)).toEqual([
      'codespace_list_ports', 'codespace_publish_port', 'codespace_unpublish_port',
    ])
    const exec = { signal: new AbortController().signal } as never
    await expect(tools[0]?.execute({}, exec)).resolves.toEqual([
      { port: 3000, label: 'App', visibility: 'authenticated' },
    ])
    await expect(tools[1]?.execute({ port: 8080, label: 'Docs' }, exec)).resolves.toEqual({
      port: 8080, label: 'Docs', visibility: 'authenticated',
    })
    await expect(tools[2]?.execute({ port: 8080 }, exec)).resolves.toEqual({ port: 8080 })
    expect(runner.mock.calls[1]?.[0]).toEqual(['set', '8080', '--label', 'Docs'])
  })
})
