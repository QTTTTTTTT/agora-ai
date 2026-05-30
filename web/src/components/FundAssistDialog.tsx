import React, { useMemo, useState } from "react";
import {
  assistCreateFund,
  formatApiError,
  FundAssistRejectedError,
  type FundAssistPlan,
  type FundAssistPlanIssue,
  type FundAssistResponse,
} from "../lib/api";

// FundAssistDialog is the LLM-backed "describe the fund + team you
// want, we'll fill in the config for you" entry point. Two-stage
// flow:
//   1. user types a free-form brief, hits "生成方案 (preview)" → we
//      POST with dryRun=true; server returns a validated + defaulted
//      plan.
//   2. user reviews the plan card; on "确认创建" we POST with
//      dryRun=false → server creates the fund + agents and returns
//      the fund id, which we hand back to the parent so it can
//      navigate.
//
// We deliberately don't allow inline plan editing: the field set is
// large (universe, specialization, system prompts) and the whole
// point of the assistant is to skip that. Users who want a tweaked
// plan should refine the prompt and regenerate; users who want full
// control should use the existing manual create-fund modal.
export interface FundAssistDialogProps {
  companyId: string;
  companyName: string;
  open: boolean;
  onClose: () => void;
  // onCreated is fired after dryRun=false succeeds. Parent typically
  // navigates to /funds/{fundId} or refreshes the company list.
  onCreated: (resp: FundAssistResponse) => void;
}

const SAMPLE_PROMPTS: { label: string; prompt: string }[] = [
  {
    label: "美股 AI 主题",
    prompt:
      "做一个美股 AI 主题基金，初始资金 100 万美元，重点跟踪 NVDA、AMD、AVGO。团队需要：1 个 PM，2 个研究员（一个专门研究 NVDA，一个研究 AMD/AVGO 算力链），1 个交易员。",
  },
  {
    label: "A 股 OCS 光通信",
    prompt:
      "做一只 A 股 OCS 光交换 / 硅光主题基金，关注德科立 (688205) 和腾景科技 (688195)。团队需要 1 个 PM，2 个个股研究员（每只一个），1 个 A 股交易员。",
  },
  {
    label: "港股科技高息",
    prompt:
      "做一个港股科技 + 高息基金，覆盖腾讯 0700 和友邦 1299。团队 1 PM、2 研究员（按公司分工）、1 交易员。",
  },
];

function PlanPreviewCard({ plan, warnings }: { plan: FundAssistPlan; warnings?: string[] }) {
  const fund = plan.fund;
  return (
    <div className="rounded-xl border border-indigo-200 bg-indigo-50/50 p-4 text-sm text-slate-800">
      <div className="mb-2 flex items-center gap-2">
        <span className="rounded-full bg-indigo-600 px-2 py-0.5 text-xs font-medium text-white">AI 推荐方案</span>
        <span className="text-xs text-slate-500">请先确认下面的配置，再点击创建</span>
      </div>

      {plan.rationale ? (
        <p className="mb-3 rounded-lg bg-white/70 px-3 py-2 text-xs italic text-slate-600">{plan.rationale}</p>
      ) : null}

      <div className="grid grid-cols-2 gap-3">
        <div>
          <p className="text-xs uppercase tracking-wide text-slate-500">基金</p>
          <p className="font-medium">{fund.name}</p>
          <p className="text-xs text-slate-600">{fund.description ?? "—"}</p>
        </div>
        <div>
          <p className="text-xs uppercase tracking-wide text-slate-500">市场 / 资产</p>
          <p>
            <span className="font-mono text-xs">{fund.market}</span>
            {fund.exchange ? <span className="ml-1 text-slate-500">/ {fund.exchange}</span> : null}
            {fund.assetClass ? <span className="ml-1 text-slate-500">· {fund.assetClass}</span> : null}
          </p>
          <p className="text-xs text-slate-600">
            初始资金：{(fund.initialCapital ?? 0).toLocaleString()} {fund.baseCurrency ?? ""}
          </p>
        </div>
      </div>

      {fund.universe?.symbols && fund.universe.symbols.length > 0 ? (
        <div className="mt-3">
          <p className="text-xs uppercase tracking-wide text-slate-500">投资标的</p>
          <p className="font-mono text-xs">{fund.universe.symbols.join(", ")}</p>
        </div>
      ) : null}

      <div className="mt-3">
        <p className="text-xs uppercase tracking-wide text-slate-500">团队（共 {plan.agents.length} 人）</p>
        <ul className="mt-1 space-y-1">
          {plan.agents.map((ag, idx) => (
            <li key={idx} className="rounded-md bg-white/70 px-2 py-1 text-xs">
              <span className="font-medium uppercase">{ag.role}</span>
              {ag.name ? <span className="ml-1 text-slate-700">· {ag.name}</span> : null}
              {ag.focus ? <span className="ml-1 font-mono text-slate-500">[{ag.focus}]</span> : null}
              {ag.systemPrompt ? <p className="mt-0.5 text-slate-500">{ag.systemPrompt}</p> : null}
            </li>
          ))}
        </ul>
      </div>

      {warnings && warnings.length > 0 ? (
        <div className="mt-3 rounded-lg border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-800">
          <p className="font-medium">提示</p>
          <ul className="ml-4 list-disc">
            {warnings.map((w, i) => (
              <li key={i}>{w}</li>
            ))}
          </ul>
        </div>
      ) : null}
    </div>
  );
}

