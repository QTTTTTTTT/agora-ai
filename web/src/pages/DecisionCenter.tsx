import React, { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import {
  apiGet,
  apiPost,
  buildPortfolioQuotesStreamUrl,
  fetchFundDecisionTrace,
  formatApiError,
  type DecisionTrace,
  type DecisionTraceActionExecution,
  type DecisionTraceReviewEntry,
  type MarketResearch,
  type PortfolioQuote,
  type PortfolioQuotesFrame,
} from "../lib/api";
import LiveReadinessBanner from "../components/LiveReadinessBanner";
import {
  formatDateForLanguage,
  formatDateTimeForLanguage,
  formatNumberForLanguage,
  useAppPreferences,
} from "../lib/preferences";
import { ActionListCard } from "./decisionCenter/ActionListCard";
import { ApprovalActions } from "./decisionCenter/ApprovalActions";
import { CommitteeMemoCard } from "./decisionCenter/CommitteeMemoCard";
import { PlanListSidebar } from "./decisionCenter/PlanListSidebar";
import { PriceRefreshDialog, type PriceRefreshRow } from "./decisionCenter/PriceRefreshDialog";
import { RiskConclusionCard } from "./decisionCenter/RiskConclusionCard";
import { TraceboardCard } from "./decisionCenter/TraceboardCard";
import { DecisionSourceChip } from "../components/DecisionSourceChip";
import { DecisionReasonModal } from "../components/DecisionReasonModal";
import NextRunBanner from "../components/NextRunBanner";
import {
  canReviewPlan,
  compactStrategyReasoning,
  expandWorkflowSteps,
  humanizeValue,
  isEmptyObject,
  isPendingPlan,
  normalizeTraceStepKey,
  normalizeTradeActionKey,
  normalizeTradingDateParam,
  parseRiskReview,
  pickLocalizedList,
  pickLocalizedText,
  planEffectiveStatusKey,
} from "./decisionCenter/helpers";
import type { ApiPlan, ExecutionTraceView } from "./decisionCenter/types";

const DecisionCenter: React.FC = () => {
  const { fundId } = useParams<{ fundId: string }>();
  const { language, displayCurrency } = useAppPreferences();
  const [searchParams, setSearchParams] = useSearchParams();
  const [plans, setPlans] = useState<ApiPlan[]>([]);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Sprint 12-alt — accept ?planId=... to drive deep-links from the
  // admin LLM-health board. Initialised once from the query string so
  // subsequent navigations to the same fund without a planId param
  // don't clobber the user's clicks; the param is also cleared from
  // the URL once we've consumed it (see consumeQueryPlanIdEffect).
  const queryPlanId = searchParams.get("planId");
  const [selectedId, setSelectedId] = useState<string | null>(queryPlanId);
  const [showHistory, setShowHistory] = useState(true);
  const [actionError, setActionError] = useState<string | null>(null);
  // actionSuccess is a transient confirmation banner shown for ~3s
  // after a successful approve / reject / refresh. Kept separate from
  // actionError so the success path doesn't paint into the red panel.
  // Auto-dismisses via the timer set in the helpers below.
  const [actionSuccess, setActionSuccess] = useState<string | null>(null);
  // Price-refresh dialog state. After the user clicks "Refresh quote"
  // we compare each action's price before and after the API call; if
  // any drift exceeds PRICE_DRIFT_DIALOG_THRESHOLD we surface a modal
  // listing the changes so the user can re-confirm before approving.
  // The dialog is purely advisory — it doesn't block approval — because
  // the backend SlippageGuard provides the hard safety net at
  // execution time.
  const [priceRefreshDialogOpen, setPriceRefreshDialogOpen] = useState(false);
  const [priceRefreshRows, setPriceRefreshRows] = useState<PriceRefreshRow[]>([]);
  // submitting / showRejectBox / rejectReason were lifted into ApprovalActions
  // so typing a reject reason no longer re-runs this 1500-line component on
  // every keystroke. The remaining `actionError` lives here because it is
  // surfaced above the grid (outside the approval card) when any of approve /
  // reject / refresh-quote fails.
  const [decisionTraceByPlanKey, setDecisionTraceByPlanKey] = useState<Record<string, DecisionTrace | null>>({});
  const [decisionTraceErrorByPlanKey, setDecisionTraceErrorByPlanKey] = useState<Record<string, string | null>>({});
  const [decisionTraceLoading, setDecisionTraceLoading] = useState(false);
  // Bumping this counter triggers the trace effect to re-fetch even
  // when the plan key already has an entry in the cache; used by the
  // "Retry" button to recover from a previous fetch error.
  const [decisionTraceRetryCounter, setDecisionTraceRetryCounter] = useState(0);

  // "Why this decision?" drill-down modal — aggregates every
  // reason / reasoning surface for the currently selected plan
  // into one scrollable view (PM thesis, strategy summary,
  // per-action reasons, debate, memo, risk review, blocking
  // reasons, raw JSON). See components/DecisionReasonModal.tsx
  // for the rationale.
  const [reasonModalOpen, setReasonModalOpen] = useState(false);
  // PR-4: live quotes pushed from the SSE stream, keyed by symbol so
  // the ActionListCard can render the "现价" column without re-rendering
  // the entire plan list on every tick.
  const [liveQuotesBySymbol, setLiveQuotesBySymbol] = useState<Record<string, PortfolioQuote>>({});

  useEffect(() => {
    if (!fundId) {
      setLiveQuotesBySymbol({});
      return;
    }
    const source = new EventSource(buildPortfolioQuotesStreamUrl(fundId), { withCredentials: true });
    const handleQuotes = (event: MessageEvent) => {
      let frame: PortfolioQuotesFrame;
      try {
        frame = JSON.parse(event.data) as PortfolioQuotesFrame;
      } catch {
        return;
      }
      if (!frame || !Array.isArray(frame.quotes) || frame.quotes.length === 0) {
        return;
      }
      setLiveQuotesBySymbol((current) => {
        const next = { ...current };
        for (const q of frame.quotes) {
          if (!q.symbol) continue;
          next[q.symbol.toUpperCase()] = q;
        }
        return next;
      });
    };
    source.addEventListener("quotes", handleQuotes as EventListener);
    return () => {
      source.removeEventListener("quotes", handleQuotes as EventListener);
      source.close();
    };
  }, [fundId]);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            loading: "Loading investment plans...",
            refreshing: "Refreshing latest plans...",
            missingFundId: "Missing fundId",
            loadError: "Failed to load investment plans",
            approveError: "Failed to approve plan",
            rejectError: "Failed to reject plan",
            refreshQuoteError: "Failed to refresh quote. Try again later.",
            refreshQuotePlan: "Refresh quote",
            refreshQuoteSuccess: "Quote refreshed; plan updated with the latest pricing.",
            refreshQuoteNoMaterialChange: "Prices are within 0.3% of the prior quote.",
            priceRefreshTitle: "Prices changed since you opened this plan",
            priceRefreshSubtitle: "These actions moved more than 0.3%. Review the new prices before approving.",
            priceRefreshColumnSymbol: "Symbol",
            priceRefreshColumnOldPrice: "Old price",
            priceRefreshColumnNewPrice: "New price",
            priceRefreshColumnDrift: "Drift",
            priceRefreshAcknowledge: "I've reviewed the changes",
            retry: "Retry",
            traceLoadFailed: "Failed to load decision trace",
            successApproved: "Plan approved — queued for execution",
            successRejected: "Plan rejected",
            successRefreshed: "Prices refreshed",
            title: "Decision center",
            subtitle: "Review pending plans, action details, and risk conclusions in one place, then approve or reject them here.",
            emptyTitle: "No investment plans yet",
            emptyDescription: "Start the strategy workflow to generate today’s plan, then it will enter the approval queue here.",
            backToDashboard: "Back to dashboard",
            pendingPlans: "Pending plans",
            noPendingPlans: "There are no pending plans right now. New strategy conclusions will appear here first for review.",
            historyPlans: "History",
            collapse: "Collapse",
            expand: "Expand",
            noHistoryPlans: "There is no plan history yet. Once the first review completes, approved and rejected plans will accumulate here.",
            selectPlan: "Select a plan from the left to view details.",
            pendingCount: "Pending",
            historyCount: "History",
            totalCount: "Total",
            planDate: "Plan date",
            updatedAt: "Updated",
            expectedReturn: "Expected return",
            riskScore: "Risk score",
            actionCount: "Actions",
            portfolioManager: "Portfolio manager",
            discussionSession: "Discussion session",
            notRecorded: "Not recorded",
            rejectedReason: "Rejection note",
            strategyReasoning: "PM reasoning",
            noRejectedReason: "No rejection note was provided.",
            noStrategyReasoning: "This plan does not include additional strategy reasoning.",
            actionList: "Action list",
            totalRows: "rows",
            noActions: "This plan does not contain any action details yet.",
            columns: {
              instrument: "Instrument",
              profile: "Market profile",
              action: "Action",
              quantity: "Quantity",
              price: "Plan price",
              livePrice: "Live price",
              amount: "Amount",
              stopLoss: "Stop loss",
              takeProfit: "Take profit",
              confidence: "Confidence",
              supporters: "Supporters",
            },
            livePriceDriftWarning: (percent: string) => `Drift ${percent}% from plan — consider refreshing quote before approval.`,
            livePriceUnavailable: "—",
            none: "None",
            actionReasonMissing: "This action does not include additional reasoning.",
            executionStatus: "Execution status",
            contractMultiplier: "Contract multiplier",
            expiryDate: "Expiry",
            opposedBy: "Opposed by",
            reduceOnly: "Reduce only",
            marketResearch: "Market research",
            researchSummary: "Research summary",
            researchSignals: "Signals",
            researchNews: "News",
            researchQuote: "Reference quote",
            researchUnavailable: "No market research yet.",
            quoteStaleBadge: "Stale",
            quoteStaleHint: "Quote may be outdated; refresh before executing.",
            newsLanguageZh: "ZH",
            newsLanguageEn: "EN",
            researchNotes: "Provider notes",
            traceboard: "Decision traceboard",
            traceboardSubtitle: "Follow the workflow, discussion, execution, and review records for the selected plan.",
            workflowTimeline: "Workflow timeline",
            workflowState: "Workflow state",
            workflowCurrentStep: "Current step",
            workflowStartedAt: "Started",
            workflowCompletedAt: "Completed",
            workflowUpdatedAt: "Updated",
            workflowError: "Error",
            workflowNoData: "No workflow trace is available for this plan yet.",
            discussionTrace: "Discussion trace",
            discussionSummary: "Summary",
            discussionConsensus: "Consensus",
            discussionReasoning: "Reasoning",
            discussionSnapshot: "Snapshot",
            discussionFallback: "No discussion trace summary is available yet.",
            committeeMemo: "Investment committee memo",
            committeeMemoSubtitle: "A readable meeting record stitched from roundtable discussion, PM decision, risk review, and trader execution context.",
            meetingSummary: "Meeting summary",
            marketBackground: "Market background",
            participants: "Participants",
            agentViews: "Agent viewpoints",
            keyContentions: "Key contentions",
            finalDecision: "Final PM decision",
            riskOpinion: "Risk opinion",
            traderSuggestions: "Trader suggestions",
            traceLinks: "Trace links",
            noCommitteeMemo: "No committee memo has been generated for this plan yet.",
            noContentions: "No explicit disagreement was recorded.",
            stance: "Stance",
            evidence: "Evidence",
            executionTrace: "Execution trace",
            executionStatusLabel: "Overall execution",
            traceTrades: "Trades",
            traceNoTrades: "No trades have been linked to this action yet.",
            traceNoExecution: "No execution trace is available yet.",
            tradeId: "Trade ID",
            tradeSide: "Side",
            tradeStatus: "Trade status",
            filledQuantity: "Filled quantity",
            filledPrice: "Filled price",
            orderType: "Order type",
            executedAt: "Executed",
            reviewMemory: "Review & memory",
            reviewLayer: "Layer",
            reviewUpdatedAt: "Updated",
            reviewNoEntries: "No review or memory entries are available for this date yet.",
            loadingTraceboard: "Loading decision trace...",
            loadingTraceboardHint: "Core plan data is ready. AI-enhanced trace details are still loading.",
            traceStepStatus: {
              completed: "Completed",
              success: "Completed",
              running: "Running",
              in_progress: "Running",
              pending: "Pending",
              queued: "Pending",
              failed: "Failed",
              error: "Failed",
              skipped: "Skipped",
              cancelled: "Cancelled",
              canceled: "Cancelled",
            },
            workflowSteps: {
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
            riskConclusion: "Risk conclusion",
            riskExplanation: "Risk rule explanation",
            blockingReasons: "Blocking reasons",
            riskWarnings: "Warnings",
            adjustmentAdvice: "Adjustment advice",
            userImpact: "User impact",
            currentValue: "Current",
            thresholdValue: "Threshold",
            noRiskChecks: "The risk result does not contain granular checks yet.",
            noRiskConclusion: "This plan does not have a displayable risk conclusion yet.",
            approvalActions: "Approval actions",
            rejectReasonLabel: "Reject reason",
            rejectReasonPlaceholder: "Explain why the current plan should not move into execution.",
            cancel: "Cancel",
            confirmReject: "Confirm rejection",
            approvePlan: "Approve plan",
            rejectPlan: "Reject plan",
            loadingPlanTitleFallback: "Investment plan",
            unknown: "Unknown",
            unset: "Unset",
            spotLong: "Spot / long",
            checkName: (index: number) => `Check ${index}`,
            missingCheckDetail: "No additional detail was provided.",
            missingRiskNote: "No extra note was included in this risk conclusion.",
            summaryLabel: {
              approved: "Approved",
              rejected: "Rejected",
              pending: "Pending",
              pending_user: "Pending",
            },
            riskVerdict: {
              approved: "Approved",
              rejected: "Rejected",
              pending: "Pending",
              pass: "Pass",
              warn: "Warning",
              fail: "Failed",
            },
            planStatus: {
              pending: "Pending",
              pending_user: "Pending",
              approved: "Approved",
              rejected: "Rejected",
              completed: "Completed",
              mixed: "Partial fill",
              watch_only: "Watch only",
            },
            actionType: {
              buy: "Buy",
              sell: "Sell",
              hold: "Hold",
              reduce: "Reduce",
              add: "Add",
              watch: "Watch",
            },
            checkResult: {
              pass: "Pass",
              warn: "Warning",
              fail: "Failed",
            },
            executionStatuses: {
              pending: "Pending",
              submitted: "Submitted",
              filled: "Filled",
              partially_filled: "Partially filled",
              cancelled: "Cancelled",
              canceled: "Cancelled",
              rejected: "Rejected",
            },
            positionSides: {
              long: "Long",
              short: "Short",
            },
            openClose: {
              open: "Open",
              close: "Close",
              close_today: "Close today",
              roll: "Roll",
            },
          }
        : {
            loading: "正在加载决策计划...",
            refreshing: "正在刷新最新计划...",
            missingFundId: "缺少 fundId",
            loadError: "加载投资计划失败",
            approveError: "通过计划失败",
            rejectError: "驳回计划失败",
            refreshQuoteError: "刷新报价失败，请稍后再试。",
            refreshQuotePlan: "刷新报价",
            refreshQuoteSuccess: "已刷新最新报价并更新计划。",
            refreshQuoteNoMaterialChange: "报价变动小于 0.3%，无需重新确认。",
            priceRefreshTitle: "价格自您打开此计划后发生了变动",
            priceRefreshSubtitle: "以下动作的价格变动超过 0.3%，请确认后再通过计划。",
            priceRefreshColumnSymbol: "标的",
            priceRefreshColumnOldPrice: "原价格",
            priceRefreshColumnNewPrice: "新价格",
            priceRefreshColumnDrift: "变动幅度",
            priceRefreshAcknowledge: "我已确认价格变动",
            retry: "重试",
            traceLoadFailed: "决策轨迹加载失败",
            successApproved: "计划已通过，已排入执行队列",
            successRejected: "计划已驳回",
            successRefreshed: "报价已刷新",
            title: "决策中心",
            subtitle: "集中查看待审批计划、动作明细与风控结论，并在这里完成通过或驳回。",
            emptyTitle: "当前还没有投资计划",
            emptyDescription: "先启动策略流程生成今日计划，产出后就会在这里进入审批流程。",
            backToDashboard: "返回基金总览",
            pendingPlans: "待审批计划",
            noPendingPlans: "当前没有待审批计划。新的策略结论生成后，会优先出现在这里等待你处理。",
            historyPlans: "历史计划",
            collapse: "收起",
            expand: "展开",
            noHistoryPlans: "还没有历史计划。等首轮审批完成后，这里会沉淀已通过或已驳回的历史记录。",
            selectPlan: "请选择左侧计划查看详情。",
            pendingCount: "待审批",
            historyCount: "历史计划",
            totalCount: "总计划数",
            planDate: "计划日期",
            updatedAt: "最近更新",
            expectedReturn: "预期收益",
            riskScore: "风险评分",
            actionCount: "动作数",
            portfolioManager: "组合经理成员",
            discussionSession: "讨论会话",
            notRecorded: "未记录",
            rejectedReason: "驳回说明",
            strategyReasoning: "组合经理策略说明",
            noRejectedReason: "当前没有填写驳回说明。",
            noStrategyReasoning: "当前计划没有附带更多策略说明。",
            actionList: "动作清单",
            totalRows: "条",
            noActions: "当前计划还没有动作明细。",
            columns: {
              instrument: "标的",
              profile: "市场画像",
              action: "动作",
              quantity: "数量",
              price: "计划价",
              livePrice: "现价",
              amount: "金额",
              stopLoss: "止损",
              takeProfit: "止盈",
              confidence: "置信度",
              supporters: "支持方",
            },
            livePriceDriftWarning: (percent: string) => `偏离计划价 ${percent}%，建议审批前刷新行情。`,
            livePriceUnavailable: "—",
            none: "暂无",
            actionReasonMissing: "当前动作没有附带更多说明。",
            executionStatus: "执行状态",
            contractMultiplier: "合约乘数",
            expiryDate: "到期日",
            opposedBy: "反对方",
            reduceOnly: "仅减仓",
            marketResearch: "市场研究",
            researchSummary: "研究摘要",
            researchSignals: "信号",
            researchNews: "资讯",
            researchQuote: "参考行情",
            researchUnavailable: "暂无市场研究数据。",
            quoteStaleBadge: "已过期",
            quoteStaleHint: "报价已超时，执行前请先刷新。",
            newsLanguageZh: "中",
            newsLanguageEn: "英",
            researchNotes: "数据源备注",
            traceboard: "决策追踪看板",
            traceboardSubtitle: "查看当前计划的流程进度、讨论结论、执行映射与复盘记忆。",
            workflowTimeline: "流程时间线",
            workflowState: "流程状态",
            workflowCurrentStep: "当前步骤",
            workflowStartedAt: "开始时间",
            workflowCompletedAt: "完成时间",
            workflowUpdatedAt: "最近更新",
            workflowError: "错误",
            workflowNoData: "当前计划还没有流程追踪记录。",
            discussionTrace: "讨论轨迹",
            discussionSummary: "摘要",
            discussionConsensus: "共识",
            discussionReasoning: "推理说明",
            discussionSnapshot: "快照",
            discussionFallback: "当前还没有可展示的讨论摘要。",
            committeeMemo: "投资委员会纪要",
            committeeMemoSubtitle: "把圆桌讨论、PM 决策、风控复核与交易执行上下文整理成可读会议记录。",
            meetingSummary: "会议摘要",
            marketBackground: "市场背景",
            participants: "参会角色",
            agentViews: "Agent 观点",
            keyContentions: "关键争议",
            finalDecision: "最终 PM 决策",
            riskOpinion: "风控意见",
            traderSuggestions: "交易执行建议",
            traceLinks: "追溯链接",
            noCommitteeMemo: "当前计划还没有生成投资委员会纪要。",
            noContentions: "未记录明确分歧。",
            stance: "立场",
            evidence: "依据",
            executionTrace: "执行轨迹",
            executionStatusLabel: "整体执行",
            traceTrades: "成交记录",
            traceNoTrades: "当前动作还没有关联成交记录。",
            traceNoExecution: "当前还没有执行轨迹数据。",
            tradeId: "成交单号",
            tradeSide: "方向",
            tradeStatus: "成交状态",
            filledQuantity: "成交数量",
            filledPrice: "成交均价",
            orderType: "订单类型",
            executedAt: "执行时间",
            reviewMemory: "复盘与记忆",
            reviewLayer: "层级",
            reviewUpdatedAt: "更新时间",
            reviewNoEntries: "当前日期还没有复盘或记忆记录。",
            loadingTraceboard: "正在加载决策追踪...",
            loadingTraceboardHint: "基础计划信息已可查看，AI 增强的追踪细节仍在生成。",
            traceStepStatus: {
              completed: "已完成",
              success: "已完成",
              running: "进行中",
              in_progress: "进行中",
              pending: "待执行",
              queued: "待执行",
              failed: "失败",
              error: "失败",
              skipped: "已跳过",
              cancelled: "已取消",
              canceled: "已取消",
            },
            workflowSteps: {
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
            riskConclusion: "风控结论",
            riskExplanation: "风控规则解释",
            blockingReasons: "阻塞原因",
            riskWarnings: "风险提醒",
            adjustmentAdvice: "调整建议",
            userImpact: "用户影响",
            currentValue: "当前值",
            thresholdValue: "阈值",
            noRiskChecks: "当前风控结果没有细分检查项。",
            noRiskConclusion: "当前计划还没有可展示的风控结论。",
            approvalActions: "审批操作",
            rejectReasonLabel: "驳回原因",
            rejectReasonPlaceholder: "请说明为什么当前计划不能进入执行阶段。",
            cancel: "取消",
            confirmReject: "确认驳回",
            approvePlan: "通过计划",
            rejectPlan: "驳回计划",
            loadingPlanTitleFallback: "投资计划",
            unknown: "未知",
            unset: "未设置",
            spotLong: "现货 / 多头",
            checkName: (index: number) => `检查 ${index}`,
            missingCheckDetail: "未提供更多说明。",
            missingRiskNote: "当前风控结论中没有额外说明。",
            summaryLabel: {
              approved: "已通过",
              rejected: "已驳回",
              pending: "待审批",
              pending_user: "待审批",
            },
            riskVerdict: {
              approved: "已通过",
              rejected: "已驳回",
              pending: "待处理",
              pass: "通过",
              warn: "警告",
              fail: "失败",
            },
            planStatus: {
              pending: "待审批",
              pending_user: "待审批",
              approved: "已通过",
              rejected: "已驳回",
              completed: "已完成",
              mixed: "部分成交",
              watch_only: "今日观望",
            },
            actionType: {
              buy: "买入",
              sell: "卖出",
              hold: "持有",
              reduce: "减仓",
              add: "加仓",
              watch: "观察",
            },
            checkResult: {
              pass: "通过",
              warn: "警告",
              fail: "失败",
            },
            executionStatuses: {
              pending: "待执行",
              submitted: "已提交",
              filled: "已成交",
              partially_filled: "部分成交",
              cancelled: "已取消",
              canceled: "已取消",
              rejected: "已拒绝",
            },
            positionSides: {
              long: "多头",
              short: "空头",
            },
            openClose: {
              open: "开仓",
              close: "平仓",
              close_today: "平今",
              roll: "移仓",
            },
          },
    [language],
  );

  const planStatusMeta = useCallback(
    (status: string, riskReview?: unknown) => {
      // Synthesize "watch_only" when status=completed AND the
      // auto-execute gate stamped reasonCode=no_actionable_trade.
      // Without this, a deliberate PM "observe only today" verdict
      // appears in the sidebar as just another "已完成" badge,
      // indistinguishable from a plan that actually filled trades.
      const effective = planEffectiveStatusKey(status, riskReview);
      const badgeMap: Record<string, string> = {
        pending: "bg-amber-50 text-amber-700 border-amber-200",
        pending_user: "bg-amber-50 text-amber-700 border-amber-200",
        approved: "bg-emerald-50 text-emerald-700 border-emerald-200",
        rejected: "bg-red-50 text-red-700 border-red-200",
        completed: "bg-emerald-50 text-emerald-700 border-emerald-200",
        // Sprint 3 / L2 — partial fill 用 amber + emerald 渐近的混合色，
        // 一眼能看出来"既有成功也有失败"。
        mixed: "bg-orange-50 text-orange-700 border-orange-200",
        // Watch-only deserves its own muted-blue palette so the
        // operator can scan a long history list and immediately
        // separate "PM chose to wait" rows from "trades executed"
        // rows without reading the label.
        watch_only: "bg-sky-50 text-sky-700 border-sky-200",
      };
      return {
        label: copy.planStatus[effective as keyof typeof copy.planStatus] ?? humanizeValue(status, copy.unknown),
        badge: badgeMap[effective] ?? "bg-gray-50 text-gray-600 border-gray-200",
      };
    },
    [copy],
  );

  const riskVerdictMeta = useCallback(
    (value: string) => {
      const normalized = value.toLowerCase();
      const badgeMap: Record<string, string> = {
        approved: "bg-emerald-50 text-emerald-700 border-emerald-200",
        pass: "bg-emerald-50 text-emerald-700 border-emerald-200",
        pending: "bg-amber-50 text-amber-700 border-amber-200",
        warn: "bg-amber-50 text-amber-700 border-amber-200",
        rejected: "bg-red-50 text-red-700 border-red-200",
        fail: "bg-red-50 text-red-700 border-red-200",
      };
      return {
        label: copy.riskVerdict[normalized as keyof typeof copy.riskVerdict] ?? humanizeValue(value, copy.unknown),
        badge: badgeMap[normalized] ?? "bg-gray-50 text-gray-600 border-gray-200",
      };
    },
    [copy],
  );

  const checkMeta = useCallback(
    (result: string) => {
      const normalized = result.toLowerCase();
      const badgeMap: Record<string, string> = {
        pass: "bg-emerald-50 text-emerald-700 border-emerald-200",
        warn: "bg-amber-50 text-amber-700 border-amber-200",
        fail: "bg-red-50 text-red-700 border-red-200",
      };
      return {
        label: copy.checkResult[normalized as keyof typeof copy.checkResult] ?? humanizeValue(result, copy.unknown),
        badge: badgeMap[normalized] ?? "bg-gray-50 text-gray-600 border-gray-200",
      };
    },
    [copy],
  );

  const actionMeta = useCallback(
    (action: string) => {
      const normalized = action.toLowerCase();
      const colorMap: Record<string, string> = {
        buy: "text-emerald-600",
        sell: "text-red-600",
        hold: "text-gray-600",
        reduce: "text-amber-600",
        add: "text-blue-600",
        watch: "text-violet-600",
      };
      return {
        label: copy.actionType[normalized as keyof typeof copy.actionType] ?? humanizeValue(action, copy.unknown),
        color: colorMap[normalized] ?? "text-gray-600",
      };
    },
    [copy],
  );

  const positionSideLabel = useCallback(
    (value?: string) => copy.positionSides[(value ?? "").toLowerCase() as keyof typeof copy.positionSides] ?? humanizeValue(value, copy.spotLong),
    [copy],
  );

  const openCloseLabel = useCallback(
    (value?: string) => copy.openClose[(value ?? "").toLowerCase() as keyof typeof copy.openClose] ?? humanizeValue(value, copy.unset),
    [copy],
  );

  const actionExecutionStatusLabel = useCallback(
    (value?: string) => copy.executionStatuses[(value ?? "").toLowerCase() as keyof typeof copy.executionStatuses] ?? humanizeValue(value, copy.notRecorded),
    [copy],
  );

  const workflowStepLabel = useCallback(
    (value?: string) => copy.workflowSteps[normalizeTraceStepKey(value) as keyof typeof copy.workflowSteps] ?? humanizeValue(value, copy.notRecorded),
    [copy],
  );

  const traceStepStatusMeta = useCallback(
    (value?: string) => {
      const normalized = (value ?? "").toLowerCase();
      const badgeMap: Record<string, string> = {
        completed: "bg-emerald-50 text-emerald-700 border-emerald-200",
        success: "bg-emerald-50 text-emerald-700 border-emerald-200",
        running: "bg-indigo-50 text-indigo-700 border-indigo-200",
        in_progress: "bg-indigo-50 text-indigo-700 border-indigo-200",
        pending: "bg-gray-50 text-gray-600 border-gray-200",
        queued: "bg-gray-50 text-gray-600 border-gray-200",
        failed: "bg-red-50 text-red-700 border-red-200",
        error: "bg-red-50 text-red-700 border-red-200",
        skipped: "bg-amber-50 text-amber-700 border-amber-200",
        cancelled: "bg-gray-100 text-gray-600 border-gray-200",
        canceled: "bg-gray-100 text-gray-600 border-gray-200",
      };
      return {
        label: copy.traceStepStatus[normalized as keyof typeof copy.traceStepStatus] ?? humanizeValue(value, copy.notRecorded),
        badge: badgeMap[normalized] ?? "bg-gray-50 text-gray-600 border-gray-200",
      };
    },
    [copy],
  );

  const formatPercent = useCallback(
    (value?: number, digits = 2): string => {
      if (typeof value !== "number" || Number.isNaN(value)) {
        return "—";
      }
      const normalized = Math.abs(value) <= 1 ? value * 100 : value;
      const sign = normalized > 0 ? "+" : "";
      return `${sign}${formatNumberForLanguage(normalized, language, {
        minimumFractionDigits: digits,
        maximumFractionDigits: digits,
      })}%`;
    },
    [language],
  );

  const formatQuantity = useCallback(
    (value?: number) =>
      typeof value === "number" && !Number.isNaN(value)
        ? formatNumberForLanguage(value, language, { maximumFractionDigits: 4 })
        : "—",
    [language],
  );

  const planTitle = useCallback(
    (plan: ApiPlan): string => {
      if (plan.tradingDate) {
        return `${copy.loadingPlanTitleFallback} · ${formatDateForLanguage(plan.tradingDate, language)}`;
      }
      return `${copy.loadingPlanTitleFallback} ${plan.id.slice(0, 8)}`;
    },
    [copy.loadingPlanTitleFallback, language],
  );

  const loadPlans = useCallback(async () => {
    if (!fundId) {
      setError(copy.missingFundId);
      setLoading(false);
      return;
    }

    setPlans((current) => {
      if (current.length === 0) {
        setLoading(true);
      } else {
        setRefreshing(true);
      }
      return current;
    });
    setError(null);
    try {
      const response = await apiGet<ApiPlan[]>(`/api/funds/${fundId}/plans?limit=50&offset=0`);
      const nextPlans = (response ?? []).slice().sort((a, b) => {
        const left = a.tradingDate ?? a.createdAt;
        const right = b.tradingDate ?? b.createdAt;
        return right.localeCompare(left);
      });
      setPlans(nextPlans);
      setSelectedId((current) => {
        // S12-alt: respect ?planId=... when it matches a real plan
        // in the freshly-loaded list. Falls back to the previous
        // selection or the pending-first heuristic.
        if (queryPlanId && nextPlans.some((plan) => plan.id === queryPlanId)) {
          return queryPlanId;
        }
        if (current && nextPlans.some((plan) => plan.id === current)) {
          return current;
        }
        return nextPlans.find((plan) => isPendingPlan(plan.status))?.id ?? nextPlans[0]?.id ?? null;
      });
    } catch (err) {
      setError(formatApiError(err, copy.loadError));
    } finally {
      setLoading(false);
      setRefreshing(false);
    }
  }, [copy.loadError, copy.missingFundId, fundId, queryPlanId]);

  useEffect(() => {
    void loadPlans();
  }, [loadPlans]);

  // S12-alt — once we've consumed ?planId on the first load and
  // applied it as the initial selection, strip the param from the
  // URL so the user can navigate to a sibling plan without the
  // deep-link bouncing them back.
  useEffect(() => {
    if (!queryPlanId || loading) {
      return;
    }
    if (plans.some((p) => p.id === queryPlanId)) {
      const next = new URLSearchParams(searchParams);
      next.delete("planId");
      setSearchParams(next, { replace: true });
    }
  }, [queryPlanId, loading, plans, searchParams, setSearchParams]);

  const pendingPlans = useMemo(() => plans.filter((plan) => isPendingPlan(plan.status)), [plans]);
  const historyPlans = useMemo(() => plans.filter((plan) => !isPendingPlan(plan.status)), [plans]);
  const selected = useMemo(() => plans.find((plan) => plan.id === selectedId) ?? null, [plans, selectedId]);
  const selectedTradingDate = normalizeTradingDateParam(selected?.tradingDate);
  const selectedPlanKey = selected?.id?.trim() || "";
  const selectedDecisionTrace = selectedPlanKey ? decisionTraceByPlanKey[selectedPlanKey] : null;
  const selectedPlanDetail = selectedDecisionTrace?.plan ?? selected;
  const riskReview = useMemo(
    () =>
      parseRiskReview(
        selectedDecisionTrace?.plan?.riskReview ?? selected?.riskReview,
        copy.checkName,
        copy.missingCheckDetail,
        copy.missingRiskNote,
      ),
    [copy.checkName, copy.missingCheckDetail, copy.missingRiskNote, selected, selectedDecisionTrace],
  );

  useEffect(() => {
    if (!fundId || !selectedPlanKey) {
      return;
    }
    // Skip when we already have a successful trace cached AND there's
    // no retry pending. Errors trigger a re-fetch via the retry counter.
    if (
      decisionTraceByPlanKey[selectedPlanKey] !== undefined &&
      decisionTraceErrorByPlanKey[selectedPlanKey] == null &&
      decisionTraceRetryCounter === 0
    ) {
      return;
    }
    let cancelled = false;
    setDecisionTraceLoading(true);
    // Reset any prior error for this key BEFORE the fetch so the UI
    // doesn't double-flash the error during retry.
    setDecisionTraceErrorByPlanKey((current) => ({ ...current, [selectedPlanKey]: null }));
    void fetchFundDecisionTrace(fundId, selectedTradingDate, selectedPlanKey)
      .then((trace) => {
        if (cancelled) {
          return;
        }
        setDecisionTraceByPlanKey((current) => ({ ...current, [selectedPlanKey]: trace }));
      })
      .catch((err) => {
        if (cancelled) {
          return;
        }
        setDecisionTraceByPlanKey((current) => ({ ...current, [selectedPlanKey]: null }));
        const message =
          err instanceof Error && err.message ? err.message : "Failed to load decision trace";
        setDecisionTraceErrorByPlanKey((current) => ({ ...current, [selectedPlanKey]: message }));
      })
      .finally(() => {
        if (!cancelled) {
          setDecisionTraceLoading(false);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [
    decisionTraceByPlanKey,
    decisionTraceErrorByPlanKey,
    decisionTraceRetryCounter,
    fundId,
    selectedPlanKey,
    selectedTradingDate,
  ]);

  const decisionTraceError = selectedPlanKey
    ? decisionTraceErrorByPlanKey[selectedPlanKey] ?? null
    : null;
  const retryDecisionTrace = useCallback(() => {
    if (!selectedPlanKey) return;
    // Drop the cached null so the effect dependency check passes and
    // the fetch re-runs.
    setDecisionTraceByPlanKey((current) => {
      const next = { ...current };
      delete next[selectedPlanKey];
      return next;
    });
    setDecisionTraceErrorByPlanKey((current) => {
      const next = { ...current };
      delete next[selectedPlanKey];
      return next;
    });
    setDecisionTraceRetryCounter((c) => c + 1);
  }, [selectedPlanKey]);

  const updatePlan = useCallback((updated: ApiPlan) => {
    setPlans((prev) => prev.map((plan) => (plan.id === updated.id ? updated : plan)));
    setSelectedId(updated.id);
  }, []);

  // approvePlan / rejectPlan / refreshQuotePlan are intentionally thin: they
  // do the API call and update parent state, leaving the in-flight UI and
  // error display to ApprovalActions. They each clear `actionError` on entry
  // so a fresh attempt isn't blocked by a stale red banner from a previous
  // failure, and they THROW on failure so ApprovalActions can show its own
  // error toast.
  // flashActionSuccess sets a transient confirmation banner that
  // auto-dismisses after 3s. Hides any current error so the banner is
  // unambiguous.
  const flashActionSuccess = useCallback((message: string) => {
    setActionError(null);
    setActionSuccess(message);
    window.setTimeout(() => {
      setActionSuccess((current) => (current === message ? null : current));
    }, 3000);
  }, []);

  const approvePlan = useCallback(async () => {
    if (!selected) {
      return;
    }
    setActionError(null);
    const updated = await apiPost<ApiPlan>(`/api/plans/${selected.id}/approve`);
    updatePlan(updated);
    flashActionSuccess(copy.successApproved);
  }, [copy.successApproved, flashActionSuccess, selected, updatePlan]);

  const rejectPlan = useCallback(
    async (reason: string) => {
      if (!selected) {
        return;
      }
      setActionError(null);
      const updated = await apiPost<ApiPlan>(`/api/plans/${selected.id}/reject`, { reason });
      updatePlan(updated);
      flashActionSuccess(copy.successRejected);
    },
    [copy.successRejected, flashActionSuccess, selected, updatePlan],
  );

  // Threshold for surfacing the price-refresh confirmation dialog.
  // Drifts at or below this fraction are silently applied (no modal,
  // no toast) — anything above triggers the dialog. 0.3% matches what
  // we tell users in the empty-state caption: "Prices within 0.3% of
  // the prior quote." Keep these in sync if you change one.
  const PRICE_DRIFT_DIALOG_THRESHOLD = 0.003;

  // actionKey produces a stable identifier for diffing actions across
  // an API refresh. We prefer the server-assigned id when present
  // (uuid, won't collide); when it's missing — only possible on plans
  // that haven't been persisted yet — we fall back to a synthetic
  // symbol+action+sortOrder key. The fallback is deterministic so the
  // diff still works on those rare unsaved plans.
  const actionKey = (action: { id?: string; symbol: string; action: string; sortOrder?: number }): string =>
    action.id?.trim() ? action.id : `${action.symbol}::${action.action}::${action.sortOrder ?? 0}`;

  const refreshQuotePlan = useCallback(async () => {
    if (!selected) {
      return;
    }
    setActionError(null);
    // Snapshot prices BEFORE the API call so we can diff them against
    // the response. We capture from `selectedPlanDetail` (the detailed
    // view) because the sidebar `selected` summary may omit the per-
    // action price array on plans that haven't been expanded yet.
    const oldPriceByKey = new Map<string, number | undefined>();
    for (const action of selectedPlanDetail?.actions ?? []) {
      oldPriceByKey.set(actionKey(action), action.price);
    }
    const updated = await apiPost<ApiPlan>(`/api/plans/${selected.id}/refresh-quote`);
    updatePlan(updated);
    // Compute drifts in the same key-space we snapshotted above; this
    // makes the diff robust to action reordering on the server side
    // (which shouldn't happen, but is cheap to guard against).
    const rows: PriceRefreshRow[] = [];
    for (const action of updated.actions ?? []) {
      const key = actionKey(action);
      const oldPrice = oldPriceByKey.get(key);
      const newPrice = action.price;
      if (typeof oldPrice !== "number" || oldPrice <= 0 || typeof newPrice !== "number" || newPrice <= 0) {
        continue;
      }
      const drift = (newPrice - oldPrice) / oldPrice;
      if (Math.abs(drift) > PRICE_DRIFT_DIALOG_THRESHOLD) {
        rows.push({
          key,
          symbol: action.symbol,
          oldPrice,
          newPrice,
          drift,
        });
      }
    }
    if (rows.length > 0) {
      setPriceRefreshRows(rows);
      setPriceRefreshDialogOpen(true);
    }
    // Always show a confirmation toast when refresh succeeds, even
    // when no row drifted enough to trigger the dialog — otherwise the
    // user can't tell whether the refresh ran.
    flashActionSuccess(copy.successRefreshed);
  }, [copy.successRefreshed, flashActionSuccess, selected, selectedPlanDetail, updatePlan]);

  const closePriceRefreshDialog = useCallback(() => {
    setPriceRefreshDialogOpen(false);
  }, []);

  const riskScoreTone = useCallback((value?: number): string => {
    if (typeof value !== "number" || Number.isNaN(value)) {
      return "text-gray-600";
    }
    const normalized = Math.abs(value) <= 1 ? value * 100 : value;
    if (normalized >= 70) return "text-red-600";
    if (normalized >= 40) return "text-amber-600";
    return "text-emerald-600";
  }, []);

  const handleSelectPlan = useCallback((planId: string) => {
    setSelectedId(planId);
    // ApprovalActions resets its own reject-box state because the parent
    // remounts it via key={selected.id} on plan change. Here we only clear
    // the stale page-level error banner.
    setActionError(null);
  }, []);

  const handleToggleHistory = useCallback(() => {
    setShowHistory((value) => !value);
  }, []);

  const sidebarLabels = useMemo(
    () => ({
      pendingPlans: copy.pendingPlans,
      noPendingPlans: copy.noPendingPlans,
      historyPlans: copy.historyPlans,
      collapse: copy.collapse,
      expand: copy.expand,
      noHistoryPlans: copy.noHistoryPlans,
      actionCount: copy.actionCount,
      expectedReturn: copy.expectedReturn,
      riskScore: copy.riskScore,
    }),
    [copy.actionCount, copy.collapse, copy.expand, copy.expectedReturn, copy.historyPlans, copy.noHistoryPlans, copy.noPendingPlans, copy.pendingPlans, copy.riskScore],
  );

  const actionListLabels = useMemo(
    () => ({
      actionList: copy.actionList,
      totalRows: copy.totalRows,
      noActions: copy.noActions,
      columns: copy.columns,
      livePriceDriftWarning: copy.livePriceDriftWarning,
      livePriceUnavailable: copy.livePriceUnavailable,
      none: copy.none,
      actionReasonMissing: copy.actionReasonMissing,
      executionStatus: copy.executionStatus,
      contractMultiplier: copy.contractMultiplier,
      expiryDate: copy.expiryDate,
      opposedBy: copy.opposedBy,
      reduceOnly: copy.reduceOnly,
      marketResearch: copy.marketResearch,
      researchSummary: copy.researchSummary,
      researchSignals: copy.researchSignals,
      researchNews: copy.researchNews,
      researchQuote: copy.researchQuote,
      researchUnavailable: copy.researchUnavailable,
      quoteStaleBadge: copy.quoteStaleBadge,
      quoteStaleHint: copy.quoteStaleHint,
      newsLanguageZh: copy.newsLanguageZh,
      newsLanguageEn: copy.newsLanguageEn,
      researchNotes: copy.researchNotes,
    }),
    [
      copy.actionList,
      copy.actionReasonMissing,
      copy.columns,
      copy.contractMultiplier,
      copy.executionStatus,
      copy.expiryDate,
      copy.livePriceDriftWarning,
      copy.livePriceUnavailable,
      copy.marketResearch,
      copy.newsLanguageEn,
      copy.newsLanguageZh,
      copy.noActions,
      copy.none,
      copy.opposedBy,
      copy.quoteStaleBadge,
      copy.quoteStaleHint,
      copy.reduceOnly,
      copy.researchNews,
      copy.researchNotes,
      copy.researchQuote,
      copy.researchSignals,
      copy.researchSummary,
      copy.researchUnavailable,
      copy.totalRows,
    ],
  );

  const traceboardLabels = useMemo(
    () => ({
      traceboard: copy.traceboard,
      traceboardSubtitle: copy.traceboardSubtitle,
      loadingTraceboard: copy.loadingTraceboard,
      loadingTraceboardHint: copy.loadingTraceboardHint,
      workflowTimeline: copy.workflowTimeline,
      workflowState: copy.workflowState,
      workflowCurrentStep: copy.workflowCurrentStep,
      workflowStartedAt: copy.workflowStartedAt,
      workflowCompletedAt: copy.workflowCompletedAt,
      workflowUpdatedAt: copy.workflowUpdatedAt,
      workflowError: copy.workflowError,
      workflowNoData: copy.workflowNoData,
      discussionTrace: copy.discussionTrace,
      discussionSummary: copy.discussionSummary,
      discussionConsensus: copy.discussionConsensus,
      discussionReasoning: copy.discussionReasoning,
      discussionSnapshot: copy.discussionSnapshot,
      discussionFallback: copy.discussionFallback,
      executionTrace: copy.executionTrace,
      executionStatusLabel: copy.executionStatusLabel,
      tradeId: copy.tradeId,
      tradeSide: copy.tradeSide,
      tradeStatus: copy.tradeStatus,
      filledQuantity: copy.filledQuantity,
      filledPrice: copy.filledPrice,
      orderType: copy.orderType,
      executedAt: copy.executedAt,
      traceNoTrades: copy.traceNoTrades,
      traceNoExecution: copy.traceNoExecution,
      reviewMemory: copy.reviewMemory,
      reviewLayer: copy.reviewLayer,
      reviewUpdatedAt: copy.reviewUpdatedAt,
      reviewNoEntries: copy.reviewNoEntries,
      notRecorded: copy.notRecorded,
    }),
    [
      copy.discussionConsensus,
      copy.discussionFallback,
      copy.discussionReasoning,
      copy.discussionSnapshot,
      copy.discussionSummary,
      copy.discussionTrace,
      copy.executedAt,
      copy.executionStatusLabel,
      copy.executionTrace,
      copy.filledPrice,
      copy.filledQuantity,
      copy.loadingTraceboard,
      copy.loadingTraceboardHint,
      copy.notRecorded,
      copy.orderType,
      copy.reviewLayer,
      copy.reviewMemory,
      copy.reviewNoEntries,
      copy.reviewUpdatedAt,
      copy.traceNoExecution,
      copy.traceNoTrades,
      copy.traceboard,
      copy.traceboardSubtitle,
      copy.tradeId,
      copy.tradeSide,
      copy.tradeStatus,
      copy.workflowCompletedAt,
      copy.workflowCurrentStep,
      copy.workflowError,
      copy.workflowNoData,
      copy.workflowStartedAt,
      copy.workflowState,
      copy.workflowTimeline,
      copy.workflowUpdatedAt,
    ],
  );

  const traceboardInitialLoading =
    decisionTraceLoading && Boolean(selectedPlanKey) && decisionTraceByPlanKey[selectedPlanKey] === undefined;

  const committeeMemoLabels = useMemo(
    () => ({
      committeeMemo: copy.committeeMemo,
      committeeMemoSubtitle: copy.committeeMemoSubtitle,
      meetingSummary: copy.meetingSummary,
      marketBackground: copy.marketBackground,
      participants: copy.participants,
      agentViews: copy.agentViews,
      keyContentions: copy.keyContentions,
      finalDecision: copy.finalDecision,
      riskOpinion: copy.riskOpinion,
      traderSuggestions: copy.traderSuggestions,
      noCommitteeMemo: copy.noCommitteeMemo,
      noContentions: copy.noContentions,
      discussionFallback: copy.discussionFallback,
      researchUnavailable: copy.researchUnavailable,
      traceNoExecution: copy.traceNoExecution,
      stance: copy.stance,
      portfolioManager: copy.portfolioManager,
      opposedBy: copy.opposedBy,
      unknown: copy.unknown,
    }),
    [
      copy.agentViews,
      copy.committeeMemo,
      copy.committeeMemoSubtitle,
      copy.discussionFallback,
      copy.finalDecision,
      copy.keyContentions,
      copy.marketBackground,
      copy.meetingSummary,
      copy.noCommitteeMemo,
      copy.noContentions,
      copy.opposedBy,
      copy.participants,
      copy.portfolioManager,
      copy.researchUnavailable,
      copy.riskOpinion,
      copy.stance,
      copy.traceNoExecution,
      copy.traderSuggestions,
      copy.unknown,
    ],
  );

  const approvalLabels = useMemo(
    () => ({
      approvalActions: copy.approvalActions,
      approvePlan: copy.approvePlan,
      rejectPlan: copy.rejectPlan,
      cancel: copy.cancel,
      confirmReject: copy.confirmReject,
      rejectReasonLabel: copy.rejectReasonLabel,
      rejectReasonPlaceholder: copy.rejectReasonPlaceholder,
      refreshQuotePlan: copy.refreshQuotePlan,
      approveError: copy.approveError,
      rejectError: copy.rejectError,
      refreshQuoteError: copy.refreshQuoteError,
    }),
    [
      copy.approvalActions,
      copy.approveError,
      copy.approvePlan,
      copy.cancel,
      copy.confirmReject,
      copy.refreshQuoteError,
      copy.refreshQuotePlan,
      copy.rejectError,
      copy.rejectPlan,
      copy.rejectReasonLabel,
      copy.rejectReasonPlaceholder,
    ],
  );

  const riskConclusionLabels = useMemo(
    () => ({
      riskConclusion: copy.riskConclusion,
      riskExplanation: copy.riskExplanation,
      blockingReasons: copy.blockingReasons,
      riskWarnings: copy.riskWarnings,
      adjustmentAdvice: copy.adjustmentAdvice,
      userImpact: copy.userImpact,
      currentValue: copy.currentValue,
      thresholdValue: copy.thresholdValue,
      noRiskChecks: copy.noRiskChecks,
      noRiskConclusion: copy.noRiskConclusion,
      missingCheckDetail: copy.missingCheckDetail,
      checkName: copy.checkName,
      unknown: copy.unknown,
    }),
    [
      copy.adjustmentAdvice,
      copy.blockingReasons,
      copy.checkName,
      copy.currentValue,
      copy.missingCheckDetail,
      copy.noRiskChecks,
      copy.noRiskConclusion,
      copy.riskConclusion,
      copy.riskExplanation,
      copy.riskWarnings,
      copy.thresholdValue,
      copy.unknown,
      copy.userImpact,
    ],
  );

  const workflowSteps = useMemo(
    () => expandWorkflowSteps(selectedDecisionTrace?.run?.steps ?? [], selectedDecisionTrace?.run?.step),
    [selectedDecisionTrace],
  );
  const canActOnSelectedPlan = useMemo(() => (selected ? canReviewPlan(selected.status) : false), [selected]);
  const selectedReasoning = useMemo(
    () => pickLocalizedText(language, selectedPlanDetail?.reasoning, selectedPlanDetail?.reasoningZh, selectedPlanDetail?.reasoningEn),
    [language, selectedPlanDetail],
  );
  const selectedStrategyReasoning = useMemo(
    () =>
      selectedPlanDetail?.status === "rejected"
        ? selectedReasoning
        : compactStrategyReasoning(selectedReasoning, selectedPlanDetail?.actions, language),
    [language, selectedPlanDetail, selectedReasoning],
  );
  const discussionConsensus = useMemo(
    () =>
      pickLocalizedList(
        language,
        selectedDecisionTrace?.discussion?.consensus,
        selectedDecisionTrace?.discussion?.consensusZh,
        selectedDecisionTrace?.discussion?.consensusEn,
      ),
    [language, selectedDecisionTrace],
  );
  const discussionReasoning = useMemo(
    () =>
      pickLocalizedText(
        language,
        selectedDecisionTrace?.discussion?.reasoning,
        selectedDecisionTrace?.discussion?.reasoningZh,
        selectedDecisionTrace?.discussion?.reasoningEn,
      ),
    [language, selectedDecisionTrace],
  );
  const discussionSummary = useMemo(() => {
    const discussion = selectedDecisionTrace?.discussion;
    if (!discussion) {
      return "";
    }
    const summary = pickLocalizedText(language, discussion.summary, discussion.summaryZh, discussion.summaryEn);
    if (summary) {
      return summary;
    }
    if (discussionConsensus.length) {
      return discussionConsensus.join("\n");
    }
    if (discussionReasoning) {
      return discussionReasoning;
    }
    return "";
  }, [discussionConsensus, discussionReasoning, language, selectedDecisionTrace]);
  const discussionSnapshot = useMemo(() => {
    const discussion = selectedDecisionTrace?.discussion;
    if (!discussion?.hasSnapshot || !discussion.snapshot || isEmptyObject(discussion.snapshot)) {
      return "";
    }
    return JSON.stringify(discussion.snapshot, null, 2);
  }, [selectedDecisionTrace]);

  const executionTraceRows = useMemo<ExecutionTraceView[]>(() => {
    if (!selectedPlanDetail) {
      return [];
    }
    const executionItems = selectedDecisionTrace?.execution?.actionExecutions ?? [];
    const executionByActionId = new Map<string, DecisionTraceActionExecution>();
    const executionByFallbackKey = new Map<string, DecisionTraceActionExecution>();
    executionItems.forEach((item) => {
      if (item.planActionId?.trim()) {
        executionByActionId.set(item.planActionId.trim(), item);
      }
      const fallbackKey = `${(item.symbol ?? "").trim().toUpperCase()}::${normalizeTradeActionKey(item.action)}`;
      if ((item.symbol ?? "").trim()) {
        executionByFallbackKey.set(fallbackKey, item);
      }
    });

    return (selectedPlanDetail.actions ?? []).map((action, index) => {
      const actionId = action.id?.trim() || `${action.symbol}-${index}`;
      const fallbackKey = `${action.symbol.trim().toUpperCase()}::${normalizeTradeActionKey(action.action)}`;
      const matched = (action.id ? executionByActionId.get(action.id.trim()) : undefined) ?? executionByFallbackKey.get(fallbackKey);
      return {
        actionId,
        symbol: action.symbol,
        action: action.action,
        executionStatus: matched?.executionStatus || action.executionStatus || "pending",
        trades: matched?.trades ?? [],
      };
    });
  }, [selectedDecisionTrace, selectedPlanDetail]);

  const researchBySymbol = useMemo<Record<string, MarketResearch>>(() => {
    const items = selectedDecisionTrace?.research ?? [];
    return items.reduce<Record<string, MarketResearch>>((accumulator, item) => {
      const symbol = item.instrument?.symbol?.trim();
      if (symbol) {
        accumulator[symbol] = item;
      }
      return accumulator;
    }, {});
  }, [selectedDecisionTrace]);

  const reviewEntries = useMemo<DecisionTraceReviewEntry[]>(() => selectedDecisionTrace?.review?.entries ?? [], [selectedDecisionTrace]);
  const committeeMemo = selectedDecisionTrace?.memo;
  const riskExplanation = selectedDecisionTrace?.risk;

  if (loading) {
    return (
      <div className="space-y-6">
        <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
          <div className="h-7 w-48 animate-pulse rounded bg-gray-200" />
          <div className="mt-3 h-4 w-96 max-w-full animate-pulse rounded bg-gray-100" />
        </div>
        <div className="grid grid-cols-1 gap-6 xl:grid-cols-[360px_minmax(0,1fr)]">
          <div className="space-y-4">
            {[0, 1, 2].map((index) => (
              <div key={index} className="rounded-xl border border-gray-200 bg-white p-4 shadow-sm">
                <div className="h-5 w-40 animate-pulse rounded bg-gray-200" />
                <div className="mt-3 h-3 w-32 animate-pulse rounded bg-gray-100" />
                <div className="mt-4 grid grid-cols-3 gap-2">
                  {[0, 1, 2].map((metric) => (
                    <div key={metric} className="rounded-lg bg-gray-50 px-2 py-3">
                      <div className="mx-auto h-4 w-10 animate-pulse rounded bg-gray-200" />
                      <div className="mx-auto mt-2 h-3 w-12 animate-pulse rounded bg-gray-100" />
                    </div>
                  ))}
                </div>
              </div>
            ))}
          </div>
          <div className="space-y-6">
            {[0, 1, 2].map((index) => (
              <div key={index} className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
                <div className="h-5 w-40 animate-pulse rounded bg-gray-200" />
                <div className="mt-4 h-4 w-full animate-pulse rounded bg-gray-100" />
                <div className="mt-2 h-4 w-5/6 animate-pulse rounded bg-gray-100" />
                <div className="mt-2 h-4 w-2/3 animate-pulse rounded bg-gray-100" />
              </div>
            ))}
          </div>
        </div>
        <p className="text-sm text-gray-500">{copy.loading}</p>
      </div>
    );
  }

  if (error) {
    return (
      <div className="rounded-xl border border-red-200 bg-red-50 p-6 text-sm text-red-700">
        <p>{error}</p>
        <button onClick={() => void loadPlans()} className="mt-4 rounded-lg bg-red-600 px-4 py-2 text-sm font-medium text-white hover:bg-red-700">
          {copy.retry}
        </button>
      </div>
    );
  }

  if (plans.length === 0) {
    return (
      <div className="space-y-6">
        <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
          <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
          <p className="mt-2 text-sm text-gray-500">{copy.subtitle}</p>
        </div>
        <div className="rounded-xl border border-dashed border-gray-200 bg-white p-8 text-center text-sm text-gray-500">
          <p className="font-medium text-gray-700">{copy.emptyTitle}</p>
          <p className="mt-2">{copy.emptyDescription}</p>
          <Link to=".." className="mt-4 inline-flex rounded-lg bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-700">
            {copy.backToDashboard}
          </Link>
        </div>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <NextRunBanner fundId={fundId} language={language} />
      {fundId ? (
        <LiveReadinessBanner fundId={fundId} language={language} />
      ) : null}
      <div className="flex flex-col gap-4 rounded-2xl border border-gray-200 bg-white p-6 shadow-sm lg:flex-row lg:items-start lg:justify-between">
        <div>
          <div className="flex items-center gap-3">
            <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
            {refreshing ? (
              <span
                className="inline-flex items-center gap-2 rounded-full border border-indigo-200 bg-indigo-50 px-3 py-1 text-xs font-medium text-indigo-700"
                role="status"
                aria-live="polite"
              >
                <span className="h-2 w-2 animate-pulse rounded-full bg-indigo-500" />
                {copy.refreshing}
              </span>
            ) : null}
          </div>
          <p className="mt-2 text-sm text-gray-500">{copy.subtitle}</p>
        </div>
        <div className="grid grid-cols-3 gap-3 text-center text-sm">
          <div className="rounded-xl border border-gray-200 bg-gray-50 px-4 py-3">
            <p className="text-xs text-gray-500">{copy.pendingCount}</p>
            <p className="mt-1 text-lg font-semibold text-gray-900">{formatNumberForLanguage(pendingPlans.length, language)}</p>
          </div>
          <div className="rounded-xl border border-gray-200 bg-gray-50 px-4 py-3">
            <p className="text-xs text-gray-500">{copy.historyCount}</p>
            <p className="mt-1 text-lg font-semibold text-gray-900">{formatNumberForLanguage(historyPlans.length, language)}</p>
          </div>
          <div className="rounded-xl border border-gray-200 bg-gray-50 px-4 py-3">
            <p className="text-xs text-gray-500">{copy.totalCount}</p>
            <p className="mt-1 text-lg font-semibold text-gray-900">{formatNumberForLanguage(plans.length, language)}</p>
          </div>
        </div>
      </div>

      {actionError ? <div className="rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">{actionError}</div> : null}
      {actionSuccess ? (
        <div className="rounded-xl border border-emerald-200 bg-emerald-50 px-4 py-3 text-sm text-emerald-700">{actionSuccess}</div>
      ) : null}

      <div className="grid grid-cols-1 gap-6 xl:grid-cols-[360px_minmax(0,1fr)]">
        <PlanListSidebar
          pendingPlans={pendingPlans}
          historyPlans={historyPlans}
          selectedId={selected?.id ?? null}
          showHistory={showHistory}
          language={language}
          labels={sidebarLabels}
          planStatusMeta={planStatusMeta}
          planTitle={planTitle}
          riskScoreTone={riskScoreTone}
          formatPercent={formatPercent}
          onSelectPlan={handleSelectPlan}
          onToggleHistory={handleToggleHistory}
        />

        <section className="space-y-6">
          {!selected ? (
            <div className="rounded-xl border border-dashed border-gray-200 bg-white p-8 text-center text-sm text-gray-500">{copy.selectPlan}</div>
          ) : (
            <>
              <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
                <div className="flex flex-col gap-4 lg:flex-row lg:items-start lg:justify-between">
                  <div>
                    <div className="flex flex-wrap items-center gap-3">
                      <h2 className="text-2xl font-bold text-gray-900">{planTitle(selected)}</h2>
                      <span className={`rounded-full border px-3 py-1 text-sm font-medium ${planStatusMeta(selectedPlanDetail?.status ?? selected.status, selectedPlanDetail?.riskReview ?? selected.riskReview).badge}`}>
                        {planStatusMeta(selectedPlanDetail?.status ?? selected.status, selectedPlanDetail?.riskReview ?? selected.riskReview).label}
                      </span>
                    </div>
                    <p className="mt-2 text-sm text-gray-500">
                      {copy.planDate} {formatDateTimeForLanguage(selected.tradingDate ?? selected.createdAt, language)} · {copy.updatedAt} {formatDateTimeForLanguage(selected.updatedAt, language)}
                    </p>
                  </div>
                  <div className="grid grid-cols-3 gap-3 text-center text-sm">
                    <div className="rounded-xl bg-gray-50 px-4 py-3">
                      <p className="text-xs text-gray-500">{copy.expectedReturn}</p>
                      <p className="mt-1 font-semibold text-emerald-600">{formatPercent(selectedPlanDetail?.expectedReturn ?? selected.expectedReturn)}</p>
                    </div>
                    <div className="rounded-xl bg-gray-50 px-4 py-3">
                      <p className="text-xs text-gray-500">{copy.riskScore}</p>
                      <p className={`mt-1 font-semibold ${riskScoreTone(selectedPlanDetail?.riskScore ?? selected.riskScore)}`}>{formatPercent(selectedPlanDetail?.riskScore ?? selected.riskScore, 0)}</p>
                    </div>
                    <div className="rounded-xl bg-gray-50 px-4 py-3">
                      <p className="text-xs text-gray-500">{copy.actionCount}</p>
                      <p className="mt-1 font-semibold text-gray-900">{formatNumberForLanguage(selectedPlanDetail?.actions?.length ?? 0, language)}</p>
                    </div>
                  </div>
                </div>

                <div className="mt-4 flex flex-wrap gap-3 text-xs text-gray-500">
                  <span className="rounded-full bg-gray-100 px-3 py-1">{copy.portfolioManager}: {selectedPlanDetail?.pmAgentId || selected.pmAgentId || copy.notRecorded}</span>
                  <span className="rounded-full bg-gray-100 px-3 py-1">{copy.discussionSession}: {selectedPlanDetail?.roundtableId || selected.roundtableId || copy.notRecorded}</span>
                  <DecisionSourceChip
                    language={language}
                    source={selectedPlanDetail?.decisionSource ?? selected?.decisionSource}
                    reason={selectedPlanDetail?.fallbackReason ?? selected?.fallbackReason}
                  />
                </div>
              </div>

              <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
                <div className="flex flex-wrap items-start justify-between gap-3">
                  <h3 className="text-sm font-semibold uppercase tracking-wider text-gray-500">
                    {selectedPlanDetail?.status === "rejected" ? copy.rejectedReason : copy.strategyReasoning}
                  </h3>
                  {/* Drill-down opens a modal that aggregates EVERY
                      reason source (PM thesis, strategy, per-action
                      reasons, debate, memo, risk, blocking, raw JSON)
                      for this plan in one scrollable view. Saves
                      operators from scrolling through five separate
                      cards. See components/DecisionReasonModal.tsx. */}
                  {selectedPlanDetail ? (
                    <button
                      type="button"
                      onClick={() => setReasonModalOpen(true)}
                      className="inline-flex items-center gap-1 rounded-md border border-gray-200 px-2.5 py-1 text-xs font-medium text-gray-600 transition hover:border-indigo-300 hover:bg-indigo-50 hover:text-indigo-700"
                    >
                      <span aria-hidden="true">🔍</span>
                      {language === "en-US" ? "Why this decision?" : "查看决策依据"}
                    </button>
                  ) : null}
                </div>
                <div className="mt-3 rounded-xl bg-gray-50 p-4 text-sm leading-7 text-gray-700">
                  <p className="whitespace-pre-line">{selectedStrategyReasoning || (selectedPlanDetail?.status === "rejected" ? copy.noRejectedReason : copy.noStrategyReasoning)}</p>
                </div>
              </div>

              <CommitteeMemoCard
                memo={committeeMemo}
                discussionSummary={discussionSummary}
                language={language}
                labels={committeeMemoLabels}
                planStatusMeta={planStatusMeta}
                riskVerdictMeta={riskVerdictMeta}
                actionMeta={actionMeta}
              />

              <ActionListCard
                actions={selectedPlanDetail?.actions}
                researchBySymbol={researchBySymbol}
                liveQuotesBySymbol={liveQuotesBySymbol}
                language={language}
                displayCurrency={displayCurrency}
                labels={actionListLabels}
                actionMeta={actionMeta}
                positionSideLabel={positionSideLabel}
                openCloseLabel={openCloseLabel}
                actionExecutionStatusLabel={actionExecutionStatusLabel}
                formatPercent={formatPercent}
                formatQuantity={formatQuantity}
              />

              {decisionTraceError ? (
                <div className="flex items-center justify-between gap-3 rounded-2xl border border-rose-200 bg-rose-50 px-4 py-3 text-sm text-rose-700">
                  <span>
                    {copy.traceLoadFailed}
                    {decisionTraceError ? `: ${decisionTraceError}` : ""}
                  </span>
                  <button
                    type="button"
                    onClick={retryDecisionTrace}
                    className="rounded-lg border border-rose-400 bg-white px-3 py-1.5 text-xs font-medium text-rose-700 transition hover:bg-rose-100"
                  >
                    {copy.retry}
                  </button>
                </div>
              ) : null}

              <TraceboardCard
                decisionTrace={selectedDecisionTrace}
                workflowSteps={workflowSteps}
                discussionSummary={discussionSummary}
                discussionConsensus={discussionConsensus}
                discussionReasoning={discussionReasoning}
                discussionSnapshot={discussionSnapshot}
                executionTraceRows={executionTraceRows}
                reviewEntries={reviewEntries}
                isInitialLoading={traceboardInitialLoading}
                language={language}
                displayCurrency={displayCurrency}
                labels={traceboardLabels}
                workflowStepLabel={workflowStepLabel}
                traceStepStatusMeta={traceStepStatusMeta}
                actionMeta={actionMeta}
                actionExecutionStatusLabel={actionExecutionStatusLabel}
                formatQuantity={formatQuantity}
              />

              <RiskConclusionCard
                riskExplanation={riskExplanation}
                riskReview={riskReview}
                language={language}
                labels={riskConclusionLabels}
                riskVerdictMeta={riskVerdictMeta}
                checkMeta={checkMeta}
              />

              {canActOnSelectedPlan && selected ? (
                <ApprovalActions
                  // Re-mount on plan change so transient state (reject reason
                  // textarea, in-flight flag) automatically resets without
                  // an imperative ref API.
                  key={selected.id}
                  labels={approvalLabels}
                  onApprove={approvePlan}
                  onReject={rejectPlan}
                  onRefreshQuote={refreshQuotePlan}
                  onError={setActionError}
                />
              ) : null}
            </>
          )}
        </section>
      </div>
      <PriceRefreshDialog
        open={priceRefreshDialogOpen}
        rows={priceRefreshRows}
        labels={{
          title: copy.priceRefreshTitle,
          subtitle: copy.priceRefreshSubtitle,
          columnSymbol: copy.priceRefreshColumnSymbol,
          columnOldPrice: copy.priceRefreshColumnOldPrice,
          columnNewPrice: copy.priceRefreshColumnNewPrice,
          columnDrift: copy.priceRefreshColumnDrift,
          acknowledge: copy.priceRefreshAcknowledge,
        }}
        onClose={closePriceRefreshDialog}
      />
      <DecisionReasonModal
        open={reasonModalOpen}
        onClose={() => setReasonModalOpen(false)}
        language={language}
        plan={selectedPlanDetail}
        decisionTrace={selectedDecisionTrace}
      />
    </div>
  );
};

export default DecisionCenter;
