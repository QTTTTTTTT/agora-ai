// AnnouncementCenter — sticky banner for admin-published in-app
// announcements (站内信).
//
// Mounting:
//   <AnnouncementCenter /> sits at the top of the authenticated app
//   shell (above PreferenceDock). It fetches /api/announcements on
//   mount and polls every 90s. The newest unread announcement is
//   shown as a banner; users dismiss with a single click which
//   POSTs /api/announcements/{id}/read so the banner stays gone
//   across reloads.
//
// Rendering rules:
//   * If the operator has multiple unread announcements, we cycle
//     them: the newest takes top billing; "+N earlier" is shown as
//     a chip the user can click to advance.
//   * Severity drives colour: info (sage), warning (amber),
//     critical (rose). Critical announcements are sticky — the
//     dismiss action requires a confirm tap.

import React, { useCallback, useEffect, useMemo, useState } from "react";
import { apiGet, apiPost, formatApiError, getStoredSession } from "../lib/api";
import { formatDateTimeForLanguage, useAppPreferences, type AppLanguage } from "../lib/preferences";

interface AnnouncementWire {
  id: string;
  title: string;
  body: string;
  severity: "info" | "warning" | "critical" | string;
  publishedAt: string;
  publishedBy?: string;
  read?: boolean;
}

interface AnnouncementsResponse {
  announcements?: AnnouncementWire[];
}

const REFRESH_INTERVAL_MS = 90_000;

const severityStyles: Record<string, { bg: string; text: string; ring: string; chip: string }> = {
  info: {
    bg: "bg-sage-50/90",
    text: "text-ink-700",
    ring: "ring-1 ring-sage-200",
    chip: "bg-white/80 text-sage-700",
  },
  warning: {
    bg: "bg-amber-50/90",
    text: "text-amber-900",
    ring: "ring-1 ring-amber-200",
    chip: "bg-white/80 text-amber-700",
  },
  critical: {
    bg: "bg-rose-50/90",
    text: "text-rose-900",
    ring: "ring-1 ring-rose-200",
    chip: "bg-white/80 text-rose-700",
  },
};

function copyForLanguage(language: AppLanguage) {
  if (language === "en-US") {
    return {
      severityLabel: { info: "Notice", warning: "Heads-up", critical: "Critical" } as Record<string, string>,
      moreEarlier: (n: number) => `+${n} earlier`,
      dismiss: "Got it",
      dismissCritical: "Acknowledge",
      publishedBy: "by",
    };
  }
  return {
    severityLabel: { info: "通知", warning: "注意", critical: "重要" } as Record<string, string>,
    moreEarlier: (n: number) => `还有 ${n} 条更早的通知`,
    dismiss: "已知悉",
    dismissCritical: "确认知悉",
    publishedBy: "来自",
  };
}

