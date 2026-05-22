import React, { useCallback, useEffect, useMemo, useState } from "react";
import {
  AgentSkillEntry,
  approveAgentSkill,
  fetchAgentSkills,
  formatApiError,
  rejectAgentSkill,
} from "../lib/api";
import { formatDateTimeForLanguage, type AppLanguage } from "../lib/preferences";

interface AgentSkillsPanelProps {
  agentId?: string;
  language: AppLanguage;
}

interface PanelCopy {
  title: string;
  subtitle: string;
  loading: string;
  empty: string;
  emptyHint: string;
  proposedSection: string;
  approvedSection: string;
  proposedBadge: string;
  approvedBadge: string;
  reflectionSource: string;
  approve: string;
  reject: string;
  rejectConfirm: string;
  approvedAt: string;
  proposedAt: string;
  rolesLabel: string;
  retry: string;
  loadFailed: string;
  unavailableTitle: string;
  unavailableHint: string;
  busy: string;
}

const COPY: Record<AppLanguage, PanelCopy> = {
  "zh-CN": {
    title: "技能库 / Skill Library",
    subtitle: "由 memory.Reflect 自动反思生成的候选技能。需要你审批后才会被研究/PM/交易 agent 的 prompt 调用。",
    loading: "加载中…",
    empty: "暂无技能",
    emptyHint: "等待下一次每日复盘和长期反思后产生候选技能；或手动通过 Agent 配置编辑技能库。",
    proposedSection: "待审批",
    approvedSection: "已生效",
    proposedBadge: "PROPOSED",
    approvedBadge: "APPROVED",
    reflectionSource: "来自反思",
    approve: "批准",
    reject: "拒绝",
    rejectConfirm: "确定要从技能库移除这条技能吗？后续如果反思再次提到同一主题，会重新生成候选。",
    approvedAt: "批准于",
    proposedAt: "提议于",
    rolesLabel: "适用角色",
    retry: "重试",
    loadFailed: "无法加载技能库",
    unavailableTitle: "服务未配置",
    unavailableHint: "当前服务端未启用 Skill 管理（缺少 AgentSkillService 装配）。",
    busy: "处理中…",
  },
  "en-US": {
    title: "Skill Library",
    subtitle: "Candidate skills produced by memory.Reflect. They only enter agent prompts after you approve them.",
    loading: "Loading…",
    empty: "No skills yet",
    emptyHint: "Wait for the next daily review + long-term reflection to surface candidates; or edit the skill library directly via the agent config.",
    proposedSection: "Pending approval",
    approvedSection: "Active",
    proposedBadge: "PROPOSED",
    approvedBadge: "APPROVED",
    reflectionSource: "From reflection",
    approve: "Approve",
    reject: "Reject",
    rejectConfirm: "Remove this skill from the library? A future reflection over the same theme can regenerate it.",
    approvedAt: "Approved at",
    proposedAt: "Proposed at",
    rolesLabel: "Roles",
    retry: "Retry",
    loadFailed: "Failed to load skills",
    unavailableTitle: "Service unavailable",
    unavailableHint: "Skill management is not configured on this server (AgentSkillService missing).",
    busy: "Working…",
  },
};

function statusOf(skill: AgentSkillEntry): "proposed" | "approved" {
  return skill.status === "proposed" ? "proposed" : "approved";
}

