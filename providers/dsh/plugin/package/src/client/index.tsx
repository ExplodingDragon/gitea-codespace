import type { Context } from '@deepseek-ai/cordis'
import type { MainPanelId } from '@deepseek-ai/dsh-client-ui-layout/client'
import type {} from '@deepseek-ai/dsh-client-locale/client'
import type {} from '@deepseek-ai/dsh-client-ui-renderer/client'
import type {} from '@deepseek-ai/dsh-client-ui-sidebar/client'
import type {} from '@deepseek-ai/dsh-api-gateway/client'
import codespaceRemote from '@gitea/codespace-dsh/remote'
import { CodespacePanel, CodespacePanelIcon, type CodespacePanelInjected } from './CodespacePanel.tsx'
import { de, en, es, fr, ja, NS, pt, ru, zh } from './locales.ts'
import { USB_NS, usbLocales } from './usb-locales.ts'
import type { SSHConnection } from '../types.ts'
import { CONNECTION_NS, connectionLocales } from './connection-locales.ts'

const PANEL_ID = 'codespace' as MainPanelId

/** Services and UI surfaces required by the Codespace Client contribution. */
export const inject = ['remote', 'slots', 'locale']

/** Mount the generated Remote namespace and the Codespace management panel. */
export async function apply(ctx: Context): Promise<() => Promise<void>> {
  const disposeRemote = await ctx.remote.$mount(codespaceRemote)
  const ui = ctx.inject(['remote.codespace', 'slots', 'locale'], async (scope) => {
    scope.effect(() => scope.locale.register(NS, { zh, en, ja, es, fr, de, ru, pt }), 'codespace: port dictionaries')
    const unwrap = async <T,>(operation: () => Promise<{ ok: true; value: T } | { ok: false; error: { message: string } }>): Promise<T> => {
      const result = await operation()
      if (!result.ok) throw new Error(result.error.message)
      return result.value
    }
    scope.effect(() => scope.locale.register(CONNECTION_NS, connectionLocales), 'codespace: connection dictionaries')
    let connection: SSHConnection | null = null
    try {
      connection = await unwrap(() => scope.remote.codespace.sshConnection())
    } catch (error) {
      console.warn('Codespace SSH connection query failed', error)
    }
    let usbAvailable = false
    try {
      usbAvailable = await unwrap(() => scope.remote.codespace.usbAvailable())
    } catch (error) {
      console.warn('Codespace USB capability query failed', error)
    }
    scope.effect(() => scope.locale.register(USB_NS, usbLocales), 'codespace: USB dictionaries')
    const injected: CodespacePanelInjected = {
      connection,
      portsT: scope.locale.bind(NS),
      usbT: scope.locale.bind(USB_NS),
      ports: {
        list: () => unwrap(() => scope.remote.codespace.list()),
        publish: endpoint => unwrap(() => scope.remote.codespace.publish(endpoint)),
        unpublish: port => unwrap(() => scope.remote.codespace.unpublish(port)),
      },
      usb: usbAvailable ? {
        status: signal => unwrap(() => scope.remote.codespace.usbStatus(signal)),
        list: signal => unwrap(() => scope.remote.codespace.usbList(signal)),
        attach: (busId, source) => unwrap(() => scope.remote.codespace.usbAttach(busId, source)),
        detach: port => unwrap(() => scope.remote.codespace.usbDetach(port)),
      } : null,
    }
    scope.effect(() => scope.slots.inject('main', () => scope.slots.register({
      name: 'main', key: PANEL_ID, locale: CONNECTION_NS, inject: () => injected,
    }, CodespacePanel)), 'codespace: management panel')
    scope.effect(() => scope.slots.inject('sidebar.panellist', () => scope.slots.register({
      name: 'sidebar.panellist', id: PANEL_ID, order: 20, label: () => 'Codespace',
    }, CodespacePanelIcon)), 'codespace: navigation')
  })
  try {
    await ui
  } catch (error) {
    await ui.dispose()
    await disposeRemote()
    throw error
  }
  return async () => {
    await ui.dispose()
    await disposeRemote()
  }
}
