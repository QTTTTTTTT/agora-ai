// notificationCenter.tsx — persistent in-browser notification store.
//
// WHY THIS EXISTS
// ---------------
// Today the app shows momentary toasts via lib/toast.tsx — they
// fly by, autodismiss after a few seconds, and are gone. Useful
// for "request succeeded / failed" feedback. NOT useful for:
//
//   - "workflow X completed at 14:32 — review the decisions",
//   - "skill Y was auto-promoted — verify the shadow eval",
//   - "alert: AAPL hit your stop-loss trigger",
//   - "fund Z forward-gate is now eligible — review listing",
//
// The user closes the tab, comes back at 4pm, and has no
// affordance to see what happened while they were gone. The
// "miss the important event" failure mode is the
// single-most-cited gap in product feedback.
//
// SCOPE — STARTER VERSION
// -----------------------
// This commit lands a CLIENT-ONLY persistent notification center:
//   - notifications are stored in localStorage (per-user, since
//     localStorage is per-origin and each user's tab carries
//     their session),
//   - any module can call `notifications.push({...})` to add a
//     notification (typed contract: kind, title, body, optional
//     deep link),
//   - the bell-icon in the navbar shows the unread count badge,
//   - clicking the bell opens a panel listing recent
//     notifications, with click-to-mark-read + a "mark all read"
//     button,
//   - cross-tab sync via the existing useBroadcastChannel —
//     reading in one tab updates the unread count in others.
//
// What this DOES NOT do (yet, on purpose):
//   - server-side persistence — notifications are local to the
//     browser; signing in on another device gets you a clean
//     slate. The right backend pattern (notifications table +
//     SSE delivery + per-user retention policy) is a separate
//     concern; the client primitive lands first so we can wire
//     server delivery into it later without rewriting consumers.
//   - desktop OS push (Notification API / service worker) —
//     follow-up; needs permission flow + service worker
//     registration that we'd rather think about once.
//   - filtering by category, search, unread-only filters —
//     deliberate; the starter UI is a flat reverse-chrono list.

import React, {
  ReactNode,
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";
import { useNavigate } from "react-router-dom";
import { useBroadcastChannel } from "./useBroadcastChannel";

export type NotificationKind = "info" | "success" | "warning" | "error";

export interface NotificationEntry {
  id: string;
  kind: NotificationKind;
  title: string;
  body?: string;
  /** Optional deep link — clicking the row navigates here. */
  href?: string;
  /** Epoch ms. Set automatically by push(). */
  ts: number;
  /** True once the user has clicked / dismissed / mark-read'd. */
  read: boolean;
}

const STORAGE_KEY = "fundai.notifications.v1";
const BROADCAST_CHANNEL = "fundai.notifications";
const MAX_NOTIFICATIONS = 100; // cap so localStorage doesn't grow unbounded

interface NotificationContextValue {
  notifications: NotificationEntry[];
  unreadCount: number;
  push: (entry: Omit<NotificationEntry, "id" | "ts" | "read">) => string;
  markRead: (id: string) => void;
  markAllRead: () => void;
  remove: (id: string) => void;
  clear: () => void;
}

const NotificationContext = createContext<NotificationContextValue | null>(null);

function loadInitial(): NotificationEntry[] {
  if (typeof window === "undefined") return [];
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter(
      (e): e is NotificationEntry =>
        typeof e === "object" &&
        e !== null &&
        typeof e.id === "string" &&
        typeof e.title === "string" &&
        typeof e.ts === "number" &&
        typeof e.read === "boolean",
    );
  } catch {
    return [];
  }
}

function persist(entries: NotificationEntry[]) {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(entries.slice(0, MAX_NOTIFICATIONS)));
  } catch {
    // localStorage may be full / blocked — silently drop. Better
    // than crashing on every push for a user with a quota issue.
  }
}