const AgentSkillsPanel: React.FC<AgentSkillsPanelProps> = ({ agentId, language }) => {
  const copy = COPY[language];
  const [skills, setSkills] = useState<AgentSkillEntry[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [unavailable, setUnavailable] = useState(false);
  const [busyKey, setBusyKey] = useState<string | null>(null);

  const load = useCallback(async () => {
    if (!agentId) {
      setSkills([]);
      return;
    }
    setLoading(true);
    setError(null);
    setUnavailable(false);
    try {
      const resp = await fetchAgentSkills(agentId);
      setSkills(resp.skills ?? []);
    } catch (err) {
      const message = formatApiError(err, copy.loadFailed);
      if (typeof message === "string" && message.toLowerCase().includes("agent_skills_unavailable")) {
        setUnavailable(true);
      } else {
        setError(message);
      }
    } finally {
      setLoading(false);
    }
  }, [agentId, copy.loadFailed]);

  useEffect(() => {
    void load();
  }, [load]);

  const { proposed, approved } = useMemo(() => {
    const proposedSkills: AgentSkillEntry[] = [];
    const approvedSkills: AgentSkillEntry[] = [];
    for (const skill of skills) {
      if (statusOf(skill) === "proposed") {
        proposedSkills.push(skill);
      } else {
        approvedSkills.push(skill);
      }
    }
    return { proposed: proposedSkills, approved: approvedSkills };
  }, [skills]);

  const handleApprove = useCallback(
    async (skill: AgentSkillEntry) => {
      if (!agentId) return;
      setBusyKey(skill.key);
      setError(null);
      try {
        await approveAgentSkill(agentId, skill.key);
        await load();
      } catch (err) {
        setError(formatApiError(err, copy.loadFailed));
      } finally {
        setBusyKey(null);
      }
    },
    [agentId, copy.loadFailed, load],
  );

  const handleReject = useCallback(
    async (skill: AgentSkillEntry) => {
      if (!agentId) return;
      // Confirm via the browser dialog — the action is destructive (removes
      // the entry from the library) so an explicit ack avoids accidental
      // clicks. A reflection over the same theme can regenerate the
      // candidate later, so this is not a hard delete in spirit.
      if (typeof window !== "undefined" && !window.confirm(copy.rejectConfirm)) {
        return;
      }
      setBusyKey(skill.key);
      setError(null);
      try {
        await rejectAgentSkill(agentId, skill.key);
        await load();
      } catch (err) {
        setError(formatApiError(err, copy.loadFailed));
      } finally {
        setBusyKey(null);
      }
    },
    [agentId, copy.loadFailed, copy.rejectConfirm, load],
  );

  return (
    <section className="rounded-xl bg-white p-5 shadow-sm">
      <div className="flex items-start justify-between gap-3">
        <div>
          <h2 className="text-base font-semibold text-gray-900">{copy.title}</h2>
          <p className="mt-1 text-xs text-gray-500">{copy.subtitle}</p>
        </div>
        <button
          type="button"
          onClick={() => void load()}
          className="rounded-md border border-gray-200 px-3 py-1.5 text-xs font-semibold text-gray-700 hover:bg-gray-50 disabled:opacity-60"
          disabled={loading || !agentId || busyKey !== null}
        >
          {copy.retry}
        </button>
      </div>

      {loading ? <p className="mt-4 text-sm text-gray-500">{copy.loading}</p> : null}

      {error ? (
        <div className="mt-4 rounded-md border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700">{error}</div>
      ) : null}

      {unavailable ? (
        <div className="mt-4 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-700">
          <strong>{copy.unavailableTitle}.</strong> {copy.unavailableHint}
        </div>
      ) : null}

      {!loading && !error && !unavailable ? (
        <div className="mt-5 space-y-6">
          <SkillSection
            heading={copy.proposedSection}
            highlight
            skills={proposed}
            language={language}
            copy={copy}
            busyKey={busyKey}
            onApprove={handleApprove}
            onReject={handleReject}
          />
          <SkillSection
            heading={copy.approvedSection}
            highlight={false}
            skills={approved}
            language={language}
            copy={copy}
            busyKey={busyKey}
            onApprove={handleApprove}
            onReject={handleReject}
          />
          {skills.length === 0 ? (
            <div className="rounded-md border border-dashed border-gray-200 px-4 py-6 text-center text-sm text-gray-500">
              <p>{copy.empty}</p>
              <p className="mt-1 text-xs text-gray-400">{copy.emptyHint}</p>
            </div>
          ) : null}
        </div>
      ) : null}
    </section>
  );
};

interface SectionProps {
  heading: string;
  highlight: boolean;
  skills: AgentSkillEntry[];
  language: AppLanguage;
  copy: PanelCopy;
  busyKey: string | null;
  onApprove: (skill: AgentSkillEntry) => void;
  onReject: (skill: AgentSkillEntry) => void;
}

const SkillSection: React.FC<SectionProps> = ({ heading, highlight, skills, language, copy, busyKey, onApprove, onReject }) => {
  if (skills.length === 0) {
    return null;
  }
  return (
    <div>
      <h3 className="text-xs font-semibold uppercase tracking-wide text-gray-500">{heading}</h3>
      <ul className="mt-2 space-y-3">
        {skills.map((skill) => {
          const isProposed = statusOf(skill) === "proposed";
          const busy = busyKey === skill.key;
          const sourceIsReflection = (skill.source ?? "").startsWith("reflection:");
          return (
            <li
              key={skill.key}
              className={`rounded-lg border p-4 ${highlight && isProposed ? "border-indigo-200 bg-indigo-50/50" : "border-gray-200"}`}
            >
              <div className="flex flex-wrap items-baseline justify-between gap-2">
                <div className="flex items-center gap-2">
                  <span
                    className={`rounded-full px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${
                      isProposed ? "bg-indigo-100 text-indigo-700" : "bg-emerald-100 text-emerald-700"
                    }`}
                  >
                    {isProposed ? copy.proposedBadge : copy.approvedBadge}
                  </span>
                  <span className="text-sm font-semibold text-gray-900">{skill.name || skill.key}</span>
                  {sourceIsReflection ? (
                    <span className="text-[11px] text-gray-500">· {copy.reflectionSource}</span>
                  ) : null}
                </div>
                <span className="text-xs text-gray-400">
                  {isProposed
                    ? `${copy.proposedAt}: ${formatDateTimeForLanguage(skill.proposedAt, language)}`
                    : skill.approvedAt
                      ? `${copy.approvedAt}: ${formatDateTimeForLanguage(skill.approvedAt, language)}`
                      : ""}
                </span>
              </div>
              {skill.description ? (
                <p className="mt-2 text-sm text-gray-700">{skill.description}</p>
              ) : null}
              {(skill.roles ?? []).length > 0 ? (
                <p className="mt-2 text-[11px] text-gray-500">
                  {copy.rolesLabel}: {(skill.roles ?? []).join(", ")}
                </p>
              ) : null}
              <details className="mt-3 text-xs text-gray-600">
                <summary className="cursor-pointer select-none text-gray-500 hover:text-gray-700">…</summary>
                <pre className="mt-2 whitespace-pre-wrap rounded-md bg-gray-50 p-3 text-[11px] text-gray-700">{skill.content || ""}</pre>
              </details>
              <div className="mt-3 flex flex-wrap gap-2">
                {isProposed ? (
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() => onApprove(skill)}
                    className="rounded-md bg-indigo-600 px-3 py-1.5 text-xs font-semibold text-white hover:bg-indigo-700 disabled:opacity-60"
                  >
                    {busy ? copy.busy : copy.approve}
                  </button>
                ) : null}
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => onReject(skill)}
                  className="rounded-md border border-red-200 px-3 py-1.5 text-xs font-semibold text-red-700 hover:bg-red-50 disabled:opacity-60"
                >
                  {busy ? copy.busy : copy.reject}
                </button>
              </div>
            </li>
          );
        })}
      </ul>
    </div>
  );
};

export default AgentSkillsPanel;
