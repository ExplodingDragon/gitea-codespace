import { useEffect, useId, useState } from 'react'
import { Button, IconCheckOutlineRegular, IconCopyOutlineRegular, Input, Modal, Tooltip, writeClipboard } from '@deepseek-ai/dsh-client-ui-primitives'
import { sshCommand, usbSourcePattern } from '../connection.ts'
import type { SSHConnection } from '../types.ts'
import type { ConnectionKey } from './connection-locales.ts'
import css from './ConnectionHelp.module.css'

export function ConnectionHelp({ open, onClose, connection, usb, t }: {
  open: boolean
  onClose: () => void
  connection: SSHConnection | null
  usb: boolean
  t: (key: ConnectionKey) => string
}) {
  const sourceErrorId = useId()
  const [source, setSource] = useState('workstation')
  const [copied, setCopied] = useState('')
  const [error, setError] = useState('')
  useEffect(() => { setCopied(''); setError('') }, [open, source])

  const validSource = usbSourcePattern.test(source)
  const commands = connection ? [
    { id: 'ssh', title: 'SSH', value: sshCommand(connection) },
    ...(usb ? [{ id: 'usb', title: 'USB/IP', value: validSource ? sshCommand(connection, source) : '' }] : []),
  ] : []

  return <Modal open={open} onClose={onClose} title={t('help')} closeLabel={t('close')} className={css.dialog as string}>
    {!connection ? <p>{t('unavailable')}</p> : <>
      <p className={css.address}>{connection.user}@{connection.host}:{connection.port}</p>
      <p className={css.hint}>{t('local')}</p>
      {commands.map(command => <section key={command.id} className={css.section}>
        <h3>{command.title}</h3>
        {command.id === 'usb' && <>
          <label className={css.source}>
            <span>{t('source')}</span>
            <Input value={source} maxLength={32} aria-invalid={!validSource} aria-describedby={!validSource ? sourceErrorId : undefined}
              onChange={(event) => { setSource(event.target.value) }} />
          </label>
          {!validSource && <p id={sourceErrorId} className={css.error} role="alert">{t('invalidSource')}</p>}
        </>}
        <div className={css.command}>
          <textarea aria-label={`${command.title} ${t('command')}`} readOnly rows={command.id === 'usb' ? 5 : 3}
            value={command.value} onFocus={(event) => { event.target.select() }} />
          <Tooltip label={t(copied === command.id ? 'copied' : 'copy')} portal>
            <Button size="sm" variant="ghost" className={css.copy} disabled={!command.value} aria-label={`${t('copy')} ${command.title}`}
              onClick={() => {
                void writeClipboard(command.value).then((success) => {
                  setCopied(success ? command.id : '')
                  setError(success ? '' : t('copyManually'))
                })
              }}>
              {copied === command.id ? <IconCheckOutlineRegular size={16} /> : <IconCopyOutlineRegular size={16} />}
            </Button>
          </Tooltip>
        </div>
      </section>)}
      {copied && <p className={css.hint} role="status">{t('copied')}</p>}
      {error && <p className={css.error} role="alert">{error}</p>}
    </>}
  </Modal>
}
