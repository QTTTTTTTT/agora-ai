// AdminMarketImpactSection — admin web component for the S6.2
// size-aware slippage calibration.
//
// What it shows
//
//   - Calibration table: per-instrument ADV / volatility /
//     coefficients / bps bounds. Filterable by market and asset
//     class. Inline upsert dialog and delete confirmation.
//   - Preview panel: operator types in side / quantity /
//     reference price for an instrument and gets back the
//     adverse-bps the engine would apply, the implied fill
//     price, and the notional impact cost.
//   - Cache panel: in-memory cache size and last-refresh
//     timestamp, with a force-refresh button so admins can
//     verify their write made it into the simulator.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  apiDelete,
  formatApiError,
  getAdminMarketImpactCacheStats,
  listAdminMarketImpactInstruments,
  previewAdminMarketImpact,
  refreshAdminMarketImpactCache,
  upsertAdminMarketImpactInstrument,
  type MarketImpactPreviewInput,
  type UpsertMarketImpactInstrumentInput,
} from "../lib/api";
import type {
  MarketImpactCacheStats,
  MarketImpactCalibrationSource,
  MarketImpactInstrument,
  MarketImpactPreviewResponse,
} from "@fundai/api-client";

type Language = "zh-CN" | "en-US";

interface Messages {
  panelTitle: string;
  panelSubtitle: string;
  refresh: string;
  instrumentsTitle: string;
  instrumentsEmpty: string;
  fieldKey: string;
  fieldSymbol: string;
  fieldMarket: string;
  fieldAssetClass: string;
  fieldADV: string;
  fieldADVNotional: string;
  fieldVolatility: string;
  fieldImpactCoef: string;
  fieldImpactExp: string;
  fieldMinBps: string;
  fieldMaxBps: string;
  fieldLastCalibrated: string;
  fieldSource: string;
  upsertButton: string;
  upsertDialogTitle: string;
  deleteButton: string;
  deleteConfirm: string;
  saveButton: string;
  saveSubmitting: string;
  cancelButton: string;
  sourceManual: string;
  sourceHistorical: string;
  sourceBrokerReported: string;
  previewTitle: string;
  previewSubtitle: string;
  previewSide: string;
  previewSideBuy: string;
  previewSideSell: string;
  previewQuantity: string;
  previewReferencePrice: string;
  previewSubmit: string;
  previewSubmitting: string;
  previewResult: string;
  previewBps: string;
  previewImpliedFill: string;
  previewImpactCost: string;
  previewUsedDefaults: string;
  previewUsedADVFallback: string;
  cacheTitle: string;
  cacheSize: string;
  cacheLastRefresh: string;
  cacheRefreshButton: string;
  cacheRefreshing: string;
  error: string;
}

