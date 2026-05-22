import React, { useCallback, useEffect, useMemo, useState } from "react";
import { useParams } from "react-router-dom";
import {
  AgentLearningScope,
  AgentLearningStatus,
  apiGet,
  disableAgentLearning,
  enableAgentLearning,
  fetchAgentLearning,
  formatApiError,
  revokeAgentLearning,
  updateAgentLearningScope,
} from "../lib/api";
import { formatDateForLanguage, formatDateTimeForLanguage, formatNumberForLanguage, useAppPreferences } from "../lib/preferences";
import ReflectionsPanel from "../components/ReflectionsPanel";
import AgentSkillsPanel from "../components/AgentSkillsPanel";
import StrategyAttributionPanel from "../components/StrategyAttributionPanel";
import NextRunBanner from "../components/NextRunBanner";

interface TeamAgent {
  id: string;
  agentId?: string;
  name?: string;
  role: string;
  focus?: string;
  latestLearningSummary?: string;
  latestLearningAt?: string;
  latestLearningTags?: string[];
}

interface ScopeEditorState {
  fundIds: string;
  markets: string;
  assetClasses: string;
  themes: string;
  instruments: string;
  styleHints: string;
  memoryScope: string;
}

function normalizeListInput(value: string): string[] {
  return value
    .split(/[\n\r,，、;；]/)
    .map((item) => item.trim())
    .filter(Boolean);
}

function scopeToEditor(scope?: AgentLearningScope): ScopeEditorState {
  return {
    fundIds: (scope?.fundIds ?? []).join(", "),
    markets: (scope?.markets ?? []).join(", "),
    assetClasses: (scope?.assetClasses ?? []).join(", "),
    themes: (scope?.themes ?? []).join(", "),
    instruments: (scope?.instruments ?? []).join(", "),
    styleHints: (scope?.styleHints ?? []).join(", "),
    memoryScope: scope?.memoryScope ?? "",
  };
}

function editorToScope(editor: ScopeEditorState): AgentLearningScope {
  return {
    fundIds: normalizeListInput(editor.fundIds),
    markets: normalizeListInput(editor.markets),
    assetClasses: normalizeListInput(editor.assetClasses),
    themes: normalizeListInput(editor.themes),
    instruments: normalizeListInput(editor.instruments),
    styleHints: normalizeListInput(editor.styleHints),
    memoryScope: editor.memoryScope.trim(),
  };
}

function StatusPill({ enabled, labels }: { enabled: boolean; labels: { enabled: string; disabled: string } }) {
  return (
    <span className={`inline-flex rounded-full px-2.5 py-1 text-xs font-semibold ${enabled ? "bg-emerald-100 text-emerald-700" : "bg-gray-100 text-gray-600"}`}>
      {enabled ? labels.enabled : labels.disabled}
    </span>
  );
}

