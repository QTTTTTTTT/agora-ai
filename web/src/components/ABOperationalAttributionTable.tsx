import React, { useEffect, useMemo, useState } from "react";

import {
  fetchABOperationalAttribution,
  formatApiError,
  type ABAttributionSymbolRow,
  type ABAttributionTotals,
  type ABTestOperationalAttribution,
} from "../lib/api";

interface ABOperationalAttributionTableProps {
  testId: string;
  /** Whether the parent test has reached `analyzed` status. When
   *  false the panel renders a "run analyze first" hint and skips
   *  the network call. */
  analyzed: boolean;
  language?: "zh-CN" | "en-US";
  defaultOpen?: boolean;
}

const COPY = {
  "zh-CN": {
    sectionTitle: "按标的归因",
    sectionSubtitle: "比较 A vs B 在每只标的上的成交、成本与盈亏差异；按 |差额| 降序排序",
    expand: "展开",
    collapse: "收起",
    loading: "加载归因数据…",
    error: "加载失败",
    retry: "重试",
    empty: "该测试暂无影子交易归因数据",
    notAnalyzedYet: "完成「生成分析」后即可查看 A vs B 的标的级归因",
    columnSymbol: "标的",
    columnTradesA: "A 笔数",
    columnTradesB: "B 笔数",
    columnPnLA: "A 盈亏",
    columnPnLB: "B 盈亏",
    columnTurnoverA: "A 成交额",
    columnTurnoverB: "B 成交额",
    columnGap: "差额（B−A）",
    columnGapPct: "差额占比",
    columnWinner: "胜出",
    winnerA: "A",
    winnerB: "B",
    winnerTie: "持平",
    totalsTitle: "总览",
    tradeCount: "笔数",
    turnover: "成交额",
    realizedPnL: "已实现盈亏",
    avgPnL: "平均盈亏",
    winRate: "盈利交易占比",
  },
  "en-US": {
    sectionTitle: "Per-symbol attribution",
    sectionSubtitle: "Compare A vs B trade count, turnover, and realized P&L per symbol; sorted by |gap| desc.",
    expand: "Show",
    collapse: "Hide",
    loading: "Loading attribution\u2026",
    error: "Failed to load",
    retry: "Retry",
    empty: "No shadow trade attribution for this test yet",
    notAnalyzedYet: "Run \u201cGenerate analysis\u201d first to see per-symbol A vs B attribution.",
    columnSymbol: "Symbol",
    columnTradesA: "A trades",
    columnTradesB: "B trades",
    columnPnLA: "A P&L",
    columnPnLB: "B P&L",
    columnTurnoverA: "A turnover",
    columnTurnoverB: "B turnover",
    columnGap: "Gap (B \u2212 A)",
    columnGapPct: "Gap %",
    columnWinner: "Winner",
    winnerA: "A",
    winnerB: "B",
    winnerTie: "Tie",
    totalsTitle: "Totals",
    tradeCount: "Trades",
    turnover: "Turnover",
    realizedPnL: "Realized P&L",
    avgPnL: "Avg P&L",
    winRate: "Winning trade rate",
  },
};

function formatNum(n: number, fractionDigits = 2): string {
  if (!Number.isFinite(n)) return "—";
  return n.toLocaleString(undefined, {
    minimumFractionDigits: fractionDigits,
    maximumFractionDigits: fractionDigits,
  });
}

function formatPercent(n: number, fractionDigits = 2): string {
  if (!Number.isFinite(n)) return "—";
  return `${n.toFixed(fractionDigits)}%`;
}

function pnlClass(n: number): string {
  if (!Number.isFinite(n) || n === 0) return "text-gray-700";
  return n > 0 ? "text-emerald-700" : "text-red-700";
}

function winnerBadgeClass(winner: string): string {
  switch (winner) {
    case "A":
      return "bg-blue-50 text-blue-700 ring-1 ring-blue-100";
    case "B":
      return "bg-orange-50 text-orange-700 ring-1 ring-orange-100";
    default:
      return "bg-gray-100 text-gray-600 ring-1 ring-gray-200";
  }
}

