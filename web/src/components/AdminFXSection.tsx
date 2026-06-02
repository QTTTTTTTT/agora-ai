// AdminFXSection — P1-4 admin web component.
//
// What it does
//
//   - Lists the latest FX rates from /api/admin/fx-rates,
//     grouped by source so an operator can quickly tell whether
//     the daily Yahoo loop is producing fresh rows or whether
//     someone needs to override.
//   - Provides an inline form to write a manual / override row.
//   - Shows the closed currency vocabulary the server supports.
//
// Why a self-contained component
//
//   FX is a sysadmin concern, not a fund-level one. We mount it
//   on the global Admin page rather than per-fund settings so
//   one panel covers the whole platform.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  ApiError,
  formatApiError,
  listAdminFXRates,
  upsertAdminFXRate,
  type FXRateRow,
  type ListFXRatesResponse,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

type SourceFilter = "all" | "manual" | "override" | "yahoo" | "eod";

const FX_REFRESH_MS = 60_000;

const messages: Record<
  Language,
  {
    title: string;
    subtitle: string;
    refresh: string;
    refreshing: string;
    loading: string;
    empty: string;
    errorLoad: string;
    pair: string;
    rate: string;
    rateAt: string;
    source: string;
    formTitle: string;
    formBase: string;
    formQuote: string;
    formRate: string;
    formRatePlaceholder: string;
    formSourceLabel: string;
    sourceManual: string;
    sourceOverride: string;
    formNote: string;
    formNotePlaceholder: string;
    submit: string;
    submitting: string;
    submitSuccess: string;
    formError: string;
    sourceFilterAll: string;
  }
> = {
  "zh-CN": {
    title: "FX 汇率",
    subtitle:
      "系统每 6 小时从 Yahoo 抓取 USD 主导对，操作员可在此手动覆盖。NAV 与现金台账按基金 base_currency 折算时使用最近一次 manual / override / yahoo 的值。",
    refresh: "刷新",
    refreshing: "刷新中…",
    loading: "加载中…",
    empty: "暂无 FX 汇率记录",
    errorLoad: "加载 FX 汇率失败",
    pair: "货币对",
    rate: "汇率",
    rateAt: "观察时间",
    source: "来源",
    formTitle: "手动覆盖汇率",
    formBase: "基础货币",
    formQuote: "报价货币",
    formRate: "汇率（1 base = ? quote）",
    formRatePlaceholder: "例如 7.18",
    formSourceLabel: "来源标记",
    sourceManual: "manual（操作员录入）",
    sourceOverride: "override（覆盖自动抓取）",
    formNote: "备注（可选）",
    formNotePlaceholder: "说明本次覆盖原因，便于审计回溯",
    submit: "提交",
    submitting: "提交中…",
    submitSuccess: "已写入 fx_rates 并记录审计链",
    formError: "提交失败",
    sourceFilterAll: "全部",
  },
  "en-US": {
    title: "FX rates",
    subtitle:
      "The platform fetches USD-anchored pairs from Yahoo every 6 hours; operators can override here. NAV and cash-ledger summaries fall back to the most recent manual / override / yahoo row.",
    refresh: "Refresh",
    refreshing: "Refreshing…",
    loading: "Loading…",
    empty: "No FX rates recorded yet.",
    errorLoad: "Failed to load FX rates",
    pair: "Pair",
    rate: "Rate",
    rateAt: "Observed at",
    source: "Source",
    formTitle: "Manual override",
    formBase: "Base",
    formQuote: "Quote",
    formRate: "Rate (1 base = ? quote)",
    formRatePlaceholder: "e.g. 7.18",
    formSourceLabel: "Source label",
    sourceManual: "manual (operator entered)",
    sourceOverride: "override (replaces a wrong auto fetch)",
    formNote: "Note (optional)",
    formNotePlaceholder: "Explain the override; lands in the audit chain.",
    submit: "Submit",
    submitting: "Submitting…",
    submitSuccess: "Wrote fx_rates and audit log.",
    formError: "Submit failed",
    sourceFilterAll: "All",
  },
};

interface AdminFXSectionProps {
  language?: Language;
}

