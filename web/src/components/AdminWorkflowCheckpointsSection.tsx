// AdminWorkflowCheckpointsSection — S9.2 per-step workflow
// snapshot + resume Admin view.
//
// Operator picks a run_id (the common case — paste from logs /
// SSE stream) OR a (fund_id + trading_date) pair to see the full
// timeline. Failed / paused rows expose a "re-fire this step"
// button; a top-level "resume from latest failure" button is the
// fast path for the common "the run died at PM, kick it" UX.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  formatApiError,
  listWorkflowCheckpoints,
  resumeWorkflowCheckpoint,
  type WorkflowCheckpoint,
  type WorkflowCheckpointStatus,
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
  runFilter: string;
  runFilterPlaceholder: string;
  fundFilter: string;
  fundFilterPlaceholder: string;
  dateFilter: string;
  colStep: string;
  colStatus: string;
  colAttempts: string;
  colDuration: string;
  colEnded: string;
  colError: string;
  resumeFromLatest: string;
  resumeFromHere: string;
  resumeNoFailed: string;
  resumeRunning: string;
  resumeSuccessTemplate: string; // {step} {status}
  resumeError: string;
}

const messages: Record<Language, Messages> = {
  "zh-CN": {
    title: "工作流节点快照",
    subtitle:
      "每日工作流每跑完一个节点都会写一条 checkpoint，包含步骤名 / 状态 / 耗时 / 报错原文 / 重试次数。可按 run_id 或（基金 + 交易日）筛选；对失败 / 暂停的步骤可一键重跑，整条 run 不必从头来过。",
    refresh: "刷新",
    loading: "加载中…",
    empty: "暂无 checkpoint 记录",
    error: "加载失败",
    runFilter: "Run ID",
    runFilterPlaceholder: "粘贴 workflow run_id",
    fundFilter: "基金 ID",
    fundFilterPlaceholder: "可选",
    dateFilter: "交易日",
    colStep: "步骤",
    colStatus: "状态",
    colAttempts: "尝试次数",
    colDuration: "耗时",
    colEnded: "结束时间",
    colError: "错误",
    resumeFromLatest: "从最近失败步骤恢复",
    resumeFromHere: "从本步骤重跑",
    resumeNoFailed: "所有步骤均已成功，无需恢复",
    resumeRunning: "触发中…",
    resumeSuccessTemplate: "已触发：{step} → {status}",
    resumeError: "触发失败",
  },
  "en-US": {
    title: "Workflow node checkpoints",
    subtitle:
      "Every step of the daily workflow writes a checkpoint row with status, duration, retry count and the raw error text. Filter by run_id or (fund + trading date); failed / paused steps can be re-fired in place without restarting the whole run.",
    refresh: "Refresh",
    loading: "Loading…",
    empty: "No checkpoints recorded yet",
    error: "Failed to load",
    runFilter: "Run ID",
    runFilterPlaceholder: "Paste workflow run_id",
    fundFilter: "Fund ID",
    fundFilterPlaceholder: "Optional",
    dateFilter: "Trading date",
    colStep: "Step",
    colStatus: "Status",
    colAttempts: "Attempts",
    colDuration: "Duration",
    colEnded: "Ended",
    colError: "Error",
    resumeFromLatest: "Resume from latest failure",
    resumeFromHere: "Re-fire this step",
    resumeNoFailed: "All steps succeeded — nothing to resume",
    resumeRunning: "Triggering…",
    resumeSuccessTemplate: "Triggered: {step} → {status}",
    resumeError: "Trigger failed",
  },
};

const STATUS_BADGES: Record<WorkflowCheckpointStatus, string> = {
  success: "bg-emerald-500/20 text-emerald-300 border-emerald-500/40",
  failed: "bg-rose-500/25 text-rose-200 border-rose-500/50",
  skipped: "bg-zinc-500/20 text-zinc-300 border-zinc-500/40",
  pending: "bg-amber-500/20 text-amber-200 border-amber-500/40",
  paused: "bg-sky-500/20 text-sky-200 border-sky-500/40",
};

interface ResumeFeedback {
  kind: "success" | "error" | "info";
  text: string;
}

