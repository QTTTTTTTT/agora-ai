// FundWorkflow — read-only per-step workflow checkpoint timeline
// for the fund's owner. Mirrors the admin view at
// /admin/workflow-checkpoints minus the Run-ID filter and the
// Resume buttons, because:
//
//   - Run-IDs only appear in operator logs / SSE feeds; a fund
//     owner wouldn't have one to paste. The (fund + trading-date)
//     pair already uniquely identifies a daily run from the user's
//     mental model ("show me how today's report run went").
//   - Re-firing a step can re-spend LLM budget and submit broker
//     instructions — that decision belongs with platform operators
//     via the admin endpoint. Owners hit a support flow if they
//     need a re-run, and support's audit trail captures the
//     authorisation.
//
// The page intentionally stays simple: one date input, a refresh
// button, and a single timeline table. All hard error paths flow
// through the global toast queue (lib/toast.tsx) — no inline error
// banners — so the user gets the same notification shape as
// everywhere else in the app.

import { useCallback, useEffect, useMemo, useState } from "react";
import { useParams } from "react-router-dom";

import {
  formatApiError,
  listFundWorkflowCheckpoints,
  type WorkflowCheckpoint,
  type WorkflowCheckpointStatus,
} from "../lib/api";
import { useAppPreferences } from "../lib/preferences";
import { toast } from "../lib/toast";

type Language = "zh-CN" | "en-US";

interface Messages {
  title: string;
  subtitle: string;
  dateFilter: string;
  refresh: string;
  loading: string;
  empty: string;
  colStep: string;
  colStatus: string;
  colAttempts: string;
  colDuration: string;
  colEnded: string;
  colError: string;
  needResumeHint: string;
  errorTitle: string;
  todayLabel: string;
}

const messages: Record<Language, Messages> = {
  "zh-CN": {
    title: "工作流执行情况",
    subtitle:
      "本基金每个交易日的报告 / 决策 / 复盘工作流由平台后台自动跑。下表展示该交易日每个步骤的执行结果与报错原文，便于自查今日工作流是否健康。如需重跑某步，请联系运营。",
    dateFilter: "交易日",
    refresh: "刷新",
    loading: "加载中…",
    empty: "暂无 checkpoint 记录",
    colStep: "步骤",
    colStatus: "状态",
    colAttempts: "尝试次数",
    colDuration: "耗时",
    colEnded: "结束时间",
    colError: "错误",
    needResumeHint: "如某步骤失败需要重跑，请通过运营渠道反馈，由平台 admin 触发。",
    errorTitle: "加载失败",
    todayLabel: "今日",
  },
  "en-US": {
    title: "Workflow status",
    subtitle:
      "Each trading day, the platform runs your fund's report / decision / review workflow in the background. The table below lists every step's outcome and error text for the selected date, so you can self-check whether today's run completed cleanly. To re-fire a failed step, contact platform support.",
    dateFilter: "Trading date",
    refresh: "Refresh",
    loading: "Loading…",
    empty: "No checkpoints recorded yet",
    colStep: "Step",
    colStatus: "Status",
    colAttempts: "Attempts",
    colDuration: "Duration",
    colEnded: "Ended",
    colError: "Error",
    needResumeHint: "If a step needs to be re-fired, raise a support ticket — a platform admin will trigger the resume so the audit trail stays intact.",
    errorTitle: "Failed to load",
    todayLabel: "Today",
  },
};

const STATUS_BADGES: Record<WorkflowCheckpointStatus, string> = {
  success: "bg-emerald-100 text-emerald-800 border-emerald-300",
  failed: "bg-rose-100 text-rose-800 border-rose-300",
  skipped: "bg-zinc-100 text-zinc-700 border-zinc-300",
  pending: "bg-amber-100 text-amber-800 border-amber-300",
  paused: "bg-sky-100 text-sky-800 border-sky-300",
};

