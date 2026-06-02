// AdminFactorExposureSection — admin UI for the S7 / P3-1
// instrument_factor_loadings calibration store.
//
// Capability surface
//
//   - List + filter by factor / instrument_key.
//   - Inline upsert form: (instrument_key, factor, asof, loading,
//     source, note). Server enforces loading ∈ [-10, 10] and the
//     factor enum.
//   - Per-row delete with confirmation.
//
// The page is deliberately compact: factor loadings are mostly
// written by the Quant Lab batch (source=computed). Manual upserts
// are for typo fixes and emergency overrides, so we optimise for
// "fast scan + correct one row" rather than dialog-heavy
// workflows.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  deleteAdminFactorLoading,
  formatApiError,
  listAdminFactorLoadings,
  upsertAdminFactorLoading,
  type UpsertFactorLoadingInput,
} from "../lib/api";
import type {
  Factor,
  InstrumentFactorLoading,
  LoadingSource,
} from "@fundai/api-client";
import { ALL_FACTORS } from "@fundai/api-client";

type Language = "zh-CN" | "en-US";

interface Messages {
  panelTitle: string;
  panelSubtitle: string;
  refresh: string;
  loading: string;
  empty: string;
  error: string;
  filterFactor: string;
  filterAll: string;
  filterInstrument: string;
  filterInstrumentPlaceholder: string;
  adminListTitle: string;
  adminInstrumentKey: string;
  adminFactorLabel: string;
  adminAsOfLabel: string;
  adminLoadingLabel: string;
  adminSourceLabel: string;
  adminNoteLabel: string;
  adminUpdatedAtLabel: string;
  adminUpsertTitle: string;
  adminUpsertSubmit: string;
  adminUpsertSubmitting: string;
  adminDeleteButton: string;
  adminDeleteConfirm: string;
  sourceManual: string;
  sourceEastMoney: string;
  sourceMSCI: string;
  sourceComputed: string;
  sourceOverride: string;
  factorSize: string;
  factorValue: string;
  factorMomentum: string;
  factorQuality: string;
  factorLowVol: string;
  factorMarketBeta: string;
}

const messages: Record<Language, Messages> = {
  "zh-CN": {
    panelTitle: "因子载荷管理",
    panelSubtitle:
      "维护 instrument_factor_loadings 表。Quant Lab 的批量计算 (source=computed) 自动写入；这里用于人工录入、紧急覆写与第三方供应数据校对。loading 取值范围 [-10, 10]，越界写入会被后端 400 拒绝。",
    refresh: "刷新",
    loading: "加载中…",
    empty: "暂无校准记录",
    error: "加载失败",
    filterFactor: "因子",
    filterAll: "全部因子",
    filterInstrument: "标的 Key",
    filterInstrumentPlaceholder: "如 US:AAPL",
    adminListTitle: "当前校准记录",
    adminInstrumentKey: "标的 Key",
    adminFactorLabel: "因子",
    adminAsOfLabel: "校准日 (YYYY-MM-DD)",
    adminLoadingLabel: "载荷",
    adminSourceLabel: "来源",
    adminNoteLabel: "备注",
    adminUpdatedAtLabel: "更新时间",
    adminUpsertTitle: "新增 / 更新载荷",
    adminUpsertSubmit: "保存",
    adminUpsertSubmitting: "保存中…",
    adminDeleteButton: "删除",
    adminDeleteConfirm: "确认删除这条载荷记录？",
    sourceManual: "人工",
    sourceEastMoney: "东方财富",
    sourceMSCI: "MSCI",
    sourceComputed: "Quant Lab",
    sourceOverride: "紧急覆写",
    factorSize: "Size",
    factorValue: "Value",
    factorMomentum: "Momentum",
    factorQuality: "Quality",
    factorLowVol: "Low Vol",
    factorMarketBeta: "Market β",
  },
  "en-US": {
    panelTitle: "Factor loading store",
    panelSubtitle:
      "Manage the instrument_factor_loadings table. The Quant Lab batch (source=computed) populates this in bulk; manual entries here are for typo fixes, emergency overrides, and reconciliation against third-party vendor data. The backend rejects loadings outside [-10, 10] with a 400.",
    refresh: "Refresh",
    loading: "Loading…",
    empty: "No calibrations on file",
    error: "Failed to load",
    filterFactor: "Factor",
    filterAll: "All factors",
    filterInstrument: "Instrument key",
    filterInstrumentPlaceholder: "e.g. US:AAPL",
    adminListTitle: "Current calibrations",
    adminInstrumentKey: "Instrument key",
    adminFactorLabel: "Factor",
    adminAsOfLabel: "Asof (YYYY-MM-DD)",
    adminLoadingLabel: "Loading",
    adminSourceLabel: "Source",
    adminNoteLabel: "Note",
    adminUpdatedAtLabel: "Updated",
    adminUpsertTitle: "Add / update loading",
    adminUpsertSubmit: "Save",
    adminUpsertSubmitting: "Saving…",
    adminDeleteButton: "Delete",
    adminDeleteConfirm: "Delete this calibration row?",
    sourceManual: "Manual",
    sourceEastMoney: "EastMoney",
    sourceMSCI: "MSCI",
    sourceComputed: "Quant Lab",
    sourceOverride: "Override",
    factorSize: "Size",
    factorValue: "Value",
    factorMomentum: "Momentum",
    factorQuality: "Quality",
    factorLowVol: "Low Vol",
    factorMarketBeta: "Market β",
  },
};

