import { useEffect, useRef, useState } from 'react'
import { Button, IconRefreshOutlineRegular, Modal, StateDot, Tag, Tooltip } from '@deepseek-ai/dsh-client-ui-primitives'
import type { PropsLocale } from '@deepseek-ai/dsh-client-ui-slots'
import type { USBDevice, USBImportedDevice, USBList, USBStatus } from '../types.ts'
import type { USBKey } from './usb-locales.ts'
import page from './CodespacePanel.module.css'
import css from './USBPanel.module.css'

declare module '@deepseek-ai/dsh-client-ui-slots' {
  interface LocaleNamespaceMap {
    'codespace.usb': USBKey
  }
}

export interface USBPanelInjected {
  status: (signal?: AbortSignal) => Promise<USBStatus>
  list: (signal?: AbortSignal) => Promise<USBList>
  attach: (busId: string, source: string) => Promise<void>
  detach: (port: number) => Promise<void>
}

export type USBPanelProps = USBPanelInjected & PropsLocale<'codespace.usb'> & { active: boolean }

export function USBPanel({ status, list, attach, detach, active, t }: USBPanelProps) {
  const [current, setCurrent] = useState<USBStatus | null>(null)
  const [exported, setExported] = useState<USBList | null>(null)
  const [pending, setPending] = useState(false)
  const [refreshing, setRefreshing] = useState(false)
  const [refreshVersion, setRefreshVersion] = useState(0)
  const [visible, setVisible] = useState(document.visibilityState !== 'hidden')
  const [error, setError] = useState('')
  const [statusError, setStatusError] = useState('')
  const [listError, setListError] = useState('')
  const [confirmDevice, setConfirmDevice] = useState<USBImportedDevice | null>(null)
  const readController = useRef<AbortController | null>(null)
  const writing = useRef(false)
  const mounted = useRef(false)

  useEffect(() => {
    mounted.current = true
    const visibility = () => { setVisible(document.visibilityState !== 'hidden') }
    document.addEventListener('visibilitychange', visibility)
    return () => {
      mounted.current = false
      document.removeEventListener('visibilitychange', visibility)
    }
  }, [])

  useEffect(() => {
    if (!active || !visible || pending) {
      setRefreshing(false)
      return
    }
    const controller = new AbortController()
    const { signal } = controller
    readController.current = controller
    let timer: ReturnType<typeof setTimeout> | undefined

    const refresh = async () => {
      setRefreshing(true)
      try {
        let next: USBStatus
        try {
          next = await status(signal)
          signal.throwIfAborted()
          setCurrent(next)
          setStatusError('')
        } catch (problem) {
          if (!signal.aborted) setStatusError(problem instanceof Error ? problem.message : String(problem))
          return
        }
        if (next.sources.length === 0) {
          setExported({ sources: [], devices: [] })
          setListError('')
          return
        }
        try {
          const result = await list(signal)
          signal.throwIfAborted()
          const registered = new Set(next.sources.map(source => source.name))
          const sources = result.sources.filter(source => registered.has(source.name))
          // Keep the last list only for failed sources; successful empty lists remove old devices.
          setExported(previous => ({
            sources,
            devices: sources.flatMap(source => (source.error ? previous?.devices ?? [] : result.devices)
              .filter(device => device.source === source.name)),
          }))
          setListError('')
        } catch (problem) {
          if (!signal.aborted) setListError(problem instanceof Error ? problem.message : String(problem))
        }
      } finally {
        if (!signal.aborted) {
          setRefreshing(false)
          timer = setTimeout(() => { void refresh() }, 10000)
        }
      }
    }
    void refresh()
    return () => {
      controller.abort()
      clearTimeout(timer)
    }
  }, [active, visible, pending, refreshVersion, status, list])

  const mutate = async (operation: () => Promise<void>) => {
    if (writing.current) return
    writing.current = true
    readController.current?.abort()
    setPending(true)
    setError('')
    setConfirmDevice(null)
    try {
      await operation()
    } catch (problem) {
      if (mounted.current) setError(problem instanceof Error ? problem.message : String(problem))
    } finally {
      writing.current = false
      // Even a failed command may have changed the device. Restart the read cycle.
      if (mounted.current) {
        setPending(false)
        setRefreshVersion(value => value + 1)
      }
    }
  }

  const statusReady = current !== null && statusError === ''
  const sources = [...new Set([
    ...(current?.sources.map(source => source.name) ?? []),
    ...(current?.devices.map(device => device.source) ?? []),
  ])]
  const canDetach = statusReady && current.devices.some(device => device.port === confirmDevice?.port
    && device.source === confirmDevice.source && device.busId === confirmDevice.busId)

  return <section aria-label={t('title')}>
    <div className={page.toolbar}>
      <p className={page.status} role="status">
        {statusError ? t('stale') : current === null ? t('loading') : t(current.sources.length > 0 ? 'forwarded' : 'disconnected')}
      </p>
      <Tooltip label={t('refresh')} portal>
        <Button className={page.iconButton} size="sm" variant="ghost" aria-label={t('refresh')} aria-busy={refreshing}
          disabled={pending || refreshing} onClick={() => { setError(''); setRefreshVersion(value => value + 1) }}>
          {refreshing ? <StateDot state="ongoing" size={16} /> : <IconRefreshOutlineRegular size={16} />}
        </Button>
      </Tooltip>
    </div>
    {(error || statusError || listError) && <div className={page.error} role="alert">
      {error && <p>{t('operationFailed')}: {error}</p>}
      {statusError && <p>{t('statusFailed')}: {statusError}</p>}
      {listError && <p>{t('listFailed')}: {listError}</p>}
    </div>}
    {statusReady && sources.length === 0 && <p className={page.feedback}>{t('empty')}</p>}
    {sources.map((source) => {
      const registered = current?.sources.some(value => value.name === source) ?? false
      const sourceError = exported?.sources.find(value => value.name === source)?.error
      const imported = current?.devices.filter(device => device.source === source) ?? []
      const rows: { device: USBDevice; imported?: USBImportedDevice }[] = [
        ...imported.map(device => ({ device, imported: device })),
        ...(registered ? exported?.devices.filter(device => device.source === source
          && !imported.some(value => value.busId === device.busId)).map(device => ({ device })) ?? [] : []),
      ]
      return <section key={source} className={css.group} aria-label={source || t('unknownSource')}>
        <header className={css.heading}>
          <h2>{source || t('unknownSource')}</h2>
          {!registered && <span>{t('sourceDisconnected')}</span>}
        </header>
        {sourceError && <p className={page.error} role="alert">{t('listFailed')}: {sourceError}</p>}
        {rows.length === 0 && !sourceError && !listError && <p className={page.feedback}>{t(refreshing ? 'loading' : 'empty')}</p>}
        {rows.map(({ device, imported }) => <div className={css.row} key={imported ? `port:${imported.port}` : `bus:${device.busId}`}>
          <div className={css.device}>
            <strong>{device.description}</strong>
            <span>{device.busId || '?'}{imported && <> · {t('port')} {imported.port} · {imported.state}</>}</span>
          </div>
          <Tag tone="neutral">{t(imported ? 'attached' : 'unattached')}</Tag>
          {imported
            ? <Button size="sm" variant="outline" disabled={pending || !statusReady}
              onClick={() => { setConfirmDevice(imported) }}>{t('detach')}</Button>
            : <Button size="sm" variant="outline" disabled={pending || !statusReady || listError !== '' || Boolean(sourceError)}
              onClick={() => { void mutate(() => attach(device.busId, device.source)) }}>{t('attach')}</Button>}
        </div>)}
      </section>
    })}
    <Modal open={confirmDevice !== null && active} onClose={() => { setConfirmDevice(null) }} title={t('detach')}
      closeLabel={t('close')} tone="warning" description={t('detachWarning')}
      footer={<>
        <Button size="sm" onClick={() => { setConfirmDevice(null) }}>{t('cancel')}</Button>
        <Button size="sm" variant="primary" disabled={pending || !canDetach}
          onClick={() => { if (confirmDevice) void mutate(() => detach(confirmDevice.port)) }}>{t('detach')}</Button>
      </>}>
      {confirmDevice && <p className={css.confirmDevice}>{confirmDevice.description}<br />
        {confirmDevice.source || t('unknownSource')} / {confirmDevice.busId || '?'} · {t('port')} {confirmDevice.port}
      </p>}
      {!canDetach && <p className={page.status}>{t('stale')}</p>}
    </Modal>
  </section>
}