export function AdminWorkflowCheckpointsSection({ language }: Props) {
  const t = messages[language];

  const [runIdInput, setRunIdInput] = useState("");
  const [fundIdInput, setFundIdInput] = useState("");
  const [dateInput, setDateInput] = useState("");
  const [checkpoints, setCheckpoints] = useState<WorkflowCheckpoint[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [resuming, setResuming] = useState(false);
  const [feedback, setFeedback] = useState<ResumeFeedback | null>(null);

  const fetchData = useCallback(async () => {
    if (!runIdInput && (!fundIdInput || !dateInput)) {
      setCheckpoints([]);
      return;
    }
    setLoading(true);
    setError("");
    try {
      const resp = await listWorkflowCheckpoints({
        runId: runIdInput || undefined,
        fundId: fundIdInput || undefined,
        tradingDate: dateInput || undefined,
      });
      setCheckpoints(resp.checkpoints ?? []);
    } catch (err) {
      setError(formatApiError(err, t.error));
      setCheckpoints([]);
    } finally {
      setLoading(false);
    }
  }, [runIdInput, fundIdInput, dateInput, t.error]);

  useEffect(() => {
    void fetchData();
  }, [fetchData]);

  const latestFailedRunId = useMemo(() => {
    for (let i = checkpoints.length - 1; i >= 0; i -= 1) {
      const cp = checkpoints[i];
      if (cp.status === "failed" || cp.status === "paused") {
        return cp.run_id;
      }
    }
    return "";
  }, [checkpoints]);

  const handleResume = useCallback(
    async (runId: string, step?: string) => {
      if (!runId) return;
      setResuming(true);
      setFeedback(null);
      try {
        const resp = await resumeWorkflowCheckpoint({ run_id: runId, step });
        setFeedback({
          kind: "success",
          text: t.resumeSuccessTemplate
            .replace("{step}", resp.step)
            .replace("{status}", resp.status),
        });
        await fetchData();
      } catch (err) {
        const msg = formatApiError(err, t.resumeError);
        if (msg.includes("no_failed_step")) {
          setFeedback({ kind: "info", text: t.resumeNoFailed });
        } else {
          setFeedback({ kind: "error", text: `${t.resumeError}: ${msg}` });
        }
      } finally {
        setResuming(false);
      }
    },
    [fetchData, t.resumeError, t.resumeNoFailed, t.resumeSuccessTemplate],
  );

  return (
    <section className="rounded-2xl border border-zinc-700/70 bg-zinc-900/50 p-6 shadow-lg">
      <header className="mb-4">
        <h2 className="text-xl font-semibold text-zinc-100">{t.title}</h2>
        <p className="mt-1 text-sm leading-relaxed text-zinc-400">{t.subtitle}</p>
      </header>

      <div className="grid grid-cols-1 gap-3 md:grid-cols-3">
        <label className="flex flex-col gap-1 text-sm text-zinc-300">
          <span>{t.runFilter}</span>
          <input
            type="text"
            value={runIdInput}
            onChange={(e) => setRunIdInput(e.target.value.trim())}
            placeholder={t.runFilterPlaceholder}
            className="rounded-md border border-zinc-700 bg-zinc-800 px-3 py-1.5 text-sm text-zinc-100 placeholder:text-zinc-500"
          />
        </label>
        <label className="flex flex-col gap-1 text-sm text-zinc-300">
          <span>{t.fundFilter}</span>
          <input
            type="text"
            value={fundIdInput}
            onChange={(e) => setFundIdInput(e.target.value.trim())}
            placeholder={t.fundFilterPlaceholder}
            className="rounded-md border border-zinc-700 bg-zinc-800 px-3 py-1.5 text-sm text-zinc-100 placeholder:text-zinc-500"
          />
        </label>
        <label className="flex flex-col gap-1 text-sm text-zinc-300">
          <span>{t.dateFilter}</span>
          <input
            type="date"
            value={dateInput}
            onChange={(e) => setDateInput(e.target.value)}
            className="rounded-md border border-zinc-700 bg-zinc-800 px-3 py-1.5 text-sm text-zinc-100"
          />
        </label>
      </div>

      <div className="mt-4 flex flex-wrap items-center gap-2">
        <button
          type="button"
          onClick={() => void fetchData()}
          disabled={loading}
          className="rounded-md border border-zinc-700 bg-zinc-800 px-3 py-1.5 text-sm text-zinc-100 hover:bg-zinc-700 disabled:opacity-50"
        >
          {loading ? t.loading : t.refresh}
        </button>
        <button
          type="button"
          onClick={() => void handleResume(latestFailedRunId)}
          disabled={resuming || !latestFailedRunId}
          className="rounded-md border border-sky-500 bg-sky-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-sky-500 disabled:cursor-not-allowed disabled:opacity-50"
        >
          {resuming ? t.resumeRunning : t.resumeFromLatest}
        </button>
        {feedback && (
          <span
            className={
              feedback.kind === "success"
                ? "text-sm text-emerald-300"
                : feedback.kind === "info"
                  ? "text-sm text-zinc-300"
                  : "text-sm text-rose-300"
            }
          >
            {feedback.text}
          </span>
        )}
      </div>

      {error && <p className="mt-3 text-sm text-rose-300">{`${t.error}: ${error}`}</p>}

      <div className="mt-4 overflow-x-auto">
        <table className="min-w-full text-left text-sm">
          <thead className="border-b border-zinc-700 text-zinc-400">
            <tr>
              <th className="px-3 py-2">{t.colStep}</th>
              <th className="px-3 py-2">{t.colStatus}</th>
              <th className="px-3 py-2">{t.colAttempts}</th>
              <th className="px-3 py-2">{t.colDuration}</th>
              <th className="px-3 py-2">{t.colEnded}</th>
              <th className="px-3 py-2">{t.colError}</th>
              <th className="px-3 py-2"></th>
            </tr>
          </thead>
          <tbody className="text-zinc-200">
            {checkpoints.length === 0 ? (
              <tr>
                <td colSpan={7} className="px-3 py-4 text-center text-zinc-500">
                  {loading ? t.loading : t.empty}
                </td>
              </tr>
            ) : (
              checkpoints.map((cp) => (
                <tr key={cp.id} className="border-b border-zinc-800/60">
                  <td className="px-3 py-2 font-mono text-zinc-200">{cp.step}</td>
                  <td className="px-3 py-2">
                    <span
                      className={`rounded-full border px-2 py-0.5 text-xs ${STATUS_BADGES[cp.status]}`}
                    >
                      {cp.status}
                    </span>
                  </td>
                  <td className="px-3 py-2 text-zinc-400">{cp.attempts}</td>
                  <td className="px-3 py-2 text-zinc-400">{formatDuration(cp.duration_ms)}</td>
                  <td className="px-3 py-2 text-zinc-400">{formatRelative(cp.ended_at)}</td>
                  <td className="px-3 py-2 text-rose-300">
                    {cp.error_text ? (
                      <span title={cp.error_text} className="line-clamp-1">
                        {truncate(cp.error_text, 80)}
                      </span>
                    ) : (
                      "—"
                    )}
                  </td>
                  <td className="px-3 py-2 text-right">
                    {(cp.status === "failed" || cp.status === "paused") && (
                      <button
                        type="button"
                        onClick={() => void handleResume(cp.run_id, cp.step)}
                        disabled={resuming}
                        className="rounded-md border border-amber-500 bg-amber-600/20 px-2 py-1 text-xs text-amber-100 hover:bg-amber-600/40 disabled:opacity-50"
                      >
                        {t.resumeFromHere}
                      </button>
                    )}
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </section>
  );
}

function formatDuration(ms: number): string {
  if (!ms || ms <= 0) return "—";
  if (ms < 1000) return `${ms}ms`;
  const seconds = ms / 1000;
  if (seconds < 60) return `${seconds.toFixed(1)}s`;
  const minutes = seconds / 60;
  return `${minutes.toFixed(1)}m`;
}

function formatRelative(iso: string): string {
  try {
    const d = new Date(iso);
    return d.toLocaleString();
  } catch {
    return iso;
  }
}

function truncate(s: string, n: number): string {
  if (s.length <= n) return s;
  return `${s.slice(0, n - 1)}…`;
}
