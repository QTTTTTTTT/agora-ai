import React, { useCallback, useEffect, useMemo, useState } from "react";
import { useParams, useNavigate } from "react-router-dom";
import {
  activatePromotion,
  approvePromotion,
  formatApiError,
  getPromotion,
  listPromotions,
  rejectPromotion,
  rollbackPromotion,
  type Promotion,
  type PromotionDetail,
  type PromotionStatus,
} from "../lib/api";
import {
  formatDateForLanguage,
  formatDateTimeForLanguage,
  formatNumberForLanguage,
  useAppPreferences,
  type AppLanguage,
} from "../lib/preferences";

// statusTone mirrors the Backtest page's colour scheme so the
// header pill is visually consistent across the strategy-lifecycle
// surface.
function statusTone(status?: string): string {
  switch ((status ?? "").toLowerCase()) {
    case "active":
      return "border-emerald-200 bg-emerald-50 text-emerald-700";
    case "shadow":
      return "border-indigo-200 bg-indigo-50 text-indigo-700";
    case "approved":
    case "pending_review":
      return "border-amber-200 bg-amber-50 text-amber-700";
    case "rejected":
    case "rolled_back":
    case "decayed":
      return "border-red-200 bg-red-50 text-red-700";
    case "superseded":
      return "border-gray-200 bg-gray-50 text-gray-600";
    default:
      return "border-gray-200 bg-gray-50 text-gray-600";
  }
}

function buildCopy(language: AppLanguage) {
  if (language === "en-US") {
    return {
      title: "Strategy promotions",
      subtitle:
        "Review backtest-validated strategies, run them through shadow mode, and promote them to drive the live PMAgent.",
      missingFundId: "Missing fundId",
      retry: "Retry",
      loadError: "Failed to load promotions",
      empty: "No promotions yet. Promote a completed backtest from the Backtest Lab to get started.",
      list: "Promotion ledger",
      detail: "Promotion detail",
      back: "← Back to list",
      basisJob: "Basis backtest",
      proposedBy: "Proposed by",
      approvedBy: "Approved by",
      shadowDays: "Shadow days",
      decayRatio: "Decay threshold",
      baseline: "Baseline metrics",
      sharpe: "Sharpe",
      cumulative: "Cumulative return",
      maxDD: "Max drawdown",
      tradeCount: "Trade count",
      oosSharpe: "OOS Sharpe",
      oosReturn: "OOS return",
      events: "Lifecycle timeline",
      shadowSection: "Shadow comparisons (daily)",
      agreementRatio: "Agreement ratio",
      noShadow: "No shadow comparisons recorded yet.",
      healthSection: "Decay monitor snapshots",
      noHealth: "No health snapshots yet.",
      colDate: "Date",
      colWindow: "Window",
      colSharpe: "Actual Sharpe",
      colDecay: "Decay ratio",
      colFlag: "Flag",
      colAgreement: "Agreement",
      colShadowDecision: "Shadow",
      colActiveDecision: "Active",
      approve: "Approve",
      reject: "Reject",
      activate: "Activate now",
      rollback: "Rollback",
      reasonPrompt: "Reason (required)",
      transitionError: "Failed to update promotion",
      actor: "Actor",
      type: "Event",
      decayFlagYes: "decay",
      decayFlagNo: "OK",
      agreementYes: "agree",
      agreementNo: "disagree",
      noBasis: "No baseline metrics available.",
    };
  }
  return {
    title: "策略升级",
    subtitle:
      "审查经过回测验证的策略，让其先跑影子模式，再正式升级为驱动实盘 PMAgent 的引擎。",
    missingFundId: "缺少 fundId",
    retry: "重试",
    loadError: "升级列表加载失败",
    empty: "暂无升级记录。从回测实验室对一个已完成的回测点 \"升级到实盘\" 开始。",
    list: "升级列表",
    detail: "升级详情",
    back: "← 返回列表",
    basisJob: "依据回测",
    proposedBy: "提交人",
    approvedBy: "审核人",
    shadowDays: "影子天数",
    decayRatio: "衰减阈值",
    baseline: "基准指标",
    sharpe: "夏普",
    cumulative: "累计收益",
    maxDD: "最大回撤",
    tradeCount: "成交数",
    oosSharpe: "OOS 夏普",
    oosReturn: "OOS 收益",
    events: "生命周期时间线",
    shadowSection: "影子模式对比（每日）",
    agreementRatio: "一致率",
    noShadow: "暂无影子对比记录。",
    healthSection: "衰减监控快照",
    noHealth: "暂无健康度快照。",
    colDate: "日期",
    colWindow: "窗口",
    colSharpe: "实盘夏普",
    colDecay: "衰减比",
    colFlag: "标记",
    colAgreement: "一致性",
    colShadowDecision: "影子",
    colActiveDecision: "实盘",
    approve: "通过审核",
    reject: "拒绝",
    activate: "立即激活",
    rollback: "回滚",
    reasonPrompt: "原因（必填）",
    transitionError: "升级状态更新失败",
    actor: "操作人",
    type: "事件",
    decayFlagYes: "衰减",
    decayFlagNo: "正常",
    agreementYes: "一致",
    agreementNo: "不一致",
    noBasis: "暂无基准指标。",
  };
}

