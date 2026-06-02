/**
 * Per-action step-up authentication helper (P0-7).
 *
 * Pairs a biometric prompt with a server-side mint of a short-
 * lived step-up JWT. Callers wrap a high-risk action with
 * `withStepUp()` to acquire the token and attach it to the
 * request:
 *
 *     await withStepUp('Confirm cancel order', (token) =>
 *       apiClient.cancelOrder(fundId, tradeId, { stepUpToken: token })
 *     );
 *
 * Caching policy
 *
 *  - We cache the most recent token in module-level state so
 *    chained actions (e.g. open dialog → review → confirm) do
 *    not re-prompt the biometric every keystroke.
 *  - The cache lifetime is min(server_ttl - 5s, BIOMETRIC_GRACE_MS)
 *    where BIOMETRIC_GRACE_MS is a deliberately conservative 60s.
 *    Whichever expires first wins.
 *  - The cache is voided when the user toggles the "require
 *    step-up for orders" preference off (handled in userPrefs)
 *    AND on every logout (auth.tsx clears it via clearStepUpCache).
 *
 * Failure modes
 *
 *  - Biometric library unavailable (older devices / dev builds):
 *    requireBiometrics returns null. We treat this as "skip" and
 *    return undefined so the caller can decide whether to proceed
 *    without step-up. Today the server logs this as
 *    {step_up:false, reason:"missing"} but doesn't reject — see
 *    P0-9 for the live-trading hard gate.
 *  - Biometric explicitly cancelled (user pressed Cancel): we
 *    throw `ErrStepUpCancelled` so the UI can quietly abort the
 *    pending action rather than firing it without proof.
 *  - Network error on /step-up: we throw the underlying ApiError;
 *    callers can either retry or proceed without (a server with a
 *    stale cache returns the appropriate "missing" status).
 */

import { ApiError, type StepUpResponse } from '@fundai/api-client';

import { apiClient } from './api';
import { requireBiometrics } from './secureStore';

// BIOMETRIC_GRACE_MS bounds how long a single biometric assertion
// powers subsequent actions before a fresh prompt is required.
// 60s matches what users tolerate based on internal usability
// research; we don't expose it as a knob to keep the security
// envelope predictable.
const BIOMETRIC_GRACE_MS = 60_000;

// Safety buffer subtracted from the server-issued TTL. Prevents a
// race where the client fires a request with a token that
// expires server-side mid-flight.
const TTL_SAFETY_MS = 5_000;

// ErrStepUpCancelled is thrown when the user explicitly cancels
// the biometric prompt. Callers are expected to swallow this and
// abort the action without showing an error toast.
export const ErrStepUpCancelled = Object.assign(new Error('biometric prompt cancelled'), {
  isStepUpCancelled: true as const,
});

// ErrStepUpUnavailable is thrown when the biometric library is
// not available on this device AND the caller passed
// requireBiometric=true. Without that flag we silently degrade.
export const ErrStepUpUnavailable = Object.assign(new Error('biometric authentication unavailable on this device'), {
  isStepUpUnavailable: true as const,
});

interface CachedStepUp {
  token: string;
  // Earliest of (server-side exp − safety buffer, biometric grace
  // window). After this wall-clock instant the cache is void.
  expiresAtMs: number;
}

let cached: CachedStepUp | null = null;

/**
 * clearStepUpCache forgets the last-acquired token. Call from:
 *   - logout flow (auth.tsx logout)
 *   - when the user toggles the require-step-up preference
 *   - on biometric failure mid-action (defensive)
 */
export function clearStepUpCache(): void {
  cached = null;
}

/**
 * peekStepUpToken returns the cached token if still valid, else
 * null. Exposed for tests and for the rare caller that wants to
 * preflight-check whether a re-prompt will be needed (e.g. show
 * "🔒 will require fingerprint" hint on a button label).
 */
export function peekStepUpToken(): string | null {
  if (!cached) return null;
  if (Date.now() >= cached.expiresAtMs) {
    cached = null;
    return null;
  }
  return cached.token;
}

