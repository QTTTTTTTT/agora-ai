import React, { useEffect, useMemo, useState } from "react";
import { Link, NavLink, Outlet, useLocation, useNavigate, useParams } from "react-router-dom";
import { ApiError, apiGet, formatApiError, getStoredSession, logoutSession } from "../lib/api";
import { formatMoneyForDisplay, formatNumberForLanguage, useAppPreferences } from "../lib/preferences";
import { AutoExecuteHeaderBadge, type AutoExecuteConfig } from "./AutoExecuteControls";

interface NavItem {
  key: string;
  label: string;
  to: string;
  icon: React.ReactNode;
}

interface FundSection {
  key: string;
  label: string;
  icon: React.ReactNode;
}

interface Fund {
  id: string;
  companyId: string;
  name: string;
  description?: string;
  tradingMode: "simulation" | "live";
  initialCapital: number;
  currentCapital: number;
  totalAssets: number;
  nav: number;
  status: string;
  baseCurrency?: string;
  // Canonical market code (e.g. "a_share", "us_equity"). Surfaced
  // so the auto-execute settings modal can render a slot preview
  // anchored to the right trading hours.
  market?: string;
  autoExecute?: AutoExecuteConfig | null;
  researchTier?: string;
}

interface Company {
  id: string;
  ownerUserId: string;
  name: string;
  description?: string;
}

interface WorkflowStatus {
  fundId: string;
  tradingDate?: string;
  state: string;
  step: string;
  startedAt?: string;
}

const icons = {
  chart: (
    <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.5" className="h-5 w-5">
      <rect x="2" y="10" width="3" height="8" rx="0.5" />
      <rect x="7" y="6" width="3" height="12" rx="0.5" />
      <rect x="12" y="3" width="3" height="15" rx="0.5" />
      <rect x="17" y="8" width="0" height="0" />
    </svg>
  ),
  people: (
    <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.5" className="h-5 w-5">
      <circle cx="7" cy="6" r="2.5" />
      <path d="M2 16c0-2.5 2-4.5 5-4.5s5 2 5 4.5" />
      <circle cx="14" cy="6" r="2" />
      <path d="M14.5 11.5c2 .2 3.5 1.8 3.5 3.5" />
    </svg>
  ),
  clipboard: (
    <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.5" className="h-5 w-5">
      <rect x="4" y="3" width="12" height="15" rx="1.5" />
      <path d="M7 2.5h6v2H7z" />
      <path d="M7 9h6M7 12h4" />
    </svg>
  ),
  split: (
    <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.5" className="h-5 w-5">
      <path d="M10 2v16M3 5h4M3 10h4M3 15h4M13 5h4M13 10h4M13 15h4" />
    </svg>
  ),
  brain: (
    <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.5" className="h-5 w-5">
      <path d="M10 18V10" />
      <path d="M10 10C10 7 7.5 4 5 4S2 6.5 2 8s1 3 3 3h5" />
      <path d="M10 10c0-3 2.5-6 5-6s3 2.5 3 4-1 3-3 3h-5" />
      <circle cx="6" cy="7" r="0.8" fill="currentColor" stroke="none" />
      <circle cx="14" cy="7" r="0.8" fill="currentColor" stroke="none" />
    </svg>
  ),
  list: (
    <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.5" className="h-5 w-5">
      <path d="M4 5h12M4 10h12M4 15h12" />
      <circle cx="2" cy="5" r="0.8" fill="currentColor" stroke="none" />
      <circle cx="2" cy="10" r="0.8" fill="currentColor" stroke="none" />
      <circle cx="2" cy="15" r="0.8" fill="currentColor" stroke="none" />
    </svg>
  ),
  gear: (
    <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.5" className="h-5 w-5">
      <circle cx="10" cy="10" r="3" />
      <path d="M10 1.5v2M10 16.5v2M1.5 10h2M16.5 10h2M3.4 3.4l1.4 1.4M15.2 15.2l1.4 1.4M3.4 16.6l1.4-1.4M15.2 4.8l1.4-1.4" />
    </svg>
  ),
  menu: (
    <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="2" className="h-5 w-5">
      <path d="M3 5h14M3 10h14M3 15h14" />
    </svg>
  ),
  chevronLeft: (
    <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="2" className="h-5 w-5">
      <path d="M13 4l-6 6 6 6" />
    </svg>
  ),
  chevronRight: (
    <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="2" className="h-5 w-5">
      <path d="M7 4l6 6-6 6" />
    </svg>
  ),
};

