// lib/sessionExpiryEvent.ts
//
// Bridge between the API layer (api.ts knows about the 401) and the
// router/UI layer (App.tsx needs to navigate + show a toast). We can't
// just call `useNavigate()` from inside `apiRequest` because that's not
// a React component. We also can't hardcode a `window.location.href =
// '/login'` because we want a soft client-side navigation that preserves
// the original deep link as `?next=...` so the user lands back where
// they were after re-auth.
//
// A custom DOM event is the simplest cross-cutting primitive: api.ts
// fires it, a tiny watcher component mounted near the router root
// listens for it, and there's exactly one place that knows how to react
// — no per-page error handling, no prop drilling.
//
// Debouncing is critical: a typical page mount fires 5-10 parallel API
// calls, and if the session just expired *all of them* return 401 at
// roughly the same time. Without dedup the user would see five toasts
// stacked and we'd race five navigate() calls. The watcher itself
// implements the dedup; this module just defines the contract.

export const SESSION_EXPIRED_EVENT = "fundai:session-expired";

export interface SessionExpiredDetail {
  /** Server request_id of the offending response, when available. Useful
   *  for cross-referencing with backend logs ("why was my session
   *  killed?" → grep for this id). */
  requestId?: string;
  /** API path that triggered the 401. Helps the watcher decide whether
   *  to silently ignore (e.g. /api/auth/session probes during bootstrap
   *  are expected to 401 for logged-out users and should NOT redirect). */
  path?: string;
  /** Optional reason string for diagnostics; surfaced in the dev
   *  console, not shown to users. */
  reason?: string;
}

/** Fire the session-expired event. Safe to call in non-browser
 *  environments (SSR, tests) — silently no-ops if `window` is absent. */
export function dispatchSessionExpired(detail: SessionExpiredDetail = {}): void {
  if (typeof window === "undefined") return;
  try {
    window.dispatchEvent(
      new CustomEvent<SessionExpiredDetail>(SESSION_EXPIRED_EVENT, { detail }),
    );
  } catch {
    // Some very old browsers can't construct CustomEvent; we just drop
    // the notification rather than crash the request pipeline.
  }
}