interface AcquireOptions {
  /**
   * Reason shown in the biometric prompt. Localised by the caller
   * (we deliberately don't reach into i18n here to keep this lib
   * UI-agnostic).
   */
  promptReason: string;
  /**
   * Hint sent to the server for audit purposes. "fingerprint" /
   * "face" / "device_credential". Optional — the server doesn't
   * gate on it today.
   */
  biometricKind?: string;
  /**
   * When true, treat biometric-unavailable as a hard error
   * (throws ErrStepUpUnavailable). When false (default), the
   * helper returns undefined so the caller can decide whether to
   * proceed without proof.
   */
  requireBiometric?: boolean;
}

/**
 * acquireStepUpToken returns a fresh-or-cached step-up token.
 *
 * Flow:
 *  1. Return cached token if still valid.
 *  2. Run biometric prompt. Cancel → throw cancelled error.
 *     Unavailable → either degrade to undefined or throw.
 *  3. Call /api/auth/step-up; cache the result with the lesser of
 *     server TTL and BIOMETRIC_GRACE_MS as the expiry.
 *  4. Return the token string.
 */
export async function acquireStepUpToken(opts: AcquireOptions): Promise<string | undefined> {
  const fromCache = peekStepUpToken();
  if (fromCache) return fromCache;

  const ok = await requireBiometrics(opts.promptReason);
  if (ok === false) {
    throw ErrStepUpCancelled;
  }
  if (ok === null) {
    // Biometric library unavailable. Hard-error if the caller
    // demanded one, otherwise quietly skip — caller will fire
    // the action without a step-up token and the server will
    // record step_up=false in audit.
    if (opts.requireBiometric) {
      throw ErrStepUpUnavailable;
    }
    return undefined;
  }

  const resp: StepUpResponse = await apiClient.stepUp(
    opts.biometricKind ? { biometricKind: opts.biometricKind } : undefined,
  );
  const serverTtlMs = Math.max(0, resp.ttl_seconds * 1000 - TTL_SAFETY_MS);
  const expiresAtMs = Date.now() + Math.min(serverTtlMs, BIOMETRIC_GRACE_MS);
  cached = { token: resp.token, expiresAtMs };
  return resp.token;
}

/**
 * withStepUp wraps an async action with step-up acquisition. On
 * a biometric cancel, the action is NOT invoked and the
 * cancellation is re-thrown for the caller's catch to swallow.
 *
 * The fallback path (biometric unavailable + requireBiometric
 * unset) calls the action with `undefined` for the token, leaving
 * the server to record step_up=false in audit but still execute.
 */
export async function withStepUp<T>(
  promptReason: string,
  action: (token: string | undefined) => Promise<T>,
  opts: { biometricKind?: string; requireBiometric?: boolean } = {},
): Promise<T> {
  const token = await acquireStepUpToken({
    promptReason,
    biometricKind: opts.biometricKind,
    requireBiometric: opts.requireBiometric,
  });
  try {
    return await action(token);
  } catch (err) {
    // On 401 we conservatively void the cached token — the
    // server has likely rotated keys or the token expired
    // between cache check and request dispatch. The caller's
    // existing 401 handler still runs (apiClient.onUnauthorized)
    // so logout / re-auth happens via the existing path.
    if (err instanceof ApiError && err.code === 401) {
      clearStepUpCache();
    }
    throw err;
  }
}

/**
 * isStepUpCancelled is a narrow type guard so callers can write
 * `if (isStepUpCancelled(err)) return;` instead of fishing into
 * the error's mutable `.isStepUpCancelled` flag.
 */
export function isStepUpCancelled(err: unknown): boolean {
  return Boolean((err as { isStepUpCancelled?: unknown } | null)?.isStepUpCancelled);
}

export function isStepUpUnavailable(err: unknown): boolean {
  return Boolean((err as { isStepUpUnavailable?: unknown } | null)?.isStepUpUnavailable);
}
