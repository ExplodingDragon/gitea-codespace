import { describe, expect, it, vi } from 'vitest'
import type { Context } from '@deepseek-ai/cordis'
import type { ToolDefinition } from '@deepseek-ai/dsh-tools'
import { USBClient } from '../src/usb.ts'
import { executeUSBCommand, registerUSBCommands } from '../src/usb-commands.ts'
import type { CommandRunner } from '../src/cli.ts'

describe('Codespace USB commands and tools', () => {
  it('shares argv and structured status across direct commands and model tools', async () => {
    const runner = vi.fn<CommandRunner>(async () => ({ stdout: JSON.stringify({ version: 1, sources: [{ name: 'lab' }],
      devices: [{ source: 'lab', busId: '1-1', description: 'CDC ACM', port: 0, state: 'Port in Use' }],
    }), stderr: '' }))
    const client = new USBClient(runner, true)
    expect((await client.status()).devices[0]?.port).toBe(0)
    expect(await client.list()).toEqual({ sources: [{ name: 'lab' }], devices: [{ source: 'lab', busId: '1-1', description: 'CDC ACM' }] })
    expect(await executeUSBCommand(client, 'attach 1-1 --source lab')).toMatchObject({ kind: 'success', text: expect.stringContaining('USB/IP import completed for lab/1-1') })
    const commands = { register: vi.fn() }
    const tools = { register: vi.fn<(tool: ToolDefinition) => void>() }
    registerUSBCommands({ commands, tools } as unknown as Context, client)
    const detach = tools.register.mock.calls.find(([tool]) => tool.name === 'codespace_usb_detach')![0]
    const signal = new AbortController().signal
    await detach.execute({ port: 0 }, { signal } as never)
    expect(runner.mock.calls.map(call => call[0])).toEqual([
      ['status', '--format', 'json'], ['list', '--format', 'json'], ['attach', '1-1', '--source', 'lab'], ['detach', '0'],
    ])
    expect(runner.mock.calls[3]?.[1]).toBe(signal)
    const list = tools.register.mock.calls.find(([tool]) => tool.name === 'codespace_usb_list')![0]
    await list.execute({ source: 'lab' }, { signal } as never)
    expect(runner.mock.lastCall).toEqual([['list', '--format', 'json', '--source', 'lab'], signal])
    expect((await executeUSBCommand(client, 'attach 1-1 extra')).kind).toBe('error')
    runner.mockResolvedValueOnce({ stdout: JSON.stringify({ version: 1, sources: [{ name: 'lab' }],
      devices: [{ source: 'lab', busId: '1-1', description: 'CDC ACM' }],
    }), stderr: '' })
    await expect(client.status()).rejects.toThrow()
  })

  it('registers USB entry points only for an available CLI', () => {
    for (const available of [true, false]) {
      const commands = { register: vi.fn() }
      const tools = { register: vi.fn() }
      registerUSBCommands({ commands, tools } as unknown as Context, new USBClient(vi.fn(), available))
      expect(commands.register).toHaveBeenCalledTimes(available ? 1 : 0)
      expect(tools.register).toHaveBeenCalledTimes(available ? 5 : 0)
    }
  })
})
