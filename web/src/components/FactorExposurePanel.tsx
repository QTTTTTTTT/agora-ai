// FactorExposurePanel — per-fund factor exposure dashboard
// (S7 / P3-1).
//
// Renders the six canonical factor exposures of the current
// portfolio: net + gross bars, holding count, calibration
// freshness, and overall coverage of the read. When the loadings
// are stale or coverage is partial, the panel surfaces both
// inline so the PM never sees a misleading "0.0 momentum tilt"
// when really there's no calibration to ground that number.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  fetchFactorExposureSnapshot,
  formatApiError,
  type FactorExposureSnapshot,
} from "../lib/api";
import { ALL_FACTORS, type Factor } from "@fundai/api-client";

type Language = "zh-CN" | "en-US";

interface Messages {
  panelTitle: string;
  panelSubtitle: string;
  refresh: string;
  loading: string;
  empty: string;
  error: string;
  navLabel: string;
  holdingsLabel: string;
  coverageLabel: string;
  loadingsAsOfLabel: string;
  loadingsAsOfStale: string;
  factorSize: string;
  factorValue: string;
  factorMomentum: string;
  factorQuality: string;
  factorLowVol: string;
  factorMarketBeta: string;
  netExposureLabel: string;
  grossExposureLabel: string;
  holdingCountLabel: string;
}

const messages: Record<Language, Messages> = {
  "zh-CN": {
    panelTitle: "因子敞口",
    panelSubtitle:
      "当前持仓在 size / value / momentum / quality / lowvol / market_beta 六个标准因子上的净敞口与总敞口；净敞口能看见 tilt，总敞口能看见长短对冲下的隐性因子风险。",
    refresh: "刷新",
    loading: "计算中…",
    empty: "当前无持仓",
    error: "加载失败",
    navLabel: "总市值",
    holdingsLabel: "持仓数",
    coverageLabel: "覆盖率",
    loadingsAsOfLabel: "校准日",
    loadingsAsOfStale: "校准过期",
    factorSize: "Size",
    factorValue: "Value",
    factorMomentum: "Momentum",
    factorQuality: "Quality",
    factorLowVol: "Low Vol",
    factorMarketBeta: "Market β",
    netExposureLabel: "净敞口",
    grossExposureLabel: "总敞口",
    holdingCountLabel: "贡献持仓",
  },
  "en-US": {
    panelTitle: "Factor exposure",
    panelSubtitle:
      "Net and gross exposure of the current portfolio across the six canonical factors. Net surfaces tilt; gross surfaces hidden factor risk under long-short hedges.",
    refresh: "Refresh",
    loading: "Computing…",
    empty: "No active holdings",
    error: "Failed to load",
    navLabel: "Gross MV",
    holdingsLabel: "Holdings",
    coverageLabel: "Coverage",
    loadingsAsOfLabel: "Loadings asof",
    loadingsAsOfStale: "Stale calibration",
    factorSize: "Size",
    factorValue: "Value",
    factorMomentum: "Momentum",
    factorQuality: "Quality",
    factorLowVol: "Low Vol",
    factorMarketBeta: "Market β",
    netExposureLabel: "Net",
    grossExposureLabel: "Gross",
    holdingCountLabel: "Contributing holdings",
  },
};

interface FactorExposurePanelProps {
  fundId?: string;
  language?: Language;
}

const REFRESH_MS = 5 * 60_000;
// Loadings older than this trigger the "stale" badge. 30 days
// matches a typical Quant Lab refresh cadence — beyond that we
// want the PM to know the numbers may not reflect today's market.
const STALE_LOADINGS_DAYS = 30;

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

function isStale(loadingsAsOf?: string): boolean {
  if (!loadingsAsOf) return false;
  const asof = new Date(loadingsAsOf + "T00:00:00Z");
  if (Number.isNaN(asof.getTime())) return false;
  const ageDays = (Date.now() - asof.getTime()) / (24 * 60 * 60 * 1000);
  return ageDays >= STALE_LOADINGS_DAYS;
}

// Tomahawk-style horizontal bar: positive grows right, negative
// grows left. Clamps to [-1, 1] visually because real net
// exposures past ±1 are pathological (would mean the loading
// itself is > 1 across the whole book).
function ExposureBar({ value }: { value: number }) {
  const clamped = Math.max(-1, Math.min(1, value));
  const widthPct = Math.abs(clamped) * 50;
  const positive = clamped >= 0;
  return (
    <div className="relative h-3 w-full rounded-full bg-gray-100">
      <div className="absolute inset-y-0 left-1/2 w-px bg-gray-300" />
      <div
        className={`absolute inset-y-0 ${positive ? "left-1/2 bg-emerald-500" : "right-1/2 bg-rose-500"}`}
        style={{ width: `${widthPct}%` }}
      />
    </div>
  );
}

