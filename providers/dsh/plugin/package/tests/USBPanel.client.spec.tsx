// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import { USBPanel, type USBPanelProps } from '../src/client/USBPanel.tsx'
import type { USBList, USBStatus } from '../src/types.ts'

afterEach(() => { cleanup(); vi.useRealTimers(); vi.restoreAllMocks() })

const device = { source: 'workstation', busId: '1-3', description: 'USB disk' }
const imported = { ...device, port: 0, state: 'Port in Use' }
const t = (key: string) => key

it('refreshes only while visible and active, waits for slow reads and cancels on exit', async () => {
  vi.useFakeTimers()
  const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible')
  const slow = Promise.withResolvers<USBList>()
  const status = vi.fn(async () => ({ sources: [{ name: 'workstation' }], devices: [] }))
  const list = vi.fn(async (_signal?: AbortSignal) => slow.promise)
  const props = { status, list, attach: vi.fn(), detach: vi.fn(), active: true, t } as USBPanelProps
  const { rerender, unmount } = render(<USBPanel {...props} />)
  await act(async () => {})
  await act(async () => { await vi.advanceTimersByTimeAsync(30000) })
  expect(list).toHaveBeenCalledTimes(1)
  await act(async () => { slow.resolve({ sources: [{ name: 'workstation' }], devices: [device] }) })
  expect(screen.getByText('USB disk')).toBeDefined()
  await act(async () => { await vi.advanceTimersByTimeAsync(10000) })
  expect(list).toHaveBeenCalledTimes(2)
  visibility.mockReturnValue('hidden')
  fireEvent(document, new Event('visibilitychange'))
  expect(list.mock.calls[1]?.[0]?.aborted).toBe(true)
  await act(async () => { await vi.advanceTimersByTimeAsync(30000) })
  expect(status).toHaveBeenCalledTimes(2)
  visibility.mockReturnValue('visible')
  fireEvent(document, new Event('visibilitychange'))
  await act(async () => {})
  expect(status).toHaveBeenCalledTimes(3)
  rerender(<USBPanel {...props} active={false} />)
  await act(async () => { await vi.advanceTimersByTimeAsync(30000) })
  expect(status).toHaveBeenCalledTimes(3)
  rerender(<USBPanel {...props} />)
  await act(async () => {})
  expect(status).toHaveBeenCalledTimes(4)
  unmount()
  expect(list.mock.calls.at(-1)?.[0]?.aborted).toBe(true)
  await vi.advanceTimersByTimeAsync(30000)
  expect(list).toHaveBeenCalledTimes(4)
})

it('merges devices per source and retains usable local devices when sources fail or disconnect', async () => {
  const status = vi.fn<() => Promise<USBStatus>>(async () => ({ sources: [{ name: 'workstation' }, { name: 'lab' }], devices: [imported] }))
  const list = vi.fn<() => Promise<USBList>>(async () => ({ sources: [{ name: 'workstation' }, { name: 'lab' }], devices: [device, { ...device, source: 'lab' }] }))
  const detach = vi.fn(async () => { throw new Error('Request timed out') })
  render(<USBPanel status={status} list={list} attach={vi.fn()} detach={detach} active t={t} />)
  await waitFor(() => { expect(screen.getAllByText('USB disk')).toHaveLength(2) })
  expect(within(screen.getByRole('region', { name: 'workstation' })).getAllByText('USB disk')).toHaveLength(1)
  expect(within(screen.getByRole('region', { name: 'lab' })).getByRole('button', { name: 'attach' }).hasAttribute('disabled')).toBe(false)

  list.mockResolvedValueOnce({ sources: [{ name: 'workstation' }, { name: 'lab', error: 'Server unavailable' }], devices: [device] })
  fireEvent.click(screen.getByRole('button', { name: 'refresh' }))
  expect((await screen.findByRole('alert')).textContent).toContain('Server unavailable')
  expect(within(screen.getByRole('region', { name: 'lab' })).getByRole('button', { name: 'attach' }).hasAttribute('disabled')).toBe(true)
  expect(screen.getAllByText('USB disk')).toHaveLength(2)
  expect(screen.getByRole('button', { name: 'detach' }).hasAttribute('disabled')).toBe(false)

  status.mockResolvedValue({ sources: [], devices: [imported] })
  fireEvent.click(screen.getByRole('button', { name: 'refresh' }))
  await screen.findByText('sourceDisconnected')
  expect(screen.getAllByText('USB disk')).toHaveLength(1)
  expect(list).toHaveBeenCalledTimes(2)
  fireEvent.click(screen.getByRole('button', { name: 'detach' }))
  expect(screen.getByRole('dialog').textContent).toContain('port 0')
  fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'cancel' }))
  expect(detach).not.toHaveBeenCalled()
  fireEvent.click(screen.getByRole('button', { name: 'detach' }))
  status.mockRejectedValueOnce(new Error('Status unavailable'))
  fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'detach' }))
  await waitFor(() => { expect(screen.getByRole('alert').textContent).toContain('Status unavailable') })
  expect(detach).toHaveBeenCalledWith(0)
  expect(screen.getByRole('alert').textContent).toContain('Request timed out')
  expect(screen.getByRole('button', { name: 'detach' }).hasAttribute('disabled')).toBe(true)
})

it('prioritizes a mutation over slow refresh and ignores its late result', async () => {
  const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible')
  const status = vi.fn<() => Promise<USBStatus>>(async () => ({ sources: [{ name: 'workstation' }], devices: [] }))
  const list = vi.fn(async (_signal?: AbortSignal) => ({ sources: [{ name: 'workstation' }], devices: [device] }))
  const attach = vi.fn(async () => {
    status.mockResolvedValue({ sources: [{ name: 'workstation' }], devices: [imported] })
    throw new Error('Operation result uncertain')
  })
  const slow = Promise.withResolvers<USBList>()
  render(<USBPanel status={status} list={list} attach={attach} detach={vi.fn()} active t={t} />)
  await screen.findByText('USB disk')
  list.mockImplementationOnce(() => slow.promise)
  fireEvent.click(screen.getByRole('button', { name: 'refresh' }))
  await waitFor(() => { expect(list).toHaveBeenCalledTimes(2) })
  const oldSignal = list.mock.calls[1]?.[0]
  fireEvent.click(screen.getByRole('button', { name: 'attach' }))
  expect(oldSignal?.aborted).toBe(true)
  await screen.findByRole('button', { name: 'detach' })
  expect(attach).toHaveBeenCalledWith('1-3', 'workstation')
  await act(async () => { slow.resolve({ sources: [{ name: 'workstation' }], devices: [{ ...device, busId: '2-9', description: 'Old result' }] }) })
  expect(screen.queryByText('Old result')).toBeNull()
  expect(screen.getByRole('alert').textContent).toContain('Operation result uncertain')
  visibility.mockReturnValue('hidden')
  fireEvent(document, new Event('visibilitychange'))
  visibility.mockReturnValue('visible')
  fireEvent(document, new Event('visibilitychange'))
  await waitFor(() => { expect(list).toHaveBeenCalledTimes(4) })
  expect(screen.getByRole('alert').textContent).toContain('Operation result uncertain')
})