const AgentLearning: React.FC = () => {
  const { fundId } = useParams<{ fundId: string }>();
  const { language } = useAppPreferences();
  const [agents, setAgents] = useState<TeamAgent[]>([]);
  const [selectedAgentId, setSelectedAgentId] = useState("");
  const [learning, setLearning] = useState<AgentLearningStatus | null>(null);
  const [scopeEditor, setScopeEditor] = useState<ScopeEditorState>(scopeToEditor());
  const [autoApply, setAutoApply] = useState(true);
  const [maxLessons, setMaxLessons] = useState(3);
  const [revokeReason, setRevokeReason] = useState("");
  const [loading, setLoading] = useState(true);
  const [learningLoading, setLearningLoading] = useState(false);
  const [savingAction, setSavingAction] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            title: "Agent learning management",
            subtitle:
              "Review each member's self-learning records, control whether daily lessons are applied, revoke unsafe learning, and constrain learning scope before it affects production behavior.",
            loading: "Loading learning management...",
            missingFundId: "Missing fundId",
            loadFailed: "Failed to load learning management",
            retry: "Retry",
            memberList: "Team members",
            selectMember: "Select a member to manage learning.",
            learningStatus: "Learning status",
            enabled: "Enabled",
            disabled: "Disabled",
            enable: "Enable learning",
            disable: "Disable learning",
            saveConfig: "Save learning config",
            saveScope: "Save scope",
            revoke: "Revoke learning",
            saving: "Saving...",
            autoApply: "Auto-apply adjustments",
            maxLessons: "Max lessons / day",
            scopeTitle: "Learning scope limits",
            fundIds: "Allowed fund IDs",
            markets: "Markets",
            assetClasses: "Asset classes",
            themes: "Themes",
            instruments: "Instruments",
            styleHints: "Style hints",
            memoryScope: "Memory scope",
            commaHint: "Comma or line separated",
            latest: "Latest learned state",
            noLatest: "No applied learning summary yet.",
            recentLessons: "Recent lessons",
            adjustments: "Recommended adjustments",
            records: "Learning records",
            noRecords: "No self-learning records yet.",
            hits: "Hits",
            misses: "Issues",
            lessons: "Lessons",
            dailyReturn: "Daily return",
            revoked: "Revoked",
            revokeReason: "Revocation reason",
            revokePlaceholder: "Explain why these lessons should no longer influence the agent...",
            success: "Learning settings updated.",
            role: "Role",
            focus: "Focus",
            updatedAt: "Updated",
          }
        : {
            title: "Agent 学习管理",
            subtitle: "查看每个成员的自主学习记录，控制日终经验是否应用，撤销不安全学习，并在影响生产行为前限定学习范围。",
            loading: "正在加载学习管理...",
            missingFundId: "缺少 fundId",
            loadFailed: "加载学习管理失败",
            retry: "重试",
            memberList: "团队成员",
            selectMember: "选择一个成员管理学习状态。",
            learningStatus: "学习状态",
            enabled: "已启用",
            disabled: "已禁用",
            enable: "启用学习",
            disable: "禁用学习",
            saveConfig: "保存学习配置",
            saveScope: "保存范围",
            revoke: "撤销学习",
            saving: "保存中...",
            autoApply: "自动应用调整",
            maxLessons: "每日最多经验数",
            scopeTitle: "学习范围限制",
            fundIds: "允许的基金 ID",
            markets: "市场",
            assetClasses: "资产类别",
            themes: "主题",
            instruments: "标的",
            styleHints: "风格提示",
            memoryScope: "记忆范围",
            commaHint: "支持逗号或换行分隔",
            latest: "最新学习状态",
            noLatest: "暂无已应用的学习摘要。",
            recentLessons: "近期经验",
            adjustments: "建议调整",
            records: "学习记录",
            noRecords: "暂无自主学习记录。",
            hits: "命中",
            misses: "问题",
            lessons: "经验",
            dailyReturn: "日收益",
            revoked: "已撤销",
            revokeReason: "撤销原因",
            revokePlaceholder: "说明为什么这些经验不应继续影响该 Agent...",
            success: "学习设置已更新。",
            role: "角色",
            focus: "方向",
            updatedAt: "更新于",
          },
    [language],
  );

  const selectedAgent = useMemo(
    () => agents.find((agent) => (agent.agentId ?? agent.id) === selectedAgentId) ?? null,
    [agents, selectedAgentId],
  );

  const loadLearning = useCallback(
    async (agentId: string) => {
      if (!agentId) {
        setLearning(null);
        return;
      }
      setLearningLoading(true);
      setError(null);
      try {
        const status = await fetchAgentLearning(agentId);
        setLearning(status);
        setScopeEditor(scopeToEditor(status.scope));
        setAutoApply(status.autoApplyAdjustments);
        setMaxLessons(status.maxLessonsPerDay);
      } catch (err) {
        setError(formatApiError(err, copy.loadFailed));
      } finally {
        setLearningLoading(false);
      }
    },
    [copy.loadFailed],
  );

  const load = useCallback(async () => {
    if (!fundId) {
      setError(copy.missingFundId);
      setLoading(false);
      return;
    }
    setLoading(true);
    setError(null);
    try {
      const team = await apiGet<TeamAgent[]>(`/api/funds/${fundId}/team`);
      setAgents(team);
      const nextSelected = selectedAgentId || team[0]?.agentId || team[0]?.id || "";
      setSelectedAgentId(nextSelected);
      if (nextSelected) {
        await loadLearning(nextSelected);
      }
    } catch (err) {
      setError(formatApiError(err, copy.loadFailed));
    } finally {
      setLoading(false);
    }
  }, [copy.loadFailed, copy.missingFundId, fundId, loadLearning, selectedAgentId]);

  useEffect(() => {
    void load();
  }, [load]);

  async function runAction(action: string, fn: () => Promise<AgentLearningStatus>) {
    if (!selectedAgentId) {
      return;
    }
    setSavingAction(action);
    setNotice(null);
    setError(null);
    try {
      const status = await fn();
      setLearning(status);
      setScopeEditor(scopeToEditor(status.scope));
      setAutoApply(status.autoApplyAdjustments);
      setMaxLessons(status.maxLessonsPerDay);
      setNotice(copy.success);
    } catch (err) {
      setError(formatApiError(err, copy.loadFailed));
    } finally {
      setSavingAction(null);
    }
  }

  if (loading) {
    return <div className="rounded-xl bg-white p-6 text-sm text-gray-500 shadow-sm">{copy.loading}</div>;
  }

  return (
    <div className="space-y-6">
      <div className="rounded-2xl bg-gradient-to-r from-slate-900 to-indigo-900 p-6 text-white shadow-sm">
        <p className="text-xs font-semibold uppercase tracking-[0.24em] text-indigo-200">P1 · Learning Control</p>
        <h1 className="mt-2 text-2xl font-bold">{copy.title}</h1>
        <p className="mt-2 max-w-3xl text-sm text-indigo-100">{copy.subtitle}</p>
      </div>

      <NextRunBanner fundId={fundId} language={language} />

      {error ? (
        <div className="rounded-xl border border-red-200 bg-red-50 p-4 text-sm text-red-700">
          <div>{error}</div>
          <button onClick={() => void load()} className="mt-3 rounded-md bg-red-600 px-3 py-1.5 text-xs font-semibold text-white hover:bg-red-700">
            {copy.retry}
          </button>
        </div>
      ) : null}
      {notice ? <div className="rounded-xl border border-emerald-200 bg-emerald-50 p-3 text-sm text-emerald-700">{notice}</div> : null}

      <div className="grid gap-6 lg:grid-cols-[320px_1fr]">
        <aside className="rounded-xl bg-white p-4 shadow-sm">
          <h2 className="text-sm font-semibold text-gray-900">{copy.memberList}</h2>
          <p className="mt-1 text-xs text-gray-500">{copy.selectMember}</p>
          <div className="mt-4 space-y-2">
            {agents.map((agent) => {
              const agentId = agent.agentId ?? agent.id;
              const active = agentId === selectedAgentId;
              return (
                <button
                  key={agentId}
                  onClick={() => {
                    setSelectedAgentId(agentId);
                    void loadLearning(agentId);
                  }}
                  className={`w-full rounded-lg border p-3 text-left transition ${active ? "border-indigo-500 bg-indigo-50" : "border-gray-200 hover:border-indigo-200 hover:bg-gray-50"}`}
                >
                  <div className="flex items-center justify-between gap-3">
                    <span className="text-sm font-semibold text-gray-900">{agent.name || agent.role}</span>
                    <span className="rounded-full bg-gray-100 px-2 py-0.5 text-[11px] text-gray-600">{agent.role}</span>
                  </div>
                  {agent.latestLearningSummary ? <p className="mt-2 line-clamp-2 text-xs text-gray-500">{agent.latestLearningSummary}</p> : null}
                </button>
              );
            })}
          </div>
        </aside>

        <main className="space-y-6">
          <section className="rounded-xl bg-white p-5 shadow-sm">
            <div className="flex flex-wrap items-start justify-between gap-4">
              <div>
                <p className="text-xs font-semibold uppercase tracking-wide text-gray-500">{copy.learningStatus}</p>
                <h2 className="mt-1 text-xl font-bold text-gray-900">{learning?.agentName || selectedAgent?.name || selectedAgent?.role || "-"}</h2>
                <div className="mt-2 flex flex-wrap gap-2 text-xs text-gray-500">
                  <span>{copy.role}: {learning?.role || selectedAgent?.role || "-"}</span>
                  {(learning?.focus || selectedAgent?.focus) ? <span>{copy.focus}: {learning?.focus || selectedAgent?.focus}</span> : null}
                  {learning?.learningUpdatedAt ? <span>{copy.updatedAt}: {formatDateTimeForLanguage(learning.learningUpdatedAt, language)}</span> : null}
                </div>
              </div>
              {learning ? <StatusPill enabled={learning.enabled} labels={{ enabled: copy.enabled, disabled: copy.disabled }} /> : null}
            </div>

            {learningLoading ? <p className="mt-4 text-sm text-gray-500">{copy.loading}</p> : null}
            {learning ? (
              <div className="mt-5 grid gap-4 md:grid-cols-3">
                <label className="rounded-lg border border-gray-200 p-4">
                  <span className="text-xs font-semibold uppercase tracking-wide text-gray-500">{copy.autoApply}</span>
                  <div className="mt-3 flex items-center gap-2">
                    <input type="checkbox" checked={autoApply} onChange={(event) => setAutoApply(event.target.checked)} className="h-4 w-4 rounded border-gray-300 text-indigo-600" />
                    <span className="text-sm text-gray-700">{autoApply ? copy.enabled : copy.disabled}</span>
                  </div>
                </label>
                <label className="rounded-lg border border-gray-200 p-4">
                  <span className="text-xs font-semibold uppercase tracking-wide text-gray-500">{copy.maxLessons}</span>
                  <input type="number" min={1} max={20} value={maxLessons} onChange={(event) => setMaxLessons(Number(event.target.value))} className="mt-2 w-full rounded-md border-gray-300 text-sm" />
                </label>
                <div className="flex flex-col justify-end gap-2">
                  <button
                    disabled={savingAction !== null || !selectedAgentId}
                    onClick={() => void runAction("config", () => enableAgentLearning(selectedAgentId, { autoApplyAdjustments: autoApply, maxLessonsPerDay: maxLessons }))}
                    className="rounded-md bg-indigo-600 px-4 py-2 text-sm font-semibold text-white hover:bg-indigo-700 disabled:opacity-60"
                  >
                    {savingAction === "config" ? copy.saving : copy.saveConfig}
                  </button>
                  <button
                    disabled={savingAction !== null || !selectedAgentId}
                    onClick={() => void runAction(learning.enabled ? "disable" : "enable", () => (learning.enabled ? disableAgentLearning(selectedAgentId) : enableAgentLearning(selectedAgentId)))}
                    className="rounded-md border border-gray-300 px-4 py-2 text-sm font-semibold text-gray-700 hover:bg-gray-50 disabled:opacity-60"
                  >
                    {savingAction === "disable" || savingAction === "enable" ? copy.saving : learning.enabled ? copy.disable : copy.enable}
                  </button>
                </div>
              </div>
            ) : null}
          </section>

          {learning ? (
            <section className="grid gap-6 xl:grid-cols-2">
              <div className="rounded-xl bg-white p-5 shadow-sm">
                <h2 className="text-base font-semibold text-gray-900">{copy.scopeTitle}</h2>
                <p className="mt-1 text-xs text-gray-500">{copy.commaHint}</p>
                <div className="mt-4 grid gap-3 sm:grid-cols-2">
                  {([
                    ["fundIds", copy.fundIds],
                    ["markets", copy.markets],
                    ["assetClasses", copy.assetClasses],
                    ["themes", copy.themes],
                    ["instruments", copy.instruments],
                    ["styleHints", copy.styleHints],
                    ["memoryScope", copy.memoryScope],
                  ] as const).map(([key, label]) => (
                    <label key={key} className={key === "memoryScope" ? "sm:col-span-2" : ""}>
                      <span className="text-xs font-medium text-gray-600">{label}</span>
                      <input
                        value={scopeEditor[key]}
                        onChange={(event) => setScopeEditor((prev) => ({ ...prev, [key]: event.target.value }))}
                        className="mt-1 w-full rounded-md border-gray-300 text-sm"
                      />
                    </label>
                  ))}
                </div>
                <button
                  disabled={savingAction !== null || !selectedAgentId}
                  onClick={() => void runAction("scope", () => updateAgentLearningScope(selectedAgentId, editorToScope(scopeEditor)))}
                  className="mt-4 rounded-md bg-slate-900 px-4 py-2 text-sm font-semibold text-white hover:bg-slate-800 disabled:opacity-60"
                >
                  {savingAction === "scope" ? copy.saving : copy.saveScope}
                </button>
              </div>

              <div className="rounded-xl bg-white p-5 shadow-sm">
                <h2 className="text-base font-semibold text-gray-900">{copy.latest}</h2>
                <p className="mt-3 text-sm text-gray-700">{learning.lastLearningSummary || copy.noLatest}</p>
                {learning.lastDailyReturn !== undefined ? (
                  <p className="mt-2 text-xs text-gray-500">
                    {copy.dailyReturn}: {formatNumberForLanguage(learning.lastDailyReturn * 100, language, { maximumFractionDigits: 2 })}%
                  </p>
                ) : null}
                {learning.lastLearningDate ? <p className="mt-1 text-xs text-gray-500">{formatDateForLanguage(learning.lastLearningDate, language)}</p> : null}
                <div className="mt-4 grid gap-4 sm:grid-cols-2">
                  <ListBlock title={copy.recentLessons} items={learning.recentLessons ?? []} />
                  <ListBlock title={copy.adjustments} items={learning.lastAdjustments ?? []} />
                </div>
                <div className="mt-5 border-t border-gray-100 pt-4">
                  <label className="block text-xs font-medium text-gray-600">{copy.revokeReason}</label>
                  <textarea
                    value={revokeReason}
                    onChange={(event) => setRevokeReason(event.target.value)}
                    placeholder={copy.revokePlaceholder}
                    className="mt-1 min-h-[72px] w-full rounded-md border-gray-300 text-sm"
                  />
                  <button
                    disabled={savingAction !== null || !selectedAgentId}
                    onClick={() => void runAction("revoke", () => revokeAgentLearning(selectedAgentId, revokeReason))}
                    className="mt-3 rounded-md border border-red-200 bg-red-50 px-4 py-2 text-sm font-semibold text-red-700 hover:bg-red-100 disabled:opacity-60"
                  >
                    {savingAction === "revoke" ? copy.saving : copy.revoke}
                  </button>
                  {learning.revokedAt ? <p className="mt-2 text-xs text-red-600">{copy.revoked}: {formatDateTimeForLanguage(learning.revokedAt, language)} {learning.revokedReason}</p> : null}
                </div>
              </div>
            </section>
          ) : null}

          <StrategyAttributionPanel fundId={fundId} language={language} />

          <ReflectionsPanel fundId={fundId} language={language} />

          {selectedAgentId ? <AgentSkillsPanel agentId={selectedAgentId} language={language} /> : null}

          <section className="rounded-xl bg-white p-5 shadow-sm">
            <h2 className="text-base font-semibold text-gray-900">{copy.records}</h2>
            <div className="mt-4 space-y-3">
              {(learning?.records ?? []).length === 0 ? <p className="text-sm text-gray-500">{copy.noRecords}</p> : null}
              {(learning?.records ?? []).map((record) => (
                <article key={record.id} className="rounded-lg border border-gray-200 p-4">
                  <div className="flex flex-wrap items-start justify-between gap-3">
                    <div>
                      <h3 className="text-sm font-semibold text-gray-900">{record.title || record.summary || record.id}</h3>
                      <p className="mt-1 text-xs text-gray-500">
                        {record.tradingDate ? formatDateForLanguage(record.tradingDate, language) : formatDateTimeForLanguage(record.createdAt, language)}
                        {record.dailyReturn !== undefined ? ` · ${copy.dailyReturn}: ${formatNumberForLanguage(record.dailyReturn * 100, language, { maximumFractionDigits: 2 })}%` : ""}
                      </p>
                    </div>
                    {record.revoked ? <span className="rounded-full bg-red-50 px-2 py-1 text-xs font-semibold text-red-700">{copy.revoked}</span> : null}
                  </div>
                  {record.summary ? <p className="mt-3 text-sm text-gray-700">{record.summary}</p> : null}
                  <div className="mt-3 grid gap-3 md:grid-cols-3">
                    <ListBlock title={copy.hits} items={record.hits ?? []} compact />
                    <ListBlock title={copy.misses} items={record.misses ?? []} compact />
                    <ListBlock title={copy.lessons} items={record.lessons ?? []} compact />
                  </div>
                </article>
              ))}
            </div>
          </section>
        </main>
      </div>
    </div>
  );
};

const ListBlock: React.FC<{ title: string; items: string[]; compact?: boolean }> = ({ title, items, compact }) => (
  <div>
    <h4 className="text-xs font-semibold uppercase tracking-wide text-gray-500">{title}</h4>
    {items.length === 0 ? <p className="mt-1 text-xs text-gray-400">-</p> : null}
    <ul className={`mt-2 space-y-1 ${compact ? "text-xs" : "text-sm"} text-gray-700`}>
      {items.map((item, index) => (
        <li key={`${item}-${index}`} className="flex gap-2">
          <span className="mt-1 h-1.5 w-1.5 flex-none rounded-full bg-indigo-400" />
          <span>{item}</span>
        </li>
      ))}
    </ul>
  </div>
);

export default AgentLearning;
