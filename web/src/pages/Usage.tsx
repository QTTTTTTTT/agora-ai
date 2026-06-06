import React, { useCallback, useEffect, useMemo, useState } from "react";
import {
  Bar,
  BarChart,
  CartesianGrid,
  Legend,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import { apiGet, formatApiError } from "../lib/api";
import { useSWRFetch } from "../lib/useSWRFetch";
import {
  convertMoneyForDisplay,
  formatDateTimeForLanguage,
  formatDateValueForLanguage,
  formatMoneyForLanguage,
  formatMoneyMinorForDisplay,
  formatNumberForLanguage,
  useAppPreferences,
} from "../lib/preferences";

type TabKey = "today" | "month" | "history" | "bill";

interface DailySummary {
  user_id: string;
  summary_date: string;
  total_calls: number;
  input_tokens: number;
  output_tokens: number;
  cost_cents: number;
  price_cents: number;
  custom_key_calls: number;
  model_breakdown: Record<string, BreakdownValue>;
  step_breakdown: Record<string, BreakdownValue>;
}

interface MonthlySummary {
  user_id: string;
  year_month: string;
  total_calls: number;
  input_tokens: number;
  output_tokens: number;
  cost_cents: number;
  price_cents: number;
  custom_key_calls: number;
  model_breakdown: Record<string, BreakdownValue>;
}

interface UsageEntry {
  id: string;
  fund_id?: string;
  step_name: string;
  model_provider: string;
  model_name: string;
  input_tokens: number;
  output_tokens: number;
  cost_cents: number;
  price_cents: number;
  is_custom_key: boolean;
  created_at: string;
}

interface MonthlyBill {
  id?: string;
  user_id: string;
  year_month: string;
  plan_tier: string;
  subscription_fee: number;
  model_usage_fee: number;
  custom_key_credit: number;
  total_fee: number;
  final_amount: number;
  status: string;
}

type BreakdownValue =
  | number
  | {
      calls?: number;
      total_calls?: number;
      input_tokens?: number;
      output_tokens?: number;
      price_cents?: number;
      cost_cents?: number;
      value?: number;
    };

interface TodayUsageResponse {
  summary: DailySummary | null;
  daily_limit: number;
  remaining_calls: number;
}

interface MonthlyUsageResponse {
  summary: MonthlySummary | null;
}

interface UsageHistoryResponse {
  entries: UsageEntry[];
  total: number;
  offset: number;
  limit: number;
}

interface BillResponse {
  bill: MonthlyBill | null;
}

interface EstimateResponse {
  plan_tier: string;
  subscription_fee: number;
  current_usage_fee: number;
  estimated_usage_fee: number;
  estimated_total: number;
  days_elapsed: number;
  days_in_month: number;
  year_month: string;
}

interface BreakdownRow {
  name: string;
  value: number;
  cost: number;
  color: string;
}

const pageSize = 10;
const chartColors = ["#6366f1", "#8b5cf6", "#06b6d4", "#10b981", "#f59e0b", "#ef4444"];

function parseBreakdownValue(value: BreakdownValue): number {
  if (typeof value === "number") {
    return value;
  }
  return value.total_calls ?? value.calls ?? value.value ?? 0;
}

function parseBreakdownCost(value: BreakdownValue): number {
  if (typeof value === "number") {
    return value;
  }
  return value.price_cents ?? value.cost_cents ?? 0;
}

function humanizeValue(value?: string, emptyLabel = "-"): string {
  if (!value) {
    return emptyLabel;
  }
  return value
    .replace(/[_-]+/g, " ")
    .trim()
    .replace(/\b\w/g, (char) => char.toUpperCase());
}

function buildBreakdownRows(
  breakdown: Record<string, BreakdownValue> | undefined,
  formatName: (name: string) => string = (name) => name,
): BreakdownRow[] {
  return Object.entries(breakdown ?? {})
    .map(([name, value], index) => ({
      name: formatName(name),
      value: parseBreakdownValue(value),
      cost: parseBreakdownCost(value),
      color: chartColors[index % chartColors.length],
    }))
    .sort((a, b) => b.value - a.value);
}

const Usage: React.FC = () => {
  const { language, displayCurrency } = useAppPreferences();
  const [activeTab, setActiveTab] = useState<TabKey>("today");
  const [historyPage, setHistoryPage] = useState(0);
  const [error, setError] = useState<string | null>(null);
  // Pagination history is fetched imperatively because the offset
  // changes with user clicks and we want optimistic page-flip UX.
  // The 4 "static" snapshots (today / monthly / bill / estimate)
  // are SWR-cached so a tab flip back to /usage doesn't re-trigger
  // the whole bundle.
  const [historyData, setHistoryData] = useState<UsageHistoryResponse | null>(null);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            title: "Usage & billing",
            subtitle: "Review today and month-to-date usage, historical records, and settlement estimates to track model consumption and cost changes.",
            loading: "Loading usage data...",
            loadError: "Failed to load usage data",
            loadHistoryError: "Failed to load history",
            retry: "Retry",
            tabs: {
              today: "Today",
              month: "This month",
              history: "History",
              bill: "Bill",
            },
            todayCards: {
              totalCalls: "Total calls",
              totalTokens: "Total tokens",
              todayCost: "Today cost",
              remainingCalls: "Remaining calls",
            },
            monthlyCards: {
              totalCalls: "Monthly calls",
              totalTokens: "Monthly tokens",
              totalCost: "Monthly cost",
              estimatedTotal: "Estimated monthly total",
            },
            modelDistribution: "Model distribution",
            noModelDistribution: "There are no model calls recorded today yet. Distribution will appear here once the workflow runs.",
            stepDistribution: "Step distribution",
            noStepDistribution: "There are no step-level stats for today yet. The system will populate this after generating calls.",
            dailyTrend: "Daily usage trend",
            noDailyTrend: "There is not enough data this month to build the trend chart yet.",
            chartCalls: "Calls",
            chartCost: `Cost (${displayCurrency})`,
            modelRanking: "Model ranking",
            noModelRanking: "There is no model ranking data for this month yet.",
            rankingColumns: {
              rank: "Rank",
              model: "Model",
              calls: "Calls",
              cost: "Cost",
            },
            historySummary: {
              total: "Total",
              page: "Showing latest",
              entries: "entries",
            },
            historyColumns: {
              time: "Time",
              step: "Step",
              model: "Model",
              tokens: "Tokens",
              cost: "Cost",
            },
            customKey: "Using custom key",
            previousPage: "Previous",
            nextPage: "Next",
            noHistoryTitle: "No usage history yet",
            noHistoryDescription: "Later model calls will accumulate here so you can review each workflow step and cost change over time.",
            pageIndicator: "Page",
            billTitleFallback: "Current month",
            billLabels: {
              subscription: "Subscription fee",
              modelUsage: "Model usage",
              customKeyCredit: "Custom key credit",
              estimatedUsage: "Estimated model usage",
              payable: "Payable",
            },
            unlimited: "Unlimited",
            notRecorded: "Not recorded",
            billStatus: {
              paid: "Paid",
              overdue: "Overdue",
              failed: "Failed",
              pending: "Pending",
            },
            steps: {
              macro_brief: "Macro brief",
              plan_generation: "Plan generation",
              decision_review: "Decision review",
              trade_execution: "Trade execution",
              risk_check: "Risk check",
            },
          }
        : {
            title: "用量与账单",
            subtitle: "查看今日与本月调用情况、历史明细和结算预估，快速判断模型消耗与费用变化。",
            loading: "正在加载用量数据...",
            loadError: "加载用量数据失败",
            loadHistoryError: "加载历史记录失败",
            retry: "重试",
            tabs: {
              today: "今日",
              month: "本月",
              history: "历史",
              bill: "账单",
            },
            todayCards: {
              totalCalls: "总调用次数",
              totalTokens: "总 Token 数",
              todayCost: "今日费用",
              remainingCalls: "剩余调用",
            },
            monthlyCards: {
              totalCalls: "月调用总次数",
              totalTokens: "月 Token 总数",
              totalCost: "月费用合计",
              estimatedTotal: "预计月总费用",
            },
            modelDistribution: "模型分布",
            noModelDistribution: "今日还没有模型调用记录，后续实际运行后会在这里展示模型分布。",
            stepDistribution: "步骤分布",
            noStepDistribution: "今日还没有步骤级统计，系统产生调用后会自动补齐这里的分布图。",
            dailyTrend: "每日用量趋势",
            noDailyTrend: "本月还没有足够的数据生成趋势图，随着调用积累会自动展示日趋势。",
            chartCalls: "调用次数",
            chartCost: `费用 (${displayCurrency})`,
            modelRanking: "模型排行",
            noModelRanking: "本月还没有模型排行数据。",
            rankingColumns: {
              rank: "排名",
              model: "模型",
              calls: "调用次数",
              cost: "费用",
            },
            historySummary: {
              total: "共",
              page: "当前页展示最近",
              entries: "条",
            },
            historyColumns: {
              time: "时间",
              step: "步骤",
              model: "模型",
              tokens: "Token 数",
              cost: "费用",
            },
            customKey: "使用自带密钥",
            previousPage: "上一页",
            nextPage: "下一页",
            noHistoryTitle: "当前还没有历史调用记录",
            noHistoryDescription: "后续模型调用会沉淀在这里，方便你按时间回看每次步骤执行与费用变化。",
            pageIndicator: "第",
            billTitleFallback: "当前月",
            billLabels: {
              subscription: "订阅费",
              modelUsage: "模型费用",
              customKeyCredit: "自带密钥抵扣",
              estimatedUsage: "预估模型费用",
              payable: "应付金额",
            },
            unlimited: "不限",
            notRecorded: "未记录",
            billStatus: {
              paid: "已支付",
              overdue: "已逾期",
              failed: "结算失败",
              pending: "待结算",
            },
            steps: {
              macro_brief: "宏观简报生成",
              plan_generation: "计划生成",
              decision_review: "决策复核",
              trade_execution: "交易执行",
              risk_check: "风险检查",
            },
          },
    [displayCurrency, language],
  );

  const tabs = useMemo(
    () => [
      { key: "today" as const, label: copy.tabs.today },
      { key: "month" as const, label: copy.tabs.month },
      { key: "history" as const, label: copy.tabs.history },
      { key: "bill" as const, label: copy.tabs.bill },
    ],
    [copy.tabs],
  );

  const formatCents = useCallback(
    (value: number) => formatMoneyMinorForDisplay(value, "CNY", displayCurrency, language),
    [displayCurrency, language],
  );

  const formatCount = useCallback((value: number) => formatNumberForLanguage(value, language), [language]);

  const formatTokens = useCallback(
    (value: number) => {
      if (value >= 1_000_000) {
        return `${formatNumberForLanguage(value / 1_000_000, language, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}M`;
      }
      if (value >= 1_000) {
        return `${formatNumberForLanguage(value / 1_000, language, { minimumFractionDigits: 1, maximumFractionDigits: 1 })}K`;
      }
      return formatNumberForLanguage(value, language);
    },
    [language],
  );

  const formatMonthLabel = useCallback(
    (value: string) => {
      const [year, month] = value.split("-");
      if (!year || !month) {
        return value;
      }
      return formatDateValueForLanguage(new Date(Number(year), Number(month) - 1, 1), language, {
        year: "numeric",
        month: "long",
      });
    },
    [language],
  );

  const stepLabel = useCallback(
    (value: string) => copy.steps[value as keyof typeof copy.steps] ?? value.split("_").join(" "),
    [copy],
  );

  const modelLabel = useCallback((provider: string, modelName: string): string => {
    const providerLabelMap: Record<string, string> = {
      claude: "Anthropic",
      openai: "OpenAI",
      deepseek: "DeepSeek",
      qwen: "Qwen",
    };
    const providerLabel = providerLabelMap[provider] ?? humanizeValue(provider, "");
    return providerLabel ? `${providerLabel} · ${modelName}` : modelName;
  }, []);

  const billStatusMeta = useCallback(
    (status?: string) => {
      const normalized = (status ?? "pending").toLowerCase();
      const badgeMap: Record<string, string> = {
        paid: "bg-green-100 text-green-700",
        overdue: "bg-red-100 text-red-700",
        failed: "bg-red-100 text-red-700",
        pending: "bg-amber-100 text-amber-700",
      };
      return {
        label: copy.billStatus[normalized as keyof typeof copy.billStatus] ?? humanizeValue(status, copy.billStatus.pending),
        badge: badgeMap[normalized] ?? "bg-gray-100 text-gray-700",
      };
    },
    [copy],
  );

  const loadPage = useCallback(
    async (page: number) => {
      try {
        const historyRes = await apiGet<UsageHistoryResponse>(`/api/usage/history?offset=${page * pageSize}&limit=${pageSize}`);
        setHistoryData(historyRes);
      } catch (err) {
        setError(formatApiError(err, copy.loadHistoryError));
      }
    },
    [copy.loadHistoryError],
  );

  // SWR cache for the static usage bundle. Five endpoints fetched
  // in parallel; the page treats them as one logical snapshot, so
  // we wrap them in a single Promise.all and key on a fixed slug.
  // ttl=60s — Usage figures tick at sub-minute frequency on the
  // server (after each LLM call), so a 60s window is the right
  // tradeoff between freshness and "tab-flip" responsiveness.
  type UsageBundle = {
    today: TodayUsageResponse;
    monthly: MonthlyUsageResponse;
    bill: BillResponse;
    estimate: EstimateResponse;
    history: UsageHistoryResponse;
  };
  const usageSwr = useSWRFetch<UsageBundle>(
    "usage/p0",
    async () => {
      const [today, monthly, bill, estimate, history] = await Promise.all([
        apiGet<TodayUsageResponse>("/api/usage/today"),
        apiGet<MonthlyUsageResponse>("/api/usage/monthly"),
        apiGet<BillResponse>("/api/usage/bill"),
        apiGet<EstimateResponse>("/api/usage/estimate"),
        apiGet<UsageHistoryResponse>(`/api/usage/history?offset=0&limit=${pageSize}`),
      ]);
      return { today, monthly, bill, estimate, history };
    },
    { ttlMs: 60_000 },
  );

  const todayData = usageSwr.data?.today ?? null;
  const monthlyData = usageSwr.data?.monthly ?? null;
  const billData = usageSwr.data?.bill ?? null;
  const estimate = usageSwr.data?.estimate ?? null;
  // effectiveHistory is the source-of-truth for what the History
  // tab renders. On page 0 we seed from SWR's cached snapshot
  // (free re-mount, no network); on later pages we read from the
  // imperatively-managed state, which loadPage updates.
  const effectiveHistory = historyPage === 0 ? usageSwr.data?.history ?? historyData : historyData;
  const loading = !usageSwr.data && usageSwr.isLoading;
  const swrError = usageSwr.error ? formatApiError(usageSwr.error, copy.loadError) : null;
  const loadData = useCallback(async () => {
    setError(null);
    setHistoryPage(0);
    try {
      await usageSwr.mutate();
    } catch (err) {
      setError(formatApiError(err, copy.loadError));
    }
  }, [copy.loadError, usageSwr]);

  useEffect(() => {
    if (historyPage === 0) {
      return;
    }
    void loadPage(historyPage);
  }, [historyPage, loadPage]);

  const todaySummary = todayData?.summary;
  const monthlySummary = monthlyData?.summary ?? null;
  const todayModelDist = useMemo(() => buildBreakdownRows(todaySummary?.model_breakdown), [todaySummary]);
  const todayStepDist = useMemo(() => buildBreakdownRows(todaySummary?.step_breakdown, stepLabel), [stepLabel, todaySummary]);
  const monthlyModelRank = useMemo(() => buildBreakdownRows(monthlySummary?.model_breakdown), [monthlySummary]);
  const dailyTrend = useMemo(() => {
    if (!monthlySummary || !estimate || estimate.days_elapsed <= 0) {
      return [];
    }

    const avgCalls = monthlySummary.total_calls / estimate.days_elapsed;
    const converted = convertMoneyForDisplay(monthlySummary.price_cents / estimate.days_elapsed / 100, "CNY", displayCurrency);

    return Array.from({ length: estimate.days_elapsed }, (_, index) => ({
      date: language === "en-US" ? `Day ${index + 1}` : `${index + 1}日`,
      calls: Math.round(avgCalls),
      cost: Number(converted.amount.toFixed(2)),
    }));
  }, [displayCurrency, estimate, language, monthlySummary]);
  const totalHistoryPages = effectiveHistory ? Math.max(1, Math.ceil(effectiveHistory.total / pageSize)) : 1;
  const bill = billData?.bill ?? null;
  const billStatus = billStatusMeta(bill?.status);

  // Combined error: SWR (network) takes precedence on first paint;
  // local error reflects user-driven actions (loadPage, manual
  // retry click) and overrides once it fires. Resetting to null
  // when both clear keeps the banner from sticking around.
  const displayError = error ?? swrError;

  if (loading) {
    return (
      <div className="space-y-4">
        <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
        <div className="rounded-lg border border-gray-200 bg-white p-6 text-sm text-gray-500">{copy.loading}</div>
      </div>
    );
  }

  if (displayError) {
    return (
      <div className="space-y-4">
        <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
        <div className="rounded-lg border border-red-200 bg-red-50 p-6 text-sm text-red-700">
          <p>{displayError}</p>
          <button onClick={() => void loadData()} className="mt-4 rounded-lg bg-red-600 px-4 py-2 text-sm font-medium text-white hover:bg-red-700">
            {copy.retry}
          </button>
        </div>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
        <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
        <p className="mt-2 text-sm text-gray-500">{copy.subtitle}</p>
      </div>

      <div className="flex w-fit flex-wrap gap-1 rounded-lg bg-gray-100 p-1">
        {tabs.map((tab) => (
          <button
            key={tab.key}
            onClick={() => setActiveTab(tab.key)}
            className={`rounded-md px-4 py-2 text-sm font-medium transition-colors ${
              activeTab === tab.key ? "bg-white text-indigo-700 shadow-sm" : "text-gray-600 hover:text-gray-900"
            }`}
          >
            {tab.label}
          </button>
        ))}
      </div>

      {activeTab === "today" ? (
        <div className="space-y-6">
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-4">
            <div className="rounded-xl border border-gray-200 bg-white p-5">
              <p className="text-sm text-gray-500">{copy.todayCards.totalCalls}</p>
              <p className="text-3xl font-bold text-gray-900">{formatCount(todaySummary?.total_calls ?? 0)}</p>
            </div>
            <div className="rounded-xl border border-gray-200 bg-white p-5">
              <p className="text-sm text-gray-500">{copy.todayCards.totalTokens}</p>
              <p className="text-3xl font-bold text-gray-900">{formatTokens((todaySummary?.input_tokens ?? 0) + (todaySummary?.output_tokens ?? 0))}</p>
            </div>
            <div className="rounded-xl border border-gray-200 bg-white p-5">
              <p className="text-sm text-gray-500">{copy.todayCards.todayCost}</p>
              <p className="text-3xl font-bold text-emerald-700">{formatCents(todaySummary?.price_cents ?? 0)}</p>
            </div>
            <div className="rounded-xl border border-gray-200 bg-white p-5">
              <p className="text-sm text-gray-500">{copy.todayCards.remainingCalls}</p>
              <p className="text-3xl font-bold text-gray-900">
                {todayData?.daily_limit ? formatCount(todayData.remaining_calls) : copy.unlimited}
              </p>
            </div>
          </div>

          <div className="rounded-xl border border-gray-200 bg-white p-5">
            <h3 className="mb-4 text-sm font-semibold text-gray-700">{copy.modelDistribution}</h3>
            {todayModelDist.length === 0 ? (
              <p className="text-sm text-gray-500">{copy.noModelDistribution}</p>
            ) : (
              <div className="space-y-3">
                {todayModelDist.map((item) => (
                  <div key={item.name} className="flex items-center gap-3">
                    <span className="w-28 text-sm text-gray-600">{item.name}</span>
                    <div className="h-6 flex-1 overflow-hidden rounded-full bg-gray-100">
                      <div
                        className="h-full rounded-full"
                        style={{
                          width: `${todaySummary?.total_calls ? (item.value / todaySummary.total_calls) * 100 : 0}%`,
                          backgroundColor: item.color,
                        }}
                      />
                    </div>
                    <span className="w-12 text-right text-sm font-medium text-gray-700">{formatCount(item.value)}</span>
                  </div>
                ))}
              </div>
            )}
          </div>

          <div className="rounded-xl border border-gray-200 bg-white p-5">
            <h3 className="mb-4 text-sm font-semibold text-gray-700">{copy.stepDistribution}</h3>
            {todayStepDist.length === 0 ? (
              <p className="text-sm text-gray-500">{copy.noStepDistribution}</p>
            ) : (
              <ResponsiveContainer width="100%" height={220}>
                <BarChart data={todayStepDist} layout="vertical" margin={{ left: 80, right: 20 }}>
                  <CartesianGrid strokeDasharray="3 3" />
                  <XAxis type="number" tickFormatter={(value: number) => formatCount(value)} />
                  <YAxis type="category" dataKey="name" />
                  <Tooltip formatter={(value: number) => formatCount(value)} />
                  <Bar dataKey="value" fill="#6366f1" radius={[0, 4, 4, 0]} />
                </BarChart>
              </ResponsiveContainer>
            )}
          </div>
        </div>
      ) : null}

      {activeTab === "month" ? (
        <div className="space-y-6">
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-4">
            <div className="rounded-xl border border-gray-200 bg-white p-5">
              <p className="text-sm text-gray-500">{copy.monthlyCards.totalCalls}</p>
              <p className="text-2xl font-bold text-gray-900">{formatCount(monthlySummary?.total_calls ?? 0)}</p>
            </div>
            <div className="rounded-xl border border-gray-200 bg-white p-5">
              <p className="text-sm text-gray-500">{copy.monthlyCards.totalTokens}</p>
              <p className="text-2xl font-bold text-gray-900">{formatTokens((monthlySummary?.input_tokens ?? 0) + (monthlySummary?.output_tokens ?? 0))}</p>
            </div>
            <div className="rounded-xl border border-gray-200 bg-white p-5">
              <p className="text-sm text-gray-500">{copy.monthlyCards.totalCost}</p>
              <p className="text-2xl font-bold text-emerald-700">{formatCents(monthlySummary?.price_cents ?? 0)}</p>
            </div>
            <div className="rounded-xl border border-gray-200 bg-white p-5">
              <p className="text-sm text-gray-500">{copy.monthlyCards.estimatedTotal}</p>
              <p className="text-2xl font-bold text-gray-900">{formatCents(estimate?.estimated_total ?? 0)}</p>
            </div>
          </div>

          <div className="rounded-xl border border-gray-200 bg-white p-5">
            <h3 className="mb-4 text-sm font-semibold text-gray-700">{copy.dailyTrend}</h3>
            {dailyTrend.length === 0 ? (
              <p className="text-sm text-gray-500">{copy.noDailyTrend}</p>
            ) : (
              <ResponsiveContainer width="100%" height={280}>
                <LineChart data={dailyTrend}>
                  <CartesianGrid strokeDasharray="3 3" />
                  <XAxis dataKey="date" tick={{ fontSize: 12 }} />
                  <YAxis yAxisId="left" tick={{ fontSize: 12 }} tickFormatter={(value: number) => formatCount(value)} />
                  <YAxis yAxisId="right" orientation="right" tick={{ fontSize: 12 }} tickFormatter={(value: number) => formatNumberForLanguage(value, language, { maximumFractionDigits: 2 })} />
                  <Tooltip
                    formatter={(value: number, name: string) =>
                      name === copy.chartCost
                        ? formatMoneyForLanguage(value, displayCurrency, language)
                        : formatCount(value)
                    }
                  />
                  <Legend />
                  <Line yAxisId="left" type="monotone" dataKey="calls" stroke="#6366f1" name={copy.chartCalls} strokeWidth={2} dot={false} />
                  <Line yAxisId="right" type="monotone" dataKey="cost" stroke="#10b981" name={copy.chartCost} strokeWidth={2} dot={false} />
                </LineChart>
              </ResponsiveContainer>
            )}
          </div>

          <div className="rounded-xl border border-gray-200 bg-white p-5">
            <h3 className="mb-4 text-sm font-semibold text-gray-700">{copy.modelRanking}</h3>
            {monthlyModelRank.length === 0 ? (
              <p className="text-sm text-gray-500">{copy.noModelRanking}</p>
            ) : (
              <table className="w-full text-sm">
                <thead className="border-b text-gray-500">
                  <tr>
                    <th className="py-2 text-left">{copy.rankingColumns.rank}</th>
                    <th className="py-2 text-left">{copy.rankingColumns.model}</th>
                    <th className="py-2 text-right">{copy.rankingColumns.calls}</th>
                    <th className="py-2 text-right">{copy.rankingColumns.cost}</th>
                  </tr>
                </thead>
                <tbody>
                  {monthlyModelRank.map((item, index) => (
                    <tr key={item.name} className="border-b border-gray-50">
                      <td className="py-2 font-medium text-gray-400">#{formatCount(index + 1)}</td>
                      <td className="py-2 font-medium text-gray-800">{item.name}</td>
                      <td className="py-2 text-right text-gray-600">{formatCount(item.value)}</td>
                      <td className="py-2 text-right font-medium text-emerald-700">{formatCents(item.cost)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </div>
      ) : null}

      {activeTab === "history" ? (
        <div className="overflow-hidden rounded-xl border border-gray-200 bg-white">
          {effectiveHistory && effectiveHistory.entries.length > 0 ? (
            <>
              <div className="flex items-center justify-between border-b border-gray-100 px-4 py-3 text-sm text-gray-500">
                <span>{copy.historySummary.total} {formatCount(effectiveHistory.total)} {copy.historySummary.entries}</span>
                <span>{copy.historySummary.page} {formatCount(effectiveHistory.entries.length)} {copy.historySummary.entries}</span>
              </div>
              <table className="w-full text-sm">
                <thead className="bg-gray-50 text-gray-600">
                  <tr>
                    <th className="px-4 py-3 text-left">{copy.historyColumns.time}</th>
                    <th className="px-4 py-3 text-left">{copy.historyColumns.step}</th>
                    <th className="px-4 py-3 text-left">{copy.historyColumns.model}</th>
                    <th className="px-4 py-3 text-right">{copy.historyColumns.tokens}</th>
                    <th className="px-4 py-3 text-right">{copy.historyColumns.cost}</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-gray-50">
                  {effectiveHistory.entries.map((row) => (
                    <tr key={row.id} className="hover:bg-gray-50">
                      <td className="px-4 py-3 text-gray-500">{formatDateTimeForLanguage(row.created_at, language)}</td>
                      <td className="px-4 py-3 text-gray-700">{stepLabel(row.step_name)}</td>
                      <td className="px-4 py-3 text-gray-700">
                        <div>
                          <p>{modelLabel(row.model_provider, row.model_name)}</p>
                          {row.is_custom_key ? <p className="mt-1 text-xs text-emerald-600">{copy.customKey}</p> : null}
                        </div>
                      </td>
                      <td className="px-4 py-3 text-right text-gray-600">{formatCount(row.input_tokens + row.output_tokens)}</td>
                      <td className="px-4 py-3 text-right font-medium text-emerald-700">{formatCents(row.price_cents)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              <div className="flex items-center justify-between border-t px-4 py-3">
                <span className="text-sm text-gray-500">
                  {copy.historySummary.total} {formatCount(effectiveHistory.total)} {copy.historySummary.entries}，{copy.pageIndicator} {formatCount(historyPage + 1)}/{formatCount(totalHistoryPages)}
                </span>
                <div className="flex gap-2">
                  <button
                    disabled={historyPage === 0}
                    onClick={() => setHistoryPage((page) => Math.max(0, page - 1))}
                    className="rounded border border-gray-300 px-3 py-1 text-sm disabled:opacity-40"
                  >
                    {copy.previousPage}
                  </button>
                  <button
                    disabled={historyPage + 1 >= totalHistoryPages}
                    onClick={() => setHistoryPage((page) => page + 1)}
                    className="rounded border border-gray-300 px-3 py-1 text-sm disabled:opacity-40"
                  >
                    {copy.nextPage}
                  </button>
                </div>
              </div>
            </>
          ) : (
            <div className="p-6 text-sm text-gray-500">
              <p className="font-medium text-gray-700">{copy.noHistoryTitle}</p>
              <p className="mt-2">{copy.noHistoryDescription}</p>
            </div>
          )}
        </div>
      ) : null}

      {activeTab === "bill" ? (
        <div className="space-y-4">
          <div className="rounded-xl border border-gray-200 bg-white p-6">
            <div className="mb-4 flex items-center justify-between">
              <h3 className="text-lg font-bold text-gray-900">{formatMonthLabel(bill?.year_month ?? estimate?.year_month ?? copy.billTitleFallback)}</h3>
              <span className={`rounded-full px-3 py-1 text-xs font-medium ${billStatus.badge}`}>{billStatus.label}</span>
            </div>
            <div className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-5">
              <div>
                <p className="text-gray-500">{copy.billLabels.subscription}</p>
                <p className="text-lg font-semibold text-gray-800">{formatCents(bill?.subscription_fee ?? estimate?.subscription_fee ?? 0)}</p>
              </div>
              <div>
                <p className="text-gray-500">{copy.billLabels.modelUsage}</p>
                <p className="text-lg font-semibold text-gray-800">{formatCents(bill?.model_usage_fee ?? estimate?.current_usage_fee ?? 0)}</p>
              </div>
              <div>
                <p className="text-gray-500">{copy.billLabels.customKeyCredit}</p>
                <p className="text-lg font-semibold text-green-600">-{formatCents(bill?.custom_key_credit ?? 0)}</p>
              </div>
              <div>
                <p className="text-gray-500">{copy.billLabels.estimatedUsage}</p>
                <p className="text-lg font-semibold text-gray-800">{formatCents(estimate?.estimated_usage_fee ?? 0)}</p>
              </div>
              <div>
                <p className="text-gray-500">{copy.billLabels.payable}</p>
                <p className="text-xl font-bold text-indigo-700">{formatCents(bill?.final_amount ?? estimate?.estimated_total ?? 0)}</p>
              </div>
            </div>
          </div>
        </div>
      ) : null}
    </div>
  );
};

export default Usage;