export const NotificationProvider: React.FC<{ children: ReactNode }> = ({ children }) => {
  const [notifications, setNotifications] = useState<NotificationEntry[]>(loadInitial);

  // Cross-tab sync — when another tab pushes / marks read,
  // mirror the change here.
  const { post } = useBroadcastChannel<{ kind: "set"; entries: NotificationEntry[] }>(
    BROADCAST_CHANNEL,
    (msg) => {
      if (msg.kind === "set" && Array.isArray(msg.entries)) {
        setNotifications(msg.entries);
      }
    },
  );

  // Persist + broadcast on every state change.
  useEffect(() => {
    persist(notifications);
  }, [notifications]);

  const push = useCallback(
    (entry: Omit<NotificationEntry, "id" | "ts" | "read">) => {
      const id =
        typeof crypto !== "undefined" && "randomUUID" in crypto
          ? crypto.randomUUID()
          : `n-${Math.random().toString(36).slice(2)}-${Date.now()}`;
      setNotifications((prev) => {
        const next = [{ ...entry, id, ts: Date.now(), read: false }, ...prev].slice(0, MAX_NOTIFICATIONS);
        post({ kind: "set", entries: next });
        return next;
      });
      return id;
    },
    [post],
  );

  const markRead = useCallback(
    (id: string) => {
      setNotifications((prev) => {
        const next = prev.map((n) => (n.id === id ? { ...n, read: true } : n));
        post({ kind: "set", entries: next });
        return next;
      });
    },
    [post],
  );

  const markAllRead = useCallback(() => {
    setNotifications((prev) => {
      const next = prev.map((n) => ({ ...n, read: true }));
      post({ kind: "set", entries: next });
      return next;
    });
  }, [post]);

  const remove = useCallback(
    (id: string) => {
      setNotifications((prev) => {
        const next = prev.filter((n) => n.id !== id);
        post({ kind: "set", entries: next });
        return next;
      });
    },
    [post],
  );

  const clear = useCallback(() => {
    setNotifications([]);
    post({ kind: "set", entries: [] });
  }, [post]);

  const unreadCount = useMemo(
    () => notifications.reduce((acc, n) => acc + (n.read ? 0 : 1), 0),
    [notifications],
  );

  const ctx = useMemo<NotificationContextValue>(
    () => ({ notifications, unreadCount, push, markRead, markAllRead, remove, clear }),
    [notifications, unreadCount, push, markRead, markAllRead, remove, clear],
  );

  return <NotificationContext.Provider value={ctx}>{children}</NotificationContext.Provider>;
};

export function useNotifications(): NotificationContextValue {
  const ctx = useContext(NotificationContext);
  if (!ctx) {
    throw new Error("useNotifications must be used inside NotificationProvider");
  }
  return ctx;
}

// ---------------------------------------------------------------
// Imperative push API for non-React modules. The api.ts module
// or the SSE layer can `import { notifyPush } from "./notificationCenter"`
// without holding a hook reference. We register the bound push
// callback once when the provider mounts.
// ---------------------------------------------------------------

let externalPush: ((entry: Omit<NotificationEntry, "id" | "ts" | "read">) => string) | null = null;

export function notifyPush(entry: Omit<NotificationEntry, "id" | "ts" | "read">): string {
  if (externalPush) return externalPush(entry);
  // No provider mounted (rare but possible during initial bootstrap):
  // queue into localStorage directly so the next mount picks it up.
  if (typeof window !== "undefined") {
    try {
      const cur = loadInitial();
      const id = `n-${Math.random().toString(36).slice(2)}-${Date.now()}`;
      const next = [{ ...entry, id, ts: Date.now(), read: false }, ...cur].slice(
        0,
        MAX_NOTIFICATIONS,
      );
      persist(next);
      return id;
    } catch {
      /* ignore */
    }
  }
  return "";
}

export const NotificationBridge: React.FC = () => {
  const { push } = useNotifications();
  useEffect(() => {
    externalPush = push;
    return () => {
      externalPush = null;
    };
  }, [push]);
  return null;
};

// ---------------------------------------------------------------
// UI: <NotificationBell /> — a small badge button that opens the
// panel. Drop into the top nav. Fully accessible (aria-expanded,
// aria-controls, escape closes).
// ---------------------------------------------------------------

const kindStyles: Record<NotificationKind, string> = {
  info: "bg-blue-100 text-blue-800 dark:bg-blue-900/40 dark:text-blue-200",
  success: "bg-emerald-100 text-emerald-800 dark:bg-emerald-900/40 dark:text-emerald-200",
  warning: "bg-amber-100 text-amber-800 dark:bg-amber-900/40 dark:text-amber-200",
  error: "bg-red-100 text-red-800 dark:bg-red-900/40 dark:text-red-200",
};

function formatRelative(ts: number, isEnglish: boolean): string {
  const now = Date.now();
  const delta = Math.max(0, now - ts);
  const sec = Math.floor(delta / 1000);
  const min = Math.floor(sec / 60);
  const hr = Math.floor(min / 60);
  const day = Math.floor(hr / 24);
  if (sec < 60) return isEnglish ? "just now" : "刚刚";
  if (min < 60) return isEnglish ? `${min}m ago` : `${min} 分钟前`;
  if (hr < 24) return isEnglish ? `${hr}h ago` : `${hr} 小时前`;
  if (day < 7) return isEnglish ? `${day}d ago` : `${day} 天前`;
  return new Date(ts).toLocaleDateString();
}

