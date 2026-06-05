import React, { useCallback, useEffect, useMemo, useState } from "react";
import { useParams } from "react-router-dom";
import { exportFundAuditLogsCSV, fetchFundAuditLogs, formatApiError, type AuditLogEntry } from "../lib/api";
import { formatDateTimeForLanguage, useAppPreferences } from "../lib/preferences";
import { VirtualList } from "../components/VirtualList";
import { useListSearch } from "../lib/useListSearch";

function humanize(value?: string): string {
  if (!value) return "-";
  return value.replace(/[_-]+/g, " ").replace(/\b\w/g, (char) => char.toUpperCase());
}

const AuditLog: React.FC = () => {
  const { fundId } = useParams<{ fundId: string }>();
  const { language } = useAppPreferences();
  const [entries, setEntries] = useState<AuditLogEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [exporting, setExporting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            title: "Unified audit log",
            subtitle: "Review fund-scoped data access, marketplace snapshots, memory reads, and other auditable actions in one timeline.",
            loading: "Loading audit trail...",
            loadError: "Failed to load audit logs",
            exportError: "Failed to export audit logs",
            retry: "Retry",
            exportCsv: "Export CSV",
            exporting: "Exporting...",
            emptyTitle: "No audit events yet",
            emptyDescription: "Auditable events will appear here after protected data is read, exported, snapshotted, or shared.",
            columns: { time: "Time", action: "Action", resource: "Resource", details: "Details" },
            searchPlaceholder: "Search by action, resource, or detail…",
            searchEmpty: "No entries match your search.",
            matchSummary: "Showing {{matched}} of {{total}} entries",
          }
        : {
            title: "统一审计日志",
            subtitle: "在一条时间线中查看基金相关的数据访问、市场快照、记忆读取和其他可审计动作。",
            loading: "正在加载审计轨迹...",
            loadError: "加载审计日志失败",
            exportError: "导出审计日志失败",
            retry: "重试",
            exportCsv: "导出 CSV",
            exporting: "导出中...",
            emptyTitle: "暂无审计事件",
            emptyDescription: "当受保护数据被读取、导出、生成快照或共享后，相关事件会出现在这里。",
            columns: { time: "时间", action: "动作", resource: "资源", details: "详情" },
            searchPlaceholder: "搜索动作、资源或详情…",
            searchEmpty: "未找到匹配的审计事件。",
            matchSummary: "共 {{total}} 条，匹配 {{matched}} 条",
          },
    [language],
  );

  const load = useCallback(async () => {
    if (!fundId) return;
    setLoading(true);
    setError(null);
    try {
      // 500 row cap (was 100) — the table is now virtualized via
      // <VirtualList>, so DOM cost stays O(visible-rows) regardless
      // of total entries returned. Server-side cap on this endpoint
      // is enforced separately and remains the authoritative ceiling.
      const response = await fetchFundAuditLogs(fundId, 500);
      setEntries(response.entries ?? []);
    } catch (err) {
      setError(formatApiError(err, copy.loadError));
    } finally {
      setLoading(false);
    }
  }, [copy.loadError, fundId]);

  useEffect(() => {
    void load();
  }, [load]);

  // Client-side debounced search across action, resource type / id,
  // and a stringified version of the JSON detail. The detail
  // serialisation is cheap because we only do it for the visible
  // entries (≤500); for larger volumes future work can move this
  // to a server-side ?q= parameter.
  const search = useListSearch(entries, (entry) =>
    [
      entry.action,
      entry.resourceType,
      entry.resourceId,
      JSON.stringify(entry.details ?? {}),
    ]
      .filter(Boolean)
      .join(" "),
  );

  const handleExport = useCallback(async () => {
    if (!fundId || exporting) return;
    setExporting(true);
    setError(null);
    try {
      const csv = await exportFundAuditLogsCSV(fundId, 200);
      const blob = new Blob([csv], { type: "text/csv;charset=utf-8" });
      const url = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = url;
      link.download = `audit-log-${fundId}.csv`;
      document.body.appendChild(link);
      link.click();
      link.remove();
      URL.revokeObjectURL(url);
      await load();
    } catch (err) {
      setError(formatApiError(err, copy.exportError));
    } finally {
      setExporting(false);
    }
  }, [copy.exportError, exporting, fundId, load]);

  return (
    <div className="space-y-6">
      <div className="rounded-2xl bg-gradient-to-r from-slate-900 to-slate-700 p-6 text-white shadow-sm">
        <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
          <div>
            <p className="text-xs font-semibold uppercase tracking-[0.3em] text-slate-300">Audit</p>
            <h1 className="mt-2 text-2xl font-bold">{copy.title}</h1>
            <p className="mt-2 max-w-3xl text-sm text-slate-200">{copy.subtitle}</p>
          </div>
          <button
            onClick={() => void handleExport()}
            disabled={exporting}
            className="inline-flex items-center justify-center rounded-lg bg-white px-4 py-2 text-sm font-semibold text-slate-800 shadow-sm transition hover:bg-slate-100 disabled:cursor-not-allowed disabled:opacity-60"
          >
            {exporting ? copy.exporting : copy.exportCsv}
          </button>
        </div>
      </div>

      {loading ? (
        <div className="rounded-xl border border-gray-200 bg-white p-8 text-center text-sm text-gray-500 shadow-sm">{copy.loading}</div>
      ) : error ? (
        <div className="rounded-xl border border-red-100 bg-red-50 p-6 text-sm text-red-700">
          <p>{error}</p>
          <button onClick={() => void load()} className="mt-3 rounded-lg bg-red-600 px-4 py-2 text-white hover:bg-red-700">
            {copy.retry}
          </button>
        </div>
      ) : entries.length === 0 ? (
        <div className="rounded-xl border border-dashed border-gray-300 bg-white p-10 text-center shadow-sm">
          <h2 className="text-lg font-semibold text-gray-900">{copy.emptyTitle}</h2>
          <p className="mt-2 text-sm text-gray-500">{copy.emptyDescription}</p>
        </div>
      ) : (
        // Virtualized via <VirtualList>: only ~12 rows are mounted at
        // any time regardless of how many entries the API returned.
        // Header is rendered as a sibling div (CSS Grid columns
        // matching the row layout) so it sticks above the scroll
        // viewport — react-window doesn't compose with <thead>/<tbody>
        // because rows must be absolutely positioned.
        <div className="space-y-2">
          <div className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
            <input
              type="search"
              value={search.query}
              onChange={(e) => search.setQuery(e.target.value)}
              placeholder={copy.searchPlaceholder}
              aria-label={copy.searchPlaceholder}
              className="w-full max-w-md rounded-lg border border-gray-200 bg-white px-3 py-2 text-sm shadow-sm placeholder:text-gray-400 focus:border-indigo-400 focus:outline-none focus:ring-2 focus:ring-indigo-500/20"
            />
            <p className="text-xs text-gray-500">
              {copy.matchSummary
                .replace("{{matched}}", search.matchCount.toString())
                .replace("{{total}}", entries.length.toString())}
            </p>
          </div>

          {search.filtered.length === 0 ? (
            <div className="rounded-xl border border-dashed border-gray-300 bg-white p-8 text-center text-sm text-gray-500 shadow-sm">
              {copy.searchEmpty}
            </div>
          ) : (
        <div className="overflow-hidden rounded-xl border border-gray-200 bg-white shadow-sm">
          <div className="grid grid-cols-[180px_140px_minmax(180px,1fr)_minmax(280px,2fr)] bg-gray-50 px-4 py-3 text-xs uppercase tracking-wider text-gray-500">
            <div>{copy.columns.time}</div>
            <div>{copy.columns.action}</div>
            <div>{copy.columns.resource}</div>
            <div>{copy.columns.details}</div>
          </div>
          <VirtualList
            items={search.filtered}
            itemHeight={120}
            height={Math.min(720, search.filtered.length * 120 + 8)}
            itemKey={(_, item) => item.id}
            renderRow={(entry) => (
              <div className="grid grid-cols-[180px_140px_minmax(180px,1fr)_minmax(280px,2fr)] items-start gap-x-4 border-b border-gray-100 bg-white px-4 py-3 hover:bg-gray-50">
                <div className="whitespace-nowrap text-sm text-gray-500">{formatDateTimeForLanguage(entry.createdAt, language)}</div>
                <div>
                  <span className="rounded-full bg-blue-50 px-2.5 py-1 text-xs font-medium text-blue-700">{humanize(entry.action)}</span>
                </div>
                <div className="text-sm text-gray-700">
                  <div className="font-medium">{humanize(entry.resourceType)}</div>
                  <div className="mt-1 max-w-[220px] truncate font-mono text-[11px] text-gray-400">{entry.resourceId}</div>
                </div>
                <div>
                  <pre className="max-h-[88px] max-w-xl overflow-hidden rounded-lg bg-gray-50 p-3 text-[11px] leading-relaxed text-gray-600">{JSON.stringify(entry.details ?? {}, null, 2)}</pre>
                </div>
              </div>
            )}
          />
        </div>
          )}
        </div>
      )}
    </div>
  );
};

export default AuditLog;
