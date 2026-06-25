import React, { useCallback, useEffect, useMemo, useState } from "react";
import {
  Area,
  AreaChart,
  CartesianGrid,
  Legend,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import {
  createPaperPortfolio,
  formatApiError,
  getMasterTeamBacktest,
  getPaperNavHistory,
  listPaperOrders,
  listPaperPortfolios,
  proposePaperOrder,
  type MasterBacktestResultView,
  type MasterOperationView,
  type PaperNavPointView,
  type PaperOrderView,
  type PaperPortfolioInput,
  type PaperPortfolioView,
  type ProposeOrderInput,
} from "../lib/api";
import { formatNumberForLanguage, useAppPreferences, type AppLanguage } from "../lib/preferences";
import { ComplianceBanner } from "../components/ComplianceBanner";
import { ComplianceAckModal } from "../components/ComplianceAckModal";
import {
  formatModelAction,
  useCompliance,
  type ComplianceLocale,
} from "../lib/compliance";

/**
 * Stage 4 — PaperTrading admin page.
 *
 * This is the operator-facing surface for the tamper-evident
 * performance archive:
 *
 *   1. List of all public paper-trading portfolios (one card each).
 *   2. Per-portfolio NAV chart.
 *   3. Per-portfolio order ledger (newest-first) showing SHA256 +
 *      OpenTimestamps status.
 *   4. Forms to (a) create a new portfolio, (b) submit a manual
 *      ProposeOrder for testing the SHA256 chain end-to-end.
 *
 * Auth: behind AuthGate at the route level. No fund context
 * required — Stage 4 portfolios are admin-owned for the public
 * AlphaForge / EquityCompass surface.
 */

const COPY = {
  zh: {
    title: "Paper Trading 业绩档案",
    subtitle:
      "AI 实时发布持仓变更 → SHA256 哈希 + OpenTimestamps 链上存证。每个调仓在公开链上无法被事后修改。",
    portfoliosTitle: "组合列表",
    createPortfolioCta: "创建新组合",
    formName: "组合名",
    formStrategy: "策略",
    formMarket: "市场",
    formMarketUS: "美股",
    formMarketCN: "A 股",
    formBenchmark: "基准 (可选)",
    formInitialCapital: "初始资金",
    formSubmit: "创建",
    formSubmitting: "创建中...",
    formCancel: "取消",
    selectPrompt: "选择左侧组合查看 NAV + 订单",
    portfolioMeta: "策略 {strategy} · 市场 {market} · 基准 {benchmark}",
    metricInitial: "初始",
    metricNAV: "当前 NAV",
    metricReturn: "累计收益",
    metricCash: "现金",
    navChartTitle: "NAV 曲线",
    ordersTitle: "订单流水（SHA256 防伪）",
    ordersEmpty: "暂无订单",
    ordersAction: "动作",
    ordersSymbol: "标的",
    ordersWeight: "目标权重",
    ordersDecided: "决策时间",
    ordersHash: "Hash",
    ordersStatus: "OTS 状态",
    newOrderCta: "新增订单",
    newOrderTitle: "提交新订单（手动测试）",
    newOrderSymbol: "标的",
    newOrderAction: "动作",
    newOrderTargetWeight: "目标权重 (0-1)",
    newOrderDecidedPrice: "决策价 (可选)",
    newOrderReasoning: "AI Reasoning JSON (可选)",
    newOrderSubmit: "提交并哈希",
    newOrderSubmitting: "提交中...",
    error: "操作失败",
    loading: "加载中...",
    masterCardTitle: "大师团队因子回测 vs 美股指数",
    masterCardSubtitle:
      "10 位大师风格映射到代表性标的，月度等权再平衡，与 SPY / QQQ 同窗口对比。",
    masterMetricCumulative: "大师团队累计收益",
    masterMetricAnnual: "年化收益",
    masterMetricSharpe: "Sharpe",
    masterMetricMaxDD: "最大回撤",
    masterBenchmarkLabel: "{symbol} 累计收益",
    masterUniverseTitle: "因子映射股票池",
    masterOpsTitle: "2016-2026 操作股票流水",
    masterOpsSubtitle: "展示月度等权再平衡产生的模拟买入/卖出明细，可核对每一年策略实际操作了哪些股票。",
    masterOpsEmpty: "暂无操作流水",
    masterOpsDate: "日期",
    masterOpsMaster: "大师",
    masterOpsSymbol: "股票",
    masterOpsAction: "动作",
    masterOpsWeight: "目标权重",
    masterOpsShares: "股数变化",
    masterOpsPrice: "价格",
    masterOpsNotional: "金额",
    masterOpsAccountValue: "调仓后账户价值",
    masterOpsReturn: "累计收益",
    masterOpsCurrentValue: "价值累积路径",
    masterOpsAsOf: "截至 {date}",
    masterOpsValuePath: "{initial} → {current}",
    masterOpsShowing: "共 {total} 条操作记录",
    masterRangeLabel: "回测区间",
    masterEmpty: "暂无回测数据",
    masterError: "回测加载失败",
    masterLegendStrategy: "大师团队",
  },
  en: {
    title: "Paper Trading Performance Archive",
    subtitle:
      "AI publishes target holdings in real time → SHA256 hash + OpenTimestamps stamp. Each rebalance is publicly tamper-evident.",
    portfoliosTitle: "Portfolios",
    createPortfolioCta: "Create portfolio",
    formName: "Name",
    formStrategy: "Strategy",
    formMarket: "Market",
    formMarketUS: "US Equity",
    formMarketCN: "A-Share",
    formBenchmark: "Benchmark (optional)",
    formInitialCapital: "Initial capital",
    formSubmit: "Create",
    formSubmitting: "Creating...",
    formCancel: "Cancel",
    selectPrompt: "Pick a portfolio on the left to view NAV + orders",
    portfolioMeta: "Strategy {strategy} · Market {market} · Benchmark {benchmark}",
    metricInitial: "Initial",
    metricNAV: "Current NAV",
    metricReturn: "Cum. return",
    metricCash: "Cash",
    navChartTitle: "NAV curve",
    ordersTitle: "Order ledger (SHA256 audit trail)",
    ordersEmpty: "No orders yet",
    ordersAction: "Action",
    ordersSymbol: "Symbol",
    ordersWeight: "Target wt",
    ordersDecided: "Decided at",
    ordersHash: "Hash",
    ordersStatus: "OTS status",
    newOrderCta: "Add order",
    newOrderTitle: "Submit new order (manual test)",
    newOrderSymbol: "Symbol",
    newOrderAction: "Action",
    newOrderTargetWeight: "Target weight (0-1)",
    newOrderDecidedPrice: "Decided price (optional)",
    newOrderReasoning: "AI reasoning JSON (optional)",
    newOrderSubmit: "Submit + hash",
    newOrderSubmitting: "Submitting...",
    error: "Operation failed",
    loading: "Loading...",
    masterCardTitle: "Master-team factor backtest vs US indices",
    masterCardSubtitle:
      "10 master personas mapped to representative tickers, equal-weight monthly rebalanced, compared with SPY / QQQ over the same window.",
    masterMetricCumulative: "Master team cum. return",
    masterMetricAnnual: "Annualised",
    masterMetricSharpe: "Sharpe",
    masterMetricMaxDD: "Max drawdown",
    masterBenchmarkLabel: "{symbol} cum. return",
    masterUniverseTitle: "Factor anchors",
    masterOpsTitle: "2016-2026 stock operation ledger",
    masterOpsSubtitle: "Simulated monthly equal-weight rebalance orders showing which tickers the strategy operated each year.",
    masterOpsEmpty: "No operation records",
    masterOpsDate: "Date",
    masterOpsMaster: "Master",
    masterOpsSymbol: "Ticker",
    masterOpsAction: "Action",
    masterOpsWeight: "Target wt",
    masterOpsShares: "Shares Δ",
    masterOpsPrice: "Price",
    masterOpsNotional: "Notional",
    masterOpsAccountValue: "Account value after rebalance",
    masterOpsReturn: "Cum. return",
    masterOpsCurrentValue: "Value path",
    masterOpsAsOf: "As of {date}",
    masterOpsValuePath: "{initial} → {current}",
    masterOpsShowing: "{total} operation records",
    masterRangeLabel: "Window",
    masterEmpty: "No backtest data",
    masterError: "Backtest load failed",
    masterLegendStrategy: "Master team",
  },
} as const;

type OrderAction = "BUY" | "SELL" | "REBALANCE";

const PaperTrading: React.FC = () => {
  const { language } = useAppPreferences();
  const lang: "zh" | "en" = language === "zh-CN" ? "zh" : "en";
  const copy = COPY[lang];

  const [portfolios, setPortfolios] = useState<PaperPortfolioView[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [orders, setOrders] = useState<PaperOrderView[]>([]);
  const [nav, setNav] = useState<PaperNavPointView[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const [showCreateForm, setShowCreateForm] = useState(false);
  const [showOrderForm, setShowOrderForm] = useState(false);

  const refreshPortfolios = useCallback(async () => {
    try {
      const list = await listPaperPortfolios();
      setPortfolios(list);
      if (!selectedId && list.length > 0) {
        setSelectedId(list[0].id);
      }
    } catch (e) {
      setError(formatApiError(e, lang === "zh" ? "加载组合失败" : "Failed to load portfolios"));
    }
  }, [selectedId, lang]);

  const refreshSelected = useCallback(async () => {
    if (!selectedId) return;
    setLoading(true);
    try {
      const [o, n] = await Promise.all([
        listPaperOrders(selectedId, 200),
        getPaperNavHistory(selectedId),
      ]);
      setOrders(o);
      setNav(n);
    } catch (e) {
      setError(formatApiError(e, lang === "zh" ? "加载详情失败" : "Failed to load detail"));
    } finally {
      setLoading(false);
    }
  }, [selectedId, lang]);

  useEffect(() => {
    void refreshPortfolios();
  }, [refreshPortfolios]);

  useEffect(() => {
    void refreshSelected();
  }, [refreshSelected]);

  const selected = useMemo(
    () => portfolios.find((p) => p.id === selectedId) ?? null,
    [portfolios, selectedId],
  );

  const navChartData = useMemo(() => {
    return nav.map((p) => ({
      date: p.date.slice(0, 10),
      nav: p.nav,
      benchmark: p.benchmarkNav,
    }));
  }, [nav]);

  return (
    <div className="mx-auto max-w-7xl space-y-5 p-6">
      <ComplianceAckModal surface="paper_trading" />
      <header>
        <h1 className="text-xl font-semibold text-slate-900">{copy.title}</h1>
        <p className="mt-1 text-sm text-slate-500">{copy.subtitle}</p>
      </header>
      <ComplianceBanner surface="paper_trading" />

      <MasterTeamBacktestCard copy={copy} language={language} />

      {error ? (
        <div className="rounded-md border border-rose-200 bg-rose-50 px-3 py-2 text-sm text-rose-700">
          <span className="font-medium">{copy.error}: </span>
          {error}
        </div>
      ) : null}

      <div className="grid grid-cols-12 gap-5">
        <aside className="col-span-12 md:col-span-3">
          <div className="rounded-xl border border-slate-200 bg-white p-3 shadow-sm">
            <div className="mb-2 flex items-center justify-between">
              <h2 className="text-sm font-semibold text-slate-900">{copy.portfoliosTitle}</h2>
              <button
                type="button"
                onClick={() => setShowCreateForm(true)}
                className="rounded-md bg-slate-900 px-2 py-1 text-[11px] font-medium text-white hover:bg-slate-800"
              >
                + {copy.createPortfolioCta}
              </button>
            </div>
            <ul className="space-y-1">
              {portfolios.map((p) => {
                const isSel = p.id === selectedId;
                return (
                  <li key={p.id}>
                    <button
                      type="button"
                      onClick={() => setSelectedId(p.id)}
                      className={`w-full rounded-md px-2 py-2 text-left text-xs ${
                        isSel ? "bg-slate-900 text-white" : "hover:bg-slate-50"
                      }`}
                    >
                      <div className="font-medium">{p.name}</div>
                      <div className={`mt-0.5 text-[10px] ${isSel ? "text-slate-300" : "text-slate-500"}`}>
                        {p.strategy} · {p.market}
                      </div>
                    </button>
                  </li>
                );
              })}
              {portfolios.length === 0 ? (
                <li className="rounded-md border border-dashed border-slate-200 px-2 py-3 text-center text-[11px] text-slate-400">
                  {copy.selectPrompt}
                </li>
              ) : null}
            </ul>
          </div>
        </aside>

        <main className="col-span-12 space-y-4 md:col-span-9">
          {showCreateForm ? (
            <CreatePortfolioForm
              copy={copy}
              onClose={() => setShowCreateForm(false)}
              onCreated={async (p) => {
                setShowCreateForm(false);
                setSelectedId(p.id);
                await refreshPortfolios();
              }}
              onError={setError}
            />
          ) : null}

          {!selected ? (
            <div className="rounded-xl border border-dashed border-slate-200 bg-white p-8 text-center text-sm text-slate-500">
              {copy.selectPrompt}
            </div>
          ) : (
            <>
              <PortfolioHeader copy={copy} portfolio={selected} language={language} />
              <NavChart copy={copy} data={navChartData} language={language} />
              <OrderLedger
                copy={copy}
                orders={orders}
                loading={loading}
                onAddOrder={() => setShowOrderForm(true)}
                language={language}
              />
            </>
          )}

          {showOrderForm && selected ? (
            <ProposeOrderForm
              copy={copy}
              portfolioId={selected.id}
              onClose={() => setShowOrderForm(false)}
              onSubmitted={async () => {
                setShowOrderForm(false);
                await refreshSelected();
              }}
              onError={setError}
            />
          ) : null}
        </main>
      </div>
    </div>
  );
};

// ----- Sub-components -------------------------------------------------------

type Copy = Record<keyof typeof COPY["zh"], string>;

const PortfolioHeader: React.FC<{ copy: Copy; portfolio: PaperPortfolioView; language: AppLanguage }> = ({
  copy,
  portfolio,
  language,
}) => {
  const ret = portfolio.initialCapital > 0 ? portfolio.currentNav / portfolio.initialCapital - 1 : 0;
  const isPositive = ret >= 0;
  const fmtCash = (v: number) => formatNumberForLanguage(v, language, { maximumFractionDigits: 0 });
  const fmtPct = (v: number) => `${formatNumberForLanguage(v * 100, language, { maximumFractionDigits: 2 })}%`;
  return (
    <section className="rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
      <h2 className="text-lg font-semibold text-slate-900">{portfolio.name}</h2>
      <p className="mt-1 text-xs text-slate-500">
        {copy.portfolioMeta
          .replace("{strategy}", portfolio.strategy)
          .replace("{market}", portfolio.market)
          .replace("{benchmark}", portfolio.benchmarkSymbol ?? "—")}
      </p>
      <div className="mt-3 grid grid-cols-4 gap-3 rounded-md bg-slate-50 p-3">
        <Kpi label={copy.metricInitial} value={`$${fmtCash(portfolio.initialCapital)}`} />
        <Kpi label={copy.metricNAV} value={`$${fmtCash(portfolio.currentNav)}`} />
        <Kpi
          label={copy.metricReturn}
          value={fmtPct(ret)}
          tone={isPositive ? "text-emerald-600" : "text-rose-600"}
        />
        <Kpi label={copy.metricCash} value={`$${fmtCash(portfolio.cashBalance)}`} />
      </div>
    </section>
  );
};

const Kpi: React.FC<{ label: string; value: string; tone?: string }> = ({ label, value, tone }) => (
  <div>
    <div className="text-[10px] uppercase tracking-wide text-slate-500">{label}</div>
    <div className={`mt-0.5 font-mono text-sm font-semibold ${tone ?? "text-slate-900"}`}>{value}</div>
  </div>
);

const NavChart: React.FC<{ copy: Copy; data: { date: string; nav: number; benchmark?: number }[]; language: AppLanguage }> = ({
  copy,
  data,
  language,
}) => (
  <section className="rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
    <h3 className="text-sm font-semibold text-slate-900">{copy.navChartTitle}</h3>
    <div className="mt-3 h-64 w-full">
      {data.length === 0 ? (
        <div className="flex h-full items-center justify-center text-xs text-slate-400">—</div>
      ) : (
        <ResponsiveContainer width="100%" height="100%">
          <AreaChart data={data} margin={{ top: 8, right: 16, bottom: 8, left: 4 }}>
            <defs>
              <linearGradient id="ptNavGrad" x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" stopColor="#1890ff" stopOpacity={0.25} />
                <stop offset="100%" stopColor="#1890ff" stopOpacity={0.03} />
              </linearGradient>
            </defs>
            <CartesianGrid stroke="#e5e7eb" strokeDasharray="3 3" />
            <XAxis dataKey="date" tick={{ fontSize: 11 }} minTickGap={48} />
            <YAxis
              tick={{ fontSize: 11 }}
              tickFormatter={(v: number) => formatNumberForLanguage(v, language, { maximumFractionDigits: 0 })}
              width={56}
            />
            <Tooltip />
            <Area type="monotone" dataKey="nav" stroke="#1890ff" strokeWidth={2} fill="url(#ptNavGrad)" isAnimationActive={false} dot={false} />
            {data.some((d) => typeof d.benchmark === "number") ? (
              <Area type="monotone" dataKey="benchmark" stroke="#ff4d4f" strokeWidth={1.5} fill="none" isAnimationActive={false} dot={false} />
            ) : null}
          </AreaChart>
        </ResponsiveContainer>
      )}
    </div>
  </section>
);

const OrderLedger: React.FC<{
  copy: Copy;
  orders: PaperOrderView[];
  loading: boolean;
  onAddOrder: () => void;
  language: AppLanguage;
}> = ({ copy, orders, loading, onAddOrder, language }) => (
  <section className="rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
    <div className="mb-3 flex items-center justify-between">
      <h3 className="text-sm font-semibold text-slate-900">{copy.ordersTitle}</h3>
      <button
        type="button"
        onClick={onAddOrder}
        className="rounded-md border border-slate-200 px-2 py-1 text-[11px] font-medium text-slate-700 hover:border-slate-400"
      >
        + {copy.newOrderCta}
      </button>
    </div>
    {loading && orders.length === 0 ? (
      <div className="py-4 text-center text-xs text-slate-400">{copy.loading}</div>
    ) : orders.length === 0 ? (
      <div className="py-4 text-center text-xs text-slate-400">{copy.ordersEmpty}</div>
    ) : (
      <div className="overflow-x-auto">
        <table className="min-w-full text-xs">
          <thead className="text-[10px] uppercase tracking-wide text-slate-500">
            <tr>
              <th className="py-1 pr-3 text-left">{copy.ordersAction}</th>
              <th className="py-1 pr-3 text-left">{copy.ordersSymbol}</th>
              <th className="py-1 pr-3 text-right">{copy.ordersWeight}</th>
              <th className="py-1 pr-3 text-left">{copy.ordersDecided}</th>
              <th className="py-1 pr-3 text-left">{copy.ordersHash}</th>
              <th className="py-1 pr-3 text-left">{copy.ordersStatus}</th>
            </tr>
          </thead>
          <tbody>
            {orders.map((o) => (
              <tr key={o.id} className="border-t border-slate-100">
                <td className="py-1.5 pr-3">
                  <ActionBadge action={o.action} />
                </td>
                <td className="py-1.5 pr-3 font-mono font-medium text-slate-900">{o.symbol}</td>
                <td className="py-1.5 pr-3 text-right font-mono text-slate-700">
                  {typeof o.targetWeight === "number"
                    ? `${formatNumberForLanguage(o.targetWeight * 100, language, { maximumFractionDigits: 1 })}%`
                    : "—"}
                </td>
                <td className="py-1.5 pr-3 font-mono text-[11px] text-slate-600">
                  {o.decidedAt.slice(0, 19).replace("T", " ")}
                </td>
                <td className="py-1.5 pr-3 font-mono text-[11px] text-slate-500">
                  {o.publicProofURL ? (
                    <a
                      href={o.publicProofURL}
                      target="_blank"
                      rel="noreferrer"
                      className="text-blue-600 hover:underline"
                      title={o.hashSignature}
                    >
                      {o.hashSignature.slice(0, 10)}…
                    </a>
                  ) : (
                    <span title={o.hashSignature}>{o.hashSignature.slice(0, 10)}…</span>
                  )}
                </td>
                <td className="py-1.5 pr-3">
                  <OtsStatusBadge status={o.otsStatus} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    )}
  </section>
);

// ActionBadge renders the order action with the compliance-mode-
// aware "Model action: …" framing. We keep the colour-coded
// pill (emerald = open / rose = close / blue = rebalance) so the
// operator's mental model of the ledger isn't lost — only the
// LABEL text is sanitised to avoid the bare verb reading as a
// recommendation under Publisher mode.
const ActionBadge: React.FC<{ action: OrderAction }> = ({ action }) => {
  const { mode } = useCompliance();
  const { language } = useAppPreferences();
  const locale: ComplianceLocale = language === "en-US" ? "en-US" : "zh-CN";
  const map: Record<OrderAction, string> = {
    BUY: "bg-emerald-100 text-emerald-700",
    SELL: "bg-rose-100 text-rose-700",
    REBALANCE: "bg-blue-100 text-blue-700",
  };
  const label = formatModelAction(action, locale, mode);
  return (
    <span
      title={action}
      className={`inline-flex rounded px-1.5 py-0.5 text-[10px] font-medium ${map[action]}`}
    >
      {label}
    </span>
  );
};

const OtsStatusBadge: React.FC<{ status: PaperOrderView["otsStatus"] }> = ({ status }) => {
  const map: Record<PaperOrderView["otsStatus"], string> = {
    pending: "bg-slate-100 text-slate-600",
    submitted: "bg-amber-100 text-amber-700",
    confirmed: "bg-emerald-100 text-emerald-700",
    disabled: "bg-slate-50 text-slate-400",
  };
  return (
    <span className={`inline-flex rounded px-1.5 py-0.5 text-[10px] font-medium ${map[status]}`}>{status}</span>
  );
};

// ----- Forms ----------------------------------------------------------------

const CreatePortfolioForm: React.FC<{
  copy: Copy;
  onClose: () => void;
  onCreated: (p: PaperPortfolioView) => void;
  onError: (msg: string) => void;
}> = ({ copy, onClose, onCreated, onError }) => {
  const [form, setForm] = useState<PaperPortfolioInput>({
    name: "",
    strategy: "",
    market: "us_equity",
    benchmarkSymbol: "",
    initialCapital: 100_000,
  });
  const [submitting, setSubmitting] = useState(false);

  const submit = useCallback(async () => {
    setSubmitting(true);
    try {
      const out = await createPaperPortfolio(form);
      onCreated(out);
    } catch (e) {
      onError(formatApiError(e, "create failed"));
    } finally {
      setSubmitting(false);
    }
  }, [form, onCreated, onError]);

  return (
    <section className="rounded-xl border border-slate-300 bg-slate-50 p-4 shadow-sm">
      <h3 className="text-sm font-semibold text-slate-900">{copy.createPortfolioCta}</h3>
      <div className="mt-3 grid grid-cols-2 gap-3 text-xs">
        <Field label={copy.formName}>
          <input
            type="text"
            value={form.name}
            onChange={(e) => setForm({ ...form, name: e.target.value })}
            className="w-full rounded-md border border-slate-200 px-2 py-1 text-xs"
          />
        </Field>
        <Field label={copy.formStrategy}>
          <input
            type="text"
            value={form.strategy}
            onChange={(e) => setForm({ ...form, strategy: e.target.value })}
            className="w-full rounded-md border border-slate-200 px-2 py-1 text-xs"
            placeholder="momentum_top30_monthly"
          />
        </Field>
        <Field label={copy.formMarket}>
          <select
            value={form.market}
            onChange={(e) => setForm({ ...form, market: e.target.value })}
            className="w-full rounded-md border border-slate-200 px-2 py-1 text-xs"
          >
            <option value="us_equity">{copy.formMarketUS}</option>
            <option value="a_share">{copy.formMarketCN}</option>
          </select>
        </Field>
        <Field label={copy.formBenchmark}>
          <input
            type="text"
            value={form.benchmarkSymbol ?? ""}
            onChange={(e) => setForm({ ...form, benchmarkSymbol: e.target.value })}
            placeholder="IWM / 000300.SH"
            className="w-full rounded-md border border-slate-200 px-2 py-1 text-xs"
          />
        </Field>
        <Field label={copy.formInitialCapital}>
          <input
            type="number"
            value={form.initialCapital}
            onChange={(e) => setForm({ ...form, initialCapital: Number(e.target.value) })}
            className="w-full rounded-md border border-slate-200 px-2 py-1 text-xs"
          />
        </Field>
      </div>
      <div className="mt-3 flex justify-end gap-2">
        <button
          type="button"
          onClick={onClose}
          className="rounded-md border border-slate-200 px-3 py-1.5 text-xs text-slate-700 hover:border-slate-400"
        >
          {copy.formCancel}
        </button>
        <button
          type="button"
          onClick={() => void submit()}
          disabled={submitting}
          className="rounded-md bg-slate-900 px-3 py-1.5 text-xs font-medium text-white hover:bg-slate-800 disabled:cursor-not-allowed disabled:bg-slate-400"
        >
          {submitting ? copy.formSubmitting : copy.formSubmit}
        </button>
      </div>
    </section>
  );
};

const ProposeOrderForm: React.FC<{
  copy: Copy;
  portfolioId: string;
  onClose: () => void;
  onSubmitted: () => void;
  onError: (msg: string) => void;
}> = ({ copy, portfolioId, onClose, onSubmitted, onError }) => {
  const [symbol, setSymbol] = useState("AAPL");
  const [action, setAction] = useState<OrderAction>("BUY");
  const [targetWeight, setTargetWeight] = useState<string>("0.05");
  const [decidedPrice, setDecidedPrice] = useState<string>("");
  const [reasoning, setReasoning] = useState<string>(`{"buffett":"BUY","confidence":0.85}`);
  const [submitting, setSubmitting] = useState(false);

  const submit = useCallback(async () => {
    setSubmitting(true);
    try {
      let parsedReasoning: Record<string, unknown> | undefined;
      if (reasoning.trim()) {
        try {
          parsedReasoning = JSON.parse(reasoning) as Record<string, unknown>;
        } catch {
          onError("aiReasoning is not valid JSON");
          setSubmitting(false);
          return;
        }
      }
      const input: ProposeOrderInput = {
        portfolioId,
        symbol,
        action,
        targetWeight: targetWeight ? Number(targetWeight) : undefined,
        decidedPrice: decidedPrice ? Number(decidedPrice) : undefined,
        aiReasoning: parsedReasoning,
      };
      await proposePaperOrder(input);
      onSubmitted();
    } catch (e) {
      onError(formatApiError(e, "propose order failed"));
    } finally {
      setSubmitting(false);
    }
  }, [portfolioId, symbol, action, targetWeight, decidedPrice, reasoning, onSubmitted, onError]);

  return (
    <section className="rounded-xl border border-slate-300 bg-slate-50 p-4 shadow-sm">
      <h3 className="text-sm font-semibold text-slate-900">{copy.newOrderTitle}</h3>
      <div className="mt-3 grid grid-cols-2 gap-3 text-xs">
        <Field label={copy.newOrderSymbol}>
          <input
            type="text"
            value={symbol}
            onChange={(e) => setSymbol(e.target.value.toUpperCase())}
            className="w-full rounded-md border border-slate-200 px-2 py-1 text-xs font-mono"
          />
        </Field>
        <Field label={copy.newOrderAction}>
          <select
            value={action}
            onChange={(e) => setAction(e.target.value as OrderAction)}
            className="w-full rounded-md border border-slate-200 px-2 py-1 text-xs"
          >
            <option value="BUY">BUY</option>
            <option value="SELL">SELL</option>
            <option value="REBALANCE">REBALANCE</option>
          </select>
        </Field>
        <Field label={copy.newOrderTargetWeight}>
          <input
            type="text"
            value={targetWeight}
            onChange={(e) => setTargetWeight(e.target.value)}
            className="w-full rounded-md border border-slate-200 px-2 py-1 text-xs font-mono"
          />
        </Field>
        <Field label={copy.newOrderDecidedPrice}>
          <input
            type="text"
            value={decidedPrice}
            onChange={(e) => setDecidedPrice(e.target.value)}
            className="w-full rounded-md border border-slate-200 px-2 py-1 text-xs font-mono"
          />
        </Field>
        <Field label={copy.newOrderReasoning} colspan={2}>
          <textarea
            value={reasoning}
            onChange={(e) => setReasoning(e.target.value)}
            rows={3}
            className="w-full rounded-md border border-slate-200 px-2 py-1 text-xs font-mono"
          />
        </Field>
      </div>
      <div className="mt-3 flex justify-end gap-2">
        <button
          type="button"
          onClick={onClose}
          className="rounded-md border border-slate-200 px-3 py-1.5 text-xs text-slate-700 hover:border-slate-400"
        >
          {copy.formCancel}
        </button>
        <button
          type="button"
          onClick={() => void submit()}
          disabled={submitting}
          className="rounded-md bg-slate-900 px-3 py-1.5 text-xs font-medium text-white hover:bg-slate-800 disabled:cursor-not-allowed disabled:bg-slate-400"
        >
          {submitting ? copy.newOrderSubmitting : copy.newOrderSubmit}
        </button>
      </div>
    </section>
  );
};

const Field: React.FC<{ label: string; children: React.ReactNode; colspan?: number }> = ({
  label,
  children,
  colspan,
}) => (
  <label className={`flex flex-col gap-1 ${colspan === 2 ? "col-span-2" : ""}`}>
    <span className="text-[10px] uppercase tracking-wide text-slate-500">{label}</span>
    {children}
  </label>
);

// ----- Master-team factor backtest card -------------------------------------

// Stable colour palette for the strategy + first two benchmarks, plus a
// fallback list for additional benchmarks.
const MASTER_COLORS = ["#0ea5e9", "#f97316", "#10b981", "#8b5cf6", "#f43f5e"];

const MasterTeamBacktestCard: React.FC<{ copy: Copy; language: AppLanguage }> = ({
  copy,
  language,
}) => {
  const [data, setData] = useState<MasterBacktestResultView | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    getMasterTeamBacktest({ start: "2016-01-01", end: "2026-06-25", benchmarks: ["SPY", "QQQ"] })
      .then((res) => {
        if (!cancelled) {
          setData(res);
          setError(null);
        }
      })
      .catch((e) => {
        if (!cancelled) {
          setError(formatApiError(e, copy.masterError));
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [copy.masterError]);

  // Merge strategy + each benchmark curve onto a shared X axis (date).
  // We build a date→{nav, bench:{symbol→pct}} map keyed off the
  // strategy curve, since it's the densest and SPY/QQQ trading days
  // are a strict subset.
  const chartData = useMemo(() => {
    if (!data) return [];
    type Row = Record<string, number | string>;
    const merged = new Map<string, Row>();
    for (const p of data.navCurve) {
      const date = p.date.slice(0, 10);
      merged.set(date, { date, strategy: p.pct * 100 });
    }
    for (const b of data.benchmarks) {
      for (const p of b.curve) {
        const date = p.date.slice(0, 10);
        const row: Row = merged.get(date) ?? { date, strategy: NaN };
        row[b.symbol] = p.pct * 100;
        merged.set(date, row);
      }
    }
    return Array.from(merged.values()).sort((a, b) =>
      String(a.date).localeCompare(String(b.date)),
    );
  }, [data]);

  const fmtPct = (v: number) =>
    `${formatNumberForLanguage(v * 100, language, { maximumFractionDigits: 2 })}%`;
  const fmtPctAxis = (v: number) =>
    `${formatNumberForLanguage(v, language, { maximumFractionDigits: 0 })}%`;

  return (
    <section className="rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
      <header className="flex flex-wrap items-baseline justify-between gap-2">
        <div>
          <h2 className="text-base font-semibold text-slate-900">{copy.masterCardTitle}</h2>
          <p className="mt-1 text-xs text-slate-500">{copy.masterCardSubtitle}</p>
        </div>
        {data ? (
          <div className="text-[11px] text-slate-500">
            <span className="uppercase tracking-wide">{copy.masterRangeLabel}: </span>
            <span className="font-mono text-slate-700">
              {data.start.slice(0, 10)} → {data.end.slice(0, 10)}
            </span>
          </div>
        ) : null}
      </header>

      {error ? (
        <div className="mt-4 rounded-md border border-rose-200 bg-rose-50 px-3 py-2 text-xs text-rose-700">
          {error}
        </div>
      ) : null}

      {loading && !data ? (
        <div className="mt-6 flex h-56 items-center justify-center text-xs text-slate-400">
          {copy.loading}
        </div>
      ) : !data ? (
        <div className="mt-6 flex h-56 items-center justify-center text-xs text-slate-400">
          {copy.masterEmpty}
        </div>
      ) : (
        <>
          <div className="mt-4 grid grid-cols-2 gap-3 rounded-md bg-slate-50 p-3 sm:grid-cols-4">
            <Kpi
              label={copy.masterMetricCumulative}
              value={fmtPct(data.metrics.cumulativeReturn)}
              tone={data.metrics.cumulativeReturn >= 0 ? "text-emerald-600" : "text-rose-600"}
            />
            <Kpi
              label={copy.masterMetricAnnual}
              value={fmtPct(data.metrics.annualizedReturn)}
              tone={data.metrics.annualizedReturn >= 0 ? "text-emerald-600" : "text-rose-600"}
            />
            <Kpi
              label={copy.masterMetricSharpe}
              value={formatNumberForLanguage(data.metrics.sharpeRatio, language, {
                maximumFractionDigits: 2,
              })}
            />
            <Kpi
              label={copy.masterMetricMaxDD}
              value={fmtPct(data.metrics.maxDrawdown)}
              tone="text-rose-600"
            />
          </div>

          <div className="mt-4 h-72 w-full">
            <ResponsiveContainer width="100%" height="100%">
              <LineChart data={chartData} margin={{ top: 8, right: 16, bottom: 8, left: 4 }}>
                <CartesianGrid stroke="#e5e7eb" strokeDasharray="3 3" />
                <XAxis dataKey="date" tick={{ fontSize: 11 }} minTickGap={64} />
                <YAxis
                  tick={{ fontSize: 11 }}
                  tickFormatter={fmtPctAxis}
                  width={56}
                  domain={["auto", "auto"]}
                />
                <Tooltip
                  formatter={(v: number) =>
                    `${formatNumberForLanguage(v, language, { maximumFractionDigits: 2 })}%`
                  }
                />
                <Legend wrapperStyle={{ fontSize: 11 }} />
                <Line
                  type="monotone"
                  dataKey="strategy"
                  name={copy.masterLegendStrategy}
                  stroke={MASTER_COLORS[0]}
                  strokeWidth={2.2}
                  dot={false}
                  isAnimationActive={false}
                  connectNulls
                />
                {data.benchmarks.map((b, i) => (
                  <Line
                    key={b.symbol}
                    type="monotone"
                    dataKey={b.symbol}
                    name={copy.masterBenchmarkLabel.replace("{symbol}", b.symbol)}
                    stroke={MASTER_COLORS[(i + 1) % MASTER_COLORS.length]}
                    strokeWidth={1.6}
                    strokeDasharray="4 4"
                    dot={false}
                    isAnimationActive={false}
                    connectNulls
                  />
                ))}
              </LineChart>
            </ResponsiveContainer>
          </div>

          <div className="mt-4">
            <h3 className="text-[11px] font-semibold uppercase tracking-wide text-slate-500">
              {copy.masterUniverseTitle}
            </h3>
            <ul className="mt-2 grid grid-cols-2 gap-2 sm:grid-cols-5">
              {data.universe.map((a) => (
                <li
                  key={a.master}
                  className="rounded-md border border-slate-200 bg-slate-50 px-2 py-1.5 text-[11px]"
                >
                  <div className="flex items-baseline justify-between gap-1">
                    <span className="font-medium capitalize text-slate-700">{a.master}</span>
                    <span className="font-mono text-[11px] text-slate-900">{a.symbol}</span>
                  </div>
                  <div className="mt-0.5 truncate text-[10px] text-slate-500" title={a.style}>
                    {a.style}
                  </div>
                </li>
              ))}
            </ul>
          </div>

          <MasterOperationsTable
            copy={copy}
            operations={data.operations ?? []}
            initialValue={data.initialCapital}
            currentValue={data.finalNav}
            asOf={data.end}
            language={language}
          />
        </>
      )}
    </section>
  );
};

const MasterOperationsTable: React.FC<{
  copy: Copy;
  operations: MasterOperationView[];
  initialValue: number;
  currentValue: number;
  asOf: string;
  language: AppLanguage;
}> = ({ copy, operations, initialValue, currentValue, asOf, language }) => {
  const rows = useMemo(() => {
    return [...operations]
      .sort((a, b) => String(a.date).localeCompare(String(b.date)));
  }, [operations]);
  const fmtNum = (v: number, digits = 2) =>
    formatNumberForLanguage(v, language, { maximumFractionDigits: digits });
  const fmtPct = (v: number) =>
    `${formatNumberForLanguage(v * 100, language, { maximumFractionDigits: 1 })}%`;

  return (
    <div className="mt-5 rounded-lg border border-slate-200 bg-slate-50/60 p-3">
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div>
          <h3 className="text-[11px] font-semibold uppercase tracking-wide text-slate-600">
            {copy.masterOpsTitle}
          </h3>
          <p className="mt-1 max-w-3xl text-[11px] text-slate-500">{copy.masterOpsSubtitle}</p>
        </div>
        <div className="rounded-lg border border-emerald-200 bg-white px-3 py-2 text-right shadow-sm">
          <div className="text-[10px] uppercase tracking-wide text-emerald-700">{copy.masterOpsCurrentValue}</div>
          <div className="mt-0.5 font-mono text-base font-semibold text-emerald-700">
            {copy.masterOpsValuePath
              .replace("{initial}", `$${fmtNum(initialValue, 0)}`)
              .replace("{current}", `$${fmtNum(currentValue, 0)}`)}
          </div>
          <div className="mt-0.5 text-[10px] text-slate-500">
            {copy.masterOpsAsOf.replace("{date}", asOf.slice(0, 10))}
          </div>
          {operations.length > 0 && (
            <div className="mt-1 text-[10px] text-slate-400">
              {copy.masterOpsShowing.replace("{total}", String(operations.length))}
            </div>
          )}
        </div>
      </div>
      {rows.length === 0 ? (
        <div className="py-4 text-center text-xs text-slate-400">{copy.masterOpsEmpty}</div>
      ) : (
        <div className="mt-3 max-h-96 overflow-auto rounded-md border border-slate-200 bg-white">
          <table className="min-w-full text-xs">
            <thead className="sticky top-0 bg-slate-100 text-[10px] uppercase tracking-wide text-slate-500">
              <tr>
                <th className="px-2 py-2 text-left">{copy.masterOpsDate}</th>
                <th className="px-2 py-2 text-left">{copy.masterOpsMaster}</th>
                <th className="px-2 py-2 text-left">{copy.masterOpsSymbol}</th>
                <th className="px-2 py-2 text-left">{copy.masterOpsAction}</th>
                <th className="px-2 py-2 text-right">{copy.masterOpsWeight}</th>
                <th className="px-2 py-2 text-right">{copy.masterOpsShares}</th>
                <th className="px-2 py-2 text-right">{copy.masterOpsPrice}</th>
                <th className="px-2 py-2 text-right">{copy.masterOpsNotional}</th>
                <th className="px-2 py-2 text-right">{copy.masterOpsAccountValue}</th>
                <th className="px-2 py-2 text-right">{copy.masterOpsReturn}</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-100">
              {rows.map((op, idx) => (
                <tr key={`${op.date}-${op.symbol}-${idx}`} className="hover:bg-slate-50">
                  <td className="px-2 py-1.5 font-mono text-[11px] text-slate-600">{op.date.slice(0, 10)}</td>
                  <td className="px-2 py-1.5 capitalize text-slate-700" title={op.style}>{op.master || "—"}</td>
                  <td className="px-2 py-1.5 font-mono font-semibold text-slate-900">{op.symbol}</td>
                  <td className="px-2 py-1.5">
                    <span className={`rounded px-1.5 py-0.5 text-[10px] font-medium ${op.action === "SELL" ? "bg-rose-100 text-rose-700" : "bg-emerald-100 text-emerald-700"}`}>
                      {op.action}
                    </span>
                  </td>
                  <td className="px-2 py-1.5 text-right font-mono text-slate-700">{fmtPct(op.targetWeight)}</td>
                  <td className="px-2 py-1.5 text-right font-mono text-slate-700">{fmtNum(op.sharesChange, 4)}</td>
                  <td className="px-2 py-1.5 text-right font-mono text-slate-700">${fmtNum(op.price)}</td>
                  <td className="px-2 py-1.5 text-right font-mono text-slate-700">${fmtNum(op.notional, 0)}</td>
                  <td className="px-2 py-1.5 text-right font-mono font-semibold text-emerald-700">${fmtNum(op.accountValue, 0)}</td>
                  <td className={`px-2 py-1.5 text-right font-mono ${op.cumulativeReturn >= 0 ? "text-emerald-700" : "text-rose-700"}`}>{fmtPct(op.cumulativeReturn)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
};

export default PaperTrading;
