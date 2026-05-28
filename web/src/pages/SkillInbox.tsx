import React, { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { apiGet, apiPost, formatApiError } from "../lib/api";
import { formatDateTimeForLanguage, useAppPreferences } from "../lib/preferences";

interface ProposedSkillRow {
  fundId: string;
  fundName?: string;
  agentId: string;
  agentName?: string;
  agentRole?: string;
  ownerUserId?: string;
  skillKey: string;
  skillName: string;
  description?: string;
  source?: string;
  proposedAt?: string;
  ageHours: number;
  shadowEvalRuns?: number;
  shadowMeanSharpe?: number;
  shadowMeanHitRate?: number;
  lastShadowAt?: string;
}

interface ProposedSkillsResponse {
  generatedAt: string;
  items: ProposedSkillRow[];
}

interface ShadowEvalResponse {
  skillKey: string;
  strategy: string;
  sharpe: number;
  hitRatePct: number;
  annualReturn: number;
  annualVol: number;
  maxDrawdown: number;
  tradingDays: number;
  autoApproved: boolean;
  runNumber: number;
  threshold: string;
  evaluatedAt: string;
}

const copyByLanguage = {
  "zh-CN": {
    title: "技能审批 inbox",
    subtitle:
      "跨基金汇总所有 status=proposed 的候选技能。手动 shadow 回测并审批，或等待 3 次评估全部过门槛后自动放行。",
    backToAdmin: "返回管理后台",
    filterLabel: "最少积压（小时）",
    refresh: "刷新",
    empty: "暂无候选技能。reflection cycle 会在 7 天周期内自动 propose。",
    columns: {
      skill: "技能",
      fund: "基金",
      agent: "Agent",
      age: "已积压",
      shadow: "Shadow 回测",
      actions: "操作",
    },
    runShadow: "回测",
    approve: "立即批准",
    sharpeLabel: "Sharpe",
    hitRateLabel: "命中率",
    runsLabel: "次数",
    autoApproved: "已自动批准",
    threshold: "门槛",
    hours: "小时",
    notRun: "未回测",
    refreshAfterAction: "刚刚批准或回测后会自动刷新列表。",
  },
  "en-US": {
    title: "Skill approval inbox",
    subtitle:
      "All proposed skill candidates across funds. Run a shadow backtest and approve manually, or let three consecutive runs above threshold auto-approve.",
    backToAdmin: "Back to admin",
    filterLabel: "Min backlog (hours)",
    refresh: "Refresh",
    empty: "No candidate skills yet. The reflection cycle proposes within 7 days.",
    columns: {
      skill: "Skill",
      fund: "Fund",
      agent: "Agent",
      age: "Backlog",
      shadow: "Shadow eval",
      actions: "Actions",
    },
    runShadow: "Run backtest",
    approve: "Approve now",
    sharpeLabel: "Sharpe",
    hitRateLabel: "Hit rate",
    runsLabel: "Runs",
    autoApproved: "Auto-approved",
    threshold: "Threshold",
    hours: "h",
    notRun: "Not evaluated",
    refreshAfterAction: "List auto-refreshes after a shadow eval or approval.",
  },
} as const;

const SkillInbox: React.FC = () => {
  const { language } = useAppPreferences();
  const copy = copyByLanguage[language];

  const [items, setItems] = useState<ProposedSkillRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [ageMinHours, setAgeMinHours] = useState(0);
  const [busyKey, setBusyKey] = useState<string | null>(null);
  const [lastEval, setLastEval] = useState<Record<string, ShadowEvalResponse>>({});
  const [generatedAt, setGeneratedAt] = useState<string>("");

  const fetchList = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const params = ageMinHours > 0 ? `?ageMinHours=${ageMinHours}` : "";
      const resp = await apiGet<ProposedSkillsResponse>(`/api/admin/skills/proposed${params}`);
      setItems(resp.items ?? []);
      setGeneratedAt(resp.generatedAt);
    } catch (err) {
      setError(formatApiError(err, language === "en-US" ? "Failed to load inbox" : "加载 inbox 失败"));
    } finally {
      setLoading(false);
    }
  }, [ageMinHours, language]);

  useEffect(() => {
    void fetchList();
  }, [fetchList]);

  const runShadow = useCallback(
    async (fundID: string, skillKey: string) => {
      const key = `${fundID}::${skillKey}`;
      setBusyKey(key);
      try {
        const resp = await apiPost<ShadowEvalResponse>(
          `/api/admin/skills/${encodeURIComponent(fundID)}/${encodeURIComponent(skillKey)}/shadow-evaluate`,
          {},
        );
        setLastEval((prev) => ({ ...prev, [key]: resp }));
        await fetchList();
      } catch (err) {
        setError(formatApiError(err, language === "en-US" ? "Shadow eval failed" : "Shadow 回测失败"));
      } finally {
        setBusyKey(null);
      }
    },
    [fetchList, language],
  );

  const approveNow = useCallback(
    async (fundID: string, skillKey: string) => {
      const key = `${fundID}::${skillKey}`;
      setBusyKey(key);
      try {
        await apiPost(
          `/api/admin/skills/${encodeURIComponent(fundID)}/${encodeURIComponent(skillKey)}/approve`,
          {},
        );
        await fetchList();
      } catch (err) {
        setError(formatApiError(err, language === "en-US" ? "Approve failed" : "批准失败"));
      } finally {
        setBusyKey(null);
      }
    },
    [fetchList, language],
  );

  const sortedItems = useMemo(() => items, [items]);

  return (
    <div className="min-h-screen bg-gray-50 px-6 py-10">
      <div className="mx-auto max-w-6xl">
        <div className="mb-6 flex items-center justify-between">
          <div>
            <h1 className="text-2xl font-semibold text-gray-900">{copy.title}</h1>
            <p className="mt-1 text-sm text-gray-500">{copy.subtitle}</p>
          </div>
          <Link
            to="/admin"
            className="rounded-md border border-gray-300 bg-white px-3 py-1.5 text-sm font-medium text-gray-700 shadow-sm hover:bg-gray-100"
          >
            ← {copy.backToAdmin}
          </Link>
        </div>

        <div className="mb-4 flex items-center gap-3 rounded-md bg-white p-4 shadow">
          <label className="text-sm text-gray-700">
            {copy.filterLabel}:{" "}
            <input
              type="number"
              min={0}
              value={ageMinHours}
              onChange={(e) => setAgeMinHours(Math.max(0, Number(e.target.value) || 0))}
              className="ml-2 w-20 rounded border border-gray-300 px-2 py-1 text-sm"
            />
          </label>
          <button
            type="button"
            onClick={() => void fetchList()}
            className="rounded-md bg-indigo-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-indigo-700"
            disabled={loading}
          >
            {copy.refresh}
          </button>
          {generatedAt ? (
            <span className="text-xs text-gray-400">
              {formatDateTimeForLanguage(generatedAt, language)}
            </span>
          ) : null}
        </div>

        {error ? (
          <div className="mb-4 rounded-md border border-red-200 bg-red-50 p-3 text-sm text-red-700">
            {error}
          </div>
        ) : null}

        <p className="mb-2 text-xs text-gray-400">{copy.refreshAfterAction}</p>

        <div className="overflow-hidden rounded-lg bg-white shadow">
          <table className="w-full table-auto text-left text-sm">
            <thead className="bg-gray-100 text-xs uppercase text-gray-600">
              <tr>
                <th className="px-4 py-3">{copy.columns.skill}</th>
                <th className="px-4 py-3">{copy.columns.fund}</th>
                <th className="px-4 py-3">{copy.columns.agent}</th>
                <th className="px-4 py-3">{copy.columns.age}</th>
                <th className="px-4 py-3">{copy.columns.shadow}</th>
                <th className="px-4 py-3">{copy.columns.actions}</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-100">
              {loading ? (
                <tr>
                  <td className="px-4 py-6 text-center text-gray-400" colSpan={6}>
                    …
                  </td>
                </tr>
              ) : sortedItems.length === 0 ? (
                <tr>
                  <td className="px-4 py-6 text-center text-gray-400" colSpan={6}>
                    {copy.empty}
                  </td>
                </tr>
              ) : (
                sortedItems.map((row) => {
                  const key = `${row.fundId}::${row.skillKey}`;
                  const localEval = lastEval[key];
                  const meanS = row.shadowMeanSharpe ?? 0;
                  const meanH = row.shadowMeanHitRate ?? 0;
                  const runs = row.shadowEvalRuns ?? 0;
                  return (
                    <tr key={key} className="hover:bg-gray-50">
                      <td className="px-4 py-3">
                        <div className="font-medium text-gray-900">{row.skillName}</div>
                        <div className="text-xs text-gray-400">{row.skillKey}</div>
                        {row.description ? (
                          <div className="mt-1 text-xs text-gray-500">{row.description}</div>
                        ) : null}
                      </td>
                      <td className="px-4 py-3">
                        <div className="text-gray-900">{row.fundName ?? row.fundId}</div>
                        <div className="text-xs text-gray-400">{row.fundId}</div>
                      </td>
                      <td className="px-4 py-3">
                        <div className="text-gray-900">{row.agentName}</div>
                        <div className="text-xs text-gray-400">{row.agentRole}</div>
                      </td>
                      <td className="px-4 py-3 text-gray-700">
                        {Math.round(row.ageHours)}
                        {copy.hours}
                      </td>
                      <td className="px-4 py-3">
                        {runs === 0 && !localEval ? (
                          <span className="text-xs text-gray-400">{copy.notRun}</span>
                        ) : (
                          <div className="text-xs text-gray-600">
                            <div>
                              {copy.sharpeLabel}: <span className="font-mono">{(localEval?.sharpe ?? meanS).toFixed(2)}</span>
                            </div>
                            <div>
                              {copy.hitRateLabel}: <span className="font-mono">{(localEval?.hitRatePct ?? meanH).toFixed(1)}%</span>
                            </div>
                            <div>
                              {copy.runsLabel}: <span className="font-mono">{localEval?.runNumber ?? runs}</span>
                            </div>
                            {localEval?.autoApproved ? (
                              <div className="mt-1 inline-block rounded bg-green-100 px-2 py-0.5 text-xs text-green-800">
                                {copy.autoApproved}
                              </div>
                            ) : null}
                          </div>
                        )}
                      </td>
                      <td className="px-4 py-3">
                        <div className="flex flex-col gap-1">
                          <button
                            type="button"
                            onClick={() => void runShadow(row.fundId, row.skillKey)}
                            disabled={busyKey === key}
                            className="rounded bg-blue-600 px-2.5 py-1 text-xs font-medium text-white hover:bg-blue-700 disabled:opacity-50"
                          >
                            {copy.runShadow}
                          </button>
                          <button
                            type="button"
                            onClick={() => void approveNow(row.fundId, row.skillKey)}
                            disabled={busyKey === key}
                            className="rounded bg-green-600 px-2.5 py-1 text-xs font-medium text-white hover:bg-green-700 disabled:opacity-50"
                          >
                            {copy.approve}
                          </button>
                        </div>
                      </td>
                    </tr>
                  );
                })
              )}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  );
};

export default SkillInbox;
