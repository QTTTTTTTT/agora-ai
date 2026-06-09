// AdvisorTrackRecordPanel — Phase 5 public track-record surface.
//
// Renders the rolling hit-rate / decision-count / avg-alpha for
// every master and tactic agent that has accumulated outcomes in
// agent_reputation_outcomes (advisor rows only, fund_id IS NULL).
//
// Placement:
//   * Welcome page header — gives a "67% hit rate" pitch to the
//     undecided user before they pick a mode.
//   * Advisor page header — keeps it visible after the user enters
//     a consultation so they can sanity-check the masters they're
//     about to consult.
//
// Degraded states:
//   * No data yet (decisions_count == 0 across the board) → the
//     panel renders a friendly "track record will appear after N
//     consultations age out the 1-day horizon".
//   * Backend 5xx / network error → the parent silently swallows;
//     this component just renders empty.

import React, { useEffect, useMemo, useState } from "react";
import { fetchAdvisorTrackRecord, type AdvisorTrackRecordRow } from "../lib/api";
import { useAppPreferences } from "../lib/preferences";

interface Props {
  // When true, the panel is rendered as a compact summary strip
  // (used in the Advisor page header). Otherwise full table layout.
  compact?: boolean;
  // Max rows to show per side (masters / tactics). Defaults to 6
  // in compact mode and 20 in full mode.
  limit?: number;
}

