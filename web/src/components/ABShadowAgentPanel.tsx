import React, { useEffect, useMemo, useState } from "react";

import {
  fetchABShadowAgents,
  formatApiError,
  type ABEvolutionConfigDiff,
  type ABTestShadowAgent,
  type ABTestShadowAgentResponse,
  type ABTestShadowAgentVariant,
} from "../lib/api";

interface ABShadowAgentPanelProps {
  testId: string;
  /** Whether the parent test has reached `analyzed` status. When
   *  false the panel renders a "run analyze first" hint and skips
   *  the network call so /shadow-agents isn't hit on draft tests. */
  analyzed: boolean;
  language?: "zh-CN" | "en-US";
  /** Initial expand state. Defaults to collapsed because the panel
   *  is dense; collapsing keeps the comparison page from feeling
   *  cluttered until the user opts in. */
  defaultOpen?: boolean;
  /** When true, render the "deterministic shadow" sanity-check
   *  banner. Defaults to true while Card K (real LLM shadow runs)
   *  is still on the roadmap so users don't over-interpret B's
   *  numbers as evidence of strategy differentiation. */
  showDeterministicBanner?: boolean;
}

const COPY = {
  "zh-CN": {
    sectionTitle: "影子 Agent 对比",
    sectionSubtitle: "查看 A 组与 B 组每位 agent 在影子运行中学到的内容、调整建议与提议的演化配置差异",
    expand: "展开",
    collapse: "收起",
    loading: "加载影子 agent 数据…",
    error: "加载失败",
    retry: "重试",
    empty: "该测试暂无影子 agent 学习数据",
    notAnalyzedYet: "完成「生成分析」后即可查看 A vs B 影子 agent 学习对比",
    columnA: "A 组",
    columnB: "B 组",
    eventCount: "学习事件数",
    latestDate: "最新事件日期",
    lessons: "关键经验",
    adjustments: "建议调整",
    summaries: "近期总结",
    timeline: "逐日时间线",
    memories: "影子记忆",
    proposedDiff: "提议的 evolution_config 变更",
    diffAdded: "新增",
    diffChanged: "变更（旧 → 新）",
    diffRemoved: "移除",
    noDiff: "与当前 evolution_config 一致，无需变更",
    deterministicShadowBanner: "当前 B 组采用确定性影子执行策略，数据用于策略参数 sanity check；后续 Card K 将引入真实 LLM 影子运行。",
    noAgents: "该侧暂无 agent 学习记录",
    noLessons: "—",
    expandAgent: "展开详情",
    collapseAgent: "收起详情",
  },
  "en-US": {
    sectionTitle: "Shadow agent comparison",
    sectionSubtitle: "Review what each variant's agents learned during shadow execution — lessons, adjustments, and proposed evolution-config changes.",
    expand: "Show",
    collapse: "Hide",
    loading: "Loading shadow agents…",
    error: "Failed to load",
    retry: "Retry",
    empty: "No shadow learning data for this test yet",
    notAnalyzedYet: "Run \u201cGenerate analysis\u201d first to compare A vs B shadow agent learning.",
    columnA: "Variant A",
    columnB: "Variant B",
    eventCount: "Learning events",
    latestDate: "Latest event",
    lessons: "Lessons",
    adjustments: "Adjustments",
    summaries: "Recent summaries",
    timeline: "Daily timeline",
    memories: "Shadow memories",
    proposedDiff: "Proposed evolution_config change",
    diffAdded: "Added",
    diffChanged: "Changed (prev \u2192 new)",
    diffRemoved: "Removed",
    noDiff: "No change vs current evolution_config",
    deterministicShadowBanner: "Variant B currently uses deterministic shadow execution; numbers are sanity-check only. Card K will introduce real LLM shadow runs.",
    noAgents: "No shadow learning recorded on this side yet",
    noLessons: "—",
    expandAgent: "Show details",
    collapseAgent: "Hide details",
  },
};

function formatDateForDisplay(iso?: string): string {
  if (!iso) return "—";
  return iso.slice(0, 10);
}

