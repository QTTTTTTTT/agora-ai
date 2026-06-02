// FundBaseCurrencySection — P1-4 web component.
//
// What it does
//
//   Lets the fund owner pick the reporting currency. The server
//   persists it on funds.base_currency; the NAV aggregator and
//   cash_ledger summary then convert every position + cash row
//   into this currency before showing totals.
//
// UX choices
//
//   - Read the current value from props (the parent fund-settings
//     page already loads the fund). No duplicate fetch.
//   - Disable the Save button until the dropdown actually changes.
//     The server-side handler also no-ops on equality, but the
//     UI guard avoids a flash of "Saving…" for a clearly idle
//     interaction.
//   - Surface a one-line warning under the dropdown explaining
//     that switching currencies materially changes every NAV the
//     fund will display from now on. We don't lock it behind a
//     confirm modal because the operation is fully reversible
//     (just pick a different currency) and the UX of a modal
//     for a setting click is heavy-handed.

import { useState } from "react";

import {
  ApiError,
  formatApiError,
  updateFundBaseCurrency,
} from "../lib/api";

const SUPPORTED_BASE_CURRENCIES = [
  "USD",
  "CNY",
  "HKD",
  "EUR",
  "JPY",
  "GBP",
  "SGD",
] as const;

type Language = "zh-CN" | "en-US";

const messages: Record<
  Language,
  {
    label: string;
    hint: string;
    save: string;
    saving: string;
    saved: string;
    errorPrefix: string;
  }
> = {
  "zh-CN": {
    label: "基金报告币种",
    hint: "改为非 USD 后，系统将按 USD-anchored 汇率把所有持仓和现金折算到该币种再展示 NAV。",
    save: "保存",
    saving: "保存中…",
    saved: "已保存",
    errorPrefix: "保存失败",
  },
  "en-US": {
    label: "Reporting currency",
    hint: "Switching to a non-USD base will convert every position and cash bucket via the latest USD-anchored rate before showing NAV.",
    save: "Save",
    saving: "Saving…",
    saved: "Saved.",
    errorPrefix: "Save failed",
  },
};

interface FundBaseCurrencySectionProps {
  fundId: string;
  currentBaseCurrency: string;
  language?: Language;
  onSaved?: (baseCurrency: string) => void;
}

export function FundBaseCurrencySection({
  fundId,
  currentBaseCurrency,
  language = "zh-CN",
  onSaved,
}: FundBaseCurrencySectionProps) {
  const t = messages[language];
  const [value, setValue] = useState(currentBaseCurrency || "USD");
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const dirty = value !== (currentBaseCurrency || "USD");

  const handleSave = async () => {
    setError(null);
    setMessage(null);
    setSaving(true);
    try {
      await updateFundBaseCurrency(fundId, value);
      setMessage(t.saved);
      onSaved?.(value);
    } catch (err) {
      setError(`${t.errorPrefix}: ${formatApiError(err as ApiError, t.errorPrefix)}`);
    } finally {
      setSaving(false);
    }
  };

  return (
    <section className="fund-base-currency-section">
      <label htmlFor={`fund-base-currency-${fundId}`} className="fund-base-currency-label">
        {t.label}
      </label>
      <div className="fund-base-currency-row">
        <select
          id={`fund-base-currency-${fundId}`}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          disabled={saving}
        >
          {SUPPORTED_BASE_CURRENCIES.map((c) => (
            <option key={c} value={c}>
              {c}
            </option>
          ))}
        </select>
        <button
          type="button"
          onClick={() => void handleSave()}
          disabled={!dirty || saving}
        >
          {saving ? t.saving : t.save}
        </button>
        {message && <span className="fund-base-currency-success">{message}</span>}
      </div>
      <p className="fund-base-currency-hint">{t.hint}</p>
      {error && <p className="fund-base-currency-error">{error}</p>}
    </section>
  );
}

export default FundBaseCurrencySection;