const TotalsRow: React.FC<{
  label: string;
  totals: ABAttributionTotals;
  copy: (typeof COPY)["zh-CN"];
  accentClass: string;
}> = ({ label, totals, copy, accentClass }) => (
  <div className={`rounded-xl border ${accentClass} bg-white p-4`}>
    <p className="text-sm font-semibold text-gray-800">{label}</p>
    <dl className="mt-2 grid grid-cols-2 gap-x-3 gap-y-1 text-xs text-gray-600">
      <dt className="text-gray-500">{copy.tradeCount}</dt>
      <dd className="text-right font-mono text-gray-900">{formatNum(totals.tradeCount, 0)}</dd>
      <dt className="text-gray-500">{copy.turnover}</dt>
      <dd className="text-right font-mono text-gray-900">{formatNum(totals.turnover, 2)}</dd>
      <dt className="text-gray-500">{copy.realizedPnL}</dt>
      <dd className={`text-right font-mono ${pnlClass(totals.realizedPnL)}`}>
        {formatNum(totals.realizedPnL, 2)}
      </dd>
      <dt className="text-gray-500">{copy.avgPnL}</dt>
      <dd className={`text-right font-mono ${pnlClass(totals.avgPnL)}`}>
        {formatNum(totals.avgPnL, 2)}
      </dd>
      <dt className="text-gray-500">{copy.winRate}</dt>
      <dd className="text-right font-mono text-gray-900">
        {formatPercent(totals.winTradeRate * 100, 1)}
      </dd>
    </dl>
  </div>
);

/**
 * ABOperationalAttributionTable renders a collapsible per-symbol
 * comparison table sourced from
 * `GET /api/abtests/:testId/operational-attribution`.
 *
 * Design choices:
 *   - Defaults to collapsed; lazy-fetches on first expand.
 *   - Renders TWO totals cards (A, B) above the per-symbol rows so
 *     the at-a-glance comparison happens before the user has to
 *     read 50 rows.
 *   - Uses tabular-nums + monospace for numbers so columns align
 *     cleanly even when one variant has no trades.
 */
