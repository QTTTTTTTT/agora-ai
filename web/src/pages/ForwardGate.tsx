import React, { useCallback, useEffect, useMemo, useState } from "react";
import { useParams } from "react-router-dom";
import { apiGet, formatApiError, type ForwardAgentGateStatus, type ForwardGateCheck, type ForwardGateStatus } from "../lib/api";
import { formatDateForLanguage, formatDateTimeForLanguage, formatNumberForLanguage, useAppPreferences } from "../lib/preferences";

function statusTone(status?: string): string {
  switch ((status ?? "").toLowerCase()) {
    case "eligible":
    case "pass":
      return "border-emerald-200 bg-emerald-50 text-emerald-700";
    case "blocked":
    case "block":
    case "fail":
      return "border-red-200 bg-red-50 text-red-700";
    case "pending":
    case "warn":
    case "warning":
      return "border-amber-200 bg-amber-50 text-amber-700";
    default:
      return "border-gray-200 bg-gray-50 text-gray-600";
  }
}

function humanize(value?: string): string {
  return (value ?? "")
    .replace(/[_-]+/g, " ")
    .trim()
    .replace(/\b\w/g, (char) => char.toUpperCase()) || "—";
}

// remediationHintFor maps the canonical blocker / check keys our
// server emits to a localised "here's what to do" sentence. Keeps
// the user from having to message a maintainer to find out what
// "insufficient_navs" or "insufficient_sharpe" actually means
// they should DO. Falls back to undefined when we don't have a
// curated hint — caller renders nothing in that case rather than
// guess and confuse.
function remediationHintFor(
  keyOrBlocker: string,
  current: number | undefined,
  required: number | undefined,
  language: "zh-CN" | "en-US",
): string | undefined {
  const key = keyOrBlocker.toLowerCase().replace(/[^a-z]+/g, "_");
  const isEnglish = language === "en-US";
  const need =
    typeof current === "number" && typeof required === "number"
      ? Math.max(0, required - current)
      : undefined;
  if (key.includes("insufficient_days") || key.includes("liveness_days")) {
    return isEnglish
      ? `Run live trading ${need !== undefined ? `for ${need} more day(s)` : "longer"} before retrying — the gate counts only days with at least one valid NAV observation.`
      : `继续实盘运行${need !== undefined ? `还需 ${need} 天` : "更长时间"}后再尝试 — 准入只统计有有效 NAV 观测的交易日。`;
  }
  if (key.includes("insufficient_navs") || key.includes("nav_points")) {
    return isEnglish
      ? `Wait for ${need !== undefined ? `${need} more daily NAV observation(s)` : "more daily NAV observations"}. NAV is sampled at end-of-day, so partial-day live runs do not count.`
      : `继续等${need !== undefined ? `${need} 个` : "更多"}每日 NAV 观测点。NAV 在收盘后采样，不足一日的实盘不计入。`;
  }
  if (key.includes("insufficient_sharpe")) {
    return isEnglish
      ? "Forward Sharpe is below the listing threshold. Improve consistency (reduce vol) or absolute return; gate computes Sharpe from daily NAV deltas annualised."
      : "前向 Sharpe 未达上架门槛。可降低波动率或提升绝对收益；准入按日 NAV 变化年化计算 Sharpe。";
  }
  if (key.includes("insufficient_winrate") || key.includes("hit_rate")) {
    return isEnglish
      ? "Hit rate (% of profitable days) is below threshold. Tighten exit logic or stop-loss to avoid the long tail of losing days."
      : "胜率（盈利日比例）低于门槛。可收紧止盈/止损逻辑，避免长尾亏损日。";
  }
  if (key.includes("max_drawdown") || key.includes("drawdown_excess")) {
    return isEnglish
      ? "Max drawdown exceeds the gate ceiling. Either reduce position sizing during volatile regimes or adjust the drawdown threshold in fund settings."
      : "最大回撤超过准入上限。可在波动加剧时缩减仓位，或在 Fund Settings 调整回撤阈值。";
  }
  if (key.includes("agent_") && key.includes("not_eligible")) {
    return isEnglish
      ? "One or more team agents has not yet completed their own forward-test gate. Open the agent's drill-down panel below to see specific blockers."
      : "一个或多个团队 Agent 还未通过自己的前向测试准入。展开下方对应 Agent 的详情查看具体阻塞项。";
  }
  return undefined;
}

