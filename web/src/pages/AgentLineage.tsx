import React, { useCallback, useEffect, useMemo, useState } from "react";
import { useParams } from "react-router-dom";
import { AgentLineageNode, AgentLineageTree, apiGet, fetchAgentLineage, formatApiError } from "../lib/api";
import { formatDateTimeForLanguage, useAppPreferences } from "../lib/preferences";

interface TeamAgent {
  id: string;
  agentId?: string;
  name?: string;
  role: string;
  focus?: string;
}

function roleLabel(value: string | undefined, language: "zh-CN" | "en-US"): string {
  const labels: Record<string, { zh: string; en: string }> = {
    pm: { zh: "组合经理", en: "PM" },
    researcher: { zh: "研究员", en: "Researcher" },
    trader: { zh: "交易员", en: "Trader" },
    risk: { zh: "风控", en: "Risk" },
  };
  const matched = labels[value ?? ""];
  return matched ? (language === "en-US" ? matched.en : matched.zh) : (value || "-");
}

function viaLabel(value: string | undefined, language: "zh-CN" | "en-US"): string {
  const labels: Record<string, { zh: string; en: string }> = {
    buyout: { zh: "市场买断", en: "Marketplace buyout" },
    subscribe: { zh: "订阅继承", en: "Subscription" },
    abtest_clone: { zh: "A/B 克隆", en: "A/B clone" },
    manual_copy: { zh: "手动复制", en: "Manual copy" },
  };
  const matched = labels[value ?? ""];
  return matched ? (language === "en-US" ? matched.en : matched.zh) : (value || (language === "en-US" ? "Origin" : "原创"));
}

const AgentLineage: React.FC = () => {
  const { fundId } = useParams<{ fundId: string }>();
  const { language } = useAppPreferences();
  const [agents, setAgents] = useState<TeamAgent[]>([]);
  const [selectedAgentId, setSelectedAgentId] = useState("");
  const [tree, setTree] = useState<AgentLineageTree | null>(null);
  const [loading, setLoading] = useState(true);
  const [treeLoading, setTreeLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            title: "Lineage graph",
            subtitle: "Visualize where a team member came from, which marketplace listing produced it, and whether the ancestry could trigger anti-matryoshka review.",
            loading: "Loading lineage graph...",
            loadFailed: "Failed to load lineage graph",
            missingFundId: "Missing fundId",
            retry: "Retry",
            members: "Team members",
            sourceTree: "Source tree",
            ancestorCount: "Ancestors",
            maxDepth: "Max depth",
            risk: "Anti-matryoshka risk",
            riskYes: "Review required",
            riskNo: "No loop detected",
            owner: "Owner",
            sourceListing: "Source listing",
            derivedVia: "Derived via",
            createdAt: "Created",
            original: "Original / no recorded parent",
            empty: "This agent has no recorded marketplace or clone ancestry yet.",
          }
        : {
            title: "血缘来源图",
            subtitle: "可视化团队成员来源、对应市场挂牌，以及是否存在 anti-matryoshka 套娃回购风险。",
            loading: "正在加载血缘图...",
            loadFailed: "加载血缘图失败",
            missingFundId: "缺少 fundId",
            retry: "重试",
            members: "团队成员",
            sourceTree: "来源树",
            ancestorCount: "祖先数量",
            maxDepth: "最大深度",
            risk: "套娃风险",
            riskYes: "需要复核",
            riskNo: "未发现闭环",
            owner: "所有者",
            sourceListing: "来源挂牌",
            derivedVia: "来源方式",
            createdAt: "创建时间",
            original: "原创 / 暂无记录父节点",
            empty: "该 Agent 暂无已记录的市场或克隆来源。",
          },
    [language],
  );

  const loadTree = useCallback(
    async (agentId: string) => {
      if (!agentId) {
        setTree(null);
        return;
      }
      setTreeLoading(true);
      setError(null);
      try {
        setTree(await fetchAgentLineage(agentId));
      } catch (err) {
        setError(formatApiError(err, copy.loadFailed));
      } finally {
        setTreeLoading(false);
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
      const first = selectedAgentId || team[0]?.agentId || team[0]?.id || "";
      setSelectedAgentId(first);
      if (first) {
        await loadTree(first);
      }
    } catch (err) {
      setError(formatApiError(err, copy.loadFailed));
    } finally {
      setLoading(false);
    }
  }, [copy.loadFailed, copy.missingFundId, fundId, loadTree, selectedAgentId]);

  useEffect(() => {
    void load();
  }, [load]);

  if (loading) {
    return <div className="rounded-xl bg-white p-6 text-sm text-gray-500 shadow-sm">{copy.loading}</div>;
  }

  return (
    <div className="space-y-6">
      <div className="rounded-2xl bg-white p-6 shadow-sm">
        <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
        <p className="mt-2 max-w-3xl text-sm text-gray-500">{copy.subtitle}</p>
      </div>

      {error ? (
        <div className="rounded-xl border border-red-200 bg-red-50 p-4 text-sm text-red-700">
          <p>{error}</p>
          <button onClick={() => void load()} className="mt-3 rounded-md bg-red-600 px-3 py-1.5 text-xs font-semibold text-white hover:bg-red-700">{copy.retry}</button>
        </div>
      ) : null}

      <div className="grid gap-6 lg:grid-cols-[300px_1fr]">
        <aside className="rounded-xl bg-white p-4 shadow-sm">
          <h2 className="text-sm font-semibold text-gray-900">{copy.members}</h2>
          <div className="mt-4 space-y-2">
            {agents.map((agent) => {
              const agentId = agent.agentId ?? agent.id;
              const active = agentId === selectedAgentId;
              return (
                <button
                  key={agentId}
                  onClick={() => {
                    setSelectedAgentId(agentId);
                    void loadTree(agentId);
                  }}
                  className={`w-full rounded-lg border p-3 text-left transition ${active ? "border-indigo-500 bg-indigo-50" : "border-gray-200 hover:border-indigo-200 hover:bg-gray-50"}`}
                >
                  <p className="text-sm font-semibold text-gray-900">{agent.name || roleLabel(agent.role, language)}</p>
                  <p className="mt-1 text-xs text-gray-500">{roleLabel(agent.role, language)}{agent.focus ? ` · ${agent.focus}` : ""}</p>
                </button>
              );
            })}
          </div>
        </aside>

        <main className="space-y-5 rounded-xl bg-white p-5 shadow-sm">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <h2 className="text-lg font-semibold text-gray-900">{copy.sourceTree}</h2>
            {tree ? (
              <div className="flex flex-wrap gap-2 text-xs">
                <span className="rounded-full bg-gray-100 px-2.5 py-1 text-gray-600">{copy.ancestorCount}: {tree.ancestorCount}</span>
                <span className="rounded-full bg-gray-100 px-2.5 py-1 text-gray-600">{copy.maxDepth}: {tree.maxDepth}</span>
                <span className={`rounded-full px-2.5 py-1 font-semibold ${tree.matryoshkaRisk ? "bg-red-50 text-red-700" : "bg-emerald-50 text-emerald-700"}`}>
                  {copy.risk}: {tree.matryoshkaRisk ? copy.riskYes : copy.riskNo}
                </span>
              </div>
            ) : null}
          </div>
          {treeLoading ? <p className="text-sm text-gray-500">{copy.loading}</p> : null}
          {tree?.riskExplanation ? <p className="rounded-lg bg-red-50 p-3 text-sm text-red-700">{tree.riskExplanation}</p> : null}
          {tree ? <LineageNodeCard node={tree.root} language={language} copy={copy} root /> : null}
        </main>
      </div>
    </div>
  );
};

