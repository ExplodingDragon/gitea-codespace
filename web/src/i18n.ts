import {createI18n} from 'vue-i18n';
import enUS from '../locales/en-US.json';
import jaJP from '../locales/ja-JP.json';
import koKR from '../locales/ko-KR.json';
import zhCN from '../locales/zh-CN.json';

export type SupportedLocale = 'en-US' | 'zh-CN' | 'ja-JP' | 'ko-KR';

export const supportedLocales: {label: string; value: SupportedLocale}[] = [
  {label: 'English', value: 'en-US'},
  {label: '简体中文', value: 'zh-CN'},
  {label: '日本語', value: 'ja-JP'},
  {label: '한국어', value: 'ko-KR'},
];

function initialLocale(): SupportedLocale {
  const stored = localStorage.getItem('codespace-admin-locale');
  if (supportedLocales.some(({value}) => value === stored)) return stored as SupportedLocale;
  for (const language of navigator.languages) {
    const match = supportedLocales.find(
      ({value}) => value === language || value.startsWith(`${language.split('-')[0]}-`),
    );
    if (match) return match.value;
  }
  return 'en-US';
}

export const i18n = createI18n({
  legacy: false,
  locale: initialLocale(),
  fallbackLocale: 'en-US',
  messages: {'en-US': enUS, 'zh-CN': zhCN, 'ja-JP': jaJP, 'ko-KR': koKR},
});

export function currentLocale(): SupportedLocale {
  return i18n.global.locale.value as SupportedLocale;
}

export function setLocale(locale: SupportedLocale) {
  i18n.global.locale.value = locale;
  localStorage.setItem('codespace-admin-locale', locale);
  document.documentElement.lang = locale;
}

document.documentElement.lang = currentLocale();