export function AdminFXSection({ language = "zh-CN" }: AdminFXSectionProps) {
  const t = messages[language];
  const [data, setData] = useState<ListFXRatesResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [filter, setFilter] = useState<SourceFilter>("all");

  // Manual upsert form state.
  const [base, setBase] = useState("USD");
  const [quote, setQuote] = useState("CNY");
  const [rate, setRate] = useState("");
  const [source, setSource] = useState<"manual" | "override">("manual");
  const [note, setNote] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [formMessage, setFormMessage] = useState<string | null>(null);
  const [formError, setFormError] = useState<string | null>(null);

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const resp = await listAdminFXRates({ limit: 100 });
      setData(resp);
    } catch (err) {
      setError(formatApiError(err as ApiError, t.errorLoad));
    } finally {
      setLoading(false);
    }
  }, [t.errorLoad]);

  useEffect(() => {
    void reload();
    const id = window.setInterval(() => {
      void reload();
    }, FX_REFRESH_MS);
    return () => window.clearInterval(id);
  }, [reload]);

  const filteredRates = useMemo(() => {
    if (!data) return [];
    if (filter === "all") return data.rates;
    return data.rates.filter((r) => r.source === filter);
  }, [data, filter]);

  const supportedCurrencies = data?.currencies ?? [
    "USD",
    "CNY",
    "HKD",
    "EUR",
    "JPY",
    "GBP",
    "SGD",
  ];

  const handleSubmit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault();
      setFormError(null);
      setFormMessage(null);
      const numRate = Number(rate);
      if (!Number.isFinite(numRate) || numRate <= 0) {
        setFormError(t.formError + ": rate > 0");
        return;
      }
      setSubmitting(true);
      try {
        await upsertAdminFXRate({
          base,
          quote,
          rate: numRate,
          source,
          note: note.trim() || undefined,
        });
        setFormMessage(t.submitSuccess);
        setRate("");
        setNote("");
        await reload();
      } catch (err) {
        setFormError(formatApiError(err as ApiError, t.formError));
      } finally {
        setSubmitting(false);
      }
    },
    [base, quote, rate, source, note, reload, t],
  );

  return (
    <section className="admin-fx-section" aria-labelledby="admin-fx-title">
      <header className="admin-fx-header">
        <div>
          <h2 id="admin-fx-title">{t.title}</h2>
          <p className="admin-fx-subtitle">{t.subtitle}</p>
        </div>
        <button
          type="button"
          onClick={() => void reload()}
          disabled={loading}
          className="admin-fx-refresh"
        >
          {loading ? t.refreshing : t.refresh}
        </button>
      </header>

      <form className="admin-fx-form" onSubmit={handleSubmit}>
        <fieldset disabled={submitting}>
          <legend>{t.formTitle}</legend>
          <div className="admin-fx-form-row">
            <label>
              <span>{t.formBase}</span>
              <select value={base} onChange={(e) => setBase(e.target.value)}>
                {supportedCurrencies.map((c) => (
                  <option key={c} value={c}>
                    {c}
                  </option>
                ))}
              </select>
            </label>
            <label>
              <span>{t.formQuote}</span>
              <select value={quote} onChange={(e) => setQuote(e.target.value)}>
                {supportedCurrencies.map((c) => (
                  <option key={c} value={c}>
                    {c}
                  </option>
                ))}
              </select>
            </label>
            <label className="admin-fx-form-rate">
              <span>{t.formRate}</span>
              <input
                type="number"
                step="0.000001"
                min="0"
                value={rate}
                placeholder={t.formRatePlaceholder}
                onChange={(e) => setRate(e.target.value)}
                required
              />
            </label>
            <label>
              <span>{t.formSourceLabel}</span>
              <select
                value={source}
                onChange={(e) => setSource(e.target.value as "manual" | "override")}
              >
                <option value="manual">{t.sourceManual}</option>
                <option value="override">{t.sourceOverride}</option>
              </select>
            </label>
          </div>
          <label className="admin-fx-form-note">
            <span>{t.formNote}</span>
            <textarea
              value={note}
              onChange={(e) => setNote(e.target.value)}
              placeholder={t.formNotePlaceholder}
              rows={2}
            />
          </label>
          <div className="admin-fx-form-actions">
            <button type="submit" disabled={submitting}>
              {submitting ? t.submitting : t.submit}
            </button>
            {formMessage && <span className="admin-fx-form-success">{formMessage}</span>}
            {formError && <span className="admin-fx-form-error">{formError}</span>}
          </div>
        </fieldset>
      </form>

      <div className="admin-fx-list" role="region" aria-live="polite">
        <div className="admin-fx-filters">
          {(["all", "manual", "override", "yahoo", "eod"] as SourceFilter[]).map((f) => (
            <button
              key={f}
              type="button"
              className={f === filter ? "active" : ""}
              onClick={() => setFilter(f)}
            >
              {f === "all" ? t.sourceFilterAll : f}
            </button>
          ))}
        </div>
        {loading && !data && <p className="admin-fx-state">{t.loading}</p>}
        {error && <p className="admin-fx-error">{t.errorLoad}: {error}</p>}
        {!loading && filteredRates.length === 0 && (
          <p className="admin-fx-state">{t.empty}</p>
        )}
        {filteredRates.length > 0 && (
          <table className="admin-fx-table">
            <thead>
              <tr>
                <th scope="col">{t.pair}</th>
                <th scope="col">{t.rate}</th>
                <th scope="col">{t.rateAt}</th>
                <th scope="col">{t.source}</th>
              </tr>
            </thead>
            <tbody>
              {filteredRates.map((row) => (
                <FXRow key={`${row.base}-${row.quote}-${row.rate_at}-${row.source}`} row={row} />
              ))}
            </tbody>
          </table>
        )}
      </div>
    </section>
  );
}

function FXRow({ row }: { row: FXRateRow }) {
  return (
    <tr>
      <td>
        {row.base}/{row.quote}
      </td>
      <td className="numeric">{row.rate.toFixed(6)}</td>
      <td>{formatDate(row.rate_at)}</td>
      <td>
        <span className={`fx-source fx-source-${row.source}`}>{row.source}</span>
      </td>
    </tr>
  );
}

function formatDate(iso: string): string {
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}

export default AdminFXSection;
