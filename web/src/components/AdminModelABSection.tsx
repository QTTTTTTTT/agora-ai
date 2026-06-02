// AdminModelABSection — S10.3 / S10.4 model A/B experiment admin UI.
//
// Single page combining:
//   - list / status filter
//   - per-experiment report (per-arm metrics)
//   - create draft experiment + flip status (pause / resume / archive)
//
// The page is intentionally text-heavy (no charts) so operators can
// see the raw numbers; the back-end ships only aggregates so a basic
// HTML table is sufficient for the v1 report layout.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  createModelABExperiment,
  formatApiError,
  getModelABReport,
  listModelABExperiments,
  setModelABExperimentStatus,
  type ModelABArm,
  type ModelABArmMetric,
  type ModelABExperiment,
  type ModelABExperimentStatus,
  type ModelABReport,
  type ModelABScope,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

interface Props {
  language: Language;
}

interface Messages {
  title: string;
  subtitle: string;
  refresh: string;
  loading: string;
  empty: string;
  error: string;
  filterStatus: string;
  colName: string;
  colScope: string;
  colArms: string;
  colTraffic: string;
  colStatus: string;
  colCreated: string;
  reportTitle: string;
  reportEmpty: string;
  reportArmHeader: string;
  reportColPrimary: string;
  reportColShadow: string;
  reportColErrors: string;
  reportColLatency: string;
  reportColTokens: string;
  reportColCost: string;
  createTitle: string;
  createHint: string;
  createName: string;
  createScope: string;
  createScopeTarget: string;
  createMaxTokens: string;
  createMaxTokensHint: string;
  createArmsLabel: string;
  createArmsHint: string;
  createTrafficSplit: string;
  createTrafficHint: string;
  createSubmit: string;
  createSubmitting: string;
  createStartImmediate: string;
  statusDraft: string;
  statusRunning: string;
  statusPaused: string;
  statusCompleted: string;
  statusArchived: string;
  flipPause: string;
  flipResume: string;
  flipComplete: string;
  flipArchive: string;
  flipSuccess: string;
  flipError: string;
}

