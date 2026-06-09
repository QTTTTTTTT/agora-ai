import React, { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import {
  formatApiError,
  getDailyPickDetail,
  listDailyPicks,
  type AdvisorVerdict,
  type DailyPickDetailResponse,
  type DailyPickRow,
} from "../lib/api";
import { useAppPreferences } from "../lib/preferences";
import MasterVerdictCard from "../components/MasterVerdictCard";
import TacticVerdictCard from "../components/TacticVerdictCard";
import TechnicalSnapshotCard from "../components/TechnicalSnapshotCard";
import { ComplianceBanner } from "../components/ComplianceBanner";
import { ComplianceAckModal } from "../components/ComplianceAckModal";
import { formatModelVerdict, useCompliance } from "../lib/compliance";

// language → compliance locale conversion is duplicated in
// several pages; consolidated upstream into useCompliance someday.
// For now, do it inline so this page is self-contained.

// DailyPicks — the /daily-picks publisher surface.
//
// Browse-grid view of TODAY's (or a chosen historical day's) shared
// stock-by-stock analysis, computed once nightly by the publisher
// loop and served to every reader identically. This is the SEC
// Publishers Exclusion-safe form of the advisor product: no user
// inputs, no personalisation, no on-demand LLM calls.
//
// Tier flow:
//   * Free   → cards from 14d ago and earlier; today's tab shows an
//              upgrade overlay if today's set exists.
//   * Paid   → cards from today; detail-modal opens cap at N/day.
//
// Renders share components (MasterVerdictCard / TacticVerdictCard)
// with /advisor so visual language stays consistent — what's
// different is the SOURCE of the data (shared cache vs per-user
// consultation), not its shape.

// Strategy presets surfaced in the page filter. Each one must
// have a matching active row in daily_pick_watchlists or the grid
// renders empty. Migration 108 seeded the four lenses below on a
// shared 50-symbol S&P large-cap universe so users can compare a
// value / growth / disruption / macro reading of the SAME stock
// side-by-side (open one symbol in different presets to see how
// the philosophical lens flips the verdict).
//
// Ordering note: disruptive stays first because it has the
// shortest LLM fan-out and is therefore the freshest each day
// (cron writes it before the slower 3-master panels finish).
const PRESETS: Array<{ key: string; labelZh: string; labelEn: string }> = [
  { key: "disruptive",   labelZh: "颠覆创新（Cathie Wood）",            labelEn: "Disruptive (Wood)" },
  { key: "conservative", labelZh: "价值稳健（Buffett · Munger · Graham）", labelEn: "Conservative (Buffett · Munger · Graham)" },
  { key: "garp",         labelZh: "成长合理价（Lynch · O'Neil）",         labelEn: "GARP (Lynch · O'Neil)" },
  { key: "macro",        labelZh: "宏观择时（Marks · Dalio · Druckenmiller）", labelEn: "Macro (Marks · Dalio · Druckenmiller)" },
];

// presetLabel — central lookup so the detail-modal header and any
// future surface (e.g. share cards) render the SAME human-friendly
// name as the filter dropdown. Falls back to the raw preset_key
// when the page is viewing a preset that's been removed from the
// PRESETS list but still has historical picks in the DB.
function presetLabel(key: string | undefined, lang: string): string {
  if (!key) return "";
  const hit = PRESETS.find((p) => p.key === key);
  if (!hit) return key;
  return lang === "zh-CN" ? hit.labelZh : hit.labelEn;
}

const MARKETS: Array<{ key: string; labelZh: string; labelEn: string }> = [
  { key: "us_equity", labelZh: "美股", labelEn: "US Equity" },
  // a_share / hk_equity intentionally hidden in v1 — the seed
  // watchlist is US only. Toggle in when zh pools land.
];

const VERDICT_ACCENT: Record<string, { ring: string; chip: string; row: string }> = {
  STRONG_BUY: { ring: "ring-emerald-300", chip: "bg-emerald-100 text-emerald-800", row: "bg-emerald-50/40" },
  BUY: { ring: "ring-emerald-200", chip: "bg-emerald-50 text-emerald-700", row: "bg-emerald-50/20" },
  HOLD: { ring: "ring-slate-200", chip: "bg-slate-100 text-slate-700", row: "" },
  MIXED: { ring: "ring-amber-200", chip: "bg-amber-100 text-amber-800", row: "bg-amber-50/40" },
  AVOID: { ring: "ring-rose-200", chip: "bg-rose-50 text-rose-700", row: "bg-rose-50/40" },
  SHORT: { ring: "ring-rose-300", chip: "bg-rose-100 text-rose-800", row: "bg-rose-50/40" },
  SKIP: { ring: "ring-slate-200", chip: "bg-slate-50 text-slate-500", row: "" },
};

function accentFor(verdict: AdvisorVerdict | string) {
  return VERDICT_ACCENT[String(verdict || "HOLD").toUpperCase()] ?? VERDICT_ACCENT.HOLD;
}

// Per-day verdict distribution surfaced in the accordion header.
// Bucketing rules:
//   * bullish  → STRONG_BUY + BUY
//   * neutral  → HOLD + MIXED + anything unrecognised
//   * bearish  → AVOID + SHORT + SKIP
// Three buckets keep the header readable; seven separate chips
// (one per enum value) make the row too noisy at typical card
// counts (~50 per preset per day).
function verdictDistribution(rows: DailyPickRow[]): {
  bullish: number;
  neutral: number;
  bearish: number;
} {
  let bullish = 0;
  let neutral = 0;
  let bearish = 0;
  for (const r of rows) {
    const v = String(r.aggregate_verdict || "").toUpperCase();
    if (v === "STRONG_BUY" || v === "BUY") bullish += 1;
    else if (v === "AVOID" || v === "SHORT" || v === "SKIP") bearish += 1;
    else neutral += 1;
  }
  return { bullish, neutral, bearish };
}

// "2026-06-09" → "Tue, 06-09" (locale-aware label used in the
// archive accordion header). Falls back to the ISO string on
// parse failure so a typo never blanks the header.
function formatDateLabel(iso: string, lang: string): string {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(iso);
  if (!m) return iso;
  const d = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]));
  const zh = lang === "zh-CN";
  const weekday = d.toLocaleDateString(zh ? "zh-CN" : "en-US", { weekday: "short" });
  return `${iso} · ${weekday}`;
}

