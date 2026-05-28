import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useParams } from "react-router-dom";
import {
  cancelBacktest,
  compareBacktests,
  formatApiError,
  getBacktest,
  getSweep,
  getSweepAxisCatalog,
  listBacktests,
  listSweeps,
  proposePromotion,
  submitBacktest,
  submitSweep,
  type BacktestComparison,
  type BacktestJob,
  type BacktestNavPoint,
  type BacktestSubmitInput,
  type BacktestSweep,
  type BacktestTradeEvent,
  type SweepAxisInput,
  type SweepSubmitInput,
  type WalkForwardResultView,
} from "../lib/api";
import { useNavigate } from "react-router-dom";
import { formatDateForLanguage, formatDateTimeForLanguage, formatNumberForLanguage, useAppPreferences, type AppLanguage } from "../lib/preferences";

// Default form values are intentionally conservative: 6-month
// window, $100k starting cash, fallback engine. Operators in
// the simulation lab usually want a quick sanity-check rather
// than a multi-year LLM-driven epic, so this matches the
// least-cost dimension we can support.
function defaultStart(): string {
  const d = new Date();
  d.setMonth(d.getMonth() - 6);
  return d.toISOString().slice(0, 10);
}
function defaultEnd(): string {
  return new Date().toISOString().slice(0, 10);
}

// statusTone maps the backend's job status string to Tailwind
// utility classes. Used in three places (header badge, list pill,
// progress bar) so it's worth centralising.
function statusTone(status?: string): string {
  switch ((status ?? "").toLowerCase()) {
    case "completed":
      return "border-emerald-200 bg-emerald-50 text-emerald-700";
    case "running":
      return "border-indigo-200 bg-indigo-50 text-indigo-700";
    case "queued":
      return "border-amber-200 bg-amber-50 text-amber-700";
    case "failed":
      return "border-red-200 bg-red-50 text-red-700";
    case "cancelled":
      return "border-gray-200 bg-gray-50 text-gray-600";
    default:
      return "border-gray-200 bg-gray-50 text-gray-600";
  }
}

// Tiny inline SVG NAV chart. We deliberately avoid pulling a
// charting library — backtest results are typically ≤ 500
// points and we only need a smoothed line, peak/trough markers,
// and a baseline. The cost of recharts/d3 isn't worth it.
//
// width/height/padding are module-scoped constants so that
// useMemo dependency arrays stay stable across renders and the
// react-hooks/exhaustive-deps rule is satisfied without listing
// each axis offset.
const NAV_CHART_WIDTH = 720;
const NAV_CHART_HEIGHT = 220;
const NAV_CHART_PADDING = { top: 12, right: 12, bottom: 28, left: 56 };

const NavChart: React.FC<{ curve: BacktestNavPoint[]; foldBoundaries?: number[] }> = ({ curve, foldBoundaries }) => {
  const width = NAV_CHART_WIDTH;
  const height = NAV_CHART_HEIGHT;
  const padding = NAV_CHART_PADDING;

  const chart = useMemo(() => {
    if (curve.length < 2) {
      return null;
    }
    const navs = curve.map((p) => p.nav);
    const min = Math.min(...navs);
    const max = Math.max(...navs);
    const range = max - min || 1;
    const innerW = NAV_CHART_WIDTH - NAV_CHART_PADDING.left - NAV_CHART_PADDING.right;
    const innerH = NAV_CHART_HEIGHT - NAV_CHART_PADDING.top - NAV_CHART_PADDING.bottom;
    const xStep = innerW / Math.max(1, curve.length - 1);

    const points = curve.map((p, i) => {
      const x = NAV_CHART_PADDING.left + i * xStep;
      const y = NAV_CHART_PADDING.top + innerH - ((p.nav - min) / range) * innerH;
      return { x, y, nav: p.nav, date: p.date };
    });
    const linePath = points
      .map((pt, i) => `${i === 0 ? "M" : "L"} ${pt.x.toFixed(1)} ${pt.y.toFixed(1)}`)
      .join(" ");
    const areaPath = `${linePath} L ${points[points.length - 1].x.toFixed(1)} ${(NAV_CHART_PADDING.top + innerH).toFixed(1)} L ${NAV_CHART_PADDING.left.toFixed(1)} ${(NAV_CHART_PADDING.top + innerH).toFixed(1)} Z`;

    const baseline = curve[0].nav;
    const baseY = NAV_CHART_PADDING.top + innerH - ((baseline - min) / range) * innerH;
    const peak = points.reduce((acc, p) => (p.nav > acc.nav ? p : acc), points[0]);
    const trough = points.reduce((acc, p) => (p.nav < acc.nav ? p : acc), points[0]);
    return { points, linePath, areaPath, baseY, baseline, peak, trough, min, max };
  }, [curve]);

  if (!chart) {
    return <div className="rounded border border-dashed border-gray-200 p-6 text-center text-sm text-gray-400">No NAV data</div>;
  }
  // Walk-forward fold boundaries: vertical dashed lines at the
  // NAV index where each new fold starts. Index 0 is omitted —
  // it's always the chart's left edge, so a line there adds noise.
  const boundaryLines = (foldBoundaries ?? [])
    .filter((idx) => idx > 0 && idx < chart.points.length)
    .map((idx) => chart.points[idx]);
  return (
    <svg viewBox={`0 0 ${width} ${height}`} className="w-full overflow-visible">
      <line
        x1={padding.left}
        x2={width - padding.right}
        y1={chart.baseY}
        y2={chart.baseY}
        stroke="#9ca3af"
        strokeDasharray="4 4"
        strokeWidth={1}
      />
      {boundaryLines.map((pt, i) => (
        <line
          key={`fold-${i}`}
          x1={pt.x}
          x2={pt.x}
          y1={padding.top}
          y2={height - padding.bottom}
          stroke="#10b981"
          strokeDasharray="2 4"
          strokeWidth={1}
          opacity={0.55}
        />
      ))}
      <path d={chart.areaPath} fill="rgba(99, 102, 241, 0.12)" />
      <path d={chart.linePath} fill="none" stroke="#4f46e5" strokeWidth={1.8} />
      <circle cx={chart.peak.x} cy={chart.peak.y} r={4} fill="#10b981" />
      <circle cx={chart.trough.x} cy={chart.trough.y} r={4} fill="#ef4444" />
      <text x={padding.left - 6} y={padding.top + 4} textAnchor="end" fontSize={10} fill="#6b7280">
        {chart.max.toFixed(0)}
      </text>
      <text x={padding.left - 6} y={height - padding.bottom + 2} textAnchor="end" fontSize={10} fill="#6b7280">
        {chart.min.toFixed(0)}
      </text>
      <text x={padding.left} y={height - padding.bottom + 16} fontSize={10} fill="#6b7280">
        {chart.points[0].date.slice(0, 10)}
      </text>
      <text x={width - padding.right} y={height - padding.bottom + 16} fontSize={10} fill="#6b7280" textAnchor="end">
        {chart.points[chart.points.length - 1].date.slice(0, 10)}
      </text>
    </svg>
  );
};

