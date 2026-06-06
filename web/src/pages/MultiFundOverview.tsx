// MultiFundOverview.tsx — cross-fund portfolio summary.
//
// WHY THIS EXISTS
// ---------------
// /companies lists each company plus its funds, but if you run
// 4–5 funds across 2–3 companies you have no single page that
// shows the GLOBAL state — total AUM, NAV health, today's
// workflow status across funds, which fund needs attention. You
// have to click into each one.
//
// This page is the cross-cutting summary. It fetches the same
// /api/companies/overview endpoint (no new backend needed) and
// re-renders it as a portfolio-of-funds dashboard:
//
//   - top KPIs: # funds, # companies, total AUM (display
//     currency), avg NAV, # funds in live mode;
//   - per-fund table sorted descending by total assets, with
//     status, mode, NAV, AUM, base currency, deep-link;
//   - filters: trading mode (sim/live/all), status, free-text
//     name search;
//   - mobile-aware: switches to a card layout below md via
//     ResponsiveTable.
//
// SCOPE — STARTER
// ---------------
// Doesn't yet stream daily PnL deltas (still a polled fetch).
// Workflow state per fund DOES stream now (W5-2 follow-up): we
// open one multiplexed SSE connection via `useWorkflowStreamMulti`
// for every visible fund and surface state + progress percent in
// a dedicated table column, which avoids the per-fund EventSource
// fan-out the original comment warned about. Doesn't pull
// benchmarks. The point of this commit
// is to give operators ONE PLACE to see "are all my funds OK?".