function stableValueRender(v: unknown): string {
  if (v === null || v === undefined) return "—";
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

const EvolutionDiffBlock: React.FC<{
  diff?: ABEvolutionConfigDiff;
  copy: (typeof COPY)["zh-CN"];
}> = ({ diff, copy }) => {
  if (!diff || ((!diff.added || Object.keys(diff.added).length === 0) && (!diff.changed || Object.keys(diff.changed).length === 0) && (!diff.removed || diff.removed.length === 0))) {
    return <p className="text-xs text-gray-500">{copy.noDiff}</p>;
  }
  return (
    <div className="space-y-2 text-xs">
      {diff.added && Object.keys(diff.added).length > 0 ? (
        <div>
          <span className="rounded-full bg-emerald-50 px-2 py-0.5 font-medium text-emerald-700 ring-1 ring-emerald-100">
            {copy.diffAdded}
          </span>
          <ul className="mt-1 ml-1 space-y-0.5 text-gray-700">
            {Object.entries(diff.added).map(([k, v]) => (
              <li key={`added-${k}`} className="font-mono text-[11px] break-all">
                <span className="text-gray-500">{k}</span>: {stableValueRender(v)}
              </li>
            ))}
          </ul>
        </div>
      ) : null}
      {diff.changed && Object.keys(diff.changed).length > 0 ? (
        <div>
          <span className="rounded-full bg-amber-50 px-2 py-0.5 font-medium text-amber-700 ring-1 ring-amber-100">
            {copy.diffChanged}
          </span>
          <ul className="mt-1 ml-1 space-y-0.5 text-gray-700">
            {Object.entries(diff.changed).map(([k, [prev, next]]) => (
              <li key={`changed-${k}`} className="font-mono text-[11px] break-all">
                <span className="text-gray-500">{k}</span>: <span className="text-red-600 line-through">{stableValueRender(prev)}</span>
                {" \u2192 "}
                <span className="text-emerald-700">{stableValueRender(next)}</span>
              </li>
            ))}
          </ul>
        </div>
      ) : null}
      {diff.removed && diff.removed.length > 0 ? (
        <div>
          <span className="rounded-full bg-red-50 px-2 py-0.5 font-medium text-red-700 ring-1 ring-red-100">
            {copy.diffRemoved}
          </span>
          <ul className="mt-1 ml-1 space-y-0.5 text-gray-700">
            {diff.removed.map((k) => (
              <li key={`removed-${k}`} className="font-mono text-[11px] break-all text-red-600">
                {k}
              </li>
            ))}
          </ul>
        </div>
      ) : null}
    </div>
  );
};

const AgentCard: React.FC<{
  agent: ABTestShadowAgent;
  copy: (typeof COPY)["zh-CN"];
}> = ({ agent, copy }) => {
  const [open, setOpen] = useState(false);
  return (
    <article className="rounded-xl border border-gray-200 bg-white p-4 shadow-sm">
      <header className="flex flex-wrap items-start justify-between gap-2">
        <div>
          <p className="text-sm font-semibold text-gray-900">
            {agent.agentName || agent.agentId}
          </p>
          <p className="text-[11px] uppercase tracking-wide text-gray-400">
            {agent.role || "agent"}
          </p>
        </div>
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          className="rounded-md border border-gray-200 px-2 py-1 text-[11px] font-medium text-gray-600 hover:bg-gray-50"
        >
          {open ? copy.collapseAgent : copy.expandAgent}
        </button>
      </header>

      <dl className="mt-3 grid grid-cols-2 gap-x-3 gap-y-1 text-xs text-gray-600">
        <dt className="text-gray-500">{copy.eventCount}</dt>
        <dd className="text-right font-mono text-gray-900">{agent.eventCount}</dd>
        <dt className="text-gray-500">{copy.latestDate}</dt>
        <dd className="text-right font-mono text-gray-900">{formatDateForDisplay(agent.latestTradingDate)}</dd>
      </dl>

      {agent.lessons && agent.lessons.length > 0 ? (
        <div className="mt-3">
          <p className="text-[11px] font-semibold uppercase tracking-wide text-gray-500">{copy.lessons}</p>
          <ul className="mt-1 list-disc space-y-1 pl-5 text-xs text-gray-700">
            {agent.lessons.map((lesson, idx) => (
              <li key={`lesson-${idx}`}>{lesson}</li>
            ))}
          </ul>
        </div>
      ) : null}

      {agent.adjustments && agent.adjustments.length > 0 ? (
        <div className="mt-3">
          <p className="text-[11px] font-semibold uppercase tracking-wide text-gray-500">{copy.adjustments}</p>
          <ul className="mt-1 list-disc space-y-1 pl-5 text-xs text-gray-700">
            {agent.adjustments.map((adj, idx) => (
              <li key={`adj-${idx}`}>{adj}</li>
            ))}
          </ul>
        </div>
      ) : null}

      {open ? (
        <div className="mt-4 space-y-4 border-t border-gray-100 pt-3">
          {agent.summaries && agent.summaries.length > 0 ? (
            <div>
              <p className="text-[11px] font-semibold uppercase tracking-wide text-gray-500">{copy.summaries}</p>
              <ul className="mt-1 list-disc space-y-1 pl-5 text-xs text-gray-700">
                {agent.summaries.map((s, idx) => (
                  <li key={`sum-${idx}`}>{s}</li>
                ))}
              </ul>
            </div>
          ) : null}

          <div>
            <p className="text-[11px] font-semibold uppercase tracking-wide text-gray-500">{copy.proposedDiff}</p>
            <div className="mt-1">
              <EvolutionDiffBlock diff={agent.proposedEvolutionDiff} copy={copy} />
            </div>
          </div>

          {agent.timeline && agent.timeline.length > 0 ? (
            <div>
              <p className="text-[11px] font-semibold uppercase tracking-wide text-gray-500">{copy.timeline}</p>
              <ul className="mt-1 space-y-2 text-xs">
                {agent.timeline.slice(0, 7).map((day, idx) => (
                  <li key={`day-${idx}`} className="rounded-md bg-gray-50 px-3 py-2">
                    <p className="font-mono text-[11px] text-gray-500">{day.tradingDate}</p>
                    {day.summary ? <p className="mt-1 text-gray-800">{day.summary}</p> : null}
                    {day.lessons && day.lessons.length > 0 ? (
                      <p className="mt-1 text-gray-600">
                        <span className="text-gray-400">L: </span>
                        {day.lessons.join("；")}
                      </p>
                    ) : null}
                    {day.adjustments && day.adjustments.length > 0 ? (
                      <p className="mt-0.5 text-gray-600">
                        <span className="text-gray-400">A: </span>
                        {day.adjustments.join("；")}
                      </p>
                    ) : null}
                  </li>
                ))}
              </ul>
            </div>
          ) : null}

          {agent.memories && agent.memories.length > 0 ? (
            <div>
              <p className="text-[11px] font-semibold uppercase tracking-wide text-gray-500">{copy.memories}</p>
              <ul className="mt-1 space-y-1 text-xs">
                {agent.memories.slice(0, 5).map((mem, idx) => (
                  <li key={`mem-${idx}`} className="rounded-md bg-gray-50 px-3 py-1.5">
                    <p className="font-mono text-[11px] text-gray-500">
                      {mem.tradingDate ? `${mem.tradingDate} · ` : ""}{mem.layer} · {mem.memoryKey}
                    </p>
                  </li>
                ))}
              </ul>
            </div>
          ) : null}
        </div>
      ) : null}
    </article>
  );
};

const VariantColumn: React.FC<{
  variant: ABTestShadowAgentVariant;
  title: string;
  accentClass: string;
  copy: (typeof COPY)["zh-CN"];
}> = ({ variant, title, accentClass, copy }) => (
  <div className={`rounded-2xl border ${accentClass} bg-white p-4`}>
    <div className="mb-3 flex flex-wrap items-baseline justify-between gap-2">
      <p className="text-sm font-semibold text-gray-800">{title}</p>
      {variant.variantName ? (
        <p className="text-xs text-gray-500">{variant.variantName}</p>
      ) : null}
    </div>
    {variant.agents.length === 0 ? (
      <div className="rounded-md border border-dashed border-gray-200 bg-gray-50 px-3 py-6 text-center text-xs text-gray-500">
        {copy.noAgents}
      </div>
    ) : (
      <div className="space-y-3">
        {variant.agents.map((agent) => (
          <AgentCard key={`${variant.variantKey}-${agent.agentId}`} agent={agent} copy={copy} />
        ))}
      </div>
    )}
  </div>
);

/**
 * ABShadowAgentPanel renders a collapsible side-by-side card that
 * surfaces each variant's shadow agent learning. Data flows from
 * `GET /api/abtests/:testId/shadow-agents`.
 *
 * Design choices worth knowing if you maintain this:
 *   - Defaults to collapsed. The panel is dense; collapsing keeps
 *     the AB compare page legible until the user opts in.
 *   - Lazy fetch: we don't hit the API until the user expands.
 *   - Renders BOTH variants always — even when one side has no
 *     learning we draw the empty column so the comparison stays
 *     symmetric.
 *   - When the parent test isn't analyzed yet we render a hint
 *     and skip the request entirely.
 */
export const ABShadowAgentPanel: React.FC<ABShadowAgentPanelProps> = ({
  testId,
  analyzed,
  language = "zh-CN",
  defaultOpen = false,
  showDeterministicBanner = true,
}) => {
  const copy = useMemo(() => COPY[language], [language]);
  const [open, setOpen] = useState(defaultOpen);
  const [data, setData] = useState<ABTestShadowAgentResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!open || data !== null || loading || !analyzed) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    fetchABShadowAgents(testId)
      .then((resp) => {
        if (!cancelled) setData(resp);
      })
      .catch((err) => {
        if (!cancelled) setError(formatApiError(err, copy.error));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [open, testId, data, loading, analyzed, copy.error]);

  // Whenever we flip to a different test, drop any cached payload
  // so the next expand triggers a fresh fetch instead of showing
  // stale rows from another AB test.
  useEffect(() => {
    setData(null);
    setError(null);
  }, [testId]);

  const retry = () => {
    setData(null);
    setError(null);
  };

  const variantA = data?.variants?.find((v) => v.variantKey === "A");
  const variantB = data?.variants?.find((v) => v.variantKey === "B");

  return (
    <section className="rounded-2xl border border-gray-200 bg-white shadow-sm">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-start justify-between gap-4 px-6 py-4 text-left"
        aria-expanded={open}
      >
        <div>
          <h3 className="text-base font-semibold text-gray-900">{copy.sectionTitle}</h3>
          <p className="mt-1 text-xs text-gray-500">{copy.sectionSubtitle}</p>
        </div>
        <span className="shrink-0 rounded-full border border-gray-200 px-3 py-1 text-xs font-medium text-gray-600">
          {open ? copy.collapse : copy.expand}
        </span>
      </button>

      {open ? (
        <div className="border-t border-gray-100 px-6 py-4">
          {!analyzed ? (
            <div className="rounded-lg border border-amber-100 bg-amber-50 px-4 py-3 text-sm text-amber-800">
              {copy.notAnalyzedYet}
            </div>
          ) : loading ? (
            <p className="text-sm text-gray-500">{copy.loading}</p>
          ) : error ? (
            <div className="flex items-center gap-3 text-sm">
              <span className="text-red-600">{copy.error}: {error}</span>
              <button
                type="button"
                onClick={retry}
                className="rounded-md border border-red-200 bg-red-50 px-2 py-1 text-xs font-medium text-red-700 hover:bg-red-100"
              >
                {copy.retry}
              </button>
            </div>
          ) : !data || !data.variants || data.variants.length === 0 ? (
            <p className="text-sm text-gray-500">{copy.empty}</p>
          ) : (
            <div className="space-y-4">
              {showDeterministicBanner ? (
                <div className="rounded-lg border border-sky-100 bg-sky-50 px-4 py-3 text-xs text-sky-800">
                  {copy.deterministicShadowBanner}
                </div>
              ) : null}
              <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
                <VariantColumn
                  variant={variantA ?? { variantKey: "A", agents: [] }}
                  title={copy.columnA}
                  accentClass="border-blue-100"
                  copy={copy}
                />
                <VariantColumn
                  variant={variantB ?? { variantKey: "B", agents: [] }}
                  title={copy.columnB}
                  accentClass="border-orange-100"
                  copy={copy}
                />
              </div>
            </div>
          )}
        </div>
      ) : null}
    </section>
  );
};

export default ABShadowAgentPanel;