const FACTOR_SOURCES: LoadingSource[] = [
  "manual",
  "eastmoney",
  "msci",
  "computed",
  "override",
];

interface AdminFactorExposureSectionProps {
  language?: Language;
}

const REFRESH_MS = 60_000;

function factorLabel(m: Messages, f: Factor): string {
  switch (f) {
    case "size":
      return m.factorSize;
    case "value":
      return m.factorValue;
    case "momentum":
      return m.factorMomentum;
    case "quality":
      return m.factorQuality;
    case "lowvol":
      return m.factorLowVol;
    case "market_beta":
      return m.factorMarketBeta;
    default:
      return f;
  }
}

function sourceLabel(m: Messages, s: LoadingSource): string {
  switch (s) {
    case "manual":
      return m.sourceManual;
    case "eastmoney":
      return m.sourceEastMoney;
    case "msci":
      return m.sourceMSCI;
    case "computed":
      return m.sourceComputed;
    case "override":
      return m.sourceOverride;
    default:
      return s;
  }
}

export default function AdminFactorExposureSection({
  language = "zh-CN",
}: AdminFactorExposureSectionProps) {
  const m = useMemo(() => messages[language], [language]);
  const [rows, setRows] = useState<InstrumentFactorLoading[]>([]);
  const [filterFactor, setFilterFactor] = useState<Factor | "">("");
  const [filterInstrument, setFilterInstrument] = useState("");
  const [loadingState, setLoadingState] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [form, setForm] = useState<UpsertFactorLoadingInput>({
    instrument_key: "",
    factor: "momentum",
    asof: new Date().toISOString().slice(0, 10),
    loading: 0,
    source: "manual",
    note: "",
  });
  const [submitting, setSubmitting] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);

  const fetchAll = useCallback(async () => {
    setLoadingState(true);
    setError(null);
    try {
      const resp = await listAdminFactorLoadings({
        factor: filterFactor || undefined,
        instrumentKey: filterInstrument.trim() || undefined,
        limit: 500,
      });
      setRows(resp.loadings ?? []);
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setLoadingState(false);
    }
  }, [filterFactor, filterInstrument, m.error]);

  useEffect(() => {
    fetchAll().catch(() => {});
    const id = window.setInterval(() => {
      fetchAll().catch(() => {});
    }, REFRESH_MS);
    return () => window.clearInterval(id);
  }, [fetchAll]);

  const handleUpsert = async (event: React.FormEvent) => {
    event.preventDefault();
    setSubmitting(true);
    setFormError(null);
    try {
      await upsertAdminFactorLoading(form);
      await fetchAll();
    } catch (err) {
      setFormError(formatApiError(err, m.error));
    } finally {
      setSubmitting(false);
    }
  };

  const handleDelete = async (row: InstrumentFactorLoading) => {
    if (!window.confirm(m.adminDeleteConfirm)) return;
    try {
      await deleteAdminFactorLoading({
        instrumentKey: row.instrument_key,
        factor: row.factor,
        asof: row.asof,
      });
      await fetchAll();
    } catch (err) {
      setError(formatApiError(err, m.error));
    }
  };

  return (
    <section className="space-y-4 rounded-2xl border border-gray-100 bg-white p-6 shadow-sm">
      <header className="flex items-start justify-between gap-4">
        <div>
          <h2 className="text-base font-semibold text-gray-900">{m.panelTitle}</h2>
          <p className="mt-1 text-sm text-gray-500">{m.panelSubtitle}</p>
        </div>
        <button
          type="button"
          onClick={() => fetchAll().catch(() => {})}
          className="rounded-md border border-gray-200 px-3 py-1.5 text-xs font-medium text-gray-600 hover:bg-gray-50"
        >
          {m.refresh}
        </button>
      </header>

      <div className="flex flex-wrap gap-3">
        <label className="flex items-center gap-2 text-xs text-gray-600">
          {m.filterFactor}:
          <select
            value={filterFactor}
            onChange={(e) => setFilterFactor(e.target.value as Factor | "")}
            className="rounded-md border border-gray-200 px-2 py-1 text-xs"
          >
            <option value="">{m.filterAll}</option>
            {ALL_FACTORS.map((f) => (
              <option key={f} value={f}>
                {factorLabel(m, f)}
              </option>
            ))}
          </select>
        </label>
        <label className="flex items-center gap-2 text-xs text-gray-600">
          {m.filterInstrument}:
          <input
            type="text"
            value={filterInstrument}
            onChange={(e) => setFilterInstrument(e.target.value)}
            placeholder={m.filterInstrumentPlaceholder}
            className="rounded-md border border-gray-200 px-2 py-1 text-xs"
          />
        </label>
      </div>

      {error && (
        <div className="rounded-md border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-700">
          {error}
        </div>
      )}

      <div className="overflow-x-auto">
        <table className="min-w-full text-sm">
          <thead className="bg-gray-50 text-xs text-gray-600">
            <tr>
              <th className="px-3 py-2 text-left">{m.adminInstrumentKey}</th>
              <th className="px-3 py-2 text-left">{m.adminFactorLabel}</th>
              <th className="px-3 py-2 text-left">{m.adminAsOfLabel}</th>
              <th className="px-3 py-2 text-right">{m.adminLoadingLabel}</th>
              <th className="px-3 py-2 text-left">{m.adminSourceLabel}</th>
              <th className="px-3 py-2 text-left">{m.adminNoteLabel}</th>
              <th className="px-3 py-2 text-left">{m.adminUpdatedAtLabel}</th>
              <th className="px-3 py-2"></th>
            </tr>
          </thead>
          <tbody className="divide-y divide-gray-100">
            {loadingState && rows.length === 0 && (
              <tr>
                <td colSpan={8} className="px-3 py-6 text-center text-xs text-gray-500">
                  {m.loading}
                </td>
              </tr>
            )}
            {!loadingState && rows.length === 0 && (
              <tr>
                <td colSpan={8} className="px-3 py-6 text-center text-xs text-gray-500">
                  {m.empty}
                </td>
              </tr>
            )}
            {rows.map((row) => (
              <tr key={`${row.instrument_key}-${row.factor}-${row.asof}`} className="text-xs text-gray-700">
                <td className="px-3 py-2 font-mono">{row.instrument_key}</td>
                <td className="px-3 py-2">{factorLabel(m, row.factor)}</td>
                <td className="px-3 py-2">{row.asof}</td>
                <td className="px-3 py-2 text-right font-mono">{row.loading.toFixed(4)}</td>
                <td className="px-3 py-2">{sourceLabel(m, row.source)}</td>
                <td className="px-3 py-2">{row.note ?? ""}</td>
                <td className="px-3 py-2 text-gray-500">{row.updated_at.slice(0, 19).replace("T", " ")}</td>
                <td className="px-3 py-2 text-right">
                  <button
                    type="button"
                    onClick={() => handleDelete(row)}
                    className="text-xs text-red-600 hover:underline"
                  >
                    {m.adminDeleteButton}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <form onSubmit={handleUpsert} className="space-y-3 rounded-md border border-gray-200 bg-gray-50 p-4">
        <h3 className="text-sm font-semibold text-gray-800">{m.adminUpsertTitle}</h3>
        <div className="grid grid-cols-1 gap-3 md:grid-cols-3">
          <label className="text-xs text-gray-600">
            {m.adminInstrumentKey}
            <input
              type="text"
              value={form.instrument_key}
              onChange={(e) => setForm({ ...form, instrument_key: e.target.value })}
              required
              className="mt-1 w-full rounded-md border border-gray-200 px-2 py-1 text-sm font-mono"
            />
          </label>
          <label className="text-xs text-gray-600">
            {m.adminFactorLabel}
            <select
              value={form.factor}
              onChange={(e) => setForm({ ...form, factor: e.target.value as Factor })}
              className="mt-1 w-full rounded-md border border-gray-200 px-2 py-1 text-sm"
            >
              {ALL_FACTORS.map((f) => (
                <option key={f} value={f}>
                  {factorLabel(m, f)}
                </option>
              ))}
            </select>
          </label>
          <label className="text-xs text-gray-600">
            {m.adminAsOfLabel}
            <input
              type="date"
              value={form.asof}
              onChange={(e) => setForm({ ...form, asof: e.target.value })}
              required
              className="mt-1 w-full rounded-md border border-gray-200 px-2 py-1 text-sm"
            />
          </label>
          <label className="text-xs text-gray-600">
            {m.adminLoadingLabel}
            <input
              type="number"
              step="0.0001"
              min="-10"
              max="10"
              value={form.loading}
              onChange={(e) => setForm({ ...form, loading: Number(e.target.value) })}
              required
              className="mt-1 w-full rounded-md border border-gray-200 px-2 py-1 text-sm font-mono"
            />
          </label>
          <label className="text-xs text-gray-600">
            {m.adminSourceLabel}
            <select
              value={form.source ?? "manual"}
              onChange={(e) => setForm({ ...form, source: e.target.value as LoadingSource })}
              className="mt-1 w-full rounded-md border border-gray-200 px-2 py-1 text-sm"
            >
              {FACTOR_SOURCES.map((s) => (
                <option key={s} value={s}>
                  {sourceLabel(m, s)}
                </option>
              ))}
            </select>
          </label>
          <label className="text-xs text-gray-600 md:col-span-3">
            {m.adminNoteLabel}
            <input
              type="text"
              value={form.note ?? ""}
              onChange={(e) => setForm({ ...form, note: e.target.value })}
              className="mt-1 w-full rounded-md border border-gray-200 px-2 py-1 text-sm"
            />
          </label>
        </div>
        {formError && (
          <div className="rounded-md border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-700">
            {formError}
          </div>
        )}
        <button
          type="submit"
          disabled={submitting}
          className="rounded-md bg-blue-600 px-4 py-1.5 text-xs font-medium text-white hover:bg-blue-700 disabled:opacity-50"
        >
          {submitting ? m.adminUpsertSubmitting : m.adminUpsertSubmit}
        </button>
      </form>
    </section>
  );
}
