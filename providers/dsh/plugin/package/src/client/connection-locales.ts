export const CONNECTION_NS = 'codespace.connection'

const en = {
  help: 'Connection help', close: 'Close', copy: 'Copy', copied: 'Copied', source: 'USB source name', command: 'Connection command',
  local: 'Run this command on your local computer.', unavailable: 'SSH connection information is unavailable. Use the Codespace page in Gitea.',
  invalidSource: 'Use 1-32 lowercase letters, digits or hyphens, starting and ending with a letter or digit.',
  copyManually: 'Clipboard access is unavailable. Select and copy the command.',
}
export type ConnectionKey = keyof typeof en

export const connectionLocales = {
  en,
  zh: {
    help: '连接帮助', close: '关闭', copy: '复制', copied: '已复制', source: 'USB 来源名称', command: '连接命令',
    local: '请在本机运行此命令。', unavailable: '无法获取 SSH 连接信息，请查看 Gitea 中的 Codespace 页面。',
    invalidSource: '使用 1 到 32 个小写字母、数字或中划线，并以字母或数字开头和结尾。',
    copyManually: '无法访问剪贴板，请选中命令文本手动复制。',
  },
  ja: {
    help: '接続ヘルプ', close: '閉じる', copy: 'コピー', copied: 'コピーしました', source: 'USB ソース名', command: '接続コマンド',
    local: '手元のコンピューターで実行してください。', unavailable: 'SSH 接続情報を取得できません。Gitea の Codespace ページを確認してください。',
    invalidSource: '小文字、数字、ハイフンを 1～32 文字で入力し、先頭と末尾は英数字にしてください。',
    copyManually: 'クリップボードを使用できません。コマンドを選択してコピーしてください。',
  },
  ko: {
    help: '연결 도움말', close: '닫기', copy: '복사', copied: '복사됨', source: 'USB 소스 이름', command: '연결 명령',
    local: '로컬 컴퓨터에서 이 명령을 실행하세요.', unavailable: 'SSH 연결 정보를 사용할 수 없습니다. Gitea의 Codespace 페이지를 확인하세요.',
    invalidSource: '소문자, 숫자, 하이픈 1~32자로 입력하고 영숫자로 시작하고 끝내세요.',
    copyManually: '클립보드를 사용할 수 없습니다. 명령을 선택하여 복사하세요.',
  },
  de: {
    help: 'Verbindungshilfe', close: 'Schließen', copy: 'Kopieren', copied: 'Kopiert', source: 'USB-Quellenname', command: 'Verbindungsbefehl',
    local: 'Führen Sie diesen Befehl auf Ihrem lokalen Computer aus.', unavailable: 'SSH-Verbindungsdaten sind nicht verfügbar. Öffnen Sie die Codespace-Seite in Gitea.',
    invalidSource: 'Verwenden Sie 1–32 Kleinbuchstaben, Ziffern oder Bindestriche; Anfang und Ende müssen alphanumerisch sein.',
    copyManually: 'Die Zwischenablage ist nicht verfügbar. Markieren und kopieren Sie den Befehl.',
  },
  es: {
    help: 'Ayuda de conexión', close: 'Cerrar', copy: 'Copiar', copied: 'Copiado', source: 'Nombre de origen USB', command: 'Comando de conexión',
    local: 'Ejecuta este comando en tu equipo local.', unavailable: 'La información SSH no está disponible. Consulta la página del Codespace en Gitea.',
    invalidSource: 'Usa de 1 a 32 letras minúsculas, números o guiones, empezando y terminando con una letra o un número.',
    copyManually: 'El portapapeles no está disponible. Selecciona y copia el comando.',
  },
  fr: {
    help: 'Aide à la connexion', close: 'Fermer', copy: 'Copier', copied: 'Copié', source: 'Nom de la source USB', command: 'Commande de connexion',
    local: 'Exécutez cette commande sur votre ordinateur local.', unavailable: 'Les informations SSH sont indisponibles. Consultez la page du Codespace dans Gitea.',
    invalidSource: 'Utilisez 1 à 32 lettres minuscules, chiffres ou tirets, avec une lettre ou un chiffre au début et à la fin.',
    copyManually: 'Le presse-papiers est indisponible. Sélectionnez et copiez la commande.',
  },
  ru: {
    help: 'Помощь с подключением', close: 'Закрыть', copy: 'Копировать', copied: 'Скопировано', source: 'Имя источника USB', command: 'Команда подключения',
    local: 'Выполните эту команду на своём компьютере.', unavailable: 'Данные SSH недоступны. Откройте страницу Codespace в Gitea.',
    invalidSource: 'Используйте 1–32 строчные латинские буквы, цифры или дефисы; первый и последний символ должны быть буквой или цифрой.',
    copyManually: 'Буфер обмена недоступен. Выделите и скопируйте команду.',
  },
  pt: {
    help: 'Ajuda de conexão', close: 'Fechar', copy: 'Copiar', copied: 'Copiado', source: 'Nome da origem USB', command: 'Comando de conexão',
    local: 'Execute este comando no seu computador local.', unavailable: 'As informações SSH estão indisponíveis. Consulte a página do Codespace no Gitea.',
    invalidSource: 'Use de 1 a 32 letras minúsculas, números ou hifens, começando e terminando com uma letra ou número.',
    copyManually: 'A área de transferência está indisponível. Selecione e copie o comando.',
  },
} satisfies Record<string, Record<ConnectionKey, string>>
