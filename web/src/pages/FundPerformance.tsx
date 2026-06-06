import React, { useCallback, useEffect, useMemo, useState } from "react";
import { useParams } from "react-router-dom";
import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  Cell,
  Legend,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import {
  fetchFundNavHistory,
  fetchFundPnLAttribution,
  formatApiError,
  type FundNAVPoint,
  type PnLAttribution,
  type PnLAttributionBucket,
} from "../lib/api";
import {
  formatDateForLanguage,
  formatMoneyForDisplay,
  formatNumberForLanguage,
  useAppPreferences,
  type AppLanguage,
} from "../lib/preferences";
import FactorExposurePanel from "../components/FactorExposurePanel";
import VaRPanel from "../components/VaRPanel";
import StressTestPanel from "../components/StressTestPanel";
import BrinsonAttributionPanel from "../components/BrinsonAttributionPanel";
import AgentReputationSection from "../components/AgentReputationSection";
import AnalystPanelSection from "../components/AnalystPanelSection";
import BullBearDebateSection from "../components/BullBearDebateSection";
import StrategyAttributionPanel from "../components/StrategyAttributionPanel";
// W9-3 — i18n migration. The runtime bundle comes from
// `i18n.getResourceBundle()`; the static import is the
// fallback / type-source so `copy.X` access stays type-safe
// (the `useTranslation` t() function is fine for top-level
// keys but loses static typing on nested objects, which this
// page leans on heavily — kpis.totalReturn, contributorsCols.symbol …).
import i18n from "../i18n";
import fundPerformanceEnFallback from "../i18n/locales/en-US/fundPerformance";

// PR-3A9: a dedicated /funds/:id/performance route that compresses
// everything the operator needs to answer "is this fund actually
// working?" into one scroll:
//
//   1. KPI strip: NAV, total return, realised/unrealised, fees.
//   2. Equity curve: NAV + total assets with cash/market split.
//   3. Symbol attribution: best & worst contributors.
//   4. Asset-class attribution: a colour-coded bar.
//   5. Daily-return histogram: a sanity check on tail risk.
//   6. Strategy sleeve learning panel (reused from PR-3A4 work).
//
// The page does NOT call any new backend endpoint — it leans on
// the existing NAV + P&L attribution + strategy attribution APIs.

type WindowChoice = "7d" | "30d" | "90d" | "1y" | "all";

interface WindowOption {
  key: WindowChoice;
  label: string;
  days: number | null;
}

// W9-3 — `PageCopy` shape is now derived structurally from the
// English fallback bundle, the single source of truth for the
// translation surface. Keep the alias so existing JSX type
// inference (e.g. `copy.kpis.totalReturn`) stays unchanged.
type PageCopy = typeof fundPerformanceEnFallback;

const WINDOW_OPTIONS: WindowOption[] = [
  { key: "7d", label: "7d", days: 7 },
  { key: "30d", label: "30d", days: 30 },
  { key: "90d", label: "90d", days: 90 },
  { key: "1y", label: "1y", days: 365 },
  { key: "all", label: "all", days: null },
];

function isoDateOffset(days: number): string {
  // Both /nav and /pnl-attribution call parseOptionalTime() on the
  // server, which is strict RFC 3339 (e.g. 2026-04-22T00:00:00Z).
  // Plain YYYY-MM-DD is rejected as 400, so we return the full ISO
  // datetime. The offset itself is computed in the user's local
  // calendar so "Last 7 days" lines up with the operator's
  // expectations rather than a UTC midnight slice.
  const now = new Date();
  now.setHours(0, 0, 0, 0);
  const offset = new Date(now);
  offset.setDate(offset.getDate() - days);
  return offset.toISOString();
}

function formatPercent(value: number, language: AppLanguage, fractionDigits = 2): string {
  if (!Number.isFinite(value)) {
    return "—";
  }
  return `${formatNumberForLanguage(value * 100, language, { maximumFractionDigits: fractionDigits, minimumFractionDigits: 0 })}%`;
}

