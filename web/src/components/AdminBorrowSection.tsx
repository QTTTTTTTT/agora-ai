// AdminBorrowSection — admin UI for the S6.4 securities-borrow
// stack.
//
// Capability surface
//
//   - List + filter borrow-rate calibrations.
//   - Upsert (form per row) and delete with one click; both
//     hit the live cache via the backend's ApplyChange.
//   - Locate-preview panel: dry-run the gate without placing
//     an order. Shows the would-be decision + locate fee.
//   - Locate audit panel: scroll the recent decisions.
//   - Borrow-fee ledger panel: per-day per-fund accruals.
//   - Cache panel: size + last-refresh + force refresh button.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  deleteAdminBorrowRate,
  formatApiError,
  getAdminBorrowCacheStats,
  listAdminBorrowLedger,
  listAdminBorrowLocateEvents,
  listAdminBorrowRates,
  previewAdminBorrowLocate,
  refreshAdminBorrowCache,
  upsertAdminBorrowRate,
  type UpsertBorrowRateInput,
} from "../lib/api";
import type {
  BorrowAvailability,
  BorrowCacheStats,
  BorrowCalibrationSource,
  BorrowLedgerEntry,
  BorrowLocateDecisionKind,
  BorrowLocateEvent,
  BorrowLocatePreviewResponse,
  BorrowRate,
} from "@fundai/api-client";

type Language = "zh-CN" | "en-US";

interface Messages {
  panelTitle: string;
  panelSubtitle: string;
  refresh: string;
  listTitle: string;
  listEmpty: string;
  fieldKey: string;
  fieldSymbol: string;
  fieldMarket: string;
  fieldRate: string;
  fieldLocateFee: string;
  fieldAvailability: string;
  fieldAvailable: string;
  fieldMinLocate: string;
  fieldMaxLocate: string;
  fieldSource: string;
  fieldNote: string;
  availEasy: string;
  availHard: string;
  availRestricted: string;
  availUnavailable: string;
  sourceManual: string;
  sourceBrokerQuote: string;
  sourceAgentLender: string;
  sourceHistorical: string;
  sourcePublicFeed: string;
  upsertButton: string;
  upsertSubmitting: string;
  deleteButton: string;
  cacheTitle: string;
  cacheSize: string;
  cacheLastRefresh: string;
  cacheRefreshButton: string;
  cacheRefreshing: string;
  previewTitle: string;
  previewSubtitle: string;
  previewFundLabel: string;
  previewKeyLabel: string;
  previewQtyLabel: string;
  previewPriceLabel: string;
  previewSubmit: string;
  previewSubmitting: string;
  previewResultDecision: string;
  previewResultRate: string;
  previewResultLocateFee: string;
  previewResultNotional: string;
  auditTitle: string;
  auditFundFilter: string;
  auditDecisionFilter: string;
  auditEmpty: string;
  ledgerTitle: string;
  ledgerEmpty: string;
  error: string;
}

