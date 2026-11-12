import { describe, expect, it, vi } from 'vitest'
import {
  EndpointClient,
  parseEndpointList,
} from '../src/endpoint.ts'
import type { CommandRunner } from '../src/cli.ts'

describe('Codespace endpoint client', () => {
  it('parses, validates, sorts, and rejects duplicate versioned endpoints', () => {
    expect(parseEndpointList(JSON.stringify({ version: 1, endpoints: [
      { port: 8080, label: 'Docs', visibility: 'public' },
      { port: 3000, label: 'App', visibility: 'authenticated' },
    ] }))).toEqual([
      { port: 3000, label: 'App', visibility: 'authenticated' },
      { port: 8080, label: 'Docs', visibility: 'public' },
    ])
    expect(() => parseEndpointList('{')).toThrow(/invalid JSON/)
    expect(() => parseEndpointList(JSON.stringify({ version: 2, endpoints: [] }))).toThrow(/unsupported response/)
    expect(() => parseEndpointList(JSON.stringify({ version: 1, endpoints: [
      { port: 3000, label: '', visibility: 'authenticated' },
    ] }))).toThrow(/label/)
    expect(() => parseEndpointList(JSON.stringify({ version: 1, endpoints: [
      { port: 3000, label: 'A', visibility: 'public' },
      { port: 3000, label: 'B', visibility: 'authenticated' },
    ] }))).toThrow(/duplicate port 3000/)
  })

  it('builds exact mutation arguments and forwards cancellation', async () => {
    const runner = vi.fn<CommandRunner>(async () => ({
      stdout: JSON.stringify({ version: 1, endpoints: [] }), stderr: '',
    }))
    const client = new EndpointClient(runner)
    const controller = new AbortController()
    await client.list(controller.signal)
    await client.publish({ port: 3000, label: ' App ', visibility: 'public' }, controller.signal)
    await client.unpublish(3000, controller.signal)
    expect(runner.mock.calls).toEqual([
      [['list', '--format', 'json'], controller.signal],
      [['set', '3000', '--label', 'App', '--public'], controller.signal],
      [['delete', '3000'], controller.signal],
    ])
  })

  it('rejects invalid inputs and runner failures', async () => {
    const client = new EndpointClient(async () => ({ stdout: '', stderr: '' }))
    await expect(client.publish({ port: 0, label: 'App', visibility: 'authenticated' })).rejects.toThrow(/port/)
    await expect(client.publish({ port: 3000, label: '', visibility: 'authenticated' })).rejects.toThrow(/label/)
    const failure = new Error('runtime agent unavailable')
    await expect(new EndpointClient(async () => { throw failure }).list()).rejects.toBe(failure)
  })
})
