export const NS = 'codespace.ports'

export const en = {
  panel: 'Ports',
  title: 'Ports',
  description: 'Publish services running in this Codespace.',
  refresh: 'Refresh',
  publish: 'Publish port',
  edit: 'Edit',
  save: 'Save',
  cancel: 'Cancel',
  unpublish: 'Unpublish',
  port: 'Port',
  label: 'Label',
  visibility: 'Visibility',
  authenticated: 'Authenticated',
  public: 'Public',
  empty: 'No ports are published.',
  loading: 'Loading ports...',
  retry: 'Retry',
  invalidPort: 'Enter a port between 1 and 65535.',
  invalidLabel: 'Enter a label between 1 and 128 bytes.',
} as const

export type CodespacePortsKey = keyof typeof en
type Dictionary = Record<CodespacePortsKey, string>

export const zh: Dictionary = {
  panel: '端口', title: '端口', description: '发布此 Codespace 中运行的服务。', refresh: '刷新', publish: '发布端口',
  edit: '编辑', save: '保存', cancel: '取消', unpublish: '取消发布', port: '端口', label: '名称', visibility: '可见性',
  authenticated: '需要认证', public: '公开', empty: '尚未发布端口。', loading: '正在加载端口...', retry: '重试',
  invalidPort: '请输入 1 到 65535 之间的端口。', invalidLabel: '请输入 1 到 128 字节的名称。',
}

export const ja: Dictionary = {
  panel: 'ポート', title: 'ポート', description: 'この Codespace で実行中のサービスを公開します。', refresh: '更新', publish: 'ポートを公開',
  edit: '編集', save: '保存', cancel: 'キャンセル', unpublish: '公開を解除', port: 'ポート', label: 'ラベル', visibility: '公開範囲',
  authenticated: '認証が必要', public: '公開', empty: '公開中のポートはありません。', loading: 'ポートを読み込み中...', retry: '再試行',
  invalidPort: '1 から 65535 のポートを入力してください。', invalidLabel: '1 から 128 バイトのラベルを入力してください。',
}

export const es: Dictionary = {
  panel: 'Puertos', title: 'Puertos', description: 'Publica servicios que se ejecutan en este Codespace.', refresh: 'Actualizar', publish: 'Publicar puerto',
  edit: 'Editar', save: 'Guardar', cancel: 'Cancelar', unpublish: 'Dejar de publicar', port: 'Puerto', label: 'Etiqueta', visibility: 'Visibilidad',
  authenticated: 'Autenticado', public: 'Público', empty: 'No hay puertos publicados.', loading: 'Cargando puertos...', retry: 'Reintentar',
  invalidPort: 'Introduce un puerto entre 1 y 65535.', invalidLabel: 'Introduce una etiqueta de entre 1 y 128 bytes.',
}

export const fr: Dictionary = {
  panel: 'Ports', title: 'Ports', description: 'Publiez les services exécutés dans ce Codespace.', refresh: 'Actualiser', publish: 'Publier un port',
  edit: 'Modifier', save: 'Enregistrer', cancel: 'Annuler', unpublish: 'Retirer', port: 'Port', label: 'Libellé', visibility: 'Visibilité',
  authenticated: 'Authentifié', public: 'Public', empty: 'Aucun port publié.', loading: 'Chargement des ports...', retry: 'Réessayer',
  invalidPort: 'Saisissez un port compris entre 1 et 65535.', invalidLabel: 'Saisissez un libellé de 1 à 128 octets.',
}

export const de: Dictionary = {
  panel: 'Ports', title: 'Ports', description: 'Dienste aus diesem Codespace veröffentlichen.', refresh: 'Aktualisieren', publish: 'Port veröffentlichen',
  edit: 'Bearbeiten', save: 'Speichern', cancel: 'Abbrechen', unpublish: 'Freigabe aufheben', port: 'Port', label: 'Bezeichnung', visibility: 'Sichtbarkeit',
  authenticated: 'Authentifiziert', public: 'Öffentlich', empty: 'Keine Ports veröffentlicht.', loading: 'Ports werden geladen...', retry: 'Erneut versuchen',
  invalidPort: 'Geben Sie einen Port zwischen 1 und 65535 ein.', invalidLabel: 'Geben Sie eine Bezeichnung mit 1 bis 128 Byte ein.',
}

export const ru: Dictionary = {
  panel: 'Порты', title: 'Порты', description: 'Публикация сервисов, работающих в этом Codespace.', refresh: 'Обновить', publish: 'Опубликовать порт',
  edit: 'Изменить', save: 'Сохранить', cancel: 'Отмена', unpublish: 'Снять публикацию', port: 'Порт', label: 'Название', visibility: 'Доступ',
  authenticated: 'С аутентификацией', public: 'Публичный', empty: 'Нет опубликованных портов.', loading: 'Загрузка портов...', retry: 'Повторить',
  invalidPort: 'Введите порт от 1 до 65535.', invalidLabel: 'Введите название длиной от 1 до 128 байт.',
}

export const pt: Dictionary = {
  panel: 'Portas', title: 'Portas', description: 'Publique serviços em execução neste Codespace.', refresh: 'Atualizar', publish: 'Publicar porta',
  edit: 'Editar', save: 'Salvar', cancel: 'Cancelar', unpublish: 'Remover publicação', port: 'Porta', label: 'Rótulo', visibility: 'Visibilidade',
  authenticated: 'Autenticado', public: 'Público', empty: 'Nenhuma porta publicada.', loading: 'Carregando portas...', retry: 'Tentar novamente',
  invalidPort: 'Digite uma porta entre 1 e 65535.', invalidLabel: 'Digite um rótulo entre 1 e 128 bytes.',
}
