// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import { USBPanel, type USBPanelProps } from '../src/client/USBPanel.tsx'
import { apply } from '../src/client/index.tsx'

afterEach(() => { cleanup(); vi.restoreAllMocks() })

it('refreshes after an uncertain detach and keeps devices visible without forwarding', async () => {
  const status = vi.fn(async () => ({ sources: [], devices: [{ source: '', busId: '1-1', description: 'CDC ACM', port: 0, state: 'Port in Use' }] }))
  const detach = vi.fn(async () => { throw new Error('Request timed out') })
  render(<USBPanel {...({ status, detach, list: vi.fn(), attach: vi.fn(), connection: null, connectionT: (key: string) => key, t: (key: string) => key } as unknown as USBPanelProps)} />)
  expect(await screen.findByText('CDC ACM')).toBeDefined()
  expect(screen.getByText('disconnected')).toBeDefined()
  expect((screen.getByText('list').closest('button') as HTMLButtonElement).disabled).toBe(true)
  const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
  fireEvent.click(screen.getByText('detach'))
  expect(detach).not.toHaveBeenCalled()
  expect(confirm).toHaveBeenCalledWith(expect.stringContaining('port 0'))
  confirm.mockReturnValue(true)
  status.mockRejectedValueOnce(new Error('Status unavailable'))
  fireEvent.click(screen.getByText('detach'))
  await waitFor(() => { expect(status).toHaveBeenCalledTimes(2) })
  expect(detach).toHaveBeenCalledWith(0)
  expect(screen.getByText('CDC ACM')).toBeDefined()
  expect(screen.getByRole('alert').textContent).toContain('Request timed out')
  expect(screen.getByRole('alert').textContent).toContain('Status unavailable')
  expect(screen.getByRole('status').textContent).toBe('stale')
  expect((screen.getByText('detach').closest('button') as HTMLButtonElement).disabled).toBe(true)
  fireEvent.click(screen.getByText('refresh'))
  await waitFor(() => { expect((screen.getByText('detach').closest('button') as HTMLButtonElement).disabled).toBe(false) })
  expect(screen.queryByRole('alert')).toBeNull()
})

it('refreshes remote devices and distinguishes identical bus IDs across sources', async () => {
  const device = { source: 'workstation', busId: '1-3', description: 'USB disk' }
  const status = vi.fn(async () => ({ sources: [{ name: 'workstation' }, { name: 'lab' }, { name: 'offline' }], devices: [{ ...device, port: 0, state: 'Port in Use' }] }))
  const list = vi.fn(async () => ({ sources: [{ name: 'workstation' }, { name: 'lab' }, { name: 'offline', error: 'Server unavailable' }],
    devices: [device, { ...device, source: 'lab' }],
  }))
  const attach = vi.fn(async () => {})
  render(<USBPanel {...({ status, list, attach, detach: vi.fn(), connection: null, connectionT: (key: string) => key, t: (key: string) => key } as unknown as USBPanelProps)} />)
  await waitFor(() => { expect((screen.getByText('list').closest('button') as HTMLButtonElement).disabled).toBe(false) })
  expect(list).not.toHaveBeenCalled()
  fireEvent.click(screen.getByText('list'))
  await waitFor(() => { expect((screen.getByText('attach').closest('button') as HTMLButtonElement).disabled).toBe(false) })
  expect((screen.getByText('attached').closest('button') as HTMLButtonElement).disabled).toBe(true)
  expect(screen.getByRole('alert').textContent).toContain('Server unavailable')
  fireEvent.click(screen.getByText('attach'))
  await waitFor(() => { expect(list).toHaveBeenCalledTimes(2) })
  expect(attach).toHaveBeenCalledWith('1-3', 'lab')
  list.mockRejectedValueOnce(new Error('Remote query failed'))
  fireEvent.click(screen.getByText('refresh'))
  expect(await screen.findByText('listFailed: Remote query failed')).toBeDefined()
  expect((screen.getByText('attach').closest('button') as HTMLButtonElement).disabled).toBe(true)
  status.mockResolvedValue({ sources: [{ name: 'lab' }], devices: [] })
  list.mockResolvedValue({ sources: [{ name: 'lab' }], devices: [{ ...device, source: 'lab', description: 'Replacement disk' }] })
  fireEvent.click(screen.getByText('refresh'))
  expect(await screen.findByText('Replacement disk')).toBeDefined()
  await waitFor(() => { expect((screen.getByText('attach').closest('button') as HTMLButtonElement).disabled).toBe(false) })
  expect(screen.queryByText('workstation')).toBeNull()
  expect(screen.queryByText('USB disk')).toBeNull()
  expect(screen.queryByRole('alert')).toBeNull()
})

it('keeps Ports registered when USB capability discovery is temporarily unavailable', async () => {
  const registered: string[] = []
  const disposeRemote = vi.fn()
  const disposeUI = vi.fn()
  const warning = vi.spyOn(console, 'warn').mockImplementation(() => {})
  const scope = {
    remote: {
      $mount: async () => disposeRemote,
      codespace: { sshConnection: async () => ({ ok: true, value: null }), usbAvailable: async () => { throw new Error('Connection interrupted') } },
    },
    locale: { register: () => {}, bind: () => (key: string) => key },
    slots: {
      inject: (_name: string, callback: () => void) => callback(),
      register: (slot: { key?: string; id?: string }) => { registered.push(slot.key ?? slot.id ?? '') },
    },
    effect: (callback: () => void) => callback(),
    inject: (_names: string[], callback: (context: unknown) => Promise<void>) =>
      Object.assign(Promise.resolve().then(() => callback(scope)), { dispose: disposeUI }),
  }
  try {
    const dispose = await apply(scope as unknown as Parameters<typeof apply>[0])
    expect(registered).toEqual(['codespace-ports', 'codespace-ports'])
    expect(warning).toHaveBeenCalledWith('Codespace USB capability query failed', expect.any(Error))
    expect(disposeRemote).not.toHaveBeenCalled()
    expect(disposeUI).not.toHaveBeenCalled()
    await dispose()
    expect(disposeUI).toHaveBeenCalledTimes(1)
    expect(disposeRemote).toHaveBeenCalledTimes(1)
  } finally {
    warning.mockRestore()
  }
})
