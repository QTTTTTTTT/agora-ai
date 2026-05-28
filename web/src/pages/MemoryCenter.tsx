import React, { useCallback, useEffect, useMemo, useState } from "react";
import { useParams } from "react-router-dom";
import { apiGet, formatApiError } from "../lib/api";
import { formatDateForLanguage, formatNumberForLanguage, useAppPreferences } from "../lib/preferences";

import type { MemoryLayer as SharedMemoryLayer } from "@fundai/api-client";

// MemoryCenter UI exposes a 5-tab subset of the canonical
// server-side layer union (see @fundai/api-client). "attribution"
// is excluded because it's a system-managed write-only layer that
// doesn't get its own user-facing tab — those entries surface via
// the Skill Inbox / lesson lineage views instead.
type MemoryLayer = Exclude<SharedMemoryLayer, "attribution">;
type ViewMode = "content" | "search" | "timeline" | "stats";
type MemoryFocus = "all" | "market";

interface TeamAgent {
  id: string;
  agentId?: string;
  name?: string;
  role: string;
}

interface MemoryEntry {
  id: string;
  agentId?: string;
  title?: string;
  content: string;
  layer: MemoryLayer;
  tradingDate?: string;
  tags?: string[];
  createdAt: string;
  updatedAt: string;
}

interface MemoryContextResponse {
  fundId: string;
  layer: MemoryLayer;
  entries: MemoryEntry[];
}

interface SearchResult {
  entry: MemoryEntry;
  matchedSnippet: string;
}

interface MemoryStats {
  totalEntries: number;
  mostActiveAgent: string;
  keyThemes: { theme: string; count: number }[];
  entriesByLayer: Record<MemoryLayer, number>;
}

function highlightMatch(text: string, query: string): React.ReactNode {
  if (!query) {
    return text;
  }
  const idx = text.toLowerCase().indexOf(query.toLowerCase());
  if (idx === -1) {
    return text;
  }
  const before = text.slice(Math.max(0, idx - 60), idx);
  const match = text.slice(idx, idx + query.length);
  const after = text.slice(idx + query.length, idx + query.length + 60);
  return (
    <span>
      {before.length > 0 ? "..." : ""}
      {before}
      <mark className="rounded bg-yellow-200 px-0.5">{match}</mark>
      {after}
      {after.length > 0 ? "..." : ""}
    </span>
  );
}

function searchEntries(entries: MemoryEntry[], query: string, getTitle: (entry: MemoryEntry) => string): SearchResult[] {
  if (!query.trim()) {
    return [];
  }
  const normalized = query.toLowerCase();
  return entries.flatMap((entry) => {
    const source = `${getTitle(entry)}\n${entry.content}`.toLowerCase();
    const index = source.indexOf(normalized);
    if (index === -1) {
      return [];
    }
    const contentIndex = entry.content.toLowerCase().indexOf(normalized);
    if (contentIndex === -1) {
      return [{ entry, matchedSnippet: getTitle(entry) }];
    }
    const start = Math.max(0, contentIndex - 60);
    const end = Math.min(entry.content.length, contentIndex + query.length + 60);
    return [{ entry, matchedSnippet: entry.content.slice(start, end) }];
  });
}

function normalizeEntrySearchText(entry: MemoryEntry): string {
  return `${entry.title ?? ""}\n${entry.content}\n${(entry.tags ?? []).join(" ")}`.toLowerCase();
}

function isMarketMemoryEntry(entry: MemoryEntry): boolean {
  const text = normalizeEntrySearchText(entry);
  return /(market|research|news|quote|signal|benchmark|ticker|symbol|行情|资讯|研究|信号|基准)/.test(text);
}

function marketMemoryKind(entry: MemoryEntry): "research" | "news" | "quote" | "signal" | null {
  const text = normalizeEntrySearchText(entry);
  if (/(news|headline|digest|资讯|新闻)/.test(text)) {
    return "news";
  }
  if (/(quote|price|行情|价格)/.test(text)) {
    return "quote";
  }
  if (/(signal|alpha|因子|信号)/.test(text)) {
    return "signal";
  }
  if (/(research|thesis|观点|研究)/.test(text)) {
    return "research";
  }
  return null;
}

