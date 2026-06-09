// AdminAnnouncementsSection — admin console panel for publishing
// and archiving in-app announcements (站内信).
//
// Endpoints used:
//   GET    /api/admin/announcements?includeArchived=true
//   POST   /api/admin/announcements
//   DELETE /api/admin/announcements/{id}        (soft archive)
//
// Severity drives the banner colour on the user side (info / warning
// / critical). Critical announcements require the user to confirm
// dismissal.

import React, { useCallback, useEffect, useState } from "react";
import { apiDelete, apiGet, apiPost, formatApiError } from "../lib/api";
import { formatDateTimeForLanguage, useAppPreferences, type AppLanguage } from "../lib/preferences";

type Severity = "info" | "warning" | "critical";

interface AnnouncementWire {
  id: string;
  title: string;
  body: string;
  severity: Severity | string;
  publishedAt: string;
  publishedBy?: string;
  archivedAt?: string;
}

interface AnnouncementsResponse {
  announcements?: AnnouncementWire[];
}

function copyForLanguage(language: AppLanguage) {
  if (language === "en-US") {
    return {
      title: "In-app announcements",
      subtitle: "Push platform-wide notices to every signed-in user. Dismissal is per-user; archiving here removes the banner for everyone.",
      newTitle: "New announcement",
      titleField: "Title",
      bodyField: "Body",
      severity: "Severity",
      severityInfo: "Info",
      severityWarning: "Warning",
      severityCritical: "Critical",
      publish: "Publish",
      publishing: "Publishing…",
      cancel: "Clear",
      list: "Recent announcements",
      includeArchived: "Show archived",
      archive: "Archive",
      archiving: "Archiving…",
      empty: "No announcements have been published yet.",
      retry: "Retry",
      loadFailed: "Could not load announcements",
      missingFields: "Title and body are required.",
      archived: "Archived",
      live: "Live",
      author: "by",
    };
  }
  return {
    title: "站内信",
    subtitle: "向所有登录用户发布站内通告。用户可单独标记已读，归档后所有用户的横幅会一并消失。",
    newTitle: "发布站内信",
    titleField: "标题",
    bodyField: "内容",
    severity: "等级",
    severityInfo: "通知",
    severityWarning: "注意",
    severityCritical: "重要",
    publish: "立即发布",
    publishing: "发布中…",
    cancel: "清空",
    list: "最近的站内信",
    includeArchived: "显示已归档",
    archive: "归档",
    archiving: "归档中…",
    empty: "暂未发布任何站内信。",
    retry: "重试",
    loadFailed: "加载站内信失败",
    missingFields: "标题与内容均不可为空。",
    archived: "已归档",
    live: "生效中",
    author: "发布者",
  };
}

const severityOptions: Severity[] = ["info", "warning", "critical"];

interface AdminAnnouncementsSectionProps {
  language: AppLanguage;
}