export default function FactorExposurePanel({ fundId, language = "zh-CN" }: FactorExposurePanelProps) {
  const m = useMemo(() => messages[language], [language]);
  const [snap, setSnap] = useState<FactorExposureSnapshot | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const fetchSnap = useCallback(async () => {
    if (!fundId) {
      setSnap(null);
      return;
    }
    setLoading(true);
    setError(null);
    try {
      const resp = await fetchFactorExposureSnapshot(fundId);
      setSnap(resp.snapshot);
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setLoading(false);
    }
  }, [fundId, m.error]);

  useEffect(() => {
    fetchSnap().catch(() => {});
    const id = window.setInterval(() => {
      fetchSnap().catch(() => {});
    }, REFRESH_MS);
    return () => window.clearInterval(id);
  }, [fetchSnap]);

  const coverage = snap ? snap.holdings_covered / Math.max(1, snap.holdings_total) : 0;
  const oldestStale = snap?.oldest_loading_asof && isStale(snap.oldest_loading_asof);

  return (
    <section className="space-y-4 rounded-2xl border border-gray-100 bg-white p-6 shadow-sm">
      <header className="flex items-start justify-between gap-4">
        <div>
          <h2 className="text-base font-semibold text-gray-900">{m.panelTitle}</h2>
          <p className="mt-1 text-sm text-gray-500">{m.panelSubtitle}</p>
        </div>
        <button
          type="button"
          onClick={() => fetchSnap().catch(() => {})}
          className="rounded-md border border-gray-200 px-3 py-1.5 text-xs font-medium text-gray-600 hover:bg-gray-50"
        >
          {m.refresh}
        </button>
      </header>

      {error && (
        <div className="rounded-md border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-700">
          {error}
        </div>
      )}

      {snap && (
        <div className="grid grid-cols-2 gap-4 rounded-md border border-gray-100 bg-gray-50 p-4 text-xs text-gray-600 md:grid-cols-4">
          <div>
            <div className="text-gray-400">{m.navLabel}</div>
            <div className="mt-1 font-mono text-base text-gray-900">
              {snap.nav.toLocaleString(undefined, { maximumFractionDigits: 0 })}
            </div>
          </div>
          <div>
            <div className="text-gray-400">{m.holdingsLabel}</div>
            <div className="mt-1 font-mono text-base text-gray-900">
              {snap.holdings_covered}/{snap.holdings_total}
            </div>
          </div>
          <div>
            <div className="text-gray-400">{m.coverageLabel}</div>
            <div className={`mt-1 font-mono text-base ${coverage < 0.7 ? "text-amber-600" : "text-gray-900"}`}>
              {(coverage * 100).toFixed(1)}%
            </div>
          </div>
          <div>
            <div className="text-gray-400">{m.loadingsAsOfLabel}</div>
            <div className={`mt-1 font-mono text-base ${oldestStale ? "text-amber-600" : "text-gray-900"}`}>
              {snap.oldest_loading_asof ?? "—"}
              {oldestStale && (
                <span className="ml-2 text-xs">({m.loadingsAsOfStale})</span>
              )}
            </div>
          </div>
        </div>
      )}

      {loading && !snap && (
        <p className="rounded-md border border-gray-100 bg-gray-50 px-3 py-6 text-center text-xs text-gray-500">
          {m.loading}
        </p>
      )}

      {snap && snap.holdings_total === 0 && (
        <p className="rounded-md border border-gray-100 bg-gray-50 px-3 py-6 text-center text-xs text-gray-500">
          {m.empty}
        </p>
      )}

      {snap && snap.holdings_total > 0 && (
        <div className="space-y-3">
          {ALL_FACTORS.map((factor) => {
            const row = snap.exposures.find((r) => r.factor === factor);
            const net = row?.net_exposure ?? 0;
            const gross = row?.gross_exposure ?? 0;
            const count = row?.holding_count ?? 0;
            return (
              <div key={factor} className="rounded-md border border-gray-100 p-3">
                <div className="flex items-center justify-between text-xs">
                  <span className="font-semibold text-gray-800">{factorLabel(m, factor)}</span>
                  <span className="font-mono text-gray-500">
                    {m.holdingCountLabel}: {count}
                  </span>
                </div>
                <div className="mt-2 grid grid-cols-1 gap-3 md:grid-cols-2">
                  <div>
                    <div className="flex items-center justify-between text-[11px] text-gray-500">
                      <span>{m.netExposureLabel}</span>
                      <span className="font-mono">{net.toFixed(3)}</span>
                    </div>
                    <div className="mt-1">
                      <ExposureBar value={net} />
                    </div>
                  </div>
                  <div>
                    <div className="flex items-center justify-between text-[11px] text-gray-500">
                      <span>{m.grossExposureLabel}</span>
                      <span className="font-mono">{gross.toFixed(3)}</span>
                    </div>
                    <div className="mt-1 h-3 w-full rounded-full bg-gray-100">
                      <div
                        className="h-3 rounded-full bg-blue-500"
                        style={{ width: `${Math.min(100, gross * 100)}%` }}
                      />
                    </div>
                  </div>
                </div>
              </div>
            );
          })}
        </div>
      )}
    </section>
  );
}