function IssuesCard({ issues, plan }: { issues: FundAssistPlanIssue[]; plan?: FundAssistPlan }) {
  return (
    <div className="rounded-xl border border-rose-300 bg-rose-50 p-4 text-sm text-rose-900">
      <p className="mb-2 font-medium">AI 输出的方案未通过校验，请修改提示词后重试：</p>
      <ul className="ml-5 list-disc space-y-1 text-xs">
        {issues.map((iss, i) => (
          <li key={i}>
            <span className="font-mono text-[11px] opacity-70">{iss.field}</span>
            <span className="ml-1">{iss.message}</span>
          </li>
        ))}
      </ul>
      {plan ? (
        <details className="mt-2 text-xs">
          <summary className="cursor-pointer">查看 AI 当时给出的方案</summary>
          <pre className="mt-2 max-h-48 overflow-auto rounded-md bg-white/80 p-2 text-[11px] text-slate-700">
            {JSON.stringify(plan, null, 2)}
          </pre>
        </details>
      ) : null}
    </div>
  );
}

export function FundAssistDialog({ companyId, companyName, open, onClose, onCreated }: FundAssistDialogProps) {
  const [prompt, setPrompt] = useState("");
  const [stage, setStage] = useState<"idle" | "previewing" | "creating">("idle");
  const [plan, setPlan] = useState<FundAssistPlan | null>(null);
  const [planWarnings, setPlanWarnings] = useState<string[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [issues, setIssues] = useState<FundAssistPlanIssue[] | null>(null);
  const [issuesPlan, setIssuesPlan] = useState<FundAssistPlan | undefined>(undefined);

  const canPreview = useMemo(() => prompt.trim().length >= 8 && stage === "idle", [prompt, stage]);
  const canCreate = useMemo(() => plan !== null && stage === "idle", [plan, stage]);

  const handlePreview = async () => {
    setStage("previewing");
    setError(null);
    setIssues(null);
    setIssuesPlan(undefined);
    try {
      const resp = await assistCreateFund(companyId, { prompt: prompt.trim(), dryRun: true });
      setPlan(resp.plan);
      setPlanWarnings(resp.warnings ?? []);
    } catch (err) {
      // 422 plan_rejected: show structured issues. Anything else
      // (502 LLM unusable, 503 not configured, network, 401, etc.)
      // → toast string.
      if (err instanceof FundAssistRejectedError) {
        setIssues(err.issues);
        setIssuesPlan(err.plan);
        setPlan(null);
      } else {
        setError(formatApiError(err, "AI 辅助创建失败"));
        setPlan(null);
      }
    } finally {
      setStage("idle");
    }
  };

  const handleConfirmCreate = async () => {
    if (!plan) return;
    setStage("creating");
    setError(null);
    try {
      // We re-send the same prompt rather than letting the user
      // edit the dryRun plan in-place — the plan is the result of
      // the LLM, not the source of truth. Two LLM calls is fine
      // here because the second one runs through the same prompt
      // cache (turned on for the multiprovider client) and is
      // typically a cache hit.
      const resp = await assistCreateFund(companyId, { prompt: prompt.trim(), dryRun: false });
      onCreated(resp);
    } catch (err) {
      if (err instanceof FundAssistRejectedError) {
        // Edge case: prompt produced a valid plan on dryRun then a
        // different invalid one on commit (LLM nondeterminism).
        // Roll back to issues view so the user can retry.
        setIssues(err.issues);
        setIssuesPlan(err.plan);
        setPlan(null);
      } else {
        setError(formatApiError(err, "AI 创建基金失败"));
      }
    } finally {
      setStage("idle");
    }
  };

  const handleClose = () => {
    if (stage !== "idle") return;
    setPrompt("");
    setPlan(null);
    setPlanWarnings([]);
    setError(null);
    setIssues(null);
    setIssuesPlan(undefined);
    onClose();
  };

  if (!open) return null;

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4">
      <div className="max-h-[90vh] w-full max-w-2xl overflow-y-auto rounded-2xl bg-white p-6 shadow-2xl">
        <div className="mb-4 flex items-start justify-between">
          <div>
            <h2 className="text-lg font-semibold text-slate-900">AI 辅助创建基金</h2>
            <p className="text-xs text-slate-500">
              在 <span className="font-medium">{companyName}</span> 下，描述你想做的基金和团队，AI 会帮你完成配置。
            </p>
          </div>
          <button
            type="button"
            onClick={handleClose}
            disabled={stage !== "idle"}
            className="rounded-md px-2 py-1 text-sm text-slate-400 hover:bg-slate-100 hover:text-slate-700 disabled:opacity-50"
            aria-label="关闭"
          >
            ✕
          </button>
        </div>

        <label className="block">
          <span className="text-sm font-medium text-slate-700">基金 + 团队描述</span>
          <textarea
            className="mt-1 block w-full rounded-lg border border-slate-300 px-3 py-2 text-sm shadow-sm focus:border-indigo-500 focus:outline-none focus:ring-1 focus:ring-indigo-500"
            rows={6}
            placeholder="例：做一个美股 AI 基金，初始 100 万美元，关注 NVDA / AMD / AVGO。团队 1 PM、2 个研究员（一个 NVDA、一个 AMD+AVGO）、1 个交易员。"
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            disabled={stage !== "idle"}
            aria-label="基金描述"
          />
        </label>

        <div className="mt-2 flex flex-wrap items-center gap-2">
          <span className="text-xs text-slate-500">示例：</span>
          {SAMPLE_PROMPTS.map((s) => (
            <button
              key={s.label}
              type="button"
              onClick={() => setPrompt(s.prompt)}
              disabled={stage !== "idle"}
              className="rounded-full border border-slate-300 px-2 py-0.5 text-xs text-slate-600 hover:bg-slate-100 disabled:opacity-50"
            >
              {s.label}
            </button>
          ))}
        </div>

        <div className="mt-4 flex items-center gap-2">
          <button
            type="button"
            onClick={handlePreview}
            disabled={!canPreview}
            className="rounded-lg bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-50"
          >
            {stage === "previewing" ? "AI 思考中..." : plan ? "重新生成方案" : "生成方案 (preview)"}
          </button>
          {plan ? (
            <button
              type="button"
              onClick={handleConfirmCreate}
              disabled={!canCreate}
              className="rounded-lg bg-emerald-600 px-4 py-2 text-sm font-medium text-white hover:bg-emerald-700 disabled:cursor-not-allowed disabled:opacity-50"
            >
              {stage === "creating" ? "创建中..." : "确认创建基金 + 团队"}
            </button>
          ) : null}
        </div>

        {error ? (
          <div className="mt-4 rounded-lg border border-rose-300 bg-rose-50 px-3 py-2 text-sm text-rose-800">{error}</div>
        ) : null}

        {issues ? (
          <div className="mt-4">
            <IssuesCard issues={issues} plan={issuesPlan} />
          </div>
        ) : null}

        {plan ? (
          <div className="mt-4">
            <PlanPreviewCard plan={plan} warnings={planWarnings} />
          </div>
        ) : null}

        <p className="mt-4 text-[11px] text-slate-400">
          说明：AI 生成的方案会在服务端再次校验市场 / 标的一致性（例如美股基金不允许 A 股研究员）。校验失败会列出原因，请按提示修改描述后重试。
        </p>
      </div>
    </div>
  );
}