function formatSigned(value: number, language: AppLanguage): string {
  if (!Number.isFinite(value)) {
    return "—";
  }
  const formatted = formatNumberForLanguage(value, language, { maximumFractionDigits: 2, minimumFractionDigits: 0 });
  if (value > 0) {
    return `+${formatted}`;
  }
  return formatted;
}

function pnlColorClass(value: number): string {
  if (!Number.isFinite(value) || value === 0) {
    return "text-gray-700";
  }
  return value > 0 ? "text-emerald-700" : "text-red-700";
}

interface ChartDailyPoint {
  date: string;
  nav: number;
  totalAssets: number;
  availableCash: number;
  totalMarketValue: number;
  dailyReturn: number;
}

// buildHistogram lays the daily-return series into ~12 evenly
// spaced buckets centred on zero. We compute symmetric edges so
// negative and positive tails are directly comparable in the
// chart; the alternative (Sturges' rule with min/max edges)
// would make a one-day big drop look like the entire left half.
function buildHistogram(values: number[], bucketCount = 12): { binLabel: string; count: number; mid: number }[] {
  if (values.length === 0) {
    return [];
  }
  let maxAbs = 0;
  for (const v of values) {
    if (!Number.isFinite(v)) {
      continue;
    }
    const abs = Math.abs(v);
    if (abs > maxAbs) {
      maxAbs = abs;
    }
  }
  if (maxAbs === 0) {
    // All-zero returns — show a single bucket so the user still
    // sees "yes, days happened, they just had no movement".
    return [{ binLabel: "0%", count: values.length, mid: 0 }];
  }
  // Round maxAbs up to the next 0.5% step so bin labels are
  // human-friendly (no "1.2345%" edges).
  const step = Math.max(0.005, Math.ceil((maxAbs / bucketCount) * 200) / 200);
  const half = Math.floor(bucketCount / 2);
  const buckets = Array.from({ length: bucketCount }, (_, i) => {
    const lower = (i - half) * step;
    const upper = lower + step;
    return {
      lower,
      upper,
      count: 0,
      mid: (lower + upper) / 2,
    };
  });
  for (const v of values) {
    if (!Number.isFinite(v)) {
      continue;
    }
    // Find the bucket. Out-of-range values fall into the
    // closest tail bucket — that's the right behaviour for a
    // distribution sketch because the tails are exactly what
    // the operator wants to see.
    let placed = false;
    for (let i = 0; i < buckets.length; i++) {
      if (v >= buckets[i].lower && v < buckets[i].upper) {
        buckets[i].count++;
        placed = true;
        break;
      }
    }
    if (!placed) {
      if (v < buckets[0].lower) {
        buckets[0].count++;
      } else {
        buckets[buckets.length - 1].count++;
      }
    }
  }
  return buckets.map((b) => ({
    binLabel: `${(b.lower * 100).toFixed(1)}%`,
    count: b.count,
    mid: b.mid,
  }));
}

interface KpiCardProps {
  label: string;
  value: string;
  hint?: string;
  emphasise?: boolean;
  tone?: "positive" | "negative" | "neutral";
}

const KpiCard: React.FC<KpiCardProps> = ({ label, value, hint, emphasise, tone }) => {
  const valueColor =
    tone === "positive"
      ? "text-emerald-700"
      : tone === "negative"
        ? "text-red-700"
        : emphasise
          ? "text-indigo-700"
          : "text-gray-900";
  return (
    <div className="rounded-xl border border-gray-200 bg-white p-4 shadow-sm">
      <p className="text-[11px] font-semibold uppercase tracking-wide text-gray-500">{label}</p>
      <p className={`mt-1 text-xl font-bold ${valueColor}`}>{value}</p>
      {hint ? <p className="mt-1 text-xs text-gray-500">{hint}</p> : null}
    </div>
  );
};

const ASSET_CLASS_COLOURS = ["#4f46e5", "#0ea5e9", "#10b981", "#f59e0b", "#ef4444", "#a855f7", "#14b8a6", "#f97316"];