const messages: Record<Language, Messages> = {
  "zh-CN": {
    panelTitle: "S6.4 · 借券与 locate 费",
    panelSubtitle:
      "为可融券品种登记借券费率、locate 费、可用数量。模拟器在 SHORT 开仓时按需走 locate gate；EOD 自动按持仓 × 当日收盘价 × 年化费率 / 365 计提借券费，写入 cash_ledger（borrow_fee）+ 短仓借券台账。",
    refresh: "刷新",
    listTitle: "借券费率",
    listEmpty: "暂未登记任何借券费率",
    fieldKey: "instrument_key",
    fieldSymbol: "代码",
    fieldMarket: "市场",
    fieldRate: "年化费率 (bps)",
    fieldLocateFee: "Locate 费 (bps)",
    fieldAvailability: "可借状态",
    fieldAvailable: "可借数量",
    fieldMinLocate: "locate 最小",
    fieldMaxLocate: "locate 最大",
    fieldSource: "来源",
    fieldNote: "备注",
    availEasy: "易借",
    availHard: "难借",
    availRestricted: "受限",
    availUnavailable: "不可借",
    sourceManual: "手动登记",
    sourceBrokerQuote: "券商报价",
    sourceAgentLender: "Agent lender",
    sourceHistorical: "历史校准",
    sourcePublicFeed: "公开数据",
    upsertButton: "保存",
    upsertSubmitting: "保存中…",
    deleteButton: "删除",
    cacheTitle: "内存缓存",
    cacheSize: "缓存条目数",
    cacheLastRefresh: "上次刷新",
    cacheRefreshButton: "强制刷新",
    cacheRefreshing: "刷新中…",
    previewTitle: "Locate 预演",
    previewSubtitle: "不下单的情况下试算 locate gate 的判定结果（含 locate 费）。",
    previewFundLabel: "基金 ID",
    previewKeyLabel: "instrument_key",
    previewQtyLabel: "请求数量",
    previewPriceLabel: "预期价格",
    previewSubmit: "预演",
    previewSubmitting: "计算中…",
    previewResultDecision: "判定",
    previewResultRate: "年化借券费率 (bps)",
    previewResultLocateFee: "Locate 费",
    previewResultNotional: "Notional",
    auditTitle: "Locate 审计日志",
    auditFundFilter: "按基金过滤",
    auditDecisionFilter: "按判定过滤",
    auditEmpty: "暂无 locate 审计记录",
    ledgerTitle: "借券费台账",
    ledgerEmpty: "暂无借券费记录",
    error: "加载失败",
  },
  "en-US": {
    panelTitle: "S6.4 · Securities borrow & locate",
    panelSubtitle:
      "Records the annual borrow rate, locate fee and available supply per borrowable instrument. The simulator runs a pre-trade locate gate on SHORT opens; an EOD loop accrues short-borrow fees (qty × close × rate / 365) to the cash_ledger (borrow_fee) and the short-borrow sub-ledger.",
    refresh: "Refresh",
    listTitle: "Borrow rates",
    listEmpty: "No borrow rate calibrations yet",
    fieldKey: "instrument_key",
    fieldSymbol: "Symbol",
    fieldMarket: "Market",
    fieldRate: "Annual rate (bps)",
    fieldLocateFee: "Locate fee (bps)",
    fieldAvailability: "Availability",
    fieldAvailable: "Available shares",
    fieldMinLocate: "Min locate qty",
    fieldMaxLocate: "Max locate qty",
    fieldSource: "Source",
    fieldNote: "Note",
    availEasy: "Easy-to-borrow",
    availHard: "Hard-to-borrow",
    availRestricted: "Restricted",
    availUnavailable: "Unavailable",
    sourceManual: "Manual",
    sourceBrokerQuote: "Broker quote",
    sourceAgentLender: "Agent lender",
    sourceHistorical: "Historical calibration",
    sourcePublicFeed: "Public feed",
    upsertButton: "Save",
    upsertSubmitting: "Saving…",
    deleteButton: "Delete",
    cacheTitle: "In-memory cache",
    cacheSize: "Rows",
    cacheLastRefresh: "Last refresh",
    cacheRefreshButton: "Force refresh",
    cacheRefreshing: "Refreshing…",
    previewTitle: "Locate preview",
    previewSubtitle: "Dry-run the locate gate decision (including locate fee) without placing an order.",
    previewFundLabel: "Fund ID",
    previewKeyLabel: "instrument_key",
    previewQtyLabel: "Requested qty",
    previewPriceLabel: "Intended price",
    previewSubmit: "Preview",
    previewSubmitting: "Computing…",
    previewResultDecision: "Decision",
    previewResultRate: "Annual borrow rate (bps)",
    previewResultLocateFee: "Locate fee",
    previewResultNotional: "Notional",
    auditTitle: "Locate audit log",
    auditFundFilter: "Filter by fund",
    auditDecisionFilter: "Filter by decision",
    auditEmpty: "No locate events recorded",
    ledgerTitle: "Borrow-fee ledger",
    ledgerEmpty: "No borrow-fee accruals yet",
    error: "Failed to load",
  },
};

interface Props {
  language?: Language;
}

interface UpsertForm {
  instrument_key: string;
  symbol: string;
  market: string;
  asset_class: string;
  borrow_rate_bps_annual: string;
  locate_fee_bps: string;
  availability: BorrowAvailability;
  available_shares: string;
  min_locate_qty: string;
  max_locate_qty: string;
  source: BorrowCalibrationSource;
  note: string;
}

