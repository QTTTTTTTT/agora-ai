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
// W11-3 — DecisionCenter i18n migration. Pulls translations from
// the `decisionCenter` namespace; the static fallback import is
// the type source so JSX `copy.workflowSteps.macro_brief` keeps
// narrow literal types from the `as const` bundle. Function-
// valued strings (livePriceDriftWarning, checkName) become
// `t("…", { var })` interpolation calls — the consumer surface
// for those is small (two callbacks below) so we wire them
// individually rather than via a Function shim on `copy`.
import { useTranslation } from "react-i18next";
import i18n from "../i18n";
import decisionCenterEnFallback from "../i18n/locales/en-US/decisionCenter";
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

  // W11-3 — translations now live in the `decisionCenter` i18n
  // namespace. We pull the bundle via `getResourceBundle` to keep
  // the consumer surface stable: existing JSX continues to read
  // `copy.workflowSteps.macro_brief`, `copy.columns.instrument`
  // etc. with narrow literal types from the `as const` fallback.
  //
  // The two function-valued strings (livePriceDriftWarning,
  // checkName) are passed *as functions* to subcomponents
  // (ActionListCard, RiskConclusionCard); rather than refactor
  // those interfaces, we wrap the i18next interpolation calls
  // here so the subcomponents see the same shape as before.
  const { t } = useTranslation("decisionCenter");
  const copy = useMemo(() => {
    const bundle =
      (i18n.getResourceBundle(language, "decisionCenter") as
        | typeof decisionCenterEnFallback
        | undefined) ?? decisionCenterEnFallback;
    return {
      ...bundle,
      livePriceDriftWarning: (percent: string) =>
        t("livePriceDriftWarning", { percent }),
      checkName: (index: number) => t("checkName", { index }),
    };
  }, [language, t]);

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
      listSeparator: copy.listSeparator,
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
      copy.listSeparator,
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
      listSeparator: copy.listSeparator,
      unknown: copy.unknown,
    }),
    [
      copy.agentViews,
      copy.committeeMemo,
      copy.committeeMemoSubtitle,
      copy.discussionFallback,
      copy.finalDecision,
      copy.keyContentions,
      copy.listSeparator,
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