const FundPerformance: React.FC = () => {
  const { fundId } = useParams<{ fundId: string }>();
  const { language, displayCurrency } = useAppPreferences();
  // W9-3 — fetch the full translation bundle so consumer JSX
  // continues to read `copy.kpis.totalReturn` etc. unchanged.
  // Falling back to the English bundle keeps the page rendering
  // even if a future locale ships incomplete (the parity guard
  // catches that at CI but here we want belt-and-braces).
  const copy: PageCopy = useMemo(() => {
    const bundle = i18n.getResourceBundle(language, "fundPerformance") as
      | PageCopy
      | undefined;
    return bundle ?? fundPerformanceEnFallback;
  }, [language]);

  const [windowChoice, setWindowChoice] = useState<WindowChoice>("30d");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [pnl, setPnl] = useState<PnLAttribution | null>(null);
  const [nav, setNav] = useState<FundNAVPoint[]>([]);

  const windowOption = useMemo(
    () => WINDOW_OPTIONS.find((opt) => opt.key === windowChoice) ?? WINDOW_OPTIONS[1],
    [windowChoice],
  );

  const load = useCallback(async () => {
    if (!fundId) {
      return;
    }
    setLoading(true);
    setError(null);
    try {
      const from = windowOption.days !== null ? isoDateOffset(windowOption.days) : undefined;
      const [pnlResp, navResp] = await Promise.all([
        fetchFundPnLAttribution(fundId, from),
        fetchFundNavHistory(fundId, from),
      ]);
      setPnl(pnlResp);
      setNav(Array.isArray(navResp) ? navResp : []);
    } catch (err) {
      setError(formatApiError(err, copy.errorPrefix));
    } finally {
      setLoading(false);
    }
  }, [copy.errorPrefix, fundId, windowOption.days]);

  useEffect(() => {
    void load();
  }, [load]);

  const chartData: ChartDailyPoint[] = useMemo(() => {
    return nav.map((point) => ({
      date: point.date,
      nav: point.nav,
      totalAssets: point.totalAssets,
      availableCash: point.availableCash,
      totalMarketValue: point.totalMarketValue,
      dailyReturn: point.dailyReturn,
    }));
  }, [nav]);

  // Sort symbol buckets so the contributors list runs top to
  // bottom by total P&L; the detractors list runs by negative
  // P&L. We slice independently to avoid showing the same
  // symbol twice in a thin universe.
  const contributors: PnLAttributionBucket[] = useMemo(() => {
    const rows = pnl?.bySymbol ?? [];
    return [...rows].sort((a, b) => b.totalPnl - a.totalPnl).slice(0, 5);
  }, [pnl?.bySymbol]);
  const detractors: PnLAttributionBucket[] = useMemo(() => {
    const rows = pnl?.bySymbol ?? [];
    return [...rows]
      .filter((row) => row.totalPnl < 0)
      .sort((a, b) => a.totalPnl - b.totalPnl)
      .slice(0, 5);
  }, [pnl?.bySymbol]);

  const histogramData = useMemo(() => {
    const returns = chartData
      .map((point) => point.dailyReturn)
      .filter((v) => Number.isFinite(v) && v !== 0);
    return buildHistogram(returns);
  }, [chartData]);

  const fundCurrency = "USD"; // we don't have the fund's baseCurrency in this page;
  // formatMoneyForDisplay will fall back to the operator's display preference.

  const dateAxisFormatter = useCallback(
    (value: string) => formatDateForLanguage(value, language),
    [language],
  );

  return (
    <div className="space-y-6">
      {/* Header + window picker */}
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">{copy.pageTitle}</h1>
          <p className="mt-1 max-w-2xl text-sm text-gray-500">{copy.pageSubtitle}</p>
        </div>
        <div className="flex items-center gap-2 rounded-lg bg-gray-100 p-1 text-xs">
          <span className="px-2 text-gray-500">{copy.windowGroupLabel}:</span>
          {WINDOW_OPTIONS.map((opt) => (
            <button
              key={opt.key}
              type="button"
              onClick={() => setWindowChoice(opt.key)}
              className={`rounded-md px-3 py-1 font-semibold transition ${
                windowChoice === opt.key ? "bg-white text-indigo-700 shadow-sm" : "text-gray-500 hover:text-gray-700"
              }`}
            >
              {copy.windowLabels[opt.key]}
            </button>
          ))}
        </div>
      </header>

      {error ? (
        <div className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">
          <p>{error}</p>
          <button
            type="button"
            onClick={() => void load()}
            className="mt-2 rounded-md bg-red-600 px-3 py-1 text-xs font-semibold text-white hover:bg-red-700"
          >
            {copy.retry}
          </button>
        </div>
      ) : null}

      {loading && !pnl && !error ? (
        <div className="rounded-lg border border-gray-200 bg-white p-6 text-sm text-gray-500">{copy.loading}</div>
      ) : null}

      {pnl ? (
        <>
          {/* KPI strip */}
          <section className="grid grid-cols-2 gap-3 md:grid-cols-4">
            <KpiCard
              label={copy.kpis.totalReturn}
              value={formatPercent(pnl.returnPct, language)}
              tone={pnl.returnPct >= 0 ? "positive" : "negative"}
              emphasise
            />
            <KpiCard
              label={copy.kpis.totalPnL}
              value={formatSigned(pnl.totalPnl, language)}
              tone={pnl.totalPnl >= 0 ? "positive" : "negative"}
            />
            <KpiCard label={copy.kpis.realizedPnL} value={formatSigned(pnl.realizedPnl, language)} />
            <KpiCard label={copy.kpis.unrealizedPnL} value={formatSigned(pnl.unrealizedPnl, language)} />
            <KpiCard
              label={copy.kpis.feeDrag}
              value={formatSigned(-pnl.feeDrag, language)}
              tone={pnl.feeDrag > 0 ? "negative" : "neutral"}
              hint={pnl.feeDrag > 0 ? formatPercent(pnl.feeDrag / Math.max(pnl.beginningAssets, 1), language) : undefined}
            />
            <KpiCard
              label={copy.kpis.beginningAssets}
              value={formatMoneyForDisplay(pnl.beginningAssets, fundCurrency, displayCurrency, language)}
            />
            <KpiCard
              label={copy.kpis.endingAssets}
              value={formatMoneyForDisplay(pnl.endingAssets, fundCurrency, displayCurrency, language)}
            />
            <KpiCard
              label={copy.kpis.nav}
              value={
                nav.length > 0
                  ? formatNumberForLanguage(nav[nav.length - 1].nav, language, {
                      minimumFractionDigits: 4,
                      maximumFractionDigits: 4,
                    })
                  : "—"
              }
            />
          </section>

          {/* NAV curve */}
          <section className="rounded-xl bg-white p-5 shadow-sm">
            <header>
              <h2 className="text-base font-semibold text-gray-900">{copy.navCurveTitle}</h2>
              <p className="mt-1 text-xs text-gray-500">{copy.navCurveSubtitle}</p>
            </header>
            {chartData.length === 0 ? (
              <p className="mt-4 rounded-md border border-dashed border-gray-200 p-6 text-center text-sm text-gray-500">
                {copy.noNavData}
              </p>
            ) : (
              <div className="mt-4 h-72">
                <ResponsiveContainer width="100%" height="100%">
                  <AreaChart data={chartData} margin={{ top: 5, right: 20, left: 0, bottom: 5 }}>
                    <defs>
                      <linearGradient id="cashGradient" x1="0" y1="0" x2="0" y2="1">
                        <stop offset="0%" stopColor="#cbd5e1" stopOpacity={0.6} />
                        <stop offset="100%" stopColor="#cbd5e1" stopOpacity={0.1} />
                      </linearGradient>
                      <linearGradient id="mvGradient" x1="0" y1="0" x2="0" y2="1">
                        <stop offset="0%" stopColor="#64748b" stopOpacity={0.8} />
                        <stop offset="100%" stopColor="#64748b" stopOpacity={0.2} />
                      </linearGradient>
                    </defs>
                    <CartesianGrid strokeDasharray="3 3" stroke="#e5e7eb" />
                    <XAxis dataKey="date" tick={{ fontSize: 11 }} tickFormatter={dateAxisFormatter} stroke="#9ca3af" />
                    <YAxis
                      yAxisId="assets"
                      tick={{ fontSize: 11 }}
                      stroke="#9ca3af"
                      tickFormatter={(v) => formatNumberForLanguage(v, language, { maximumFractionDigits: 0 })}
                    />
                    <YAxis
                      yAxisId="nav"
                      orientation="right"
                      tick={{ fontSize: 11 }}
                      stroke="#6366f1"
                      tickFormatter={(v) => formatNumberForLanguage(v, language, { maximumFractionDigits: 3 })}
                    />
                    <Tooltip
                      labelFormatter={(label) => formatDateForLanguage(String(label), language)}
                      formatter={(value, name) => {
                        if (typeof value !== "number") {
                          return [String(value), name as string];
                        }
                        if (name === copy.navLabel) {
                          return [formatNumberForLanguage(value, language, { maximumFractionDigits: 4 }), name];
                        }
                        return [formatMoneyForDisplay(value, fundCurrency, displayCurrency, language), name];
                      }}
                    />
                    <Legend wrapperStyle={{ fontSize: 11 }} />
                    <Area
                      yAxisId="assets"
                      type="monotone"
                      dataKey="totalMarketValue"
                      stackId="aum"
                      stroke="#475569"
                      fill="url(#mvGradient)"
                      name={copy.marketValueLabel}
                    />
                    <Area
                      yAxisId="assets"
                      type="monotone"
                      dataKey="availableCash"
                      stackId="aum"
                      stroke="#94a3b8"
                      fill="url(#cashGradient)"
                      name={copy.availableCashLabel}
                    />
                    <Area
                      yAxisId="nav"
                      type="monotone"
                      dataKey="nav"
                      stroke="#6366f1"
                      fill="transparent"
                      name={copy.navLabel}
                    />
                  </AreaChart>
                </ResponsiveContainer>
              </div>
            )}
          </section>

          {/* Contributors + detractors */}
          <section className="grid grid-cols-1 gap-4 lg:grid-cols-2">
            <ContributorsTable
              title={copy.contributorsTitle}
              subtitle={copy.contributorsSubtitle}
              empty={copy.contributorsEmpty}
              rows={contributors}
              language={language}
              cols={copy.contributorsCols}
            />
            <ContributorsTable
              title={copy.detractorsTitle}
              subtitle={copy.detractorsSubtitle}
              empty={copy.contributorsEmpty}
              rows={detractors}
              language={language}
              cols={copy.contributorsCols}
            />
          </section>

          {/* Asset-class attribution */}
          <section className="rounded-xl bg-white p-5 shadow-sm">
            <header>
              <h2 className="text-base font-semibold text-gray-900">{copy.assetClassTitle}</h2>
              <p className="mt-1 text-xs text-gray-500">{copy.assetClassSubtitle}</p>
            </header>
            {pnl.byAssetClass.length === 0 ? (
              <p className="mt-4 rounded-md border border-dashed border-gray-200 p-6 text-center text-sm text-gray-500">
                {copy.assetClassEmpty}
              </p>
            ) : (
              <div className="mt-4 h-64">
                <ResponsiveContainer width="100%" height="100%">
                  <BarChart
                    data={pnl.byAssetClass.map((row, idx) => ({
                      key: row.label || row.key || `#${idx + 1}`,
                      pnl: row.totalPnl,
                      exposure: row.exposure,
                      weight: row.weight,
                      idx,
                    }))}
                    margin={{ top: 5, right: 20, left: 0, bottom: 5 }}
                  >
                    <CartesianGrid strokeDasharray="3 3" stroke="#e5e7eb" />
                    <XAxis dataKey="key" tick={{ fontSize: 11 }} stroke="#9ca3af" />
                    <YAxis
                      tick={{ fontSize: 11 }}
                      stroke="#9ca3af"
                      tickFormatter={(v) =>
                        formatNumberForLanguage(v, language, { maximumFractionDigits: 0 })
                      }
                    />
                    <Tooltip
                      formatter={(value: number, name) => {
                        if (name === copy.pnlLabel) {
                          return [formatSigned(value, language), name];
                        }
                        if (name === copy.exposureLabel) {
                          return [formatMoneyForDisplay(value, fundCurrency, displayCurrency, language), name];
                        }
                        if (name === copy.weightLabel) {
                          return [formatPercent(value, language), name];
                        }
                        return [String(value), name as string];
                      }}
                    />
                    <Legend wrapperStyle={{ fontSize: 11 }} />
                    <Bar dataKey="pnl" name={copy.pnlLabel}>
                      {pnl.byAssetClass.map((row, idx) => (
                        <Cell
                          key={`${row.key}-${idx}`}
                          fill={row.totalPnl >= 0 ? "#10b981" : "#ef4444"}
                        />
                      ))}
                    </Bar>
                  </BarChart>
                </ResponsiveContainer>
              </div>
            )}
          </section>

          {/* Daily return histogram */}
          <section className="rounded-xl bg-white p-5 shadow-sm">
            <header>
              <h2 className="text-base font-semibold text-gray-900">{copy.dailyReturnsTitle}</h2>
              <p className="mt-1 text-xs text-gray-500">{copy.dailyReturnsSubtitle}</p>
            </header>
            {histogramData.length === 0 ? (
              <p className="mt-4 rounded-md border border-dashed border-gray-200 p-6 text-center text-sm text-gray-500">
                {copy.dailyReturnsEmpty}
              </p>
            ) : (
              <div className="mt-4 h-56">
                <ResponsiveContainer width="100%" height="100%">
                  <BarChart data={histogramData} margin={{ top: 5, right: 20, left: 0, bottom: 5 }}>
                    <CartesianGrid strokeDasharray="3 3" stroke="#e5e7eb" />
                    <XAxis dataKey="binLabel" tick={{ fontSize: 11 }} stroke="#9ca3af" />
                    <YAxis tick={{ fontSize: 11 }} stroke="#9ca3af" allowDecimals={false} />
                    <Tooltip
                      formatter={(value: number) => [
                        formatNumberForLanguage(value, language, { maximumFractionDigits: 0 }),
                        language === "zh-CN" ? "天数" : "Days",
                      ]}
                    />
                    <Bar dataKey="count">
                      {histogramData.map((row, idx) => (
                        <Cell
                          key={`bin-${idx}`}
                          fill={row.mid > 0 ? "#22c55e" : row.mid < 0 ? "#ef4444" : "#94a3b8"}
                        />
                      ))}
                    </Bar>
                  </BarChart>
                </ResponsiveContainer>
              </div>
            )}
          </section>
        </>
      ) : null}

      {/* Strategy attribution panel (reused) — sits at the bottom because
          it has its own refresh affordance and is independent of the
          window picker above. */}
      <StrategyAttributionPanel fundId={fundId} language={language} />

      {/* S7 / P3-1 — factor-exposure dashboard. Six canonical
          factors (size / value / momentum / quality / lowvol /
          market_beta) computed from current holdings against the
          instrument_factor_loadings calibration table. */}
      <FactorExposurePanel fundId={fundId} language={language} />

      {/* S7 / P3-2 — Value-at-Risk + Conditional VaR. Three
          methods (historical / parametric / Monte Carlo) ×
          three confidences (90 / 95 / 99) computed from
          nav_snapshots.daily_return. The spread across methods
          surfaces fat-tail risk that parametric alone hides. */}
      <VaRPanel fundId={fundId} language={language} />

      {/* S7 / P3-3 — Stress-scenario runner. Pick a named
          scenario (historical / hypothetical / regulatory) and
          project its shocks onto the current portfolio. */}
      <StressTestPanel fundId={fundId} language={language} />

      {/* S7 / P3-4 — Brinson three-effect attribution. Decompose
          active return vs an admin-maintained benchmark into
          allocation / selection / interaction effects per bucket. */}
      <BrinsonAttributionPanel fundId={fundId} language={language} />

      {/* S8.1 — 4-analyst (fundamentals / sentiment / news /
          technical) panel that votes on one symbol and produces an
          aggregated bullish / bearish / neutral verdict. */}
      <AnalystPanelSection fundId={fundId} language={language} />

      {/* S8.2 — forced Bull / Bear adversarial debate on top of
          the S8.1 panel: each round Bull and Bear argue against
          each other, neither allowed to settle on neutral. */}
      <BullBearDebateSection fundId={fundId} language={language} />

      {/* S8.4 — per-agent reputation ledger. Each analyst /
          advocate / PM's historical calls are scored against
          realised forward returns so the PM can up- or
          down-weight them going forward. */}
      <AgentReputationSection fundId={fundId} language={language} />
    </div>
  );
};

