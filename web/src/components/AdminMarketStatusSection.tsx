// AdminMarketStatusSection — admin web component for the S6.1
// market-status gate.
//
// What it shows
//
//   - Instruments table: per-symbol live state (status, halt
//     reason, lower/upper limits, last quote freshness). Filters
//     by market / status. Halt / Unhalt / Set-limits buttons
//     fire the convenience endpoints.
//   - Calendar table: per-market open/close/half-day; operator
//     can add or update one day at a time.
//   - Events table: append-only audit of every reject / warn the
//     gate emitted, filterable by rule_code.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  formatApiError,
  listAdminMarketStatusInstruments,
  haltAdminMarketStatusInstrument,
  unhaltAdminMarketStatusInstrument,
  setAdminMarketStatusLimits,
  listAdminMarketStatusCalendar,
  upsertAdminMarketStatusCalendar,
  listAdminMarketStatusEvents,
  type MarketStatusInstrument,
  type MarketStatusInstrumentState,
  type MarketStatusCalendarDay,
  type MarketStatusEvent,
  type MarketStatusRuleCode,
} from "../lib/api";

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
  fieldStatus: string;
  fieldHaltReason: string;
  fieldHaltUntil: string;
  fieldLower: string;
  fieldUpper: string;
  fieldLastQuoteAt: string;
  fieldStalenessBudget: string;
  statusTrading: string;
  statusHalted: string;
  statusSuspended: string;
  haltButton: string;
  haltSubmitting: string;
  haltDialogTitle: string;
  haltReasonLabel: string;
  haltUntilLabel: string;
  unhaltButton: string;
  setLimitsButton: string;
  setLimitsDialogTitle: string;
  upsertDialogTitle: string;
  saveButton: string;
  saveSubmitting: string;
  cancelButton: string;
  eventsTitle: string;
  eventDecision: string;
  eventRule: string;
  eventSummary: string;
  eventDetected: string;
  decisionAllow: string;
  decisionWarn: string;
  decisionReject: string;
  ruleHalted: string;
  ruleSuspended: string;
  rulePriceLimit: string;
  ruleStaleQuote: string;
  ruleMarketClosed: string;
  ruleHalfDayClosed: string;
  calendarTitle: string;
  calendarMarketLabel: string;
  calendarFromLabel: string;
  calendarToLabel: string;
  calendarLoadButton: string;
  calendarUpsertTitle: string;
  calendarIsOpen: string;
  calendarHalfDay: string;
  calendarOpenLocal: string;
  calendarCloseLocal: string;
  calendarTZ: string;
  calendarNote: string;
  error: string;
}