const AnnouncementCenter: React.FC = () => {
  const { language } = useAppPreferences();
  const copy = useMemo(() => copyForLanguage(language), [language]);
  const [items, setItems] = useState<AnnouncementWire[]>([]);
  const [activeIndex, setActiveIndex] = useState(0);
  // dismissingId blocks double-clicks while the POST is in flight.
  const [dismissingId, setDismissingId] = useState<string | null>(null);
  // criticalConfirmId tracks "user clicked dismiss on a critical
  // announcement" — we require a second click before sending.
  const [criticalConfirmId, setCriticalConfirmId] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    // Skip the network round-trip when there's no session — the
    // banner is mounted globally so we'd otherwise fire a 401 on
    // /login and trigger the SessionExpiryWatcher's "redirect" toast
    // every 90s for unauthenticated tabs. getStoredSession() is the
    // cheap, sync check the rest of the app uses for the same gate.
    if (!getStoredSession()) {
      setItems([]);
      return;
    }
    try {
      const resp = await apiGet<AnnouncementsResponse>("/api/announcements");
      const unread = (resp?.announcements ?? []).filter((a) => !a.read);
      setItems(unread);
      setActiveIndex((prev) => (prev >= unread.length ? 0 : prev));
    } catch {
      // Silent: missing announcements should never break the shell.
    }
  }, []);

  useEffect(() => {
    void refresh();
    const id = window.setInterval(() => {
      void refresh();
    }, REFRESH_INTERVAL_MS);
    // Listen for the session-expiry event so we clear the banner
    // immediately when the user is logged out (instead of waiting
    // for the next poll, which would keep showing stale items).
    const onExpired = () => setItems([]);
    window.addEventListener("fundai:session-expired", onExpired);
    return () => {
      window.clearInterval(id);
      window.removeEventListener("fundai:session-expired", onExpired);
    };
  }, [refresh]);

  const active = items[activeIndex];

  const handleDismiss = useCallback(
    async (id: string) => {
      setDismissingId(id);
      try {
        await apiPost(`/api/announcements/${id}/read`);
        setItems((prev) => prev.filter((a) => a.id !== id));
        setActiveIndex(0);
        setCriticalConfirmId(null);
      } catch (err) {
        // Bubble up via the global toast so the user knows their
        // dismiss didn't take. Reuse the api.ts toast surface so we
        // don't need a second toast pipeline here.
        const detail = formatApiError(err, language === "en-US" ? "Could not dismiss" : "标记已读失败");
        // eslint-disable-next-line no-console
        console.warn("announcement dismiss failed", detail);
      } finally {
        setDismissingId(null);
      }
    },
    [language],
  );

  if (!active) {
    return null;
  }
  const styles = severityStyles[active.severity] ?? severityStyles.info;
  const isCritical = active.severity === "critical";
  const dismissLabel = isCritical
    ? criticalConfirmId === active.id
      ? copy.dismissCritical
      : copy.dismissCritical
    : copy.dismiss;
  const dismissDisabled = dismissingId === active.id;
  const totalRemaining = items.length - 1;

  return (
    <div className={`sticky top-0 z-40 ${styles.bg} ${styles.ring} backdrop-blur-sm`}>
      <div className="mx-auto flex max-w-7xl flex-wrap items-start gap-3 px-4 py-3 sm:px-6">
        <span className={`rounded-full px-3 py-1 text-[11px] font-semibold ${styles.chip}`}>
          {copy.severityLabel[active.severity] ?? active.severity}
        </span>
        <div className="min-w-0 flex-1">
          <p className={`text-sm font-semibold ${styles.text}`}>{active.title}</p>
          <p className={`mt-1 whitespace-pre-line text-sm ${styles.text}`}>{active.body}</p>
          <p className="mt-1 flex flex-wrap items-center gap-2 text-[11px] text-ink-500">
            <span>{formatDateTimeForLanguage(active.publishedAt, language)}</span>
            {active.publishedBy ? (
              <>
                <span>·</span>
                <span>
                  {copy.publishedBy} {active.publishedBy}
                </span>
              </>
            ) : null}
            {totalRemaining > 0 ? (
              <>
                <span>·</span>
                <button
                  type="button"
                  className="underline-offset-2 hover:underline"
                  onClick={() => setActiveIndex((prev) => (prev + 1) % items.length)}
                >
                  {copy.moreEarlier(totalRemaining)}
                </button>
              </>
            ) : null}
          </p>
        </div>
        <button
          type="button"
          onClick={() => {
            if (isCritical && criticalConfirmId !== active.id) {
              setCriticalConfirmId(active.id);
              return;
            }
            void handleDismiss(active.id);
          }}
          disabled={dismissDisabled}
          className={`rounded-full px-4 py-1.5 text-xs font-semibold transition disabled:opacity-60 ${
            isCritical
              ? "bg-rose-700 text-white hover:bg-rose-800"
              : "bg-ink-900 text-white hover:bg-ink-700"
          }`}
        >
          {isCritical && criticalConfirmId === active.id
            ? copy.dismissCritical
            : isCritical
              ? language === "en-US"
                ? "Dismiss"
                : "关闭"
              : dismissLabel}
        </button>
      </div>
    </div>
  );
};

export default AnnouncementCenter;