export const ABOperationalAttributionTable: React.FC<ABOperationalAttributionTableProps> = ({
  testId,
  analyzed,
  language = "zh-CN",
  defaultOpen = false,
}) => {
  const copy = useMemo(() => COPY[language], [language]);
  const [open, setOpen] = useState(defaultOpen);
  const [data, setData] = useState<ABTestOperationalAttribution | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!open || data !== null || loading || !analyzed) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    fetchABOperationalAttribution(testId)
      .then((resp) => {
        if (!cancelled) setData(resp);
      })
      .catch((err) => {
        if (!cancelled) setError(formatApiError(err, copy.error));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [open, testId, data, loading, analyzed, copy.error]);

  useEffect(() => {
    setData(null);
    setError(null);
  }, [testId]);

  const retry = () => {
    setData(null);
    setError(null);
  };

  const renderRow = (row: ABAttributionSymbolRow, idx: number) => {
    const winnerLabel =
      row.winner === "A" ? copy.winnerA : row.winner === "B" ? copy.winnerB : copy.winnerTie;
    return (
      <tr
        key={`${row.symbol}-${idx}`}
        className="border-b border-gray-50 transition-colors last:border-0 hover:bg-gray-50/60"
      >
        <td className="py-2 pr-4 font-mono text-xs text-gray-900">{row.symbol}</td>
        <td className="py-2 pr-4 text-right tabular-nums text-gray-700">{formatNum(row.tradeCountA, 0)}</td>
        <td className="py-2 pr-4 text-right tabular-nums text-gray-700">{formatNum(row.tradeCountB, 0)}</td>
        <td className={`py-2 pr-4 text-right tabular-nums ${pnlClass(row.realizedPnLA)}`}>
          {formatNum(row.realizedPnLA, 2)}
        </td>
        <td className={`py-2 pr-4 text-right tabular-nums ${pnlClass(row.realizedPnLB)}`}>
          {formatNum(row.realizedPnLB, 2)}
        </td>
        <td className="py-2 pr-4 text-right tabular-nums text-gray-500">{formatNum(row.turnoverA, 2)}</td>
        <td className="py-2 pr-4 text-right tabular-nums text-gray-500">{formatNum(row.turnoverB, 2)}</td>
        <td className={`py-2 pr-4 text-right tabular-nums font-medium ${pnlClass(row.pnlGap)}`}>
          {row.pnlGap > 0 ? "+" : ""}
          {formatNum(row.pnlGap, 2)}
        </td>
        <td className="py-2 pr-4 text-right tabular-nums text-gray-600">
          {formatPercent(row.gapPctOfNotional, 2)}
        </td>
        <td className="py-2 text-center">
          <span
            className={`inline-flex items-center rounded-full px-2.5 py-0.5 text-xs font-medium ${winnerBadgeClass(row.winner)}`}
          >
            {winnerLabel}
          </span>
        </td>
      </tr>
    );
  };

  return (
    <section className="rounded-2xl border border-gray-200 bg-white shadow-sm">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-start justify-between gap-4 px-6 py-4 text-left"
        aria-expanded={open}
      >
        <div>
          <h3 className="text-base font-semibold text-gray-900">{copy.sectionTitle}</h3>
          <p className="mt-1 text-xs text-gray-500">{copy.sectionSubtitle}</p>
        </div>
        <span className="shrink-0 rounded-full border border-gray-200 px-3 py-1 text-xs font-medium text-gray-600">
          {open ? copy.collapse : copy.expand}
        </span>
      </button>

      {open ? (
        <div className="border-t border-gray-100 px-6 py-4">
          {!analyzed ? (
            <div className="rounded-lg border border-amber-100 bg-amber-50 px-4 py-3 text-sm text-amber-800">
              {copy.notAnalyzedYet}
            </div>
          ) : loading ? (
            <p className="text-sm text-gray-500">{copy.loading}</p>
          ) : error ? (
            <div className="flex items-center gap-3 text-sm">
              <span className="text-red-600">{copy.error}: {error}</span>
              <button
                type="button"
                onClick={retry}
                className="rounded-md border border-red-200 bg-red-50 px-2 py-1 text-xs font-medium text-red-700 hover:bg-red-100"
              >
                {copy.retry}
              </button>
            </div>
          ) : !data || data.bySymbol.length === 0 ? (
            <p className="text-sm text-gray-500">{copy.empty}</p>
          ) : (
            <div className="space-y-4">
              <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
                <TotalsRow label="A" totals={data.totalA} copy={copy} accentClass="border-blue-100" />
                <TotalsRow label="B" totals={data.totalB} copy={copy} accentClass="border-orange-100" />
              </div>
              <div className="overflow-x-auto">
                <table className="w-full text-sm">
                  <thead className="text-left text-xs text-gray-500">
                    <tr className="border-b border-gray-100">
                      <th className="pb-2 pr-4">{copy.columnSymbol}</th>
                      <th className="pb-2 pr-4 text-right">{copy.columnTradesA}</th>
                      <th className="pb-2 pr-4 text-right">{copy.columnTradesB}</th>
                      <th className="pb-2 pr-4 text-right">{copy.columnPnLA}</th>
                      <th className="pb-2 pr-4 text-right">{copy.columnPnLB}</th>
                      <th className="pb-2 pr-4 text-right">{copy.columnTurnoverA}</th>
                      <th className="pb-2 pr-4 text-right">{copy.columnTurnoverB}</th>
                      <th className="pb-2 pr-4 text-right">{copy.columnGap}</th>
                      <th className="pb-2 pr-4 text-right">{copy.columnGapPct}</th>
                      <th className="pb-2 text-center">{copy.columnWinner}</th>
                    </tr>
                  </thead>
                  <tbody>{data.bySymbol.map(renderRow)}</tbody>
                </table>
              </div>
            </div>
          )}
        </div>
      ) : null}
    </section>
  );
};

export default ABOperationalAttributionTable;
