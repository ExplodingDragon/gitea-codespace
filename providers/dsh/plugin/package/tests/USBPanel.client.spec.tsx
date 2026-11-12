// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import { USBPanel, type USBPanelProps } from '../src/client/USBPanel.tsx'
import { apply } from '../src/client/index.tsx'

afterEach(cleanup)

it('refreshes after an uncertain detach and keeps devices visible without forwarding', async () => {
  const status = vi.fn(async () => ({ sources: [], devices: [{ source: '', busId: '1-1', description: 'CDC ACM', port: 0, state: 'Port in Use' }] }))
  const detach = vi.fn(async () => { throw new Error('Request timed out') })
  render(<USBPanel {...({ status, detach, list: vi.fn(), attach: vi.fn(), t: (key: string) => key } as unknown as USBPanelProps)} />)
  expect(await screen.findByText('CDC ACM')).toBeDefined()
  expect(screen.getByText('disconnected')).toBeDefined()
  expect((screen.getByText('list').closest('button') as HTMLButtonElement).disabled).toBe(true)
  status.mockRejectedValueOnce(new Error('Status unavailable'))
  fireEvent.click(screen.getByText('detach'))
  await waitFor(() => { expect(status).toHaveBeenCalledTimes(2) })
  expect(detach).toHaveBeenCalledWith(0)
  expect(screen.getByText('CDC ACM')).toBeDefined()
  expect(screen.getByRole('alert').textContent).toContain('Request timed out')
})

it('keeps Ports registered when USB capability discovery is temporarily unavailable', async () => {
  const registered: string[] = []
  const disposeRemote = vi.fn()
  const disposeUI = vi.fn()
  const warning = vi.spyOn(console, 'warn').mockImplementation(() => {})
  const scope = {
    remote: {
      $mount: async () => disposeRemote,
      codespace: { usbAvailable: async () => { throw new Error('Connection interrupted') } },
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
