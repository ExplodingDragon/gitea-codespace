import { useId, useState } from 'react'
import { Button, IconLinkOutlineRegular, IconQuestionOutlineRegular, SegmentedTabs, Tooltip } from '@deepseek-ai/dsh-client-ui-primitives'
import type { InjectFace, PropsLocale, PropsRuntime, TranslateNS } from '@deepseek-ai/dsh-client-ui-slots'
import type {} from '@deepseek-ai/dsh-client-ui-layout/client'
import type { SSHConnection } from '../types.ts'
import { ConnectionHelp } from './ConnectionHelp.tsx'
import type { ConnectionKey } from './connection-locales.ts'
import { PortsPanel, type PortsPanelInjected } from './PortsPanel.tsx'
import { USBPanel, type USBPanelInjected } from './USBPanel.tsx'
import css from './CodespacePanel.module.css'

declare module '@deepseek-ai/dsh-client-ui-slots' {
  interface LocaleNamespaceMap {
    'codespace.connection': ConnectionKey
  }
}

export interface CodespacePanelInjected {
  connection: SSHConnection | null
  ports: PortsPanelInjected
  usb: USBPanelInjected | null
  portsT: TranslateNS<'codespace.ports'>
  usbT: TranslateNS<'codespace.usb'>
}

export type CodespacePanelProps = PropsRuntime<'main'> & InjectFace<CodespacePanelInjected> & PropsLocale<'codespace.connection'>

/** One navigation entry owns both tools and their shared connection help. */
export function CodespacePanel({ connection, ports, usb, portsT, usbT, t }: CodespacePanelProps) {
  const id = useId()
  const [tab, setTab] = useState<'ports' | 'usb'>('ports')
  const [usbVisited, setUSBVisited] = useState(false)
  const [help, setHelp] = useState(false)

  return <section className={css.page} aria-label="Codespace">
    <div className={css.content}>
      <header className={css.header}>
        <h1>Codespace</h1>
        <Tooltip label={t('help')} portal>
          <Button className={css.iconButton} variant="ghost" size="sm" aria-label={t('help')} onClick={() => { setHelp(true) }}>
            <IconQuestionOutlineRegular size={18} />
          </Button>
        </Tooltip>
      </header>
      {usb && <SegmentedTabs<'ports' | 'usb'> size="sm" className={css.tabs} label="Codespace" value={tab}
        onChange={(value) => { setTab(value); if (value === 'usb') setUSBVisited(true) }}
        items={[
          { value: 'ports', label: portsT('title'), id: `${id}-ports-tab`, panelId: `${id}-ports` },
          { value: 'usb', label: usbT('title'), id: `${id}-usb-tab`, panelId: `${id}-usb` },
        ]} />}
      <div id={`${id}-ports`} role={usb ? 'tabpanel' : undefined} aria-labelledby={usb ? `${id}-ports-tab` : undefined} hidden={tab !== 'ports'}>
        <PortsPanel {...ports} t={portsT} />
      </div>
      {usb && <div id={`${id}-usb`} role="tabpanel" aria-labelledby={`${id}-usb-tab`} hidden={tab !== 'usb'}>
        {usbVisited && <USBPanel {...usb} active={tab === 'usb'} t={usbT} />}
      </div>}
    </div>
    <ConnectionHelp open={help} onClose={() => { setHelp(false) }} connection={connection} usb={usb !== null} t={t} />
  </section>
}

export function CodespacePanelIcon({ size }: PropsRuntime<'sidebar.panellist'>) {
  return <IconLinkOutlineRegular size={size} />
}