const messages: Record<Language, Messages> = {
  "zh-CN": {
    title: "模型 A/B 实验",
    subtitle:
      "同一个团队、同一份 prompt，把 1 路调用拆成多路并行打到不同模型上，比较 PM/Trader/Risk 等各 agent 在不同模型上的决策一致率、延迟和成本。" +
      " 主路决策走赢家 arm 真实执行；其他 arm 只观察、写入 model_ab_shadow_responses。",
    refresh: "刷新",
    loading: "加载中…",
    empty: "暂无实验",
    error: "加载失败",
    filterStatus: "状态",
    colName: "名称",
    colScope: "作用范围",
    colArms: "Arm 数",
    colTraffic: "流量分配",
    colStatus: "状态",
    colCreated: "创建时间",
    reportTitle: "Arm 指标对比",
    reportEmpty: "选择一个实验查看 arm 维度的指标。",
    reportArmHeader: "Arm",
    reportColPrimary: "主路次数",
    reportColShadow: "影子次数",
    reportColErrors: "错误数",
    reportColLatency: "平均时延 (ms)",
    reportColTokens: "Output tokens",
    reportColCost: "成本 (¥)",
    createTitle: "新建实验",
    createHint:
      "至少 2 个 arm；arm 0 是控制组。" +
      "Provider 选 openai / claude / deepseek / qwen / gemini。" +
      "API Key 不在实验里配置——平台用 system key。",
    createName: "名称",
    createScope: "作用范围",
    createScopeTarget: "范围目标 (fund_id / role / agent_id)",
    createMaxTokens: "Output token 上限（0 = 不限）",
    createMaxTokensHint: "保护性硬上限，累计 output token 超过即停止抽样",
    createArmsLabel: "Arm 列表（JSON）",
    createArmsHint:
      '示例：[{"name":"ctrl","provider":"openai","model_name":"gpt-4o","model_tier":"critical"},{"name":"treat","provider":"claude","model_name":"claude-opus-4","model_tier":"critical"}]',
    createTrafficSplit: "流量分配（与 arms 对齐）",
    createTrafficHint: "示例：[0.5, 0.5]，必须求和为 ~1.0",
    createSubmit: "创建",
    createSubmitting: "创建中…",
    createStartImmediate: "创建后立即启动",
    statusDraft: "草稿",
    statusRunning: "运行中",
    statusPaused: "已暂停",
    statusCompleted: "已完成",
    statusArchived: "已归档",
    flipPause: "暂停",
    flipResume: "启动 / 恢复",
    flipComplete: "完成",
    flipArchive: "归档",
    flipSuccess: "状态已更新",
    flipError: "状态更新失败",
  },
  "en-US": {
    title: "Model A/B experiments",
    subtitle:
      "Same fund, same prompt — fan out one LLM call into N parallel calls to compare " +
      "decision agreement, latency, and cost per arm. The winning arm steers production; " +
      "the rest are observed and persisted to model_ab_shadow_responses.",
    refresh: "Refresh",
    loading: "Loading…",
    empty: "No experiments yet",
    error: "Failed to load",
    filterStatus: "Status",
    colName: "Name",
    colScope: "Scope",
    colArms: "Arms",
    colTraffic: "Split",
    colStatus: "Status",
    colCreated: "Created",
    reportTitle: "Per-arm metrics",
    reportEmpty: "Select an experiment to see arm metrics.",
    reportArmHeader: "Arm",
    reportColPrimary: "Primary calls",
    reportColShadow: "Shadow calls",
    reportColErrors: "Errors",
    reportColLatency: "Avg latency (ms)",
    reportColTokens: "Output tokens",
    reportColCost: "Cost (¥)",
    createTitle: "Create experiment",
    createHint:
      "At least 2 arms; arm 0 is control. Provider: openai / claude / deepseek / qwen / gemini. " +
      "API keys are NEVER stored here — the platform always uses its system keys.",
    createName: "Name",
    createScope: "Scope",
    createScopeTarget: "Scope target (fund_id / role / agent_id)",
    createMaxTokens: "Output token cap (0 = none)",
    createMaxTokensHint:
      "Safety cap — sampling stops once cumulative output tokens cross this.",
    createArmsLabel: "Arms (JSON)",
    createArmsHint:
      'Example: [{"name":"ctrl","provider":"openai","model_name":"gpt-4o","model_tier":"critical"},{"name":"treat","provider":"claude","model_name":"claude-opus-4","model_tier":"critical"}]',
    createTrafficSplit: "Traffic split (aligned with arms)",
    createTrafficHint: "Example: [0.5, 0.5] — must sum to ~1.0.",
    createSubmit: "Create",
    createSubmitting: "Creating…",
    createStartImmediate: "Start immediately after creation",
    statusDraft: "draft",
    statusRunning: "running",
    statusPaused: "paused",
    statusCompleted: "completed",
    statusArchived: "archived",
    flipPause: "Pause",
    flipResume: "Start / resume",
    flipComplete: "Complete",
    flipArchive: "Archive",
    flipSuccess: "Status updated",
    flipError: "Status update failed",
  },
};

const STATUS_BADGES: Record<ModelABExperimentStatus, string> = {
  draft: "bg-zinc-500/20 text-zinc-300 border-zinc-500/40",
  running: "bg-emerald-500/20 text-emerald-300 border-emerald-500/40",
  paused: "bg-amber-500/20 text-amber-200 border-amber-500/40",
  completed: "bg-sky-500/20 text-sky-200 border-sky-500/40",
  archived: "bg-zinc-700/30 text-zinc-400 border-zinc-600/40",
};

function statusLabel(status: ModelABExperimentStatus, t: Messages): string {
  switch (status) {
    case "draft":
      return t.statusDraft;
    case "running":
      return t.statusRunning;
    case "paused":
      return t.statusPaused;
    case "completed":
      return t.statusCompleted;
    case "archived":
      return t.statusArchived;
    default:
      return status;
  }
}

function formatCost(microYen: number): string {
  if (!microYen) return "0";
  return `${(microYen / 1_000_000).toFixed(4)}`;
}