interface NotificationBellProps {
  language: "zh-CN" | "en-US";
}

export const NotificationBell: React.FC<NotificationBellProps> = ({ language }) => {
  const { notifications, unreadCount, markRead, markAllRead, remove } = useNotifications();
  const navigate = useNavigate();
  const [open, setOpen] = useState(false);
  const isEnglish = language === "en-US";

  // Esc closes when open.
  useEffect(() => {
    if (!open) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [open]);

  const onClickEntry = (n: NotificationEntry) => {
    markRead(n.id);
    if (n.href) navigate(n.href);
    setOpen(false);
  };

  return (
    <div className="relative">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        aria-controls="notification-panel"
        aria-label={isEnglish ? `Notifications (${unreadCount} unread)` : `通知（${unreadCount} 未读）`}
        className="relative inline-flex items-center justify-center rounded-full border border-gray-200 bg-white px-3 py-1.5 text-sm text-gray-700 shadow-sm transition hover:bg-gray-50 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-100 dark:hover:bg-slate-700"
      >
        <span aria-hidden="true">🔔</span>
        {unreadCount > 0 ? (
          <span className="absolute -right-1 -top-1 inline-flex h-4 min-w-[16px] items-center justify-center rounded-full bg-red-500 px-1 text-[10px] font-semibold text-white">
            {unreadCount > 99 ? "99+" : unreadCount}
          </span>
        ) : null}
      </button>
      {open ? (
        <div
          id="notification-panel"
          role="dialog"
          aria-label={isEnglish ? "Notifications" : "通知"}
          className="absolute right-0 z-50 mt-2 w-96 max-w-[calc(100vw-2rem)] origin-top-right overflow-hidden rounded-2xl border border-gray-200 bg-white shadow-2xl dark:border-slate-700 dark:bg-slate-900"
        >
          <div className="flex items-center justify-between gap-3 border-b border-gray-100 px-4 py-3 dark:border-slate-700">
            <p className="text-sm font-semibold text-gray-900 dark:text-slate-100">
              {isEnglish ? "Notifications" : "通知"}
            </p>
            <button
              type="button"
              onClick={() => markAllRead()}
              disabled={unreadCount === 0}
              className="text-xs font-medium text-indigo-600 hover:text-indigo-500 disabled:cursor-not-allowed disabled:text-gray-400 dark:text-indigo-300 dark:hover:text-indigo-200"
            >
              {isEnglish ? "Mark all read" : "全部标为已读"}
            </button>
          </div>
          <ul className="max-h-[60vh] overflow-y-auto">
            {notifications.length === 0 ? (
              <li className="px-4 py-8 text-center text-sm text-gray-500 dark:text-slate-400">
                {isEnglish ? "No notifications yet." : "暂无通知。"}
              </li>
            ) : (
              notifications.map((n) => (
                <li
                  key={n.id}
                  className={`group flex items-start gap-3 border-b border-gray-100 px-4 py-3 transition dark:border-slate-800 ${
                    n.read ? "bg-white dark:bg-slate-900" : "bg-indigo-50/40 dark:bg-indigo-950/20"
                  }`}
                >
                  <span
                    aria-hidden="true"
                    className={`mt-0.5 inline-flex h-6 min-w-[24px] items-center justify-center rounded-full px-1 text-[10px] font-semibold uppercase tracking-wide ${kindStyles[n.kind]}`}
                  >
                    {n.kind.charAt(0)}
                  </span>
                  <button
                    type="button"
                    onClick={() => onClickEntry(n)}
                    className="flex-1 text-left"
                  >
                    <p className="text-sm font-medium text-gray-900 dark:text-slate-100">{n.title}</p>
                    {n.body ? (
                      <p className="mt-0.5 text-xs text-gray-600 dark:text-slate-300">{n.body}</p>
                    ) : null}
                    <p className="mt-1 text-[10px] uppercase tracking-wider text-gray-400 dark:text-slate-500">
                      {formatRelative(n.ts, isEnglish)}
                    </p>
                  </button>
                  <button
                    type="button"
                    onClick={() => remove(n.id)}
                    aria-label={isEnglish ? "Dismiss" : "移除"}
                    className="opacity-0 transition group-hover:opacity-100 text-gray-400 hover:text-gray-700 dark:text-slate-500 dark:hover:text-slate-200"
                  >
                    ×
                  </button>
                </li>
              ))
            )}
          </ul>
        </div>
      ) : null}
    </div>
  );
};