const Promotions: React.FC = () => {
  const { fundId, promotionId } = useParams<{ fundId: string; promotionId?: string }>();
  const navigate = useNavigate();
  const { language } = useAppPreferences();
  const copy = useMemo(() => buildCopy(language), [language]);

  const [list, setList] = useState<Promotion[]>([]);
  const [detail, setDetail] = useState<PromotionDetail | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [pendingAction, setPendingAction] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    if (!fundId) {
      setError(copy.missingFundId);
      setLoading(false);
      return;
    }
    setLoading(true);
    setError(null);
    try {
      if (promotionId) {
        setDetail(await getPromotion(fundId, promotionId));
      } else {
        setList(await listPromotions(fundId));
      }
    } catch (err) {
      setError(formatApiError(err, copy.loadError));
    } finally {
      setLoading(false);
    }
  }, [copy.loadError, copy.missingFundId, fundId, promotionId]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const onTransition = useCallback(
    async (action: "approve" | "reject" | "activate" | "rollback") => {
      if (!fundId || !promotionId) return;
      setActionError(null);
      setPendingAction(action);
      try {
        if (action === "approve") {
          await approvePromotion(fundId, promotionId);
        } else if (action === "activate") {
          await activatePromotion(fundId, promotionId);
        } else {
          const reason = window.prompt(copy.reasonPrompt) ?? "";
          if (!reason.trim()) {
            return;
          }
          if (action === "reject") {
            await rejectPromotion(fundId, promotionId, reason);
          } else {
            await rollbackPromotion(fundId, promotionId, reason);
          }
        }
        await refresh();
      } catch (err) {
        setActionError(formatApiError(err, copy.transitionError));
      } finally {
        setPendingAction(null);
      }
    },
    [copy.reasonPrompt, copy.transitionError, fundId, promotionId, refresh],
  );

  if (error) {
    return (
      <div className="p-6">
        <p className="text-sm text-red-700">{error}</p>
        <button className="mt-3 rounded border border-gray-200 bg-white px-3 py-1.5 text-xs" onClick={refresh}>
          {copy.retry}
        </button>
      </div>
    );
  }

  return (
    <div className="space-y-6 p-6">
      <header>
        <h1 className="text-2xl font-semibold text-gray-900">{copy.title}</h1>
        <p className="mt-1 text-sm text-gray-600">{copy.subtitle}</p>
      </header>

      {promotionId ? (
        <PromotionDetailView
          detail={detail}
          loading={loading}
          copy={copy}
          language={language}
          actionError={actionError}
          pendingAction={pendingAction}
          onBack={() => navigate(`/funds/${fundId}/promotions`)}
          onTransition={onTransition}
        />
      ) : (
        <PromotionList
          list={list}
          loading={loading}
          copy={copy}
          language={language}
          onPick={(id) => navigate(`/funds/${fundId}/promotions/${id}`)}
        />
      )}
    </div>
  );
};