const AdvisorTrackRecordPanel: React.FC<Props> = ({ compact = false, limit }) => {
  const { language } = useAppPreferences();
  const en = language === "en-US";
  const [data, setData] = useState<{
    masters: AdvisorTrackRecordRow[];
    tactics: AdvisorTrackRecordRow[];
  } | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    fetchAdvisorTrackRecord(limit ?? (compact ? 6 : 20))
      .then((resp) => {
        if (cancelled) return;
        setData({
          masters: resp.masters ?? [],
          tactics: resp.tactics ?? [],
        });
      })
      .catch(() => {
        if (!cancelled) setData({ masters: [], tactics: [] });
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [compact, limit]);

  const copy = useMemo(
    () =>
      en
        ? {
            heading: "Advisor track record",
            sub: "Rolling hit rate and average alpha for each master / tactic agent. Updated daily.",
            mastersTab: "Masters",
            tacticsTab: "A-share tactics",
            empty: "No graded outcomes yet — check back after the first 1-day horizon window closes.",
            decisions: "Calls",
            hitRate: "Hit %",
            avgAlpha: "Avg α",
            avgConf: "Avg conf",
            updated: "Updated",
            loading: "Loading…",
          }
        : {
            heading: "大师团 track record",
            sub: "每个大师 / 战法的滚动命中率 + 平均超额收益，每天回填一次。",
            mastersTab: "大师",
            tacticsTab: "A 股短线战法",
            empty: "暂无评分结果，等首个 1 天回测窗口结束后再回来看。",
            decisions: "决策数",
            hitRate: "命中率",
            avgAlpha: "平均 α",
            avgConf: "平均置信度",
            updated: "更新于",
            loading: "加载中…",
          },
    [en],
  );

  if (loading) {
    return (
      <div className="rounded-2xl border border-slate-200 bg-white p-4 text-sm text-slate-400">
        {copy.loading}
      </div>
    );
  }
  if (!data) return null;
  const isEmpty = (data.masters?.length ?? 0) === 0 && (data.tactics?.length ?? 0) === 0;

  if (compact) {
    return (
      <div className="rounded-2xl border border-slate-200 bg-white px-4 py-3">
        <div className="flex items-baseline justify-between">
          <h3 className="text-sm font-semibold text-slate-700">{copy.heading}</h3>
          <span className="text-[10px] text-slate-400">{copy.sub}</span>
        </div>
        {isEmpty ? (
          <div className="mt-2 text-xs text-slate-400">{copy.empty}</div>
        ) : (
          <div className="mt-3 grid grid-cols-1 gap-2 sm:grid-cols-2">
            <CompactStrip title={copy.mastersTab} rows={data.masters} copy={copy} />
            <CompactStrip title={copy.tacticsTab} rows={data.tactics} copy={copy} />
          </div>
        )}
      </div>
    );
  }

  return (
    <section className="rounded-2xl border border-slate-200 bg-white p-5">
      <header className="mb-4 flex items-baseline justify-between">
        <h2 className="text-base font-semibold text-slate-800">{copy.heading}</h2>
        <span className="text-xs text-slate-400">{copy.sub}</span>
      </header>
      {isEmpty ? (
        <div className="rounded-lg border border-dashed border-slate-200 bg-slate-50 px-4 py-6 text-center text-xs text-slate-500">
          {copy.empty}
        </div>
      ) : (
        <div className="grid grid-cols-1 gap-6 lg:grid-cols-2">
          <FullTable title={copy.mastersTab} rows={data.masters} copy={copy} />
          <FullTable title={copy.tacticsTab} rows={data.tactics} copy={copy} />
        </div>
      )}
    </section>
  );
};

interface CopyShape {
  mastersTab: string;
  tacticsTab: string;
  decisions: string;
  hitRate: string;
  avgAlpha: string;
  avgConf: string;
  updated: string;
}

const CompactStrip: React.FC<{
  title: string;
  rows: AdvisorTrackRecordRow[];
  copy: CopyShape;
}> = ({ title, rows, copy }) => {
  if (rows.length === 0) return null;
  return (
    <div className="rounded-lg bg-slate-50 px-3 py-2">
      <div className="text-[11px] font-medium uppercase tracking-wide text-slate-500">{title}</div>
      <ul className="mt-1 space-y-0.5">
        {rows.slice(0, 4).map((r) => (
          <li key={r.agent_id} className="flex items-center justify-between text-[12px]">
            <span className="truncate font-medium text-slate-700">{shortName(r)}</span>
            <span className="ml-2 shrink-0 tabular-nums text-slate-500">
              {formatHitRate(r.hit_rate)} · {r.decisions_count}
              {copy.decisions ? "" : ""}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
};

const FullTable: React.FC<{
  title: string;
  rows: AdvisorTrackRecordRow[];
  copy: CopyShape;
}> = ({ title, rows, copy }) => {
  if (rows.length === 0) return null;
  return (
    <div>
      <h3 className="mb-2 text-sm font-semibold text-slate-700">{title}</h3>
      <div className="overflow-hidden rounded-lg border border-slate-200">
        <table className="w-full text-left text-xs">
          <thead className="bg-slate-50 text-slate-500">
            <tr>
              <th className="px-3 py-2 font-medium">{title}</th>
              <th className="px-3 py-2 font-medium tabular-nums">{copy.decisions}</th>
              <th className="px-3 py-2 font-medium tabular-nums">{copy.hitRate}</th>
              <th className="px-3 py-2 font-medium tabular-nums">{copy.avgAlpha}</th>
              <th className="px-3 py-2 font-medium tabular-nums">{copy.avgConf}</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.agent_id} className="border-t border-slate-100">
                <td className="px-3 py-2 text-slate-700">
                  <div className="font-medium">{shortName(r)}</div>
                  <div className="text-[10px] text-slate-400">{r.agent_id}</div>
                </td>
                <td className="px-3 py-2 tabular-nums text-slate-600">{r.decisions_count}</td>
                <td className="px-3 py-2 tabular-nums">
                  <span className={hitRateTone(r.hit_rate, r.decisions_count)}>
                    {formatHitRate(r.hit_rate)}
                  </span>
                </td>
                <td className="px-3 py-2 tabular-nums">
                  <span className={alphaTone(r.avg_alpha)}>{formatAlpha(r.avg_alpha)}</span>
                </td>
                <td className="px-3 py-2 tabular-nums text-slate-500">
                  {Math.round(r.avg_confidence)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
};

function shortName(r: AdvisorTrackRecordRow): string {
  if (r.agent_name && r.agent_name.trim() !== "") return r.agent_name;
  const key = r.agent_id.includes(":") ? r.agent_id.split(":")[1] : r.agent_id;
  return key.replace(/_/g, " ");
}

function formatHitRate(rate: number): string {
  if (!Number.isFinite(rate) || rate <= 0) return "—";
  return `${Math.round(rate * 100)}%`;
}

function hitRateTone(rate: number, calls: number): string {
  if (calls < 5) return "text-slate-400";
  if (rate >= 0.6) return "text-emerald-600 font-medium";
  if (rate >= 0.5) return "text-slate-700";
  return "text-rose-600";
}

function formatAlpha(alpha: number): string {
  if (!Number.isFinite(alpha) || alpha === 0) return "0.0%";
  const pct = alpha * 100;
  const sign = pct >= 0 ? "+" : "";
  return `${sign}${pct.toFixed(1)}%`;
}

function alphaTone(alpha: number): string {
  if (alpha > 0.001) return "text-emerald-600 font-medium";
  if (alpha < -0.001) return "text-rose-600";
  return "text-slate-500";
}

export default AdvisorTrackRecordPanel;