const messages: Record<Language, Messages> = {
  "zh-CN": {
    panelTitle: "市场状态门控（停牌 / 涨跌停 / 陈旧报价 / 交易日历）",
    panelSubtitle:
      "在订单进入撮合引擎前先做市场可达性检查：停牌或暂停的标的、超出涨跌停的限价、陈旧报价、节假日 / 半天市等。任意硬性条件不满足则拒绝；仅警告项（如报价稍陈旧）会以备注形式跟随订单流向回放与对账。",
    refresh: "刷新",
    instrumentsTitle: "标的状态",
    instrumentsEmpty: "尚未配置任何标的",
    fieldKey: "instrument_key",
    fieldSymbol: "代码",
    fieldMarket: "市场",
    fieldStatus: "状态",
    fieldHaltReason: "停牌原因",
    fieldHaltUntil: "停牌至",
    fieldLower: "跌停价",
    fieldUpper: "涨停价",
    fieldLastQuoteAt: "最近报价时间",
    fieldStalenessBudget: "陈旧阈值（秒）",
    statusTrading: "正常交易",
    statusHalted: "临时停牌",
    statusSuspended: "长期暂停",
    haltButton: "停牌",
    haltSubmitting: "处理中…",
    haltDialogTitle: "停牌",
    haltReasonLabel: "原因（必填）",
    haltUntilLabel: "恢复时间（可选 RFC3339）",
    unhaltButton: "复牌",
    setLimitsButton: "设置涨跌停",
    setLimitsDialogTitle: "涨跌停限价",
    upsertDialogTitle: "编辑标的状态",
    saveButton: "保存",
    saveSubmitting: "保存中…",
    cancelButton: "取消",
    eventsTitle: "门控事件",
    eventDecision: "判定",
    eventRule: "规则",
    eventSummary: "说明",
    eventDetected: "时间",
    decisionAllow: "通过",
    decisionWarn: "警告",
    decisionReject: "拒绝",
    ruleHalted: "已停牌",
    ruleSuspended: "长期暂停",
    rulePriceLimit: "涨跌停越线",
    ruleStaleQuote: "报价陈旧",
    ruleMarketClosed: "休市",
    ruleHalfDayClosed: "半天市后",
    calendarTitle: "交易日历",
    calendarMarketLabel: "市场代码（如 CN / US / HK）",
    calendarFromLabel: "从",
    calendarToLabel: "到",
    calendarLoadButton: "加载",
    calendarUpsertTitle: "新增 / 编辑日历",
    calendarIsOpen: "开市",
    calendarHalfDay: "半天市",
    calendarOpenLocal: "开市时间（HH:MM）",
    calendarCloseLocal: "收市时间（HH:MM）",
    calendarTZ: "时区",
    calendarNote: "备注",
    error: "加载失败",
  },
  "en-US": {
    panelTitle: "Market-status gate (halts / price limits / stale quotes / calendar)",
    panelSubtitle:
      "Pre-trade reachability gate. Suspended or halted instruments, limit-breaching prices, stale quotes, market-closed and half-day-closed sessions are caught BEFORE the matching engine sees the order. Hard rejects block the trade; soft warnings (e.g. mildly stale quote) ride on the order so attribution can see them later.",
    refresh: "Refresh",
    instrumentsTitle: "Instrument status",
    instrumentsEmpty: "No instruments configured yet.",
    fieldKey: "instrument_key",
    fieldSymbol: "Symbol",
    fieldMarket: "Market",
    fieldStatus: "Status",
    fieldHaltReason: "Halt reason",
    fieldHaltUntil: "Halt until",
    fieldLower: "Lower limit",
    fieldUpper: "Upper limit",
    fieldLastQuoteAt: "Last quote",
    fieldStalenessBudget: "Staleness budget (s)",
    statusTrading: "Trading",
    statusHalted: "Halted",
    statusSuspended: "Suspended",
    haltButton: "Halt",
    haltSubmitting: "Submitting…",
    haltDialogTitle: "Halt instrument",
    haltReasonLabel: "Reason (required)",
    haltUntilLabel: "Halt until (optional RFC3339)",
    unhaltButton: "Unhalt",
    setLimitsButton: "Set price limits",
    setLimitsDialogTitle: "Price limits",
    upsertDialogTitle: "Edit instrument",
    saveButton: "Save",
    saveSubmitting: "Saving…",
    cancelButton: "Cancel",
    eventsTitle: "Gate events",
    eventDecision: "Decision",
    eventRule: "Rule",
    eventSummary: "Summary",
    eventDetected: "When",
    decisionAllow: "allow",
    decisionWarn: "warn",
    decisionReject: "reject",
    ruleHalted: "halted",
    ruleSuspended: "suspended",
    rulePriceLimit: "price-limit",
    ruleStaleQuote: "stale quote",
    ruleMarketClosed: "market closed",
    ruleHalfDayClosed: "half-day closed",
    calendarTitle: "Trading calendar",
    calendarMarketLabel: "Market (e.g. CN / US / HK)",
    calendarFromLabel: "From",
    calendarToLabel: "To",
    calendarLoadButton: "Load",
    calendarUpsertTitle: "Add / edit calendar day",
    calendarIsOpen: "Open",
    calendarHalfDay: "Half-day",
    calendarOpenLocal: "Open local (HH:MM)",
    calendarCloseLocal: "Close local (HH:MM)",
    calendarTZ: "Timezone",
    calendarNote: "Note",
    error: "Failed to load",
  },
};

interface Props {
  language?: Language;
}

const statusLabel = (m: Messages, s: MarketStatusInstrumentState): string => {
  switch (s) {
    case "trading":
      return m.statusTrading;
    case "halted":
      return m.statusHalted;
    case "suspended":
      return m.statusSuspended;
    default:
      return s;
  }
};

const ruleLabel = (m: Messages, r: MarketStatusRuleCode): string => {
  switch (r) {
    case "halted":
      return m.ruleHalted;
    case "suspended":
      return m.ruleSuspended;
    case "price_limit":
      return m.rulePriceLimit;
    case "stale_quote":
      return m.ruleStaleQuote;
    case "market_closed":
      return m.ruleMarketClosed;
    case "half_day_closed":
      return m.ruleHalfDayClosed;
    default:
      return r;
  }
};

