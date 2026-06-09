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
import { EnvelopeCard, PillTag, MetricBlock, SectionLabel } from "../theme";
import type { PillTagToneName } from "../theme/PillTag";

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

const statusToneName = (status: string): PillTagToneName => {
  const s = status.toLowerCase();
  if (s === "active" || s === "running") return "sage";
  if (s === "paused" || s === "draft") return "coral";
  if (s === "archived" || s === "closed") return "muted";
  return "info";
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
          <Link
            to={`/funds/${row.id}`}
            className="font-semibold text-sage-700 hover:text-sage-500 dark:text-sage-300"
          >
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
          <PillTag
            size="sm"
            tone={row.tradingMode.toLowerCase() === "live" ? "risk" : "info"}
          >
            {tradingModeLabel(row.tradingMode, language)}
          </PillTag>
        ),
      },
      {
        key: "status",
        header: copy.col.status,
        cell: (row) => (
          <PillTag size="sm" tone={statusToneName(row.status)}>
            {row.status}
          </PillTag>
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
            return <PillTag size="sm" tone="muted">{copy.workflow.forbidden}</PillTag>;
          }
          const frame = workflow.statuses[row.id] ?? null;
          if (!frame) {
            return <span className="text-xs text-ink-300 dark:text-slate-500">{copy.workflow.idle}</span>;
          }
          const state = (frame.state ?? "").toLowerCase();
          const stateTone: PillTagToneName =
            state === "completed" ? "sage"
            : state === "failed" ? "risk"
            : state === "running" ? "info"
            : "coral";
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
              <PillTag size="sm" tone={stateTone} dot={showDot}>{stateLabel}</PillTag>
              {state === "running" && typeof frame.progressPercent === "number" ? (
                <span className="text-[11px] text-ink-300 dark:text-slate-400">
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
      <EnvelopeCard className="text-sm text-ink-300 dark:text-slate-400">
        {copy.loading}
      </EnvelopeCard>
    );
  }

  if (error) {
    return (
      <EnvelopeCard className="text-sm text-risk-500 ring-risk-100" tone="risk">
        <p>{error}</p>
      </EnvelopeCard>
    );
  }

  const inputClass =
    "w-full max-w-md rounded-full bg-cream-50 px-4 py-2 text-sm text-ink-900 ring-1 ring-ink-100/80 outline-none transition focus:ring-2 focus:ring-sage-500/60 placeholder:text-ink-300 dark:bg-slate-800 dark:text-slate-100 dark:ring-slate-700";

  return (
    <div className="space-y-6">
      <SectionLabel>组合驾驶舱</SectionLabel>
      <EnvelopeCard className="animate-fade-up">
        <div className="flex flex-col gap-2 sm:flex-row sm:items-end sm:justify-between">
          <div>
            <h1 className="text-2xl font-extrabold text-ink-900 dark:text-slate-100">
              {copy.title}
            </h1>
            <p className="mt-1 text-sm text-ink-300 dark:text-slate-400">
              {copy.subtitle}
            </p>
          </div>
        </div>
        <dl className="mt-6 grid grid-cols-2 gap-x-6 gap-y-4 md:grid-cols-5">
          <MetricBlock compact label={copy.kFunds}     value={formatNumberForLanguage(kpis.fundCount, language)} />
          <MetricBlock compact label={copy.kCompanies} value={formatNumberForLanguage(kpis.companyCount, language)} />
          <MetricBlock compact label={copy.kLive}      value={formatNumberForLanguage(kpis.liveCount, language)} tone={kpis.liveCount > 0 ? "positive" : "neutral"} />
          <MetricBlock compact label={copy.kAum}       value={formatMoneyForDisplay(kpis.totalAssets, "USD", displayCurrency, language)} />
          <MetricBlock compact label={copy.kAvgNav}    value={formatNumberForLanguage(kpis.avgNav, language, { minimumFractionDigits: 4, maximumFractionDigits: 4 })} />
        </dl>
      </EnvelopeCard>

      <EnvelopeCard className="animate-fade-up">
        <div className="mb-4 flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <input
            type="search"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder={copy.searchPh}
            className={inputClass}
          />
          <select
            value={modeFilter}
            onChange={(e) => setModeFilter(e.target.value as "all" | "live" | "simulation")}
            className="rounded-full bg-cream-50 px-4 py-2 text-sm text-ink-900 ring-1 ring-ink-100/80 outline-none transition focus:ring-2 focus:ring-sage-500/60 dark:bg-slate-800 dark:text-slate-100 dark:ring-slate-700"
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
            <div className="rounded-2xl bg-cream-50 p-10 text-center text-sm text-ink-300 ring-1 ring-dashed ring-ink-200 dark:bg-slate-800 dark:text-slate-400 dark:ring-slate-600">
              <p className="font-semibold text-ink-700 dark:text-slate-200">{copy.emptyTitle}</p>
              <p className="mt-1">{copy.emptyDescription}</p>
            </div>
          }
        />
      </EnvelopeCard>
    </div>
  );
};

export default MultiFundOverview;