const ForwardGate: React.FC = () => {
  const { fundId } = useParams<{ fundId: string }>();
  const { language } = useAppPreferences();
  const [gate, setGate] = useState<ForwardGateStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  // expandedChecks / expandedAgents — drill-down state. Each
  // failing card collapses the remediation hint + raw payload
  // by default; click to expand. The Set<string> shape is
  // straightforward and avoids the "all-expanded" footgun
  // when there are 20+ failing checks.
  const [expandedChecks, setExpandedChecks] = useState<Set<string>>(new Set());
  const [expandedAgents, setExpandedAgents] = useState<Set<string>>(new Set());

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            title: "Forward-test gate",
            subtitle: "Check whether the fund strategy and each team agent have enough live forward-test evidence before listing, promotion, or broader reuse.",
            loadError: "Failed to load forward-test gate",
            missingFundId: "Missing fundId",
            retry: "Retry",
            overallStatus: "Overall status",
            liveDays: "Live days",
            navPoints: "NAV observations",
            trackRecord: "Forward track record",
            checks: "Gate checks",
            agents: "Agent admission",
            generatedAt: "Generated",
            period: "Period",
            totalReturn: "Total return",
            annualReturn: "Annual return",
            sharpe: "Sharpe",
            maxDrawdown: "Max drawdown",
            volatility: "Volatility",
            winRate: "Win rate",
            current: "Current",
            required: "Required",
            blockers: "Blockers",
            warnings: "Warnings",
            canList: "Can list",
            cannotList: "Not ready",
            noAgents: "No team agents are bound to this fund yet.",
            noTrackRecord: "Track record requires at least two valid NAV observations.",
            showDetails: "Show details",
            hideDetails: "Hide details",
            remediation: "How to address this",
            rawPayload: "Raw gate payload",
            agentDrillTitle: "Agent diagnostics",
          }
        : {
            title: "Forward-test 准入",
            subtitle: "检查基金策略与团队 Agent 是否已有足够实盘前向测试证据，再进入上架、推广或复用流程。",
            loadError: "加载 Forward-test 准入状态失败",
            missingFundId: "缺少 fundId",
            retry: "重试",
            overallStatus: "整体状态",
            liveDays: "实盘天数",
            navPoints: "净值观测数",
            trackRecord: "前向测试履历",
            checks: "准入检查",
            agents: "Agent 准入",
            generatedAt: "生成时间",
            period: "区间",
            totalReturn: "累计收益",
            annualReturn: "年化收益",
            sharpe: "夏普",
            maxDrawdown: "最大回撤",
            volatility: "波动率",
            winRate: "胜率",
            current: "当前",
            required: "要求",
            blockers: "阻塞项",
            warnings: "提醒项",
            canList: "可上架",
            cannotList: "未就绪",
            noAgents: "当前基金还没有绑定团队 Agent。",
            noTrackRecord: "至少需要两个有效 NAV 观测点才能计算履历。",
            showDetails: "查看详情",
            hideDetails: "收起详情",
            remediation: "如何解除阻塞",
            rawPayload: "原始 Gate 数据",
            agentDrillTitle: "Agent 诊断",
          },
    [language],
  );

  const loadGate = useCallback(async () => {
    if (!fundId) {
      setError(copy.missingFundId);
      setLoading(false);
      return;
    }
    setLoading(true);
    setError(null);
    try {
      setGate(await apiGet<ForwardGateStatus>(`/api/funds/${fundId}/forward-gate`));
    } catch (err) {
      setError(formatApiError(err, copy.loadError));
    } finally {
      setLoading(false);
    }
  }, [copy.loadError, copy.missingFundId, fundId]);

  useEffect(() => {
    void loadGate();
  }, [loadGate]);

  const formatPercent = useCallback(
    (value?: number) => {
      if (typeof value !== "number" || Number.isNaN(value)) return "—";
      const normalized = Math.abs(value) <= 1 ? value * 100 : value;
      const sign = normalized > 0 ? "+" : "";
      return `${sign}${formatNumberForLanguage(normalized, language, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}%`;
    },
    [language],
  );

  const toggleCheckExpanded = (key: string) =>
    setExpandedChecks((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });

  const toggleAgentExpanded = (id: string) =>
    setExpandedAgents((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  const renderCheck = (check: ForwardGateCheck) => {
    const expanded = expandedChecks.has(check.key);
    const status = (check.status ?? "").toLowerCase();
    const isFailingOrWarning = status === "blocked" || status === "block" || status === "fail" || status === "warn" || status === "warning";
    const remediation = isFailingOrWarning
      ? remediationHintFor(check.key, check.current, check.required, language)
      : undefined;

    return (
      <div key={check.key} className="rounded-xl border border-gray-200 bg-white p-4">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <p className="font-medium text-gray-900">{check.label || humanize(check.key)}</p>
            {check.message ? <p className="mt-1 text-sm leading-6 text-gray-600">{check.message}</p> : null}
          </div>
          <span className={`rounded-full border px-3 py-1 text-xs font-medium ${statusTone(check.status)}`}>{humanize(check.status)}</span>
        </div>
        {(typeof check.current === "number" || typeof check.required === "number") ? (
          <div className="mt-3 flex flex-wrap gap-3 text-xs text-gray-500">
            {typeof check.current === "number" ? <span>{copy.current}: {formatNumberForLanguage(check.current, language)}</span> : null}
            {typeof check.required === "number" ? <span>{copy.required}: {formatNumberForLanguage(check.required, language)}</span> : null}
          </div>
        ) : null}

        {/* Drill-down: only render the toggle when there's something extra to show
            (a remediation hint or any extra payload). Avoids "Show details" buttons
            that lead to an empty drawer. */}
        {(remediation || isFailingOrWarning) ? (
          <button
            type="button"
            onClick={() => toggleCheckExpanded(check.key)}
            aria-expanded={expanded}
            className="mt-3 inline-flex items-center gap-1 text-xs font-medium text-indigo-600 hover:text-indigo-500"
          >
            <span aria-hidden="true">{expanded ? "▾" : "▸"}</span>
            {expanded ? copy.hideDetails : copy.showDetails}
          </button>
        ) : null}
        {expanded ? (
          <div className="mt-3 space-y-3">
            {remediation ? (
              <div className="rounded-lg border border-indigo-200 bg-indigo-50 p-3 text-xs leading-5 text-indigo-900">
                <p className="font-medium">{copy.remediation}</p>
                <p className="mt-1">{remediation}</p>
              </div>
            ) : null}
            <details className="text-xs text-gray-500">
              <summary className="cursor-pointer select-none text-gray-600">{copy.rawPayload}</summary>
              <pre className="mt-2 max-h-48 overflow-auto rounded-lg bg-gray-50 p-3 font-mono text-[11px] text-gray-700">
                {JSON.stringify(check, null, 2)}
              </pre>
            </details>
          </div>
        ) : null}
      </div>
    );
  };

  const renderAgent = (agent: ForwardAgentGateStatus) => {
    const expanded = expandedAgents.has(agent.agentId);
    const hasIssues = (agent.blockers?.length ?? 0) > 0 || (agent.warnings?.length ?? 0) > 0;

    return (
      <div key={agent.agentId} className="rounded-2xl border border-gray-200 bg-white p-5 shadow-sm">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <p className="text-base font-semibold text-gray-900">{agent.agentName || agent.agentId}</p>
            <p className="mt-1 text-sm text-gray-500">{humanize(agent.role)}{agent.focus ? ` · ${agent.focus}` : ""}</p>
            {agent.joinedAt ? <p className="mt-1 text-xs text-gray-400">{formatDateForLanguage(agent.joinedAt, language)}</p> : null}
          </div>
          <span className={`rounded-full border px-3 py-1 text-xs font-medium ${statusTone(agent.status)}`}>{agent.canList ? copy.canList : copy.cannotList}</span>
        </div>
        {agent.blockers?.length ? (
          <div className="mt-4 rounded-xl bg-red-50 p-3 text-sm text-red-700">
            <p className="font-medium">{copy.blockers}</p>
            <ul className="mt-1 list-disc space-y-1 pl-5">{agent.blockers.map((item) => <li key={item}>{item}</li>)}</ul>
          </div>
        ) : null}
        {agent.warnings?.length ? (
          <div className="mt-4 rounded-xl bg-amber-50 p-3 text-sm text-amber-700">
            <p className="font-medium">{copy.warnings}</p>
            <ul className="mt-1 list-disc space-y-1 pl-5">{agent.warnings.map((item) => <li key={item}>{item}</li>)}</ul>
          </div>
        ) : null}

        {hasIssues ? (
          <button
            type="button"
            onClick={() => toggleAgentExpanded(agent.agentId)}
            aria-expanded={expanded}
            className="mt-3 inline-flex items-center gap-1 text-xs font-medium text-indigo-600 hover:text-indigo-500"
          >
            <span aria-hidden="true">{expanded ? "▾" : "▸"}</span>
            {expanded ? copy.hideDetails : copy.showDetails}
          </button>
        ) : null}
        {expanded && hasIssues ? (
          <div className="mt-3 space-y-3">
            <p className="text-xs font-semibold uppercase tracking-wider text-gray-500">{copy.agentDrillTitle}</p>
            <ul className="space-y-2 text-xs">
              {(agent.blockers ?? []).map((item) => {
                const hint = remediationHintFor(item, undefined, undefined, language);
                return (
                  <li key={`blocker-${item}`} className="rounded-lg border border-red-100 bg-red-50/60 p-3">
                    <p className="font-mono text-[11px] text-red-800">{item}</p>
                    {hint ? <p className="mt-1 text-red-700">{hint}</p> : null}
                  </li>
                );
              })}
              {(agent.warnings ?? []).map((item) => {
                const hint = remediationHintFor(item, undefined, undefined, language);
                return (
                  <li key={`warn-${item}`} className="rounded-lg border border-amber-100 bg-amber-50/60 p-3">
                    <p className="font-mono text-[11px] text-amber-800">{item}</p>
                    {hint ? <p className="mt-1 text-amber-700">{hint}</p> : null}
                  </li>
                );
              })}
            </ul>
            <details className="text-xs text-gray-500">
              <summary className="cursor-pointer select-none text-gray-600">{copy.rawPayload}</summary>
              <pre className="mt-2 max-h-48 overflow-auto rounded-lg bg-gray-50 p-3 font-mono text-[11px] text-gray-700">
                {JSON.stringify(agent, null, 2)}
              </pre>
            </details>
          </div>
        ) : null}
      </div>
    );
  };

  if (loading) {
    return <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm"><div className="h-7 w-56 animate-pulse rounded bg-gray-200" /><div className="mt-4 h-4 w-full animate-pulse rounded bg-gray-100" /></div>;
  }

  if (error) {
    return <div className="rounded-2xl border border-red-200 bg-red-50 p-6 text-sm text-red-700"><p>{error}</p><button onClick={() => void loadGate()} className="mt-4 rounded-lg bg-red-600 px-4 py-2 text-white">{copy.retry}</button></div>;
  }

  return (
    <div className="space-y-6">
      <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
        <div className="flex flex-col gap-4 lg:flex-row lg:items-start lg:justify-between">
          <div>
            <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
            <p className="mt-2 max-w-3xl text-sm leading-6 text-gray-500">{copy.subtitle}</p>
            {gate?.summary ? <p className="mt-3 rounded-xl bg-gray-50 p-4 text-sm leading-6 text-gray-700">{gate.summary}</p> : null}
          </div>
          <span className={`inline-flex rounded-full border px-4 py-2 text-sm font-semibold ${statusTone(gate?.status)}`}>{copy.overallStatus}: {humanize(gate?.status)}</span>
        </div>
      </div>

      <div className="grid gap-4 md:grid-cols-3">
        <div className="rounded-2xl border border-gray-200 bg-white p-5 shadow-sm"><p className="text-sm text-gray-500">{copy.liveDays}</p><p className="mt-2 text-2xl font-bold text-gray-900">{formatNumberForLanguage(gate?.liveDays ?? 0, language)} / {formatNumberForLanguage(gate?.requiredDays ?? 0, language)}</p></div>
        <div className="rounded-2xl border border-gray-200 bg-white p-5 shadow-sm"><p className="text-sm text-gray-500">{copy.navPoints}</p><p className="mt-2 text-2xl font-bold text-gray-900">{formatNumberForLanguage(gate?.navPoints ?? 0, language)} / {formatNumberForLanguage(gate?.requiredNavs ?? 0, language)}</p></div>
        <div className="rounded-2xl border border-gray-200 bg-white p-5 shadow-sm"><p className="text-sm text-gray-500">{copy.period}</p><p className="mt-2 text-lg font-semibold text-gray-900">{gate?.startDate ? formatDateForLanguage(gate.startDate, language) : "—"} → {gate?.endDate ? formatDateForLanguage(gate.endDate, language) : "—"}</p><p className="mt-2 text-xs text-gray-400">{copy.generatedAt}: {gate?.generatedAt ? formatDateTimeForLanguage(gate.generatedAt, language) : "—"}</p></div>
      </div>

      <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
        <h2 className="text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.trackRecord}</h2>
        {gate?.trackRecord ? (
          <div className="mt-4 grid gap-3 sm:grid-cols-2 xl:grid-cols-6">
            {[{ label: copy.totalReturn, value: formatPercent(gate.trackRecord.totalReturn) }, { label: copy.annualReturn, value: formatPercent(gate.trackRecord.annualReturn) }, { label: copy.sharpe, value: formatNumberForLanguage(gate.trackRecord.sharpe, language, { maximumFractionDigits: 2 }) }, { label: copy.maxDrawdown, value: formatPercent(-Math.abs(gate.trackRecord.maxDrawdown)) }, { label: copy.volatility, value: formatPercent(gate.trackRecord.volatility) }, { label: copy.winRate, value: formatPercent(gate.trackRecord.winRate) }].map((item) => (
              <div key={item.label} className="rounded-xl bg-gray-50 p-4"><p className="text-xs text-gray-500">{item.label}</p><p className="mt-2 text-lg font-semibold text-gray-900">{item.value}</p></div>
            ))}
          </div>
        ) : <div className="mt-4 rounded-xl border border-dashed border-gray-200 bg-gray-50 p-5 text-sm text-gray-500">{copy.noTrackRecord}</div>}
      </div>

      <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
        <h2 className="text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.checks}</h2>
        <div className="mt-4 grid gap-4 lg:grid-cols-3">{(gate?.checks ?? []).map(renderCheck)}</div>
      </div>

      <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
        <h2 className="text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.agents}</h2>
        {(gate?.agents ?? []).length ? <div className="mt-4 grid gap-4 xl:grid-cols-2">{gate?.agents?.map(renderAgent)}</div> : <div className="mt-4 rounded-xl border border-dashed border-gray-200 bg-gray-50 p-5 text-sm text-gray-500">{copy.noAgents}</div>}
      </div>
    </div>
  );
};

export default ForwardGate;
