import { useState } from 'react'
import { Button, IconCopyOutlineRegular, Input } from '@deepseek-ai/dsh-client-ui-primitives'
import { sshCommand, usbSourcePattern } from '../connection.ts'
import type { SSHConnection } from '../types.ts'
import type { ConnectionKey } from './connection-locales.ts'
import css from './ConnectionActions.module.css'

declare module '@deepseek-ai/dsh-client-ui-slots' {
  interface LocaleNamespaceMap {
    'codespace.connection': ConnectionKey
  }
}

export interface ConnectionInjected {
  connection: SSHConnection | null
  connectionT: (key: ConnectionKey) => string
}

export function ConnectionActions({ connection, connectionT: t, usb = false }: ConnectionInjected & { usb?: boolean }) {
  const [source, setSource] = useState('workstation')
  const [command, setCommand] = useState('')
  const [error, setError] = useState('')

  const copy = async (forward: boolean) => {
    if (!connection) return
    if (forward && !usbSourcePattern.test(source)) {
      setError(t('invalidSource'))
      return
    }
    const value = sshCommand(connection, forward ? source : undefined)
    setCommand(value)
    setError('')
    try {
      await navigator.clipboard.writeText(value)
    } catch {
      setError(t('copyManually'))
    }
  }

  if (!connection) return <p>{t('unavailable')}</p>
  return <div className={css.connection}>
    <div className={css.actions}>
      <Button size="sm" variant="outline" icon={<IconCopyOutlineRegular size={14} />} title={t('local')}
        onClick={() => { void copy(false) }}>{t('ssh')}</Button>
      {usb && <>
        <label className={css.source}>{t('source')}<Input value={source} maxLength={32}
          onChange={event => { setSource(event.target.value); setCommand(''); setError('') }} /></label>
        <Button size="sm" variant="outline" icon={<IconCopyOutlineRegular size={14} />} title={t('local')}
          onClick={() => { void copy(true) }}>{t('usb')}</Button>
      </>}
    </div>
    {error && <p role="alert">{error}</p>}
    {command && <>
      <textarea className={css.command} aria-label={t('command')} readOnly rows={3} value={command} onFocus={event => event.target.select()} />
      <p>{t('local')}</p>
    </>}
  </div>
}