// ContributorsTable renders the top-K / bottom-K bucket grid.
// Extracted so the contributor + detractor sides share one
// implementation and stay visually identical.
const ContributorsTable: React.FC<{
  title: string;
  subtitle: string;
  empty: string;
  rows: PnLAttributionBucket[];
  language: AppLanguage;
  cols: PageCopy["contributorsCols"];
}> = ({ title, subtitle, empty, rows, language, cols }) => {
  return (
    <div className="rounded-xl bg-white p-5 shadow-sm">
      <header>
        <h2 className="text-base font-semibold text-gray-900">{title}</h2>
        <p className="mt-1 text-xs text-gray-500">{subtitle}</p>
      </header>
      {rows.length === 0 ? (
        <p className="mt-4 rounded-md border border-dashed border-gray-200 p-6 text-center text-sm text-gray-500">{empty}</p>
      ) : (
        <div className="mt-4 overflow-x-auto">
          <table className="w-full min-w-[640px] divide-y divide-gray-100 text-sm">
            <thead className="bg-gray-50 text-xs uppercase tracking-wide text-gray-500">
              <tr>
                <th className="px-3 py-2 text-left">{cols.symbol}</th>
                <th className="px-3 py-2 text-right">{cols.realized}</th>
                <th className="px-3 py-2 text-right">{cols.unrealized}</th>
                <th className="px-3 py-2 text-right">{cols.total}</th>
                <th className="px-3 py-2 text-right">{cols.trades}</th>
                <th className="px-3 py-2 text-right">{cols.weight}</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-100">
              {rows.map((row) => (
                <tr key={row.key} className="hover:bg-gray-50">
                  <td className="px-3 py-2 font-medium text-gray-900">{row.label || row.key}</td>
                  <td className={`px-3 py-2 text-right ${pnlColorClass(row.realizedPnl)}`}>
                    {formatSigned(row.realizedPnl, language)}
                  </td>
                  <td className={`px-3 py-2 text-right ${pnlColorClass(row.unrealizedPnl)}`}>
                    {formatSigned(row.unrealizedPnl, language)}
                  </td>
                  <td className={`px-3 py-2 text-right font-semibold ${pnlColorClass(row.totalPnl)}`}>
                    {formatSigned(row.totalPnl, language)}
                  </td>
                  <td className="px-3 py-2 text-right text-gray-700">{row.tradeCount}</td>
                  <td className="px-3 py-2 text-right text-gray-700">{formatPercent(row.weight, language, 1)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
};

// Silence the unused-var warning Vite throws when a future
// refactor stops touching one of the colour palette entries.
// We export it instead of inlining so the constants are
// discoverable when someone wants to swap in a brand palette.
export const __assetClassColours = ASSET_CLASS_COLOURS;

export default FundPerformance;
