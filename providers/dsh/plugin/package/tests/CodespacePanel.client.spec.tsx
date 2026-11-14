// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import { CodespacePanel, type CodespacePanelProps } from '../src/client/CodespacePanel.tsx'
import { apply } from '../src/client/index.tsx'

afterEach(() => { cleanup(); vi.restoreAllMocks() })

it('keeps port drafts across lazy USB tabs and hides USB when unavailable', async () => {
  const status = vi.fn(async () => ({ sources: [], devices: [] }))
  const props = {
    connection: null, t: (key: string) => key,
    portsT: (key: string) => `ports.${key}`, usbT: (key: string) => `usb.${key}`,
    ports: { list: async () => [], publish: vi.fn(), unpublish: vi.fn() },
    usb: { status, list: vi.fn(), attach: vi.fn(), detach: vi.fn() },
  } as unknown as CodespacePanelProps
  const { unmount } = render(<CodespacePanel {...props} />)
  await screen.findByText('ports.empty')
  expect(status).not.toHaveBeenCalled()
  fireEvent.click(screen.getByRole('button', { name: 'ports.publish' }))
  fireEvent.change(screen.getByLabelText('ports.label'), { target: { value: 'Draft service' } })
  fireEvent.click(screen.getByRole('tab', { name: 'usb.title' }))
  await screen.findByText('usb.disconnected')
  expect(screen.getByRole('tabpanel').getAttribute('aria-labelledby')).toBe(screen.getByRole('tab', { selected: true }).id)
  fireEvent.click(screen.getByRole('tab', { name: 'ports.title' }))
  expect(screen.getByDisplayValue('Draft service')).toBeDefined()
  unmount()
  render(<CodespacePanel {...props} usb={null} />)
  await screen.findByText('ports.empty')
  expect(screen.queryAllByRole('tab')).toHaveLength(0)
  expect(screen.getByRole('button', { name: 'help' })).toBeDefined()
})

it('registers one management entry even when optional capability discovery fails', async () => {
  const registered: { key?: string; id?: string; inject?: () => { usb: unknown } }[] = []
  const disposeRemote = vi.fn()
  const disposeUI = vi.fn()
  vi.spyOn(console, 'warn').mockImplementation(() => {})
  const scope = {
    remote: {
      $mount: async () => disposeRemote,
      codespace: { sshConnection: async () => ({ ok: true, value: null }), usbAvailable: async () => { throw new Error('Connection interrupted') } },
    },
    locale: { register: () => {}, bind: () => (key: string) => key },
    slots: {
      inject: (_name: string, callback: () => void) => { callback() },
      register: (slot: typeof registered[number]) => { registered.push(slot) },
    },
    effect: (callback: () => void) => { callback() },
    inject: (_names: string[], callback: (context: unknown) => Promise<void>) =>
      Object.assign(Promise.resolve().then(() => callback(scope)), { dispose: disposeUI }),
  }
  const dispose = await apply(scope as unknown as Parameters<typeof apply>[0])
  expect(registered.map(slot => slot.key ?? slot.id)).toEqual(['codespace', 'codespace'])
  expect(registered[0]?.inject?.().usb).toBeNull()
  await dispose()
  expect(disposeUI).toHaveBeenCalledTimes(1)
  expect(disposeRemote).toHaveBeenCalledTimes(1)
})
