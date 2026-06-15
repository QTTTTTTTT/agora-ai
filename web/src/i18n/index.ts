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
// W4-26 — i18n coverage of 5 core pages. Each new namespace
// mirrors the previously-inline `copy` block of the matching
// page; see the per-page TSX for the consumer side.
import auditLogEn from "./locales/en-US/auditLog";
import auditLogZh from "./locales/zh-CN/auditLog";
import resetPasswordEn from "./locales/en-US/resetPassword";
import resetPasswordZh from "./locales/zh-CN/resetPassword";
import verifyEmailEn from "./locales/en-US/verifyEmail";
import verifyEmailZh from "./locales/zh-CN/verifyEmail";
import walletEn from "./locales/en-US/wallet";
import walletZh from "./locales/zh-CN/wallet";
import kycEn from "./locales/en-US/kyc";
import kycZh from "./locales/zh-CN/kyc";
// W5-1 — deep i18n migration of TradeHistory's full string surface
// (column headers, status / side / order-type dictionaries,
// modify-order modal labels, slice splitter labels). The shape
// mirrors the previously-inline `copy` block one-for-one.
import tradeHistoryEn from "./locales/en-US/tradeHistory";
import tradeHistoryZh from "./locales/zh-CN/tradeHistory";
// W7-2 — Dashboard.tsx i18n migration. Function-valued strings
// (priceFreshness.{secondsAgo,minutesAgo,...}) are stored as
// interpolation templates with `{{n}}`; the consumer renders via
// `t("priceFreshness.secondsAgo", { n })` so the bundle stays
// JSON-serializable for the W6-3 parity guard.
import dashboardEn from "./locales/en-US/dashboard";
import dashboardZh from "./locales/zh-CN/dashboard";
// W8-3 — MultiFundOverview i18n migration. Small surface (~30
// keys), no function-valued translations, mirrors the structure
// of the previously-inline `copy = isEnglish ? {...} : {...}`
// ternary one-for-one.
import multiFundOverviewEn from "./locales/en-US/multiFundOverview";
import multiFundOverviewZh from "./locales/zh-CN/multiFundOverview";
// W9-3 — FundPerformance i18n migration. Mirrors the previously-
// inline `COPY: Record<AppLanguage, PageCopy>` table; nested
// `kpis`, `windowLabels`, `contributorsCols` objects are
// preserved verbatim so the consumer keeps using the same
// `copy.kpis.totalReturn` access shape.
import fundPerformanceEn from "./locales/en-US/fundPerformance";
import fundPerformanceZh from "./locales/zh-CN/fundPerformance";
// W10-3 — MemoryCenter i18n migration. Largest surface so far
// (~50 keys including 4 enum-keyed nested objects: focusOptions,
// viewModes, layerLabels, layerIcons). Same `as const` pattern
// as the prior page migrations so the consumer keeps narrow
// literal-string types when indexing by typed enum.
import memoryCenterEn from "./locales/en-US/memoryCenter";
import memoryCenterZh from "./locales/zh-CN/memoryCenter";
// W11-3 — DecisionCenter i18n migration. New high-water mark for
// surface size (~150 keys spread across 11 enum-keyed nested
// objects: traceStepStatus, workflowSteps, summaryLabel,
// riskVerdict, planStatus, actionType, checkResult,
// executionStatuses, positionSides, openClose, columns).
// Function-valued translations (`livePriceDriftWarning(percent)`,
// `checkName(index)`) are flattened to `{{percent}}` /
// `{{index}}` interpolation templates.
import decisionCenterEn from "./locales/en-US/decisionCenter";
import decisionCenterZh from "./locales/zh-CN/decisionCenter";
// Step 6 — apiErrors namespace centralises every user-visible error
// string emitted by web/src/lib/api.ts so en-US users no longer see
// Chinese fallbacks when fetch / 401 / timeout / parse paths trip.
import apiErrorsEn from "./locales/en-US/apiErrors";
import apiErrorsZh from "./locales/zh-CN/apiErrors";

export type SupportedLanguage = "zh-CN" | "en-US";

const RESOURCES = {
  "zh-CN": {
    forgotPassword: forgotPasswordZh,
    validation: validationZh,
    auditLog: auditLogZh,
    resetPassword: resetPasswordZh,
    verifyEmail: verifyEmailZh,
    wallet: walletZh,
    kyc: kycZh,
    tradeHistory: tradeHistoryZh,
    dashboard: dashboardZh,
    multiFundOverview: multiFundOverviewZh,
    fundPerformance: fundPerformanceZh,
    memoryCenter: memoryCenterZh,
    decisionCenter: decisionCenterZh,
    apiErrors: apiErrorsZh,
  },
  "en-US": {
    forgotPassword: forgotPasswordEn,
    validation: validationEn,
    auditLog: auditLogEn,
    resetPassword: resetPasswordEn,
    verifyEmail: verifyEmailEn,
    wallet: walletEn,
    kyc: kycEn,
    tradeHistory: tradeHistoryEn,
    dashboard: dashboardEn,
    multiFundOverview: multiFundOverviewEn,
    fundPerformance: fundPerformanceEn,
    memoryCenter: memoryCenterEn,
    decisionCenter: decisionCenterEn,
    apiErrors: apiErrorsEn,
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