function copyForLang(language: string) {
  const zh = language === "zh-CN";
  return {
    heading: zh ? "每日股票观察榜" : "Daily Stock Watch",
    sub: zh
      ? "公开发布的每日股票分析，由大师团队 AI 自动评议，所有读者看到相同内容。仅供研究参考，不构成任何投资建议。"
      : "Publicly distributed daily stock analysis scored by our master-persona AI panel. Identical content for all readers. Research only — not investment advice.",
    backCta: zh ? "返回主页" : "Back to home",
    advisorCta: zh ? "或自助查询单股" : "Or look up a single stock",
    pickedFor: zh ? "覆盖范围" : "Coverage",
    preset: zh ? "策略视角" : "Strategy lens",
    market: zh ? "市场" : "Market",
    date: zh ? "日期" : "Date",
    today: zh ? "今天" : "Today",
    loading: zh ? "加载中…" : "Loading…",
    empty: zh
      ? "该日期 / 策略 / 市场尚无榜单数据。可能 cron 还未运行，或这一组合本身未配置 watchlist。"
      : "No picks for this date / preset / market. The cron may not have run yet, or no watchlist is configured.",
    error: zh ? "加载失败：" : "Failed to load: ",
    tierBadge: (tier: string) => (zh ? `当前套餐：${tier}` : `Plan: ${tier}`),
    freeLagWarning: (days: number, newest?: string) =>
      zh
        ? `Free 套餐延迟 ${days} 天观看。${newest ? `最新公开日是 ${newest}，` : ""}升级 Pro 看实时榜单。`
        : `Free tier sees content with a ${days}-day delay.${newest ? ` Latest available: ${newest}.` : ""} Upgrade to Pro for real-time picks.`,
    upgradeToToday: zh ? "升级解锁今日榜单" : "Upgrade to see today's picks",
    headlineFallback: zh ? "（暂无观点摘要）" : "(no thesis summary)",
    score: zh ? "评分" : "Score",
    consensus: zh ? "共识度" : "Consensus",
    runByCron: zh ? "本榜单由系统每日美东收盘后自动生成" : "Generated nightly after US market close",
    detailQuota: (used: number, cap: number) =>
      cap < 0
        ? zh
          ? "无限查阅"
          : "Unlimited reads today"
        : zh
        ? `今日已查阅 ${used}/${cap} 只`
        : `Read ${used}/${cap} stocks today`,
    detailQuotaExceeded: zh
      ? "今日深度查阅已用完。明日 UTC 0:00 后重置，或升级套餐。"
      : "Detail quota exhausted for today. Resets at UTC 0:00 or upgrade your plan.",
    closeDetail: zh ? "关闭" : "Close",
    masterReports: zh ? "大师团队评议" : "Master Panel Reports",
    tacticReports: zh ? "战术建议" : "Tactic Reports",
    headlineThesisLabel: zh ? "首席观点" : "Lead Thesis",
    publisherNote: zh
      ? "本页内容为面向所有订阅者的公开发布，非个性化建议。同一只股票同一天所有读者看到的内容相同。"
      : "This page is a public newsletter; every subscriber sees identical content for the same (stock, date). Not personalised advice.",
  };
}