const emptyUpsert: UpsertForm = {
  instrument_key: "",
  symbol: "",
  market: "US",
  asset_class: "equity",
  borrow_rate_bps_annual: "",
  locate_fee_bps: "",
  availability: "easy",
  available_shares: "",
  min_locate_qty: "",
  max_locate_qty: "",
  source: "manual",
  note: "",
};

interface PreviewForm {
  fund_id: string;
  instrument_key: string;
  requested_qty: string;
  intended_price: string;
}

const emptyPreview: PreviewForm = {
  fund_id: "",
  instrument_key: "",
  requested_qty: "",
  intended_price: "",
};

export function AdminBorrowSection({ language = "zh-CN" }: Props) {
  const m = useMemo<Messages>(
    () => messages[language] ?? messages["zh-CN"],
    [language],
  );

  const [rows, setRows] = useState<BorrowRate[]>([]);
  const [events, setEvents] = useState<BorrowLocateEvent[]>([]);
  const [ledger, setLedger] = useState<BorrowLedgerEntry[]>([]);
  const [cache, setCache] = useState<BorrowCacheStats | null>(null);
  const [filterAvail, setFilterAvail] = useState<BorrowAvailability | "">("");
  const [filterFund, setFilterFund] = useState("");
  const [filterDecision, setFilterDecision] = useState<BorrowLocateDecisionKind | "">("");

  const [upsertForm, setUpsertForm] = useState<UpsertForm>(emptyUpsert);
  const [submitting, setSubmitting] = useState(false);
  const [previewForm, setPreviewForm] = useState<PreviewForm>(emptyPreview);
  const [previewResult, setPreviewResult] = useState<BorrowLocatePreviewResponse | null>(null);
  const [previewing, setPreviewing] = useState(false);
  const [refreshingCache, setRefreshingCache] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback(async () => {
    setError(null);
    try {
      const [ratesRes, eventsRes, ledgerRes, cacheRes] = await Promise.all([
        listAdminBorrowRates(
          filterAvail ? { availability: filterAvail } : {},
        ),
        listAdminBorrowLocateEvents({
          fundId: filterFund || undefined,
          decision: filterDecision || undefined,
          limit: 100,
        }),
        listAdminBorrowLedger({
          fundId: filterFund || undefined,
          limit: 100,
        }),
        getAdminBorrowCacheStats(),
      ]);
      setRows(ratesRes.rates);
      setEvents(eventsRes.events);
      setLedger(ledgerRes.entries);
      setCache(cacheRes);
    } catch (err) {
      setError(formatApiError(err, m.error));
    }
  }, [filterAvail, filterFund, filterDecision, m.error]);

  useEffect(() => {
    void reload();
  }, [reload]);

  const availabilityLabel = (a: BorrowAvailability): string => {
    switch (a) {
      case "easy":
        return m.availEasy;
      case "hard":
        return m.availHard;
      case "restricted":
        return m.availRestricted;
      case "unavailable":
        return m.availUnavailable;
      default:
        return a;
    }
  };

  const sourceLabel = (s: BorrowCalibrationSource): string => {
    switch (s) {
      case "manual":
        return m.sourceManual;
      case "broker_quote":
        return m.sourceBrokerQuote;
      case "agent_lender":
        return m.sourceAgentLender;
      case "historical_calibration":
        return m.sourceHistorical;
      case "public_feed":
        return m.sourcePublicFeed;
      default:
        return s;
    }
  };

  const handleUpsert = async () => {
    setSubmitting(true);
    setError(null);
    try {
      const input: UpsertBorrowRateInput = {
        instrument_key: upsertForm.instrument_key.trim(),
        symbol: upsertForm.symbol.trim(),
        market: upsertForm.market.trim() || undefined,
        asset_class: upsertForm.asset_class.trim() || undefined,
        availability: upsertForm.availability,
        source: upsertForm.source,
        note: upsertForm.note.trim() || undefined,
      };
      if (upsertForm.borrow_rate_bps_annual.trim() !== "") {
        input.borrow_rate_bps_annual = Number(upsertForm.borrow_rate_bps_annual);
      }
      if (upsertForm.locate_fee_bps.trim() !== "") {
        input.locate_fee_bps = Number(upsertForm.locate_fee_bps);
      }
      if (upsertForm.available_shares.trim() !== "") {
        input.available_shares = Number(upsertForm.available_shares);
      }
      if (upsertForm.min_locate_qty.trim() !== "") {
        input.min_locate_qty = Number(upsertForm.min_locate_qty);
      }
      if (upsertForm.max_locate_qty.trim() !== "") {
        input.max_locate_qty = Number(upsertForm.max_locate_qty);
      }
      await upsertAdminBorrowRate(input);
      setUpsertForm(emptyUpsert);
      await reload();
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setSubmitting(false);
    }
  };

  const handleDelete = async (instrumentKey: string) => {
    setError(null);
    try {
      await deleteAdminBorrowRate(instrumentKey);
      await reload();
    } catch (err) {
      setError(formatApiError(err, m.error));
    }
  };

  const handlePreview = async () => {
    if (!previewForm.instrument_key.trim() || !previewForm.requested_qty.trim()) {
      return;
    }
    setPreviewing(true);
    setPreviewResult(null);
    setError(null);
    try {
      const res = await previewAdminBorrowLocate({
        fund_id: previewForm.fund_id.trim() || undefined,
        instrument_key: previewForm.instrument_key.trim(),
        requested_qty: Number(previewForm.requested_qty),
        intended_price: previewForm.intended_price.trim()
          ? Number(previewForm.intended_price)
          : undefined,
      });
      setPreviewResult(res);
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setPreviewing(false);
    }
  };

  const handleCacheRefresh = async () => {
    setRefreshingCache(true);
    setError(null);
    try {
      const res = await refreshAdminBorrowCache();
      setCache(res);
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setRefreshingCache(false);
    }
  };

  return (
    <section className="admin-section admin-borrow">
      <header className="admin-section__header">
        <div>
          <h2>{m.panelTitle}</h2>
          <p className="admin-section__subtitle">{m.panelSubtitle}</p>
        </div>
        <div className="admin-section__actions">
          <button type="button" onClick={() => void reload()}>
            {m.refresh}
          </button>
        </div>
      </header>

      {error && <div className="admin-section__error">{error}</div>}

      {/* ----- rate table + upsert form ----- */}
      <div className="admin-section__filters">
        <select
          value={filterAvail}
          onChange={(e) => setFilterAvail(e.target.value as BorrowAvailability | "")}
        >
          <option value="">{m.fieldAvailability}</option>
          <option value="easy">{m.availEasy}</option>
          <option value="hard">{m.availHard}</option>
          <option value="restricted">{m.availRestricted}</option>
          <option value="unavailable">{m.availUnavailable}</option>
        </select>
      </div>

      <h3>{m.listTitle}</h3>
      {rows.length === 0 ? (
        <p className="admin-section__empty">{m.listEmpty}</p>
      ) : (
        <div className="admin-table-wrap">
          <table className="admin-table">
            <thead>
              <tr>
                <th>{m.fieldKey}</th>
                <th>{m.fieldSymbol}</th>
                <th>{m.fieldRate}</th>
                <th>{m.fieldLocateFee}</th>
                <th>{m.fieldAvailability}</th>
                <th>{m.fieldAvailable}</th>
                <th>{m.fieldSource}</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.instrument_key}>
                  <td>{r.instrument_key}</td>
                  <td>{r.symbol}</td>
                  <td>{r.borrow_rate_bps_annual}</td>
                  <td>{r.locate_fee_bps}</td>
                  <td>{availabilityLabel(r.availability)}</td>
                  <td>{r.available_shares ?? "—"}</td>
                  <td>{sourceLabel(r.source)}</td>
                  <td>
                    <button type="button" onClick={() => void handleDelete(r.instrument_key)}>
                      {m.deleteButton}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div className="admin-card">
        <h4>{m.upsertButton}</h4>
        <div className="admin-grid">
          <label>
            <span>{m.fieldKey}</span>
            <input
              value={upsertForm.instrument_key}
              onChange={(e) =>
                setUpsertForm({ ...upsertForm, instrument_key: e.target.value })
              }
            />
          </label>
          <label>
            <span>{m.fieldSymbol}</span>
            <input
              value={upsertForm.symbol}
              onChange={(e) =>
                setUpsertForm({ ...upsertForm, symbol: e.target.value })
              }
            />
          </label>
          <label>
            <span>{m.fieldRate}</span>
            <input
              value={upsertForm.borrow_rate_bps_annual}
              onChange={(e) =>
                setUpsertForm({
                  ...upsertForm,
                  borrow_rate_bps_annual: e.target.value,
                })
              }
            />
          </label>
          <label>
            <span>{m.fieldLocateFee}</span>
            <input
              value={upsertForm.locate_fee_bps}
              onChange={(e) =>
                setUpsertForm({ ...upsertForm, locate_fee_bps: e.target.value })
              }
            />
          </label>
          <label>
            <span>{m.fieldAvailability}</span>
            <select
              value={upsertForm.availability}
              onChange={(e) =>
                setUpsertForm({
                  ...upsertForm,
                  availability: e.target.value as BorrowAvailability,
                })
              }
            >
              <option value="easy">{m.availEasy}</option>
              <option value="hard">{m.availHard}</option>
              <option value="restricted">{m.availRestricted}</option>
              <option value="unavailable">{m.availUnavailable}</option>
            </select>
          </label>
          <label>
            <span>{m.fieldAvailable}</span>
            <input
              value={upsertForm.available_shares}
              onChange={(e) =>
                setUpsertForm({
                  ...upsertForm,
                  available_shares: e.target.value,
                })
              }
            />
          </label>
          <label>
            <span>{m.fieldMinLocate}</span>
            <input
              value={upsertForm.min_locate_qty}
              onChange={(e) =>
                setUpsertForm({
                  ...upsertForm,
                  min_locate_qty: e.target.value,
                })
              }
            />
          </label>
          <label>
            <span>{m.fieldMaxLocate}</span>
            <input
              value={upsertForm.max_locate_qty}
              onChange={(e) =>
                setUpsertForm({
                  ...upsertForm,
                  max_locate_qty: e.target.value,
                })
              }
            />
          </label>
          <label>
            <span>{m.fieldSource}</span>
            <select
              value={upsertForm.source}
              onChange={(e) =>
                setUpsertForm({
                  ...upsertForm,
                  source: e.target.value as BorrowCalibrationSource,
                })
              }
            >
              <option value="manual">{m.sourceManual}</option>
              <option value="broker_quote">{m.sourceBrokerQuote}</option>
              <option value="agent_lender">{m.sourceAgentLender}</option>
              <option value="historical_calibration">{m.sourceHistorical}</option>
              <option value="public_feed">{m.sourcePublicFeed}</option>
            </select>
          </label>
          <label className="admin-grid__full">
            <span>{m.fieldNote}</span>
            <input
              value={upsertForm.note}
              onChange={(e) =>
                setUpsertForm({ ...upsertForm, note: e.target.value })
              }
            />
          </label>
        </div>
        <button
          type="button"
          onClick={() => void handleUpsert()}
          disabled={submitting}
        >
          {submitting ? m.upsertSubmitting : m.upsertButton}
        </button>
      </div>

      {/* ----- locate preview ----- */}
      <div className="admin-card">
        <h4>{m.previewTitle}</h4>
        <p className="admin-section__subtitle">{m.previewSubtitle}</p>
        <div className="admin-grid">
          <label>
            <span>{m.previewFundLabel}</span>
            <input
              value={previewForm.fund_id}
              onChange={(e) =>
                setPreviewForm({ ...previewForm, fund_id: e.target.value })
              }
            />
          </label>
          <label>
            <span>{m.previewKeyLabel}</span>
            <input
              value={previewForm.instrument_key}
              onChange={(e) =>
                setPreviewForm({
                  ...previewForm,
                  instrument_key: e.target.value,
                })
              }
            />
          </label>
          <label>
            <span>{m.previewQtyLabel}</span>
            <input
              value={previewForm.requested_qty}
              onChange={(e) =>
                setPreviewForm({
                  ...previewForm,
                  requested_qty: e.target.value,
                })
              }
            />
          </label>
          <label>
            <span>{m.previewPriceLabel}</span>
            <input
              value={previewForm.intended_price}
              onChange={(e) =>
                setPreviewForm({
                  ...previewForm,
                  intended_price: e.target.value,
                })
              }
            />
          </label>
        </div>
        <button
          type="button"
          onClick={() => void handlePreview()}
          disabled={previewing}
        >
          {previewing ? m.previewSubmitting : m.previewSubmit}
        </button>
        {previewResult && (
          <table className="admin-table">
            <tbody>
              <tr>
                <th>{m.previewResultDecision}</th>
                <td>{previewResult.decision}</td>
              </tr>
              <tr>
                <th>{m.previewResultRate}</th>
                <td>{previewResult.borrow_rate_bps}</td>
              </tr>
              <tr>
                <th>{m.previewResultLocateFee}</th>
                <td>{previewResult.locate_fee_amount}</td>
              </tr>
              <tr>
                <th>{m.previewResultNotional}</th>
                <td>{previewResult.notional}</td>
              </tr>
            </tbody>
          </table>
        )}
      </div>

      {/* ----- cache panel ----- */}
      <div className="admin-card">
        <h4>{m.cacheTitle}</h4>
        <p>
          {m.cacheSize}: {cache?.size ?? "—"}
          {cache?.last_refresh && (
            <>
              {" · "}
              {m.cacheLastRefresh}: {formatTimestamp(cache.last_refresh)}
            </>
          )}
        </p>
        <button
          type="button"
          onClick={() => void handleCacheRefresh()}
          disabled={refreshingCache}
        >
          {refreshingCache ? m.cacheRefreshing : m.cacheRefreshButton}
        </button>
      </div>

      {/* ----- locate audit panel ----- */}
      <div className="admin-card">
        <h4>{m.auditTitle}</h4>
        <div className="admin-section__filters">
          <input
            placeholder={m.auditFundFilter}
            value={filterFund}
            onChange={(e) => setFilterFund(e.target.value)}
          />
          <select
            value={filterDecision}
            onChange={(e) =>
              setFilterDecision(e.target.value as BorrowLocateDecisionKind | "")
            }
          >
            <option value="">{m.auditDecisionFilter}</option>
            <option value="allow">allow</option>
            <option value="reject_unavailable">reject_unavailable</option>
            <option value="reject_insufficient">reject_insufficient</option>
            <option value="reject_below_min">reject_below_min</option>
            <option value="reject_above_max">reject_above_max</option>
            <option value="no_calibration">no_calibration</option>
          </select>
        </div>
        {events.length === 0 ? (
          <p className="admin-section__empty">{m.auditEmpty}</p>
        ) : (
          <div className="admin-table-wrap">
            <table className="admin-table">
              <thead>
                <tr>
                  <th>created_at</th>
                  <th>fund</th>
                  <th>symbol</th>
                  <th>qty</th>
                  <th>decision</th>
                  <th>rate</th>
                  <th>fee</th>
                  <th>reason</th>
                </tr>
              </thead>
              <tbody>
                {events.map((e) => (
                  <tr key={e.id}>
                    <td>{formatTimestamp(e.created_at)}</td>
                    <td>{shortFundID(e.fund_id)}</td>
                    <td>{e.symbol}</td>
                    <td>{e.requested_qty}</td>
                    <td>{e.decision}</td>
                    <td>{e.rate_bps_annual ?? "—"}</td>
                    <td>{e.locate_fee_amount ?? "—"}</td>
                    <td>{e.reason ?? ""}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {/* ----- borrow-fee ledger panel ----- */}
      <div className="admin-card">
        <h4>{m.ledgerTitle}</h4>
        {ledger.length === 0 ? (
          <p className="admin-section__empty">{m.ledgerEmpty}</p>
        ) : (
          <div className="admin-table-wrap">
            <table className="admin-table">
              <thead>
                <tr>
                  <th>date</th>
                  <th>fund</th>
                  <th>symbol</th>
                  <th>short qty</th>
                  <th>price</th>
                  <th>rate bps</th>
                  <th>fee</th>
                </tr>
              </thead>
              <tbody>
                {ledger.map((e) => (
                  <tr key={e.id}>
                    <td>{e.accrual_date}</td>
                    <td>{shortFundID(e.fund_id)}</td>
                    <td>{e.symbol}</td>
                    <td>{e.short_qty}</td>
                    <td>{e.market_price}</td>
                    <td>{e.rate_bps_annual}</td>
                    <td>{e.fee_amount.toFixed(2)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </section>
  );
}

function formatTimestamp(s: string | null | undefined): string {
  if (!s) return "—";
  const d = new Date(s);
  if (Number.isNaN(d.getTime())) return s;
  return d.toLocaleString();
}

function shortFundID(id: string): string {
  if (!id) return "—";
  if (id.length <= 12) return id;
  return `${id.slice(0, 8)}…`;
}

export default AdminBorrowSection;
