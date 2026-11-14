import { useCallback, useEffect, useId, useState } from 'react'
import {
  Button,
  IconEditOutlineRegular,
  IconChevronDownOutlineRegular,
  IconGlobeOutlineRegular,
  IconLinkOutlineRegular,
  IconPlusOutlineRegular,
  IconRefreshOutlineRegular,
  IconTrashOutlineRegular,
  Input,
  Menu,
  Tooltip,
} from '@deepseek-ai/dsh-client-ui-primitives'
import type { PropsLocale } from '@deepseek-ai/dsh-client-ui-slots'
import type { Endpoint, EndpointVisibility } from '../types.ts'
import type { CodespacePortsKey } from './locales.ts'
import css from './PortsPanel.module.css'
import page from './CodespacePanel.module.css'

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

export type PortsPanelProps = PortsPanelInjected & PropsLocale<'codespace.ports'>

interface EditorState {
  originalPort?: number
  port: string
  label: string
  visibility: EndpointVisibility
}

const EMPTY_EDITOR: EditorState = { port: '', label: '', visibility: 'authenticated' }

/** Render the current endpoint set and its direct management controls. */
export function PortsPanel({ list, publish, unpublish, t }: PortsPanelProps) {
  const visibilityId = useId()
  const [visibilityMenu, setVisibilityMenu] = useState(false)
  const [endpoints, setEndpoints] = useState<Endpoint[]>([])
  const [editor, setEditor] = useState<EditorState | null>(null)
  const [status, setStatus] = useState<'loading' | 'ready' | 'error'>('loading')
  const [error, setError] = useState('')
  const [pending, setPending] = useState(false)
  const busy = pending || status === 'loading'

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

  const mutate = async (operation: () => Promise<Endpoint[]>, closeEditor: boolean): Promise<void> => {
    if (busy) return
    setPending(true)
    setError('')
    try {
      setEndpoints(await operation())
      if (closeEditor) setEditor(null)
      setStatus('ready')
    } catch (problem: unknown) {
      setError(problem instanceof Error ? problem.message : String(problem))
    } finally {
      setPending(false)
    }
  }

  const save = async (): Promise<void> => {
    if (editor === null || busy) return
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
    await mutate(() => publish({ port, label, visibility: editor.visibility }), true)
  }

  return <section aria-label={t('title')}>
    <div className={page.toolbar}>
      <div className={page.actions}>
        <Tooltip label={t('refresh')} portal>
          <Button className={page.iconButton} size="sm" variant="ghost" aria-label={t('refresh')}
            onClick={() => { void refresh() }} disabled={busy} aria-busy={status === 'loading'}>
            <IconRefreshOutlineRegular size={16} />
          </Button>
        </Tooltip>
        <Button size="sm" variant="primary" icon={<IconPlusOutlineRegular size={14} />} onClick={() => { setEditor({ ...EMPTY_EDITOR }); setVisibilityMenu(false); setError('') }} disabled={busy}>
          {t('publish')}
        </Button>
      </div>
    </div>

    {editor !== null && <div className={css.editor}>
      <label>
        <span>{t('port')}</span>
        <Input type="number" min={1} max={65535} value={editor.port} disabled={editor.originalPort !== undefined || pending}
          onChange={(event) => { setEditor({ ...editor, port: event.target.value }) }} />
      </label>
      <label>
        <span>{t('label')}</span>
        <Input value={editor.label} disabled={pending} onChange={(event) => { setEditor({ ...editor, label: event.target.value }) }} />
      </label>
      <div className={css.field}>
        <span id={visibilityId}>{t('visibility')}</span>
        <Menu open={visibilityMenu} onClose={() => { setVisibilityMenu(false) }} selectedId={editor.visibility}
          items={(['authenticated', 'public'] as const).map(value => ({ id: value, label: t(value) }))}
          onSelect={(value) => { setEditor({ ...editor, visibility: value as EndpointVisibility }); setVisibilityMenu(false) }}
          anchor={<Button variant="outline" disabled={pending} aria-labelledby={visibilityId}
            aria-haspopup="menu" aria-expanded={visibilityMenu}
            onClick={() => { setVisibilityMenu(!visibilityMenu) }}>
            {t(editor.visibility)}<IconChevronDownOutlineRegular size={14} />
          </Button>} />
      </div>
      <div className={css.editorActions}>
        <Button size="sm" variant="primary" onClick={() => { void save() }} disabled={busy}>{t('save')}</Button>
        <Button size="sm" variant="ghost" onClick={() => { setEditor(null); setError('') }} disabled={pending}>{t('cancel')}</Button>
      </div>
    </div>}

    {error !== '' && <div className={page.error} role="alert">
      <span>{error}</span>
      {status === 'error' && <Button size="sm" variant="outline" disabled={pending} onClick={() => { void refresh() }}>{t('retry')}</Button>}
    </div>}

    {status === 'loading' && endpoints.length === 0
      ? <p className={page.feedback}>{t('loading')}</p>
      : endpoints.length === 0
        ? status === 'ready' && <p className={page.feedback}>{t('empty')}</p>
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
              <Tooltip label={t('edit')} portal>
                <Button className={page.iconButton} size="sm" variant="ghost" aria-label={t('edit')} disabled={busy}
                  onClick={() => { setEditor({ originalPort: endpoint.port, port: String(endpoint.port), label: endpoint.label, visibility: endpoint.visibility }); setVisibilityMenu(false); setError('') }}>
                  <IconEditOutlineRegular size={16} />
                </Button>
              </Tooltip>
              <Tooltip label={t('unpublish')} portal>
                <Button className={page.iconButton} size="sm" variant="ghost" aria-label={t('unpublish')} disabled={busy}
                  onClick={() => { void mutate(() => unpublish(endpoint.port), editor?.originalPort === endpoint.port) }}>
                  <IconTrashOutlineRegular size={16} />
                </Button>
              </Tooltip>
            </div>
          </div>)}
        </div>}
  </section>
}