export default function DailyPicks() {
  const { language } = useAppPreferences();
  const compliance = useCompliance();
  // Derived once: pass the language-to-locale conversion through
  // formatModelVerdict consistently rather than re-deriving in
  // every call site. (compliance context exposes `mode` but not
  // `locale` — locale comes from the user-language preference.)
  const complianceLocale: "zh-CN" | "en-US" = language === "zh-CN" ? "zh-CN" : "en-US";
  const copy = useMemo(() => copyForLang(language), [language]);

  // ?preset=KEY support — the MastersHub deep-links into this
  // page with a strategy preset already chosen. We treat the URL
  // param as the initial value and (below) keep it in sync when
  // the user changes the dropdown so refreshes / link-shares stay
  // stable. Unknown / missing values fall back to PRESETS[0].
  const [searchParams, setSearchParams] = useSearchParams();
  const initialPreset = useMemo(() => {
    const fromUrl = searchParams.get("preset")?.trim() ?? "";
    return PRESETS.find((p) => p.key === fromUrl)?.key ?? PRESETS[0].key;
  // eslint-disable-next-line react-hooks/exhaustive-deps — initial only
  }, []);
  const [preset, setPreset] = useState<string>(initialPreset);
  const [market, setMarket] = useState<string>(MARKETS[0].key);
  // date === "" → "today" / newest-available; otherwise an
  // explicit yyyy-mm-dd the user picked from the date input.
  const [date, setDate] = useState<string>("");
  const [picks, setPicks] = useState<DailyPickRow[]>([]);
  const [tier, setTier] = useState<string>("free");
  const [freeLagDays, setFreeLagDays] = useState<number>(14);
  const [newestAvailable, setNewestAvailable] = useState<string | undefined>();
  const [newestForTier, setNewestForTier] = useState<string | undefined>();
  const [upgradeForToday, setUpgradeForToday] = useState<boolean>(false);
  const [listLoading, setListLoading] = useState<boolean>(false);
  const [listError, setListError] = useState<string | null>(null);

  const [detail, setDetail] = useState<DailyPickDetailResponse | null>(null);
  const [detailSymbol, setDetailSymbol] = useState<string | null>(null);
  const [detailLoading, setDetailLoading] = useState<boolean>(false);
  const [detailError, setDetailError] = useState<string | null>(null);

  // Date-archive accordion state. Each entry is a pick_date string
  // (yyyy-mm-dd); presence in the Set means "open". We DON'T persist
  // across mounts — collapsing is purely a per-session quality-of-
  // life nudge so users coming back to the page always see today's
  // picks expanded by default (see `openDates` derivation below).
  const [openDates, setOpenDates] = useState<Set<string>>(() => new Set());
  const toggleDate = useCallback((d: string) => {
    setOpenDates((prev) => {
      const next = new Set(prev);
      if (next.has(d)) next.delete(d);
      else next.add(d);
      return next;
    });
  }, []);

  // Group picks by pick_date. Output is date-descending so the
  // newest day renders at the top of the accordion. Stable sort
  // within a date is preserved from the API response (which sorts
  // by aggregate_score DESC server-side via idx_daily_picks_browse).
  const picksByDate = useMemo(() => {
    const map = new Map<string, DailyPickRow[]>();
    for (const r of picks) {
      const k = r.pick_date;
      const bucket = map.get(k);
      if (bucket) bucket.push(r);
      else map.set(k, [r]);
    }
    // Convert to a date-DESC sorted array so React keys are stable
    // and the iteration order matches the visual order.
    return Array.from(map.entries()).sort((a, b) => (a[0] < b[0] ? 1 : -1));
  }, [picks]);

  // Newest date is always expanded by default. We re-derive on
  // every picksByDate change so switching presets / markets resets
  // the open set to "only newest" — otherwise a stale "open" entry
  // from a previous preset could keep an empty old date expanded.
  useEffect(() => {
    if (picksByDate.length === 0) return;
    setOpenDates(new Set([picksByDate[0][0]]));
  }, [picksByDate]);

  // Mirror `preset` into the URL so the choice survives a refresh
  // and is shareable. Uses replace=true so we don't pollute the
  // browser history with one entry per dropdown change. Skipping
  // the write when the URL already has the right value avoids
  // a no-op re-render loop with the SearchParams object identity.
  useEffect(() => {
    const current = searchParams.get("preset") ?? "";
    if (current === preset) return;
    const next = new URLSearchParams(searchParams);
    next.set("preset", preset);
    setSearchParams(next, { replace: true });
  }, [preset, searchParams, setSearchParams]);

  // List-fetch effect. Re-runs on (preset, market, date) change.
  // We deliberately DON'T debounce — the user changes these via
  // discrete pickers, not free-form typing, so every change is
  // intentional.
  useEffect(() => {
    let cancelled = false;
    setListLoading(true);
    setListError(null);
    // When `date` is empty we want the multi-day archive view to
    // render below — fetch a generous slice (~10 days at 50
    // symbols/day per preset) so the client can group by pick_date.
    // When `date` is set the user has narrowed to one day, so a
    // single-day's worth is enough.
    listDailyPicks({
      market,
      preset,
      date: date || undefined,
      limit: date ? 100 : 500,
    })
      .then((res) => {
        if (cancelled) return;
        setPicks(res.picks ?? []);
        setTier(res.tier ?? "free");
        setFreeLagDays(res.free_lag_days ?? 14);
        setNewestAvailable(res.newest_available_date);
        setNewestForTier(res.newest_for_tier_date);
        setUpgradeForToday(Boolean(res.upgrade_required_for_today));
      })
      .catch((err) => {
        if (cancelled) return;
        setListError(formatApiError(err, copy.error));
      })
      .finally(() => {
        if (cancelled) return;
        setListLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [market, preset, date]);

  const handleOpenDetail = useCallback(
    (row: DailyPickRow) => {
      setDetail(null);
      setDetailSymbol(row.symbol);
      setDetailError(null);
      setDetailLoading(true);
      getDailyPickDetail(row.pick_date, row.symbol, { market: row.market, preset: row.preset_key })
        .then((res) => {
          setDetail(res);
        })
        .catch((err) => {
          setDetailError(formatApiError(err, copy.error));
        })
        .finally(() => {
          setDetailLoading(false);
        });
    },
    [copy.error],
  );

  const handleCloseDetail = useCallback(() => {
    setDetail(null);
    setDetailSymbol(null);
    setDetailError(null);
  }, []);

  const isFree = tier === "free";

  return (
    <div className="min-h-screen bg-slate-50 px-6 py-8">
      <ComplianceAckModal surface="daily_picks" />
      <div className="mx-auto max-w-6xl space-y-6">
        <header className="flex items-start justify-between gap-4">
          <div>
            <h1 className="text-2xl font-semibold tracking-tight text-slate-900">
              {copy.heading}
            </h1>
            <p className="mt-1 max-w-2xl text-sm text-slate-500">{copy.sub}</p>
            <p className="mt-2 text-[11px] text-slate-400">{copy.publisherNote}</p>
          </div>
          <div className="flex flex-col items-end gap-1">
            <Link
              to="/"
              className="rounded-full border border-slate-200 bg-white px-4 py-1.5 text-xs font-medium text-slate-600 hover:bg-slate-50"
            >
              {copy.backCta}
            </Link>
            <Link
              to="/advisor"
              className="text-[11px] text-indigo-600 hover:text-indigo-700 hover:underline"
            >
              {copy.advisorCta}
            </Link>
            <Link
              to="/trending/most-active"
              className="text-[11px] text-indigo-600 hover:text-indigo-700 hover:underline"
            >
              {language === "zh-CN" ? "看成交量排行榜" : "View Most Active by Volume"}
            </Link>
          </div>
        </header>

        <ComplianceBanner surface="daily_picks" />

        {/* Filters */}
        <section className="grid grid-cols-1 gap-3 rounded-2xl bg-white p-5 shadow-sm sm:grid-cols-4">
          <label className="block">
            <span className="mb-1 block text-xs font-medium text-slate-600">{copy.preset}</span>
            <select
              value={preset}
              onChange={(e) => setPreset(e.target.value)}
              className="w-full rounded-md border border-slate-300 px-3 py-2 text-sm focus:border-indigo-400 focus:outline-none focus:ring-1 focus:ring-indigo-400"
            >
              {PRESETS.map((p) => (
                <option key={p.key} value={p.key}>
                  {language === "zh-CN" ? p.labelZh : p.labelEn}
                </option>
              ))}
            </select>
          </label>
          <label className="block">
            <span className="mb-1 block text-xs font-medium text-slate-600">{copy.market}</span>
            <select
              value={market}
              onChange={(e) => setMarket(e.target.value)}
              className="w-full rounded-md border border-slate-300 px-3 py-2 text-sm focus:border-indigo-400 focus:outline-none focus:ring-1 focus:ring-indigo-400"
            >
              {MARKETS.map((m) => (
                <option key={m.key} value={m.key}>
                  {language === "zh-CN" ? m.labelZh : m.labelEn}
                </option>
              ))}
            </select>
          </label>
          <label className="block sm:col-span-2">
            <span className="mb-1 block text-xs font-medium text-slate-600">
              {copy.date}{" "}
              <button
                type="button"
                onClick={() => setDate("")}
                className="ml-2 rounded-full border border-slate-200 px-2 py-0.5 text-[10px] font-medium text-slate-500 hover:bg-slate-50"
                disabled={!date}
              >
                {copy.today}
              </button>
            </span>
            <input
              type="date"
              value={date}
              onChange={(e) => setDate(e.target.value)}
              max={newestForTier || newestAvailable}
              className="w-full rounded-md border border-slate-300 px-3 py-2 text-sm focus:border-indigo-400 focus:outline-none focus:ring-1 focus:ring-indigo-400"
            />
          </label>
        </section>

        {/* Tier strip */}
        <div className="flex flex-wrap items-center justify-between gap-2 rounded-xl border border-slate-200 bg-white px-4 py-2 text-xs text-slate-500">
          <span>
            <span className="mr-2 rounded-full bg-slate-100 px-2 py-0.5 font-medium text-slate-700">
              {copy.tierBadge(tier)}
            </span>
            {copy.runByCron}
          </span>
          {isFree ? (
            <span className="text-amber-700">{copy.freeLagWarning(freeLagDays, newestAvailable)}</span>
          ) : null}
        </div>

        {/* Upgrade overlay for free users when today exists */}
        {upgradeForToday ? (
          <div className="rounded-2xl border-2 border-dashed border-indigo-200 bg-gradient-to-br from-indigo-50 to-white p-6 text-center">
            <div className="mx-auto max-w-md">
              <h3 className="text-lg font-semibold text-indigo-900">
                {copy.upgradeToToday}
              </h3>
              <p className="mt-2 text-sm text-indigo-700">
                {copy.freeLagWarning(freeLagDays, newestAvailable)}
              </p>
              <Link
                to="/wallet"
                className="mt-4 inline-block rounded-full bg-indigo-600 px-6 py-2 text-sm font-medium text-white shadow-sm hover:bg-indigo-700"
              >
                {language === "zh-CN" ? "查看升级方案" : "View Pro plans"}
              </Link>
            </div>
          </div>
        ) : null}

        {/* List */}
        <section className="rounded-2xl bg-white shadow-sm">
          {listLoading ? (
            <div className="px-5 py-10 text-center text-sm text-slate-500">{copy.loading}</div>
          ) : listError ? (
            <div className="px-5 py-6 text-sm text-rose-700">
              {copy.error}
              {listError}
            </div>
          ) : picks.length === 0 ? (
            <div className="px-5 py-10 text-center text-sm text-slate-500">{copy.empty}</div>
          ) : (
            // Archive accordion — one section per pick_date. The
            // outer `divide-y` separates dates; each date contains
            // its own `divide-y` for symbols. Newest date is open
            // by default (see openDates effect above).
            <div className="divide-y divide-slate-200">
              {picksByDate.map(([pickDate, rows]) => {
                const isOpen = openDates.has(pickDate);
                const dist = verdictDistribution(rows);
                const isToday = pickDate === (newestForTier || newestAvailable);
                return (
                  <section key={pickDate}>
                    <button
                      type="button"
                      onClick={() => toggleDate(pickDate)}
                      className="flex w-full items-center gap-3 bg-slate-50/60 px-5 py-3 text-left transition hover:bg-slate-100/80"
                      aria-expanded={isOpen}
                    >
                      <span className="text-xs font-mono text-slate-400" aria-hidden="true">
                        {isOpen ? "▾" : "▸"}
                      </span>
                      <span className="text-sm font-semibold text-slate-800">
                        {formatDateLabel(pickDate, language)}
                      </span>
                      {isToday ? (
                        <span className="rounded-full bg-indigo-100 px-2 py-0.5 text-[10px] font-medium text-indigo-700">
                          {language === "zh-CN" ? "最新" : "Latest"}
                        </span>
                      ) : null}
                      <span className="text-[11px] text-slate-500">
                        {language === "zh-CN" ? `${rows.length} 只` : `${rows.length} stocks`}
                      </span>
                      <span className="ml-auto flex items-center gap-1.5">
                        {dist.bullish > 0 ? (
                          <span className="rounded-full bg-emerald-50 px-2 py-0.5 text-[10px] font-medium text-emerald-700">
                            {language === "zh-CN" ? "看多" : "Bullish"} {dist.bullish}
                          </span>
                        ) : null}
                        {dist.neutral > 0 ? (
                          <span className="rounded-full bg-slate-100 px-2 py-0.5 text-[10px] font-medium text-slate-700">
                            {language === "zh-CN" ? "中性" : "Neutral"} {dist.neutral}
                          </span>
                        ) : null}
                        {dist.bearish > 0 ? (
                          <span className="rounded-full bg-rose-50 px-2 py-0.5 text-[10px] font-medium text-rose-700">
                            {language === "zh-CN" ? "看空" : "Bearish"} {dist.bearish}
                          </span>
                        ) : null}
                      </span>
                    </button>
                    {isOpen ? (
                      <div className="divide-y divide-slate-100">
                        {rows.map((row) => {
                          const accent = accentFor(row.aggregate_verdict);
                          return (
                            <button
                              type="button"
                              key={`${row.symbol}-${row.pick_date}`}
                              onClick={() => handleOpenDetail(row)}
                              className={`group block w-full px-5 py-4 text-left transition hover:bg-indigo-50/30 ${accent.row}`}
                            >
                              <div className="flex flex-wrap items-baseline gap-3">
                                <span className="text-base font-semibold text-slate-900">
                                  {row.symbol_name ? `${row.symbol_name} (${row.symbol})` : row.symbol}
                                </span>
                                <span className={`rounded-full px-2.5 py-0.5 text-xs font-semibold ${accent.chip}`}>
                                  {formatModelVerdict(
                                    row.aggregate_verdict,
                                    complianceLocale,
                                    compliance.mode,
                                  )}
                                </span>
                                <span className="text-[11px] text-slate-500">
                                  {copy.score}: {row.aggregate_score}
                                </span>
                                <span className="text-[11px] text-slate-500">
                                  {copy.consensus}: {Math.round((row.consensus ?? 0) * 100)}%
                                </span>
                              </div>
                              {row.headline_thesis ? (
                                <p className="mt-1 text-sm text-slate-600 line-clamp-2">
                                  <span className="mr-1 text-[10px] uppercase tracking-wider text-slate-400">
                                    {copy.headlineThesisLabel}:
                                  </span>
                                  {row.headline_thesis}
                                </p>
                              ) : (
                                <p className="mt-1 text-xs italic text-slate-400">{copy.headlineFallback}</p>
                              )}
                            </button>
                          );
                        })}
                      </div>
                    ) : null}
                  </section>
                );
              })}
            </div>
          )}
        </section>
      </div>

      {/* Detail modal */}
      {detailSymbol !== null ? (
        <div
          className="fixed inset-0 z-40 flex items-start justify-center overflow-y-auto bg-slate-900/50 p-4 sm:p-8"
          onClick={handleCloseDetail}
        >
          <div
            className="w-full max-w-4xl rounded-2xl bg-white p-6 shadow-2xl"
            onClick={(e) => e.stopPropagation()}
          >
            <header className="mb-4 flex items-start justify-between gap-3">
              <div>
                <h2 className="text-xl font-semibold text-slate-900">
                  {detail?.symbol_name
                    ? `${detail.symbol_name} (${detail.symbol})`
                    : detailSymbol}
                </h2>
                <p className="mt-0.5 text-xs text-slate-400">
                  {detail?.pick_date}
                  {detail?.preset_key
                    ? ` · ${presetLabel(detail.preset_key, language)}`
                    : null}
                </p>
              </div>
              <div className="flex flex-col items-end gap-2">
                {detail ? (
                  <span className="rounded-full bg-slate-100 px-3 py-0.5 text-[11px] text-slate-600">
                    {copy.detailQuota(detail.quota_used_today, detail.quota_cap_today)}
                  </span>
                ) : null}
                <button
                  type="button"
                  onClick={handleCloseDetail}
                  className="rounded-full border border-slate-200 bg-white px-3 py-1 text-xs font-medium text-slate-600 hover:bg-slate-50"
                >
                  {copy.closeDetail}
                </button>
              </div>
            </header>

            {detailLoading ? (
              <div className="py-10 text-center text-sm text-slate-500">{copy.loading}</div>
            ) : detailError ? (
              <div className="rounded-md bg-rose-50 px-3 py-2 text-sm text-rose-700">
                {detailError.includes("quota") ? copy.detailQuotaExceeded : `${copy.error}${detailError}`}
              </div>
            ) : detail?.pick ? (
              <div className="space-y-4">
                {/* Aggregate header — same accent treatment as
                    Advisor.tsx so users immediately recognise the
                    verdict pill shape. */}
                <div className={`rounded-xl ring-2 ${accentFor(detail.pick.aggregate_verdict).ring} bg-slate-50/40 px-4 py-3`}>
                  <div className="flex flex-wrap items-baseline gap-3">
                    <span
                      className={`rounded-full px-3 py-1 text-sm font-semibold ${
                        accentFor(detail.pick.aggregate_verdict).chip
                      }`}
                    >
                      {formatModelVerdict(
                        detail.pick.aggregate_verdict,
                        complianceLocale,
                        compliance.mode,
                      )}
                    </span>
                    <span className="text-xs text-slate-500">
                      {copy.consensus}: {Math.round((detail.pick.consensus_score ?? 0) * 100)}%
                    </span>
                    <span className="text-xs text-slate-500">
                      Confidence: {detail.pick.aggregate_confidence}%
                    </span>
                  </div>
                </div>

                {/* Technical snapshot — rendered ABOVE the master
                    reports so readers see the factual market data
                    the panel reasoned over before reading the
                    persona theses. Compliance-first ordering: data
                    → analysis, never analysis → data. */}
                {detail.pick.technical ? (
                  <TechnicalSnapshotCard technical={detail.pick.technical} language={language} />
                ) : null}

                {detail.pick.master_reports?.length ? (
                  <div>
                    <h3 className="mb-2 text-sm font-semibold text-slate-800">
                      {copy.masterReports}
                    </h3>
                    <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
                      {detail.pick.master_reports.map((m) => (
                        <MasterVerdictCard key={m.master_key} report={m} language={language} />
                      ))}
                    </div>
                  </div>
                ) : null}

                {detail.pick.tactic_reports?.length ? (
                  <div>
                    <h3 className="mb-2 text-sm font-semibold text-slate-800">
                      {copy.tacticReports}
                    </h3>
                    <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
                      {detail.pick.tactic_reports.map((t) => (
                        <TacticVerdictCard key={t.tactic_key} report={t} language={language} />
                      ))}
                    </div>
                  </div>
                ) : null}
              </div>
            ) : null}
          </div>
        </div>
      ) : null}
    </div>
  );
}
