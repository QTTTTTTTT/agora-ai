import React, { useCallback, useEffect, useState } from "react";
import { ReflectionItem, fetchReflections, formatApiError } from "../lib/api";
import { formatDateTimeForLanguage, type AppLanguage } from "../lib/preferences";

interface ReflectionsPanelProps {
  fundId?: string;
  language: AppLanguage;
}

interface PanelCopy {
  title: string;
  subtitle: string;
  loading: string;
  empty: string;
  emptyHint: string;
  errorPrefix: string;
  retry: string;
  themeLabel: string;
  loadFailed: string;
  unavailableTitle: string;
  unavailableHint: string;
}

const COPY: Record<AppLanguage, PanelCopy> = {
  "zh-CN": {
    title: "长期反思 / Long-term Reflections",
    subtitle: "由 memory.Reflect 在每日复盘后自动产出，按主题聚合最近 30 天的学习要点。",
    loading: "加载中…",
    empty: "暂无长期反思",
    emptyHint: "需要至少 3 条同主题的日学习才能触发一次反思；下一次复盘后会再尝试。",
    errorPrefix: "加载失败：",
    retry: "重试",
    themeLabel: "主题",
    loadFailed: "无法加载长期反思",
    unavailableTitle: "服务未配置",
    unavailableHint: "当前服务端未启用长期反思功能（缺少 LLM Runtime 或 ReflectionService 未挂载）。",
  },
  "en-US": {
    title: "Long-term Reflections",
    subtitle: "Auto-produced by memory.Reflect after each daily review, clustering the last 30 days of learnings by theme.",
    loading: "Loading…",
    empty: "No long-term reflections yet",
    emptyHint: "A theme needs at least 3 daily learnings before a reflection is emitted; the next daily review will try again.",
    errorPrefix: "Load failed: ",
    retry: "Retry",
    themeLabel: "Theme",
    loadFailed: "Failed to load long-term reflections",
    unavailableTitle: "Service unavailable",
    unavailableHint: "Long-term reflections are not configured on this server (LLM runtime missing or ReflectionService not wired).",
  },
};

const ReflectionsPanel: React.FC<ReflectionsPanelProps> = ({ fundId, language }) => {
  const copy = COPY[language];
  const [items, setItems] = useState<ReflectionItem[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [unavailable, setUnavailable] = useState(false);

  const load = useCallback(async () => {
    if (!fundId) {
      return;
    }
    setLoading(true);
    setError(null);
    setUnavailable(false);
    try {
      const resp = await fetchReflections(fundId);
      setItems(resp.items ?? []);
    } catch (err) {
      const message = formatApiError(err, copy.loadFailed);
      // The handler returns 503 when the server has no ReflectionService
      // wired — surface a softer "not configured" message instead of a
      // generic error to make troubleshooting easier in dev environments.
      if (typeof message === "string" && message.toLowerCase().includes("reflection_unavailable")) {
        setUnavailable(true);
      } else {
        setError(message);
      }
    } finally {
      setLoading(false);
    }
  }, [copy.loadFailed, fundId]);

  useEffect(() => {
    void load();
  }, [load]);

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
          disabled={loading || !fundId}
        >
          {copy.retry}
        </button>
      </div>

      {loading ? <p className="mt-4 text-sm text-gray-500">{copy.loading}</p> : null}

      {error ? (
        <div className="mt-4 rounded-md border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700">
          {copy.errorPrefix}
          {error}
        </div>
      ) : null}

      {unavailable ? (
        <div className="mt-4 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-700">
          <strong>{copy.unavailableTitle}.</strong> {copy.unavailableHint}
        </div>
      ) : null}

      {!loading && !error && !unavailable ? (
        items.length === 0 ? (
          <div className="mt-4 rounded-md border border-dashed border-gray-200 px-4 py-6 text-center text-sm text-gray-500">
            <p>{copy.empty}</p>
            <p className="mt-1 text-xs text-gray-400">{copy.emptyHint}</p>
          </div>
        ) : (
          <ol className="mt-4 space-y-3">
            {items.map((item) => (
              <li key={item.id} className="rounded-lg border border-gray-200 p-4">
                <div className="flex flex-wrap items-baseline justify-between gap-2">
                  <div className="flex items-center gap-2">
                    <span className="rounded-full bg-indigo-100 px-2 py-0.5 text-[11px] font-semibold uppercase tracking-wide text-indigo-700">
                      {copy.themeLabel}: {item.theme || "general"}
                    </span>
                    {(item.tags ?? []).slice(0, 4).map((tag) => (
                      <span key={tag} className="rounded-full bg-gray-100 px-2 py-0.5 text-[11px] text-gray-600">
                        {tag}
                      </span>
                    ))}
                  </div>
                  <span className="text-xs text-gray-400">{formatDateTimeForLanguage(item.createdAt, language)}</span>
                </div>
                <p className="mt-3 whitespace-pre-line text-sm text-gray-800">{item.content}</p>
              </li>
            ))}
          </ol>
        )
      ) : null}
    </section>
  );
};

export default ReflectionsPanel;