const MarkdownBlock: React.FC<{ text: string }> = ({ text }) => {
  const lines = text.split("\n");
  return (
    <div className="prose prose-sm max-w-none text-gray-700">
      {lines.map((line, index) => {
        if (line.startsWith("# ")) {
          return (
            <h1 key={index} className="mb-2 mt-4 text-xl font-bold text-gray-900">
              {line.slice(2)}
            </h1>
          );
        }
        if (line.startsWith("## ")) {
          return (
            <h2 key={index} className="mb-1.5 mt-3 text-lg font-bold text-gray-800">
              {line.slice(3)}
            </h2>
          );
        }
        if (line.startsWith("### ")) {
          return (
            <h3 key={index} className="mb-1 mt-2 text-base font-semibold text-gray-800">
              {line.slice(4)}
            </h3>
          );
        }
        if (line.startsWith("- ")) {
          return (
            <li key={index} className="ml-4 list-disc">
              {renderInline(line.slice(2))}
            </li>
          );
        }
        if (/^\d+\.\s/.test(line)) {
          return (
            <li key={index} className="ml-4 list-decimal">
              {renderInline(line.replace(/^\d+\.\s/, ""))}
            </li>
          );
        }
        if (line.startsWith("|")) {
          return (
            <code key={index} className="block bg-gray-50 px-2 py-0.5 font-mono text-xs">
              {line}
            </code>
          );
        }
        if (line.trim() === "") {
          return <div key={index} className="h-2" />;
        }
        return (
          <p key={index} className="mb-1">
            {renderInline(line)}
          </p>
        );
      })}
    </div>
  );
};

function renderInline(text: string): React.ReactNode {
  const parts = text.split(/(\*\*[^*]+\*\*)/g);
  return parts.map((part, index) =>
    part.startsWith("**") && part.endsWith("**") ? (
      <strong key={index} className="font-semibold text-gray-900">
        {part.slice(2, -2)}
      </strong>
    ) : (
      <span key={index}>{part}</span>
    ),
  );
}

