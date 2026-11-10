// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { PortsPanel, type PortsPanelProps } from '../src/client/PortsPanel.tsx'
import type { Endpoint } from '../src/types.ts'

afterEach(cleanup)

describe('Codespace Ports panel', () => {
  it('loads endpoints and edits one through the injected operations', async () => {
    const endpoint: Endpoint = { port: 3000, label: 'Application', visibility: 'authenticated' }
    const publish = vi.fn(async (value: Endpoint) => [value])
    render(<PortsPanel {...({
      list: async () => [endpoint],
      publish,
      unpublish: async () => [],
      t: (key: string) => key,
    } as unknown as PortsPanelProps)} />)

    expect(await screen.findByText('Application')).toBeDefined()
    fireEvent.click(screen.getByText('edit'))
    fireEvent.change(screen.getByDisplayValue('Application'), { target: { value: 'Web application' } })
    fireEvent.click(screen.getByText('save'))
    await waitFor(() => {
      expect(publish).toHaveBeenCalledWith({ port: 3000, label: 'Web application', visibility: 'authenticated' })
    })
    expect(await screen.findByText('Web application')).toBeDefined()
  })
})
