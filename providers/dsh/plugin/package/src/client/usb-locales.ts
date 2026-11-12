export const USB_NS = 'codespace.usb'

const en = {
  unknownSource: 'Unidentified source',
  title: 'USB Devices', refresh: 'Refresh', list: 'List remote devices', attach: 'Attach', detach: 'Detach',
  loading: 'Loading USB status...', forwarded: 'SSH USB forwarding is active', disconnected: 'SSH USB forwarding is not connected',
  imported: 'Imported devices', exported: 'Exported devices', emptyImported: 'No imported devices.', emptyExported: 'No exported devices.', port: 'VHCI port',
}

export type USBKey = keyof typeof en
export const usbLocales = {
  en,
  zh: {
    unknownSource: '来源未知',
    title: 'USB 设备', refresh: '刷新', list: '查询远端设备', attach: '连接', detach: '断开',
    loading: '正在读取 USB 状态...', forwarded: 'SSH USB 转发已建立', disconnected: 'SSH USB 转发未连接',
    imported: '已导入的设备', exported: '可导出的设备', emptyImported: '没有已导入的设备。', emptyExported: '没有可导出的设备。', port: 'VHCI 端口',
  },
  ja: {
    unknownSource: '不明な接続元',
    title: 'USB デバイス', refresh: '更新', list: 'リモートデバイス一覧', attach: '接続', detach: '切断',
    loading: 'USB 状態を読み込み中...', forwarded: 'SSH USB 転送が有効です', disconnected: 'SSH USB 転送が接続されていません',
    imported: 'インポート済みデバイス', exported: 'エクスポートされたデバイス', emptyImported: 'インポート済みデバイスはありません。', emptyExported: 'エクスポートされたデバイスはありません。', port: 'VHCI ポート',
  },
  es: {
    unknownSource: 'Origen desconocido',
    title: 'Dispositivos USB', refresh: 'Actualizar', list: 'Listar dispositivos remotos', attach: 'Conectar', detach: 'Desconectar',
    loading: 'Cargando estado USB...', forwarded: 'Reenvío USB SSH activo', disconnected: 'Reenvío USB SSH desconectado',
    imported: 'Dispositivos importados', exported: 'Dispositivos exportados', emptyImported: 'No hay dispositivos importados.', emptyExported: 'No hay dispositivos exportados.', port: 'Puerto VHCI',
  },
  fr: {
    unknownSource: 'Source inconnue',
    title: 'Périphériques USB', refresh: 'Actualiser', list: 'Lister les périphériques distants', attach: 'Connecter', detach: 'Déconnecter',
    loading: 'Chargement de l’état USB...', forwarded: 'Transfert USB SSH actif', disconnected: 'Transfert USB SSH déconnecté',
    imported: 'Périphériques importés', exported: 'Périphériques exportés', emptyImported: 'Aucun périphérique importé.', emptyExported: 'Aucun périphérique exporté.', port: 'Port VHCI',
  },
  de: {
    unknownSource: 'Unbekannte Quelle',
    title: 'USB-Geräte', refresh: 'Aktualisieren', list: 'Remote-Geräte anzeigen', attach: 'Verbinden', detach: 'Trennen',
    loading: 'USB-Status wird geladen...', forwarded: 'SSH-USB-Weiterleitung aktiv', disconnected: 'SSH-USB-Weiterleitung nicht verbunden',
    imported: 'Importierte Geräte', exported: 'Exportierte Geräte', emptyImported: 'Keine importierten Geräte.', emptyExported: 'Keine exportierten Geräte.', port: 'VHCI-Port',
  },
  ru: {
    unknownSource: 'Неизвестный источник',
    title: 'USB-устройства', refresh: 'Обновить', list: 'Удалённые устройства', attach: 'Подключить', detach: 'Отключить',
    loading: 'Загрузка состояния USB...', forwarded: 'Перенаправление USB по SSH активно', disconnected: 'Перенаправление USB по SSH не подключено',
    imported: 'Импортированные устройства', exported: 'Экспортированные устройства', emptyImported: 'Нет импортированных устройств.', emptyExported: 'Нет экспортированных устройств.', port: 'Порт VHCI',
  },
  pt: {
    unknownSource: 'Origem desconhecida',
    title: 'Dispositivos USB', refresh: 'Atualizar', list: 'Listar dispositivos remotos', attach: 'Conectar', detach: 'Desconectar',
    loading: 'Carregando estado USB...', forwarded: 'Encaminhamento USB SSH ativo', disconnected: 'Encaminhamento USB SSH desconectado',
    imported: 'Dispositivos importados', exported: 'Dispositivos exportados', emptyImported: 'Nenhum dispositivo importado.', emptyExported: 'Nenhum dispositivo exportado.', port: 'Porta VHCI',
  },
} satisfies Record<string, Record<USBKey, string>>