function todayISO(): string {
  // Local YYYY-MM-DD so the default matches what the user sees on
  // their wall clock, not UTC. The backend parses with
  // time.Parse("2006-01-02") which is timezone-naive — pairing
  // local-formatted dates with the local trading day produces the
  // expected match against rows the workflow wrote earlier today.
  const d = new Date();
  const y = d.getFullYear();
  const m = `${d.getMonth() + 1}`.padStart(2, "0");
  const dd = `${d.getDate()}`.padStart(2, "0");
  return `${y}-${m}-${dd}`;
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

export default function FundWorkflow(): JSX.Element {
  const { fundId = "" } = useParams<{ fundId: string }>();
  const { language } = useAppPreferences();
  const t = useMemo(() => messages[language === "en-US" ? "en-US" : "zh-CN"], [language]);

  const [date, setDate] = useState<string>(todayISO);
  const [checkpoints, setCheckpoints] = useState<WorkflowCheckpoint[]>([]);
  const [loading, setLoading] = useState(false);

  const fetchData = useCallback(async () => {
    if (!fundId || !date) return;
    setLoading(true);
    try {
      const resp = await listFundWorkflowCheckpoints({ fundId, tradingDate: date });
      setCheckpoints(resp.checkpoints ?? []);
    } catch (err) {
      // 5xx + network already toast in lib/api.ts; explicit toast
      // here covers 4xx (e.g. 403 if the user lost ownership of
      // this fund mid-session, or 400 from a malformed date) so
      // the user gets feedback rather than a silent empty table.
      toast.error(t.errorTitle, formatApiError(err, t.errorTitle));
      setCheckpoints([]);
    } finally {
      setLoading(false);
    }
  }, [fundId, date, t.errorTitle]);

  useEffect(() => {
    void fetchData();
  }, [fetchData]);

  return (
    <div className="space-y-6 p-6">
      <header>
        <h1 className="text-2xl font-semibold text-gray-900">{t.title}</h1>
        <p className="mt-1 max-w-3xl text-sm leading-relaxed text-gray-600">{t.subtitle}</p>
      </header>

      <section className="rounded-lg border border-gray-200 bg-white p-5 shadow-sm">
        <div className="flex flex-wrap items-end gap-3">
          <label className="flex flex-col gap-1 text-sm text-gray-700">
            <span>{t.dateFilter}</span>
            <input
              type="date"
              value={date}
              onChange={(e) => setDate(e.target.value)}
              className="rounded-md border border-gray-300 bg-white px-3 py-1.5 text-sm text-gray-900"
            />
          </label>
          <button
            type="button"
            onClick={() => setDate(todayISO())}
            className="rounded-md border border-gray-300 bg-white px-3 py-1.5 text-sm text-gray-700 hover:bg-gray-50"
          >
            {t.todayLabel}
          </button>
          <button
            type="button"
            onClick={() => void fetchData()}
            disabled={loading}
            className="rounded-md bg-indigo-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-50"
          >
            {loading ? t.loading : t.refresh}
          </button>
        </div>

        <div className="mt-4 overflow-x-auto">
          <table className="min-w-full text-left text-sm">
            <thead className="border-b border-gray-200 text-gray-500">
              <tr>
                <th className="px-3 py-2 font-medium">{t.colStep}</th>
                <th className="px-3 py-2 font-medium">{t.colStatus}</th>
                <th className="px-3 py-2 font-medium">{t.colAttempts}</th>
                <th className="px-3 py-2 font-medium">{t.colDuration}</th>
                <th className="px-3 py-2 font-medium">{t.colEnded}</th>
                <th className="px-3 py-2 font-medium">{t.colError}</th>
              </tr>
            </thead>
            <tbody className="text-gray-800">
              {checkpoints.length === 0 ? (
                <tr>
                  <td colSpan={6} className="px-3 py-6 text-center text-gray-400">
                    {loading ? t.loading : t.empty}
                  </td>
                </tr>
              ) : (
                checkpoints.map((cp) => (
                  <tr key={cp.id} className="border-b border-gray-100">
                    <td className="px-3 py-2 font-mono text-gray-800">{cp.step}</td>
                    <td className="px-3 py-2">
                      <span
                        className={`rounded-full border px-2 py-0.5 text-xs ${STATUS_BADGES[cp.status]}`}
                      >
                        {cp.status}
                      </span>
                    </td>
                    <td className="px-3 py-2 text-gray-500">{cp.attempts}</td>
                    <td className="px-3 py-2 text-gray-500">{formatDuration(cp.duration_ms)}</td>
                    <td className="px-3 py-2 text-gray-500">{formatRelative(cp.ended_at)}</td>
                    <td className="px-3 py-2 text-rose-600">
                      {cp.error_text ? (
                        <span title={cp.error_text} className="line-clamp-1">
                          {truncate(cp.error_text, 80)}
                        </span>
                      ) : (
                        "—"
                      )}
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>

        <p className="mt-4 text-xs text-gray-500">{t.needResumeHint}</p>
      </section>
    </div>
  );
}
