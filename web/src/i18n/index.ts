// web/src/i18n/index.ts — react-i18next bootstrap.
//
// WHY THIS EXISTS
// ---------------
// Today the codebase has ~100 components each carrying a hand-rolled
//
//     const copy = useMemo(() =>
//       language === 'en-US' ? { … en … } : { … zh … }, [language])
//
// block. The pattern works but
//   - it scatters translations across 100 files (impossible to spot drift),
//   - it ties translations to component lifecycle (a key change forces a
//     useMemo re-evaluation downstream),
//   - and reviewers can't see what changed in the translation surface in
//     a PR diff because everything's tangled with JSX.
//
// react-i18next is the canonical answer: translations live in JSON-style
// dictionaries keyed by namespace, components only see `t('forgotPassword.title')`.
// This module bootstraps i18next once at app start with the namespaces we've
// migrated so far, and `useI18nLanguageSync` (lib/preferences.tsx ->
// see useI18nLanguageSync below) keeps it in lockstep with the existing
// `useAppPreferences().language` so the migration is incremental — pages
// that haven't been migrated still use their hand-rolled `copy` blocks
// from the same language source, no UX seam.
//
// MIGRATION GUIDANCE
// ------------------
// To migrate a page:
//   1. Move its `copy` object into web/src/i18n/locales/<lang>/<namespace>.ts
//      (one namespace per logical page / feature is the norm).
//   2. Add the namespace to RESOURCES below.
//   3. In the page, replace
//        const copy = useMemo(() => language === 'en-US' ? {...} : {...}, [language])
//        copy.actions.submit
//      with
//        const { t } = useTranslation('<namespace>')
//        t('actions.submit')
//   4. Drop the now-unused `useAppPreferences` / `language` if nothing else
//      in the component needs it.
//
// When EVERY page has migrated, lib/preferences.tsx's `language` field
// can be deleted and i18next becomes the single source of truth. Until
// then the two co-exist deliberately.

import i18n from "i18next";
import { initReactI18next } from "react-i18next";

import forgotPasswordEn from "./locales/en-US/forgotPassword";
import forgotPasswordZh from "./locales/zh-CN/forgotPassword";
import validationEn from "./locales/en-US/validation";
import validationZh from "./locales/zh-CN/validation";

export type SupportedLanguage = "zh-CN" | "en-US";

const RESOURCES = {
  "zh-CN": {
    forgotPassword: forgotPasswordZh,
    validation: validationZh,
  },
  "en-US": {
    forgotPassword: forgotPasswordEn,
    validation: validationEn,
  },
} as const;

void i18n.use(initReactI18next).init({
  resources: RESOURCES,
  lng: "zh-CN",
  fallbackLng: "zh-CN",
  // The current pattern in the codebase passes literal English / Chinese
  // strings into JSX without escaping; matching that here means a
  // translation containing `<` won't accidentally HTML-escape on render.
  // i18next's default would be `true`, which would silently break any
  // copy that contains `&`, `<`, `>` etc. — turning it off gives us the
  // same behaviour the hand-rolled `copy` blocks have today.
  interpolation: {
    escapeValue: false,
  },
  // We don't use namespaces for fallback resolution — every key is
  // explicitly addressed via `useTranslation('namespace')`.
  defaultNS: false,
  ns: Object.keys(RESOURCES["zh-CN"]),
  // Keep startup synchronous: bundling all locale files at compile time
  // means we don't need to defer rendering on a network fetch.
  initImmediate: false,
});

export default i18n;
