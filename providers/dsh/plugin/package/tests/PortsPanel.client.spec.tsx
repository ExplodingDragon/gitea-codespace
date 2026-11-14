// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { PortsPanel } from '../src/client/PortsPanel.tsx'
import type { Endpoint } from '../src/types.ts'

afterEach(cleanup)

describe('Codespace Ports panel', () => {
  it('serializes refresh and mutations while preserving the editor draft', async () => {
    const endpoint: Endpoint = { port: 3000, label: 'Application', visibility: 'authenticated' }
    const list = vi.fn(async () => [endpoint])
    const publish = vi.fn(async (value: Endpoint) => [value])
    const unpublish = vi.fn(async () => [])
    render(<PortsPanel {...{
      list,
      publish,
      unpublish,
      t: (key: string) => key,
    }} />)

    expect(await screen.findByText('Application')).toBeDefined()
    fireEvent.click(screen.getByRole('button', { name: 'edit' }))
    fireEvent.change(screen.getByDisplayValue('Application'), { target: { value: 'Web application' } })
    const refresh = Promise.withResolvers<Endpoint[]>()
    list.mockReturnValueOnce(refresh.promise)
    fireEvent.click(screen.getByRole('button', { name: 'refresh' }))
    expect(screen.getByRole('button', { name: 'save' }).hasAttribute('disabled')).toBe(true)
    expect(screen.getByRole('button', { name: 'unpublish' }).hasAttribute('disabled')).toBe(true)
    await act(async () => { refresh.resolve([endpoint]) })
    expect(screen.getByDisplayValue('Web application')).toBeDefined()
    fireEvent.click(screen.getByText('save'))
    await waitFor(() => {
      expect(publish).toHaveBeenCalledWith({ port: 3000, label: 'Web application', visibility: 'authenticated' })
    })
    expect(await screen.findByText('Web application')).toBeDefined()
    fireEvent.click(screen.getByRole('button', { name: 'unpublish' }))
    await screen.findByText('empty')
    expect(unpublish).toHaveBeenCalledWith(3000)
  })
})