const formatTimestamp = (iso?: string): string => {
  if (!iso) return "—";
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
};

interface HaltDraft {
  reason: string;
  haltUntil: string;
}

interface LimitsDraft {
  lower: string;
  upper: string;
}

interface CalendarDraft {
  market: string;
  date: string;
  isOpen: boolean;
  openLocal: string;
  closeLocal: string;
  marketTZ: string;
  halfDay: boolean;
  note: string;
}

const blankCalendarDraft: CalendarDraft = {
  market: "",
  date: "",
  isOpen: true,
  openLocal: "09:30",
  closeLocal: "15:00",
  marketTZ: "Asia/Shanghai",
  halfDay: false,
  note: "",
};

export function AdminMarketStatusSection({ language = "zh-CN" }: Props) {
  const m = useMemo<Messages>(() => messages[language] ?? messages["zh-CN"], [language]);
  const [instruments, setInstruments] = useState<MarketStatusInstrument[]>([]);
  const [events, setEvents] = useState<MarketStatusEvent[]>([]);
  const [calendar, setCalendar] = useState<MarketStatusCalendarDay[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [marketFilter, setMarketFilter] = useState("");
  const [statusFilter, setStatusFilter] = useState<MarketStatusInstrumentState | "">("");
  const [symbolFilter, setSymbolFilter] = useState("");
  const [haltTarget, setHaltTarget] = useState<MarketStatusInstrument | null>(null);
  const [haltDraft, setHaltDraft] = useState<HaltDraft>({ reason: "", haltUntil: "" });
  const [haltSubmitting, setHaltSubmitting] = useState(false);
  const [limitsTarget, setLimitsTarget] = useState<MarketStatusInstrument | null>(null);
  const [limitsDraft, setLimitsDraft] = useState<LimitsDraft>({ lower: "", upper: "" });
  const [limitsSubmitting, setLimitsSubmitting] = useState(false);
  const [calendarMarket, setCalendarMarket] = useState("CN");
  const [calendarFrom, setCalendarFrom] = useState("");
  const [calendarTo, setCalendarTo] = useState("");
  const [calendarDraft, setCalendarDraft] = useState<CalendarDraft>(blankCalendarDraft);
  const [calendarSubmitting, setCalendarSubmitting] = useState(false);

  const fetchInstruments = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const resp = await listAdminMarketStatusInstruments({
        market: marketFilter || undefined,
        status: statusFilter || undefined,
        symbol: symbolFilter || undefined,
        limit: 200,
      });
      setInstruments(resp.instruments ?? []);
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setLoading(false);
    }
  }, [marketFilter, statusFilter, symbolFilter, m.error]);

  const fetchEvents = useCallback(async () => {
    try {
      const resp = await listAdminMarketStatusEvents({ limit: 50 });
      setEvents(resp.events ?? []);
    } catch (err) {
      setError(formatApiError(err, m.error));
    }
  }, [m.error]);

  const fetchCalendar = useCallback(async () => {
    if (!calendarMarket) return;
    try {
      const resp = await listAdminMarketStatusCalendar(
        calendarMarket,
        calendarFrom || undefined,
        calendarTo || undefined,
      );
      setCalendar(resp.days ?? []);
    } catch (err) {
      setError(formatApiError(err, m.error));
    }
  }, [calendarMarket, calendarFrom, calendarTo, m.error]);

  useEffect(() => {
    fetchInstruments().catch(() => {});
    fetchEvents().catch(() => {});
  }, [fetchInstruments, fetchEvents]);

  const submitHalt = async () => {
    if (!haltTarget || !haltDraft.reason.trim()) return;
    setHaltSubmitting(true);
    try {
      await haltAdminMarketStatusInstrument(haltTarget.instrument_key, haltDraft.reason.trim(), haltDraft.haltUntil.trim() || undefined);
      setHaltTarget(null);
      setHaltDraft({ reason: "", haltUntil: "" });
      await fetchInstruments();
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setHaltSubmitting(false);
    }
  };

  const submitUnhalt = async (key: string) => {
    try {
      await unhaltAdminMarketStatusInstrument(key);
      await fetchInstruments();
    } catch (err) {
      setError(formatApiError(err, m.error));
    }
  };

  const submitLimits = async () => {
    if (!limitsTarget) return;
    setLimitsSubmitting(true);
    try {
      const lower = limitsDraft.lower.trim() === "" ? null : Number(limitsDraft.lower);
      const upper = limitsDraft.upper.trim() === "" ? null : Number(limitsDraft.upper);
      await setAdminMarketStatusLimits(limitsTarget.instrument_key, lower, upper);
      setLimitsTarget(null);
      setLimitsDraft({ lower: "", upper: "" });
      await fetchInstruments();
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setLimitsSubmitting(false);
    }
  };

  const submitCalendar = async () => {
    if (!calendarDraft.market || !calendarDraft.date) return;
    setCalendarSubmitting(true);
    try {
      await upsertAdminMarketStatusCalendar(calendarDraft.market, calendarDraft.date, {
        is_open: calendarDraft.isOpen,
        open_local: calendarDraft.openLocal,
        close_local: calendarDraft.closeLocal,
        market_tz: calendarDraft.marketTZ,
        half_day: calendarDraft.halfDay,
        note: calendarDraft.note,
      });
      setCalendarDraft({ ...blankCalendarDraft, market: calendarDraft.market });
      await fetchCalendar();
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setCalendarSubmitting(false);
    }
  };

  return (
    <section className="rounded-lg border border-slate-200 bg-white p-4">
      <header className="flex items-start justify-between gap-4 pb-3">
        <div>
          <h2 className="text-lg font-semibold text-slate-900">{m.panelTitle}</h2>
          <p className="text-sm text-slate-500">{m.panelSubtitle}</p>
        </div>
        <button
          type="button"
          onClick={() => {
            fetchInstruments().catch(() => {});
            fetchEvents().catch(() => {});
          }}
          className="rounded bg-slate-900 px-3 py-1 text-sm text-white"
        >
          {m.refresh}
        </button>
      </header>

      {error && (
        <div className="mb-3 rounded border border-red-300 bg-red-50 p-2 text-sm text-red-700">
          {error}
        </div>
      )}

      {/* Filters */}
      <div className="mb-3 flex flex-wrap gap-2 text-sm">
        <input
          placeholder={m.fieldMarket}
          value={marketFilter}
          onChange={(e) => setMarketFilter(e.target.value)}
          className="rounded border border-slate-300 p-1"
        />
        <select
          value={statusFilter}
          onChange={(e) => setStatusFilter(e.target.value as MarketStatusInstrumentState | "")}
          className="rounded border border-slate-300 p-1"
        >
          <option value="">{m.fieldStatus}</option>
          <option value="trading">{m.statusTrading}</option>
          <option value="halted">{m.statusHalted}</option>
          <option value="suspended">{m.statusSuspended}</option>
        </select>
        <input
          placeholder={m.fieldSymbol}
          value={symbolFilter}
          onChange={(e) => setSymbolFilter(e.target.value)}
          className="rounded border border-slate-300 p-1"
        />
      </div>

      {/* Instruments table */}
      <h3 className="mb-2 text-sm font-semibold text-slate-800">{m.instrumentsTitle}</h3>
      <div className="overflow-x-auto pb-3">
        <table className="min-w-full divide-y divide-slate-200 text-sm">
          <thead className="bg-slate-50 text-left text-xs font-semibold uppercase text-slate-600">
            <tr>
              <th className="px-2 py-1">{m.fieldSymbol}</th>
              <th className="px-2 py-1">{m.fieldMarket}</th>
              <th className="px-2 py-1">{m.fieldStatus}</th>
              <th className="px-2 py-1">{m.fieldHaltReason}</th>
              <th className="px-2 py-1">{m.fieldLower}</th>
              <th className="px-2 py-1">{m.fieldUpper}</th>
              <th className="px-2 py-1">{m.fieldLastQuoteAt}</th>
              <th className="px-2 py-1"></th>
            </tr>
          </thead>
          <tbody className="divide-y divide-slate-200 text-slate-700">
            {instruments.length === 0 && !loading && (
              <tr>
                <td colSpan={8} className="px-2 py-2 text-center text-slate-400">
                  {m.instrumentsEmpty}
                </td>
              </tr>
            )}
            {instruments.map((inst) => (
              <tr key={inst.instrument_key}>
                <td className="px-2 py-1">
                  <div className="font-mono text-xs">{inst.symbol}</div>
                  <div className="font-mono text-xs text-slate-400">{inst.instrument_key}</div>
                </td>
                <td className="px-2 py-1">{inst.market}</td>
                <td className="px-2 py-1">
                  <span
                    className={`rounded px-2 py-0.5 text-xs font-semibold ${
                      inst.status === "trading"
                        ? "bg-emerald-100 text-emerald-800"
                        : inst.status === "halted"
                        ? "bg-amber-100 text-amber-800"
                        : "bg-red-100 text-red-800"
                    }`}
                  >
                    {statusLabel(m, inst.status)}
                  </span>
                </td>
                <td className="px-2 py-1 text-xs text-slate-500">{inst.halt_reason ?? ""}</td>
                <td className="px-2 py-1 tabular-nums">
                  {inst.lower_limit !== undefined && inst.lower_limit !== null
                    ? inst.lower_limit
                    : "—"}
                </td>
                <td className="px-2 py-1 tabular-nums">
                  {inst.upper_limit !== undefined && inst.upper_limit !== null
                    ? inst.upper_limit
                    : "—"}
                </td>
                <td className="px-2 py-1 text-xs">{formatTimestamp(inst.last_quote_at)}</td>
                <td className="px-2 py-1 text-right text-xs space-x-1">
                  {inst.status !== "halted" && (
                    <button
                      type="button"
                      onClick={() => {
                        setHaltTarget(inst);
                        setHaltDraft({ reason: "", haltUntil: "" });
                      }}
                      className="text-amber-700 underline"
                    >
                      {m.haltButton}
                    </button>
                  )}
                  {inst.status === "halted" && (
                    <button
                      type="button"
                      onClick={() => submitUnhalt(inst.instrument_key)}
                      className="text-emerald-700 underline"
                    >
                      {m.unhaltButton}
                    </button>
                  )}
                  <button
                    type="button"
                    onClick={() => {
                      setLimitsTarget(inst);
                      setLimitsDraft({
                        lower: inst.lower_limit !== undefined && inst.lower_limit !== null ? String(inst.lower_limit) : "",
                        upper: inst.upper_limit !== undefined && inst.upper_limit !== null ? String(inst.upper_limit) : "",
                      });
                    }}
                    className="text-blue-700 underline"
                  >
                    {m.setLimitsButton}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {/* Calendar */}
      <h3 className="mb-2 text-sm font-semibold text-slate-800">{m.calendarTitle}</h3>
      <div className="mb-2 flex flex-wrap items-end gap-2 text-sm">
        <input
          placeholder={m.calendarMarketLabel}
          value={calendarMarket}
          onChange={(e) => setCalendarMarket(e.target.value)}
          className="rounded border border-slate-300 p-1"
        />
        <input
          type="date"
          value={calendarFrom}
          onChange={(e) => setCalendarFrom(e.target.value)}
          className="rounded border border-slate-300 p-1"
        />
        <input
          type="date"
          value={calendarTo}
          onChange={(e) => setCalendarTo(e.target.value)}
          className="rounded border border-slate-300 p-1"
        />
        <button
          type="button"
          onClick={() => fetchCalendar().catch(() => {})}
          className="rounded border border-slate-300 px-3 py-1"
        >
          {m.calendarLoadButton}
        </button>
      </div>
      <div className="mb-3 overflow-x-auto">
        <table className="min-w-full divide-y divide-slate-200 text-sm">
          <thead className="bg-slate-50 text-left text-xs font-semibold uppercase text-slate-600">
            <tr>
              <th className="px-2 py-1">{m.calendarFromLabel}</th>
              <th className="px-2 py-1">{m.calendarIsOpen}</th>
              <th className="px-2 py-1">{m.calendarHalfDay}</th>
              <th className="px-2 py-1">{m.calendarOpenLocal}</th>
              <th className="px-2 py-1">{m.calendarCloseLocal}</th>
              <th className="px-2 py-1">{m.calendarTZ}</th>
              <th className="px-2 py-1">{m.calendarNote}</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-slate-200 text-slate-700">
            {calendar.map((d) => (
              <tr key={`${d.market}:${d.trading_date}`}>
                <td className="px-2 py-1 font-mono text-xs">{d.trading_date}</td>
                <td className="px-2 py-1">{d.is_open ? "✓" : "✗"}</td>
                <td className="px-2 py-1">{d.half_day ? "✓" : ""}</td>
                <td className="px-2 py-1 font-mono text-xs">{d.open_local}</td>
                <td className="px-2 py-1 font-mono text-xs">{d.close_local}</td>
                <td className="px-2 py-1 font-mono text-xs">{d.market_tz}</td>
                <td className="px-2 py-1 text-xs text-slate-500">{d.note ?? ""}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <details className="mb-4 rounded border border-slate-200 bg-slate-50 p-3 text-sm">
        <summary className="cursor-pointer font-semibold text-slate-700">{m.calendarUpsertTitle}</summary>
        <div className="mt-2 grid grid-cols-2 gap-2 md:grid-cols-4">
          <input
            placeholder={m.calendarMarketLabel}
            value={calendarDraft.market}
            onChange={(e) => setCalendarDraft({ ...calendarDraft, market: e.target.value })}
            className="rounded border border-slate-300 p-1"
          />
          <input
            type="date"
            value={calendarDraft.date}
            onChange={(e) => setCalendarDraft({ ...calendarDraft, date: e.target.value })}
            className="rounded border border-slate-300 p-1"
          />
          <label className="text-xs">
            <input
              type="checkbox"
              checked={calendarDraft.isOpen}
              onChange={(e) => setCalendarDraft({ ...calendarDraft, isOpen: e.target.checked })}
            />{" "}
            {m.calendarIsOpen}
          </label>
          <label className="text-xs">
            <input
              type="checkbox"
              checked={calendarDraft.halfDay}
              onChange={(e) => setCalendarDraft({ ...calendarDraft, halfDay: e.target.checked })}
            />{" "}
            {m.calendarHalfDay}
          </label>
          <input
            placeholder={m.calendarOpenLocal}
            value={calendarDraft.openLocal}
            onChange={(e) => setCalendarDraft({ ...calendarDraft, openLocal: e.target.value })}
            className="rounded border border-slate-300 p-1"
          />
          <input
            placeholder={m.calendarCloseLocal}
            value={calendarDraft.closeLocal}
            onChange={(e) => setCalendarDraft({ ...calendarDraft, closeLocal: e.target.value })}
            className="rounded border border-slate-300 p-1"
          />
          <input
            placeholder={m.calendarTZ}
            value={calendarDraft.marketTZ}
            onChange={(e) => setCalendarDraft({ ...calendarDraft, marketTZ: e.target.value })}
            className="rounded border border-slate-300 p-1"
          />
          <input
            placeholder={m.calendarNote}
            value={calendarDraft.note}
            onChange={(e) => setCalendarDraft({ ...calendarDraft, note: e.target.value })}
            className="rounded border border-slate-300 p-1"
          />
        </div>
        <div className="mt-2 flex justify-end">
          <button
            type="button"
            onClick={submitCalendar}
            disabled={calendarSubmitting || !calendarDraft.market || !calendarDraft.date}
            className="rounded bg-slate-900 px-3 py-1 text-sm text-white disabled:opacity-50"
          >
            {calendarSubmitting ? m.saveSubmitting : m.saveButton}
          </button>
        </div>
      </details>

      {/* Events */}
      <h3 className="mb-2 text-sm font-semibold text-slate-800">{m.eventsTitle}</h3>
      <div className="overflow-x-auto">
        <table className="min-w-full divide-y divide-slate-200 text-sm">
          <thead className="bg-slate-50 text-left text-xs font-semibold uppercase text-slate-600">
            <tr>
              <th className="px-2 py-1">{m.eventDetected}</th>
              <th className="px-2 py-1">{m.fieldSymbol}</th>
              <th className="px-2 py-1">{m.eventDecision}</th>
              <th className="px-2 py-1">{m.eventRule}</th>
              <th className="px-2 py-1">{m.eventSummary}</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-slate-200 text-slate-700">
            {events.length === 0 && (
              <tr>
                <td colSpan={5} className="px-2 py-2 text-center text-slate-400">
                  —
                </td>
              </tr>
            )}
            {events.map((ev) => (
              <tr key={ev.id} className="align-top">
                <td className="px-2 py-1 text-xs tabular-nums">{formatTimestamp(ev.detected_at)}</td>
                <td className="px-2 py-1 font-mono text-xs">{ev.symbol ?? ev.instrument_key}</td>
                <td className="px-2 py-1">
                  <span
                    className={`rounded px-2 py-0.5 text-xs font-semibold ${
                      ev.decision === "reject"
                        ? "bg-red-100 text-red-800"
                        : ev.decision === "warn"
                        ? "bg-amber-100 text-amber-800"
                        : "bg-emerald-100 text-emerald-800"
                    }`}
                  >
                    {ev.decision === "reject"
                      ? m.decisionReject
                      : ev.decision === "warn"
                      ? m.decisionWarn
                      : m.decisionAllow}
                  </span>
                </td>
                <td className="px-2 py-1 text-xs">{ruleLabel(m, ev.rule_code)}</td>
                <td className="px-2 py-1 text-xs">{ev.summary ?? ""}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {/* Halt dialog */}
      {haltTarget && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/40"
          onClick={() => setHaltTarget(null)}
          role="dialog"
          aria-modal="true"
        >
          <div
            className="w-full max-w-md rounded-lg bg-white p-4 shadow-lg"
            onClick={(e) => e.stopPropagation()}
          >
            <h3 className="text-base font-semibold text-slate-900">
              {m.haltDialogTitle}: {haltTarget.symbol}
            </h3>
            <label className="mt-3 block text-xs">
              <span className="block text-slate-500">{m.haltReasonLabel}</span>
              <input
                value={haltDraft.reason}
                onChange={(e) => setHaltDraft({ ...haltDraft, reason: e.target.value })}
                className="mt-1 w-full rounded border border-slate-300 p-2 text-sm"
              />
            </label>
            <label className="mt-2 block text-xs">
              <span className="block text-slate-500">{m.haltUntilLabel}</span>
              <input
                value={haltDraft.haltUntil}
                onChange={(e) => setHaltDraft({ ...haltDraft, haltUntil: e.target.value })}
                placeholder="2026-06-01T16:30:00Z"
                className="mt-1 w-full rounded border border-slate-300 p-2 text-sm font-mono"
              />
            </label>
            <div className="mt-3 flex justify-end gap-2">
              <button
                type="button"
                className="rounded border border-slate-300 px-3 py-1 text-sm"
                onClick={() => setHaltTarget(null)}
                disabled={haltSubmitting}
              >
                {m.cancelButton}
              </button>
              <button
                type="button"
                className="rounded bg-slate-900 px-3 py-1 text-sm text-white disabled:opacity-50"
                onClick={() => submitHalt()}
                disabled={haltSubmitting || !haltDraft.reason.trim()}
              >
                {haltSubmitting ? m.haltSubmitting : m.haltButton}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Limits dialog */}
      {limitsTarget && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/40"
          onClick={() => setLimitsTarget(null)}
          role="dialog"
          aria-modal="true"
        >
          <div
            className="w-full max-w-md rounded-lg bg-white p-4 shadow-lg"
            onClick={(e) => e.stopPropagation()}
          >
            <h3 className="text-base font-semibold text-slate-900">
              {m.setLimitsDialogTitle}: {limitsTarget.symbol}
            </h3>
            <div className="mt-3 grid grid-cols-2 gap-2">
              <label className="text-xs">
                <span className="block text-slate-500">{m.fieldLower}</span>
                <input
                  value={limitsDraft.lower}
                  onChange={(e) => setLimitsDraft({ ...limitsDraft, lower: e.target.value })}
                  className="mt-1 w-full rounded border border-slate-300 p-2 text-sm tabular-nums"
                />
              </label>
              <label className="text-xs">
                <span className="block text-slate-500">{m.fieldUpper}</span>
                <input
                  value={limitsDraft.upper}
                  onChange={(e) => setLimitsDraft({ ...limitsDraft, upper: e.target.value })}
                  className="mt-1 w-full rounded border border-slate-300 p-2 text-sm tabular-nums"
                />
              </label>
            </div>
            <div className="mt-3 flex justify-end gap-2">
              <button
                type="button"
                className="rounded border border-slate-300 px-3 py-1 text-sm"
                onClick={() => setLimitsTarget(null)}
                disabled={limitsSubmitting}
              >
                {m.cancelButton}
              </button>
              <button
                type="button"
                className="rounded bg-slate-900 px-3 py-1 text-sm text-white disabled:opacity-50"
                onClick={() => submitLimits()}
                disabled={limitsSubmitting}
              >
                {limitsSubmitting ? m.saveSubmitting : m.saveButton}
              </button>
            </div>
          </div>
        </div>
      )}
    </section>
  );
}

export default AdminMarketStatusSection;
