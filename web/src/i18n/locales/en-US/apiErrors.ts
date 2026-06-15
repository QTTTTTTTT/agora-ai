// Translations for web/src/lib/api.ts error fallbacks — English (en-US).
//
// Centralising every user-visible error string in one namespace means:
//
//   1. The api layer can call `i18next.t('apiErrors:<key>')` from any
//      module-level helper without needing to be inside a React tree;
//      i18next's runtime is a static singleton and changes language
//      whenever the user toggles it via the preference dock.
//   2. Per-locale parity is enforced by the namespace parity test (Step
//      12), so dropping a key on one side fails CI rather than silently
//      surfacing an empty string.
//   3. We can swap the wording without touching call sites.
const apiErrors = {
  // 401 / token-presence guard fired before the request leaves the
  // browser. Triggered when the local session storage no longer
  // carries a token (e.g. the user logged out in another tab).
  missingToken: "This session has no access token. Please sign in again.",
  // Fetch hit the per-request timeout. We surface "try again" because
  // a transient timeout is the most common cause and the request is
  // safely retryable.
  timeout: "The request timed out. Please try again shortly.",
  // 401 returned by the server. Triggers an automatic redirect to
  // /login in lib/api.ts; the message is what the user briefly sees
  // before the redirect.
  sessionExpired: "Your sign-in has expired. Please sign in again to continue.",
  // Login flow could not parse the JSON envelope returned by the
  // server (e.g. the proxy injected an HTML error page).
  loginBadResponse: "Sign-in failed: malformed server response.",
  // Login returned a non-2xx status with no usable error body.
  // {{status}} is interpolated by lib/api.ts.
  loginFailedStatus: "Sign-in failed (status {{status}}).",
  // Generic fallback when a non-2xx response carries no recognisable
  // error body. Surfaces the HTTP status so the user can include it
  // in a support ticket without us leaking the raw response payload.
  requestFailedStatus: "Request failed (status {{status}}).",
  // Same as above but specifically for the /api/auth/session endpoint
  // (which has a slightly different recovery path — see SessionExpiryWatcher).
  sessionFailedStatus: "Session request failed (status {{status}}).",
  // The advisor service ran but returned a plan that didn't pass our
  // schema check. The user-facing impact is the same as a 502, but
  // we keep the message specific so support can grep logs by it.
  planValidationFailed: "The AI returned a plan that did not pass validation.",
} as const;

export default apiErrors;
