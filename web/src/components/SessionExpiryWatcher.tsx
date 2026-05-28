// components/SessionExpiryWatcher.tsx
//
// Single global listener for `fundai:session-expired` events emitted by
// the API layer. When fired it (a) shows a transient amber toast
// explaining what happened, and (b) navigates to /login carrying a
// `?next=<original path>` so the user lands back where they were after
// re-auth.
//
// Why this lives in its own component (not in App or a hook):
//
//   - Must be inside <BrowserRouter> to call `useNavigate`.
//   - Must dedup bursty 401s — a fresh page mount with an expired
//     session fires 5-10 concurrent API calls; without dedup the user
//     sees a wall of toasts and we race that many navigate() calls.
//     Keeping the state in one component lets us use a ref to guard.
//   - Must NOT redirect away from the auth flow itself. Pages like
//     /login, /forgot-password, /reset-password, /verify-email all
//     legitimately hit endpoints that may 401 (especially session
//     probes during bootstrap); redirecting them to /login from /login
//     would either no-op or, worse, drop the `next` they came in with.
//
// Accessibility: the toast is rendered with `role="status"` +
// `aria-live="polite"` so screen readers announce it without
// interrupting the user; visually it sits in the top-right corner with
// a high z-index and a 6s auto-dismiss timer.

import { useEffect, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { useAppPreferences } from "../lib/preferences";
import {
  SESSION_EXPIRED_EVENT,
  type SessionExpiredDetail,
} from "../lib/sessionExpiryEvent";

const AUTH_FLOW_PATHS = [
  "/login",
  "/forgot-password",
  "/reset-password",
  "/verify-email",
];

// Session-probe paths that legitimately return 401 for unauthenticated
// users and must not trigger the redirect. Without this carve-out the
// initial `apiClient.session()` call from AuthGate would yank a fresh
// visitor to /login twice in a row.
const SILENT_PATHS = ["/api/auth/session", "/api/auth/me"];

const TOAST_VISIBLE_MS = 6000;
// Burst dedup window: any session-expired events arriving within this
// many ms of the first one are folded into the first one (single
// toast, single navigate). Tuned to comfortably cover the parallel
// fan-out of `useEffect` queries on a freshly mounted page.
const DEDUP_WINDOW_MS = 1500;

interface ToastCopy {
  title: string;
  message: string;
  dismiss: string;
}

function copyForLanguage(language: string): ToastCopy {
  if (language === "en-US") {
    return {
      title: "Session expired",
      message: "Please sign in again to continue. Redirecting to the login page…",
      dismiss: "Dismiss",
    };
  }
  return {
    title: "登录状态已失效",
    message: "请重新登录后继续操作，正在跳转到登录页…",
    dismiss: "关闭",
  };
}

export default function SessionExpiryWatcher(): JSX.Element | null {
  const navigate = useNavigate();
  const location = useLocation();
  const { language } = useAppPreferences();
  const [visible, setVisible] = useState(false);
  // Tracks the timestamp of the most recent triggering event so a burst
  // of 401s within DEDUP_WINDOW_MS produces exactly one toast/navigate.
  const lastTriggeredRef = useRef<number>(0);
  // Tracks pending dismiss timers so we can cancel them on re-trigger
  // or unmount instead of leaking. `window.setTimeout` returns a
  // `number` in the DOM lib (not the `NodeJS.Timeout` the global
  // `setTimeout` resolves to under @types/node), so we explicitly type
  // the ref to match the API we're calling — otherwise tsc complains.
  const dismissTimerRef = useRef<number | null>(null);

  useEffect(() => {
    function onExpired(event: Event) {
      const detail = (event as CustomEvent<SessionExpiredDetail>).detail ?? {};

      // Ignore silent session-probe failures.
      if (detail.path && SILENT_PATHS.some((p) => detail.path?.startsWith(p))) {
        return;
      }
      // Already on an auth-flow page — toast/redirect would be noise.
      const onAuthFlow = AUTH_FLOW_PATHS.some((p) => location.pathname.startsWith(p));
      if (onAuthFlow) {
        return;
      }
      // Dedup burst of concurrent 401s.
      const now = Date.now();
      if (now - lastTriggeredRef.current < DEDUP_WINDOW_MS) {
        return;
      }
      lastTriggeredRef.current = now;

      setVisible(true);
      // Schedule the redirect on a short delay so the user can read the
      // toast before the page swaps; not so long that they have time to
      // re-click something and trigger more 401s.
      const next = `${location.pathname}${location.search}${location.hash}` || "/";
      window.setTimeout(() => {
        // Preserve the original deep link so the Login page can route
        // the user back after a successful sign-in.
        const search = new URLSearchParams({ next }).toString();
        navigate(`/login?${search}`, { replace: true });
      }, 900);

      // Auto-hide the toast even on slow/dev navigations.
      if (dismissTimerRef.current) clearTimeout(dismissTimerRef.current);
      dismissTimerRef.current = window.setTimeout(() => {
        setVisible(false);
      }, TOAST_VISIBLE_MS);
    }

    window.addEventListener(SESSION_EXPIRED_EVENT, onExpired);
    return () => {
      window.removeEventListener(SESSION_EXPIRED_EVENT, onExpired);
      if (dismissTimerRef.current) clearTimeout(dismissTimerRef.current);
    };
  }, [navigate, location.pathname, location.search, location.hash]);

  if (!visible) return null;

  const copy = copyForLanguage(language);
  return (
    <div
      role="status"
      aria-live="polite"
      className="fixed right-4 top-4 z-[9999] flex max-w-sm items-start gap-3 rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 shadow-lg"
    >
      <div className="mt-0.5 h-2 w-2 shrink-0 rounded-full bg-amber-500" aria-hidden="true" />
      <div className="flex-1">
        <p className="text-sm font-semibold text-amber-900">{copy.title}</p>
        <p className="mt-1 text-xs text-amber-800">{copy.message}</p>
      </div>
      <button
        type="button"
        onClick={() => setVisible(false)}
        className="rounded-md px-2 py-1 text-xs font-medium text-amber-700 hover:bg-amber-100"
        aria-label={copy.dismiss}
      >
        ×
      </button>
    </div>
  );
}
