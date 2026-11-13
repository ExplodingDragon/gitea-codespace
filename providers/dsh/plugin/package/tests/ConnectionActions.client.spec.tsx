// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import { ConnectionActions } from '../src/client/ConnectionActions.tsx'

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

it('copies local SSH and USB commands and keeps them readable when clipboard access fails', async () => {
  const writeText = vi.fn(async (_value: string) => {})
  vi.stubGlobal('navigator', { clipboard: { writeText } })
  const connection = { user: 'cs-933ef4a9-54c7-4f36-aad9-32ea72c4c986', host: 'ssh.example.com', port: 2222 }
  const connectionT = (key: string) => key
  const { rerender } = render(<ConnectionActions connection={connection} connectionT={connectionT} />)
  fireEvent.click(screen.getByText('ssh'))
  await waitFor(() => { expect(writeText).toHaveBeenCalledWith(`ssh -p 2222 -l ${connection.user} ssh.example.com`) })
  rerender(<ConnectionActions connection={connection} connectionT={connectionT} usb />)
  expect((screen.getByLabelText('source') as HTMLInputElement).value).toBe('workstation')
  fireEvent.change(screen.getByLabelText('source'), { target: { value: 'lab' } })
  writeText.mockRejectedValueOnce(new Error('Clipboard requires HTTPS'))
  fireEvent.click(screen.getByText('usb'))
  await waitFor(() => { expect(screen.getByRole('alert').textContent).toBe('copyManually') })
  expect((screen.getByLabelText('command') as HTMLTextAreaElement).value).toBe(
    `ssh -NT -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -R usbip.lab:3240:127.0.0.1:3240 -p 2222 -l ${connection.user} ssh.example.com`,
  )
  fireEvent.change(screen.getByLabelText('source'), { target: { value: 'invalid source' } })
  fireEvent.click(screen.getByText('usb'))
  expect(screen.getByRole('alert').textContent).toBe('invalidSource')
  expect(writeText).toHaveBeenCalledTimes(2)
  rerender(<ConnectionActions connection={null} connectionT={connectionT} usb />)
  expect(screen.getByText('unavailable')).toBeDefined()
})