const AdminAnnouncementsSection: React.FC<AdminAnnouncementsSectionProps> = ({ language }) => {
  const copy = copyForLanguage(language);
  const [announcements, setAnnouncements] = useState<AnnouncementWire[]>([]);
  const [includeArchived, setIncludeArchived] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [draftTitle, setDraftTitle] = useState("");
  const [draftBody, setDraftBody] = useState("");
  const [draftSeverity, setDraftSeverity] = useState<Severity>("info");
  const [publishing, setPublishing] = useState(false);
  const [archivingId, setArchivingId] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const qs = includeArchived ? "?includeArchived=true" : "";
      const resp = await apiGet<AnnouncementsResponse>(`/api/admin/announcements${qs}`);
      setAnnouncements(resp?.announcements ?? []);
    } catch (err) {
      setError(formatApiError(err, copy.loadFailed));
    } finally {
      setLoading(false);
    }
  }, [copy.loadFailed, includeArchived]);

  useEffect(() => {
    void load();
  }, [load]);

  const publish = useCallback(async () => {
    if (!draftTitle.trim() || !draftBody.trim()) {
      setError(copy.missingFields);
      return;
    }
    setPublishing(true);
    setError(null);
    try {
      await apiPost<AnnouncementWire>("/api/admin/announcements", {
        title: draftTitle.trim(),
        body: draftBody.trim(),
        severity: draftSeverity,
      });
      setDraftTitle("");
      setDraftBody("");
      setDraftSeverity("info");
      await load();
    } catch (err) {
      setError(formatApiError(err, copy.loadFailed));
    } finally {
      setPublishing(false);
    }
  }, [copy.loadFailed, copy.missingFields, draftBody, draftSeverity, draftTitle, load]);

  const archive = useCallback(
    async (id: string) => {
      setArchivingId(id);
      setError(null);
      try {
        await apiDelete<{ ok: boolean }>(`/api/admin/announcements/${id}`);
        await load();
      } catch (err) {
        setError(formatApiError(err, copy.loadFailed));
      } finally {
        setArchivingId(null);
      }
    },
    [copy.loadFailed, load],
  );

  return (
    <section className="rounded-2xl border border-ink-100 bg-white px-5 py-5 shadow-envelope">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold text-ink-900">{copy.title}</h2>
          <p className="mt-1 max-w-2xl text-sm text-ink-500">{copy.subtitle}</p>
        </div>
        <button
          type="button"
          onClick={() => void load()}
          className="rounded-full border border-ink-200 bg-white px-3 py-1 text-xs font-medium text-ink-700 hover:bg-cream-100"
        >
          {copy.retry}
        </button>
      </div>

      {/* Composer */}
      <div className="mt-4 rounded-xl border border-ink-100 bg-cream-50 p-4">
        <h3 className="text-sm font-semibold text-ink-900">{copy.newTitle}</h3>
        <div className="mt-3 grid grid-cols-1 gap-3 md:grid-cols-3">
          <label className="md:col-span-2">
            <span className="text-xs text-ink-500">{copy.titleField}</span>
            <input
              type="text"
              value={draftTitle}
              onChange={(e) => setDraftTitle(e.target.value)}
              maxLength={200}
              className="mt-1 w-full rounded-lg border border-ink-200 bg-white px-3 py-2 text-sm text-ink-900 focus:border-ink-400 focus:outline-none"
            />
          </label>
          <label>
            <span className="text-xs text-ink-500">{copy.severity}</span>
            <select
              value={draftSeverity}
              onChange={(e) => setDraftSeverity(e.target.value as Severity)}
              className="mt-1 w-full rounded-lg border border-ink-200 bg-white px-3 py-2 text-sm text-ink-900 focus:border-ink-400 focus:outline-none"
            >
              {severityOptions.map((s) => (
                <option key={s} value={s}>
                  {s === "info" ? copy.severityInfo : s === "warning" ? copy.severityWarning : copy.severityCritical}
                </option>
              ))}
            </select>
          </label>
        </div>
        <label className="mt-3 block">
          <span className="text-xs text-ink-500">{copy.bodyField}</span>
          <textarea
            value={draftBody}
            onChange={(e) => setDraftBody(e.target.value)}
            rows={4}
            className="mt-1 w-full rounded-lg border border-ink-200 bg-white px-3 py-2 text-sm text-ink-900 focus:border-ink-400 focus:outline-none"
          />
        </label>
        <div className="mt-3 flex flex-wrap items-center justify-end gap-2">
          <button
            type="button"
            onClick={() => {
              setDraftTitle("");
              setDraftBody("");
              setDraftSeverity("info");
              setError(null);
            }}
            className="rounded-full border border-ink-200 bg-white px-4 py-1.5 text-xs font-medium text-ink-700 hover:bg-cream-100"
          >
            {copy.cancel}
          </button>
          <button
            type="button"
            onClick={() => void publish()}
            disabled={publishing}
            className="rounded-full bg-ink-900 px-4 py-1.5 text-xs font-semibold text-white transition hover:bg-ink-700 disabled:opacity-60"
          >
            {publishing ? copy.publishing : copy.publish}
          </button>
        </div>
      </div>

      {error ? <p className="mt-3 text-sm text-rose-600">{error}</p> : null}

      {/* List */}
      <div className="mt-5">
        <div className="flex items-center justify-between">
          <h3 className="text-sm font-semibold text-ink-900">{copy.list}</h3>
          <label className="flex items-center gap-2 text-xs text-ink-500">
            <input
              type="checkbox"
              checked={includeArchived}
              onChange={(e) => setIncludeArchived(e.target.checked)}
              className="h-3.5 w-3.5 rounded border-ink-300"
            />
            {copy.includeArchived}
          </label>
        </div>
        <ul className="mt-3 space-y-2">
          {loading ? (
            <li className="rounded-xl border border-dashed border-ink-100 px-4 py-6 text-center text-sm text-ink-400">
              {language === "en-US" ? "Loading…" : "加载中…"}
            </li>
          ) : announcements.length === 0 ? (
            <li className="rounded-xl border border-dashed border-ink-100 px-4 py-6 text-center text-sm text-ink-400">
              {copy.empty}
            </li>
          ) : (
            announcements.map((a) => (
              <li
                key={a.id}
                className={`rounded-xl border px-4 py-3 transition ${
                  a.archivedAt
                    ? "border-ink-100 bg-cream-50 text-ink-500"
                    : a.severity === "critical"
                      ? "border-rose-200 bg-rose-50/60"
                      : a.severity === "warning"
                        ? "border-amber-200 bg-amber-50/60"
                        : "border-sage-200 bg-sage-50/40"
                }`}
              >
                <div className="flex flex-wrap items-start justify-between gap-3">
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-2">
                      <span
                        className={`rounded-full px-2 py-0.5 text-[11px] font-semibold ${
                          a.severity === "critical"
                            ? "bg-rose-700 text-white"
                            : a.severity === "warning"
                              ? "bg-amber-600 text-white"
                              : "bg-sage-600 text-white"
                        }`}
                      >
                        {a.severity === "critical"
                          ? copy.severityCritical
                          : a.severity === "warning"
                            ? copy.severityWarning
                            : copy.severityInfo}
                      </span>
                      <span className="text-sm font-semibold text-ink-900">{a.title}</span>
                      <span
                        className={`rounded-full px-2 py-0.5 text-[10px] ${
                          a.archivedAt ? "bg-ink-100 text-ink-500" : "bg-emerald-100 text-emerald-700"
                        }`}
                      >
                        {a.archivedAt ? copy.archived : copy.live}
                      </span>
                    </div>
                    <p className="mt-1 whitespace-pre-line text-sm text-ink-700">{a.body}</p>
                    <p className="mt-1 text-[11px] text-ink-500">
                      {formatDateTimeForLanguage(a.publishedAt, language)}
                      {a.publishedBy ? (
                        <>
                          {" · "}
                          {copy.author} {a.publishedBy}
                        </>
                      ) : null}
                    </p>
                  </div>
                  {!a.archivedAt ? (
                    <button
                      type="button"
                      onClick={() => void archive(a.id)}
                      disabled={archivingId === a.id}
                      className="rounded-full border border-ink-200 bg-white px-3 py-1 text-xs font-medium text-ink-700 hover:bg-cream-100 disabled:opacity-60"
                    >
                      {archivingId === a.id ? copy.archiving : copy.archive}
                    </button>
                  ) : null}
                </div>
              </li>
            ))
          )}
        </ul>
      </div>
    </section>
  );
};

export default AdminAnnouncementsSection;