export function AdminModelABSection({ language }: Props) {
  const t = messages[language];

  const [statusFilter, setStatusFilter] = useState<string>("");
  const [experiments, setExperiments] = useState<ModelABExperiment[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  const [selectedID, setSelectedID] = useState("");
  const [report, setReport] = useState<ModelABReport | null>(null);
  const [reportError, setReportError] = useState("");
  const [reportLoading, setReportLoading] = useState(false);

  const [flipBusy, setFlipBusy] = useState(false);
  const [flipFeedback, setFlipFeedback] = useState<string>("");

  // Create form state — strings so the textarea can be edited freely;
  // JSON.parse happens on submit.
  const [formName, setFormName] = useState("");
  const [formScope, setFormScope] = useState<ModelABScope>("global");
  const [formScopeTarget, setFormScopeTarget] = useState("");
  const [formArms, setFormArms] = useState(
    '[\n  {"name":"ctrl","provider":"openai","model_name":"gpt-4o","model_tier":"critical"},\n  {"name":"treat","provider":"claude","model_name":"claude-opus-4","model_tier":"critical"}\n]',
  );
  const [formTraffic, setFormTraffic] = useState("[0.5, 0.5]");
  const [formMaxTokens, setFormMaxTokens] = useState("0");
  const [formStartImmediate, setFormStartImmediate] = useState(false);
  const [formBusy, setFormBusy] = useState(false);
  const [formError, setFormError] = useState("");
  const [formSuccess, setFormSuccess] = useState("");

  const fetchList = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const resp = await listModelABExperiments({ status: statusFilter || undefined });
      setExperiments(resp.experiments ?? []);
    } catch (err) {
      setError(formatApiError(err, t.error));
      setExperiments([]);
    } finally {
      setLoading(false);
    }
  }, [statusFilter, t.error]);

  useEffect(() => {
    void fetchList();
  }, [fetchList]);

  const fetchReport = useCallback(
    async (id: string) => {
      if (!id) {
        setReport(null);
        return;
      }
      setReportLoading(true);
      setReportError("");
      try {
        const resp = await getModelABReport(id);
        setReport(resp);
      } catch (err) {
        setReportError(formatApiError(err, t.error));
        setReport(null);
      } finally {
        setReportLoading(false);
      }
    },
    [t.error],
  );

  useEffect(() => {
    void fetchReport(selectedID);
  }, [selectedID, fetchReport]);

  const selectedExperiment = useMemo(
    () => experiments.find((e) => e.id === selectedID) ?? null,
    [experiments, selectedID],
  );

  const handleFlip = useCallback(
    async (status: ModelABExperimentStatus) => {
      if (!selectedID) return;
      setFlipBusy(true);
      setFlipFeedback("");
      try {
        await setModelABExperimentStatus(selectedID, { status });
        setFlipFeedback(t.flipSuccess);
        await fetchList();
      } catch (err) {
        setFlipFeedback(`${t.flipError}: ${formatApiError(err, t.flipError)}`);
      } finally {
        setFlipBusy(false);
      }
    },
    [selectedID, fetchList, t.flipError, t.flipSuccess],
  );

  const handleSubmitCreate = useCallback(async () => {
    setFormBusy(true);
    setFormError("");
    setFormSuccess("");
    try {
      let arms: ModelABArm[];
      try {
        arms = JSON.parse(formArms) as ModelABArm[];
      } catch (parseErr) {
        throw new Error(`arms JSON: ${(parseErr as Error).message}`);
      }
      let traffic: number[];
      try {
        traffic = JSON.parse(formTraffic) as number[];
      } catch (parseErr) {
        throw new Error(`traffic_split JSON: ${(parseErr as Error).message}`);
      }
      const maxTokens = Number.parseInt(formMaxTokens || "0", 10);
      const created = await createModelABExperiment({
        name: formName,
        scope: formScope,
        scope_target: formScopeTarget || undefined,
        arms,
        traffic_split: traffic,
        max_total_tokens: Number.isFinite(maxTokens) && maxTokens > 0 ? maxTokens : undefined,
        start_immediate: formStartImmediate,
      });
      setFormSuccess(`OK · id=${created.id}`);
      setFormName("");
      await fetchList();
    } catch (err) {
      setFormError(formatApiError(err, t.error));
    } finally {
      setFormBusy(false);
    }
  }, [
    formName,
    formScope,
    formScopeTarget,
    formArms,
    formTraffic,
    formMaxTokens,
    formStartImmediate,
    fetchList,
    t.error,
  ]);

  return (
    <section className="space-y-6">
      <div className="rounded-2xl border border-zinc-700/70 bg-zinc-900/50 p-6 shadow-lg">
        <header className="mb-4">
          <h2 className="text-xl font-semibold text-zinc-100">{t.title}</h2>
          <p className="mt-1 text-sm leading-relaxed text-zinc-400">{t.subtitle}</p>
        </header>

        <div className="flex flex-wrap items-end gap-3">
          <label className="flex flex-col gap-1 text-sm text-zinc-300">
            <span>{t.filterStatus}</span>
            <select
              value={statusFilter}
              onChange={(e) => setStatusFilter(e.target.value)}
              className="rounded-md border border-zinc-700 bg-zinc-800 px-3 py-1.5 text-sm text-zinc-100"
            >
              <option value="">{language === "zh-CN" ? "全部" : "All"}</option>
              <option value="draft">{t.statusDraft}</option>
              <option value="running">{t.statusRunning}</option>
              <option value="paused">{t.statusPaused}</option>
              <option value="completed">{t.statusCompleted}</option>
              <option value="archived">{t.statusArchived}</option>
            </select>
          </label>
          <button
            type="button"
            onClick={() => void fetchList()}
            disabled={loading}
            className="rounded-md border border-zinc-700 bg-zinc-800 px-3 py-1.5 text-sm text-zinc-100 hover:bg-zinc-700 disabled:opacity-50"
          >
            {loading ? t.loading : t.refresh}
          </button>
        </div>

        {error && <p className="mt-3 text-sm text-rose-300">{`${t.error}: ${error}`}</p>}

        <div className="mt-4 overflow-x-auto">
          <table className="min-w-full text-left text-sm">
            <thead className="border-b border-zinc-700 text-zinc-400">
              <tr>
                <th className="px-3 py-2">{t.colName}</th>
                <th className="px-3 py-2">{t.colScope}</th>
                <th className="px-3 py-2">{t.colArms}</th>
                <th className="px-3 py-2">{t.colTraffic}</th>
                <th className="px-3 py-2">{t.colStatus}</th>
                <th className="px-3 py-2">{t.colCreated}</th>
              </tr>
            </thead>
            <tbody>
              {experiments.length === 0 && !loading ? (
                <tr>
                  <td colSpan={6} className="px-3 py-6 text-center text-zinc-500">
                    {t.empty}
                  </td>
                </tr>
              ) : (
                experiments.map((e) => {
                  const selected = e.id === selectedID;
                  return (
                    <tr
                      key={e.id}
                      onClick={() => setSelectedID(e.id)}
                      className={`cursor-pointer border-b border-zinc-800 ${
                        selected ? "bg-zinc-800/60" : "hover:bg-zinc-800/30"
                      }`}
                    >
                      <td className="px-3 py-2 text-zinc-100">{e.name}</td>
                      <td className="px-3 py-2 text-zinc-300">
                        {e.scope}
                        {e.scope_target ? ` · ${e.scope_target}` : ""}
                      </td>
                      <td className="px-3 py-2 text-zinc-300">{e.arms.length}</td>
                      <td className="px-3 py-2 text-zinc-300">
                        {e.traffic_split.map((p) => p.toFixed(2)).join("/")}
                      </td>
                      <td className="px-3 py-2">
                        <span
                          className={`rounded border px-2 py-0.5 text-xs ${
                            STATUS_BADGES[e.status as ModelABExperimentStatus]
                          }`}
                        >
                          {statusLabel(e.status as ModelABExperimentStatus, t)}
                        </span>
                      </td>
                      <td className="px-3 py-2 text-zinc-400">{e.created_at?.slice(0, 19)}</td>
                    </tr>
                  );
                })
              )}
            </tbody>
          </table>
        </div>
      </div>

      <div className="rounded-2xl border border-zinc-700/70 bg-zinc-900/50 p-6 shadow-lg">
        <header className="mb-4 flex flex-wrap items-center justify-between gap-3">
          <h3 className="text-lg font-semibold text-zinc-100">{t.reportTitle}</h3>
          {selectedExperiment && (
            <div className="flex flex-wrap items-center gap-2 text-sm">
              <span className="text-zinc-400">{selectedExperiment.name}</span>
              <button
                type="button"
                disabled={flipBusy}
                onClick={() => void handleFlip("running")}
                className="rounded-md border border-emerald-500 bg-emerald-600 px-3 py-1 text-xs font-medium text-white hover:bg-emerald-500 disabled:opacity-50"
              >
                {t.flipResume}
              </button>
              <button
                type="button"
                disabled={flipBusy}
                onClick={() => void handleFlip("paused")}
                className="rounded-md border border-amber-500 bg-amber-600 px-3 py-1 text-xs font-medium text-white hover:bg-amber-500 disabled:opacity-50"
              >
                {t.flipPause}
              </button>
              <button
                type="button"
                disabled={flipBusy}
                onClick={() => void handleFlip("completed")}
                className="rounded-md border border-sky-500 bg-sky-600 px-3 py-1 text-xs font-medium text-white hover:bg-sky-500 disabled:opacity-50"
              >
                {t.flipComplete}
              </button>
              <button
                type="button"
                disabled={flipBusy}
                onClick={() => void handleFlip("archived")}
                className="rounded-md border border-zinc-500 bg-zinc-600 px-3 py-1 text-xs font-medium text-white hover:bg-zinc-500 disabled:opacity-50"
              >
                {t.flipArchive}
              </button>
              {flipFeedback && <span className="text-xs text-zinc-300">{flipFeedback}</span>}
            </div>
          )}
        </header>
        {!selectedID ? (
          <p className="text-sm text-zinc-400">{t.reportEmpty}</p>
        ) : reportLoading ? (
          <p className="text-sm text-zinc-400">{t.loading}</p>
        ) : reportError ? (
          <p className="text-sm text-rose-300">{reportError}</p>
        ) : !report || report.arms.length === 0 ? (
          <p className="text-sm text-zinc-500">{t.empty}</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="min-w-full text-left text-sm">
              <thead className="border-b border-zinc-700 text-zinc-400">
                <tr>
                  <th className="px-3 py-2">{t.reportArmHeader}</th>
                  <th className="px-3 py-2">{t.reportColPrimary}</th>
                  <th className="px-3 py-2">{t.reportColShadow}</th>
                  <th className="px-3 py-2">{t.reportColErrors}</th>
                  <th className="px-3 py-2">{t.reportColLatency}</th>
                  <th className="px-3 py-2">{t.reportColTokens}</th>
                  <th className="px-3 py-2">{t.reportColCost}</th>
                </tr>
              </thead>
              <tbody>
                {report.arms.map((a: ModelABArmMetric) => (
                  <tr key={a.arm_index} className="border-b border-zinc-800">
                    <td className="px-3 py-2 text-zinc-100">
                      {a.arm_name || `#${a.arm_index}`}
                      <span className="ml-2 text-xs text-zinc-500">{a.arm_label}</span>
                    </td>
                    <td className="px-3 py-2 text-zinc-300">{a.primary_count}</td>
                    <td className="px-3 py-2 text-zinc-300">{a.shadow_count}</td>
                    <td className="px-3 py-2 text-zinc-300">{a.error_count}</td>
                    <td className="px-3 py-2 text-zinc-300">{a.avg_latency_ms}</td>
                    <td className="px-3 py-2 text-zinc-300">{a.total_output_tokens}</td>
                    <td className="px-3 py-2 text-zinc-300">{formatCost(a.total_cost_micro)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <div className="rounded-2xl border border-zinc-700/70 bg-zinc-900/50 p-6 shadow-lg">
        <header className="mb-4">
          <h3 className="text-lg font-semibold text-zinc-100">{t.createTitle}</h3>
          <p className="mt-1 text-sm text-zinc-400">{t.createHint}</p>
        </header>
        <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
          <label className="flex flex-col gap-1 text-sm text-zinc-300">
            <span>{t.createName}</span>
            <input
              type="text"
              value={formName}
              onChange={(e) => setFormName(e.target.value)}
              className="rounded-md border border-zinc-700 bg-zinc-800 px-3 py-1.5 text-sm text-zinc-100"
            />
          </label>
          <label className="flex flex-col gap-1 text-sm text-zinc-300">
            <span>{t.createScope}</span>
            <select
              value={formScope}
              onChange={(e) => setFormScope(e.target.value as ModelABScope)}
              className="rounded-md border border-zinc-700 bg-zinc-800 px-3 py-1.5 text-sm text-zinc-100"
            >
              <option value="global">global</option>
              <option value="fund">fund</option>
              <option value="agent_role">agent_role</option>
              <option value="agent_id">agent_id</option>
            </select>
          </label>
          {formScope !== "global" && (
            <label className="flex flex-col gap-1 text-sm text-zinc-300 md:col-span-2">
              <span>{t.createScopeTarget}</span>
              <input
                type="text"
                value={formScopeTarget}
                onChange={(e) => setFormScopeTarget(e.target.value.trim())}
                className="rounded-md border border-zinc-700 bg-zinc-800 px-3 py-1.5 text-sm text-zinc-100"
              />
            </label>
          )}
          <label className="flex flex-col gap-1 text-sm text-zinc-300">
            <span>{t.createMaxTokens}</span>
            <input
              type="number"
              value={formMaxTokens}
              onChange={(e) => setFormMaxTokens(e.target.value)}
              className="rounded-md border border-zinc-700 bg-zinc-800 px-3 py-1.5 text-sm text-zinc-100"
            />
            <span className="text-xs text-zinc-500">{t.createMaxTokensHint}</span>
          </label>
          <label className="md:col-span-2 flex flex-col gap-1 text-sm text-zinc-300">
            <span>{t.createArmsLabel}</span>
            <textarea
              value={formArms}
              onChange={(e) => setFormArms(e.target.value)}
              rows={6}
              className="rounded-md border border-zinc-700 bg-zinc-800 px-3 py-1.5 font-mono text-xs text-zinc-100"
            />
            <span className="text-xs text-zinc-500">{t.createArmsHint}</span>
          </label>
          <label className="md:col-span-2 flex flex-col gap-1 text-sm text-zinc-300">
            <span>{t.createTrafficSplit}</span>
            <input
              type="text"
              value={formTraffic}
              onChange={(e) => setFormTraffic(e.target.value)}
              className="rounded-md border border-zinc-700 bg-zinc-800 px-3 py-1.5 font-mono text-sm text-zinc-100"
            />
            <span className="text-xs text-zinc-500">{t.createTrafficHint}</span>
          </label>
          <label className="md:col-span-2 flex items-center gap-2 text-sm text-zinc-300">
            <input
              type="checkbox"
              checked={formStartImmediate}
              onChange={(e) => setFormStartImmediate(e.target.checked)}
            />
            <span>{t.createStartImmediate}</span>
          </label>
        </div>
        <div className="mt-4 flex items-center gap-3">
          <button
            type="button"
            disabled={formBusy || !formName}
            onClick={() => void handleSubmitCreate()}
            className="rounded-md border border-sky-500 bg-sky-600 px-4 py-1.5 text-sm font-medium text-white hover:bg-sky-500 disabled:cursor-not-allowed disabled:opacity-50"
          >
            {formBusy ? t.createSubmitting : t.createSubmit}
          </button>
          {formError && <span className="text-sm text-rose-300">{formError}</span>}
          {formSuccess && <span className="text-sm text-emerald-300">{formSuccess}</span>}
        </div>
      </div>
    </section>
  );
}
