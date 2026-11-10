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
  ctx.effect(() => ctx.locale.register(NS, { zh, en, ja, es, fr, de, ru, pt }), 'codespace: port dictionaries')
  const t = ctx.locale.bind(NS)
  const unwrap = async <T,>(operation: () => Promise<{ ok: true; value: T } | { ok: false; error: { message: string } }>): Promise<T> => {
    const result = await operation()
    if (!result.ok) throw new Error(result.error.message)
    return result.value
  }
  const injected: PortsPanelInjected = {
    list: () => unwrap(() => ctx.remote.codespace.list()),
    publish: endpoint => unwrap(() => ctx.remote.codespace.publish(endpoint)),
    unpublish: port => unwrap(() => ctx.remote.codespace.unpublish(port)),
  }
  ctx.effect(() => ctx.slots.inject('main', () => ctx.slots.register({
    name: 'main', key: PANEL_ID, locale: NS, inject: () => injected,
  }, PortsPanel)), 'codespace: ports panel')
  ctx.effect(() => ctx.slots.inject('sidebar.panellist', () => ctx.slots.register({
    name: 'sidebar.panellist', id: PANEL_ID, order: 20, locale: NS, label: () => t('panel'),
  }, PortsPanelIcon)), 'codespace: ports navigation')
  return disposeRemote
}