const PromotionList: React.FC<{
  list: Promotion[];
  loading: boolean;
  copy: ReturnType<typeof buildCopy>;
  language: AppLanguage;
  onPick: (id: string) => void;
}> = ({ list, loading, copy, language, onPick }) => {
  if (loading) {
    return <p className="text-sm text-gray-500">…</p>;
  }
  if (list.length === 0) {
    return <p className="text-sm text-gray-500">{copy.empty}</p>;
  }
  return (
    <div className="rounded-lg border border-gray-200 bg-white">
      <header className="border-b border-gray-100 px-4 py-2 text-xs font-semibold uppercase tracking-wide text-gray-500">
        {copy.list}
      </header>
      <ul>
        {list.map((p) => (
          <li
            key={p.id}
            className="cursor-pointer border-b border-gray-100 p-4 last:border-b-0 hover:bg-gray-50"
            onClick={() => onPick(p.id)}
          >
            <div className="flex items-center justify-between">
              <div>
                <div className="flex items-center gap-2">
                  <span className="font-medium text-gray-900">{p.id.slice(0, 8)}</span>
                  <StatusBadge status={p.status} />
                  <span className="text-xs text-gray-500">{p.engineKind}</span>
                </div>
                <div className="mt-1 text-xs text-gray-500">
                  {copy.basisJob}: {p.basisJobId.slice(0, 8)} · {copy.sharpe}: {formatNumberForLanguage(p.baselineMetrics.sharpeRatio, language, { maximumFractionDigits: 2 })}
                </div>
              </div>
              <div className="text-right text-xs text-gray-500">
                <div>{formatDateTimeForLanguage(p.createdAt, language)}</div>
                <div>{copy.proposedBy}: {p.proposedBy}</div>
              </div>
            </div>
          </li>
        ))}
      </ul>
    </div>
  );
};

const StatusBadge: React.FC<{ status: PromotionStatus }> = ({ status }) => (
  <span className={`rounded-full border px-2 py-0.5 text-[10px] font-medium ${statusTone(status)}`}>
    {status}
  </span>
);

