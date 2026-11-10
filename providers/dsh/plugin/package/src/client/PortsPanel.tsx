import { useCallback, useEffect, useState } from 'react'
import {
  Button,
  IconEditOutlineRegular,
  IconGlobeOutlineRegular,
  IconLinkOutlineRegular,
  IconPlusOutlineRegular,
  IconRefreshOutlineRegular,
  IconTrashOutlineRegular,
  Input,
} from '@deepseek-ai/dsh-client-ui-primitives'
import type { InjectFace, PropsLocale, PropsRuntime } from '@deepseek-ai/dsh-client-ui-slots'
import type {} from '@deepseek-ai/dsh-client-ui-layout/client'
import type { Endpoint, EndpointVisibility } from '../types.ts'
import type { CodespacePortsKey } from './locales.ts'
import css from './PortsPanel.module.css'

declare module '@deepseek-ai/dsh-client-ui-slots' {
  interface LocaleNamespaceMap {
    /** Codespace endpoint management copy. */
    'codespace.ports': CodespacePortsKey
  }
}

/** Browser operations injected into the Ports panel. */
export interface PortsPanelInjected {
  list: () => Promise<Endpoint[]>
  publish: (endpoint: Endpoint) => Promise<Endpoint[]>
  unpublish: (port: number) => Promise<Endpoint[]>
}

export type PortsPanelProps = PropsRuntime<'main'> & InjectFace<PortsPanelInjected> & PropsLocale<'codespace.ports'>

interface EditorState {
  originalPort?: number
  port: string
  label: string
  visibility: EndpointVisibility
}

const EMPTY_EDITOR: EditorState = { port: '', label: '', visibility: 'authenticated' }

/** Render the current endpoint set and its direct management controls. */
export function PortsPanel({ list, publish, unpublish, t }: PortsPanelProps) {
  const [endpoints, setEndpoints] = useState<Endpoint[]>([])
  const [editor, setEditor] = useState<EditorState | null>(null)
  const [status, setStatus] = useState<'loading' | 'ready' | 'error'>('loading')
  const [error, setError] = useState('')
  const [pending, setPending] = useState(false)

  const refresh = useCallback(async (): Promise<void> => {
    setStatus('loading')
    setError('')
    try {
      setEndpoints(await list())
      setStatus('ready')
    } catch (problem: unknown) {
      setError(problem instanceof Error ? problem.message : String(problem))
      setStatus('error')
    }
  }, [list])

  useEffect(() => { void refresh() }, [refresh])

  const save = async (): Promise<void> => {
    if (editor === null || pending) return
    const port = Number(editor.port)
    if (!Number.isSafeInteger(port) || port < 1 || port > 65535) {
      setError(t('invalidPort'))
      return
    }
    const label = editor.label.trim()
    const bytes = new TextEncoder().encode(label).length
    if (bytes < 1 || bytes > 128) {
      setError(t('invalidLabel'))
      return
    }
    setPending(true)
    setError('')
    try {
      setEndpoints(await publish({ port, label, visibility: editor.visibility }))
      setEditor(null)
      setStatus('ready')
    } catch (problem: unknown) {
      setError(problem instanceof Error ? problem.message : String(problem))
    } finally {
      setPending(false)
    }
  }

  const remove = async (port: number): Promise<void> => {
    if (pending) return
    setPending(true)
    setError('')
    try {
      setEndpoints(await unpublish(port))
      if (editor?.originalPort === port) setEditor(null)
      setStatus('ready')
    } catch (problem: unknown) {
      setError(problem instanceof Error ? problem.message : String(problem))
    } finally {
      setPending(false)
    }
  }

  return <section className={css.page} aria-label={t('title')}>
    <div className={css.content}>
      <header className={css.header}>
        <div>
          <h1>{t('title')}</h1>
          <p>{t('description')}</p>
        </div>
        <div className={css.headerActions}>
          <Button size="sm" variant="outline" icon={<IconRefreshOutlineRegular size={14} />} onClick={() => { void refresh() }} disabled={pending}>
            {t('refresh')}
          </Button>
          <Button size="sm" variant="primary" icon={<IconPlusOutlineRegular size={14} />} onClick={() => { setEditor({ ...EMPTY_EDITOR }); setError('') }} disabled={pending}>
            {t('publish')}
          </Button>
        </div>
      </header>

      {editor !== null && <div className={css.editor}>
        <label>
          <span>{t('port')}</span>
          <Input type="number" min={1} max={65535} value={editor.port} disabled={editor.originalPort !== undefined || pending}
            onChange={(event) => { setEditor({ ...editor, port: event.target.value }) }} />
        </label>
        <label className={css.labelField}>
          <span>{t('label')}</span>
          <Input value={editor.label} disabled={pending} onChange={(event) => { setEditor({ ...editor, label: event.target.value }) }} />
        </label>
        <label>
          <span>{t('visibility')}</span>
          <select value={editor.visibility} disabled={pending}
            onChange={(event) => { setEditor({ ...editor, visibility: event.target.value as EndpointVisibility }) }}>
            <option value="authenticated">{t('authenticated')}</option>
            <option value="public">{t('public')}</option>
          </select>
        </label>
        <div className={css.editorActions}>
          <Button size="sm" variant="primary" onClick={() => { void save() }} disabled={pending}>{t('save')}</Button>
          <Button size="sm" variant="ghost" onClick={() => { setEditor(null); setError('') }} disabled={pending}>{t('cancel')}</Button>
        </div>
      </div>}

      {error !== '' && <div className={css.error} role="alert">
        <span>{error}</span>
        {status === 'error' && <Button size="sm" variant="outline" onClick={() => { void refresh() }}>{t('retry')}</Button>}
      </div>}

      {status === 'loading' && endpoints.length === 0
        ? <p className={css.feedback}>{t('loading')}</p>
        : endpoints.length === 0
          ? <p className={css.feedback}>{t('empty')}</p>
          : <div className={css.list}>
            {endpoints.map(endpoint => <div className={css.row} key={endpoint.port}>
              <div className={css.portIcon} aria-hidden><IconLinkOutlineRegular size={18} /></div>
              <div className={css.endpoint}>
                <strong>{endpoint.label}</strong>
                <span>:{endpoint.port}</span>
              </div>
              <span className={css.visibility}>
                {endpoint.visibility === 'public' && <IconGlobeOutlineRegular size={13} />}
                {t(endpoint.visibility)}
              </span>
              <div className={css.rowActions}>
                <Button size="sm" variant="ghost" icon={<IconEditOutlineRegular size={14} />} disabled={pending}
                  onClick={() => { setEditor({ originalPort: endpoint.port, port: String(endpoint.port), label: endpoint.label, visibility: endpoint.visibility }); setError('') }}>
                  {t('edit')}
                </Button>
                <Button size="sm" variant="ghost" icon={<IconTrashOutlineRegular size={14} />} disabled={pending}
                  onClick={() => { void remove(endpoint.port) }}>
                  {t('unpublish')}
                </Button>
              </div>
            </div>)}
          </div>}
    </div>
  </section>
}

/** Sidebar glyph for the Ports panel. */
export function PortsPanelIcon({ size }: PropsRuntime<'sidebar.panellist'>) {
  return <IconLinkOutlineRegular size={size} />
}
