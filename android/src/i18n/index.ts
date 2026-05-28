/**
 * i18n bootstrap. 从 @fundai/api-client 取 shared messages — 单一
 * 字典点，与 web 端共享。设备语言探测 + i18next standard wiring。
 */

import i18n from 'i18next';
import { initReactI18next } from 'react-i18next';
import { NativeModules, Platform } from 'react-native';

import { messages, type LocaleId } from '@fundai/api-client';

function detectLanguage(): LocaleId {
  if (Platform.OS === 'ios') {
    const settings = NativeModules.SettingsManager?.settings ?? {};
    const lang: string = settings.AppleLocale ?? settings.AppleLanguages?.[0] ?? 'zh-CN';
    return lang.startsWith('zh') ? 'zh-CN' : 'en-US';
  }
  const locale: string = NativeModules.I18nManager?.localeIdentifier ?? 'zh_CN';
  return locale.startsWith('zh') ? 'zh-CN' : 'en-US';
}

void i18n.use(initReactI18next).init({
  lng: detectLanguage(),
  fallbackLng: 'zh-CN',
  interpolation: { escapeValue: false },
  resources: {
    'zh-CN': { translation: messages['zh-CN'] },
    'en-US': { translation: messages['en-US'] },
  },
});

export { i18n };