const PromotionDetailView: React.FC<{
  detail: PromotionDetail | null;
  loading: boolean;
  copy: ReturnType<typeof buildCopy>;
  language: AppLanguage;
  actionError: string | null;
  pendingAction: string | null;
  onBack: () => void;
  onTransition: (action: "approve" | "reject" | "activate" | "rollback") => void;
}> = ({ detail, loading, copy, language, actionError, pendingAction, onBack, onTransition }) => {
  if (loading || !detail) {
    return <p className="text-sm text-gray-500">…</p>;
  }
  const { promotion: p, events, shadowDiffs, health, agreementRatio, agreementSamples } = detail;
  const pct = (v?: number) =>
    v == null ? "—" : `${formatNumberForLanguage(v * 100, language, { maximumFractionDigits: 2 })}%`;
  const num = (v?: number, digits = 2) =>
    v == null ? "—" : formatNumberForLanguage(v, language, { maximumFractionDigits: digits });

  const canApprove = p.status === "pending_review";
  const canActivate = p.status === "shadow" || p.status === "approved";
  const canReject = p.status === "pending_review" || p.status === "shadow" || p.status === "approved";
  const canRollback = p.status === "active";

  return (
    <div className="space-y-4">
      <button onClick={onBack} className="text-xs text-indigo-600 hover:underline">
        {copy.back}
      </button>

      <div className="rounded-lg border border-gray-200 bg-white p-4">
        <header className="mb-4 flex items-center justify-between">
          <div>
            <h2 className="text-base font-semibold text-gray-800">{p.id.slice(0, 12)}</h2>
            <p className="text-xs text-gray-500">
              {copy.proposedBy}: {p.proposedBy} · {formatDateTimeForLanguage(p.createdAt, language)}
            </p>
          </div>
          <div className="flex items-center gap-2">
            <StatusBadge status={p.status} />
            {canApprove && (
              <ActionButton onClick={() => onTransition("approve")} disabled={pendingAction != null} tone="primary">
                {copy.approve}
              </ActionButton>
            )}
            {canActivate && (
              <ActionButton onClick={() => onTransition("activate")} disabled={pendingAction != null} tone="primary">
                {copy.activate}
              </ActionButton>
            )}
            {canReject && (
              <ActionButton onClick={() => onTransition("reject")} disabled={pendingAction != null} tone="danger">
                {copy.reject}
              </ActionButton>
            )}
            {canRollback && (
              <ActionButton onClick={() => onTransition("rollback")} disabled={pendingAction != null} tone="danger">
                {copy.rollback}
              </ActionButton>
            )}
          </div>
        </header>

        {actionError && <p className="mb-3 text-xs text-red-600">{actionError}</p>}

        <dl className="grid grid-cols-2 gap-3 text-xs md:grid-cols-4">
          <Field label={copy.basisJob} value={p.basisJobId.slice(0, 12)} />
          <Field label="engine" value={p.engineKind} />
          <Field label={copy.shadowDays} value={String(p.shadowDays)} />
          <Field label={copy.decayRatio} value={String(p.decayRatio)} />
          <Field label={copy.sharpe} value={num(p.baselineMetrics.sharpeRatio)} />
          <Field label={copy.cumulative} value={pct(p.baselineMetrics.cumulativeReturn)} />
          <Field label={copy.maxDD} value={pct(p.baselineMetrics.maxDrawdown)} />
          <Field label={copy.tradeCount} value={String(p.baselineMetrics.tradeCount)} />
          {p.baselineMetrics.oosSharpe != null && (
            <Field label={copy.oosSharpe} value={num(p.baselineMetrics.oosSharpe)} />
          )}
          {p.baselineMetrics.oosReturn != null && (
            <Field label={copy.oosReturn} value={pct(p.baselineMetrics.oosReturn)} />
          )}
        </dl>

        {p.notes && <p className="mt-3 text-xs text-gray-600">{p.notes}</p>}
      </div>

      {/* Shadow comparisons */}
      <section className="rounded-lg border border-gray-200 bg-white p-4">
        <header className="mb-3 flex items-center justify-between">
          <h3 className="text-sm font-semibold text-gray-700">{copy.shadowSection}</h3>
          <span className="text-xs text-gray-500">
            {copy.agreementRatio}: {formatNumberForLanguage(agreementRatio * 100, language, { maximumFractionDigits: 1 })}% ({agreementSamples})
          </span>
        </header>
        {shadowDiffs.length === 0 ? (
          <p className="text-xs text-gray-500">{copy.noShadow}</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[600px] text-xs">
              <thead className="text-left text-gray-500">
                <tr>
                  <th className="py-1">{copy.colDate}</th>
                  <th>{copy.colShadowDecision}</th>
                  <th>{copy.colActiveDecision}</th>
                  <th>{copy.colAgreement}</th>
                </tr>
              </thead>
              <tbody>
                {shadowDiffs.map((d) => (
                  <tr key={d.id} className="border-t border-gray-100">
                    <td className="py-1.5">{formatDateForLanguage(d.tradingDate, language)}</td>
                    <td className="py-1.5">{summariseDecision(d.shadowDecision)}</td>
                    <td className="py-1.5">{summariseDecision(d.activeDecision)}</td>
                    <td className="py-1.5">
                      <span className={`rounded-full border px-2 py-0.5 text-[10px] ${d.agreement ? "border-emerald-200 bg-emerald-50 text-emerald-700" : "border-red-200 bg-red-50 text-red-700"}`}>
                        {d.agreement ? copy.agreementYes : copy.agreementNo}
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      {/* Decay monitor snapshots */}
      <section className="rounded-lg border border-gray-200 bg-white p-4">
        <h3 className="mb-3 text-sm font-semibold text-gray-700">{copy.healthSection}</h3>
        {health.length === 0 ? (
          <p className="text-xs text-gray-500">{copy.noHealth}</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[600px] text-xs">
              <thead className="text-left text-gray-500">
                <tr>
                  <th className="py-1">{copy.colDate}</th>
                  <th>{copy.colWindow}</th>
                  <th>{copy.colSharpe}</th>
                  <th>{copy.colDecay}</th>
                  <th>{copy.colFlag}</th>
                </tr>
              </thead>
              <tbody>
                {health.map((h) => (
                  <tr key={h.id} className="border-t border-gray-100">
                    <td className="py-1.5">{formatDateTimeForLanguage(h.snapshotAt, language)}</td>
                    <td className="py-1.5">{h.windowDays}d</td>
                    <td className="py-1.5">{num(h.actualSharpe)}</td>
                    <td className="py-1.5">{num(h.sharpeDecayRatio)}</td>
                    <td className="py-1.5">
                      <span className={`rounded-full border px-2 py-0.5 text-[10px] ${h.decayFlag ? "border-red-200 bg-red-50 text-red-700" : "border-emerald-200 bg-emerald-50 text-emerald-700"}`}>
                        {h.decayFlag ? copy.decayFlagYes : copy.decayFlagNo}
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      {/* Audit timeline */}
      <section className="rounded-lg border border-gray-200 bg-white p-4">
        <h3 className="mb-3 text-sm font-semibold text-gray-700">{copy.events}</h3>
        <ol className="space-y-2 text-xs">
          {events.map((ev) => (
            <li key={ev.id} className="flex items-baseline gap-3">
              <span className="w-40 text-gray-500">{formatDateTimeForLanguage(ev.createdAt, language)}</span>
              <span className="font-medium text-gray-800">{ev.eventType}</span>
              {ev.actorUserId && <span className="text-gray-500">· {ev.actorUserId}</span>}
            </li>
          ))}
        </ol>
      </section>
    </div>
  );
};

const Field: React.FC<{ label: string; value: string }> = ({ label, value }) => (
  <div>
    <dt className="text-[10px] uppercase tracking-wide text-gray-500">{label}</dt>
    <dd className="text-gray-900">{value}</dd>
  </div>
);

const ActionButton: React.FC<{
  children: React.ReactNode;
  onClick: () => void;
  disabled?: boolean;
  tone: "primary" | "danger";
}> = ({ children, onClick, disabled, tone }) => {
  const className =
    tone === "primary"
      ? "rounded border border-indigo-600 bg-indigo-600 px-3 py-1 text-xs font-medium text-white hover:bg-indigo-700 disabled:opacity-60"
      : "rounded border border-red-200 bg-red-50 px-3 py-1 text-xs font-medium text-red-700 hover:bg-red-100 disabled:opacity-60";
  return (
    <button type="button" className={className} onClick={onClick} disabled={disabled}>
      {children}
    </button>
  );
};

// summariseDecision picks the salient fields from the per-side
// decision blob for compact tabular display. Keeps the UI honest
// when the comparator's schema grows: we only show what we know
// about, the rest stays in the raw blob and is available via the
// admin/dev tools.
function summariseDecision(d: Record<string, unknown> | null | undefined): string {
  if (!d) return "—";
  const action = (d.action as string | undefined) ?? "—";
  const symbol = (d.symbol as string | undefined) ?? "";
  const qty = d.quantity as number | undefined;
  if (action === "hold") {
    return "hold";
  }
  return `${action}${symbol ? " " + symbol : ""}${qty != null ? " ×" + qty : ""}`;
}

export default Promotions;