const TradeTable: React.FC<{ trades: BacktestTradeEvent[]; copy: ReturnType<typeof buildCopy> }> = ({ trades, copy }) => {
  // Show filled trades first (sorted by date desc), then a
  // collapsed "skipped/capped" group so the table doesn't
  // drown the operator in noise.
  const filled = trades.filter((t) => t.status === "filled");
  const other = trades.filter((t) => t.status !== "filled");
  if (filled.length === 0 && other.length === 0) {
    return <p className="text-sm text-gray-500">{copy.noTrades}</p>;
  }
  return (
    <div className="overflow-x-auto">
      <table className="min-w-full text-sm">
        <thead className="border-b border-gray-200 bg-gray-50 text-left text-xs uppercase tracking-wide text-gray-500">
          <tr>
            <th className="px-3 py-2 font-medium">{copy.colDate}</th>
            <th className="px-3 py-2 font-medium">{copy.colSymbol}</th>
            <th className="px-3 py-2 font-medium">{copy.colAction}</th>
            <th className="px-3 py-2 font-medium">{copy.colStatus}</th>
            <th className="px-3 py-2 text-right font-medium">{copy.colQty}</th>
            <th className="px-3 py-2 text-right font-medium">{copy.colPrice}</th>
            <th className="px-3 py-2 text-right font-medium">{copy.colNotional}</th>
            <th className="px-3 py-2 font-medium">{copy.colReason}</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-gray-100">
          {filled.slice(0, 200).map((t, i) => (
            <tr key={`${t.date}-${i}-${t.symbol}`} className="hover:bg-gray-50">
              <td className="px-3 py-2 text-gray-700">{t.date.slice(0, 10)}</td>
              <td className="px-3 py-2 font-medium text-gray-900">{t.symbol}</td>
              <td className="px-3 py-2 text-gray-700">{t.action}</td>
              <td className="px-3 py-2">
                <span className="rounded-full border border-emerald-200 bg-emerald-50 px-2 py-0.5 text-xs font-medium text-emerald-700">
                  {t.status}
                </span>
              </td>
              <td className="px-3 py-2 text-right text-gray-700">{t.quantity ?? "—"}</td>
              <td className="px-3 py-2 text-right text-gray-700">{t.fillPrice?.toFixed(2) ?? "—"}</td>
              <td className="px-3 py-2 text-right text-gray-700">{t.notional?.toFixed(0) ?? "—"}</td>
              <td className="px-3 py-2 text-xs text-gray-500">{(t.reason ?? "").slice(0, 80)}</td>
            </tr>
          ))}
          {other.length > 0 && (
            <tr>
              <td colSpan={8} className="px-3 py-2 text-xs text-gray-500">
                {copy.otherCount.replace("{n}", String(other.length))}
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  );
};

function buildCopy(language: AppLanguage) {
  if (language === "en-US") {
    return {
      title: "Backtest lab",
      subtitle: "Replay the strategy stack against historical OHLC bars. Choose engine, universe, and window; results stream back as the runner walks each day.",
      newRun: "New backtest",
      runName: "Run name (optional)",
      market: "Market",
      symbols: "Symbols (comma separated)",
      symbolsPlaceholder: "AAPL, MSFT, GOOG",
      start: "Start",
      end: "End",
      initialCash: "Initial cash",
      baseCurrency: "Base currency",
      slippage: "Slippage (bps)",
      commission: "Commission (bps)",
      maxOrders: "Max orders per day",
      engineKind: "Decision engine",
      engineFallback: "Fallback (deterministic, no LLM)",
      engineLLM: "LLM PM Agent",
      engineDebate: "LLM + multi-agent debate",
      engineWarn: "LLM-backed runs call the model once per simulated day. Budget accordingly.",
      runIt: "Run backtest",
      runningLabel: "Running…",
      results: "Results",
      history: "Recent runs",
      noJobs: "No backtests yet. Submit the form on the left to start.",
      cumulative: "Cumulative",
      annualized: "Annualised",
      volatility: "Volatility",
      sharpe: "Sharpe",
      maxDD: "Max drawdown",
      winRate: "Daily win rate",
      tradeCount: "Trades",
      winLoss: "Wins / Losses",
      navCurve: "NAV curve",
      tradeLog: "Trade log (filled)",
      noTrades: "No trades recorded in this run.",
      colDate: "Date",
      colSymbol: "Symbol",
      colAction: "Action",
      colStatus: "Status",
      colQty: "Qty",
      colPrice: "Fill",
      colNotional: "Notional",
      colReason: "Reason",
      otherCount: "{n} non-filled events suppressed (skipped/capped/no-quote)",
      cancel: "Cancel run",
      loadError: "Failed to load backtests",
      submitError: "Failed to submit backtest",
      progress: "Progress",
      day: "day",
      missingFundId: "Missing fundId",
      requireSymbols: "Provide at least one symbol",
      compareSelect: "Select",
      compareSelected: "Selected for compare",
      compareHint: "Pick two completed runs to compare.",
      compareButton: "Compare",
      compareClear: "Clear",
      compareTitle: "Side-by-side comparison",
      compareExit: "Exit compare",
      compareLoading: "Loading comparison…",
      compareNotComparable: "Both runs must be completed to compare.",
      compareSameWindow: "Same window",
      compareDifferentWindow: "Different windows — deltas are not apples-to-apples",
      compareSameUniverse: "Same universe",
      compareDifferentUniverse: "Different symbol lists",
      compareDelta: "Δ (B − A)",
      compareNavOverlay: "NAV curves (normalised to 1.0 at run start)",
      compareLegendA: "A",
      compareLegendB: "B",
      compareNotEnough: "Pick exactly 2 runs",
      sweepToggle: "Parameter sweep mode",
      sweepHint: "Fan out a grid of runs by varying 1-2 axes.",
      sweepAxis: "Axis",
      sweepValues: "Values (comma-separated)",
      sweepAddAxis: "Add axis",
      sweepRemoveAxis: "Remove",
      sweepSubmit: "Run sweep",
      sweepSubmitting: "Submitting sweep…",
      sweepCells: "cells",
      sweepHistory: "Recent sweeps",
      sweepNoHistory: "No sweeps yet.",
      sweepGrid: "Sweep grid",
      sweepExit: "Exit sweep view",
      sweepDoneOf: "{done} / {total} done",
      sweepAxisCatalogError: "Failed to load axis catalog",
      sweepInvalidValues: "Each axis needs at least one value",
      sweepMaxAxes: "Up to 2 axes are supported.",
      sweepMaxCells: "Total cells must be at most 25.",
      walkForwardToggle: "Walk-forward validation",
      walkForwardHint: "Split the window into folds and run each as an OOS test.",
      walkForwardFolds: "Folds",
      walkForwardMode: "Mode",
      walkForwardAnchored: "Anchored (expanding train)",
      walkForwardRolling: "Rolling (sliding train)",
      walkForwardTrainRatio: "Train ratio",
      walkForwardTrainRatioHint: "Fraction of each fold reserved for the (informational) train window. 0 = pure OOS chunking.",
      walkForwardSection: "Walk-forward",
      walkForwardOOSReturn: "OOS return",
      walkForwardOOSSharpe: "OOS Sharpe",
      walkForwardMeanFold: "Mean fold return",
      walkForwardWorstFold: "Worst fold",
      walkForwardBestFold: "Best fold",
      walkForwardPerFold: "Per-fold breakdown",
      walkForwardFoldCol: "Fold",
      walkForwardWindowCol: "Test window",
      walkForwardReturnCol: "Return",
      walkForwardSharpeCol: "Sharpe",
      walkForwardDDCol: "Max DD",
      walkForwardTradesCol: "Trades",
      walkForwardErrorCol: "Error",
      walkForwardFoldsExceeded: "Folds must be between 2 and 12.",
      walkForwardRatioRange: "Train ratio must be in [0, 1).",
      promote: "Promote",
      promoteTitle: "Promote to production",
      promoteHint: "Send this completed backtest to the strategy promotion gate. A reviewer must approve before it goes live.",
      promoteNotes: "Notes (optional)",
      promoteShadowDays: "Shadow days",
      promoteShadowDaysHint: "Days of shadow trading before activation (0 = activate immediately on approval).",
      promoteDecayRatio: "Decay threshold",
      promoteDecayRatioHint: "Live Sharpe must stay above baseline × this ratio. 0.5 = pull plug at half the backtest Sharpe.",
      promoteSubmit: "Send to review",
      promoteSubmitting: "Sending…",
      promoteSuccess: "Promotion proposed — pending review.",
      promoteError: "Failed to propose promotion",
      promoteCancel: "Cancel",
      promoteOnlyCompleted: "Only completed backtests can be promoted.",
    };
  }
  return {
    title: "回测实验室",
    subtitle: "在历史 OHLC K 线上回放策略链路。选择决策引擎、股票池与时间窗，结果会随每一天的回放实时更新。",
    newRun: "新建回测",
    runName: "回测名称（可选）",
    market: "市场",
    symbols: "股票代码（逗号分隔）",
    symbolsPlaceholder: "AAPL, MSFT, GOOG",
    start: "起始日期",
    end: "结束日期",
    initialCash: "初始资金",
    baseCurrency: "基础货币",
    slippage: "滑点（bps）",
    commission: "手续费（bps）",
    maxOrders: "每日最大下单数",
    engineKind: "决策引擎",
    engineFallback: "Fallback（确定性，无 LLM）",
    engineLLM: "LLM PM Agent",
    engineDebate: "LLM + 多智能体辩论",
    engineWarn: "LLM 回测每个交易日都会调用模型，请评估预算后再开启。",
    runIt: "开始回测",
    runningLabel: "运行中…",
    results: "结果",
    history: "最近的回测",
    noJobs: "暂无回测记录，使用左侧表单提交一个吧。",
    cumulative: "累计收益",
    annualized: "年化收益",
    volatility: "波动率",
    sharpe: "夏普比率",
    maxDD: "最大回撤",
    winRate: "日胜率",
    tradeCount: "成交笔数",
    winLoss: "盈利 / 亏损",
    navCurve: "净值曲线",
    tradeLog: "成交记录",
    noTrades: "本次回测没有成交记录。",
    colDate: "日期",
    colSymbol: "代码",
    colAction: "动作",
    colStatus: "状态",
    colQty: "数量",
    colPrice: "成交价",
    colNotional: "成交金额",
    colReason: "说明",
    otherCount: "其余 {n} 条未成交事件已折叠（被跳过 / 上限 / 无报价）",
    cancel: "取消运行",
    loadError: "加载回测列表失败",
    submitError: "提交回测失败",
    progress: "进度",
    day: "天",
    missingFundId: "缺少 fundId",
    requireSymbols: "至少填一个标的",
    compareSelect: "对比",
    compareSelected: "已选用于对比",
    compareHint: "勾选两个已完成的回测来对比。",
    compareButton: "对比",
    compareClear: "清空",
    compareTitle: "对比视图",
    compareExit: "退出对比",
    compareLoading: "正在加载对比数据…",
    compareNotComparable: "两个回测都需要完成后才能对比。",
    compareSameWindow: "时间窗一致",
    compareDifferentWindow: "时间窗不一致 —— 差值仅供参考",
    compareSameUniverse: "股票池一致",
    compareDifferentUniverse: "股票池不一致",
    compareDelta: "差值 (B − A)",
    compareNavOverlay: "净值曲线（首日归一为 1.0）",
    compareLegendA: "A",
    compareLegendB: "B",
    compareNotEnough: "请勾选两个回测",
    sweepToggle: "参数扫描模式",
    sweepHint: "对 1-2 个维度做笛卡尔积，一次提交多个回测。",
    sweepAxis: "维度",
    sweepValues: "取值（逗号分隔）",
    sweepAddAxis: "添加维度",
    sweepRemoveAxis: "移除",
    sweepSubmit: "运行参数扫描",
    sweepSubmitting: "正在提交扫描…",
    sweepCells: "单元",
    sweepHistory: "最近的参数扫描",
    sweepNoHistory: "暂无参数扫描记录。",
    sweepGrid: "扫描网格",
    sweepExit: "退出扫描视图",
    sweepDoneOf: "{done} / {total} 完成",
    sweepAxisCatalogError: "维度列表加载失败",
    sweepInvalidValues: "每个维度至少要有一个取值",
    sweepMaxAxes: "最多支持 2 个维度。",
    sweepMaxCells: "总单元数不能超过 25。",
    walkForwardToggle: "Walk-forward 验证",
    walkForwardHint: "把时间窗切成若干 fold，每 fold 作为独立 OOS 段。",
    walkForwardFolds: "Fold 数",
    walkForwardMode: "模式",
    walkForwardAnchored: "Anchored（训练窗扩张）",
    walkForwardRolling: "Rolling（训练窗滚动）",
    walkForwardTrainRatio: "训练占比",
    walkForwardTrainRatioHint: "每个 fold 中用于训练（信息性）的窗口占比。0 表示纯 OOS 分段。",
    walkForwardSection: "Walk-forward",
    walkForwardOOSReturn: "OOS 收益",
    walkForwardOOSSharpe: "OOS 夏普",
    walkForwardMeanFold: "Fold 均值",
    walkForwardWorstFold: "最差 fold",
    walkForwardBestFold: "最佳 fold",
    walkForwardPerFold: "各 Fold 明细",
    walkForwardFoldCol: "Fold",
    walkForwardWindowCol: "测试窗口",
    walkForwardReturnCol: "收益",
    walkForwardSharpeCol: "夏普",
    walkForwardDDCol: "最大回撤",
    walkForwardTradesCol: "成交数",
    walkForwardErrorCol: "错误",
    walkForwardFoldsExceeded: "Fold 数必须在 2 到 12 之间。",
    walkForwardRatioRange: "训练占比必须在 [0, 1) 范围内。",
    promote: "升级到实盘",
    promoteTitle: "把策略推到实盘",
    promoteHint: "把这个已完成的回测送进策略升级流程。审核通过后才会真正上线。",
    promoteNotes: "备注（可选）",
    promoteShadowDays: "影子运行天数",
    promoteShadowDaysHint: "正式启用前先跑多少天影子（0 表示审核通过后直接激活）。",
    promoteDecayRatio: "衰减阈值",
    promoteDecayRatioHint: "实盘 Sharpe 必须高于 baseline × 该比值。0.5 表示腰斩即自动降级。",
    promoteSubmit: "提交审核",
    promoteSubmitting: "提交中…",
    promoteSuccess: "已提交升级提案 — 等待审核。",
    promoteError: "提交升级失败",
    promoteCancel: "取消",
    promoteOnlyCompleted: "只能升级已完成的回测。",
  };
}

const Backtest: React.FC = () => {
  const { fundId } = useParams<{ fundId: string }>();
  const { language } = useAppPreferences();
  const copy = useMemo(() => buildCopy(language), [language]);

  const [jobs, setJobs] = useState<BacktestJob[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  // Compare mode: holds an ordered pair (A, B) of jobIDs the
  // user picked via the per-row checkboxes. We cap at 2 — a
  // third click rotates A out so it always reflects the last
  // two selections. When `comparison` is non-null the right
  // pane swaps from single-result view to overlay view.
  const [compareIds, setCompareIds] = useState<[string?, string?]>([undefined, undefined]);
  const [comparison, setComparison] = useState<BacktestComparison | null>(null);
  const [compareLoading, setCompareLoading] = useState(false);

  // Sweep state: when sweepMode is on the form's "submit" button
  // calls submitSweep instead of submitBacktest. axisDrafts is
  // the editable list of axis form rows; axisCatalog is the
  // server-provided allow-list populated on mount.
  const [sweepMode, setSweepMode] = useState(false);
  const [sweepName, setSweepName] = useState("");
  const [axisDrafts, setAxisDrafts] = useState<Array<{ name: string; raw: string }>>([
    { name: "slippageBps", raw: "3, 5, 10" },
  ]);
  const [axisCatalog, setAxisCatalog] = useState<string[]>([]);
  const [sweeps, setSweeps] = useState<BacktestSweep[]>([]);
  const [activeSweepId, setActiveSweepId] = useState<string | null>(null);
  const [activeSweep, setActiveSweep] = useState<BacktestSweep | null>(null);
  const [activeSweepLoading, setActiveSweepLoading] = useState(false);

  // Walk-forward state: when wfMode is on, the submit handler
  // attaches a walkForward sub-spec to the request body. Folds is
  // capped at 12 to match the backend's MaxWalkForwardFolds; mode
  // toggles between anchored (expanding train window) and rolling
  // (sliding fixed-length train window).
  const [wfMode, setWfMode] = useState(false);
  const [wfFolds, setWfFolds] = useState(6);
  const [wfTrainRatio, setWfTrainRatio] = useState(0.5);
  const [wfRunMode, setWfRunMode] = useState<"anchored" | "rolling">("anchored");

  const [form, setForm] = useState<BacktestSubmitInput>({
    symbols: [],
    start: defaultStart(),
    end: defaultEnd(),
    initialCash: 100_000,
    baseCurrency: "USD",
    slippageBps: 5,
    commissionBps: 5,
    maxOrdersPerDay: 5,
    engineKind: "fallback",
    market: "us_equity",
  });
  const [symbolsRaw, setSymbolsRaw] = useState("AAPL, MSFT");

  // Poll cadence: every 1.5s while a job is queued/running. We
  // bail out of the polling loop as soon as every job is in a
  // terminal state to avoid hammering the API forever.
  const pollRef = useRef<number | null>(null);

  const refreshJobs = useCallback(async () => {
    if (!fundId) return;
    try {
      const list = await listBacktests(fundId);
      setJobs(list);
      setLoading(false);
      // If the user hasn't selected anything yet, default to the
      // newest job — usually what they want to look at.
      if (!selectedId && list.length > 0) {
        setSelectedId(list[0].id);
      }
    } catch (err) {
      setError(formatApiError(err, copy.loadError));
      setLoading(false);
    }
  }, [fundId, selectedId, copy.loadError]);

  useEffect(() => {
    refreshJobs();
  }, [refreshJobs]);

  // Poll while any job is non-terminal.
  useEffect(() => {
    const anyActive = jobs.some((j) => j.status === "queued" || j.status === "running");
    if (!anyActive) {
      if (pollRef.current) {
        window.clearInterval(pollRef.current);
        pollRef.current = null;
      }
      return;
    }
    if (pollRef.current) return;
    pollRef.current = window.setInterval(() => {
      refreshJobs();
    }, 1500);
    return () => {
      if (pollRef.current) {
        window.clearInterval(pollRef.current);
        pollRef.current = null;
      }
    };
  }, [jobs, refreshJobs]);

  // Whenever the selected job is non-terminal, pull just that
  // job (cheaper than the list call) so progress feels live.
  useEffect(() => {
    if (!fundId || !selectedId) return;
    const job = jobs.find((j) => j.id === selectedId);
    if (!job) return;
    if (job.status !== "queued" && job.status !== "running") return;
    let cancelled = false;
    const tick = async () => {
      try {
        const fresh = await getBacktest(fundId, selectedId);
        if (!cancelled) {
          setJobs((prev) => prev.map((j) => (j.id === selectedId ? fresh : j)));
        }
      } catch {
        // Swallow — refreshJobs will surface broader errors.
      }
    };
    const handle = window.setInterval(tick, 1000);
    return () => {
      cancelled = true;
      window.clearInterval(handle);
    };
  }, [fundId, selectedId, jobs]);

  const submit = async () => {
    if (!fundId) return;
    const symbols = symbolsRaw
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean);
    if (symbols.length === 0) {
      setError(copy.requireSymbols);
      return;
    }
    // Validate walk-forward sub-spec client-side so the user gets
    // immediate feedback rather than waiting for a 400 round-trip.
    if (wfMode) {
      if (wfFolds < 2 || wfFolds > 12) {
        setError(copy.walkForwardFoldsExceeded);
        return;
      }
      if (wfTrainRatio < 0 || wfTrainRatio >= 1) {
        setError(copy.walkForwardRatioRange);
        return;
      }
    }
    setError(null);
    setSubmitting(true);
    try {
      const body: BacktestSubmitInput = { ...form, symbols };
      if (wfMode) {
        body.walkForward = {
          numFolds: wfFolds,
          trainRatio: wfTrainRatio,
          mode: wfRunMode,
        };
      }
      const job = await submitBacktest(fundId, body);
      setJobs((prev) => [job, ...prev]);
      setSelectedId(job.id);
    } catch (err) {
      setError(formatApiError(err, copy.submitError));
    } finally {
      setSubmitting(false);
    }
  };

  const cancel = async (jobId: string) => {
    if (!fundId) return;
    try {
      await cancelBacktest(fundId, jobId);
      refreshJobs();
    } catch (err) {
      setError(formatApiError(err, copy.submitError));
    }
  };

  // toggleCompareSelection adds jobId to the compare pair, or
  // removes it if already selected. A third click rotates: the
  // older slot is replaced by the new ID so the user always sees
  // the most recently-picked pair.
  const toggleCompareSelection = (jobId: string) => {
    setCompareIds((prev) => {
      const [a, b] = prev;
      if (a === jobId) return [b, undefined];
      if (b === jobId) return [a, undefined];
      if (!a) return [jobId, b];
      if (!b) return [a, jobId];
      return [b, jobId]; // rotate older out
    });
    // Selecting a new pair invalidates any in-flight comparison.
    setComparison(null);
  };

  const clearCompare = () => {
    setCompareIds([undefined, undefined]);
    setComparison(null);
  };

  const runCompare = async () => {
    if (!fundId) return;
    const [a, b] = compareIds;
    if (!a || !b) return;
    setCompareLoading(true);
    setError(null);
    try {
      const result = await compareBacktests(fundId, a, b);
      setComparison(result);
    } catch (err) {
      setError(formatApiError(err, copy.loadError));
      setComparison(null);
    } finally {
      setCompareLoading(false);
    }
  };

  const refreshSweeps = useCallback(async () => {
    if (!fundId) return;
    try {
      const list = await listSweeps(fundId);
      setSweeps(list ?? []);
    } catch {
      // Non-fatal — older deployments may not have the sweeps
      // endpoint wired yet; swallow rather than scaring the user.
    }
  }, [fundId]);

  useEffect(() => {
    refreshSweeps();
  }, [refreshSweeps]);

  // Pull the axis catalog once on mount so the form dropdown is
  // populated.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await getSweepAxisCatalog();
        if (!cancelled) {
          setAxisCatalog(res.axes ?? []);
        }
      } catch {
        // Non-fatal — fall back to hard-coded labels if catalog
        // fetch fails. The submit endpoint still validates.
        if (!cancelled) {
          setAxisCatalog(["slippageBps", "commissionBps", "maxOrdersPerDay", "initialCash", "engineKind"]);
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  // Poll the active sweep while any of its children is non-terminal.
  useEffect(() => {
    if (!fundId || !activeSweepId) return;
    let cancelled = false;
    const tick = async () => {
      try {
        const sweep = await getSweep(fundId, activeSweepId);
        if (!cancelled) {
          setActiveSweep(sweep);
        }
        // Stop polling when every child is terminal.
        if (sweep && sweep.children && sweep.children.every((c) => ["completed", "failed", "cancelled"].includes(c.job.status))) {
          return false;
        }
        return true;
      } catch (err) {
        if (!cancelled) {
          setError(formatApiError(err, copy.loadError));
        }
        return false;
      }
    };
    let handle: number | null = null;
    const start = async () => {
      setActiveSweepLoading(true);
      const keepGoing = await tick();
      setActiveSweepLoading(false);
      if (!keepGoing || cancelled) return;
      handle = window.setInterval(async () => {
        const cont = await tick();
        if (!cont && handle != null) {
          window.clearInterval(handle);
          handle = null;
        }
      }, 2000);
    };
    start();
    return () => {
      cancelled = true;
      if (handle != null) window.clearInterval(handle);
    };
  }, [fundId, activeSweepId, copy.loadError]);

  // axisDrafts editing helpers — kept local to the component
  // since they don't need to be hoisted.
  const updateAxisDraft = (index: number, patch: Partial<{ name: string; raw: string }>) => {
    setAxisDrafts((prev) => prev.map((d, i) => (i === index ? { ...d, ...patch } : d)));
  };
  const addAxisDraft = () => {
    if (axisDrafts.length >= 2) return;
    // Pick a default axis that's NOT already used so the user
    // doesn't get a duplicate-axis 400 on submit.
    const used = new Set(axisDrafts.map((d) => d.name));
    const fallback = (axisCatalog.length > 0 ? axisCatalog : ["slippageBps", "commissionBps", "maxOrdersPerDay", "initialCash", "engineKind"]).find((n) => !used.has(n)) ?? "slippageBps";
    setAxisDrafts((prev) => [...prev, { name: fallback, raw: fallback === "engineKind" ? "fallback, llm" : "1, 3, 5" }]);
  };
  const removeAxisDraft = (index: number) => {
    setAxisDrafts((prev) => prev.filter((_, i) => i !== index));
  };

  // submitSweepRun mirrors `submit` but routes through the sweep
  // endpoint. We do a light client-side validation up-front so
  // the user gets immediate feedback instead of round-tripping
  // for obvious mistakes.
  const submitSweepRun = async () => {
    if (!fundId) return;
    const symbols = symbolsRaw
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean);
    if (symbols.length === 0) {
      setError(copy.requireSymbols);
      return;
    }
    if (axisDrafts.length === 0 || axisDrafts.length > 2) {
      setError(copy.sweepMaxAxes);
      return;
    }
    const axes: SweepAxisInput[] = [];
    let totalCells = 1;
    for (const draft of axisDrafts) {
      const values = draft.raw.split(",").map((v) => v.trim()).filter(Boolean);
      if (values.length === 0) {
        setError(copy.sweepInvalidValues);
        return;
      }
      axes.push({ name: draft.name, values });
      totalCells *= values.length;
    }
    if (totalCells > 25) {
      setError(copy.sweepMaxCells);
      return;
    }
    setError(null);
    setSubmitting(true);
    try {
      const payload: SweepSubmitInput = {
        name: sweepName.trim(),
        base: { ...form, symbols },
        axes,
      };
      const sweep = await submitSweep(fundId, payload);
      setSweeps((prev) => [sweep, ...prev]);
      setActiveSweepId(sweep.id);
      // Pull fresh job list so the new children show up too.
      refreshJobs();
    } catch (err) {
      setError(formatApiError(err, copy.submitError));
    } finally {
      setSubmitting(false);
    }
  };

  const selected = useMemo(() => jobs.find((j) => j.id === selectedId) ?? null, [jobs, selectedId]);
  const compareReady = Boolean(compareIds[0] && compareIds[1]);
  const totalSweepCells = useMemo(() => {
    let total = 1;
    for (const d of axisDrafts) {
      const n = d.raw.split(",").map((v) => v.trim()).filter(Boolean).length;
      if (n === 0) return 0;
      total *= n;
    }
    return total;
  }, [axisDrafts]);

  if (!fundId) {
    return <div className="p-6 text-sm text-gray-500">{copy.missingFundId}</div>;
  }

  return (
    <div className="space-y-6 p-6">
      <header>
        <h1 className="text-2xl font-semibold text-gray-900">{copy.title}</h1>
        <p className="mt-1 max-w-3xl text-sm text-gray-500">{copy.subtitle}</p>
      </header>

      {error && (
        <div className="rounded border border-red-200 bg-red-50 px-4 py-2 text-sm text-red-700">{error}</div>
      )}

      <div className="grid gap-6 lg:grid-cols-[360px_1fr]">
        {/* form */}
        <aside className="space-y-4 rounded-lg border border-gray-200 bg-white p-4">
          <h2 className="text-base font-semibold text-gray-800">{copy.newRun}</h2>
          <div className="space-y-3 text-sm">
            <label className="block">
              <span className="text-gray-700">{copy.runName}</span>
              <input
                className="mt-1 w-full rounded border border-gray-300 px-2 py-1 text-sm"
                value={form.name ?? ""}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
              />
            </label>
            <label className="block">
              <span className="text-gray-700">{copy.market}</span>
              <select
                className="mt-1 w-full rounded border border-gray-300 px-2 py-1 text-sm"
                value={form.market}
                onChange={(e) => setForm({ ...form, market: e.target.value })}
              >
                <option value="us_equity">US Equity</option>
                <option value="a_share">A-Share</option>
                <option value="hk_equity">HK Equity</option>
                <option value="crypto">Crypto</option>
                <option value="futures">Futures</option>
              </select>
            </label>
            <label className="block">
              <span className="text-gray-700">{copy.symbols}</span>
              <input
                className="mt-1 w-full rounded border border-gray-300 px-2 py-1 text-sm"
                placeholder={copy.symbolsPlaceholder}
                value={symbolsRaw}
                onChange={(e) => setSymbolsRaw(e.target.value)}
              />
            </label>
            <div className="grid grid-cols-2 gap-3">
              <label className="block">
                <span className="text-gray-700">{copy.start}</span>
                <input
                  type="date"
                  className="mt-1 w-full rounded border border-gray-300 px-2 py-1 text-sm"
                  value={form.start.slice(0, 10)}
                  onChange={(e) => setForm({ ...form, start: e.target.value })}
                />
              </label>
              <label className="block">
                <span className="text-gray-700">{copy.end}</span>
                <input
                  type="date"
                  className="mt-1 w-full rounded border border-gray-300 px-2 py-1 text-sm"
                  value={form.end.slice(0, 10)}
                  onChange={(e) => setForm({ ...form, end: e.target.value })}
                />
              </label>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <label className="block">
                <span className="text-gray-700">{copy.initialCash}</span>
                <input
                  type="number"
                  className="mt-1 w-full rounded border border-gray-300 px-2 py-1 text-sm"
                  value={form.initialCash}
                  onChange={(e) => setForm({ ...form, initialCash: Number(e.target.value) })}
                />
              </label>
              <label className="block">
                <span className="text-gray-700">{copy.baseCurrency}</span>
                <input
                  className="mt-1 w-full rounded border border-gray-300 px-2 py-1 text-sm uppercase"
                  value={form.baseCurrency ?? ""}
                  onChange={(e) => setForm({ ...form, baseCurrency: e.target.value })}
                />
              </label>
            </div>
            <div className="grid grid-cols-3 gap-3">
              <label className="block">
                <span className="text-gray-700">{copy.slippage}</span>
                <input
                  type="number"
                  className="mt-1 w-full rounded border border-gray-300 px-2 py-1 text-sm"
                  value={form.slippageBps ?? 5}
                  onChange={(e) => setForm({ ...form, slippageBps: Number(e.target.value) })}
                />
              </label>
              <label className="block">
                <span className="text-gray-700">{copy.commission}</span>
                <input
                  type="number"
                  className="mt-1 w-full rounded border border-gray-300 px-2 py-1 text-sm"
                  value={form.commissionBps ?? 5}
                  onChange={(e) => setForm({ ...form, commissionBps: Number(e.target.value) })}
                />
              </label>
              <label className="block">
                <span className="text-gray-700">{copy.maxOrders}</span>
                <input
                  type="number"
                  className="mt-1 w-full rounded border border-gray-300 px-2 py-1 text-sm"
                  value={form.maxOrdersPerDay ?? 5}
                  onChange={(e) => setForm({ ...form, maxOrdersPerDay: Number(e.target.value) })}
                />
              </label>
            </div>
            <label className="block">
              <span className="text-gray-700">{copy.engineKind}</span>
              <select
                className="mt-1 w-full rounded border border-gray-300 px-2 py-1 text-sm"
                value={form.engineKind}
                onChange={(e) => setForm({ ...form, engineKind: e.target.value as BacktestSubmitInput["engineKind"] })}
              >
                <option value="fallback">{copy.engineFallback}</option>
                <option value="llm">{copy.engineLLM}</option>
                <option value="llm-debate">{copy.engineDebate}</option>
              </select>
            </label>
            {form.engineKind !== "fallback" && (
              <p className="rounded border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-700">
                {copy.engineWarn}
              </p>
            )}

            <label className="flex items-center gap-2 text-xs text-gray-700">
              <input
                type="checkbox"
                checked={sweepMode}
                disabled={wfMode}
                onChange={(e) => setSweepMode(e.target.checked)}
              />
              <span>{copy.sweepToggle}</span>
            </label>

            <label className="flex items-center gap-2 text-xs text-gray-700">
              <input
                type="checkbox"
                checked={wfMode}
                disabled={sweepMode}
                onChange={(e) => setWfMode(e.target.checked)}
              />
              <span>{copy.walkForwardToggle}</span>
            </label>

            {wfMode && (
              <div className="space-y-3 rounded border border-emerald-100 bg-emerald-50/50 p-3">
                <p className="text-xs text-emerald-800">{copy.walkForwardHint}</p>
                <div className="grid grid-cols-2 gap-3">
                  <label className="block text-xs">
                    <span className="text-gray-700">{copy.walkForwardFolds}</span>
                    <input
                      type="number"
                      min={2}
                      max={12}
                      step={1}
                      className="mt-1 w-full rounded border border-gray-300 px-2 py-1 text-sm"
                      value={wfFolds}
                      onChange={(e) => setWfFolds(Number(e.target.value))}
                    />
                  </label>
                  <label className="block text-xs">
                    <span className="text-gray-700">{copy.walkForwardMode}</span>
                    <select
                      className="mt-1 w-full rounded border border-gray-300 px-2 py-1 text-sm"
                      value={wfRunMode}
                      onChange={(e) => setWfRunMode(e.target.value as "anchored" | "rolling")}
                    >
                      <option value="anchored">{copy.walkForwardAnchored}</option>
                      <option value="rolling">{copy.walkForwardRolling}</option>
                    </select>
                  </label>
                </div>
                <label className="block text-xs">
                  <span className="text-gray-700">{copy.walkForwardTrainRatio}</span>
                  <input
                    type="number"
                    min={0}
                    max={0.95}
                    step={0.05}
                    className="mt-1 w-full rounded border border-gray-300 px-2 py-1 text-sm"
                    value={wfTrainRatio}
                    onChange={(e) => setWfTrainRatio(Number(e.target.value))}
                  />
                  <span className="mt-1 block text-[11px] text-gray-500">{copy.walkForwardTrainRatioHint}</span>
                </label>
              </div>
            )}

            {sweepMode && (
              <div className="space-y-3 rounded border border-indigo-100 bg-indigo-50/50 p-3">
                <p className="text-xs text-indigo-800">{copy.sweepHint}</p>
                <label className="block text-xs">
                  <span className="text-gray-700">{copy.runName}</span>
                  <input
                    type="text"
                    className="mt-1 w-full rounded border border-gray-300 px-2 py-1 text-sm"
                    value={sweepName}
                    onChange={(e) => setSweepName(e.target.value)}
                  />
                </label>
                {axisDrafts.map((draft, i) => (
                  <div key={i} className="space-y-2 rounded border border-indigo-200 bg-white p-2">
                    <div className="flex items-center gap-2">
                      <label className="flex-1 text-xs">
                        <span className="text-gray-700">{copy.sweepAxis}</span>
                        <select
                          className="mt-1 w-full rounded border border-gray-300 px-2 py-1 text-sm"
                          value={draft.name}
                          onChange={(e) => updateAxisDraft(i, { name: e.target.value })}
                        >
                          {(axisCatalog.length > 0 ? axisCatalog : [draft.name]).map((opt) => (
                            <option key={opt} value={opt}>
                              {opt}
                            </option>
                          ))}
                        </select>
                      </label>
                      {axisDrafts.length > 1 && (
                        <button
                          type="button"
                          className="self-end rounded border border-red-200 bg-white px-2 py-1 text-xs text-red-700 hover:bg-red-50"
                          onClick={() => removeAxisDraft(i)}
                        >
                          {copy.sweepRemoveAxis}
                        </button>
                      )}
                    </div>
                    <label className="block text-xs">
                      <span className="text-gray-700">{copy.sweepValues}</span>
                      <input
                        type="text"
                        className="mt-1 w-full rounded border border-gray-300 px-2 py-1 text-sm font-mono"
                        value={draft.raw}
                        onChange={(e) => updateAxisDraft(i, { raw: e.target.value })}
                      />
                    </label>
                  </div>
                ))}
                {axisDrafts.length < 2 && (
                  <button
                    type="button"
                    className="w-full rounded border border-dashed border-indigo-300 px-2 py-1 text-xs font-medium text-indigo-700 hover:bg-indigo-50"
                    onClick={addAxisDraft}
                  >
                    + {copy.sweepAddAxis}
                  </button>
                )}
                <p className="text-xs text-gray-500">
                  {totalSweepCells} {copy.sweepCells}
                </p>
              </div>
            )}

            <button
              type="button"
              className="w-full rounded bg-indigo-600 px-3 py-2 text-sm font-semibold text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:bg-gray-300"
              disabled={submitting}
              onClick={sweepMode ? submitSweepRun : submit}
            >
              {submitting
                ? sweepMode
                  ? copy.sweepSubmitting
                  : copy.runningLabel
                : sweepMode
                ? copy.sweepSubmit
                : copy.runIt}
            </button>
          </div>
        </aside>

        {/* right column: history + result */}
        <section className="space-y-6">
          <div className="rounded-lg border border-gray-200 bg-white p-4">
            <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
              <h2 className="text-base font-semibold text-gray-800">{copy.history}</h2>
              <div className="flex items-center gap-2 text-xs">
                <span className="text-gray-500">
                  {copy.compareSelected}: {[compareIds[0], compareIds[1]].filter(Boolean).length}/2
                </span>
                <button
                  type="button"
                  className="rounded border border-indigo-200 bg-white px-2 py-1 text-xs font-medium text-indigo-700 hover:bg-indigo-50 disabled:cursor-not-allowed disabled:border-gray-200 disabled:text-gray-400"
                  onClick={runCompare}
                  disabled={!compareReady || compareLoading}
                  title={!compareReady ? copy.compareNotEnough : ""}
                >
                  {compareLoading ? copy.compareLoading : copy.compareButton}
                </button>
                {(compareIds[0] || compareIds[1]) && (
                  <button
                    type="button"
                    className="rounded border border-gray-200 bg-white px-2 py-1 text-xs text-gray-600 hover:bg-gray-50"
                    onClick={clearCompare}
                  >
                    {copy.compareClear}
                  </button>
                )}
              </div>
            </div>
            {(compareIds[0] || compareIds[1]) && !comparison && (
              <p className="mb-2 text-xs text-gray-500">{copy.compareHint}</p>
            )}
            {loading ? (
              <p className="text-sm text-gray-500">…</p>
            ) : jobs.length === 0 ? (
              <p className="text-sm text-gray-500">{copy.noJobs}</p>
            ) : (
              <ul className="divide-y divide-gray-100">
                {jobs.slice(0, 12).map((j) => {
                  const isActive = j.id === selectedId;
                  const total = j.progress?.totalDays ?? 0;
                  const done = j.progress?.doneDays ?? 0;
                  const pct = total > 0 ? Math.min(100, Math.round((done / total) * 100)) : 0;
                  const checked = compareIds[0] === j.id || compareIds[1] === j.id;
                  const compareSlot = compareIds[0] === j.id ? "A" : compareIds[1] === j.id ? "B" : null;
                  // Only completed jobs can be compared. Disable
                  // the checkbox for queued/running/failed runs;
                  // the row stays clickable for inspection.
                  const compareDisabled = j.status !== "completed";
                  return (
                    <li
                      key={j.id}
                      className={`flex cursor-pointer items-center gap-3 py-2 ${isActive ? "bg-indigo-50/40 px-2" : ""}`}
                      onClick={() => setSelectedId(j.id)}
                    >
                      <label
                        className={`flex items-center gap-1 ${compareDisabled ? "cursor-not-allowed opacity-40" : "cursor-pointer"}`}
                        onClick={(ev) => ev.stopPropagation()}
                        title={compareDisabled ? copy.compareNotComparable : copy.compareSelect}
                      >
                        <input
                          type="checkbox"
                          className="h-3.5 w-3.5"
                          checked={checked}
                          disabled={compareDisabled}
                          onChange={() => toggleCompareSelection(j.id)}
                        />
                        {compareSlot && (
                          <span className="rounded bg-indigo-100 px-1.5 py-0.5 text-[10px] font-bold text-indigo-700">
                            {compareSlot}
                          </span>
                        )}
                      </label>
                      <div className="flex-1">
                        <div className="flex items-center gap-2">
                          <span className="font-medium text-gray-900">{j.name || j.id.slice(0, 8)}</span>
                          <span className={`rounded-full border px-2 py-0.5 text-[10px] font-medium ${statusTone(j.status)}`}>
                            {j.status}
                          </span>
                          <span className="text-xs text-gray-500">{j.engineKind}</span>
                        </div>
                        <div className="mt-1 flex items-center gap-3 text-xs text-gray-500">
                          <span>{formatDateTimeForLanguage(j.submittedAt, language)}</span>
                          {(j.status === "running" || j.status === "queued") && (
                            <span>
                              {copy.progress}: {done}/{total} {copy.day} ({pct}%)
                            </span>
                          )}
                          {j.result && (
                            <span>
                              {copy.cumulative}: {formatNumberForLanguage(j.result.metrics.cumulativeReturn * 100, language, { maximumFractionDigits: 2 })}%
                            </span>
                          )}
                        </div>
                      </div>
                      {(j.status === "running" || j.status === "queued") && (
                        <button
                          type="button"
                          className="rounded border border-red-200 bg-white px-2 py-1 text-xs text-red-700 hover:bg-red-50"
                          onClick={(ev) => {
                            ev.stopPropagation();
                            cancel(j.id);
                          }}
                        >
                          {copy.cancel}
                        </button>
                      )}
                    </li>
                  );
                })}
              </ul>
            )}
          </div>

          {sweeps.length > 0 && (
            <div className="rounded-lg border border-gray-200 bg-white p-4">
              <h2 className="mb-3 text-base font-semibold text-gray-800">{copy.sweepHistory}</h2>
              <ul className="divide-y divide-gray-100">
                {sweeps.slice(0, 8).map((sw) => {
                  const isActive = sw.id === activeSweepId;
                  return (
                    <li
                      key={sw.id}
                      className={`flex cursor-pointer items-center gap-3 py-2 ${isActive ? "bg-indigo-50/40 px-2" : ""}`}
                      onClick={() => setActiveSweepId(sw.id)}
                    >
                      <div className="flex-1">
                        <div className="flex items-center gap-2">
                          <span className="font-medium text-gray-900">{sw.name || sw.id.slice(0, 8)}</span>
                          <span className={`rounded-full border px-2 py-0.5 text-[10px] font-medium ${statusTone(sw.status)}`}>
                            {sw.status}
                          </span>
                          <span className="text-xs text-gray-500">
                            {(sw.axes || []).map((a) => a.name).join(" × ") || "—"}
                          </span>
                        </div>
                        <div className="mt-1 flex items-center gap-3 text-xs text-gray-500">
                          <span>{formatDateTimeForLanguage(sw.createdAt, language)}</span>
                          <span>{copy.sweepDoneOf.replace("{done}", String(sw.doneCells)).replace("{total}", String(sw.totalCells))}</span>
                        </div>
                      </div>
                    </li>
                  );
                })}
              </ul>
            </div>
          )}

          {activeSweepId && activeSweep ? (
            <SweepView
              sweep={activeSweep}
              loading={activeSweepLoading}
              copy={copy}
              language={language}
              onExit={() => {
                setActiveSweepId(null);
                setActiveSweep(null);
              }}
              onPickJob={(jobId) => {
                setSelectedId(jobId);
                setActiveSweepId(null);
                setActiveSweep(null);
                refreshJobs();
              }}
            />
          ) : comparison ? (
            <CompareView
              comparison={comparison}
              copy={copy}
              language={language}
              onExit={() => setComparison(null)}
            />
          ) : selected && (
            <div className="rounded-lg border border-gray-200 bg-white p-4">
              <header className="mb-4 flex items-center justify-between">
                <div>
                  <h2 className="text-base font-semibold text-gray-800">{selected.name || selected.id.slice(0, 8)}</h2>
                  <p className="text-xs text-gray-500">
                    {formatDateForLanguage(selected.request?.start ?? "", language)} → {formatDateForLanguage(selected.request?.end ?? "", language)} · {selected.engineKind}
                  </p>
                </div>
                <div className="flex items-center gap-2">
                  {selected.status === "completed" && fundId && (
                    <PromoteButton
                      fundId={fundId}
                      job={selected}
                      copy={copy}
                    />
                  )}
                  <span className={`rounded-full border px-3 py-1 text-xs font-medium ${statusTone(selected.status)}`}>
                    {selected.status}
                  </span>
                </div>
              </header>

              {selected.status === "queued" || selected.status === "running" ? (
                <div className="space-y-2">
                  <div className="h-2 w-full rounded bg-gray-100">
                    <div
                      className="h-2 rounded bg-indigo-500 transition-all"
                      style={{ width: `${selected.progress?.totalDays ? Math.min(100, Math.round((selected.progress.doneDays / selected.progress.totalDays) * 100)) : 0}%` }}
                    />
                  </div>
                  <p className="text-xs text-gray-500">
                    {copy.progress}: {selected.progress?.doneDays ?? 0}/{selected.progress?.totalDays ?? 0} {copy.day}
                    {selected.progress?.currentDate && (
                      <> · {formatDateForLanguage(selected.progress.currentDate, language)}</>
                    )}
                  </p>
                </div>
              ) : selected.status === "failed" ? (
                <p className="text-sm text-red-700">{selected.error}</p>
              ) : selected.result ? (
                <div className="space-y-4">
                  <MetricsGrid copy={copy} metrics={selected.result.metrics} initial={selected.result.initialCash} final={selected.result.finalNav} language={language} />
                  <div>
                    <h3 className="mb-2 text-sm font-semibold text-gray-700">{copy.navCurve}</h3>
                    <NavChart
                      curve={selected.result.navCurve}
                      foldBoundaries={selected.result.walkForward?.foldBoundaries}
                    />
                  </div>
                  {selected.result.walkForward && (
                    <WalkForwardView wf={selected.result.walkForward} copy={copy} language={language} />
                  )}
                  <div>
                    <h3 className="mb-2 text-sm font-semibold text-gray-700">{copy.tradeLog}</h3>
                    <TradeTable trades={selected.result.trades} copy={copy} />
                  </div>
                </div>
              ) : null}
            </div>
          )}
        </section>
      </div>
    </div>
  );
};

// PromoteButton renders the "Promote" CTA + an inline dialog
// that posts the proposal to /api/funds/{fundId}/promotions.
// On success it navigates to the Promotions page so the operator
// can take it through the review / shadow / activate flow.
const PromoteButton: React.FC<{
  fundId: string;
  job: BacktestJob;
  copy: ReturnType<typeof buildCopy>;
}> = ({ fundId, job, copy }) => {
  const navigate = useNavigate();
  const [open, setOpen] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [notes, setNotes] = useState("");
  const [shadowDays, setShadowDays] = useState<number>(7);
  const [decayRatio, setDecayRatio] = useState<number>(0.5);
  const [error, setError] = useState<string | null>(null);

  const onSubmit = useCallback(async () => {
    setError(null);
    setSubmitting(true);
    try {
      await proposePromotion(fundId, {
        basisJobId: job.id,
        notes: notes || undefined,
        shadowDays,
        decayRatio,
      });
      setOpen(false);
      navigate(`/funds/${fundId}/promotions`);
    } catch (err) {
      setError(formatApiError(err, copy.promoteError));
    } finally {
      setSubmitting(false);
    }
  }, [copy.promoteError, decayRatio, fundId, job.id, navigate, notes, shadowDays]);

  if (!open) {
    return (
      <button
        type="button"
        className="rounded border border-indigo-200 bg-indigo-50 px-3 py-1 text-xs font-medium text-indigo-700 hover:bg-indigo-100"
        onClick={() => setOpen(true)}
      >
        {copy.promote}
      </button>
    );
  }
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/30 p-4">
      <div className="w-full max-w-md rounded-lg border border-gray-200 bg-white p-5 shadow-xl">
        <h3 className="text-base font-semibold text-gray-800">{copy.promoteTitle}</h3>
        <p className="mt-1 text-xs text-gray-500">{copy.promoteHint}</p>
        <div className="mt-4 space-y-3">
          <label className="block text-xs font-medium text-gray-700">
            {copy.promoteNotes}
            <textarea
              className="mt-1 w-full rounded border border-gray-200 p-2 text-sm focus:border-indigo-400 focus:outline-none"
              rows={2}
              value={notes}
              onChange={(e) => setNotes(e.target.value)}
            />
          </label>
          <label className="block text-xs font-medium text-gray-700">
            {copy.promoteShadowDays}
            <input
              type="number"
              min={0}
              max={90}
              className="mt-1 w-full rounded border border-gray-200 p-2 text-sm focus:border-indigo-400 focus:outline-none"
              value={shadowDays}
              onChange={(e) => setShadowDays(Number(e.target.value))}
            />
            <span className="mt-1 block text-[10px] text-gray-500">{copy.promoteShadowDaysHint}</span>
          </label>
          <label className="block text-xs font-medium text-gray-700">
            {copy.promoteDecayRatio}
            <input
              type="number"
              min={0.05}
              max={0.95}
              step={0.05}
              className="mt-1 w-full rounded border border-gray-200 p-2 text-sm focus:border-indigo-400 focus:outline-none"
              value={decayRatio}
              onChange={(e) => setDecayRatio(Number(e.target.value))}
            />
            <span className="mt-1 block text-[10px] text-gray-500">{copy.promoteDecayRatioHint}</span>
          </label>
          {error && <p className="text-xs text-red-600">{error}</p>}
        </div>
        <div className="mt-5 flex justify-end gap-2">
          <button
            type="button"
            className="rounded border border-gray-200 bg-white px-3 py-1.5 text-xs font-medium text-gray-700 hover:bg-gray-50"
            onClick={() => setOpen(false)}
            disabled={submitting}
          >
            {copy.promoteCancel}
          </button>
          <button
            type="button"
            className="rounded border border-indigo-600 bg-indigo-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-indigo-700 disabled:opacity-60"
            onClick={onSubmit}
            disabled={submitting}
          >
            {submitting ? copy.promoteSubmitting : copy.promoteSubmit}
          </button>
        </div>
      </div>
    </div>
  );
};

const MetricsGrid: React.FC<{
  metrics: NonNullable<BacktestJob["result"]>["metrics"];
  initial: number;
  final: number;
  copy: ReturnType<typeof buildCopy>;
  language: AppLanguage;
}> = ({ metrics, initial, final, copy, language }) => {
  const pct = (v: number) =>
    `${formatNumberForLanguage(v * 100, language, { maximumFractionDigits: 2 })}%`;
  const cards = [
    { label: copy.cumulative, value: pct(metrics.cumulativeReturn), tone: metrics.cumulativeReturn >= 0 ? "text-emerald-700" : "text-red-700" },
    { label: copy.annualized, value: pct(metrics.annualizedReturn), tone: metrics.annualizedReturn >= 0 ? "text-emerald-700" : "text-red-700" },
    { label: copy.sharpe, value: formatNumberForLanguage(metrics.sharpeRatio, language, { maximumFractionDigits: 2 }), tone: "text-gray-900" },
    { label: copy.volatility, value: pct(metrics.volatility), tone: "text-gray-900" },
    { label: copy.maxDD, value: pct(metrics.maxDrawdown), tone: "text-red-700" },
    { label: copy.winRate, value: pct(metrics.winRate), tone: "text-gray-900" },
    { label: copy.tradeCount, value: String(metrics.tradeCount), tone: "text-gray-900" },
    { label: copy.winLoss, value: `${metrics.winningTradeCount} / ${metrics.losingTradeCount}`, tone: "text-gray-900" },
  ];
  return (
    <div>
      <p className="mb-3 text-xs text-gray-500">
        {formatNumberForLanguage(initial, language, { maximumFractionDigits: 0 })} → {formatNumberForLanguage(final, language, { maximumFractionDigits: 0 })}
      </p>
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        {cards.map((c) => (
          <div key={c.label} className="rounded border border-gray-100 bg-gray-50 px-3 py-2">
            <div className="text-xs uppercase tracking-wide text-gray-500">{c.label}</div>
            <div className={`text-base font-semibold ${c.tone}`}>{c.value}</div>
          </div>
        ))}
      </div>
    </div>
  );
};

// WalkForwardView renders the per-fold breakdown for runs that
// used the walkForward sub-spec: an OOS summary row + a per-fold
// table. We keep the table compact (no per-fold NAV mini-charts)
// because the parent NavChart already shows fold boundaries —
// drilling into a single fold's trades is a future iteration.
const WalkForwardView: React.FC<{
  wf: WalkForwardResultView;
  copy: ReturnType<typeof buildCopy>;
  language: AppLanguage;
}> = ({ wf, copy, language }) => {
  const pct = (v: number) =>
    `${formatNumberForLanguage(v * 100, language, { maximumFractionDigits: 2 })}%`;
  const sharpe = (v: number) => formatNumberForLanguage(v, language, { maximumFractionDigits: 2 });
  const oosTone = wf.oosReturn >= 0 ? "text-emerald-700" : "text-red-700";
  return (
    <div className="rounded border border-emerald-100 bg-emerald-50/40 p-3">
      <div className="mb-3 flex items-baseline justify-between">
        <h3 className="text-sm font-semibold text-gray-700">{copy.walkForwardSection}</h3>
        <span className="text-xs text-emerald-700">
          {wf.mode} · {wf.folds.length} folds
        </span>
      </div>
      <div className="mb-3 grid grid-cols-2 gap-2 sm:grid-cols-5">
        <div className="rounded border border-emerald-100 bg-white px-3 py-2">
          <div className="text-[11px] uppercase tracking-wide text-gray-500">{copy.walkForwardOOSReturn}</div>
          <div className={`text-base font-semibold ${oosTone}`}>{pct(wf.oosReturn)}</div>
        </div>
        <div className="rounded border border-emerald-100 bg-white px-3 py-2">
          <div className="text-[11px] uppercase tracking-wide text-gray-500">{copy.walkForwardOOSSharpe}</div>
          <div className="text-base font-semibold text-gray-900">{sharpe(wf.oosSharpe)}</div>
        </div>
        <div className="rounded border border-emerald-100 bg-white px-3 py-2">
          <div className="text-[11px] uppercase tracking-wide text-gray-500">{copy.walkForwardMeanFold}</div>
          <div className="text-base font-semibold text-gray-900">{pct(wf.meanFoldReturn)}</div>
        </div>
        <div className="rounded border border-emerald-100 bg-white px-3 py-2">
          <div className="text-[11px] uppercase tracking-wide text-gray-500">{copy.walkForwardWorstFold}</div>
          <div className="text-base font-semibold text-red-700">{pct(wf.worstFoldReturn)}</div>
        </div>
        <div className="rounded border border-emerald-100 bg-white px-3 py-2">
          <div className="text-[11px] uppercase tracking-wide text-gray-500">{copy.walkForwardBestFold}</div>
          <div className="text-base font-semibold text-emerald-700">{pct(wf.bestFoldReturn)}</div>
        </div>
      </div>
      <h4 className="mb-2 text-xs font-semibold uppercase tracking-wide text-gray-600">{copy.walkForwardPerFold}</h4>
      <div className="overflow-x-auto">
        <table className="min-w-full text-xs">
          <thead className="border-b border-emerald-100 text-left uppercase tracking-wide text-gray-500">
            <tr>
              <th className="px-2 py-1 font-medium">{copy.walkForwardFoldCol}</th>
              <th className="px-2 py-1 font-medium">{copy.walkForwardWindowCol}</th>
              <th className="px-2 py-1 text-right font-medium">{copy.walkForwardReturnCol}</th>
              <th className="px-2 py-1 text-right font-medium">{copy.walkForwardSharpeCol}</th>
              <th className="px-2 py-1 text-right font-medium">{copy.walkForwardDDCol}</th>
              <th className="px-2 py-1 text-right font-medium">{copy.walkForwardTradesCol}</th>
              <th className="px-2 py-1 font-medium">{copy.walkForwardErrorCol}</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-emerald-50">
            {wf.folds.map((f) => (
              <tr key={f.index}>
                <td className="px-2 py-1 font-medium text-gray-800">#{f.index + 1}</td>
                <td className="px-2 py-1 text-gray-600">
                  {f.testStart.slice(0, 10)} → {f.testEnd.slice(0, 10)}
                </td>
                <td className={`px-2 py-1 text-right font-medium ${f.return >= 0 ? "text-emerald-700" : "text-red-700"}`}>
                  {pct(f.return)}
                </td>
                <td className="px-2 py-1 text-right text-gray-700">{sharpe(f.metrics.sharpeRatio)}</td>
                <td className="px-2 py-1 text-right text-red-600">{pct(f.metrics.maxDrawdown)}</td>
                <td className="px-2 py-1 text-right text-gray-700">{f.tradeCount}</td>
                <td className="px-2 py-1 text-[11px] text-red-700">{f.error ?? ""}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
};

// NavOverlay renders two NAV curves on the same axes,
// normalised to 1.0 at each run's first point. Normalising
// makes runs with different initialCash values visually
// comparable. Color: A = indigo, B = emerald.
//
// Module-scoped chart geometry so useMemo deps stay stable.
const NAV_OVERLAY_WIDTH = 720;
const NAV_OVERLAY_HEIGHT = 240;
const NAV_OVERLAY_PADDING = { top: 14, right: 14, bottom: 32, left: 56 };

const NavOverlay: React.FC<{
  a: BacktestNavPoint[];
  b: BacktestNavPoint[];
  copy: ReturnType<typeof buildCopy>;
}> = ({ a, b, copy }) => {
  const width = NAV_OVERLAY_WIDTH;
  const height = NAV_OVERLAY_HEIGHT;
  const padding = NAV_OVERLAY_PADDING;

  const chart = useMemo(() => {
    if (a.length < 2 && b.length < 2) return null;
    const normalize = (curve: BacktestNavPoint[]) => {
      if (curve.length === 0) return [];
      const base = curve[0].nav || 1;
      return curve.map((p) => ({ date: p.date, value: p.nav / base }));
    };
    const sa = normalize(a);
    const sb = normalize(b);
    const all = [...sa, ...sb];
    const values = all.map((p) => p.value);
    if (values.length === 0) return null;
    const minV = Math.min(...values, 1);
    const maxV = Math.max(...values, 1);
    const range = maxV - minV || 1;
    const innerW = NAV_OVERLAY_WIDTH - NAV_OVERLAY_PADDING.left - NAV_OVERLAY_PADDING.right;
    const innerH = NAV_OVERLAY_HEIGHT - NAV_OVERLAY_PADDING.top - NAV_OVERLAY_PADDING.bottom;
    // Both series may have different lengths — use parallel x
    // axes (fraction of own length) so the curves stretch to fit
    // the same rectangle. This is what users intuit from "same
    // run length" comparisons.
    const project = (curve: { date: string; value: number }[]) => {
      if (curve.length === 0) return { path: "", dots: [] as { x: number; y: number; v: number; d: string }[] };
      const step = innerW / Math.max(1, curve.length - 1);
      const dots = curve.map((p, i) => ({
        x: NAV_OVERLAY_PADDING.left + i * step,
        y: NAV_OVERLAY_PADDING.top + innerH - ((p.value - minV) / range) * innerH,
        v: p.value,
        d: p.date,
      }));
      const path = dots.map((pt, i) => `${i === 0 ? "M" : "L"} ${pt.x.toFixed(1)} ${pt.y.toFixed(1)}`).join(" ");
      return { path, dots };
    };
    const pa = project(sa);
    const pb = project(sb);
    const ticks = 4;
    const yLabels = Array.from({ length: ticks + 1 }, (_, i) => {
      const v = minV + (range * i) / ticks;
      const y = NAV_OVERLAY_PADDING.top + innerH - ((v - minV) / range) * innerH;
      return { y, v };
    });
    return { pa, pb, yLabels };
  }, [a, b]);

  if (!chart) {
    return <p className="text-sm text-gray-500">—</p>;
  }
  return (
    <div className="overflow-x-auto">
      <svg width={width} height={height} role="img" aria-label="NAV overlay">
        <rect x={padding.left} y={padding.top} width={width - padding.left - padding.right} height={height - padding.top - padding.bottom} fill="#f9fafb" stroke="#e5e7eb" />
        {chart.yLabels.map((t, i) => (
          <g key={i}>
            <line x1={padding.left} x2={width - padding.right} y1={t.y} y2={t.y} stroke="#e5e7eb" strokeDasharray="2 3" />
            <text x={padding.left - 6} y={t.y + 3} fontSize="10" textAnchor="end" fill="#6b7280">
              {t.v.toFixed(2)}
            </text>
          </g>
        ))}
        {chart.pa.path && (
          <path d={chart.pa.path} fill="none" stroke="#4f46e5" strokeWidth="1.75" />
        )}
        {chart.pb.path && (
          <path d={chart.pb.path} fill="none" stroke="#059669" strokeWidth="1.75" />
        )}
      </svg>
      <div className="mt-2 flex items-center gap-4 text-xs text-gray-600">
        <span className="flex items-center gap-1">
          <span className="inline-block h-2 w-3 rounded bg-indigo-600" /> {copy.compareLegendA}
        </span>
        <span className="flex items-center gap-1">
          <span className="inline-block h-2 w-3 rounded bg-emerald-600" /> {copy.compareLegendB}
        </span>
      </div>
    </div>
  );
};

// CompareView renders the side-by-side B-vs-A panel. Layout:
// header + warning banners + NAV overlay + metrics matrix
// (3 columns: A | B | Δ). Trades are omitted intentionally —
// the single-job view is the right place to dig into trades.
const CompareView: React.FC<{
  comparison: BacktestComparison;
  copy: ReturnType<typeof buildCopy>;
  language: AppLanguage;
  onExit: () => void;
}> = ({ comparison, copy, language, onExit }) => {
  const { a, b, diff } = comparison;
  const pct = (v: number) =>
    `${formatNumberForLanguage(v * 100, language, { maximumFractionDigits: 2 })}%`;
  const num = (v: number, frac = 2) =>
    formatNumberForLanguage(v, language, { maximumFractionDigits: frac });
  // signed: format the delta with explicit sign, color-coded.
  // For drawdown (where smaller magnitude is better), the
  // caller flips the tone — we just colour by raw sign here.
  const signed = (v: number, fmt: (n: number) => string, invert = false) => {
    const positive = invert ? v < 0 : v > 0;
    const negative = invert ? v > 0 : v < 0;
    const tone = positive ? "text-emerald-700" : negative ? "text-red-700" : "text-gray-600";
    const sign = v > 0 ? "+" : "";
    return <span className={tone}>{sign}{fmt(v)}</span>;
  };
  // Rows in the metrics table. `invert` is set on drawdown so a
  // smaller absolute drawdown shows up green.
  const rows: Array<{
    label: string;
    aVal: string;
    bVal: string;
    delta: React.ReactNode;
  }> = [
    {
      label: copy.cumulative,
      aVal: pct(a.result?.metrics.cumulativeReturn ?? 0),
      bVal: pct(b.result?.metrics.cumulativeReturn ?? 0),
      delta: signed(diff.cumulativeReturnDelta, pct),
    },
    {
      label: copy.annualized,
      aVal: pct(a.result?.metrics.annualizedReturn ?? 0),
      bVal: pct(b.result?.metrics.annualizedReturn ?? 0),
      delta: signed(diff.annualizedReturnDelta, pct),
    },
    {
      label: copy.sharpe,
      aVal: num(a.result?.metrics.sharpeRatio ?? 0),
      bVal: num(b.result?.metrics.sharpeRatio ?? 0),
      delta: signed(diff.sharpeDelta, (v) => num(v)),
    },
    {
      label: copy.volatility,
      aVal: pct(a.result?.metrics.volatility ?? 0),
      bVal: pct(b.result?.metrics.volatility ?? 0),
      delta: signed(diff.volatilityDelta, pct, true),
    },
    {
      label: copy.maxDD,
      aVal: pct(a.result?.metrics.maxDrawdown ?? 0),
      bVal: pct(b.result?.metrics.maxDrawdown ?? 0),
      // maxDrawdown is reported as a positive magnitude — a
      // smaller B is better, so invert tone.
      delta: signed(diff.maxDrawdownDelta, pct, true),
    },
    {
      label: copy.winRate,
      aVal: pct(a.result?.metrics.winRate ?? 0),
      bVal: pct(b.result?.metrics.winRate ?? 0),
      delta: signed(diff.winRateDelta, pct),
    },
    {
      label: copy.tradeCount,
      aVal: String(a.result?.metrics.tradeCount ?? 0),
      bVal: String(b.result?.metrics.tradeCount ?? 0),
      delta: signed(diff.tradeCountDelta, (v) => num(v, 0)),
    },
  ];
  return (
    <div className="rounded-lg border border-gray-200 bg-white p-4">
      <header className="mb-4 flex items-start justify-between gap-3">
        <div>
          <h2 className="text-base font-semibold text-gray-800">{copy.compareTitle}</h2>
          <p className="mt-1 flex flex-wrap items-center gap-2 text-xs text-gray-500">
            <span className="rounded bg-indigo-100 px-1.5 py-0.5 text-[10px] font-bold text-indigo-700">A</span>
            <span>{a.name || a.id.slice(0, 8)}</span>
            <span className="text-gray-300">·</span>
            <span className="rounded bg-emerald-100 px-1.5 py-0.5 text-[10px] font-bold text-emerald-700">B</span>
            <span>{b.name || b.id.slice(0, 8)}</span>
          </p>
        </div>
        <button
          type="button"
          className="rounded border border-gray-200 bg-white px-2 py-1 text-xs text-gray-600 hover:bg-gray-50"
          onClick={onExit}
        >
          {copy.compareExit}
        </button>
      </header>

      <div className="mb-3 flex flex-wrap gap-2 text-[11px]">
        <span className={`rounded-full border px-2 py-0.5 ${diff.sameWindow ? "border-emerald-200 bg-emerald-50 text-emerald-700" : "border-amber-200 bg-amber-50 text-amber-700"}`}>
          {diff.sameWindow ? copy.compareSameWindow : copy.compareDifferentWindow}
        </span>
        <span className={`rounded-full border px-2 py-0.5 ${diff.sameUniverse ? "border-emerald-200 bg-emerald-50 text-emerald-700" : "border-amber-200 bg-amber-50 text-amber-700"}`}>
          {diff.sameUniverse ? copy.compareSameUniverse : copy.compareDifferentUniverse}
        </span>
      </div>

      <div className="mb-5">
        <h3 className="mb-2 text-sm font-semibold text-gray-700">{copy.compareNavOverlay}</h3>
        <NavOverlay a={a.result?.navCurve ?? []} b={b.result?.navCurve ?? []} copy={copy} />
      </div>

      <div className="overflow-hidden rounded border border-gray-100">
        <table className="w-full text-sm">
          <thead className="bg-gray-50">
            <tr>
              <th className="px-3 py-2 text-left text-xs font-semibold uppercase tracking-wide text-gray-500">
                {copy.results}
              </th>
              <th className="px-3 py-2 text-right text-xs font-semibold uppercase tracking-wide text-indigo-700">A</th>
              <th className="px-3 py-2 text-right text-xs font-semibold uppercase tracking-wide text-emerald-700">B</th>
              <th className="px-3 py-2 text-right text-xs font-semibold uppercase tracking-wide text-gray-500">
                {copy.compareDelta}
              </th>
            </tr>
          </thead>
          <tbody className="divide-y divide-gray-100">
            {rows.map((r) => (
              <tr key={r.label}>
                <td className="px-3 py-2 text-gray-700">{r.label}</td>
                <td className="px-3 py-2 text-right font-medium text-gray-900">{r.aVal}</td>
                <td className="px-3 py-2 text-right font-medium text-gray-900">{r.bVal}</td>
                <td className="px-3 py-2 text-right font-medium">{r.delta}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
};

// SweepView renders a parameter-sweep result as a 2D grid (or
// 1D strip for single-axis sweeps). Each cell shows the
// cumulative return of one child run, color-coded green for
// positive / red for negative, with status pills for runs that
// haven't completed yet. Click a cell to drill into that
// child's full single-run view.
const SweepView: React.FC<{
  sweep: BacktestSweep;
  loading: boolean;
  copy: ReturnType<typeof buildCopy>;
  language: AppLanguage;
  onExit: () => void;
  onPickJob: (jobId: string) => void;
}> = ({ sweep, loading, copy, language, onExit, onPickJob }) => {
  // Build the grid: index children by their (rowKey, colKey)
  // tuple. For 1D sweeps colKey is "_" so the grid collapses to
  // a single column. `axes` is derived inline so the dependency
  // array stays stable when sweep is the same reference.
  const grid = useMemo(() => {
    const axes = sweep.axes ?? [];
    const children = sweep.children ?? [];
    const rowAxis = axes[0];
    const colAxis = axes[1];
    const lookup = new Map<string, (typeof children)[number]>();
    for (const c of children) {
      const r = rowAxis ? c.axisValues[rowAxis.name] ?? "" : "_";
      const k = colAxis ? c.axisValues[colAxis.name] ?? "" : "_";
      lookup.set(`${r}|${k}`, c);
    }
    return { lookup, rowAxis, colAxis };
  }, [sweep]);

  // Color scale for cumulative return: map [-maxAbs, +maxAbs] →
  // [red 800, white, green 700]. We use perceived-luminance
  // anchors instead of straight HSL so the gradient stays
  // legible against the page background.
  const colorFor = (value: number, maxAbs: number) => {
    if (maxAbs <= 0) return "bg-gray-50 text-gray-700";
    const norm = Math.max(-1, Math.min(1, value / maxAbs));
    if (norm > 0.66) return "bg-emerald-200 text-emerald-900";
    if (norm > 0.33) return "bg-emerald-100 text-emerald-800";
    if (norm > 0) return "bg-emerald-50 text-emerald-700";
    if (norm > -0.33) return "bg-red-50 text-red-700";
    if (norm > -0.66) return "bg-red-100 text-red-800";
    return "bg-red-200 text-red-900";
  };

  const maxAbs = useMemo(() => {
    let m = 0;
    for (const c of sweep.children ?? []) {
      const v = c.job.result?.metrics.cumulativeReturn ?? 0;
      if (Math.abs(v) > m) m = Math.abs(v);
    }
    return m;
  }, [sweep]);

  const rowVals = grid.rowAxis?.values ?? ["_"];
  const colVals = grid.colAxis?.values ?? ["_"];

  return (
    <div className="rounded-lg border border-gray-200 bg-white p-4">
      <header className="mb-4 flex items-start justify-between gap-3">
        <div>
          <h2 className="text-base font-semibold text-gray-800">{copy.sweepGrid}</h2>
          <p className="mt-1 flex flex-wrap items-center gap-2 text-xs text-gray-500">
            <span>{sweep.name || sweep.id.slice(0, 8)}</span>
            <span className={`rounded-full border px-2 py-0.5 text-[10px] font-medium ${statusTone(sweep.status)}`}>
              {sweep.status}
            </span>
            <span>{copy.sweepDoneOf.replace("{done}", String(sweep.doneCells)).replace("{total}", String(sweep.totalCells))}</span>
            <span>{formatDateTimeForLanguage(sweep.createdAt, language)}</span>
          </p>
        </div>
        <button
          type="button"
          className="rounded border border-gray-200 bg-white px-2 py-1 text-xs text-gray-600 hover:bg-gray-50"
          onClick={onExit}
        >
          {copy.sweepExit}
        </button>
      </header>

      {loading && <p className="mb-3 text-xs text-gray-500">…</p>}

      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr>
              <th className="border-b border-gray-100 bg-gray-50 px-2 py-1 text-left text-xs font-semibold text-gray-500">
                {grid.rowAxis?.name ?? "—"} \ {grid.colAxis?.name ?? ""}
              </th>
              {colVals.map((c) => (
                <th key={c} className="border-b border-gray-100 bg-gray-50 px-3 py-1 text-center text-xs font-semibold text-gray-700">
                  {c === "_" ? "" : c}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rowVals.map((r) => (
              <tr key={r}>
                <th className="bg-gray-50 px-2 py-2 text-left text-xs font-semibold text-gray-700">
                  {r === "_" ? "" : r}
                </th>
                {colVals.map((c) => {
                  const cell = grid.lookup.get(`${r}|${c}`);
                  if (!cell) {
                    return (
                      <td key={c} className="px-2 py-2 text-center text-xs text-gray-300">
                        —
                      </td>
                    );
                  }
                  const cumret = cell.job.result?.metrics.cumulativeReturn;
                  const sharpe = cell.job.result?.metrics.sharpeRatio;
                  const tone = cell.job.status === "completed" && cumret != null
                    ? colorFor(cumret, maxAbs)
                    : "bg-gray-50 text-gray-600";
                  return (
                    <td
                      key={c}
                      className={`cursor-pointer border border-white px-2 py-2 text-center align-top transition hover:ring-2 hover:ring-indigo-300 ${tone}`}
                      onClick={() => onPickJob(cell.job.id)}
                      title={cell.job.id}
                    >
                      {cell.job.status === "completed" && cumret != null ? (
                        <div className="space-y-0.5">
                          <div className="text-base font-semibold">
                            {(cumret * 100).toFixed(2)}%
                          </div>
                          {sharpe != null && (
                            <div className="text-[10px] text-gray-600">
                              {copy.sharpe}: {sharpe.toFixed(2)}
                            </div>
                          )}
                        </div>
                      ) : (
                        <span className={`rounded-full border px-2 py-0.5 text-[10px] font-medium ${statusTone(cell.job.status)}`}>
                          {cell.job.status}
                        </span>
                      )}
                    </td>
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
};

export default Backtest;