const MemoryCenter: React.FC = () => {
  const { fundId } = useParams<{ fundId: string }>();
  const { language } = useAppPreferences();
  const [activeLayer, setActiveLayer] = useState<MemoryLayer>("long_term");
  const [viewMode, setViewMode] = useState<ViewMode>("content");
  const [entriesByLayer, setEntriesByLayer] = useState<Record<MemoryLayer, MemoryEntry[]>>({
    long_term: [],
    daily: [],
    dreams: [],
    agent: [],
    analysis: [],
  });
  const [agents, setAgents] = useState<TeamAgent[]>([]);
  const [selectedAgentId, setSelectedAgentId] = useState<string>("");
  const [selectedEntry, setSelectedEntry] = useState<MemoryEntry | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [query, setQuery] = useState("");
  const [focus, setFocus] = useState<MemoryFocus>("all");

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            loading: "Loading memory center...",
            retry: "Retry",
            missingFundId: "Missing fundId",
            loadFailed: "Failed to load memory center",
            title: "Memory center",
            subtitle:
              "Browse long-term memory, daily logs, distilled insights, and collaborative records for the fund, then filter by member, timeline, and statistics.",
            memberFilter: "Member filter",
            allMembers: "All members",
            focusLabel: "Focus",
            focusOptions: {
              all: "All memory",
              market: "Market research",
            } as Record<MemoryFocus, string>,
            marketCoverage: "Market coverage",
            marketCoverageSubtitle: "Track how much of the team memory is now grounded in quotes, news, and research snapshots.",
            marketEntries: "Market entries",
            marketTags: "Market tags",
            latestMarketEntry: "Latest market entry",
            noMarketEntry: "None yet",
            researchBadge: "Research",
            newsBadge: "News",
            quoteBadge: "Quote",
            signalBadge: "Signal",
            viewModes: {
              content: "Content",
              search: "Search",
              timeline: "Timeline",
              stats: "Stats",
            } as Record<ViewMode, string>,
            statsEmptyTitle: "No memory data to analyze yet",
            statsEmptyDescription:
              "Once workflows, research discussions, or team collaboration produce records, this view will summarize layer distribution, active members, and key themes.",
            totalEntries: "Total entries",
            mostActiveMember: "Most active member",
            layerDistribution: "Layer distribution",
            topTags: "Top tags",
            noTags: "No tags yet",
            searchPlaceholder: "Search memory by title or content...",
            foundResults: "results found",
            noSearchResults: "No matching memory found. Try different keywords or switch back to content view.",
            timelineTitle: "Daily log timeline",
            timelineEmpty: "No daily memory exists yet. Once day-to-day runs and collaboration records appear, key events will show up here in order.",
            contentEmpty: "This layer has no memory entries yet. Related collaboration records will settle here automatically later.",
            noSelectedEntry: "There is no memory to display in this layer yet. Switch to another layer or wait for new records.",
            memberPrefix: "Member:",
            unassignedMember: "Unassigned member",
            noActiveMember: "None yet",
            layerLabels: {
              long_term: "Long-term memory",
              daily: "Daily logs",
              dreams: "Insights",
              agent: "Collaboration memory",
              analysis: "Market analysis",
            } as Record<MemoryLayer, string>,
            layerIcons: {
              long_term: "🧠",
              daily: "📅",
              dreams: "💭",
              agent: "🤖",
              analysis: "📈",
            } as Record<MemoryLayer, string>,
          }
        : {
            loading: "正在加载记忆中心...",
            retry: "重试",
            missingFundId: "缺少 fundId",
            loadFailed: "加载记忆中心失败",
            title: "记忆中心",
            subtitle: "查看基金的长期记忆、日记日志、洞察沉淀与成员协作记录，并按成员筛选内容、时间线与统计。",
            memberFilter: "成员筛选",
            allMembers: "全部成员",
            focusLabel: "查看重点",
            focusOptions: {
              all: "全部记忆",
              market: "市场研究",
            } as Record<MemoryFocus, string>,
            marketCoverage: "市场研究覆盖",
            marketCoverageSubtitle: "快速查看团队记忆里有多少内容已经沉淀为行情、资讯和研究快照。",
            marketEntries: "市场条目",
            marketTags: "市场标签",
            latestMarketEntry: "最近市场条目",
            noMarketEntry: "暂无",
            researchBadge: "研究",
            newsBadge: "资讯",
            quoteBadge: "行情",
            signalBadge: "信号",
            viewModes: {
              content: "内容视图",
              search: "全文搜索",
              timeline: "时间线",
              stats: "统计",
            } as Record<ViewMode, string>,
            statsEmptyTitle: "当前还没有可统计的记忆内容",
            statsEmptyDescription: "等策略流程、研究讨论或成员协作产生记录后，这里会自动汇总条目分布、活跃成员与高频主题。",
            totalEntries: "总条目数",
            mostActiveMember: "最活跃成员",
            layerDistribution: "分层分布",
            topTags: "高频标签",
            noTags: "暂无标签统计",
            searchPlaceholder: "按标题、内容关键词搜索记忆...",
            foundResults: "条结果",
            noSearchResults: "没有匹配的记忆内容，请尝试更换关键词或切到内容视图浏览各层记录。",
            timelineTitle: "日记时间线",
            timelineEmpty: "当前还没有日记层记忆。等日常运行和协作记录产生后，这里会按时间顺序展示关键节点。",
            contentEmpty: "该层还没有记忆条目，后续相关协作内容会自动沉淀到这里。",
            noSelectedEntry: "当前层还没有可展示的记忆内容，请先切换到其他层或等待新的协作记录写入。",
            memberPrefix: "成员：",
            unassignedMember: "未关联成员",
            noActiveMember: "暂无",
            layerLabels: {
              long_term: "长期记忆",
              daily: "日记日志",
              dreams: "洞察沉淀",
              agent: "协作记忆",
              analysis: "市场分析",
            } as Record<MemoryLayer, string>,
            layerIcons: {
              long_term: "🧠",
              daily: "📅",
              dreams: "💭",
              agent: "🤖",
              analysis: "📈",
            } as Record<MemoryLayer, string>,
          },
    [language],
  );

  const layerMeta = useMemo(
    () => ({
      long_term: {
        label: copy.layerLabels.long_term,
        icon: copy.layerIcons.long_term,
        badgeClass: "bg-indigo-100 text-indigo-700",
        tabClass: "border-indigo-500 bg-indigo-50",
      },
      daily: {
        label: copy.layerLabels.daily,
        icon: copy.layerIcons.daily,
        badgeClass: "bg-blue-100 text-blue-700",
        tabClass: "border-blue-500 bg-blue-50",
      },
      dreams: {
        label: copy.layerLabels.dreams,
        icon: copy.layerIcons.dreams,
        badgeClass: "bg-purple-100 text-purple-700",
        tabClass: "border-purple-500 bg-purple-50",
      },
      agent: {
        label: copy.layerLabels.agent,
        icon: copy.layerIcons.agent,
        badgeClass: "bg-emerald-100 text-emerald-700",
        tabClass: "border-emerald-500 bg-emerald-50",
      },
      analysis: {
        label: copy.layerLabels.analysis,
        icon: copy.layerIcons.analysis,
        badgeClass: "bg-amber-100 text-amber-700",
        tabClass: "border-amber-500 bg-amber-50",
      },
    }),
    [copy.layerIcons, copy.layerLabels],
  );

  const displayDate = useCallback(
    (entry: MemoryEntry) => formatDateForLanguage(entry.tradingDate || entry.createdAt.slice(0, 10), language),
    [language],
  );

  const displayTitle = useCallback(
    (entry: MemoryEntry) => entry.title?.trim() || `${layerMeta[entry.layer].label}${language === "en-US" ? " entry" : "条目"}`,
    [language, layerMeta],
  );

  const agentLabels = useMemo(() => {
    const labels: Record<string, string> = {};
    agents.forEach((agent) => {
      labels[agent.id] = agent.name?.trim() || agent.agentId?.trim() || agent.id;
    });
    return labels;
  }, [agents]);

  const displayAgentName = useCallback(
    (agentId: string | undefined) => {
      if (!agentId) {
        return copy.unassignedMember;
      }
      return agentLabels[agentId] ?? agentId;
    },
    [agentLabels, copy.unassignedMember],
  );

  const computeStats = useCallback(
    (entries: MemoryEntry[]): MemoryStats => {
      const agentCounts: Record<string, number> = {};
      const themeCounts: Record<string, number> = {};
      const layerCounts: Record<MemoryLayer, number> = { long_term: 0, daily: 0, dreams: 0, agent: 0, analysis: 0 };

      entries.forEach((entry) => {
        layerCounts[entry.layer] += 1;
        if (entry.agentId) {
          agentCounts[entry.agentId] = (agentCounts[entry.agentId] || 0) + 1;
        }
        (entry.tags ?? []).forEach((tag) => {
          themeCounts[tag] = (themeCounts[tag] || 0) + 1;
        });
      });

      const mostActiveAgentId = Object.entries(agentCounts).sort((a, b) => b[1] - a[1])[0]?.[0];

      return {
        totalEntries: entries.length,
        mostActiveAgent: mostActiveAgentId ? displayAgentName(mostActiveAgentId) : copy.noActiveMember,
        keyThemes: Object.entries(themeCounts)
          .sort((a, b) => b[1] - a[1])
          .slice(0, 6)
          .map(([theme, count]) => ({ theme, count })),
        entriesByLayer: layerCounts,
      };
    },
    [copy.noActiveMember, displayAgentName],
  );

  const loadAgents = useCallback(async () => {
    if (!fundId) {
      return;
    }
    try {
      const response = await apiGet<TeamAgent[]>(`/api/funds/${fundId}/team`);
      setAgents(response ?? []);
    } catch {
      setAgents([]);
    }
  }, [fundId]);

  const loadMemory = useCallback(async () => {
    if (!fundId) {
      setError(copy.missingFundId);
      setLoading(false);
      return;
    }

    setLoading(true);
    setError(null);
    try {
      const layers = Object.keys(layerMeta) as MemoryLayer[];
      const responses = await Promise.all(
        layers.map((layer) => {
          const params = new URLSearchParams({ layer });
          if (selectedAgentId) {
            params.set("agentId", selectedAgentId);
          }
          return apiGet<MemoryContextResponse>(`/api/funds/${fundId}/memory?${params.toString()}`);
        }),
      );

      const nextEntries = responses.reduce(
        (acc, response, index) => {
          const fallbackLayer = layers[index];
          const responseLayer = response.layer ?? fallbackLayer;
          acc[responseLayer] = (response.entries ?? []).sort((a, b) => (b.tradingDate || b.createdAt).localeCompare(a.tradingDate || a.createdAt));
          return acc;
        },
        { long_term: [], daily: [], dreams: [], agent: [], analysis: [] } as Record<MemoryLayer, MemoryEntry[]>,
      );

      setEntriesByLayer(nextEntries);
      setSelectedEntry((current) => {
        if (current) {
          const stillExists = Object.values(nextEntries).flat().find((entry) => entry.id === current.id);
          if (stillExists) {
            return stillExists;
          }
        }
        return nextEntries.analysis[0] ?? nextEntries.long_term[0] ?? nextEntries.daily[0] ?? nextEntries.dreams[0] ?? nextEntries.agent[0] ?? null;
      });
    } catch (err) {
      setError(formatApiError(err, copy.loadFailed));
    } finally {
      setLoading(false);
    }
  }, [copy.loadFailed, copy.missingFundId, fundId, layerMeta, selectedAgentId]);

  useEffect(() => {
    void loadAgents();
  }, [loadAgents]);

  useEffect(() => {
    void loadMemory();
  }, [loadMemory]);

  const allEntries = useMemo(() => Object.values(entriesByLayer).flat(), [entriesByLayer]);
  const scopedEntries = useMemo(() => (focus === "market" ? allEntries.filter((entry) => isMarketMemoryEntry(entry)) : allEntries), [allEntries, focus]);
  const stats = useMemo(() => computeStats(scopedEntries), [computeStats, scopedEntries]);
  const layerEntries = useMemo(
    () => (focus === "market" ? entriesByLayer[activeLayer].filter((entry) => isMarketMemoryEntry(entry)) : entriesByLayer[activeLayer]),
    [activeLayer, entriesByLayer, focus],
  );
  const searchResults = useMemo(() => searchEntries(scopedEntries, query, displayTitle), [displayTitle, query, scopedEntries]);
  const timelineEntries = useMemo(
    () => (focus === "market" ? entriesByLayer.daily.filter((entry) => isMarketMemoryEntry(entry)) : entriesByLayer.daily),
    [entriesByLayer.daily, focus],
  );
  const marketEntries = useMemo(() => allEntries.filter((entry) => isMarketMemoryEntry(entry)), [allEntries]);
  const marketTags = useMemo(() => {
    const counts = new Map<string, number>();
    marketEntries.forEach((entry) => {
      (entry.tags ?? []).forEach((tag) => {
        counts.set(tag, (counts.get(tag) ?? 0) + 1);
      });
    });
    return Array.from(counts.entries())
      .sort((a, b) => b[1] - a[1])
      .slice(0, 6);
  }, [marketEntries]);
  const latestMarketEntry = marketEntries[0] ?? null;

  if (loading) {
    return <div className="rounded-xl border border-gray-200 bg-white p-6 text-sm text-gray-500">{copy.loading}</div>;
  }

  if (error) {
    return (
      <div className="rounded-xl border border-red-200 bg-red-50 p-6 text-sm text-red-700">
        <p>{error}</p>
        <button onClick={() => void loadMemory()} className="mt-4 rounded-lg bg-red-600 px-4 py-2 text-sm font-medium text-white hover:bg-red-700">
          {copy.retry}
        </button>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
        <div className="flex flex-col gap-4 lg:flex-row lg:items-center lg:justify-between">
          <div>
            <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
            <p className="mt-2 text-sm text-gray-500">{copy.subtitle}</p>
          </div>
          <div className="flex flex-col gap-3 sm:flex-row sm:items-center">
            <div>
              <label htmlFor="memory-agent-filter" className="mb-1 block text-xs font-medium text-gray-500">{copy.memberFilter}</label>
              <select
                id="memory-agent-filter"
                value={selectedAgentId}
                onChange={(e) => setSelectedAgentId(e.target.value)}
                className="min-w-56 rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
              >
                <option value="">{copy.allMembers}</option>
                {agents.map((agent) => (
                  <option key={agent.id} value={agent.id}>
                    {agentLabels[agent.id]}
                  </option>
                ))}
              </select>
            </div>
            <div>
              <label htmlFor="memory-focus-filter" className="mb-1 block text-xs font-medium text-gray-500">{copy.focusLabel}</label>
              <select
                id="memory-focus-filter"
                value={focus}
                onChange={(e) => setFocus(e.target.value as MemoryFocus)}
                className="min-w-40 rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
              >
                <option value="all">{copy.focusOptions.all}</option>
                <option value="market">{copy.focusOptions.market}</option>
              </select>
            </div>
            <div className="flex rounded-lg bg-gray-100 p-1">
              {(["content", "search", "timeline", "stats"] as ViewMode[]).map((mode) => (
                <button
                  key={mode}
                  onClick={() => setViewMode(mode)}
                  className={`rounded-md px-3 py-1.5 text-xs font-medium transition ${
                    viewMode === mode ? "bg-white text-gray-900 shadow-sm" : "text-gray-500 hover:text-gray-700"
                  }`}
                >
                  {copy.viewModes[mode]}
                </button>
              ))}
            </div>
          </div>
        </div>
      </div>

      <div className="grid grid-cols-1 gap-4 xl:grid-cols-3">
        <div className="rounded-2xl border border-gray-200 bg-white p-5 shadow-sm xl:col-span-2">
          <p className="text-sm font-semibold text-gray-900">{copy.marketCoverage}</p>
          <p className="mt-1 text-sm text-gray-500">{copy.marketCoverageSubtitle}</p>
          <div className="mt-4 grid grid-cols-1 gap-3 sm:grid-cols-3">
            <div className="rounded-xl bg-gray-50 px-4 py-4">
              <p className="text-xs text-gray-500">{copy.marketEntries}</p>
              <p className="mt-1 text-2xl font-semibold text-indigo-600">{formatNumberForLanguage(marketEntries.length, language)}</p>
            </div>
            <div className="rounded-xl bg-gray-50 px-4 py-4">
              <p className="text-xs text-gray-500">{copy.marketTags}</p>
              <p className="mt-1 text-2xl font-semibold text-gray-900">{formatNumberForLanguage(marketTags.length, language)}</p>
            </div>
            <div className="rounded-xl bg-gray-50 px-4 py-4">
              <p className="text-xs text-gray-500">{copy.latestMarketEntry}</p>
              <p className="mt-1 truncate text-sm font-semibold text-gray-900">{latestMarketEntry ? displayTitle(latestMarketEntry) : copy.noMarketEntry}</p>
            </div>
          </div>
        </div>
        <div className="rounded-2xl border border-gray-200 bg-white p-5 shadow-sm">
          <p className="text-sm font-semibold text-gray-900">{copy.marketTags}</p>
          <div className="mt-4 flex flex-wrap gap-2">
            {marketTags.length > 0 ? (
              marketTags.map(([tag, count]) => (
                <span key={tag} className="rounded-full bg-indigo-50 px-2.5 py-1 text-xs font-medium text-indigo-700">
                  #{tag} ({formatNumberForLanguage(count, language)})
                </span>
              ))
            ) : (
              <span className="text-sm text-gray-400">{copy.noTags}</span>
            )}
          </div>
        </div>
      </div>

      {viewMode === "stats" ? (
        allEntries.length === 0 ? (
          <div className="rounded-2xl border border-dashed border-gray-200 bg-white p-8 text-center text-sm text-gray-500 shadow-sm">
            <p className="font-medium text-gray-700">{copy.statsEmptyTitle}</p>
            <p className="mt-2">{copy.statsEmptyDescription}</p>
          </div>
        ) : (
          <div className="space-y-5">
            <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-4">
              <div className="rounded-xl border border-gray-200 bg-white p-4 text-center shadow-sm">
                <p className="text-2xl font-bold text-indigo-600">{formatNumberForLanguage(stats.totalEntries, language)}</p>
                <p className="mt-1 text-xs text-gray-500">{copy.totalEntries}</p>
              </div>
              <div className="rounded-xl border border-gray-200 bg-white p-4 text-center shadow-sm">
                <p className="break-all text-sm font-bold text-emerald-600">{stats.mostActiveAgent}</p>
                <p className="mt-1 text-xs text-gray-500">{copy.mostActiveMember}</p>
              </div>
              <div className="rounded-xl border border-gray-200 bg-white p-4 shadow-sm">
                <p className="mb-2 text-xs font-medium text-gray-500">{copy.layerDistribution}</p>
                {(Object.keys(stats.entriesByLayer) as MemoryLayer[]).map((layer) => (
                  <div key={layer} className="mb-1 flex justify-between text-xs">
                    <span className="text-gray-600">
                      {layerMeta[layer].icon} {layerMeta[layer].label}
                    </span>
                    <span className="font-semibold text-gray-800">{formatNumberForLanguage(stats.entriesByLayer[layer], language)}</span>
                  </div>
                ))}
              </div>
              <div className="rounded-xl border border-gray-200 bg-white p-4 shadow-sm">
                <p className="mb-2 text-xs font-medium text-gray-500">{copy.topTags}</p>
                <div className="flex flex-wrap gap-1.5">
                  {stats.keyThemes.length > 0 ? (
                    stats.keyThemes.map((theme) => (
                      <span key={theme.theme} className="rounded-full bg-gray-100 px-2 py-0.5 text-[10px] font-medium text-gray-600">
                        {theme.theme} ({formatNumberForLanguage(theme.count, language)})
                      </span>
                    ))
                  ) : (
                    <span className="text-xs text-gray-400">{copy.noTags}</span>
                  )}
                </div>
              </div>
            </div>
          </div>
        )
      ) : null}

      {viewMode === "search" ? (
        <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
          <div className="relative mb-4">
            <input
              type="text"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder={copy.searchPlaceholder}
              className="w-full rounded-xl border border-gray-300 px-4 py-2.5 pl-10 text-sm outline-none focus:border-indigo-500"
            />
            <svg className="absolute left-3 top-3 h-4 w-4 text-gray-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M21 21l-6-6m2-5a7 7 0 11-14 0 7 7 0 0114 0z" />
            </svg>
          </div>

          {query.trim() ? (
            <p className="mb-3 text-xs text-gray-500">
              {formatNumberForLanguage(searchResults.length, language)} {copy.foundResults}
            </p>
          ) : null}

          <div className="space-y-2">
            {searchResults.map((result) => (
              <button
                key={result.entry.id}
                onClick={() => {
                  setSelectedEntry(result.entry);
                  setActiveLayer(result.entry.layer);
                  setViewMode("content");
                }}
                className="w-full rounded-lg border border-gray-200 p-3 text-left transition hover:border-indigo-300 hover:bg-gray-50"
              >
                <div className="mb-1 flex items-center gap-2">
                  <span className={`rounded-full px-2 py-0.5 text-[10px] font-medium ${layerMeta[result.entry.layer].badgeClass}`}>
                    {layerMeta[result.entry.layer].icon} {layerMeta[result.entry.layer].label}
                  </span>
                  <span className="text-xs text-gray-400">{displayDate(result.entry)}</span>
                  {result.entry.agentId ? <span className="text-xs text-gray-400">{displayAgentName(result.entry.agentId)}</span> : null}
                </div>
                <p className="text-sm font-semibold text-gray-800">{displayTitle(result.entry)}</p>
                <p className="mt-1 text-xs leading-relaxed text-gray-500">{highlightMatch(result.matchedSnippet, query)}</p>
              </button>
            ))}
          </div>

          {query.trim() && searchResults.length === 0 ? (
            <div className="rounded-xl border border-dashed border-gray-200 bg-gray-50 px-4 py-10 text-center text-sm text-gray-400">
              {copy.noSearchResults}
            </div>
          ) : null}
        </div>
      ) : null}

      {viewMode === "timeline" ? (
        <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
          <h2 className="mb-4 text-lg font-bold text-gray-900">{copy.timelineTitle}</h2>
          {timelineEntries.length === 0 ? (
            <div className="rounded-xl border border-dashed border-gray-200 bg-gray-50 px-4 py-10 text-center text-sm text-gray-400">
              {copy.timelineEmpty}
            </div>
          ) : (
            <div className="space-y-0">
              {timelineEntries.map((entry, index) => (
                <div key={entry.id} className="relative pb-4 pl-8 last:pb-0">
                  {index < timelineEntries.length - 1 ? <span className="absolute left-[11px] top-6 h-full w-0.5 bg-gray-200" /> : null}
                  <span className="absolute left-1 top-1.5 h-4 w-4 rounded-full border-2 border-blue-400 bg-blue-100" />
                  <p className="text-xs font-mono text-gray-400">{displayDate(entry)}</p>
                  <button
                    onClick={() => {
                      setSelectedEntry(entry);
                      setViewMode("content");
                    }}
                    className="mt-0.5 text-left text-sm font-semibold text-gray-700 hover:text-indigo-600"
                  >
                    {displayTitle(entry)}
                  </button>
                  {entry.agentId ? <p className="mt-1 text-xs text-gray-400">{displayAgentName(entry.agentId)}</p> : null}
                </div>
              ))}
            </div>
          )}
        </div>
      ) : null}

      {viewMode === "content" ? (
        <div className="grid grid-cols-1 gap-5 xl:grid-cols-[288px_minmax(0,1fr)]">
          <div className="space-y-3">
            <div className="overflow-hidden rounded-xl border border-gray-200 bg-white">
              {(Object.keys(layerMeta) as MemoryLayer[]).map((layer) => {
                const meta = layerMeta[layer];
                const count = entriesByLayer[layer].length;
                return (
                  <button
                    key={layer}
                    onClick={() => {
                      setActiveLayer(layer);
                      setSelectedEntry(entriesByLayer[layer][0] ?? null);
                      setViewMode("content");
                    }}
                    className={`flex w-full items-center justify-between border-l-4 px-4 py-3 text-left transition hover:bg-gray-50 ${
                      activeLayer === layer ? meta.tabClass : "border-transparent"
                    }`}
                  >
                    <span className="text-sm font-medium text-gray-700">
                      {meta.icon} {meta.label}
                    </span>
                    <span className="rounded-full bg-gray-100 px-2 py-0.5 text-xs text-gray-500">{formatNumberForLanguage(count, language)}</span>
                  </button>
                );
              })}
            </div>

            <div className="max-h-[60vh] divide-y divide-gray-100 overflow-y-auto rounded-xl border border-gray-200 bg-white">
              {layerEntries.map((entry) => (
                <button
                  key={entry.id}
                  onClick={() => setSelectedEntry(entry)}
                  className={`w-full px-4 py-3 text-left transition hover:bg-gray-50 ${selectedEntry?.id === entry.id ? "bg-indigo-50" : ""}`}
                >
                  <p className="truncate text-sm font-semibold text-gray-800">{displayTitle(entry)}</p>
                  <div className="mt-1 flex items-center gap-2 text-[10px] text-gray-400">
                    <span>{displayDate(entry)}</span>
                    {entry.agentId ? <span className="truncate">{displayAgentName(entry.agentId)}</span> : null}
                  </div>
                </button>
              ))}
              {layerEntries.length === 0 ? (
                <div className="px-4 py-8 text-center text-sm text-gray-400">{copy.contentEmpty}</div>
              ) : null}
            </div>
          </div>

          <div>
            {selectedEntry ? (
              <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
                <div className="mb-4 flex flex-wrap items-center gap-3">
                  <span className={`rounded-full px-2.5 py-1 text-xs font-medium ${layerMeta[selectedEntry.layer].badgeClass}`}>
                    {layerMeta[selectedEntry.layer].icon} {layerMeta[selectedEntry.layer].label}
                  </span>
                  <span className="text-xs text-gray-400">{displayDate(selectedEntry)}</span>
                  {selectedEntry.agentId ? (
                    <span className="rounded-full bg-emerald-100 px-2 py-0.5 text-xs font-medium text-emerald-700">
                      {copy.memberPrefix} {displayAgentName(selectedEntry.agentId)}
                    </span>
                  ) : null}
                  {marketMemoryKind(selectedEntry) === "research" ? <span className="rounded-full bg-indigo-100 px-2 py-0.5 text-xs font-medium text-indigo-700">{copy.researchBadge}</span> : null}
                  {marketMemoryKind(selectedEntry) === "news" ? <span className="rounded-full bg-amber-100 px-2 py-0.5 text-xs font-medium text-amber-700">{copy.newsBadge}</span> : null}
                  {marketMemoryKind(selectedEntry) === "quote" ? <span className="rounded-full bg-sky-100 px-2 py-0.5 text-xs font-medium text-sky-700">{copy.quoteBadge}</span> : null}
                  {marketMemoryKind(selectedEntry) === "signal" ? <span className="rounded-full bg-purple-100 px-2 py-0.5 text-xs font-medium text-purple-700">{copy.signalBadge}</span> : null}
                </div>
                <h2 className="text-2xl font-bold text-gray-900">{displayTitle(selectedEntry)}</h2>
                {(selectedEntry.tags ?? []).length > 0 ? (
                  <div className="mt-3 flex flex-wrap gap-1.5">
                    {(selectedEntry.tags ?? []).map((tag) => (
                      <span key={tag} className="rounded-full bg-gray-100 px-2 py-0.5 text-[10px] font-medium text-gray-600">
                        #{tag}
                      </span>
                    ))}
                  </div>
                ) : null}
                <div className="mt-4 border-t border-gray-100 pt-4">
                  <MarkdownBlock text={selectedEntry.content} />
                </div>
              </div>
            ) : (
              <div className="flex h-80 items-center justify-center rounded-2xl border border-dashed border-gray-200 bg-white px-6 text-center text-sm text-gray-400">
                {copy.noSelectedEntry}
              </div>
            )}
          </div>
        </div>
      ) : null}
    </div>
  );
};

export default MemoryCenter;
