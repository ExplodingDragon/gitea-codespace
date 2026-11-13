import { useCallback, useEffect, useRef, useState } from 'react'
import { Button, IconLinkOutlineRegular, IconPlusOutlineRegular, IconRefreshOutlineRegular, IconTrashOutlineRegular } from '@deepseek-ai/dsh-client-ui-primitives'
import type { InjectFace, PropsLocale, PropsRuntime } from '@deepseek-ai/dsh-client-ui-slots'
import type { USBList, USBStatus } from '../types.ts'
import type { USBKey } from './usb-locales.ts'
import css from './PortsPanel.module.css'
import usbCSS from './USBPanel.module.css'
import { ConnectionActions, type ConnectionInjected } from './ConnectionActions.tsx'

declare module '@deepseek-ai/dsh-client-ui-slots' {
  interface LocaleNamespaceMap {
    'codespace.usb': USBKey
  }
}

export interface USBPanelInjected extends ConnectionInjected {
  status: () => Promise<USBStatus>
  list: () => Promise<USBList>
  attach: (busId: string, source: string) => Promise<void>
  detach: (port: number) => Promise<void>
}

export type USBPanelProps = PropsRuntime<'main'> & InjectFace<USBPanelInjected> & PropsLocale<'codespace.usb'>

export function USBPanel({ status, list, attach, detach, connection, connectionT, t }: USBPanelProps) {
  const [current, setCurrent] = useState<USBStatus | null>(null)
  const [exported, setExported] = useState<USBList | null>(null)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState('')
  const [statusError, setStatusError] = useState('')
  const [listError, setListError] = useState('')
  const busy = useRef(false)

  const run = useCallback(async (action?: () => Promise<void>, queryRemote = false) => {
    if (busy.current) return
    busy.current = true
    setPending(true)
    setError('')
    try {
      await action?.()
    } catch (problem) {
      setError(problem instanceof Error ? problem.message : String(problem))
    }
    // A failed mutation may still have taken effect. Refresh facts once.
    let next: USBStatus | undefined
    try {
      next = await status()
      setCurrent(next)
      setStatusError('')
    } catch (problem) {
      setStatusError(problem instanceof Error ? problem.message : String(problem))
    }
    if (queryRemote) {
      try {
        const result = next?.sources.length === 0 ? { sources: [], devices: [] } : await list()
        if (next) {
          const sources = new Set(next.sources.map(source => source.name))
          result.sources = result.sources.filter(source => sources.has(source.name))
          result.devices = result.devices.filter(device => sources.has(device.source))
        }
        setExported(result)
        setListError('')
      } catch (problem) {
        setExported(previous => previous ?? { sources: [], devices: [] })
        setListError(problem instanceof Error ? problem.message : String(problem))
      }
    }
    busy.current = false
    setPending(false)
  }, [status, list])

  useEffect(() => { void run() }, [run])
  const statusReady = current !== null && statusError === ''

  return <section className={css.page} aria-label={t('title')}>
    <div className={css.content}>
      <header className={css.header}>
        <h1>{t('title')}</h1>
        <div className={usbCSS.actions}>
          <Button size="sm" variant="outline" icon={<IconRefreshOutlineRegular size={14} />} disabled={pending}
            onClick={() => { void run(undefined, exported !== null) }}>{t('refresh')}</Button>
          <Button size="sm" variant="outline" icon={<IconPlusOutlineRegular size={14} />} disabled={pending || !statusReady || !current?.sources.length}
            onClick={() => { void run(undefined, true) }}>{t('list')}</Button>
        </div>
      </header>
      <ConnectionActions connection={connection} connectionT={connectionT} usb />
      <p role="status">{statusError !== '' ? t('stale') : current === null ? t('loading') : t(current.sources.length > 0 ? 'forwarded' : 'disconnected')}</p>
      {(error || statusError || listError) && <div className={`${css.error} ${usbCSS.error}`} role="alert">
        {error && <p>{t('operationFailed')}: {error}</p>}
        {statusError && <p>{t('statusFailed')}: {statusError}</p>}
        {listError && <p>{t('listFailed')}: {listError}</p>}
      </div>}

      <h2 className={usbCSS.heading}>{t('imported')}</h2>
      {current !== null && current.devices.length === 0 && <p>{t('emptyImported')}</p>}
      {[...new Set([...(current?.sources.map(source => source.name) ?? []), ...(current?.devices.map(device => device.source) ?? [])])].map(source => <section key={source}>
        <h3 className={usbCSS.heading}>{source || t('unknownSource')}</h3>
        <div className={css.list}>
        {current?.devices.filter(device => device.source === source).map(device => <div className={usbCSS.row} key={device.port}>
          <div className={usbCSS.device}><strong>{device.description}</strong><span>{t('port')} {device.port} · {device.busId || '?'} · {device.state}</span></div>
          <Button size="sm" variant="outline" icon={<IconTrashOutlineRegular size={14} />} disabled={pending || !statusReady}
            onClick={() => {
              if (window.confirm(`${t('detach')} ${device.description} (${source || t('unknownSource')} / ${device.busId || '?'}, ${t('port')} ${device.port})?\n${t('detachWarning')}`)) {
                void run(() => detach(device.port), exported !== null)
              }
            }}>{t('detach')}</Button>
        </div>)}
        </div>
      </section>)}

      {exported !== null && <>
        <h2 className={usbCSS.heading}>{t('exported')}</h2>
        {exported.sources.map(source => <section key={source.name}>
          <h3 className={usbCSS.heading}>{source.name}</h3>
          {source.error && <p className={css.error} role="alert">{source.error}</p>}
          {!source.error && !exported.devices.some(device => device.source === source.name) && <p>{t('emptyExported')}</p>}
          <div className={css.list}>
          {exported.devices.filter(device => device.source === source.name).map(device => {
            const imported = current?.devices.some(value => value.source === device.source && value.busId === device.busId)
            return <div className={usbCSS.row} key={device.busId}>
            <div className={usbCSS.device}><strong>{device.description}</strong><span>{device.busId}</span></div>
            <Button size="sm" variant="outline" icon={<IconPlusOutlineRegular size={14} />}
              disabled={pending || !statusReady || listError !== '' || Boolean(source.error) || !current?.sources.some(value => value.name === device.source) || imported}
              onClick={() => { void run(() => attach(device.busId, device.source), true) }}>{t(imported ? 'attached' : 'attach')}</Button>
          </div>})}
          </div>
        </section>)}
      </>}
    </div>
  </section>
}

export function USBPanelIcon({ size }: PropsRuntime<'sidebar.panellist'>) {
  return <IconLinkOutlineRegular size={size} />
}
