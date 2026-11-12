import { useCallback, useEffect, useRef, useState } from 'react'
import { Button, IconLinkOutlineRegular, IconPlusOutlineRegular, IconRefreshOutlineRegular, IconTrashOutlineRegular } from '@deepseek-ai/dsh-client-ui-primitives'
import type { InjectFace, PropsLocale, PropsRuntime } from '@deepseek-ai/dsh-client-ui-slots'
import type { USBList, USBStatus } from '../types.ts'
import type { USBKey } from './usb-locales.ts'
import css from './PortsPanel.module.css'
import usbCSS from './USBPanel.module.css'

declare module '@deepseek-ai/dsh-client-ui-slots' {
  interface LocaleNamespaceMap {
    'codespace.usb': USBKey
  }
}

export interface USBPanelInjected {
  status: () => Promise<USBStatus>
  list: () => Promise<USBList>
  attach: (busId: string, source: string) => Promise<void>
  detach: (port: number) => Promise<void>
}

export type USBPanelProps = PropsRuntime<'main'> & InjectFace<USBPanelInjected> & PropsLocale<'codespace.usb'>

export function USBPanel({ status, list, attach, detach, t }: USBPanelProps) {
  const [current, setCurrent] = useState<USBStatus | null>(null)
  const [exported, setExported] = useState<USBList | null>(null)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState('')
  const busy = useRef(false)

  const run = useCallback(async (action?: () => Promise<void>) => {
    if (busy.current) return
    busy.current = true
    setPending(true)
    setError('')
    try {
      await action?.()
    } catch (problem) {
      setError(problem instanceof Error ? problem.message : String(problem))
    } finally {
      try {
        const next = await status()
        setCurrent(next)
        if (next.sources.length === 0) setExported(null)
      } catch (problem) {
        setError(previous => previous || (problem instanceof Error ? problem.message : String(problem)))
      }
      busy.current = false
      setPending(false)
    }
  }, [status])

  useEffect(() => { void run() }, [run])

  return <section className={css.page} aria-label={t('title')}>
    <div className={css.content}>
      <header className={css.header}>
        <h1>{t('title')}</h1>
        <div className={usbCSS.actions}>
          <Button size="sm" variant="outline" icon={<IconRefreshOutlineRegular size={14} />} disabled={pending}
            onClick={() => { void run() }}>{t('refresh')}</Button>
          <Button size="sm" variant="outline" icon={<IconPlusOutlineRegular size={14} />} disabled={pending || !current?.sources.length}
            onClick={() => { void run(async () => { setExported(await list()) }) }}>{t('list')}</Button>
        </div>
      </header>
      {(current !== null || error === '') && <p role="status">{current === null ? t('loading') : t(current.sources.length > 0 ? 'forwarded' : 'disconnected')}</p>}
      {error !== '' && <div className={`${css.error} ${usbCSS.error}`} role="alert">{error}</div>}

      <h2 className={usbCSS.heading}>{t('imported')}</h2>
      {current !== null && current.devices.length === 0 && <p>{t('emptyImported')}</p>}
      {[...new Set([...(current?.sources.map(source => source.name) ?? []), ...(current?.devices.map(device => device.source) ?? [])])].map(source => <section key={source}>
        <h3 className={usbCSS.heading}>{source || t('unknownSource')}</h3>
        <div className={css.list}>
        {current?.devices.filter(device => device.source === source).map(device => <div className={usbCSS.row} key={device.port}>
          <div className={usbCSS.device}><strong>{device.description}</strong><span>{t('port')} {device.port} · {device.busId || '?'} · {device.state}</span></div>
          <Button size="sm" variant="outline" icon={<IconTrashOutlineRegular size={14} />} disabled={pending}
            onClick={() => { void run(() => detach(device.port)) }}>{t('detach')}</Button>
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
          {exported.devices.filter(device => device.source === source.name).map(device => <div className={usbCSS.row} key={device.busId}>
            <div className={usbCSS.device}><strong>{device.description}</strong><span>{device.busId}</span></div>
            <Button size="sm" variant="outline" icon={<IconPlusOutlineRegular size={14} />}
              disabled={pending || !current?.sources.some(value => value.name === device.source) || current.devices.some(value => value.source === device.source && value.busId === device.busId)}
              onClick={() => { void run(() => attach(device.busId, device.source)) }}>{t('attach')}</Button>
          </div>)}
          </div>
        </section>)}
      </>}
    </div>
  </section>
}

export function USBPanelIcon({ size }: PropsRuntime<'sidebar.panellist'>) {
  return <IconLinkOutlineRegular size={size} />
}
