export const USB_NS = 'codespace.usb'

const en = {
  unknownSource: 'Unidentified source',
  title: 'USB Devices', refresh: 'Refresh', attach: 'Attach', detach: 'Detach',
  loading: 'Loading USB status...', forwarded: 'SSH USB forwarding is active', disconnected: 'SSH USB forwarding is not connected',
  empty: 'No USB devices.', unattached: 'Not imported', sourceDisconnected: 'Source disconnected', cancel: 'Cancel', port: 'VHCI port',
  attached: 'Imported', stale: 'USB status is out of date. Refresh to continue.',
  operationFailed: 'USB operation failed', statusFailed: 'USB status refresh failed', listFailed: 'Remote device list could not be refreshed',
  detachWarning: 'This interrupts applications using the device. Unmount disks first.',
}

export type USBKey = keyof typeof en
export const usbLocales = {
  en,
  zh: {
    unknownSource: '来源未知',
    title: 'USB 设备', refresh: '刷新', attach: '连接', detach: '断开',
    loading: '正在读取 USB 状态...', forwarded: 'SSH USB 转发已建立', disconnected: 'SSH USB 转发未连接',
    empty: '暂无 USB 设备。', unattached: '未导入', sourceDisconnected: '来源已断开', cancel: '取消', port: 'VHCI 端口',
    attached: '已导入', stale: 'USB 状态未更新，请刷新后操作。',
    operationFailed: 'USB 操作失败', statusFailed: 'USB 状态刷新失败', listFailed: '远端设备清单刷新失败',
    detachWarning: '这会中断正在使用设备的应用。磁盘请先卸载。',
  },
  ja: {
    unknownSource: '不明な接続元',
    title: 'USB デバイス', refresh: '更新', attach: '接続', detach: '切断',
    loading: 'USB 状態を読み込み中...', forwarded: 'SSH USB 転送が有効です', disconnected: 'SSH USB 転送が接続されていません',
    empty: 'USB デバイスはありません。', unattached: '未インポート', sourceDisconnected: '接続元が切断されました', cancel: 'キャンセル', port: 'VHCI ポート',
    attached: 'インポート済み', stale: 'USB 状態が更新されていません。更新してから操作してください。',
    operationFailed: 'USB 操作に失敗しました', statusFailed: 'USB 状態の更新に失敗しました', listFailed: 'リモートデバイス一覧を更新できませんでした',
    detachWarning: 'デバイスを使用中のアプリケーションが中断されます。先にディスクをアンマウントしてください。',
  },
  es: {
    unknownSource: 'Origen desconocido',
    title: 'Dispositivos USB', refresh: 'Actualizar', attach: 'Conectar', detach: 'Desconectar',
    loading: 'Cargando estado USB...', forwarded: 'Reenvío USB SSH activo', disconnected: 'Reenvío USB SSH desconectado',
    empty: 'No hay dispositivos USB.', unattached: 'Sin importar', sourceDisconnected: 'Origen desconectado', cancel: 'Cancelar', port: 'Puerto VHCI',
    attached: 'Importado', stale: 'El estado USB no está actualizado. Actualice para continuar.',
    operationFailed: 'Error en la operación USB', statusFailed: 'Error al actualizar el estado USB', listFailed: 'No se pudo actualizar la lista de dispositivos remotos',
    detachWarning: 'Esto interrumpe las aplicaciones que usan el dispositivo. Desmonte los discos primero.',
  },
  fr: {
    unknownSource: 'Source inconnue',
    title: 'Périphériques USB', refresh: 'Actualiser', attach: 'Connecter', detach: 'Déconnecter',
    loading: 'Chargement de l’état USB...', forwarded: 'Transfert USB SSH actif', disconnected: 'Transfert USB SSH déconnecté',
    empty: 'Aucun périphérique USB.', unattached: 'Non importé', sourceDisconnected: 'Source déconnectée', cancel: 'Annuler', port: 'Port VHCI',
    attached: 'Importé', stale: 'L’état USB n’est pas à jour. Actualisez pour continuer.',
    operationFailed: 'Échec de l’opération USB', statusFailed: 'Échec de l’actualisation de l’état USB', listFailed: 'Impossible d’actualiser la liste des périphériques distants',
    detachWarning: 'Cela interrompt les applications utilisant le périphérique. Démontez d’abord les disques.',
  },
  de: {
    unknownSource: 'Unbekannte Quelle',
    title: 'USB-Geräte', refresh: 'Aktualisieren', attach: 'Verbinden', detach: 'Trennen',
    loading: 'USB-Status wird geladen...', forwarded: 'SSH-USB-Weiterleitung aktiv', disconnected: 'SSH-USB-Weiterleitung nicht verbunden',
    empty: 'Keine USB-Geräte.', unattached: 'Nicht importiert', sourceDisconnected: 'Quelle getrennt', cancel: 'Abbrechen', port: 'VHCI-Port',
    attached: 'Importiert', stale: 'Der USB-Status ist veraltet. Zum Fortfahren aktualisieren.',
    operationFailed: 'USB-Vorgang fehlgeschlagen', statusFailed: 'USB-Status konnte nicht aktualisiert werden', listFailed: 'Die Liste entfernter Geräte konnte nicht aktualisiert werden',
    detachWarning: 'Anwendungen, die das Gerät verwenden, werden unterbrochen. Datenträger zuerst aushängen.',
  },
  ru: {
    unknownSource: 'Неизвестный источник',
    title: 'USB-устройства', refresh: 'Обновить', attach: 'Подключить', detach: 'Отключить',
    loading: 'Загрузка состояния USB...', forwarded: 'Перенаправление USB по SSH активно', disconnected: 'Перенаправление USB по SSH не подключено',
    empty: 'Нет USB-устройств.', unattached: 'Не импортировано', sourceDisconnected: 'Источник отключён', cancel: 'Отмена', port: 'Порт VHCI',
    attached: 'Импортировано', stale: 'Состояние USB устарело. Обновите его для продолжения.',
    operationFailed: 'Ошибка операции USB', statusFailed: 'Не удалось обновить состояние USB', listFailed: 'Не удалось обновить список удалённых устройств',
    detachWarning: 'Это прервёт работу приложений, использующих устройство. Сначала размонтируйте диски.',
  },
  pt: {
    unknownSource: 'Origem desconhecida',
    title: 'Dispositivos USB', refresh: 'Atualizar', attach: 'Conectar', detach: 'Desconectar',
    loading: 'Carregando estado USB...', forwarded: 'Encaminhamento USB SSH ativo', disconnected: 'Encaminhamento USB SSH desconectado',
    empty: 'Nenhum dispositivo USB.', unattached: 'Não importado', sourceDisconnected: 'Origem desconectada', cancel: 'Cancelar', port: 'Porta VHCI',
    attached: 'Importado', stale: 'O estado USB está desatualizado. Atualize para continuar.',
    operationFailed: 'Falha na operação USB', statusFailed: 'Falha ao atualizar o estado USB', listFailed: 'Não foi possível atualizar a lista de dispositivos remotos',
    detachWarning: 'Isto interrompe as aplicações que usam o dispositivo. Desmonte os discos primeiro.',
  },
} satisfies Record<string, Record<USBKey, string>>