const messages: Record<Language, Messages> = {
  "zh-CN": {
    panelTitle: "S6.2 · 大单冲击模型",
    panelSubtitle:
      "为模拟器配置每个标的的 ADV / 波动率，撮合引擎将以平方根冲击模型（bps = σ · 系数 · √(Q/ADV) · 10000）估算大单滑点，避免回测中大单仍以 last 成交、放大 P&L 的问题。未校准的标的回退到资产类别默认值。",
    refresh: "刷新",
    instrumentsTitle: "校准列表",
    instrumentsEmpty: '尚无标的校准。可点击下方"新增校准"，或保留为空走资产类别默认。',
    fieldKey: "instrument_key",
    fieldSymbol: "代码",
    fieldMarket: "市场",
    fieldAssetClass: "资产类别",
    fieldADV: "ADV（股 / 张）",
    fieldADVNotional: "ADV（名义金额）",
    fieldVolatility: "日波动率 σ",
    fieldImpactCoef: "冲击系数 k",
    fieldImpactExp: "指数 α（默认 0.5）",
    fieldMinBps: "最小滑点 bps",
    fieldMaxBps: "最大滑点 bps",
    fieldLastCalibrated: "最近校准时间",
    fieldSource: "校准来源",
    upsertButton: "新增 / 编辑",
    upsertDialogTitle: "编辑校准",
    deleteButton: "删除",
    deleteConfirm: "确认删除此标的的校准？删除后将回退到资产类别默认值。",
    saveButton: "保存",
    saveSubmitting: "保存中…",
    cancelButton: "取消",
    sourceManual: "手工录入",
    sourceHistorical: "历史回算",
    sourceBrokerReported: "券商上报",
    previewTitle: "撮合冲击预演",
    previewSubtitle: "不下单，仅基于当前校准估算一笔订单的滑点 bps 与隐含成交价。",
    previewSide: "方向",
    previewSideBuy: "买入",
    previewSideSell: "卖出",
    previewQuantity: "数量",
    previewReferencePrice: "参考价格",
    previewSubmit: "运行预演",
    previewSubmitting: "运行中…",
    previewResult: "预演结果",
    previewBps: "不利滑点",
    previewImpliedFill: "隐含成交价",
    previewImpactCost: "冲击成本（参考币）",
    previewUsedDefaults: "使用资产类别默认",
    previewUsedADVFallback: "ADV 缺失，回退到 min_bps",
    cacheTitle: "内存缓存",
    cacheSize: "校准条目",
    cacheLastRefresh: "最近刷新",
    cacheRefreshButton: "强制刷新",
    cacheRefreshing: "刷新中…",
    error: "加载失败",
  },
  "en-US": {
    panelTitle: "S6.2 · Market-impact calibration",
    panelSubtitle:
      "Per-instrument ADV and volatility used by the simulator's square-root impact model (bps = σ · k · √(Q/ADV) · 10000). Uncalibrated names fall back to asset-class defaults so big orders never silently fill at last.",
    refresh: "Refresh",
    instrumentsTitle: "Calibration rows",
    instrumentsEmpty:
      "No calibrations yet. Add one below or leave empty to use asset-class defaults.",
    fieldKey: "instrument_key",
    fieldSymbol: "Symbol",
    fieldMarket: "Market",
    fieldAssetClass: "Asset class",
    fieldADV: "ADV (shares / contracts)",
    fieldADVNotional: "ADV (notional)",
    fieldVolatility: "Daily volatility σ",
    fieldImpactCoef: "Impact coef k",
    fieldImpactExp: "Exponent α (default 0.5)",
    fieldMinBps: "Min slippage bps",
    fieldMaxBps: "Max slippage bps",
    fieldLastCalibrated: "Last calibrated",
    fieldSource: "Source",
    upsertButton: "Upsert",
    upsertDialogTitle: "Edit calibration",
    deleteButton: "Delete",
    deleteConfirm:
      "Delete this calibration? Future fills will fall back to asset-class defaults.",
    saveButton: "Save",
    saveSubmitting: "Saving…",
    cancelButton: "Cancel",
    sourceManual: "Manual entry",
    sourceHistorical: "Historical replay",
    sourceBrokerReported: "Broker-reported",
    previewTitle: "Preview impact",
    previewSubtitle:
      "Run the engine on a probe; nothing is booked. Useful for sanity-checking calibration.",
    previewSide: "Side",
    previewSideBuy: "Buy",
    previewSideSell: "Sell",
    previewQuantity: "Quantity",
    previewReferencePrice: "Reference price",
    previewSubmit: "Run preview",
    previewSubmitting: "Running…",
    previewResult: "Preview result",
    previewBps: "Adverse bps",
    previewImpliedFill: "Implied fill",
    previewImpactCost: "Impact cost (notional)",
    previewUsedDefaults: "Asset-class default",
    previewUsedADVFallback: "ADV missing → floor only",
    cacheTitle: "In-memory cache",
    cacheSize: "Calibration rows",
    cacheLastRefresh: "Last refresh",
    cacheRefreshButton: "Force refresh",
    cacheRefreshing: "Refreshing…",
    error: "Failed to load",
  },
};

interface Props {
  language?: Language;
}