const statusColors: Record<string, string> = {
  running: "bg-green-500",
  paused: "bg-yellow-400",
  rejected: "bg-red-500",
  failed: "bg-red-500",
  cancelled: "bg-slate-400",
  completed: "bg-blue-500",
  idle: "bg-gray-400",
};

function workflowStateLabel(value: string | undefined, labels: Record<string, string>, fallback: string): string {
  return labels[value ?? ""] ?? fallback;
}

function workflowStepLabel(value: string | undefined, labels: Record<string, string>, fallback: string): string {
  if (!value) {
    return fallback;
  }
  return labels[value] ?? value.split("_").join(" ");
}

function useBreadcrumb(
  fundName: string,
  companyName: string,
  pageLabels: Record<string, string>,
  defaultPageLabel: string,
) {
  const { pathname } = useLocation();
  const segments = pathname.split("/").filter(Boolean);
  const pageSlug = segments[2] ?? "";
  const pageLabel = pageLabels[pageSlug] ?? defaultPageLabel;
  return { pageLabel, company: companyName, fund: fundName };
}

const FundLayout: React.FC = () => {
  const navigate = useNavigate();
  const { fundId } = useParams<{ fundId: string }>();
  const { language, displayCurrency } = useAppPreferences();
  const [collapsed, setCollapsed] = useState(false);
  const [mobileOpen, setMobileOpen] = useState(false);
  const [fund, setFund] = useState<Fund | null>(null);
  const [workflow, setWorkflow] = useState<WorkflowStatus | null>(null);
  const [companyName, setCompanyName] = useState("");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [reloadKey, setReloadKey] = useState(0);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            companiesFallback: "Companies",
            missingFundId: "Missing fundId",
            loadFundError: "Failed to load fund information",
            loadingFundName: "Loading fund...",
            fundNotFound: "Fund not found",
            notLoggedIn: "Not signed in",
            live: "Live",
            simulation: "Simulation",
            expandSidebar: "Expand sidebar",
            collapseSidebar: "Collapse sidebar",
            openSidebar: "Open sidebar",
            backToCompanies: "Back to companies",
            unitNav: "NAV",
            totalAssets: "Total assets",
            currentUser: "Current user",
            accountSecurity: "Account & security",
            logout: "Log out",
            loadingFundInfo: "Loading fund information...",
            retry: "Retry",
            defaultPageLabel: "Overview",
            workflowIdle: "Idle",
            workflowNotStarted: "Not started",
            navLabels: {
              "": "Dashboard",
              performance: "Performance",
              team: "Team",
              decisions: "Decision Center",
              compare: "A/B Compare",
              "forward-gate": "Forward Gate",
              backtests: "Backtest Lab",
              promotions: "Promotions",
              learning: "Agent Learning",
              lineage: "Lineage",
              memory: "Memory Center",
              trades: "Trades",
              "cash-ledger": "Cash ledger",
              subscription: "Subscription",
              models: "Model Config",
              usage: "Usage & Billing",
              audit: "Audit Log",
              settings: "Fund Settings",
            },
            workflowStates: {
              running: "Running",
              paused: "Paused",
              rejected: "Rejected",
              failed: "Failed",
              cancelled: "Cancelled",
              completed: "Completed",
              idle: "Idle",
            },
            workflowSteps: {
              not_started: "Not started",
              macro_brief: "Macro brief",
              research_parallel: "Research",
              quant_signals: "Quant signals",
              roundtable: "Roundtable",
              pm_plan: "PM plan",
              risk_review: "Risk review",
              user_approval: "User approval",
              trade_execution: "Trade execution",
              settlement: "Settlement",
              daily_review: "Daily review",
            },
          }
        : {
            companiesFallback: "公司列表",
            missingFundId: "缺少 fundId",
            loadFundError: "加载基金信息失败",
            loadingFundName: "正在加载基金...",
            fundNotFound: "未找到基金",
            notLoggedIn: "未登录",
            live: "实盘",
            simulation: "模拟",
            expandSidebar: "展开侧边栏",
            collapseSidebar: "收起侧边栏",
            openSidebar: "打开侧边栏",
            backToCompanies: "返回公司列表",
            unitNav: "单位净值",
            totalAssets: "总资产",
            currentUser: "当前用户",
            accountSecurity: "账号与安全",
            logout: "退出登录",
            loadingFundInfo: "正在加载基金信息...",
            retry: "重试",
            defaultPageLabel: "总览",
            workflowIdle: "空闲",
            workflowNotStarted: "尚未启动",
            navLabels: {
              "": "基金总览",
              performance: "业绩中心",
              team: "团队管理",
              decisions: "决策中心",
              compare: "A/B 对比",
              "forward-gate": "Forward 准入",
              backtests: "回测实验室",
              promotions: "策略升级",
              learning: "Agent 学习",
              lineage: "血缘图",
              memory: "记忆中心",
              trades: "交易记录",
              "cash-ledger": "资金流水",
              subscription: "订阅管理",
              models: "模型配置",
              usage: "用量与账单",
              audit: "审计日志",
              settings: "基金设置",
            },
            workflowStates: {
              running: "运行中",
              paused: "已暂停",
              rejected: "已拒绝",
              failed: "运行失败",
              cancelled: "已取消",
              completed: "已完成",
              idle: "空闲",
            },
            workflowSteps: {
              not_started: "尚未启动",
              macro_brief: "宏观简报",
              research_parallel: "研究并行",
              quant_signals: "量化信号",
              roundtable: "圆桌讨论",
              pm_plan: "组合经理计划",
              risk_review: "风控复核",
              user_approval: "用户审批",
              trade_execution: "交易执行",
              settlement: "结算落账",
              daily_review: "日终复盘",
            },
          },
    [language],
  );

  const fundSections: FundSection[] = useMemo(
    () => [
      { key: "", label: copy.navLabels[""], icon: icons.chart },
      { key: "performance", label: copy.navLabels.performance, icon: icons.chart },
      { key: "team", label: copy.navLabels.team, icon: icons.people },
      { key: "decisions", label: copy.navLabels.decisions, icon: icons.clipboard },
      { key: "compare", label: copy.navLabels.compare, icon: icons.split },
      { key: "forward-gate", label: copy.navLabels["forward-gate"], icon: icons.chart },
      { key: "backtests", label: copy.navLabels.backtests, icon: icons.chart },
      { key: "promotions", label: copy.navLabels.promotions, icon: icons.chart },
      { key: "learning", label: copy.navLabels.learning, icon: icons.brain },
      { key: "lineage", label: copy.navLabels.lineage, icon: icons.split },
      { key: "memory", label: copy.navLabels.memory, icon: icons.brain },
      { key: "trades", label: copy.navLabels.trades, icon: icons.list },
      { key: "cash-ledger", label: copy.navLabels["cash-ledger"], icon: icons.list },
      { key: "subscription", label: copy.navLabels.subscription, icon: icons.clipboard },
      { key: "models", label: copy.navLabels.models, icon: icons.brain },
      { key: "usage", label: copy.navLabels.usage, icon: icons.list },
      { key: "audit", label: copy.navLabels.audit, icon: icons.clipboard },
      { key: "settings", label: copy.navLabels.settings, icon: icons.gear },
    ],
    [copy],
  );

  const pageLabels = useMemo(
    () => Object.fromEntries(fundSections.map((item) => [item.key, item.label])) as Record<string, string>,
    [fundSections],
  );

  useEffect(() => {
    let cancelled = false;

    async function load() {
      if (!fundId) {
        setError(copy.missingFundId);
        setLoading(false);
        return;
      }

      setLoading(true);
      setError(null);

      try {
        const [fundRes, companies, workflowRes] = await Promise.all([
          apiGet<Fund>(`/api/funds/${fundId}`),
          apiGet<Company[]>("/api/companies"),
          apiGet<WorkflowStatus>(`/api/funds/${fundId}/workflow/status`),
        ]);

        if (cancelled) {
          return;
        }

        setFund(fundRes);
        setWorkflow(workflowRes);
        const company = companies.find((item) => item.id === fundRes.companyId);
        setCompanyName(company?.name ?? copy.companiesFallback);
      } catch (err) {
        if (!cancelled) {
          if (err instanceof ApiError && err.status === 404) {
            navigate("/companies", { replace: true });
            return;
          }
          setError(formatApiError(err, copy.loadFundError));
        }
      } finally {
        if (!cancelled) {
          setLoading(false);
        }
      }
    }

    void load();
    return () => {
      cancelled = true;
    };
  }, [copy.companiesFallback, copy.loadFundError, copy.missingFundId, fundId, navigate, reloadKey]);

  const fundName = fund?.name ?? (loading ? copy.loadingFundName : copy.fundNotFound);
  const { pageLabel, company } = useBreadcrumb(fundName, companyName || copy.companiesFallback, pageLabels, copy.defaultPageLabel);
  const basePath = `/funds/${fundId}`;
  const currentSession = getStoredSession();
  const currentUserLabel = currentSession?.displayName || currentSession?.email || currentSession?.userId || copy.notLoggedIn;
  const currentUserInitial = (currentSession?.displayName || currentSession?.email || currentSession?.userId || "U").slice(0, 1).toUpperCase();

  async function handleLogout() {
    await logoutSession();
    navigate("/login", { replace: true });
  }

  const navItems: NavItem[] = useMemo(
    () =>
      fundSections.map((item) => ({
        key: item.key,
        label: item.label,
        icon: item.icon,
        to: item.key ? `${basePath}/${item.key}` : basePath,
      })),
    [basePath, fundSections],
  );

  const sidebarContent = (
    <div className="flex h-full flex-col">
      <div className="border-b border-gray-700 px-4 py-5">
        <div className="flex items-center justify-between">
          {!collapsed && (
            <div className="min-w-0">
              <h2 className="truncate text-sm font-semibold text-white">{fundName}</h2>
              {fund ? (
                <span
                  className={`mt-1 inline-block rounded-full px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide ${
                    fund.tradingMode === "live" ? "bg-green-600/20 text-green-400" : "bg-indigo-600/20 text-indigo-400"
                  }`}
                >
                  {fund.tradingMode === "live" ? copy.live : copy.simulation}
                </span>
              ) : null}
            </div>
          )}
          <button
            onClick={() => setCollapsed((prev) => !prev)}
            className="hidden items-center justify-center rounded-md p-1 text-gray-400 transition-colors hover:bg-gray-700 hover:text-white lg:flex"
            aria-label={collapsed ? copy.expandSidebar : copy.collapseSidebar}
          >
            {collapsed ? icons.chevronRight : icons.chevronLeft}
          </button>
        </div>
      </div>

      <nav className="flex-1 space-y-1 overflow-y-auto px-2 py-3">
        {navItems.map((item) => (
          <NavLink
            key={item.to}
            to={item.to}
            end={item.to === basePath}
            onClick={() => setMobileOpen(false)}
            className={({ isActive }) =>
              `flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors ${
                isActive ? "bg-indigo-600 text-white" : "text-gray-300 hover:bg-gray-700 hover:text-white"
              } ${collapsed ? "justify-center" : ""}`
            }
          >
            <span className="flex-shrink-0">{item.icon}</span>
            {!collapsed && <span className="truncate">{item.label}</span>}
          </NavLink>
        ))}
      </nav>

      <div className="border-t border-gray-700 px-4 py-3">
        <Link
          to="/companies"
          className={`flex items-center gap-2 text-xs text-gray-400 transition-colors hover:text-white ${collapsed ? "justify-center" : ""}`}
          onClick={() => setMobileOpen(false)}
        >
          <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.5" className="h-4 w-4">
            <path d="M12 16l-6-6 6-6" />
          </svg>
          {!collapsed && <span>{copy.backToCompanies}</span>}
        </Link>
      </div>
    </div>
  );

  return (
    <div className="flex h-screen overflow-hidden bg-gray-50">
      {mobileOpen ? <div className="fixed inset-0 z-30 bg-black/50 lg:hidden" onClick={() => setMobileOpen(false)} /> : null}

      <aside
        className={`fixed inset-y-0 left-0 z-40 flex-shrink-0 bg-gray-900 transition-transform duration-200 lg:static lg:translate-x-0 ${
          mobileOpen ? "translate-x-0" : "-translate-x-full"
        } ${collapsed ? "w-16" : "w-60"}`}
      >
        {sidebarContent}
      </aside>

      <div className="flex min-w-0 flex-1 flex-col overflow-hidden">
        <header className="flex items-center justify-between border-b border-gray-200 bg-white px-4 py-3 shadow-sm">
          <div className="flex min-w-0 items-center gap-3">
            <button
              onClick={() => setMobileOpen(true)}
              className="flex items-center justify-center rounded-md p-1.5 text-gray-500 hover:bg-gray-100 lg:hidden"
              aria-label={copy.openSidebar}
            >
              {icons.menu}
            </button>

            <nav className="hidden min-w-0 items-center text-sm text-gray-500 sm:flex">
              <Link to="/companies" className="truncate hover:text-indigo-600">
                {company}
              </Link>
              <span className="mx-1.5 text-gray-300">/</span>
              <Link to={basePath} className="truncate hover:text-indigo-600">
                {fundName}
              </Link>
              <span className="mx-1.5 text-gray-300">/</span>
              <span className="truncate font-medium text-gray-800">{pageLabel}</span>
            </nav>

            <span className="truncate text-sm font-medium text-gray-800 sm:hidden">{pageLabel}</span>
          </div>

          <div className="flex items-center gap-4">
            {fund ? (
              <div className="hidden items-center gap-4 text-xs md:flex">
                <div className="text-right">
                  <span className="block tracking-wide text-gray-400">{copy.unitNav}</span>
                  <span className="font-semibold text-gray-800">
                    {formatNumberForLanguage(fund.nav, language, { minimumFractionDigits: 4, maximumFractionDigits: 4 })}
                  </span>
                </div>
                <div className="text-right">
                  <span className="block tracking-wide text-gray-400">{copy.totalAssets}</span>
                  <span className="font-semibold text-gray-800">
                    {formatMoneyForDisplay(fund.totalAssets, fund.baseCurrency, displayCurrency, language)}
                  </span>
                </div>
                <div className="flex items-center gap-1.5">
                  <span className={`h-2 w-2 rounded-full ${statusColors[workflow?.state ?? "idle"] ?? statusColors.idle}`} />
                  <span className="text-gray-600">{workflowStateLabel(workflow?.state, copy.workflowStates, copy.workflowIdle)}</span>
                  <span className="text-gray-300">·</span>
                  <span className="text-gray-500">{workflowStepLabel(workflow?.step, copy.workflowSteps, copy.workflowNotStarted)}</span>
                </div>
                <AutoExecuteHeaderBadge fund={fund} onUpdated={(updated) => setFund((prev) => (prev ? { ...prev, ...updated } : prev))} />
              </div>
            ) : null}

            <div className="hidden h-6 w-px bg-gray-200 md:block" />

            <div className="flex items-center gap-3">
              <div className="hidden text-right md:block">
                <p className="text-[11px] uppercase tracking-wide text-gray-400">{copy.currentUser}</p>
                <p className="max-w-[180px] truncate text-xs font-medium text-gray-700">{currentUserLabel}</p>
              </div>
              <Link
                to="/account/security"
                title={copy.accountSecurity}
                aria-label={copy.accountSecurity}
                className="flex h-8 w-8 items-center justify-center rounded-full bg-indigo-100 text-xs font-bold text-indigo-600 transition-all hover:ring-2 hover:ring-indigo-300"
              >
                {currentUserInitial}
              </Link>
              <button
                onClick={() => void handleLogout()}
                className="rounded-lg border border-gray-200 bg-white px-3 py-2 text-xs font-medium text-gray-600 transition hover:bg-gray-50 hover:text-gray-900"
              >
                {copy.logout}
              </button>
            </div>
          </div>
        </header>

        <main className="flex-1 overflow-y-auto p-6">
          {loading ? (
            <div className="rounded-lg border border-gray-200 bg-white p-6 text-sm text-gray-500">{copy.loadingFundInfo}</div>
          ) : error ? (
            <div className="rounded-lg border border-red-200 bg-red-50 p-6 text-sm text-red-700">
              <p>{error}</p>
              <button
                onClick={() => setReloadKey((value) => value + 1)}
                className="mt-4 rounded-lg bg-red-600 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-red-700"
              >
                {copy.retry}
              </button>
            </div>
          ) : (
            <Outlet />
          )}
        </main>
      </div>
    </div>
  );
};

export default FundLayout;