const LineageNodeCard: React.FC<{
  node: AgentLineageNode;
  language: "zh-CN" | "en-US";
  copy: Record<string, string>;
  root?: boolean;
}> = ({ node, language, copy, root }) => {
  const ancestors = node.ancestors ?? [];
  return (
    <div className={root ? "" : "border-l-2 border-indigo-100 pl-4"}>
      <div className="rounded-xl border border-gray-200 bg-white p-4 shadow-sm">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div>
            <h3 className="text-sm font-semibold text-gray-900">{node.agentName || node.agentId}</h3>
            <p className="mt-1 text-xs text-gray-500">{roleLabel(node.role, language)}{node.focus ? ` · ${node.focus}` : ""}</p>
          </div>
          <span className="rounded-full bg-indigo-50 px-2.5 py-1 text-xs font-medium text-indigo-700">{viaLabel(node.derivedVia, language)}</span>
        </div>
        <div className="mt-3 grid gap-2 text-xs text-gray-500 sm:grid-cols-2">
          <span>Agent: {node.agentId}</span>
          {node.ownerUserId ? <span>{copy.owner}: {node.ownerUserId}</span> : null}
          {node.sourceListingId ? <span>{copy.sourceListing}: {node.sourceListingId}</span> : null}
          {node.createdAt ? <span>{copy.createdAt}: {formatDateTimeForLanguage(node.createdAt, language)}</span> : null}
        </div>
      </div>
      {ancestors.length === 0 && root ? <p className="mt-4 rounded-lg bg-gray-50 p-3 text-sm text-gray-500">{copy.empty}</p> : null}
      {ancestors.length > 0 ? (
        <div className="mt-4 space-y-4">
          {ancestors.map((ancestor) => <LineageNodeCard key={`${node.agentId}-${ancestor.agentId}`} node={ancestor} language={language} copy={copy} />)}
        </div>
      ) : null}
    </div>
  );
};

export default AgentLineage;
