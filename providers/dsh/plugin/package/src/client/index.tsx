import type { Context } from '@deepseek-ai/cordis'
import type { MainPanelId } from '@deepseek-ai/dsh-client-ui-layout/client'
import type {} from '@deepseek-ai/dsh-client-locale/client'
import type {} from '@deepseek-ai/dsh-client-ui-renderer/client'
import type {} from '@deepseek-ai/dsh-client-ui-sidebar/client'
import type {} from '@deepseek-ai/dsh-api-gateway/client'
import codespaceRemote from '@gitea/codespace-dsh/remote'
import { PortsPanel, PortsPanelIcon, type PortsPanelInjected } from './PortsPanel.tsx'
import { de, en, es, fr, ja, NS, pt, ru, zh } from './locales.ts'

const PANEL_ID = 'codespace-ports' as MainPanelId

/** Services and UI surfaces required by the Codespace Client contribution. */
export const inject = ['remote', 'slots', 'locale']

/** Mount the generated Remote namespace and the global Ports panel. */
export async function apply(ctx: Context): Promise<() => Promise<void>> {
  const disposeRemote = await ctx.remote.$mount(codespaceRemote)
  const ui = ctx.inject(['remote.codespace', 'slots', 'locale'], (scope) => {
    scope.effect(() => scope.locale.register(NS, { zh, en, ja, es, fr, de, ru, pt }), 'codespace: port dictionaries')
    const t = scope.locale.bind(NS)
    const unwrap = async <T,>(operation: () => Promise<{ ok: true; value: T } | { ok: false; error: { message: string } }>): Promise<T> => {
      const result = await operation()
      if (!result.ok) throw new Error(result.error.message)
      return result.value
    }
    const injected: PortsPanelInjected = {
      list: () => unwrap(() => scope.remote.codespace.list()),
      publish: endpoint => unwrap(() => scope.remote.codespace.publish(endpoint)),
      unpublish: port => unwrap(() => scope.remote.codespace.unpublish(port)),
    }
    scope.effect(() => scope.slots.inject('main', () => scope.slots.register({
      name: 'main', key: PANEL_ID, locale: NS, inject: () => injected,
    }, PortsPanel)), 'codespace: ports panel')
    scope.effect(() => scope.slots.inject('sidebar.panellist', () => scope.slots.register({
      name: 'sidebar.panellist', id: PANEL_ID, order: 20, locale: NS, label: () => t('panel'),
    }, PortsPanelIcon)), 'codespace: ports navigation')
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
