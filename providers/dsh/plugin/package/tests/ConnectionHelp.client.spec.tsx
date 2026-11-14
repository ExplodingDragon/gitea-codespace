// @vitest-environment jsdom

import { useState } from 'react'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import { ConnectionHelp } from '../src/client/ConnectionHelp.tsx'

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

it('shows connection commands before copying, validates sources and restores modal focus', async () => {
  const writeText = vi.fn(async (_value: string) => {})
  vi.stubGlobal('navigator', { clipboard: { writeText } })
  const connection = { user: 'cs-933ef4a9-54c7-4f36-aad9-32ea72c4c986', host: 'ssh.example.com', port: 2222 }
  function Help() {
    const [open, setOpen] = useState(false)
    return <><button onClick={() => { setOpen(true) }}>help</button>
      <ConnectionHelp open={open} onClose={() => { setOpen(false) }} connection={connection} usb t={key => key} />
    </>
  }
  const { unmount } = render(<Help />)
  const trigger = screen.getByRole('button', { name: 'help' })
  trigger.focus()
  fireEvent.click(trigger)
  const ssh = `ssh -p 2222 -l ${connection.user} ssh.example.com`
  expect(screen.getByLabelText<HTMLTextAreaElement>('SSH command').value).toBe(ssh)
  fireEvent.click(screen.getByRole('button', { name: 'copy SSH' }))
  await waitFor(() => { expect(writeText).toHaveBeenCalledWith(ssh) })
  expect((await screen.findByRole('status')).textContent).toBe('copied')

  fireEvent.change(screen.getByLabelText('source'), { target: { value: 'lab' } })
  writeText.mockRejectedValueOnce(new Error('Clipboard requires HTTPS'))
  fireEvent.click(screen.getByRole('button', { name: 'copy USB/IP' }))
  expect((await screen.findByRole('alert')).textContent).toBe('copyManually')
  expect(screen.getByLabelText<HTMLTextAreaElement>('USB/IP command').value).toBe(
    `ssh -NT -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -R usbip.lab:3240:127.0.0.1:3240 -p 2222 -l ${connection.user} ssh.example.com`,
  )
  fireEvent.change(screen.getByLabelText('source'), { target: { value: 'invalid source' } })
  expect(screen.getByRole('alert').textContent).toBe('invalidSource')
  expect(screen.getByRole('button', { name: 'copy USB/IP' }).hasAttribute('disabled')).toBe(true)
  fireEvent.change(screen.getByLabelText('source'), { target: { value: 'workstation' } })
  expect(screen.queryByRole('alert')).toBeNull()
  expect(screen.getByRole('button', { name: 'copy USB/IP' }).hasAttribute('disabled')).toBe(false)
  fireEvent.keyDown(document, { key: 'Escape' })
  expect(document.activeElement).toBe(trigger)
  unmount()
  render(<ConnectionHelp open onClose={() => {}} connection={null} usb={false} t={key => key} />)
  expect(screen.getByRole('dialog').textContent).toContain('unavailable')
})