import React, { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import i18n from "../i18n";
import { apiGet, formatApiError } from "../lib/api";
import { formatMoneyForDisplay, formatNumberForLanguage, useAppPreferences } from "../lib/preferences";
import { ResponsiveTable, type ResponsiveColumn } from "../components/ResponsiveTable";
import { useWorkflowStreamMulti } from "../lib/useWorkflowStream";
import multiFundOverviewEnFallback from "../i18n/locales/en-US/multiFundOverview";

interface Fund {
  id: string;
  companyId: string;
  name: string;
  tradingMode: string;
  totalAssets: number;
  nav: number;
  status: string;
  baseCurrency?: string;
  market?: string;
}

interface CompanyWithFunds {
  id: string;
  name: string;
  funds: Fund[];
}

interface FundRow extends Fund {
  companyName: string;
}

// tradingModeLabel resolves a fund's trading-mode string (live /
// simulation / paper) to its localised label. Reads from the
// active i18n bundle so adding a new mode means adding ONE key
// per locale, no code change. The original `mode` is returned
// verbatim for unknown values so we never silently swallow a
// new backend mode the bundle hasn't caught up with.
const tradingModeLabel = (mode: string, language: string): string => {
  const k = mode.toLowerCase();
  const bundle =
    (i18n.getResourceBundle(language, "multiFundOverview") as
      | typeof multiFundOverviewEnFallback
      | undefined) ?? multiFundOverviewEnFallback;
  const map = bundle.tradingModes as Record<string, string>;
  return map[k] ?? mode;
};

const statusTone = (status: string): string => {
  const s = status.toLowerCase();
  if (s === "active" || s === "running") return "bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-200";
  if (s === "paused" || s === "draft") return "bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-200";
  if (s === "archived" || s === "closed") return "bg-gray-200 text-gray-600 dark:bg-slate-700 dark:text-slate-300";
  return "bg-blue-100 text-blue-700 dark:bg-blue-900/40 dark:text-blue-200";
};

const MultiFundOverview: React.FC = () => {
  const { language, displayCurrency } = useAppPreferences();
  const { t } = useTranslation("multiFundOverview");
  const [companies, setCompanies] = useState<CompanyWithFunds[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  const [modeFilter, setModeFilter] = useState<"all" | "live" | "simulation">("all");

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    apiGet<CompanyWithFunds[]>("/api/companies/overview")
      .then((res) => {
        if (cancelled) return;
        setCompanies(res ?? []);
      })
      .catch((err) => {
        if (cancelled) return;
        setError(formatApiError(err, t("loadError")));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
    // The fetch is one-shot per mount; t() is captured at mount
    // time and language switches re-render the component anyway,
    // so we deliberately don't add `t` to the dependency array.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const allFunds = useMemo<FundRow[]>(() => {
    const out: FundRow[] = [];
    for (const company of companies) {
      for (const fund of company.funds ?? []) {
        out.push({ ...fund, companyName: company.name });
      }
    }
    return out;
  }, [companies]);

  // W5-2 — single multiplexed SSE for every visible fund. We pass
  // the FULL fund list (not the filtered list) so search / mode
  // filtering on the client doesn't tear down and reopen the
  // connection on every keystroke; the backend caps the
  // subscription set at 50 funds, which is well above the
  // current portfolio dashboard scale. The hook returns a
  // `Record<fundId, WorkflowStatusFrame|null>` we look up per row.
  const fundIds = useMemo(() => allFunds.map((f) => f.id), [allFunds]);
  const workflow = useWorkflowStreamMulti({
    fundIds,
    enabled: fundIds.length > 0,
  });

  const filteredFunds = useMemo<FundRow[]>(() => {
    const trimmed = search.trim().toLowerCase();
    return allFunds
      .filter((f) => {
        if (modeFilter !== "all" && f.tradingMode.toLowerCase() !== modeFilter) return false;
        if (trimmed && !`${f.name} ${f.companyName}`.toLowerCase().includes(trimmed)) return false;
        return true;
      })
      .sort((a, b) => (b.totalAssets ?? 0) - (a.totalAssets ?? 0));
  }, [allFunds, search, modeFilter]);

  // Top-line KPIs. Total AUM is summed in display currency; we
  // call formatMoneyForDisplay per fund and would normally need
  // FX conversion, but the existing helper handles base→display
  // automatically. For the aggregate KPI we sum the
  // already-converted (display-currency) numbers, which is
  // approximate but matches what the user sees per-fund. (Real
  // FX-aware aggregation belongs server-side; we'd inline a
  // /api/portfolio/summary endpoint later.)
  const kpis = useMemo(() => {
    const liveCount = allFunds.filter((f) => f.tradingMode.toLowerCase() === "live").length;
    const totalAssets = allFunds.reduce((acc, f) => acc + (Number.isFinite(f.totalAssets) ? f.totalAssets : 0), 0);
    const avgNav =
      allFunds.length > 0
        ? allFunds.reduce((acc, f) => acc + (Number.isFinite(f.nav) ? f.nav : 0), 0) / allFunds.length
        : 0;
    return {
      fundCount: allFunds.length,
      companyCount: companies.length,
      liveCount,
      totalAssets,
      avgNav,
    };
  }, [allFunds, companies.length]);

  // W8-3 — translations now live in the multiFundOverview i18n
  // namespace. We resolve the bundle once via `getResourceBundle`
  // to keep the existing `copy.col.foo` / `copy.workflow.bar`
  // accessor pattern working without per-key churn.
  const copy = useMemo(() => {
    const bundle =
      (i18n.getResourceBundle(language, "multiFundOverview") as
        | typeof multiFundOverviewEnFallback
        | undefined) ?? multiFundOverviewEnFallback;
    return bundle;
  }, [language]);

  const columns: ResponsiveColumn<FundRow>[] = useMemo(
    () => [
      {
        key: "fund",
        header: copy.col.fund,
        primary: true,
        cell: (row) => (
          <Link to={`/funds/${row.id}`} className="font-medium text-indigo-600 hover:text-indigo-500 dark:text-indigo-300">
            {row.name}
          </Link>
        ),
      },
      {
        key: "company",
        header: copy.col.company,
        cell: (row) => row.companyName,
      },
      {
        key: "mode",
        header: copy.col.mode,
        cell: (row) => (
          <span
            className={`inline-flex rounded-full px-2 py-0.5 text-[11px] font-medium ${
              row.tradingMode.toLowerCase() === "live"
                ? "bg-rose-100 text-rose-700 dark:bg-rose-900/40 dark:text-rose-200"
                : "bg-blue-100 text-blue-700 dark:bg-blue-900/40 dark:text-blue-200"
            }`}
          >
            {tradingModeLabel(row.tradingMode, language)}
          </span>
        ),
      },
      {
        key: "status",
        header: copy.col.status,
        cell: (row) => (
          <span className={`inline-flex rounded-full px-2 py-0.5 text-[11px] font-medium ${statusTone(row.status)}`}>
            {row.status}
          </span>
        ),
      },
      {
        // W5-2 — workflow streaming column. Reads the latest
        // multiplexed SSE frame for this row's fundId. Three
        // visual paths:
        //   - forbidden (server told us "no access"): static dash.
        //   - no frame yet: "idle / not started" placeholder.
        //   - frame present: state badge + (when running) % done
        //     + a small pulsing dot iff the connection is live
        //     and the workflow is non-terminal.
        key: "workflow",
        header: copy.col.workflow,
        cell: (row) => {
          if (workflow.forbidden.includes(row.id)) {
            return (
              <span className="inline-flex rounded-full bg-gray-100 px-2 py-0.5 text-[11px] font-medium text-gray-500 dark:bg-slate-700 dark:text-slate-300">
                {copy.workflow.forbidden}
              </span>
            );
          }
          const frame = workflow.statuses[row.id] ?? null;
          if (!frame) {
            return <span className="text-xs text-gray-400 dark:text-slate-500">{copy.workflow.idle}</span>;
          }
          const state = (frame.state ?? "").toLowerCase();
          const stateClass =
            state === "completed"
              ? "bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-200"
              : state === "failed"
              ? "bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-200"
              : state === "running"
              ? "bg-indigo-100 text-indigo-700 dark:bg-indigo-900/40 dark:text-indigo-200"
              : "bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-200";
          const stateLabel =
            state === "completed"
              ? copy.workflow.stateCompleted
              : state === "failed"
              ? copy.workflow.stateFailed
              : state === "running"
              ? copy.workflow.stateRunning
              : copy.workflow.stateQueued;
          const isTerminal = workflow.terminal[row.id] === true || state === "completed" || state === "failed";
          const showDot = workflow.connected && !isTerminal;
          return (
            <div className="flex flex-wrap items-center gap-1.5">
              <span className={`inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[11px] font-medium ${stateClass}`}>
                {showDot ? <span className="h-1.5 w-1.5 animate-pulse rounded-full bg-current" /> : null}
                {stateLabel}
              </span>
              {state === "running" && typeof frame.progressPercent === "number" ? (
                <span className="text-[11px] text-gray-500 dark:text-slate-400">
                  {Math.round(frame.progressPercent)}%
                </span>
              ) : null}
            </div>
          );
        },
        hideOnMobile: false,
      },
      {
        key: "totalAssets",
        header: copy.col.totalAssets,
        className: "text-right",
        cell: (row) => formatMoneyForDisplay(row.totalAssets, row.baseCurrency, displayCurrency, language),
      },
      {
        key: "nav",
        header: copy.col.nav,
        className: "text-right",
        cell: (row) => formatNumberForLanguage(row.nav, language, { minimumFractionDigits: 4, maximumFractionDigits: 4 }),
      },
      {
        key: "baseCurrency",
        header: copy.col.baseCurrency,
        cell: (row) => row.baseCurrency ?? "USD",
        hideOnMobile: true,
      },
    ],
    [
      copy.col,
      copy.workflow,
      displayCurrency,
      language,
      workflow.statuses,
      workflow.forbidden,
      workflow.terminal,
      workflow.connected,
    ],
  );

  if (loading) {
    return (
      <div className="rounded-xl border border-gray-200 bg-white p-6 text-sm text-gray-500 dark:border-slate-700 dark:bg-slate-900 dark:text-slate-400">
        {copy.loading}
      </div>
    );
  }

  if (error) {
    return (
      <div className="rounded-xl border border-red-200 bg-red-50 p-6 text-sm text-red-700 dark:border-red-900/50 dark:bg-red-950/20 dark:text-red-300">
        <p>{error}</p>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm dark:border-slate-700 dark:bg-slate-900">
        <div className="flex flex-col gap-2 sm:flex-row sm:items-end sm:justify-between">
          <div>
            <h1 className="text-2xl font-bold text-gray-900 dark:text-slate-100">{copy.title}</h1>
            <p className="mt-1 text-sm text-gray-500 dark:text-slate-400">{copy.subtitle}</p>
          </div>
        </div>
        <dl className="mt-6 grid grid-cols-2 gap-4 md:grid-cols-5">
          <KpiCard label={copy.kFunds} value={formatNumberForLanguage(kpis.fundCount, language)} />
          <KpiCard label={copy.kCompanies} value={formatNumberForLanguage(kpis.companyCount, language)} />
          <KpiCard label={copy.kLive} value={formatNumberForLanguage(kpis.liveCount, language)} />
          <KpiCard
            label={copy.kAum}
            value={formatMoneyForDisplay(kpis.totalAssets, "USD", displayCurrency, language)}
          />
          <KpiCard
            label={copy.kAvgNav}
            value={formatNumberForLanguage(kpis.avgNav, language, { minimumFractionDigits: 4, maximumFractionDigits: 4 })}
          />
        </dl>
      </div>

      <div className="rounded-2xl border border-gray-200 bg-white p-4 shadow-sm dark:border-slate-700 dark:bg-slate-900 sm:p-6">
        <div className="mb-4 flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <input
            type="search"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder={copy.searchPh}
            className="w-full max-w-md rounded-lg border border-gray-300 px-3 py-2 text-sm outline-none focus:border-indigo-500 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-100"
          />
          <select
            value={modeFilter}
            onChange={(e) => setModeFilter(e.target.value as "all" | "live" | "simulation")}
            className="rounded-lg border border-gray-300 px-3 py-2 text-sm outline-none focus:border-indigo-500 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-100"
          >
            <option value="all">{copy.modeAll}</option>
            <option value="live">{copy.modeLive}</option>
            <option value="simulation">{copy.modeSim}</option>
          </select>
        </div>
        <ResponsiveTable<FundRow>
          rows={filteredFunds}
          columns={columns}
          keyOf={(row) => row.id}
          empty={
            <div className="rounded-xl border border-dashed border-gray-300 p-10 text-center text-sm text-gray-500 dark:border-slate-600 dark:text-slate-400">
              <p className="font-medium">{copy.emptyTitle}</p>
              <p className="mt-1">{copy.emptyDescription}</p>
            </div>
          }
        />
      </div>
    </div>
  );
};

const KpiCard: React.FC<{ label: string; value: string }> = ({ label, value }) => (
  <div className="rounded-xl border border-gray-100 bg-gray-50 p-4 dark:border-slate-700 dark:bg-slate-800">
    <dt className="text-[11px] font-semibold uppercase tracking-wide text-gray-500 dark:text-slate-400">{label}</dt>
    <dd className="mt-1 text-lg font-bold text-gray-900 dark:text-slate-100">{value}</dd>
  </div>
);

export default MultiFundOverview;
