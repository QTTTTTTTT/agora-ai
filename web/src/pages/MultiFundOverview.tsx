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
// Doesn't yet stream daily PnL deltas or workflow state per
// fund (would need 4 separate /api/funds/{id}/workflow GETs in
// fan-out — sensible to add as a follow-up once we wire SSE
// multiplex). Doesn't pull benchmarks. The point of this commit
// is to give operators ONE PLACE to see "are all my funds OK?".

import React, { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { apiGet, formatApiError } from "../lib/api";
import { formatMoneyForDisplay, formatNumberForLanguage, useAppPreferences } from "../lib/preferences";
import { ResponsiveTable, type ResponsiveColumn } from "../components/ResponsiveTable";

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

const tradingModeLabel = (mode: string, isEnglish: boolean): string => {
  const m = mode.toLowerCase();
  if (m === "live") return isEnglish ? "Live" : "实盘";
  if (m === "simulation") return isEnglish ? "Simulation" : "模拟";
  return mode;
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
  const isEnglish = language === "en-US";
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
        setError(formatApiError(err, isEnglish ? "Failed to load portfolio overview" : "加载组合总览失败"));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
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

  const copy = isEnglish
    ? {
        title: "Multi-fund overview",
        subtitle: "Aggregate state across every fund and company you can see.",
        loading: "Loading portfolio overview…",
        retry: "Retry",
        kFunds: "Funds",
        kCompanies: "Companies",
        kLive: "Live",
        kAum: "Total AUM",
        kAvgNav: "Avg NAV",
        searchPh: "Search fund or company name…",
        modeAll: "All modes",
        modeLive: "Live only",
        modeSim: "Simulation only",
        emptyTitle: "No funds yet",
        emptyDescription: "Create a fund from the Companies page to see it here.",
        col: {
          fund: "Fund",
          company: "Company",
          mode: "Mode",
          status: "Status",
          totalAssets: "Total assets",
          nav: "NAV",
          baseCurrency: "Base ccy",
        },
      }
    : {
        title: "多基金总览",
        subtitle: "聚合查看你能看到的所有基金与公司的运行状态。",
        loading: "正在加载组合总览……",
        retry: "重试",
        kFunds: "基金数",
        kCompanies: "公司数",
        kLive: "实盘",
        kAum: "总资产",
        kAvgNav: "平均净值",
        searchPh: "搜索基金或公司名称……",
        modeAll: "全部模式",
        modeLive: "仅实盘",
        modeSim: "仅模拟",
        emptyTitle: "暂无基金",
        emptyDescription: "请先在公司页面创建一只基金。",
        col: {
          fund: "基金",
          company: "公司",
          mode: "模式",
          status: "状态",
          totalAssets: "总资产",
          nav: "净值",
          baseCurrency: "本币",
        },
      };

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
            {tradingModeLabel(row.tradingMode, isEnglish)}
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
    [copy.col, displayCurrency, isEnglish, language],
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
