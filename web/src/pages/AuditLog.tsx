import React, { useCallback, useState } from "react";
import { useParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { exportFundAuditLogsCSV, fetchFundAuditLogs, formatApiError, type AuditLogEntry } from "../lib/api";
import { formatDateTimeForLanguage, useAppPreferences } from "../lib/preferences";
import { VirtualList } from "../components/VirtualList";
import { useListSearch } from "../lib/useListSearch";
import { useSWRFetch } from "../lib/useSWRFetch";
import { useIsBelow } from "../lib/useBreakpoint";

function humanize(value?: string): string {
  if (!value) return "-";
  return value.replace(/[_-]+/g, " ").replace(/\b\w/g, (char) => char.toUpperCase());
}

const AuditLog: React.FC = () => {
  const { fundId } = useParams<{ fundId: string }>();
  const { language } = useAppPreferences();
  // W4-26 — react-i18next migration. The previously-inline `copy`
  // block now lives in web/src/i18n/locales/{en-US,zh-CN}/auditLog.ts;
  // language switching is still driven by `useAppPreferences`
  // (see lib/preferences.tsx) which calls i18n.changeLanguage on
  // every change, so we don't need to re-thread `language` into a
  // `key` prop or anything similar.
  const { t } = useTranslation("auditLog");
  const [exporting, setExporting] = useState(false);
  const [exportError, setExportError] = useState<string | null>(null);

  // SWR-cached audit log fetch. ttl=20s keeps the page snappy
  // without making the audit feed feel "stuck" — auditable
  // events are mostly write-side (a user explicitly took an
  // action), and the page is typically read-revisited rather
  // than long-held. revalidateOnFocus=true is the right default
  // because operators flip between Audit + adjacent ops tabs to
  // cross-check actions, so a tab switch should refresh.
  // 500 row cap matches the prior code; the table is virtualised
  // via <VirtualList>, so DOM cost stays O(visible-rows).
  const swr = useSWRFetch(
    fundId ? `auditLog/${fundId}/500` : null,
    () => fetchFundAuditLogs(fundId!, 500),
    { ttlMs: 20_000, revalidateOnFocus: true },
  );

  const entries: AuditLogEntry[] = swr.data?.entries ?? [];
  const loading = !swr.data && swr.isLoading;
  // We surface the cached fetch error if any AND the (separate)
  // exporter error if it fired. Two distinct error sources on the
  // same screen but only one banner slot — present whichever is
  // more recent, with the exporter taking precedence because it's
  // user-initiated.
  const error = exportError
    ?? (swr.error ? formatApiError(swr.error, t("loadError")) : null);
  const load = swr.mutate;

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

  // W4-24 ResponsiveTable wiring: below the md breakpoint the
  // 4-column grid layout becomes unreadable (180+140+180+280 px
  // is wider than a phone viewport, forcing horizontal scroll).
  // We swap to a stack of self-contained cards in that range.
  // Above md the existing virtualised grid is preserved verbatim
  // because (a) it's the only place we have the row-virtualiser
  // wired, and (b) operators on a desktop are usually scanning
  // dozens of events at once where the grid pays off.
  const isMobile = useIsBelow("md");

  const handleExport = useCallback(async () => {
    if (!fundId || exporting) return;
    setExporting(true);
    setExportError(null);
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
      // After a successful export, force-refresh the SWR cache so
      // the on-screen list shows whatever the server snapshotted
      // (including the new audit-export event itself).
      await load();
    } catch (err) {
      setExportError(formatApiError(err, t("exportError")));
    } finally {
      setExporting(false);
    }
  }, [t, exporting, fundId, load]);

  return (
    <div className="space-y-6">
      <div className="rounded-2xl bg-gradient-to-r from-slate-900 to-slate-700 p-6 text-white shadow-sm">
        <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
          <div>
            <p className="text-xs font-semibold uppercase tracking-[0.3em] text-slate-300">Audit</p>
            <h1 className="mt-2 text-2xl font-bold">{t("title")}</h1>
            <p className="mt-2 max-w-3xl text-sm text-slate-200">{t("subtitle")}</p>
          </div>
          <button
            onClick={() => void handleExport()}
            disabled={exporting}
            className="inline-flex items-center justify-center rounded-lg bg-white px-4 py-2 text-sm font-semibold text-slate-800 shadow-sm transition hover:bg-slate-100 disabled:cursor-not-allowed disabled:opacity-60"
          >
            {exporting ? t("exporting") : t("exportCsv")}
          </button>
        </div>
      </div>

      {loading ? (
        <div className="rounded-xl border border-gray-200 bg-white p-8 text-center text-sm text-gray-500 shadow-sm">{t("loading")}</div>
      ) : error ? (
        <div className="rounded-xl border border-red-100 bg-red-50 p-6 text-sm text-red-700">
          <p>{error}</p>
          <button onClick={() => void load()} className="mt-3 rounded-lg bg-red-600 px-4 py-2 text-white hover:bg-red-700">
            {t("retry")}
          </button>
        </div>
      ) : entries.length === 0 ? (
        <div className="rounded-xl border border-dashed border-gray-300 bg-white p-10 text-center shadow-sm">
          <h2 className="text-lg font-semibold text-gray-900">{t("emptyTitle")}</h2>
          <p className="mt-2 text-sm text-gray-500">{t("emptyDescription")}</p>
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
              placeholder={t("searchPlaceholder")}
              aria-label={t("searchPlaceholder")}
              className="w-full max-w-md rounded-lg border border-gray-200 bg-white px-3 py-2 text-sm shadow-sm placeholder:text-gray-400 focus:border-indigo-400 focus:outline-none focus:ring-2 focus:ring-indigo-500/20"
            />
            <p className="text-xs text-gray-500">
              {t("matchSummary", {
                matched: search.matchCount,
                total: entries.length,
              })}
            </p>
          </div>

          {search.filtered.length === 0 ? (
            <div className="rounded-xl border border-dashed border-gray-300 bg-white p-8 text-center text-sm text-gray-500 shadow-sm">
              {t("searchEmpty")}
            </div>
          ) : isMobile ? (
        // Mobile card layout — same data, vertical stack. We do
        // not virtualise here on the assumption that a phone
        // operator rarely scrolls past a few dozen events, and
        // the cap of 500 entries from the SWR fetch keeps the
        // worst-case DOM size bounded.
        <ul className="space-y-2">
          {search.filtered.map((entry) => (
            <li
              key={entry.id}
              className="rounded-xl border border-gray-200 bg-white p-3 shadow-sm"
            >
              <div className="flex flex-wrap items-center justify-between gap-2">
                <span className="text-xs font-medium text-gray-500">
                  {formatDateTimeForLanguage(entry.createdAt, language)}
                </span>
                <span className="rounded-full bg-blue-50 px-2.5 py-1 text-[11px] font-medium text-blue-700">
                  {humanize(entry.action)}
                </span>
              </div>
              <div className="mt-2 text-sm font-medium text-gray-800">
                {humanize(entry.resourceType)}
              </div>
              <div className="mt-0.5 truncate font-mono text-[11px] text-gray-400">
                {entry.resourceId}
              </div>
              <pre className="mt-2 max-h-32 overflow-auto rounded-lg bg-gray-50 p-2 text-[11px] leading-relaxed text-gray-600">
                {JSON.stringify(entry.details ?? {}, null, 2)}
              </pre>
            </li>
          ))}
        </ul>
          ) : (
        <div className="overflow-hidden rounded-xl border border-gray-200 bg-white shadow-sm">
          <div className="grid grid-cols-[180px_140px_minmax(180px,1fr)_minmax(280px,2fr)] bg-gray-50 px-4 py-3 text-xs uppercase tracking-wider text-gray-500">
            <div>{t("columns.time")}</div>
            <div>{t("columns.action")}</div>
            <div>{t("columns.resource")}</div>
            <div>{t("columns.details")}</div>
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
