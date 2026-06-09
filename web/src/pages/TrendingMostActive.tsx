import React, { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import {
  formatApiError,
  getTrendingMostActive,
  type TrendingMostActiveResponse,
  type TrendingMostActiveRow,
} from "../lib/api";
import { useAppPreferences } from "../lib/preferences";
import { ComplianceBanner } from "../components/ComplianceBanner";
import { ComplianceAckModal } from "../components/ComplianceAckModal";

// TrendingMostActive — the /trending/most-active page.
//
// Compliance posture (mirrors the framing the AI's plan
// recommended and Yahoo Finance / TradingView's Most Active
// pages use in production):
//
//   * The page name is "Most Active by Volume" — an OBJECTIVE,
//     verifiable market observation, not a "we picked these"
//     recommendation.
//   * The ranking criteria are spelled out in the SUBHEADER
//     (criteria_disclosed pulled from the API response). This
//     is what makes the list "algorithmic output", not "stock
//     pick".
//   * Every cell is raw market data — closing price, day-over-day
//     percent change, volume, vol/20D ratio. Zero "should-buy" /
//     "looks-strong" prose.
//   * Footer disclaimer reiterates "factual market data, not
//     personalised advice".
//
// What's INTENTIONALLY MISSING and SHOULD NOT BE ADDED later
// without compliance review:
//
//   * "Top picks" / "Featured" / "Editor's choice" badges.
//   * A subjective ranking signal ("hot 🔥", "trending up ↑").
//   * Per-symbol prose that interprets the ranking.
//   * A "buy this now" CTA pointing to an order screen.
//
// The page deliberately reuses the existing ComplianceBanner /
// ComplianceAckModal under the `daily_picks` surface — the
// disclosure language is the same product family ("publisher-mode
// algorithmic content"), so adding a second surface key would
// double-prompt the user without compliance benefit.

const MARKETS: Array<{ key: string; labelZh: string; labelEn: string }> = [
  { key: "us_equity", labelZh: "美股", labelEn: "US Equity" },
];

function copyForLang(language: string) {
  const zh = language === "zh-CN";
  return {
    heading: zh ? "今日成交量排行（客观市场榜单）" : "Today's Most Active Stocks",
    sub: zh
      ? "按当日成交量 / 20 日均量比从高到低排序。基于公开行情数据的算法输出，不构成任何投资建议。"
      : "Ranked by latest-bar volume divided by 20-bar SMA volume. Algorithmic output from public market data — not investment advice.",
    market: zh ? "市场" : "Market",
    universe: zh ? "样本范围" : "Universe",
    asOf: zh ? "数据截止" : "As of",
    rank: zh ? "排名" : "Rank",
    symbol: zh ? "代码" : "Symbol",
    close: zh ? "收盘" : "Close",
    chg: zh ? "日涨跌" : "Day Δ",
    vol: zh ? "成交量" : "Volume",
    ratio: zh ? "量比 (vs 20D)" : "Vol vs 20D",
    loading: zh ? "加载中…" : "Loading…",
    empty: zh ? "该市场暂无可排序数据。请稍后再试。" : "No data available for this market yet.",
    error: zh ? "加载失败：" : "Failed to load: ",
    criteriaHeader: zh ? "排序规则（公开披露）" : "Ranking criteria (publicly disclosed)",
    backCta: zh ? "返回主页" : "Back to home",
    dailyPicksCta: zh ? "查看大师团队 AI 评议" : "View AI master-panel analyses",
    moreLists: zh ? "更多观察榜（即将上线）" : "More lists (coming soon)",
    comingSoon: zh
      ? "Momentum Screen / Disruptive Screen 即将上线"
      : "Momentum Screen / Disruptive Screen coming soon",
  };
}

function fmt(n: number | undefined, decimals = 2): string {
  if (n === undefined || n === null || !isFinite(n)) return "—";
  return n.toLocaleString("en-US", {
    minimumFractionDigits: decimals,
    maximumFractionDigits: decimals,
  });
}

function fmtPct(n: number | undefined): string {
  if (n === undefined || n === null || !isFinite(n)) return "—";
  const v = n * 100;
  return (v >= 0 ? "+" : "") + v.toFixed(2) + "%";
}

function fmtBigInt(n: number | undefined): string {
  if (n === undefined || n === null || !isFinite(n)) return "—";
  if (Math.abs(n) >= 1e9) return (n / 1e9).toFixed(2) + "B";
  if (Math.abs(n) >= 1e6) return (n / 1e6).toFixed(2) + "M";
  if (Math.abs(n) >= 1e3) return (n / 1e3).toFixed(1) + "K";
  return n.toFixed(0);
}

function chgColour(pct: number | undefined): string {
  if (pct === undefined || pct === null || !isFinite(pct) || pct === 0) return "text-slate-600";
  return pct > 0 ? "text-emerald-700" : "text-rose-700";
}

export default function TrendingMostActive() {
  const { language } = useAppPreferences();
  const copy = useMemo(() => copyForLang(language), [language]);

  const [market, setMarket] = useState<string>(MARKETS[0].key);
  const [data, setData] = useState<TrendingMostActiveResponse | null>(null);
  const [loading, setLoading] = useState<boolean>(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    getTrendingMostActive({ market, limit: 50 })
      .then((res) => {
        if (cancelled) return;
        setData(res);
      })
      .catch((err) => {
        if (cancelled) return;
        setError(formatApiError(err, copy.error));
      })
      .finally(() => {
        if (cancelled) return;
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [market]);

  return (
    <div className="min-h-screen bg-slate-50 px-6 py-8">
      <ComplianceAckModal surface="daily_picks" />
      <div className="mx-auto max-w-5xl space-y-5">
        <header className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <h1 className="text-2xl font-semibold tracking-tight text-slate-900">
              {copy.heading}
            </h1>
            <p className="mt-1 max-w-2xl text-sm text-slate-500">{copy.sub}</p>
          </div>
          <div className="flex flex-col items-end gap-1">
            <Link
              to="/"
              className="rounded-full border border-slate-200 bg-white px-4 py-1.5 text-xs font-medium text-slate-600 hover:bg-slate-50"
            >
              {copy.backCta}
            </Link>
            <Link
              to="/daily-picks"
              className="text-[11px] text-indigo-600 hover:text-indigo-700 hover:underline"
            >
              {copy.dailyPicksCta}
            </Link>
          </div>
        </header>

        <ComplianceBanner surface="daily_picks" />

        {/* Filters */}
        <section className="grid grid-cols-1 gap-3 rounded-2xl bg-white p-5 shadow-sm sm:grid-cols-3">
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
          {data ? (
            <>
              <div className="flex flex-col justify-end">
                <div className="text-[10px] uppercase tracking-wider text-slate-400">{copy.universe}</div>
                <div className="text-sm font-medium text-slate-800">
                  {data.universe_size} {language === "zh-CN" ? "只" : "symbols"}
                </div>
              </div>
              <div className="flex flex-col justify-end">
                <div className="text-[10px] uppercase tracking-wider text-slate-400">{copy.asOf}</div>
                <div className="text-sm font-medium text-slate-800">
                  {data.generated_at ? new Date(data.generated_at).toLocaleString() : "—"}
                </div>
              </div>
            </>
          ) : null}
        </section>

        {/* Criteria disclosure — the SEC-friendly proof-of-criteria */}
        {data && data.criteria_disclosed?.length > 0 ? (
          <details className="rounded-xl border border-slate-200 bg-white px-4 py-3 text-xs text-slate-600">
            <summary className="cursor-pointer font-medium text-slate-700">
              {copy.criteriaHeader}
            </summary>
            <ul className="mt-2 space-y-1 pl-4">
              {data.criteria_disclosed.map((c, i) => (
                <li key={i} className="list-disc">{c}</li>
              ))}
            </ul>
          </details>
        ) : null}

        {/* Table */}
        <section className="overflow-hidden rounded-2xl bg-white shadow-sm">
          {loading ? (
            <div className="px-5 py-10 text-center text-sm text-slate-500">{copy.loading}</div>
          ) : error ? (
            <div className="px-5 py-6 text-sm text-rose-700">
              {copy.error}
              {error}
            </div>
          ) : !data || data.results.length === 0 ? (
            <div className="px-5 py-10 text-center text-sm text-slate-500">{copy.empty}</div>
          ) : (
            <table className="w-full text-sm">
              <thead className="bg-slate-50 text-[11px] uppercase tracking-wider text-slate-500">
                <tr>
                  <th className="px-4 py-2 text-left">{copy.rank}</th>
                  <th className="px-4 py-2 text-left">{copy.symbol}</th>
                  <th className="px-4 py-2 text-right">{copy.close}</th>
                  <th className="px-4 py-2 text-right">{copy.chg}</th>
                  <th className="px-4 py-2 text-right">{copy.vol}</th>
                  <th className="px-4 py-2 text-right">{copy.ratio}</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-100">
                {data.results.map((row: TrendingMostActiveRow) => (
                  <tr key={row.symbol} className="hover:bg-indigo-50/30">
                    <td className="px-4 py-2.5 text-sm text-slate-500">{row.rank}</td>
                    <td className="px-4 py-2.5">
                      <div className="font-medium text-slate-900">
                        {row.symbol_name ? `${row.symbol_name} (${row.symbol})` : row.symbol}
                      </div>
                    </td>
                    <td className="px-4 py-2.5 text-right text-slate-800">{fmt(row.last_close)}</td>
                    <td className={`px-4 py-2.5 text-right font-medium ${chgColour(row.pct_change_1d)}`}>
                      {fmtPct(row.pct_change_1d)}
                    </td>
                    <td className="px-4 py-2.5 text-right text-slate-700">{fmtBigInt(row.volume)}</td>
                    <td className="px-4 py-2.5 text-right font-medium text-slate-900">
                      {fmt(row.vol_20d_ratio, 2)}x
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </section>

        {/* Disclaimer */}
        {data?.disclaimer ? (
          <p className="px-2 text-[11px] leading-relaxed text-slate-400">{data.disclaimer}</p>
        ) : null}

        {/* "More lists coming soon" placeholder — keeps the
            information architecture honest: we'll add Momentum
            Screen / Disruptive Screen here, not into a hidden
            settings menu. */}
        <section className="rounded-xl border border-dashed border-slate-200 bg-white/50 px-4 py-3 text-xs text-slate-500">
          <span className="mr-2 font-medium text-slate-600">{copy.moreLists}</span>
          {copy.comingSoon}
        </section>
      </div>
    </div>
  );
}