interface UpsertFormState {
  open: boolean;
  isEdit: boolean;
  instrument_key: string;
  symbol: string;
  market: string;
  asset_class: string;
  adv_shares: string;
  adv_notional: string;
  daily_volatility: string;
  impact_coefficient: string;
  impact_exponent: string;
  min_slippage_bps: string;
  max_slippage_bps: string;
  calibration_source: MarketImpactCalibrationSource;
  note: string;
}

const emptyForm: UpsertFormState = {
  open: false,
  isEdit: false,
  instrument_key: "",
  symbol: "",
  market: "US",
  asset_class: "equity",
  adv_shares: "",
  adv_notional: "",
  daily_volatility: "",
  impact_coefficient: "",
  impact_exponent: "",
  min_slippage_bps: "",
  max_slippage_bps: "",
  calibration_source: "manual",
  note: "",
};

interface PreviewFormState {
  instrument_key: string;
  asset_class: string;
  side: "buy" | "sell";
  quantity: string;
  reference_price: string;
}

const emptyPreview: PreviewFormState = {
  instrument_key: "",
  asset_class: "equity",
  side: "buy",
  quantity: "10000",
  reference_price: "100",
};

export function AdminMarketImpactSection({ language = "zh-CN" }: Props) {
  const m = useMemo<Messages>(
    () => messages[language] ?? messages["zh-CN"],
    [language],
  );

  const [instruments, setInstruments] = useState<MarketImpactInstrument[]>([]);
  const [filterMarket, setFilterMarket] = useState("");
  const [filterAsset, setFilterAsset] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const [form, setForm] = useState<UpsertFormState>(emptyForm);
  const [saving, setSaving] = useState(false);

  const [preview, setPreview] = useState<PreviewFormState>(emptyPreview);
  const [previewResult, setPreviewResult] =
    useState<MarketImpactPreviewResponse | null>(null);
  const [previewSubmitting, setPreviewSubmitting] = useState(false);

  const [cacheStats, setCacheStats] = useState<MarketImpactCacheStats | null>(
    null,
  );
  const [cacheRefreshing, setCacheRefreshing] = useState(false);

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const [list, stats] = await Promise.all([
        listAdminMarketImpactInstruments({
          market: filterMarket || undefined,
          assetClass: filterAsset || undefined,
        }),
        getAdminMarketImpactCacheStats().catch(() => null),
      ]);
      setInstruments(list.instruments);
      setCacheStats(stats);
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setLoading(false);
    }
  }, [filterMarket, filterAsset, m.error]);

  useEffect(() => {
    void reload();
  }, [reload]);

  const openNew = () => {
    setForm({ ...emptyForm, open: true });
  };

  const openEdit = (row: MarketImpactInstrument) => {
    setForm({
      open: true,
      isEdit: true,
      instrument_key: row.instrument_key,
      symbol: row.symbol,
      market: row.market,
      asset_class: row.asset_class,
      adv_shares: row.adv_shares != null ? String(row.adv_shares) : "",
      adv_notional: row.adv_notional != null ? String(row.adv_notional) : "",
      daily_volatility:
        row.daily_volatility != null ? String(row.daily_volatility) : "",
      impact_coefficient: String(row.impact_coefficient ?? ""),
      impact_exponent: String(row.impact_exponent ?? ""),
      min_slippage_bps: String(row.min_slippage_bps ?? ""),
      max_slippage_bps: String(row.max_slippage_bps ?? ""),
      calibration_source: row.calibration_source,
      note: row.note ?? "",
    });
  };

  const closeForm = () => setForm({ ...emptyForm, open: false });

  const handleSave = async () => {
    if (!form.instrument_key.trim() || !form.symbol.trim() || !form.market.trim()) {
      setError("instrument_key / symbol / market 必填");
      return;
    }
    setSaving(true);
    setError(null);
    try {
      const input: UpsertMarketImpactInstrumentInput = {
        symbol: form.symbol,
        market: form.market,
        asset_class: form.asset_class || "equity",
        adv_shares: parseOptionalNumber(form.adv_shares),
        adv_notional: parseOptionalNumber(form.adv_notional),
        daily_volatility: parseOptionalNumber(form.daily_volatility),
        impact_coefficient: parseOptionalNumber(form.impact_coefficient),
        impact_exponent: parseOptionalNumber(form.impact_exponent),
        min_slippage_bps: parseOptionalNumber(form.min_slippage_bps),
        max_slippage_bps: parseOptionalNumber(form.max_slippage_bps),
        calibration_source: form.calibration_source,
        note: form.note,
      };
      await upsertAdminMarketImpactInstrument(form.instrument_key, input);
      closeForm();
      await reload();
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setSaving(false);
    }
  };

  const handleDelete = async (row: MarketImpactInstrument) => {
    if (!window.confirm(m.deleteConfirm)) return;
    setError(null);
    try {
      await apiDelete(`/api/admin/marketimpact/instruments/${encodeURIComponent(row.instrument_key)}`);
      await reload();
    } catch (err) {
      setError(formatApiError(err, m.error));
    }
  };

  const handleRunPreview = async () => {
    if (!preview.instrument_key.trim()) return;
    setPreviewSubmitting(true);
    setError(null);
    try {
      const input: MarketImpactPreviewInput = {
        instrument_key: preview.instrument_key.trim(),
        asset_class: preview.asset_class || "equity",
        side: preview.side,
        quantity: Number(preview.quantity) || 0,
        reference_price: Number(preview.reference_price) || 0,
      };
      const res = await previewAdminMarketImpact(input);
      setPreviewResult(res);
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setPreviewSubmitting(false);
    }
  };

  const handleRefreshCache = async () => {
    setCacheRefreshing(true);
    setError(null);
    try {
      const stats = await refreshAdminMarketImpactCache();
      setCacheStats({
        size: stats.size,
        last_refresh: stats.last_refresh,
      });
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setCacheRefreshing(false);
    }
  };

  return (
    <section className="admin-section admin-marketimpact">
      <header className="admin-section__header">
        <div>
          <h2>{m.panelTitle}</h2>
          <p className="admin-section__subtitle">{m.panelSubtitle}</p>
        </div>
        <div className="admin-section__actions">
          <button type="button" onClick={() => void reload()} disabled={loading}>
            {m.refresh}
          </button>
          <button type="button" onClick={openNew}>
            {m.upsertButton}
          </button>
        </div>
      </header>

      {error && <div className="admin-section__error">{error}</div>}

      <div className="admin-section__filters">
        <input
          placeholder={m.fieldMarket}
          value={filterMarket}
          onChange={(e) => setFilterMarket(e.target.value)}
        />
        <input
          placeholder={m.fieldAssetClass}
          value={filterAsset}
          onChange={(e) => setFilterAsset(e.target.value)}
        />
      </div>

      <h3>{m.instrumentsTitle}</h3>
      {instruments.length === 0 ? (
        <p className="admin-section__empty">{m.instrumentsEmpty}</p>
      ) : (
        <div className="admin-table-wrap">
          <table className="admin-table">
            <thead>
              <tr>
                <th>{m.fieldSymbol}</th>
                <th>{m.fieldMarket}</th>
                <th>{m.fieldAssetClass}</th>
                <th>{m.fieldADV}</th>
                <th>{m.fieldVolatility}</th>
                <th>{m.fieldImpactCoef}</th>
                <th>{m.fieldImpactExp}</th>
                <th>
                  {m.fieldMinBps} / {m.fieldMaxBps}
                </th>
                <th>{m.fieldLastCalibrated}</th>
                <th>{m.fieldSource}</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {instruments.map((row) => (
                <tr key={row.instrument_key}>
                  <td>{row.symbol}</td>
                  <td>{row.market}</td>
                  <td>{row.asset_class}</td>
                  <td>{formatNumber(row.adv_shares)}</td>
                  <td>{formatNumber(row.daily_volatility)}</td>
                  <td>{formatNumber(row.impact_coefficient)}</td>
                  <td>{formatNumber(row.impact_exponent)}</td>
                  <td>
                    {formatNumber(row.min_slippage_bps)} /{" "}
                    {formatNumber(row.max_slippage_bps)}
                  </td>
                  <td>{formatTimestamp(row.last_calibrated_at)}</td>
                  <td>{row.calibration_source}</td>
                  <td>
                    <button type="button" onClick={() => openEdit(row)}>
                      {m.upsertButton}
                    </button>{" "}
                    <button type="button" onClick={() => void handleDelete(row)}>
                      {m.deleteButton}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {form.open && (
        <div className="admin-modal">
          <div className="admin-modal__inner">
            <h3>{m.upsertDialogTitle}</h3>
            <div className="admin-grid">
              <label>
                <span>{m.fieldKey}</span>
                <input
                  value={form.instrument_key}
                  disabled={form.isEdit}
                  onChange={(e) => setForm({ ...form, instrument_key: e.target.value })}
                />
              </label>
              <label>
                <span>{m.fieldSymbol}</span>
                <input
                  value={form.symbol}
                  onChange={(e) => setForm({ ...form, symbol: e.target.value })}
                />
              </label>
              <label>
                <span>{m.fieldMarket}</span>
                <input
                  value={form.market}
                  onChange={(e) => setForm({ ...form, market: e.target.value })}
                />
              </label>
              <label>
                <span>{m.fieldAssetClass}</span>
                <input
                  value={form.asset_class}
                  onChange={(e) =>
                    setForm({ ...form, asset_class: e.target.value })
                  }
                />
              </label>
              <label>
                <span>{m.fieldADV}</span>
                <input
                  value={form.adv_shares}
                  onChange={(e) => setForm({ ...form, adv_shares: e.target.value })}
                />
              </label>
              <label>
                <span>{m.fieldADVNotional}</span>
                <input
                  value={form.adv_notional}
                  onChange={(e) =>
                    setForm({ ...form, adv_notional: e.target.value })
                  }
                />
              </label>
              <label>
                <span>{m.fieldVolatility}</span>
                <input
                  value={form.daily_volatility}
                  onChange={(e) =>
                    setForm({ ...form, daily_volatility: e.target.value })
                  }
                />
              </label>
              <label>
                <span>{m.fieldImpactCoef}</span>
                <input
                  value={form.impact_coefficient}
                  onChange={(e) =>
                    setForm({ ...form, impact_coefficient: e.target.value })
                  }
                />
              </label>
              <label>
                <span>{m.fieldImpactExp}</span>
                <input
                  value={form.impact_exponent}
                  onChange={(e) =>
                    setForm({ ...form, impact_exponent: e.target.value })
                  }
                />
              </label>
              <label>
                <span>{m.fieldMinBps}</span>
                <input
                  value={form.min_slippage_bps}
                  onChange={(e) =>
                    setForm({ ...form, min_slippage_bps: e.target.value })
                  }
                />
              </label>
              <label>
                <span>{m.fieldMaxBps}</span>
                <input
                  value={form.max_slippage_bps}
                  onChange={(e) =>
                    setForm({ ...form, max_slippage_bps: e.target.value })
                  }
                />
              </label>
              <label>
                <span>{m.fieldSource}</span>
                <select
                  value={form.calibration_source}
                  onChange={(e) =>
                    setForm({
                      ...form,
                      calibration_source: e.target
                        .value as MarketImpactCalibrationSource,
                    })
                  }
                >
                  <option value="manual">{m.sourceManual}</option>
                  <option value="historical">{m.sourceHistorical}</option>
                  <option value="broker_reported">
                    {m.sourceBrokerReported}
                  </option>
                </select>
              </label>
            </div>
            <div className="admin-modal__actions">
              <button type="button" onClick={closeForm} disabled={saving}>
                {m.cancelButton}
              </button>
              <button
                type="button"
                onClick={() => void handleSave()}
                disabled={saving}
              >
                {saving ? m.saveSubmitting : m.saveButton}
              </button>
            </div>
          </div>
        </div>
      )}

      <h3>{m.previewTitle}</h3>
      <p className="admin-section__subtitle">{m.previewSubtitle}</p>
      <div className="admin-grid">
        <label>
          <span>{m.fieldKey}</span>
          <input
            value={preview.instrument_key}
            onChange={(e) =>
              setPreview({ ...preview, instrument_key: e.target.value })
            }
          />
        </label>
        <label>
          <span>{m.fieldAssetClass}</span>
          <input
            value={preview.asset_class}
            onChange={(e) =>
              setPreview({ ...preview, asset_class: e.target.value })
            }
          />
        </label>
        <label>
          <span>{m.previewSide}</span>
          <select
            value={preview.side}
            onChange={(e) =>
              setPreview({
                ...preview,
                side: e.target.value as "buy" | "sell",
              })
            }
          >
            <option value="buy">{m.previewSideBuy}</option>
            <option value="sell">{m.previewSideSell}</option>
          </select>
        </label>
        <label>
          <span>{m.previewQuantity}</span>
          <input
            value={preview.quantity}
            onChange={(e) => setPreview({ ...preview, quantity: e.target.value })}
          />
        </label>
        <label>
          <span>{m.previewReferencePrice}</span>
          <input
            value={preview.reference_price}
            onChange={(e) =>
              setPreview({ ...preview, reference_price: e.target.value })
            }
          />
        </label>
      </div>
      <div className="admin-section__actions">
        <button
          type="button"
          onClick={() => void handleRunPreview()}
          disabled={previewSubmitting}
        >
          {previewSubmitting ? m.previewSubmitting : m.previewSubmit}
        </button>
      </div>
      {previewResult && (
        <div className="admin-preview-result">
          <h4>{m.previewResult}</h4>
          <ul>
            <li>
              <strong>{m.previewBps}:</strong>{" "}
              {previewResult.estimate.adverse_bps.toFixed(2)} bps
            </li>
            <li>
              <strong>{m.previewImpliedFill}:</strong>{" "}
              {previewResult.implied_fill.toFixed(4)}
            </li>
            <li>
              <strong>{m.previewImpactCost}:</strong>{" "}
              {previewResult.impact_cost.toFixed(2)} (
              {previewResult.impact_cost_pct.toFixed(2)}%)
            </li>
            {previewResult.estimate.used_defaults && (
              <li>{m.previewUsedDefaults}</li>
            )}
            {previewResult.estimate.used_adv_fallback && (
              <li>{m.previewUsedADVFallback}</li>
            )}
          </ul>
        </div>
      )}

      <h3>{m.cacheTitle}</h3>
      <ul>
        <li>
          <strong>{m.cacheSize}:</strong> {cacheStats?.size ?? "—"}
        </li>
        <li>
          <strong>{m.cacheLastRefresh}:</strong>{" "}
          {formatTimestamp(cacheStats?.last_refresh)}
        </li>
      </ul>
      <div className="admin-section__actions">
        <button
          type="button"
          onClick={() => void handleRefreshCache()}
          disabled={cacheRefreshing}
        >
          {cacheRefreshing ? m.cacheRefreshing : m.cacheRefreshButton}
        </button>
      </div>
    </section>
  );
}

function parseOptionalNumber(s: string): number | null | undefined {
  if (s === undefined || s === null) return undefined;
  const trimmed = s.trim();
  if (trimmed === "") return undefined;
  const n = Number(trimmed);
  if (!Number.isFinite(n)) return undefined;
  return n;
}

function formatNumber(n: number | null | undefined): string {
  if (n === null || n === undefined) return "—";
  if (Math.abs(n) >= 1e6) return n.toExponential(2);
  return String(n);
}

function formatTimestamp(s: string | null | undefined): string {
  if (!s) return "—";
  const d = new Date(s);
  if (Number.isNaN(d.getTime())) return s;
  return d.toLocaleString();
}

export default AdminMarketImpactSection;
