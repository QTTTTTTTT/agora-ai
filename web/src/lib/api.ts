/// <reference types="vite/client" />

// ---------------------------------------------------------------------------
// Shared API type affinity
// ---------------------------------------------------------------------------
//
// We import the wire-shape types from `@fundai/api-client` (the shared
// workspace package that also feeds the Android RN app) and intersect
// them with the additional web-only / admin / KYC fields below. The
// shared package is the SINGLE source of truth for any field that
// both web and Android rely on: if anyone renames `user_id` →
// `userId` over in shared/, the web build breaks immediately because
// our intersection types stop matching the wire payload.
//
// Why intersection (`Shared.X & WebExtras`) instead of duplication:
//   1. shared/ stays minimal — it only declares the 5 core-page
//      endpoints that Android consumes.
//   2. web/ keeps its richer admin / wallet / marketplace / KYC
//      surface area without forcing those fields into RN.
//   3. tsc enforces drift detection at compile time across BOTH
//      workspaces because the same type tokens flow through both.
//
// If you add a field to a `Shared.*` interface, the web side
// inherits it automatically. If you add a web-only field, it lives
// in the `& { ... }` block here so it stays out of Android's bundle.
import type {
  LoginResponse as SharedLoginResponse,
  SessionResponse as SharedSessionResponse,
  LoginInput as SharedLoginInput,
  CorpActionApplication as SharedCorpActionApplication,
  CorpActionListResponse as SharedCorpActionListResponse,
  BenchmarkPoint as SharedBenchmarkPoint,
  BenchmarkSeries as SharedBenchmarkSeries,
  BenchmarkCatalogItem as SharedBenchmarkCatalogItem,
  BenchmarkPartialFailure as SharedBenchmarkPartialFailure,
  BenchmarkHistoryResponse as SharedBenchmarkHistoryResponse,
  BenchmarkHoldingOverlap as SharedBenchmarkHoldingOverlap,
  HoldingSeries as SharedHoldingSeries,
  HoldingsSeriesResponse as SharedHoldingsSeriesResponse,
  ABTestShadowAgent as SharedABTestShadowAgent,
  ABTestShadowAgentVariant as SharedABTestShadowAgentVariant,
  ABTestShadowAgentResponse as SharedABTestShadowAgentResponse,
  ABTestShadowAgentDay as SharedABTestShadowAgentDay,
  ABTestShadowMemory as SharedABTestShadowMemory,
  ABEvolutionConfigDiff as SharedABEvolutionConfigDiff,
  ABAttributionTotals as SharedABAttributionTotals,
  ABAttributionSymbolRow as SharedABAttributionSymbolRow,
  ABTestOperationalAttribution as SharedABTestOperationalAttribution,
} from "@fundai/api-client";
import i18n from "i18next";
import { dispatchSessionExpired } from "./sessionExpiryEvent";
import { toast } from "./toast";

const API_BASE_URL = (import.meta.env.VITE_API_BASE_URL ?? "").replace(/\/$/, "");
const PRIMARY_TOKEN_STORAGE_KEY = "fundai.jwt";
const PRIMARY_SESSION_STORAGE_KEY = "fundai.session";
const PRIMARY_USER_ID_STORAGE_KEY = "fundai.user_id";
const LEGACY_TOKEN_STORAGE_KEYS = ["auth_token", "token"] as const;

export interface AuthSession {
  userId: string;
  email: string;
  displayName: string;
  role: string;
  kycStatus?: string;
  kycLevel?: string;
}

export class ApiError extends Error {
  status: number;
  detail?: string;
  requestId?: string;
  // payload carries the full server-side JSON body when present.
  // Most callers don't care, but a few endpoints (e.g. /assist
  // returning 422 with a structured `issues` list) need access to
  // the raw body to render a typed UI. Stored as `unknown` so any
  // client code that touches it does its own type narrowing.
  payload?: unknown;

  constructor(message: string, status = 500, detail?: string, requestId?: string, payload?: unknown) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.detail = detail;
    this.requestId = requestId;
    this.payload = payload;
  }
}

function normalizeToken(value?: string | null): string {
  return value?.trim() ?? "";
}

function readStoredToken(): string {
  if (typeof window === "undefined") {
    return "";
  }

  const primaryToken = normalizeToken(window.localStorage.getItem(PRIMARY_TOKEN_STORAGE_KEY));
  if (primaryToken) {
    return primaryToken;
  }

  for (const key of LEGACY_TOKEN_STORAGE_KEYS) {
    const legacyToken = normalizeToken(window.localStorage.getItem(key));
    if (!legacyToken) {
      continue;
    }
    window.localStorage.setItem(PRIMARY_TOKEN_STORAGE_KEY, legacyToken);
    return legacyToken;
  }

  return "";
}

export function getApiToken(): string {
  return normalizeToken(import.meta.env.VITE_API_TOKEN) || readStoredToken();
}

export function clearApiToken(): void {
  if (typeof window === "undefined") {
    return;
  }
  window.localStorage.removeItem(PRIMARY_TOKEN_STORAGE_KEY);
  window.localStorage.removeItem(PRIMARY_SESSION_STORAGE_KEY);
  window.localStorage.removeItem(PRIMARY_USER_ID_STORAGE_KEY);
  window.dispatchEvent(new CustomEvent("fundai:session-updated", { detail: { authenticated: false } }));
}

export function storeSession(token: string, session: AuthSession): void {
  if (typeof window === "undefined") {
    return;
  }
  const normalizedToken = normalizeToken(token);
  if (normalizedToken) {
    window.localStorage.setItem(PRIMARY_TOKEN_STORAGE_KEY, normalizedToken);
  }
  const normalizedSession: AuthSession = {
    userId: session.userId.trim(),
    email: session.email.trim(),
    displayName: session.displayName.trim(),
    role: session.role.trim(),
    kycStatus: session.kycStatus?.trim() || undefined,
    kycLevel: session.kycLevel?.trim() || undefined,
  };
  window.localStorage.setItem(PRIMARY_SESSION_STORAGE_KEY, JSON.stringify(normalizedSession));
  window.localStorage.setItem(PRIMARY_USER_ID_STORAGE_KEY, normalizedSession.userId);
  window.dispatchEvent(new CustomEvent("fundai:session-updated", { detail: { authenticated: true, userId: normalizedSession.userId } }));
}

export function getStoredSession(): AuthSession | null {
  if (typeof window === "undefined") {
    return null;
  }
  const raw = window.localStorage.getItem(PRIMARY_SESSION_STORAGE_KEY);
  if (!raw) {
    return null;
  }
  try {
    const parsed = JSON.parse(raw) as Partial<AuthSession>;
    const userId = parsed.userId?.trim() ?? "";
    if (!userId) {
      return null;
    }
    return {
      userId,
      email: parsed.email?.trim() ?? "",
      displayName: parsed.displayName?.trim() ?? "",
      role: parsed.role?.trim() ?? "",
      kycStatus: parsed.kycStatus?.trim() || undefined,
      kycLevel: parsed.kycLevel?.trim() || undefined,
    };
  } catch {
    return null;
  }
}

export function getStoredUserId(): string {
  return getStoredSession()?.userId ?? "";
}

export function getMissingTokenMessage(): string {
  return i18n.t("apiErrors:missingToken");
}

function buildUrl(path: string): string {
  if (/^https?:\/\//.test(path)) {
    return path;
  }
  if (!path.startsWith("/")) {
    path = `/${path}`;
  }
  return `${API_BASE_URL}${path}`;
}

// buildApiUrl is the public alias of buildUrl. Pages that need to construct
// an absolute URL for non-fetch APIs (EventSource, native WebSocket, image
// preloading, ...) should call this helper so the VITE_API_BASE_URL prefix is
// applied consistently.
export function buildApiUrl(path: string): string {
  return buildUrl(path);
}

function normalizeErrorMessage(payload: unknown, fallback: string): { message: string; detail?: string } {
  if (!payload || typeof payload !== "object") {
    return { message: fallback };
  }

  if ("error" in payload && typeof payload.error === "string") {
    const detail = "detail" in payload && typeof payload.detail === "string" ? payload.detail : undefined;
    return { message: payload.error, detail };
  }

  if ("message" in payload && typeof payload.message === "string") {
    const detail = "detail" in payload && typeof payload.detail === "string" ? payload.detail : undefined;
    return { message: payload.message, detail };
  }

  return { message: fallback };
}

function createRequestId(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  return `req_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 10)}`;
}

// DEFAULT_REQUEST_TIMEOUT_MS caps any single API request at 45 seconds.
// We pick 45s instead of a smaller bound because a few legit endpoints
// (decision-trace LLM localisation, full A/B analysis recompute) can
// genuinely take 20-30s end-to-end. Anything beyond 45s is almost
// certainly a hung connection, queue-blocked request, or stalled
// proxy hop — leaving the page in an indefinite spinner is a worse
// UX than surfacing a "timeout, please retry" error. Callers that
// know they need longer can pass `init.signal` themselves; we only
// install our own AbortController when none is supplied.
const DEFAULT_REQUEST_TIMEOUT_MS = 45_000;

export async function apiRequest<T>(path: string, init: RequestInit = {}): Promise<T> {
  const token = getApiToken();
  const headers = new Headers(init.headers ?? {});
  const requestId = headers.get("X-Request-ID") || createRequestId();
  if (token) {
    headers.set("Authorization", `Bearer ${token}`);
  }
  headers.set("X-Request-ID", requestId);
  if (!headers.has("X-User-Language") && typeof window !== "undefined") {
    try {
      const stored = window.localStorage.getItem("fundai.language");
      const language =
        stored === "zh-CN" || stored === "en-US"
          ? stored
          : window.navigator.language?.toLowerCase().startsWith("en")
            ? "en-US"
            : "zh-CN";
      headers.set("X-User-Language", language);
    } catch {
      headers.set("X-User-Language", "zh-CN");
    }
  }

  if (init.body && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }

  // Install a default abort controller so a hung response can't lock
  // the UI forever. We set a flag so the catch block can tell our
  // own timeout-driven abort apart from a caller-supplied AbortSignal
  // (which should surface as the original AbortError, not a "timeout").
  let timeoutId: ReturnType<typeof setTimeout> | null = null;
  let timedOut = false;
  let signal = init.signal ?? undefined;
  if (!signal && typeof AbortController !== "undefined") {
    const controller = new AbortController();
    signal = controller.signal;
    timeoutId = setTimeout(() => {
      timedOut = true;
      controller.abort();
    }, DEFAULT_REQUEST_TIMEOUT_MS);
  }

  // Network-failure path: fetch() rejects on DNS, CORS, offline, TLS,
  // or a hard browser abort. None of those produce a Response object
  // so the !response.ok branch below can't see them. We toast a
  // localised "network error" before rethrowing so every caller of
  // apiRequest gets a uniform user-facing notification without each
  // page having to wrap its own try/catch. The original error is
  // rethrown unchanged so existing inline handlers still get the
  // TypeError they already know how to deal with.
  let response: Response;
  try {
    response = await fetch(buildUrl(path), {
      ...init,
      credentials: init.credentials ?? "include",
      headers,
      signal,
    });
  } catch (err) {
    if (timeoutId !== null) {
      clearTimeout(timeoutId);
    }
    if (timedOut) {
      toast.error(
        { zh: "请求超时", en: "Request timed out" },
        { zh: "服务响应较慢，请稍后重试。", en: "The server is slow to respond. Please retry shortly." },
      );
      throw new ApiError(
        i18n.t("apiErrors:timeout"),
        0,
        `Timed out after ${DEFAULT_REQUEST_TIMEOUT_MS}ms`,
        requestId,
        null,
      );
    }
    toast.error(
      { zh: "网络异常", en: "Network error" },
      { zh: "请检查网络连接后重试。", en: "Check your network connection and try again." },
    );
    throw err;
  }
  if (timeoutId !== null) {
    clearTimeout(timeoutId);
  }

  const responseRequestId = response.headers.get("X-Request-ID") ?? requestId;
  const contentType = response.headers.get("content-type") ?? "";
  const isJSON = contentType.includes("application/json");
  const payload = isJSON ? await response.json().catch(() => null) : await response.text().catch(() => "");

  if (!response.ok) {
    const fallback =
      typeof payload === "string" && payload
        ? payload
        : i18n.t("apiErrors:requestFailedStatus", { status: response.status });
    const normalized = normalizeErrorMessage(payload, fallback);
    if (response.status === 401) {
      clearApiToken();
      // Notify the global SessionExpiryWatcher so the UI can show a
      // friendly toast and soft-navigate to /login carrying ?next=…
      // instead of leaving every page to paint its own raw "session
      // expired" banner. Throwing still happens — callers that already
      // render localised inline error states keep working unchanged.
      dispatchSessionExpired({
        requestId: responseRequestId,
        path,
        reason: "api_request_401",
      });
      throw new ApiError(
        i18n.t("apiErrors:sessionExpired"),
        response.status,
        normalized.detail,
        responseRequestId,
        payload,
      );
    }
    // Server errors (5xx) are infrastructure failures, not business
    // outcomes — every caller benefits from a uniform toast rather
    // than each page re-rendering "请求失败，状态码 502" inline. 4xx
    // (except 401) is intentionally left to the calling page because
    // those typically encode validation / permission feedback the
    // page wants to render against a specific form field.
    if (response.status >= 500) {
      toast.error(
        { zh: "服务暂时不可用", en: "Service temporarily unavailable" },
        normalized.message
          ? normalized.message
          : { zh: `状态码 ${response.status}`, en: `Status ${response.status}` },
      );
    }
    throw new ApiError(normalized.message, response.status, normalized.detail, responseRequestId, payload);
  }

  return payload as T;
}

export function apiGet<T>(path: string): Promise<T> {
  return apiRequest<T>(path);
}

export function apiPost<T>(path: string, body?: unknown): Promise<T> {
  return apiRequest<T>(path, {
    method: "POST",
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

export interface UsageTelemetryInput {
  event_name: "page_view" | "feature_use";
  feature_key: string;
  page_path?: string;
  count?: number;
  metadata?: Record<string, unknown>;
}

export interface UsageTelemetryResponse {
  recorded: boolean;
  reason?: string;
}

export interface AdminUsageFeatureCount {
  feature_key: string;
  event_name: string;
  count: number;
}

export interface AdminUsageUserAggregate {
  user_id: string;
  email: string;
  display_name: string;
  role: string;
  total_events: number;
  page_views: number;
  feature_uses: number;
  active_days: number;
  last_seen_at: string;
  top_features: AdminUsageFeatureCount[];
}

export interface AdminUsageAnalyticsResponse {
  since: string;
  users: AdminUsageUserAggregate[];
}

export function recordUsageTelemetry(input: UsageTelemetryInput): Promise<UsageTelemetryResponse> {
  return apiPost<UsageTelemetryResponse>("/api/telemetry/usage", input);
}

export function fetchAdminUsageAnalytics(params: { since?: string; limit?: number } = {}): Promise<AdminUsageAnalyticsResponse> {
  const qs = new URLSearchParams();
  if (params.since) {
    qs.set("since", params.since);
  }
  if (params.limit && params.limit > 0) {
    qs.set("limit", String(params.limit));
  }
  return apiGet<AdminUsageAnalyticsResponse>(`/api/admin/usage-analytics${qs.toString() ? `?${qs}` : ""}`);
}

export interface WalletAccountResponse {
  wallet: {
    id?: string;
    user_id: string;
    base_currency: string;
    balance_minor: number;
    created_at?: string;
    updated_at?: string;
  };
}

export interface WalletLedgerEntryResponse {
  id: string;
  account_id: string;
  entry_type: string;
  amount_minor: number;
  balance_after_minor: number;
  currency: string;
  reference_type?: string;
  reference_id?: string;
  created_by_user_id?: string;
  metadata?: Record<string, unknown> | null;
  created_at: string;
}

export interface WalletLedgerResponse {
  entries: WalletLedgerEntryResponse[];
  total: number;
  offset: number;
  limit: number;
}

export function apiDelete<T>(path: string): Promise<T> {
  return apiRequest<T>(path, { method: "DELETE" });
}

// LoginResponse extends the shared wire type with the web-only
// KYC + request_id fields the admin surface needs. expires_at is
// non-optional in web (sessions are token-based and the UI relies
// on it to schedule silent refresh) but optional in shared (RN
// can run without it).
export type LoginResponse = SharedLoginResponse & {
  email: string;
  display_name: string;
  role: string;
  kyc_status?: string;
  kyc_level?: string;
  preferred_language?: string;
  expires_at: string;
  request_id?: string;
};

export type SessionResponse = SharedSessionResponse & {
  authenticated: boolean;
  kyc_status?: string;
  kyc_level?: string;
  preferred_language?: string;
  request_id?: string;
  error?: string;
  detail?: string;
};

// AuthPayload extends the shared LoginInput shape with the optional
// displayName field used by the web /register page.
export type AuthPayload = SharedLoginInput & {
  displayName?: string;
};

function persistLogin(payload: LoginResponse): LoginResponse {
  storeSession(payload.token, {
    userId: payload.user_id,
    email: payload.email,
    displayName: payload.display_name,
    role: payload.role,
    kycStatus: payload.kyc_status,
    kycLevel: payload.kyc_level,
  });
  return payload;
}

// LoginOutcome is what login flows return: a finalised session,
// a 2FA challenge for users who have already enrolled, or an
// enrollment requirement for super_admin accounts that have never
// configured 2FA. The `kind` discriminator keeps the consuming UI
// exhaustive — TS will refuse to compile without handling all three.
export type LoginOutcome =
  | { kind: "session"; payload: LoginResponse }
  | { kind: "challenge"; challenge: string; expiresAt: string }
  | { kind: "enrollment_required"; grant: string; expiresAt: string };

async function submitAuth(path: string, body: AuthPayload): Promise<LoginOutcome> {
  const response = await fetch(buildUrl(path), {
    method: "POST",
    credentials: "include",
    headers: {
      "Content-Type": "application/json",
      "X-Request-ID": createRequestId(),
    },
    body: JSON.stringify(body),
  });
  // We accept four flavours of body:
  //   1. classic LoginResponse  → finalise the session.
  //   2. { requires_2fa, challenge, expires_at } → return a
  //      challenge envelope. The caller renders the TOTP prompt
  //      and posts the code to /api/auth/2fa/challenge.
  //   3. { requires_2fa_enrollment, enrollment_grant, expires_at }
  //      → A5: super_admin without 2FA enrolled. The caller pivots
  //      to the enrollment wizard (start → scan QR → verify).
  //   4. error JSON → throw ApiError as before.
  const payload = (await response.json().catch(() => null)) as
    | LoginResponse
    | (TwoFAChallengeResponse & { request_id?: string })
    | (TwoFAEnrollmentRequiredResponse & { request_id?: string })
    | null;
  if (!response.ok) {
    const fallback = i18n.t("apiErrors:loginFailedStatus", { status: response.status });
    const normalized = normalizeErrorMessage(payload, fallback);
    throw new ApiError(normalized.message, response.status, normalized.detail, payload?.request_id);
  }
  if (payload && (payload as TwoFAChallengeResponse).requires_2fa) {
    const ch = payload as TwoFAChallengeResponse;
    return { kind: "challenge", challenge: ch.challenge, expiresAt: ch.expires_at };
  }
  if (payload && (payload as TwoFAEnrollmentRequiredResponse).requires_2fa_enrollment) {
    const en = payload as TwoFAEnrollmentRequiredResponse;
    return { kind: "enrollment_required", grant: en.enrollment_grant, expiresAt: en.expires_at };
  }
  if (!payload || !(payload as LoginResponse).token || !(payload as LoginResponse).user_id) {
    throw new ApiError(
      i18n.t("apiErrors:loginBadResponse"),
      response.status,
      undefined,
      payload?.request_id,
    );
  }
  return { kind: "session", payload: persistLogin(payload as LoginResponse) };
}

// TwoFAEnrollmentRequiredResponse is the A5 fork of TwoFAChallengeResponse.
// Issued at login when the user is super_admin AND has no fully-enrolled
// TOTP row. The grant token is consumed by /api/auth/2fa/enroll-start +
// /api/auth/2fa/enroll-complete and is NOT a session — every other
// endpoint will reject it.
export interface TwoFAEnrollmentRequiredResponse {
  requires_2fa_enrollment: true;
  enrollment_grant: string;
  expires_at: string;
}

// TwoFAEnrollmentStartResponse is the shape /enroll-start returns.
// Identical to the regular /setup response (enrollment is the same
// underlying flow — only the auth mechanism differs).
export interface TwoFAEnrollmentStartResponse {
  secret: string;
  provisioningUri: string;
  recoveryCodes: string[];
  issuer: string;
  accountLabel: string;
  digits: number;
  period: number;
  algorithm: string;
}

// startTwoFAEnrollment kicks off the QR-code + recovery-codes
// phase of forced enrollment. Caller MUST display the recovery
// codes to the user exactly once and tell them to store the codes
// somewhere safe — they will not be shown again.
export async function startTwoFAEnrollment(grant: string): Promise<TwoFAEnrollmentStartResponse> {
  const response = await fetch(buildUrl("/api/auth/2fa/enroll-start"), {
    method: "POST",
    credentials: "include",
    headers: {
      "Content-Type": "application/json",
      "X-Request-ID": createRequestId(),
    },
    body: JSON.stringify({ grant }),
  });
  const payload = (await response.json().catch(() => null)) as
    | TwoFAEnrollmentStartResponse
    | { error?: string; detail?: string; request_id?: string }
    | null;
  if (!response.ok) {
    const normalized = normalizeErrorMessage(payload, "2FA enrollment start failed");
    throw new ApiError(normalized.message, response.status, normalized.detail, (payload as { request_id?: string } | null)?.request_id);
  }
  return payload as TwoFAEnrollmentStartResponse;
}

// completeTwoFAEnrollment verifies the first TOTP code, flips the
// row to enabled, and returns a real session token in the same
// response shape /login would have produced for a non-super-admin
// user. The grant is single-use at the server side — a replay
// after success returns 404 "not_enrolled" (the row is now in the
// enabled-without-pending state).
export async function completeTwoFAEnrollment(grant: string, code: string): Promise<LoginResponse> {
  const response = await fetch(buildUrl("/api/auth/2fa/enroll-complete"), {
    method: "POST",
    credentials: "include",
    headers: {
      "Content-Type": "application/json",
      "X-Request-ID": createRequestId(),
    },
    body: JSON.stringify({ grant, code }),
  });
  const payload = (await response.json().catch(() => null)) as
    | LoginResponse
    | { error?: string; detail?: string; request_id?: string }
    | null;
  if (!response.ok) {
    const normalized = normalizeErrorMessage(payload, "2FA enrollment verify failed");
    throw new ApiError(normalized.message, response.status, normalized.detail, (payload as { request_id?: string } | null)?.request_id);
  }
  if (!payload || !(payload as LoginResponse).token || !(payload as LoginResponse).user_id) {
    throw new ApiError("2FA enrollment finalisation returned an unexpected body", response.status);
  }
  return persistLogin(payload as LoginResponse);
}

export function loginWithPassword(payload: AuthPayload): Promise<LoginOutcome> {
  return submitAuth("/api/auth/login", payload);
}

export function registerWithPassword(payload: AuthPayload): Promise<LoginOutcome> {
  return submitAuth("/api/auth/register", payload);
}

export interface SendVerificationResponse {
  status: string;
  expires_at?: string;
  dev_code?: string;
  request_id?: string;
}

export async function requestEmailVerification(): Promise<SendVerificationResponse> {
  return jsonRequest<SendVerificationResponse>("/api/auth/send-verification", { method: "POST" });
}

export interface VerifyEmailResponse {
  status: string;
  email_verified?: boolean;
  email_verified_at?: string;
  request_id?: string;
}

export async function confirmEmailVerification(code: string): Promise<VerifyEmailResponse> {
  return jsonRequest<VerifyEmailResponse>("/api/auth/verify-email", {
    method: "POST",
    body: JSON.stringify({ code }),
  });
}

export interface ForgotPasswordResponse {
  status: string;
  dev_reset_link?: string;
  request_id?: string;
}

export async function requestPasswordReset(email: string): Promise<ForgotPasswordResponse> {
  return jsonRequest<ForgotPasswordResponse>("/api/auth/forgot-password", {
    method: "POST",
    body: JSON.stringify({ email }),
  });
}

export interface ResetPasswordResponse {
  status: string;
  request_id?: string;
}

export async function confirmPasswordReset(token: string, newPassword: string): Promise<ResetPasswordResponse> {
  return jsonRequest<ResetPasswordResponse>("/api/auth/reset-password", {
    method: "POST",
    body: JSON.stringify({ token, newPassword }),
  });
}

export interface ChangePasswordResponse {
  status: string;
  request_id?: string;
}

export async function changePassword(oldPassword: string, newPassword: string): Promise<ChangePasswordResponse> {
  return jsonRequest<ChangePasswordResponse>("/api/auth/change-password", {
    method: "POST",
    body: JSON.stringify({ oldPassword, newPassword }),
  });
}

async function jsonRequest<T>(path: string, init: RequestInit): Promise<T> {
  const headers = new Headers(init.headers ?? {});
  headers.set("Content-Type", "application/json");
  headers.set("X-Request-ID", createRequestId());
  const token = getApiToken();
  if (token) {
    headers.set("Authorization", `Bearer ${token}`);
  }
  const response = await fetch(buildUrl(path), {
    ...init,
    headers,
    credentials: "include",
  });
  const payload = (await response.json().catch(() => null)) as (T & { error?: string; detail?: string; request_id?: string }) | null;
  if (!response.ok) {
    const fallback = i18n.t("apiErrors:requestFailedStatus", { status: response.status });
    const normalized = normalizeErrorMessage(payload, fallback);
    throw new ApiError(normalized.message, response.status, normalized.detail, payload?.request_id);
  }
  return (payload ?? ({} as T));
}

export async function fetchSession(): Promise<SessionResponse> {
  const headers = new Headers({
    "X-Request-ID": createRequestId(),
  });
  const token = getApiToken();
  if (token) {
    headers.set("Authorization", `Bearer ${token}`);
  }
  const response = await fetch(buildUrl("/api/auth/session"), {
    method: "GET",
    credentials: "include",
    headers,
  });
  const payload = (await response.json().catch(() => null)) as SessionResponse | null;
  if (response.status === 401) {
    clearApiToken();
    return payload ?? { authenticated: false };
  }
  if (!response.ok) {
    clearApiToken();
    const fallback = i18n.t("apiErrors:sessionFailedStatus", { status: response.status });
    const normalized = normalizeErrorMessage(payload, fallback);
    throw new ApiError(normalized.message, response.status, normalized.detail, payload?.request_id);
  }
  if (payload?.authenticated && payload.user_id) {
    const existingToken = getApiToken();
    storeSession(existingToken, {
      userId: payload.user_id,
      email: payload.email ?? "",
      displayName: payload.display_name ?? "",
      role: payload.role ?? "",
      kycStatus: payload.kyc_status,
      kycLevel: payload.kyc_level,
    });
  }
  return payload ?? { authenticated: false };
}

export async function logoutSession(): Promise<void> {
  await fetch(buildUrl("/api/auth/logout"), {
    method: "POST",
    credentials: "include",
    headers: {
      "X-Request-ID": createRequestId(),
    },
  }).catch(() => undefined);
  clearApiToken();
}

export function apiPut<T>(path: string, body?: unknown): Promise<T> {
  return apiRequest<T>(path, {
    method: "PUT",
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

export function apiPatch<T>(path: string, body?: unknown): Promise<T> {
  return apiRequest<T>(path, {
    method: "PATCH",
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

export interface MarketQuote {
  symbol: string;
  instrumentKey?: string;
  market?: string;
  exchange?: string;
  assetClass?: string;
  price: number;
  bid?: number;
  ask?: number;
  volume?: number;
  quoteCurrency?: string;
  asOf: string;
  source: string;
  isStale?: boolean;
}

export interface MarketNewsItem {
  title: string;
  titleZh?: string;
  titleEn?: string;
  summary?: string;
  summaryZh?: string;
  summaryEn?: string;
  url?: string;
  source?: string;
  language?: string;
  publishedAt?: string;
  symbols?: string[];
}

export interface MarketInstrument {
  instrumentKey?: string;
  symbol: string;
  market?: string;
  exchange?: string;
  assetClass?: string;
  instrumentType?: string;
  quoteCurrency?: string;
  settlementCurrency?: string;
  contractMultiplier?: number;
  expiryDate?: string;
}

export interface MarketResearch {
  instrument: MarketInstrument;
  quote?: MarketQuote;
  news?: MarketNewsItem[];
  benchmarkQuote?: MarketQuote;
  signals?: string[];
  summary?: string;
  providerNotes?: string[];
  generatedAt: string;
}

export interface FundMarketQuotes {
  fundId: string;
  quotes: MarketQuote[];
}

export interface MarketNewsDigest {
  fundId: string;
  symbols?: string[];
  items: MarketNewsItem[];
  providerNotes?: string[];
  generatedAt: string;
}

export interface ForwardTrackRecord {
  totalReturn: number;
  annualReturn: number;
  sharpe: number;
  maxDrawdown: number;
  volatility: number;
  winRate: number;
}

export interface ForwardGateCheck {
  key: string;
  label: string;
  status: string;
  required?: number;
  current?: number;
  message?: string;
}

export interface ForwardAgentGateStatus {
  agentId: string;
  agentName?: string;
  role?: string;
  focus?: string;
  status: string;
  eligible: boolean;
  joinedAt?: string;
  checks?: ForwardGateCheck[];
  canList: boolean;
  blockers?: string[];
  warnings?: string[];
}

export interface ForwardGateStatus {
  fundId: string;
  status: string;
  eligible: boolean;
  mode?: string;
  summary?: string;
  requiredDays: number;
  liveDays: number;
  requiredNavs: number;
  navPoints: number;
  startDate?: string;
  endDate?: string;
  trackRecord?: ForwardTrackRecord;
  checks?: ForwardGateCheck[];
  agents?: ForwardAgentGateStatus[];
  generatedAt: string;
}

// Phase 2E: backtest types ---------------------------------------

export interface BacktestInitialPosition {
  symbol: string;
  quantity: number;
  costPrice: number;
}

export interface BacktestSubmitInput {
  name?: string;
  market?: string;
  symbols: string[];
  initialPositions?: BacktestInitialPosition[];
  start: string;
  end: string;
  initialCash: number;
  baseCurrency?: string;
  slippageBps?: number;
  commissionBps?: number;
  maxOrdersPerDay?: number;
  engineKind?: "fallback" | "llm" | "llm-debate" | string;
  walkForward?: WalkForwardInput;
  /** Index ticker to track alongside the strategy. e.g. "SPY", "QQQ", "IWM". */
  benchmarkSymbol?: string;
  /**
   * How often the decision engine fires. "daily" (default) runs every
   * trading day, "monthly" runs on the first trading day of each month —
   * matches the Stage-1 US SaaS rebalance cadence.
   */
  rebalanceFrequency?: "daily" | "weekly" | "monthly" | string;
}

export interface BacktestHistoricalBuySymbol {
  symbol: string;
  market?: string;
  buyCount: number;
  firstBoughtAt?: string;
  lastBoughtAt?: string;
  grossBuyAmount?: number;
}

export interface BacktestHistoricalBuySymbolsResponse {
  symbols: BacktestHistoricalBuySymbol[];
}

export interface WalkForwardInput {
  numFolds: number;
  trainRatio?: number;
  mode?: "anchored" | "rolling" | string;
}

export interface WalkForwardFoldView {
  index: number;
  testStart: string;
  testEnd: string;
  initialNav: number;
  finalNav: number;
  return: number;
  metrics: BacktestMetricsView;
  tradeCount: number;
  error?: string;
}

export interface WalkForwardResultView {
  spec: WalkForwardInput;
  mode: string;
  folds: WalkForwardFoldView[];
  oosReturn: number;
  oosSharpe: number;
  meanFoldReturn: number;
  worstFoldReturn: number;
  bestFoldReturn: number;
  foldBoundaries: number[];
}

export interface BacktestProgressView {
  totalDays: number;
  doneDays: number;
  currentDate?: string;
}

export interface BacktestNavPoint {
  date: string;
  nav: number;
  cash: number;
  positionValue: number;
  drawdownPct: number;
  positions?: Record<string, number>;
}

export interface BacktestTradeEvent {
  date: string;
  symbol: string;
  action: string;
  status: string;
  quantity?: number;
  fillPrice?: number;
  notional?: number;
  reason?: string;
  confidence?: number;
}

export interface BacktestMetricsView {
  cumulativeReturn: number;
  annualizedReturn: number;
  volatility: number;
  sharpeRatio: number;
  maxDrawdown: number;
  winRate: number;
  tradeCount: number;
  winningTradeCount: number;
  losingTradeCount: number;
  /** All zero when the run had no benchmark or only had a 1-day window. */
  benchmarkCumulativeReturn?: number;
  excessReturn?: number;
  excessMaxDrawdown?: number;
  alpha?: number;
  beta?: number;
  trackingError?: number;
  informationRatio?: number;
}

export interface BacktestBenchmarkPoint {
  date: string;
  close: number;
  nav: number;
  pct: number;
}

export interface BacktestResultView {
  initialCash: number;
  finalNav: number;
  navCurve: BacktestNavPoint[];
  trades: BacktestTradeEvent[];
  metrics: BacktestMetricsView;
  completedAt?: string;
  walkForward?: WalkForwardResultView;
  benchmarkSymbol?: string;
  benchmarkCurve?: BacktestBenchmarkPoint[];
}

export interface BacktestRequestEcho {
  symbols: string[];
  start: string;
  end: string;
  initialCash: number;
  baseCurrency?: string;
  slippageBps: number;
  commissionBps: number;
  maxOrdersPerDay: number;
  initialPositions?: BacktestInitialPosition[];
  walkForward?: WalkForwardInput;
  benchmarkSymbol?: string;
  rebalanceFrequency?: string;
}

export interface BacktestJob {
  id: string;
  fundId: string;
  name: string;
  engineKind: string;
  status: "queued" | "running" | "completed" | "failed" | "cancelled" | string;
  progress: BacktestProgressView;
  submittedAt: string;
  startedAt?: string;
  completedAt?: string;
  error?: string;
  result?: BacktestResultView;
  request?: BacktestRequestEcho;
}

export async function submitBacktest(fundId: string, body: BacktestSubmitInput): Promise<BacktestJob> {
  return apiPost<BacktestJob>(`/api/funds/${encodeURIComponent(fundId)}/backtests`, body);
}

export async function listBacktests(fundId: string): Promise<BacktestJob[]> {
  return apiGet<BacktestJob[]>(`/api/funds/${encodeURIComponent(fundId)}/backtests`);
}

export async function listHistoricalBuySymbols(fundId: string, limit = 50): Promise<BacktestHistoricalBuySymbolsResponse> {
  const capped = Math.max(1, Math.min(200, Math.floor(limit)));
  return apiGet<BacktestHistoricalBuySymbolsResponse>(`/api/funds/${encodeURIComponent(fundId)}/backtests/historical-buy-symbols?limit=${capped}`);
}

// ---------------------------------------------------------------------------
// Stage 2 — Factor IC/IR/分层 report
// ---------------------------------------------------------------------------

export interface FactorReportInput {
  factorNames?: string[];
  fixtureName?: string;
  horizons?: number[];
  layeredHorizonDays?: number;
  seedOverride?: number;
  daysOverride?: number;
}

export interface FactorICStats {
  horizonDays: number;
  pearsonSeries: number[];
  spearmanSeries: number[];
  pearsonMean: number;
  pearsonStd: number;
  pearsonIR: number;
  pearsonTStat: number;
  spearmanMean: number;
  spearmanStd: number;
  spearmanIR: number;
  spearmanTStat: number;
  positiveICRatio: number;
}

export interface FactorLayeredResult {
  horizonDays: number;
  quintileMeanReturn: [number, number, number, number, number];
  quintileAnnualReturn: [number, number, number, number, number];
  spread: number;
  spreadAnnual: number;
  spreadTStat: number;
  monotonic: boolean;
  observationPeriods: number;
}

export interface FactorLongShortResult {
  navCurve: { date: string; nav: number }[];
  annualReturn: number;
  annualVol: number;
  sharpe: number;
  maxDrawdown: number;
}

export interface FactorQualificationReport {
  horizonDaysReference: number;
  passesIC: boolean;
  passesIR: boolean;
  passesTStat: boolean;
  passesPositiveRatio: boolean;
  passesLongShort: boolean;
}

export interface FactorReportView {
  factorName: string;
  startDate: string;
  endDate: string;
  universeMedianSize: number;
  observationDays: number;
  ic: Record<string, FactorICStats>;
  layered?: FactorLayeredResult;
  longShort?: FactorLongShortResult;
  qualified: boolean;
  qualReport: FactorQualificationReport;
}

export async function runFactorReport(input: FactorReportInput = {}): Promise<FactorReportView[]> {
  return apiPost<FactorReportView[]>(`/api/factorlab/reports`, input);
}

// --- Walk-forward factor IC stability ---------------------------------------

export interface WalkForwardFactorInput {
  factorName: string;
  numFolds?: number;
  horizons?: number[];
  fixtureName?: string;
  seedOverride?: number;
  daysOverride?: number;
}

export interface FoldICResultView {
  index: number;
  startDate: string;
  endDate: string;
  observationDays: number;
  spearmanMean: number;
  spearmanIR: number;
  spearmanTStat: number;
  positiveICRatio: number;
  longShortSharpe: number;
  longShortAnnual: number;
  layeredSpreadAnnual: number;
  qualified: boolean;
  error?: string;
}

export interface WalkForwardFactorResultView {
  factorName: string;
  numFolds: number;
  folds: FoldICResultView[];
  meanIC22d: number;
  minIC22d: number;
  icStabilityRatio: number;
  allFoldsQualified: boolean;
  qualifiedFoldCount: number;
}

export async function runWalkForwardFactor(input: WalkForwardFactorInput): Promise<WalkForwardFactorResultView> {
  return apiPost<WalkForwardFactorResultView>(`/api/factorlab/walkforward`, input);
}

// ---------------------------------------------------------------------------
// Stage 4 — Paper Trading (tamper-evident performance archive)
// ---------------------------------------------------------------------------

export interface PaperPortfolioInput {
  name: string;
  strategy: string;
  market: string;
  benchmarkSymbol?: string;
  initialCapital: number;
}

export interface PaperPortfolioView {
  id: string;
  name: string;
  strategy: string;
  market: string;
  benchmarkSymbol?: string;
  initialCapital: number;
  currentNav: number;
  cashBalance: number;
  createdAt: string;
  lastRebalanceAt?: string;
}

export interface ProposeOrderInput {
  portfolioId: string;
  symbol: string;
  action: "BUY" | "SELL" | "REBALANCE";
  targetWeight?: number;
  sharesChange?: number;
  decidedPrice?: number;
  aiReasoning?: Record<string, unknown>;
}

export interface PaperOrderView {
  id: string;
  portfolioId: string;
  symbol: string;
  action: "BUY" | "SELL" | "REBALANCE";
  targetWeight?: number;
  sharesChange?: number;
  decidedAt: string;
  decidedPrice?: number;
  executedAt?: string;
  executedPrice?: number;
  aiReasoning?: Record<string, unknown>;
  hashSignature: string;
  canonicalPayload: string;
  publicProofURL?: string;
  otsStatus: "pending" | "submitted" | "confirmed" | "disabled";
}

export interface PaperNavPointView {
  date: string;
  nav: number;
  dailyReturn?: number;
  benchmarkNav?: number;
}

export async function createPaperPortfolio(input: PaperPortfolioInput): Promise<PaperPortfolioView> {
  return apiPost<PaperPortfolioView>(`/api/papertrading/portfolios`, input);
}

export async function listPaperPortfolios(): Promise<PaperPortfolioView[]> {
  return apiGet<PaperPortfolioView[]>(`/api/papertrading/portfolios`);
}

export async function getPaperPortfolio(portfolioId: string): Promise<PaperPortfolioView> {
  return apiGet<PaperPortfolioView>(`/api/papertrading/portfolios/${encodeURIComponent(portfolioId)}`);
}

export async function proposePaperOrder(input: ProposeOrderInput): Promise<PaperOrderView> {
  return apiPost<PaperOrderView>(`/api/papertrading/orders`, input);
}

export async function listPaperOrders(portfolioId: string, limit = 100): Promise<PaperOrderView[]> {
  return apiGet<PaperOrderView[]>(
    `/api/papertrading/portfolios/${encodeURIComponent(portfolioId)}/orders?limit=${limit}`,
  );
}

export async function getPaperNavHistory(portfolioId: string): Promise<PaperNavPointView[]> {
  return apiGet<PaperNavPointView[]>(
    `/api/papertrading/portfolios/${encodeURIComponent(portfolioId)}/nav`,
  );
}

// ---------------------------------------------------------------------------
// Subscription / Pricing — surfaces /api/plans, /api/subscription/*
//
//  - PlanWire mirrors `subscription.Plan` from the Go side.
//  - Pricing 页面固定按 USD (`price_cents_usd_month`) 渲染；老的
//    Subscription.tsx 仍可读 `price_cents_month` 做 CNY 兼容。
//  - createSubscriptionCheckout 创建一个 LemonSqueezy hosted-checkout
//    intent，前端拿到 checkout_url 后跳走，回跳后用 intent_id 轮询。
// ---------------------------------------------------------------------------

export interface PlanWire {
  tier: string;
  name: string;
  price_cents_month: number;       // CNY cents (legacy)
  price_cents_usd_month: number;   // USD cents (new)
  price_cents_usd_year: number;    // USD cents annual (0 = no yearly)
  min_seats: number;               // 1 for personal, 3 for team
  contact_sales: boolean;          // true = render "Contact Sales" instead of checkout
  max_funds: number;
  max_calls_per_day: number;
  model_tiers: string[];
  recommended: boolean;
  max_agents_per_fund: number;
  max_workflow_per_day: number;
  allow_custom_key: boolean;
  allow_ab_test: boolean;
  allow_export: boolean;
  simulation_capital: number;
  included_tokens: number;
  description: string;
}

export interface SubscriptionCheckoutInput {
  tier: "pro" | "premium" | "team";
  billing_period?: "monthly" | "yearly";
  /** Team 档必填，min 3。其它档忽略。 */
  seat_count?: number;
}

export interface SubscriptionCheckoutResponse {
  intent_id: string;
  checkout_url: string;
  expires_at: string;
}

export interface SubscriptionIntentView {
  intent_id: string;
  status: "pending" | "completed" | "expired" | "cancelled";
  plan_tier: string;
  billing_period: string;
  completed_at?: string;
}

export async function listPlans(): Promise<{ plans: PlanWire[] }> {
  return apiGet<{ plans: PlanWire[] }>(`/api/plans`);
}

export async function createSubscriptionCheckout(
  input: SubscriptionCheckoutInput,
): Promise<SubscriptionCheckoutResponse> {
  return apiPost<SubscriptionCheckoutResponse>(`/api/subscription/checkout`, {
    tier: input.tier,
    billing_period: input.billing_period ?? "monthly",
    seat_count: input.seat_count ?? 1,
  });
}

export async function getSubscriptionIntent(
  intentID: string,
): Promise<SubscriptionIntentView> {
  return apiGet<SubscriptionIntentView>(
    `/api/subscription/intent/${encodeURIComponent(intentID)}`,
  );
}

export async function getCustomerPortalURL(): Promise<{ portal_url: string }> {
  return apiGet<{ portal_url: string }>(`/api/subscription/portal`);
}

// ---------------------------------------------------------------------------
// Support contact ("Get help" floating button)
//
//   GET /api/support-contact         — public, drives the global button
//   PUT /api/admin/support-contact   — super_admin, edits the singleton config
// ---------------------------------------------------------------------------

export interface SupportContactView {
  enabled: boolean;
  discordUrl: string;
  qrImageUrl: string;
  message: string;
  updatedAt: string;
}

export type SupportContactInput = Omit<SupportContactView, "updatedAt">;

export async function getSupportContact(): Promise<SupportContactView> {
  return apiGet<SupportContactView>(`/api/support-contact`);
}

export async function updateSupportContact(
  input: SupportContactInput,
): Promise<SupportContactView> {
  return apiPut<SupportContactView>(`/api/admin/support-contact`, input);
}

// ---------------------------------------------------------------------------
// Master-team factor backtest (public, unauthenticated). The /papertrading
// page renders this as a single hero card showing the 10-master ensemble vs
// SPY/QQQ over the 2015→today window. Defaults match the backend handler.
// ---------------------------------------------------------------------------

export interface MasterAnchorView {
  master: string;
  symbol: string;
  style: string;
}

export interface MasterCurvePointView {
  date: string;
  nav: number;
  pct: number;
}

export interface MasterOperationView {
  date: string;
  master: string;
  symbol: string;
  style: string;
  action: "BUY" | "SELL" | string;
  price: number;
  sharesChange: number;
  sharesAfter: number;
  targetWeight: number;
  notional: number;
  accountValue: number;
  cumulativeReturn: number;
}

export interface MasterBenchmarkCurveView {
  symbol: string;
  curve: MasterCurvePointView[];
  cumulativeReturn: number;
}

export interface MasterBacktestMetricsView {
  cumulativeReturn: number;
  annualizedReturn: number;
  volatility: number;
  sharpeRatio: number;
  maxDrawdown: number;
  winRate: number;
}

export interface MasterBacktestResultView {
  strategy: string;
  start: string;
  end: string;
  initialCapital: number;
  finalNav: number;
  universe: MasterAnchorView[];
  operations: MasterOperationView[];
  navCurve: MasterCurvePointView[];
  benchmarks: MasterBenchmarkCurveView[];
  metrics: MasterBacktestMetricsView;
  generatedAt: string;
}

export interface MasterBacktestQuery {
  start?: string; // YYYY-MM-DD
  end?: string;
  initial?: number;
  benchmarks?: string[];
}

export async function getMasterTeamBacktest(
  query: MasterBacktestQuery = {},
): Promise<MasterBacktestResultView> {
  const params = new URLSearchParams();
  if (query.start) params.set("start", query.start);
  if (query.end) params.set("end", query.end);
  if (typeof query.initial === "number" && query.initial > 0) {
    params.set("initial", String(query.initial));
  }
  if (query.benchmarks && query.benchmarks.length > 0) {
    params.set("benchmarks", query.benchmarks.join(","));
  }
  const qs = params.toString();
  const path = `/api/papertrading/public/master-backtest${qs ? `?${qs}` : ""}`;
  return apiGet<MasterBacktestResultView>(path);
}

// ---------------------------------------------------------------------------
// Stage 5 — A-share intraday signal dry-run
// ---------------------------------------------------------------------------

export type CNIntradayMarket = "main_board" | "chinext" | "star" | "st" | "bse";
export type CNIntradayRuleSet = "conservative" | "aggressive";

export interface CNIntradayBarInput {
  timestamp: string; // "YYYY-MM-DD HH:MM" or RFC3339
  open: number;
  high: number;
  low: number;
  close: number;
  volume: number;
  amount?: number;
  bidAskRatio?: number;
  bigOrderNet?: number;
}

export interface CNIntradayDryRunInput {
  symbol: string;
  name?: string;
  market: CNIntradayMarket;
  prevClose: number;
  bars: CNIntradayBarInput[];
  nowBeijing?: string;
  sectorRank?: number;
  ruleSet?: CNIntradayRuleSet;
}

export interface CNIntradaySignalView {
  timestamp: string;
  symbol: string;
  name: string;
  type: "BUY" | "ADD" | "SELL" | "WARNING";
  price: number;
  confidence: number;
  suggestedPosition: number;
  targetPrice: number;
  stopLoss: number;
  reasons: string[];
  riskWarnings: string[];
}

export interface CNIntradayFactorTuple {
  breakout: number;
  volumeSurge: number;
  bigInflow: number;
  orderImbalance: number;
  sectorRank: number;
}

export interface CNIntradayFeishuPreview {
  title: string;
  lines: string[];
}

export interface CNIntradayDryRunResult {
  signal?: CNIntradaySignalView;
  factorScores: CNIntradayFactorTuple;
  feishu?: CNIntradayFeishuPreview;
}

export async function dryRunCNIntradaySignal(input: CNIntradayDryRunInput): Promise<CNIntradayDryRunResult> {
  return apiPost<CNIntradayDryRunResult>(`/api/cnintraday/signals/dry-run`, input);
}

export async function getBacktest(fundId: string, jobId: string): Promise<BacktestJob> {
  return apiGet<BacktestJob>(`/api/funds/${encodeURIComponent(fundId)}/backtests/${encodeURIComponent(jobId)}`);
}

export async function cancelBacktest(fundId: string, jobId: string): Promise<void> {
  await apiPost<{ cancelled: boolean }>(`/api/funds/${encodeURIComponent(fundId)}/backtests/${encodeURIComponent(jobId)}/cancel`);
}

// ---------------------------------------------------------------------------
// P0-5 — Order cancel / replace
// ---------------------------------------------------------------------------

/**
 * OrderActionResponse mirrors the trim wire shape returned by the
 * cancel/replace endpoints. Fields beyond the basics are 0/empty
 * when unset by the underlying order — the UI should treat 0 as
 * "no value" and only render the field when truthy.
 */
export interface OrderActionResponse {
  id: string;
  fundId: string;
  symbol: string;
  side: string;
  orderType: string;
  status: string;
  quantity: number;
  filledQty: number;
  limitPrice?: number;
  stopPrice?: number;
  trailAmount?: number;
  trailPercent?: number;
  displayQty?: number;
  cancelReason?: string;
  replaceCount: number;
}

/** Replace fields. All are optional — nil-as-no-change at the wire. */
export interface ReplaceOrderPayload {
  quantity?: number;
  limitPrice?: number;
  stopPrice?: number;
  trailAmount?: number;
  trailPercent?: number;
  displayQty?: number;
  /** Free-text rationale captured into the audit metadata. */
  note?: string;
}

/**
 * Cancel an order (status pending / working / triggered / partial).
 * Backend rejects terminal orders with 409.
 */
export async function cancelOrder(
  fundId: string,
  tradeId: string,
  options?: { reason?: string; note?: string },
): Promise<OrderActionResponse> {
  const body: Record<string, string> = {};
  if (options?.reason) body.reason = options.reason;
  if (options?.note) body.note = options.note;
  const resp = await apiPost<{ order: OrderActionResponse }>(
    `/api/funds/${encodeURIComponent(fundId)}/orders/${encodeURIComponent(tradeId)}/cancel`,
    Object.keys(body).length > 0 ? body : undefined,
  );
  return resp.order;
}

/**
 * Replace one or more modifiable fields of an open order. At least
 * one of (quantity, limitPrice, stopPrice, trailAmount, trailPercent,
 * displayQty) must be set, or the backend returns 400.
 */
export async function replaceOrder(
  fundId: string,
  tradeId: string,
  payload: ReplaceOrderPayload,
): Promise<OrderActionResponse> {
  const resp = await apiPost<{ order: OrderActionResponse }>(
    `/api/funds/${encodeURIComponent(fundId)}/orders/${encodeURIComponent(tradeId)}/replace`,
    payload,
  );
  return resp.order;
}

// ---------------------------------------------------------------------------
// Live trading hard gate (P0-9)
// ---------------------------------------------------------------------------

/**
 * Wire shape of GET /api/funds/{fundId}/live-readiness. Mirrors the
 * backend's LiveReadiness — see server/cmd/server/live_trading_gate.go
 * for the source-of-truth comments. We deliberately keep the
 * snake_case keys to match the Go JSON encoder.
 */
export interface LiveReadinessResponse {
  trading_mode: string;
  ready: boolean;
  gate_enforced: boolean;
  kyc_ok: boolean;
  broker_link_ok: boolean;
  two_fa_ok: boolean;
  step_up_ok: boolean;
  first_failing?: string;
  broker_link_id?: string;
  step_up_user_id?: string;
}

/**
 * Fetch the per-fund live-trading readiness picture. The web UI
 * uses this to render a checklist before the user attempts a
 * cancel/replace on a live fund. Pass `stepUpToken` if the user
 * just completed a biometric prompt — it's appended as a query
 * parameter so cached page navigations through proxies that
 * strip non-standard headers still work.
 *
 * Errors:
 *   - 401 when not signed in
 *   - 403/404 when the user doesn't own the fund (we map both
 *     to ApiError so the caller can show "fund not found").
 */
export async function getLiveReadiness(
  fundId: string,
  stepUpToken?: string,
): Promise<LiveReadinessResponse> {
  const qs = stepUpToken
    ? `?step_up_token=${encodeURIComponent(stepUpToken)}`
    : "";
  return apiGet<LiveReadinessResponse>(
    `/api/funds/${encodeURIComponent(fundId)}/live-readiness${qs}`,
  );
}

// ---------------------------------------------------------------------------
// Broker link self-service (P1-6)
// ---------------------------------------------------------------------------

// BrokerLinkRow mirrors the wire shape returned by the user-side
// /api/funds/{fundId}/broker-links endpoints (account_id is
// already redacted by the server — never the full value).
export interface BrokerLinkRow {
  id: string;
  fundId: string;
  userId: string;
  brokerId: string;
  accountId: string; // redacted (e.g. "••••4567")
  status: "pending" | "active" | "suspended" | "revoked";
  approvedBy?: string;
  approvedAt?: string;
  createdAt: string;
  updatedAt: string;
}

export interface CreateBrokerLinkPayload {
  brokerId: string;       // one of: ibkr, futu, alpaca, binance, mock
  accountId: string;      // broker-side account id; not a secret
  metadata?: Record<string, unknown>;
}

/**
 * Submit a new broker-link request. The row starts in 'pending'
 * and waits for an admin approval (4-eye check). Backend returns
 * 400 on unknown broker_id.
 */
export async function requestBrokerLink(
  fundId: string,
  payload: CreateBrokerLinkPayload,
): Promise<{ link_id: string; status: BrokerLinkRow["status"] }> {
  return apiPost<{ link_id: string; status: BrokerLinkRow["status"] }>(
    `/api/funds/${encodeURIComponent(fundId)}/broker-links`,
    payload,
  );
}

/**
 * List ALL broker links for the fund (any status), newest first.
 * Used by AccountSecurity to render the per-broker badges.
 */
export async function listBrokerLinks(fundId: string): Promise<BrokerLinkRow[]> {
  const resp = await apiGet<{ links: BrokerLinkRow[] }>(
    `/api/funds/${encodeURIComponent(fundId)}/broker-links`,
  );
  return resp.links ?? [];
}

/**
 * User-side revoke. Moves the link to terminal 'revoked'. The
 * hard gate (P0-9) starts blocking cancel/replace immediately
 * because broker_link_ok flips to false.
 */
export async function revokeBrokerLink(
  fundId: string,
  linkId: string,
): Promise<{ link_id: string; status: BrokerLinkRow["status"] }> {
  return apiPost<{ link_id: string; status: BrokerLinkRow["status"] }>(
    `/api/funds/${encodeURIComponent(fundId)}/broker-links/${encodeURIComponent(linkId)}/revoke`,
  );
}

// ---------------------------------------------------------------------------
// Cash ledger (P1-1)
// ---------------------------------------------------------------------------

// CashLedgerEntry mirrors the JSON projection from
// server/cmd/server/cash_ledger_handler.go. The amount is signed
// (negative = debit / cash out, positive = credit / cash in)
// matching the storage convention.
export interface CashLedgerEntry {
  id: string;
  fund_id: string;
  posted_at: string;
  trading_date?: string;
  entry_type: string;
  amount: number;
  currency: string;
  trade_id?: string;
  plan_id?: string;
  plan_action_id?: string;
  corp_action_id?: string;
  broker_link_id?: string;
  description?: string;
  metadata?: Record<string, unknown>;
  idempotency_key?: string;
  created_at: string;
}

export interface CashLedgerListResponse {
  entries: CashLedgerEntry[];
  next_cursor?: string;
  subtotals?: Record<string, number>;
  balance?: number;
  currency?: string;
}

export interface ListCashLedgerOptions {
  from?: string;
  to?: string;
  types?: string[];
  limit?: number;
  cursor?: string;
  summary?: boolean;
  balance?: boolean;
}

/**
 * Fetch a page of cash-ledger entries. Use `summary: true` /
 * `balance: true` to also pull the subtotals + total balance
 * (extra DB hit each — only set when the UI actually renders
 * those panels).
 */
export async function listCashLedger(
  fundId: string,
  options: ListCashLedgerOptions = {},
): Promise<CashLedgerListResponse> {
  const params = new URLSearchParams();
  if (options.from) params.set("from", options.from);
  if (options.to) params.set("to", options.to);
  if (options.limit) params.set("limit", String(options.limit));
  if (options.cursor) params.set("cursor", options.cursor);
  if (options.summary) params.set("summary", "1");
  if (options.balance) params.set("balance", "1");
  // type=foo&type=bar — repeatable param.
  if (options.types && options.types.length > 0) {
    for (const t of options.types) params.append("type", t);
  }
  const qs = params.toString();
  return apiGet<CashLedgerListResponse>(
    `/api/funds/${encodeURIComponent(fundId)}/cash-ledger${qs ? `?${qs}` : ""}`,
  );
}

// ---------------------------------------------------------------------------
// Funding requests (P1-2)
// ---------------------------------------------------------------------------

export type {
  FundingDirection,
  FundingMethod,
  FundingRequestRow,
  FundingStatus,
} from "@fundai/api-client";

import type {
  FundingDirection as _FundingDirection,
  FundingMethod as _FundingMethod,
  FundingRequestRow as _FundingRequestRow,
  FundingStatus as _FundingStatus,
} from "@fundai/api-client";

export interface CreateFundingRequestInput {
  direction: _FundingDirection;
  amount: number;
  method: _FundingMethod;
  currency?: string;
  externalReference?: string;
  notes?: string;
}

export async function listFundingRequests(
  fundId: string,
  options: { statuses?: _FundingStatus[]; limit?: number } = {},
): Promise<_FundingRequestRow[]> {
  const params = new URLSearchParams();
  if (options.statuses && options.statuses.length > 0) {
    for (const s of options.statuses) params.append("status", s);
  }
  if (options.limit) params.set("limit", String(options.limit));
  const qs = params.toString();
  const resp = await apiGet<{ requests: _FundingRequestRow[] }>(
    `/api/funds/${encodeURIComponent(fundId)}/funding-requests${qs ? `?${qs}` : ""}`,
  );
  return resp.requests;
}

export async function createFundingRequest(
  fundId: string,
  input: CreateFundingRequestInput,
): Promise<{ id: string; status: _FundingStatus }> {
  return apiPost<{ id: string; status: _FundingStatus }>(
    `/api/funds/${encodeURIComponent(fundId)}/funding-requests`,
    input,
  );
}

export async function cancelFundingRequest(
  fundId: string,
  requestId: string,
): Promise<{ status: _FundingStatus }> {
  return apiPost<{ status: _FundingStatus }>(
    `/api/funds/${encodeURIComponent(fundId)}/funding-requests/${encodeURIComponent(requestId)}/cancel`,
  );
}

// ---------------------------------------------------------------------------
// Admin funding (P1-2)
// ---------------------------------------------------------------------------

export type AdminFundingRow = _FundingRequestRow;

export async function listAdminFundingRequests(
  status?: string,
): Promise<{ requests: AdminFundingRow[]; status: string; row_count: number }> {
  const qs = status ? `?status=${encodeURIComponent(status)}` : "";
  return apiGet<{
    requests: AdminFundingRow[];
    status: string;
    row_count: number;
  }>(`/api/admin/funding-requests${qs}`);
}

export async function approveFundingRequest(
  id: string,
  note?: string,
): Promise<{ id: string; status: _FundingStatus; cashLedgerEntryId?: string }> {
  return apiPost<{ id: string; status: _FundingStatus; cashLedgerEntryId?: string }>(
    `/api/admin/funding-requests/${encodeURIComponent(id)}/approve`,
    { note: note ?? "" },
  );
}

export async function rejectFundingRequest(
  id: string,
  reason: string,
): Promise<{ id: string; status: _FundingStatus }> {
  return apiPost<{ id: string; status: _FundingStatus }>(
    `/api/admin/funding-requests/${encodeURIComponent(id)}/reject`,
    { reason },
  );
}

// ---------------------------------------------------------------------------
// FX rates (P1-4)
// ---------------------------------------------------------------------------

// FXRateRow mirrors fxAdminWire on the server. Snake-case rate_at
// matches the wire format the Go handler emits.
export interface FXRateRow {
  base: string;
  quote: string;
  rate: number;
  rate_at: string;
  source: string;
}

export interface ListFXRatesResponse {
  rates: FXRateRow[];
  currencies: string[];
}

// listAdminFXRates fetches the FX-rate table. `pair` accepts the
// "USD/CNY" style filter the GET endpoint understands.
export async function listAdminFXRates(opts: { pair?: string; limit?: number } = {}): Promise<ListFXRatesResponse> {
  const qs = new URLSearchParams();
  if (opts.pair) qs.set("pair", opts.pair);
  if (opts.limit) qs.set("limit", String(opts.limit));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<ListFXRatesResponse>(`/api/admin/fx-rates${tail}`);
}

export interface UpsertFXRateInput {
  base: string;
  quote: string;
  rate: number;
  source?: "manual" | "override";
  rate_at?: string;
  note?: string;
}

// upsertAdminFXRate writes a manual rate. The server emits a
// 400 with a structured ApiError on validation failure (bad
// currency, non-positive rate, bad source) so the form can show
// inline errors per field.
export async function upsertAdminFXRate(input: UpsertFXRateInput): Promise<{ id: string }> {
  return apiPost<{ id: string }>(`/api/admin/fx-rates`, input);
}

// updateFundBaseCurrency posts to the standard fund-update
// endpoint with the new base_currency. We expose a thin helper
// here so the fund-settings UI doesn't have to remember the
// shape.
//
// The server side accepts NULLIF('') so passing the same value
// back is a no-op; the FundsSettings component still gates the
// PUT on a real change to avoid a chatty audit log.
export async function updateFundBaseCurrency(fundId: string, baseCurrency: string): Promise<{ ok: true }> {
  return apiPost<{ ok: true }>(`/api/funds/${encodeURIComponent(fundId)}/settings/base-currency`, {
    base_currency: baseCurrency,
  });
}

// updateFundPreferredLanguage persists a per-fund language override.
// Pass null (or "") to clear the override and let the fund inherit the
// owner's users.preferred_language. The server validates against
// {"zh-CN","en-US"} so anything else returns 400.
export async function updateFundPreferredLanguage(
  fundId: string,
  preferredLanguage: string | null,
): Promise<{ ok: true; preferred_language: string }> {
  return apiPost<{ ok: true; preferred_language: string }>(
    `/api/funds/${encodeURIComponent(fundId)}/settings/preferred-language`,
    { preferred_language: preferredLanguage ?? "" },
  );
}

// updateUserPreferredLanguage flips the authenticated user's language
// preference. The PATCH lands on /api/me/preferences and returns the
// resulting state for the caller to mirror into local stores.
export async function updateUserPreferredLanguage(
  language: "zh-CN" | "en-US",
): Promise<{ preferred_language: string }> {
  return apiPatch<{ preferred_language: string }>(`/api/me/preferences`, {
    language,
  });
}

// ---------------------------------------------------------------------------
// Reconciliation (P1-3)
// ---------------------------------------------------------------------------
//
// The recon admin surface speaks JSON exclusively. Wire types
// mirror the Go side; nullable numerics are surfaced as `number |
// null` because JSON.parse turns `null` into the JS null and
// component code branches on that.

import type {
  ReconciliationRun,
  ReconciliationBreak,
} from "@fundai/api-client";

export type { ReconciliationRun, ReconciliationBreak } from "@fundai/api-client";

export interface ListReconRunsResponse {
  runs: ReconciliationRun[];
}

export interface ListReconBreaksResponse {
  breaks: ReconciliationBreak[];
}

export interface GetReconRunResponse {
  run: ReconciliationRun;
  breaks: ReconciliationBreak[];
}

export interface TriggerReconRunInput {
  fund_id: string;
  as_of_date?: string;
  use_mock_provider: boolean;
  mock_drift_qty?: number;
  mock_drift_cash?: number;
  mock_drift_price?: number;
}

// listAdminReconRuns reads the recent runs feed. fund_id filters
// to one fund; absent → all funds (operator overview).
export async function listAdminReconRuns(opts: { fundId?: string; limit?: number } = {}): Promise<ListReconRunsResponse> {
  const qs = new URLSearchParams();
  if (opts.fundId) qs.set("fund_id", opts.fundId);
  if (opts.limit) qs.set("limit", String(opts.limit));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<ListReconRunsResponse>(`/api/admin/reconciliation/runs${tail}`);
}

export async function getAdminReconRun(runId: string): Promise<GetReconRunResponse> {
  return apiGet<GetReconRunResponse>(`/api/admin/reconciliation/runs/${encodeURIComponent(runId)}`);
}

// listAdminReconBreaks supports the open-breaks dashboard. The
// usual filter is `{ status: 'open', severity: 'critical' }` —
// the server orders rows by severity DESC so the highest-priority
// break lands first.
export async function listAdminReconBreaks(opts: {
  fundId?: string;
  runId?: string;
  status?: "open" | "acknowledged" | "resolved" | "ignored";
  severity?: "info" | "warning" | "critical";
  limit?: number;
} = {}): Promise<ListReconBreaksResponse> {
  const qs = new URLSearchParams();
  if (opts.fundId) qs.set("fund_id", opts.fundId);
  if (opts.runId) qs.set("run_id", opts.runId);
  if (opts.status) qs.set("status", opts.status);
  if (opts.severity) qs.set("severity", opts.severity);
  if (opts.limit) qs.set("limit", String(opts.limit));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<ListReconBreaksResponse>(`/api/admin/reconciliation/breaks${tail}`);
}

// triggerAdminReconRun fires an on-demand run. Currently mock
// provider only; the server enforces this via 400 with code
// `provider_required` if `use_mock_provider` is false.
export async function triggerAdminReconRun(input: TriggerReconRunInput): Promise<GetReconRunResponse> {
  return apiPost<GetReconRunResponse>(`/api/admin/reconciliation/runs`, input);
}

// resolveAdminReconBreak transitions a break out of 'open'. The
// note is recorded on the audit chain.
export async function resolveAdminReconBreak(
  breakId: string,
  status: "open" | "acknowledged" | "resolved" | "ignored",
  note?: string,
): Promise<{ break: ReconciliationBreak }> {
  return apiPost<{ break: ReconciliationBreak }>(
    `/api/admin/reconciliation/breaks/${encodeURIComponent(breakId)}/resolve`,
    { status, note: note ?? "" },
  );
}

// ---------------------------------------------------------------------------
// Trade Surveillance admin (P1-7)
// ---------------------------------------------------------------------------
//
// Mirrors the recon shape: list events / runs, detail, review, scan.
// All endpoints sit under /api/admin/surveillance/* and require the
// admin gate.

import type {
  SurveillanceEvent,
  SurveillanceRun,
  SurveillanceEventStatus,
  SurveillanceRuleCode,
} from "@fundai/api-client";

export type {
  SurveillanceEvent,
  SurveillanceRun,
  SurveillanceEventStatus,
  SurveillanceRuleCode,
} from "@fundai/api-client";

export interface ListSurveillanceEventsResponse {
  events: SurveillanceEvent[];
}

export interface ListSurveillanceRunsResponse {
  runs: SurveillanceRun[];
}

export interface GetSurveillanceEventResponse {
  event: SurveillanceEvent;
}

export interface TriggerSurveillanceScanInput {
  fund_id: string;
  as_of_date?: string;
  session_close_utc?: string;
}

export interface TriggerSurveillanceScanResponse {
  run: SurveillanceRun;
  events: SurveillanceEvent[];
}

// listAdminSurveillanceEvents reads the events feed. Filters are
// composed server-side; status='open' is the dashboard's default
// because everything else has already been triaged.
export async function listAdminSurveillanceEvents(
  opts: {
    fundId?: string;
    ruleCode?: SurveillanceRuleCode;
    status?: SurveillanceEventStatus;
    severity?: "info" | "warning" | "critical";
    limit?: number;
  } = {},
): Promise<ListSurveillanceEventsResponse> {
  const qs = new URLSearchParams();
  if (opts.fundId) qs.set("fund_id", opts.fundId);
  if (opts.ruleCode) qs.set("rule_code", opts.ruleCode);
  if (opts.status) qs.set("status", opts.status);
  if (opts.severity) qs.set("severity", opts.severity);
  if (opts.limit) qs.set("limit", String(opts.limit));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<ListSurveillanceEventsResponse>(`/api/admin/surveillance/events${tail}`);
}

export async function getAdminSurveillanceEvent(
  eventId: string,
): Promise<GetSurveillanceEventResponse> {
  return apiGet<GetSurveillanceEventResponse>(
    `/api/admin/surveillance/events/${encodeURIComponent(eventId)}`,
  );
}

// reviewAdminSurveillanceEvent transitions an event between
// open / reviewing / cleared / escalated. The note is recorded
// on the audit chain.
export async function reviewAdminSurveillanceEvent(
  eventId: string,
  status: SurveillanceEventStatus,
  note?: string,
): Promise<{ event: SurveillanceEvent }> {
  return apiPost<{ event: SurveillanceEvent }>(
    `/api/admin/surveillance/events/${encodeURIComponent(eventId)}/review`,
    { status, note: note ?? "" },
  );
}

export async function listAdminSurveillanceRuns(
  opts: { fundId?: string; limit?: number } = {},
): Promise<ListSurveillanceRunsResponse> {
  const qs = new URLSearchParams();
  if (opts.fundId) qs.set("fund_id", opts.fundId);
  if (opts.limit) qs.set("limit", String(opts.limit));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<ListSurveillanceRunsResponse>(`/api/admin/surveillance/runs${tail}`);
}

export async function triggerAdminSurveillanceScan(
  input: TriggerSurveillanceScanInput,
): Promise<TriggerSurveillanceScanResponse> {
  return apiPost<TriggerSurveillanceScanResponse>(
    `/api/admin/surveillance/scan`,
    input,
  );
}

// ---------------------------------------------------------------------------
// Drawdown soft circuit breaker (P3-5)
// ---------------------------------------------------------------------------
//
// REST surface mirrors the engine: tiered policy CRUD, on-demand
// status preview, on-demand check (records event on breach), event
// list + review.

import type {
  DrawdownPolicy,
  DrawdownTier,
  DrawdownEvent,
  DrawdownEventStatus,
  DrawdownStatus,
} from "@fundai/api-client";

export type {
  DrawdownPolicy,
  DrawdownTier,
  DrawdownEvent,
  DrawdownEventStatus,
  DrawdownStatus,
  DrawdownAction,
} from "@fundai/api-client";

export interface GetDrawdownPolicyResponse {
  policy: DrawdownPolicy;
}

export interface GetDrawdownStatusResponse {
  status: DrawdownStatus;
}

export interface ListDrawdownEventsResponse {
  events: DrawdownEvent[];
}

export interface GetDrawdownEventResponse {
  event: DrawdownEvent;
}

// TriggerDrawdownCheckResponse: when no tier matches we get
// `{breach: false}`; otherwise we get the persisted event.
export interface TriggerDrawdownCheckResponse {
  breach: boolean;
  event_id?: string;
  event?: DrawdownEvent;
}

export async function getAdminDrawdownPolicy(fundID: string): Promise<GetDrawdownPolicyResponse> {
  return apiGet<GetDrawdownPolicyResponse>(
    `/api/admin/drawdown/funds/${encodeURIComponent(fundID)}/policy`,
  );
}

// upsertAdminDrawdownTier writes one tier row. Re-calling for the
// same (fund, tier) updates in place.
export async function upsertAdminDrawdownTier(
  fundID: string,
  tier: DrawdownTier,
): Promise<{ tier: DrawdownTier }> {
  return apiPut<{ tier: DrawdownTier }>(
    `/api/admin/drawdown/funds/${encodeURIComponent(fundID)}/policy/tiers/${tier.tier}`,
    {
      dd_pct: tier.dd_pct,
      action: tier.action,
      trim_ratio: tier.trim_ratio,
      cooldown_hours: tier.cooldown_hours,
      auto_execute: tier.auto_execute,
      note: tier.note ?? "",
    },
  );
}

export async function deleteAdminDrawdownTier(fundID: string, tier: number): Promise<{ ok: boolean }> {
  return apiDelete<{ ok: boolean }>(
    `/api/admin/drawdown/funds/${encodeURIComponent(fundID)}/policy/tiers/${tier}`,
  );
}

export async function getAdminDrawdownStatus(fundID: string): Promise<GetDrawdownStatusResponse> {
  return apiGet<GetDrawdownStatusResponse>(
    `/api/admin/drawdown/funds/${encodeURIComponent(fundID)}/status`,
  );
}

export async function triggerAdminDrawdownCheck(fundID: string): Promise<TriggerDrawdownCheckResponse> {
  return apiPost<TriggerDrawdownCheckResponse>(
    `/api/admin/drawdown/funds/${encodeURIComponent(fundID)}/check`,
    {},
  );
}

export async function listAdminDrawdownEvents(
  opts: {
    fundId?: string;
    status?: DrawdownEventStatus;
    limit?: number;
  } = {},
): Promise<ListDrawdownEventsResponse> {
  const qs = new URLSearchParams();
  if (opts.fundId) qs.set("fund_id", opts.fundId);
  if (opts.status) qs.set("status", opts.status);
  if (opts.limit) qs.set("limit", String(opts.limit));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<ListDrawdownEventsResponse>(`/api/admin/drawdown/events${tail}`);
}

export async function getAdminDrawdownEvent(eventID: string): Promise<GetDrawdownEventResponse> {
  return apiGet<GetDrawdownEventResponse>(
    `/api/admin/drawdown/events/${encodeURIComponent(eventID)}`,
  );
}

// reviewAdminDrawdownEvent transitions an event between proposed /
// approved / dismissed / superseded. The note lands on the audit
// chain. The 'executed' status is reserved for the auto-execute
// worker; the API rejects manual setting of 'executed'.
export async function reviewAdminDrawdownEvent(
  eventID: string,
  status: Exclude<DrawdownEventStatus, "executed">,
  note?: string,
): Promise<{ event: DrawdownEvent }> {
  return apiPost<{ event: DrawdownEvent }>(
    `/api/admin/drawdown/events/${encodeURIComponent(eventID)}/review`,
    { status, note: note ?? "" },
  );
}

// ---------------------------------------------------------------------------
// Market-status gate (S6.1)
// ---------------------------------------------------------------------------

import type {
  MarketStatusInstrument,
  MarketStatusInstrumentState,
  MarketStatusCalendarDay,
  MarketStatusEvent,
  MarketStatusDecision,
  MarketStatusRuleCode,
} from "@fundai/api-client";

export type {
  MarketStatusInstrument,
  MarketStatusInstrumentState,
  MarketStatusCalendarDay,
  MarketStatusEvent,
  MarketStatusDecision,
  MarketStatusRuleCode,
} from "@fundai/api-client";

export interface ListMarketStatusInstrumentsResponse {
  instruments: MarketStatusInstrument[];
}

export interface GetMarketStatusInstrumentResponse {
  instrument: MarketStatusInstrument;
}

export interface ListMarketStatusCalendarResponse {
  days: MarketStatusCalendarDay[];
}

export interface ListMarketStatusEventsResponse {
  events: MarketStatusEvent[];
}

export async function listAdminMarketStatusInstruments(
  opts: { market?: string; status?: MarketStatusInstrumentState; symbol?: string; limit?: number } = {},
): Promise<ListMarketStatusInstrumentsResponse> {
  const qs = new URLSearchParams();
  if (opts.market) qs.set("market", opts.market);
  if (opts.status) qs.set("status", opts.status);
  if (opts.symbol) qs.set("symbol", opts.symbol);
  if (opts.limit) qs.set("limit", String(opts.limit));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<ListMarketStatusInstrumentsResponse>(`/api/admin/marketstatus/instruments${tail}`);
}

export async function getAdminMarketStatusInstrument(key: string): Promise<GetMarketStatusInstrumentResponse> {
  return apiGet<GetMarketStatusInstrumentResponse>(
    `/api/admin/marketstatus/instruments/${encodeURIComponent(key)}`,
  );
}

export interface UpsertMarketStatusInstrumentInput {
  symbol: string;
  market: string;
  status: MarketStatusInstrumentState;
  halt_reason?: string;
  halt_started_at?: string;
  halt_until?: string;
  lower_limit?: number | null;
  upper_limit?: number | null;
  asset_class?: string;
  staleness_budget_seconds?: number | null;
  note?: string;
}

export async function upsertAdminMarketStatusInstrument(
  key: string,
  input: UpsertMarketStatusInstrumentInput,
): Promise<GetMarketStatusInstrumentResponse> {
  return apiPut<GetMarketStatusInstrumentResponse>(
    `/api/admin/marketstatus/instruments/${encodeURIComponent(key)}`,
    input,
  );
}

export async function haltAdminMarketStatusInstrument(
  key: string,
  reason: string,
  haltUntil?: string,
): Promise<GetMarketStatusInstrumentResponse> {
  return apiPost<GetMarketStatusInstrumentResponse>(
    `/api/admin/marketstatus/instruments/${encodeURIComponent(key)}/halt`,
    { reason, halt_until: haltUntil ?? "" },
  );
}

export async function unhaltAdminMarketStatusInstrument(
  key: string,
): Promise<GetMarketStatusInstrumentResponse> {
  return apiPost<GetMarketStatusInstrumentResponse>(
    `/api/admin/marketstatus/instruments/${encodeURIComponent(key)}/unhalt`,
    {},
  );
}

export async function setAdminMarketStatusLimits(
  key: string,
  lower: number | null,
  upper: number | null,
): Promise<GetMarketStatusInstrumentResponse> {
  return apiPost<GetMarketStatusInstrumentResponse>(
    `/api/admin/marketstatus/instruments/${encodeURIComponent(key)}/limits`,
    { lower_limit: lower, upper_limit: upper },
  );
}

export async function listAdminMarketStatusCalendar(
  market: string,
  fromDate?: string,
  toDate?: string,
): Promise<ListMarketStatusCalendarResponse> {
  const qs = new URLSearchParams({ market });
  if (fromDate) qs.set("from", fromDate);
  if (toDate) qs.set("to", toDate);
  return apiGet<ListMarketStatusCalendarResponse>(
    `/api/admin/marketstatus/calendar?${qs.toString()}`,
  );
}

export interface UpsertMarketStatusCalendarInput {
  is_open: boolean;
  open_local?: string;
  close_local?: string;
  market_tz?: string;
  half_day?: boolean;
  note?: string;
}

export async function upsertAdminMarketStatusCalendar(
  market: string,
  date: string,
  input: UpsertMarketStatusCalendarInput,
): Promise<{ ok: boolean }> {
  return apiPut<{ ok: boolean }>(
    `/api/admin/marketstatus/calendar/${encodeURIComponent(market)}/${encodeURIComponent(date)}`,
    input,
  );
}

export async function listAdminMarketStatusEvents(
  opts: {
    fundId?: string;
    instrumentKey?: string;
    ruleCode?: MarketStatusRuleCode;
    decision?: MarketStatusDecision;
    limit?: number;
  } = {},
): Promise<ListMarketStatusEventsResponse> {
  const qs = new URLSearchParams();
  if (opts.fundId) qs.set("fund_id", opts.fundId);
  if (opts.instrumentKey) qs.set("instrument_key", opts.instrumentKey);
  if (opts.ruleCode) qs.set("rule_code", opts.ruleCode);
  if (opts.decision) qs.set("decision", opts.decision);
  if (opts.limit) qs.set("limit", String(opts.limit));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<ListMarketStatusEventsResponse>(`/api/admin/marketstatus/events${tail}`);
}

// ---------------------------------------------------------------------------
// Market-impact / size-aware slippage (S6.2)
// ---------------------------------------------------------------------------

export interface ListMarketImpactInstrumentsResponse {
  instruments: import("@fundai/api-client").MarketImpactInstrument[];
  total: number;
}

export interface GetMarketImpactInstrumentResponse {
  instrument: import("@fundai/api-client").MarketImpactInstrument;
}

export interface UpsertMarketImpactInstrumentInput {
  symbol: string;
  market: string;
  asset_class?: string;
  adv_shares?: number | null;
  adv_notional?: number | null;
  adv_window_days?: number | null;
  daily_volatility?: number | null;
  impact_coefficient?: number | null;
  impact_exponent?: number | null;
  min_slippage_bps?: number | null;
  max_slippage_bps?: number | null;
  last_calibrated_at?: string;
  calibration_source?: import("@fundai/api-client").MarketImpactCalibrationSource;
  note?: string;
}

export async function listAdminMarketImpactInstruments(
  opts: { market?: string; assetClass?: string; limit?: number; offset?: number } = {},
): Promise<ListMarketImpactInstrumentsResponse> {
  const qs = new URLSearchParams();
  if (opts.market) qs.set("market", opts.market);
  if (opts.assetClass) qs.set("asset_class", opts.assetClass);
  if (opts.limit !== undefined) qs.set("limit", String(opts.limit));
  if (opts.offset !== undefined) qs.set("offset", String(opts.offset));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<ListMarketImpactInstrumentsResponse>(`/api/admin/marketimpact/instruments${tail}`);
}

export async function getAdminMarketImpactInstrument(
  key: string,
): Promise<GetMarketImpactInstrumentResponse> {
  return apiGet<GetMarketImpactInstrumentResponse>(
    `/api/admin/marketimpact/instruments/${encodeURIComponent(key)}`,
  );
}

export async function upsertAdminMarketImpactInstrument(
  key: string,
  input: UpsertMarketImpactInstrumentInput,
): Promise<GetMarketImpactInstrumentResponse> {
  return apiPut<GetMarketImpactInstrumentResponse>(
    `/api/admin/marketimpact/instruments/${encodeURIComponent(key)}`,
    input,
  );
}

export async function deleteAdminMarketImpactInstrument(
  key: string,
): Promise<{ ok: boolean }> {
  return apiDelete<{ ok: boolean }>(
    `/api/admin/marketimpact/instruments/${encodeURIComponent(key)}`,
  );
}

export interface MarketImpactPreviewInput {
  instrument_key: string;
  symbol?: string;
  asset_class?: string;
  side: "buy" | "sell";
  quantity: number;
  reference_price: number;
}

export async function previewAdminMarketImpact(
  input: MarketImpactPreviewInput,
): Promise<import("@fundai/api-client").MarketImpactPreviewResponse> {
  return apiPost<import("@fundai/api-client").MarketImpactPreviewResponse>(
    `/api/admin/marketimpact/preview`,
    input,
  );
}

export async function getAdminMarketImpactCacheStats(): Promise<
  import("@fundai/api-client").MarketImpactCacheStats
> {
  return apiGet<import("@fundai/api-client").MarketImpactCacheStats>(
    `/api/admin/marketimpact/cache`,
  );
}

export async function refreshAdminMarketImpactCache(): Promise<
  import("@fundai/api-client").MarketImpactCacheStats & { ok?: boolean }
> {
  return apiPost<
    import("@fundai/api-client").MarketImpactCacheStats & { ok?: boolean }
  >(`/api/admin/marketimpact/cache/refresh`, {});
}

// ---------------------------------------------------------------------------
// IPO / restricted-share lock-ups (S6.3)
// ---------------------------------------------------------------------------

export interface ListAdminLockupsResponse {
  lockups: import("@fundai/api-client").LockupRecord[];
  total: number;
}

export interface GetAdminLockupResponse {
  lockup: import("@fundai/api-client").LockupRecord;
}

export interface CreateLockupInput {
  fund_id: string;
  instrument_key: string;
  symbol: string;
  locked_qty: number;
  locked_until: string;
  reason?: import("@fundai/api-client").LockupReason;
  source_lot_id?: string;
  note?: string;
}

export interface UpdateLockupInput {
  locked_qty?: number;
  locked_until?: string;
  reason?: import("@fundai/api-client").LockupReason;
  note?: string;
}

export async function listAdminLockups(opts: {
  fundId?: string;
  instrumentKey?: string;
  status?: import("@fundai/api-client").LockupStatus;
  limit?: number;
  offset?: number;
} = {}): Promise<ListAdminLockupsResponse> {
  const qs = new URLSearchParams();
  if (opts.fundId) qs.set("fund_id", opts.fundId);
  if (opts.instrumentKey) qs.set("instrument_key", opts.instrumentKey);
  if (opts.status) qs.set("status", opts.status);
  if (opts.limit !== undefined) qs.set("limit", String(opts.limit));
  if (opts.offset !== undefined) qs.set("offset", String(opts.offset));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<ListAdminLockupsResponse>(`/api/admin/lockups${tail}`);
}

export async function getAdminLockup(id: string): Promise<GetAdminLockupResponse> {
  return apiGet<GetAdminLockupResponse>(`/api/admin/lockups/${encodeURIComponent(id)}`);
}

export async function createAdminLockup(
  input: CreateLockupInput,
): Promise<GetAdminLockupResponse> {
  return apiPost<GetAdminLockupResponse>(`/api/admin/lockups`, input);
}

export async function updateAdminLockup(
  id: string,
  input: UpdateLockupInput,
): Promise<GetAdminLockupResponse> {
  return apiPatch<GetAdminLockupResponse>(
    `/api/admin/lockups/${encodeURIComponent(id)}`,
    input,
  );
}

export async function deleteAdminLockup(id: string): Promise<{ ok: boolean }> {
  return apiDelete<{ ok: boolean }>(`/api/admin/lockups/${encodeURIComponent(id)}`);
}

export async function releaseAdminLockup(
  id: string,
  reason: string,
): Promise<GetAdminLockupResponse> {
  return apiPost<GetAdminLockupResponse>(
    `/api/admin/lockups/${encodeURIComponent(id)}/release`,
    { reason },
  );
}

// ---------------------------------------------------------------------------
// Securities borrow / locate (S6.4)
// ---------------------------------------------------------------------------

export interface ListAdminBorrowRatesResponse {
  rates: import("@fundai/api-client").BorrowRate[];
  total: number;
}

export interface UpsertBorrowRateInput {
  instrument_key: string;
  symbol: string;
  market?: string;
  asset_class?: string;
  borrow_rate_bps_annual?: number;
  locate_fee_bps?: number;
  availability?: import("@fundai/api-client").BorrowAvailability;
  available_shares?: number;
  min_locate_qty?: number;
  max_locate_qty?: number;
  source?: import("@fundai/api-client").BorrowCalibrationSource;
  note?: string;
}

export interface BorrowLocatePreviewInput {
  fund_id?: string;
  instrument_key: string;
  requested_qty: number;
  intended_price?: number;
}

export interface ListBorrowLocateEventsResponse {
  events: import("@fundai/api-client").BorrowLocateEvent[];
  total: number;
}

export interface ListBorrowLedgerResponse {
  entries: import("@fundai/api-client").BorrowLedgerEntry[];
  total: number;
}

export async function listAdminBorrowRates(opts: {
  market?: string;
  assetClass?: string;
  availability?: import("@fundai/api-client").BorrowAvailability;
  limit?: number;
  offset?: number;
} = {}): Promise<ListAdminBorrowRatesResponse> {
  const qs = new URLSearchParams();
  if (opts.market) qs.set("market", opts.market);
  if (opts.assetClass) qs.set("asset_class", opts.assetClass);
  if (opts.availability) qs.set("availability", opts.availability);
  if (opts.limit !== undefined) qs.set("limit", String(opts.limit));
  if (opts.offset !== undefined) qs.set("offset", String(opts.offset));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<ListAdminBorrowRatesResponse>(`/api/admin/borrow/rates${tail}`);
}

export async function upsertAdminBorrowRate(
  input: UpsertBorrowRateInput,
): Promise<{ rate: import("@fundai/api-client").BorrowRate }> {
  return apiPost<{ rate: import("@fundai/api-client").BorrowRate }>(
    `/api/admin/borrow/rates`,
    input,
  );
}

export async function deleteAdminBorrowRate(
  instrumentKey: string,
): Promise<{ ok: boolean }> {
  return apiDelete<{ ok: boolean }>(
    `/api/admin/borrow/rates/${encodeURIComponent(instrumentKey)}`,
  );
}

export async function previewAdminBorrowLocate(
  input: BorrowLocatePreviewInput,
): Promise<import("@fundai/api-client").BorrowLocatePreviewResponse> {
  return apiPost<import("@fundai/api-client").BorrowLocatePreviewResponse>(
    `/api/admin/borrow/locate/preview`,
    input,
  );
}

export async function listAdminBorrowLocateEvents(opts: {
  fundId?: string;
  instrumentKey?: string;
  decision?: import("@fundai/api-client").BorrowLocateDecisionKind;
  since?: string;
  limit?: number;
  offset?: number;
} = {}): Promise<ListBorrowLocateEventsResponse> {
  const qs = new URLSearchParams();
  if (opts.fundId) qs.set("fund_id", opts.fundId);
  if (opts.instrumentKey) qs.set("instrument_key", opts.instrumentKey);
  if (opts.decision) qs.set("decision", opts.decision);
  if (opts.since) qs.set("since", opts.since);
  if (opts.limit !== undefined) qs.set("limit", String(opts.limit));
  if (opts.offset !== undefined) qs.set("offset", String(opts.offset));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<ListBorrowLocateEventsResponse>(`/api/admin/borrow/locate/events${tail}`);
}

export async function listAdminBorrowLedger(opts: {
  fundId?: string;
  instrumentKey?: string;
  since?: string;
  until?: string;
  limit?: number;
  offset?: number;
} = {}): Promise<ListBorrowLedgerResponse> {
  const qs = new URLSearchParams();
  if (opts.fundId) qs.set("fund_id", opts.fundId);
  if (opts.instrumentKey) qs.set("instrument_key", opts.instrumentKey);
  if (opts.since) qs.set("since", opts.since);
  if (opts.until) qs.set("until", opts.until);
  if (opts.limit !== undefined) qs.set("limit", String(opts.limit));
  if (opts.offset !== undefined) qs.set("offset", String(opts.offset));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<ListBorrowLedgerResponse>(`/api/admin/borrow/ledger${tail}`);
}

export async function getAdminBorrowCacheStats(): Promise<
  import("@fundai/api-client").BorrowCacheStats
> {
  return apiGet<import("@fundai/api-client").BorrowCacheStats>(`/api/admin/borrow/cache`);
}

export async function refreshAdminBorrowCache(): Promise<
  import("@fundai/api-client").BorrowCacheStats & { ok?: boolean }
> {
  return apiPost<
    import("@fundai/api-client").BorrowCacheStats & { ok?: boolean }
  >(`/api/admin/borrow/cache/refresh`, {});
}

// ---------------------------------------------------------------------------
// WebSocket real-time market data admin (S6.5)
// ---------------------------------------------------------------------------

import type {
  WSFeedStatus,
  WSFeedConnection,
  WSFeedSubscription,
  WSFeedCacheListResponse,
  WSFeedCacheSnapshot,
} from "@fundai/api-client";

export async function getAdminWSFeedStatus(): Promise<WSFeedStatus> {
  return apiGet<WSFeedStatus>(`/api/admin/wsfeed/status`);
}

export async function listAdminWSFeedConnections(): Promise<{
  connections: WSFeedConnection[];
}> {
  return apiGet<{ connections: WSFeedConnection[] }>(
    `/api/admin/wsfeed/connections`
  );
}

export async function listAdminWSFeedSubscriptions(): Promise<{
  subscriptions: WSFeedSubscription[];
}> {
  return apiGet<{ subscriptions: WSFeedSubscription[] }>(
    `/api/admin/wsfeed/subscriptions`
  );
}

export async function listAdminWSFeedCache(): Promise<WSFeedCacheListResponse> {
  return apiGet<WSFeedCacheListResponse>(`/api/admin/wsfeed/cache`);
}

export async function getAdminWSFeedCacheEntry(
  symbol: string,
): Promise<WSFeedCacheSnapshot> {
  return apiGet<WSFeedCacheSnapshot>(
    `/api/admin/wsfeed/cache/${encodeURIComponent(symbol)}`,
  );
}

export async function subscribeAdminWSFeed(
  symbol: string,
  market?: string,
): Promise<{ ok: boolean; symbol: string }> {
  return apiPost<{ ok: boolean; symbol: string }>(
    `/api/admin/wsfeed/subscribe`,
    { symbol, market: market ?? "" },
  );
}

export async function unsubscribeAdminWSFeed(
  symbol: string,
): Promise<{ ok: boolean; symbol: string }> {
  return apiPost<{ ok: boolean; symbol: string }>(
    `/api/admin/wsfeed/unsubscribe`,
    { symbol },
  );
}

export async function evictAdminWSFeedCache(
  symbol: string,
): Promise<{ ok: boolean; evicted: number }> {
  return apiPost<{ ok: boolean; evicted: number }>(
    `/api/admin/wsfeed/cache/evict`,
    { symbol },
  );
}

export async function reconcileAdminWSFeed(): Promise<{ ok: boolean }> {
  return apiPost<{ ok: boolean }>(`/api/admin/wsfeed/reconcile`, {});
}

// ---------------------------------------------------------------------------
// Factor exposure (S7 / P3-1)
// ---------------------------------------------------------------------------

export type {
  Factor,
  LoadingSource as FactorLoadingSource,
  InstrumentFactorLoading,
  FactorExposureRow,
  FactorExposureSnapshot,
  FactorExposureTrendPoint,
} from "@fundai/api-client";
export { ALL_FACTORS } from "@fundai/api-client";

export interface ListAdminFactorLoadingsResponse {
  loadings: import("@fundai/api-client").InstrumentFactorLoading[];
  factors: import("@fundai/api-client").Factor[];
  row_count: number;
}

export interface UpsertFactorLoadingInput {
  instrument_key: string;
  factor: import("@fundai/api-client").Factor;
  asof: string;
  loading: number;
  source?: import("@fundai/api-client").LoadingSource;
  note?: string;
}

// listAdminFactorLoadings supports filtering by factor or
// instrument_key; default limit on the server is 200, max 1000.
export async function listAdminFactorLoadings(opts: {
  factor?: import("@fundai/api-client").Factor;
  instrumentKey?: string;
  limit?: number;
  offset?: number;
} = {}): Promise<ListAdminFactorLoadingsResponse> {
  const qs = new URLSearchParams();
  if (opts.factor) qs.set("factor", opts.factor);
  if (opts.instrumentKey) qs.set("instrument_key", opts.instrumentKey);
  if (opts.limit !== undefined) qs.set("limit", String(opts.limit));
  if (opts.offset !== undefined) qs.set("offset", String(opts.offset));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<ListAdminFactorLoadingsResponse>(
    `/api/admin/factor-loadings${tail}`,
  );
}

export async function upsertAdminFactorLoading(
  input: UpsertFactorLoadingInput,
): Promise<{ loading: import("@fundai/api-client").InstrumentFactorLoading }> {
  return apiPost<{ loading: import("@fundai/api-client").InstrumentFactorLoading }>(
    `/api/admin/factor-loadings`,
    input,
  );
}

export async function deleteAdminFactorLoading(opts: {
  instrumentKey: string;
  factor: import("@fundai/api-client").Factor;
  asof: string;
}): Promise<{ deleted: boolean }> {
  const qs = new URLSearchParams({
    instrument_key: opts.instrumentKey,
    factor: opts.factor,
    asof: opts.asof,
  });
  return apiDelete<{ deleted: boolean }>(
    `/api/admin/factor-loadings?${qs.toString()}`,
  );
}

// fetchFactorExposureSnapshot returns the live factor-exposure
// view for a fund; pass persist=true to archive the snapshot in
// the same round trip.
export async function fetchFactorExposureSnapshot(
  fundId: string,
  opts: { persist?: boolean } = {},
): Promise<{
  snapshot: import("@fundai/api-client").FactorExposureSnapshot;
  factors: import("@fundai/api-client").Factor[];
  persist_error?: string;
}> {
  const qs = opts.persist ? "?persist=1" : "";
  return apiGet<{
    snapshot: import("@fundai/api-client").FactorExposureSnapshot;
    factors: import("@fundai/api-client").Factor[];
    persist_error?: string;
  }>(`/api/funds/${encodeURIComponent(fundId)}/risk/factor-exposure${qs}`);
}

export async function fetchFactorExposureTrend(
  fundId: string,
  opts: { factor?: import("@fundai/api-client").Factor; limit?: number } = {},
): Promise<{
  points: import("@fundai/api-client").FactorExposureTrendPoint[];
  factors: import("@fundai/api-client").Factor[];
}> {
  const qs = new URLSearchParams();
  if (opts.factor) qs.set("factor", opts.factor);
  if (opts.limit !== undefined) qs.set("limit", String(opts.limit));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<{
    points: import("@fundai/api-client").FactorExposureTrendPoint[];
    factors: import("@fundai/api-client").Factor[];
  }>(`/api/funds/${encodeURIComponent(fundId)}/risk/factor-exposure/trend${tail}`);
}

// ---------------------------------------------------------------------------
// VaR / CVaR (S7 / P3-2)
// ---------------------------------------------------------------------------

export type {
  VaRMethod,
  VaRConfidence,
  VaRResult,
  VaRSnapshot,
  VaRTrendPoint,
} from "@fundai/api-client";
export { ALL_VAR_METHODS, ALL_VAR_CONFIDENCES } from "@fundai/api-client";

// fetchVaRSnapshot pulls the live (method × confidence) tile set
// for one fund. lookback / horizon mirror the backend query
// surface; persist=true archives the snapshot in the same call.
export async function fetchVaRSnapshot(
  fundId: string,
  opts: {
    lookback?: number;
    horizon?: number;
    persist?: boolean;
  } = {},
): Promise<{
  snapshot: import("@fundai/api-client").VaRSnapshot;
  methods: import("@fundai/api-client").VaRMethod[];
  confidences: import("@fundai/api-client").VaRConfidence[];
  persist_error?: string;
}> {
  const qs = new URLSearchParams();
  if (opts.lookback !== undefined) qs.set("lookback", String(opts.lookback));
  if (opts.horizon !== undefined) qs.set("horizon", String(opts.horizon));
  if (opts.persist) qs.set("persist", "1");
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<{
    snapshot: import("@fundai/api-client").VaRSnapshot;
    methods: import("@fundai/api-client").VaRMethod[];
    confidences: import("@fundai/api-client").VaRConfidence[];
    persist_error?: string;
  }>(`/api/funds/${encodeURIComponent(fundId)}/risk/var${tail}`);
}

export async function fetchVaRTrend(
  fundId: string,
  opts: {
    method: import("@fundai/api-client").VaRMethod;
    confidence: import("@fundai/api-client").VaRConfidence;
    horizon?: number;
    limit?: number;
  },
): Promise<{
  points: import("@fundai/api-client").VaRTrendPoint[];
  methods: import("@fundai/api-client").VaRMethod[];
  confidences: import("@fundai/api-client").VaRConfidence[];
}> {
  const qs = new URLSearchParams();
  qs.set("method", opts.method);
  qs.set("confidence", String(opts.confidence));
  qs.set("horizon", String(opts.horizon ?? 1));
  if (opts.limit !== undefined) qs.set("limit", String(opts.limit));
  return apiGet<{
    points: import("@fundai/api-client").VaRTrendPoint[];
    methods: import("@fundai/api-client").VaRMethod[];
    confidences: import("@fundai/api-client").VaRConfidence[];
  }>(`/api/funds/${encodeURIComponent(fundId)}/risk/var/trend?${qs.toString()}`);
}

// ---------------------------------------------------------------------------
// Stress scenarios + per-fund runs (S7 / P3-3)
// ---------------------------------------------------------------------------

export type {
  StressCategory,
  StressShockTargetType,
  StressShock,
  StressScenario,
  StressImpact,
  StressResult,
} from "@fundai/api-client";
export {
  ALL_STRESS_CATEGORIES,
  ALL_STRESS_TARGET_TYPES,
} from "@fundai/api-client";

// Admin: list scenarios with optional category filter.
export async function listAdminStressScenarios(opts: {
  category?: import("@fundai/api-client").StressCategory;
} = {}): Promise<{
  scenarios: import("@fundai/api-client").StressScenario[];
  categories: import("@fundai/api-client").StressCategory[];
}> {
  const qs = opts.category ? `?category=${encodeURIComponent(opts.category)}` : "";
  return apiGet<{
    scenarios: import("@fundai/api-client").StressScenario[];
    categories: import("@fundai/api-client").StressCategory[];
  }>(`/api/admin/stress-scenarios${qs}`);
}

export interface UpsertStressScenarioInput {
  name: string;
  category: import("@fundai/api-client").StressCategory;
  description?: string;
  shocks: import("@fundai/api-client").StressShock[];
}

export async function upsertAdminStressScenario(
  input: UpsertStressScenarioInput,
): Promise<{ scenario: import("@fundai/api-client").StressScenario }> {
  return apiPost<{ scenario: import("@fundai/api-client").StressScenario }>(
    `/api/admin/stress-scenarios`,
    input,
  );
}

export async function deleteAdminStressScenario(
  id: string,
): Promise<{ deleted: boolean }> {
  return apiDelete<{ deleted: boolean }>(
    `/api/admin/stress-scenarios/${encodeURIComponent(id)}`,
  );
}

// Per-fund stress run. POST so it's never cached.
// Note: the api-contract validator's inferMethod only scans the
// 2 lines above each URL literal, so we keep apiPost< T > and
// the URL on adjacent lines here.
export interface StressRunResponse {
  result: import("@fundai/api-client").StressResult;
  scenario: import("@fundai/api-client").StressScenario;
  persist_error?: string;
}
export async function runFundStressScenario(
  fundId: string,
  opts: { scenarioId: string; persist?: boolean },
): Promise<StressRunResponse> {
  return apiPost<StressRunResponse>(`/api/funds/${encodeURIComponent(fundId)}/risk/stress`,
    { scenario_id: opts.scenarioId, persist: !!opts.persist });
}

export async function fetchFundStressHistory(
  fundId: string,
  opts: { scenarioId?: string; limit?: number } = {},
): Promise<{
  results: import("@fundai/api-client").StressResult[];
}> {
  const qs = new URLSearchParams();
  if (opts.scenarioId) qs.set("scenarioId", opts.scenarioId);
  if (opts.limit !== undefined) qs.set("limit", String(opts.limit));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<{
    results: import("@fundai/api-client").StressResult[];
  }>(`/api/funds/${encodeURIComponent(fundId)}/risk/stress/history${tail}`);
}

// Public list of scenarios for the fund-level dropdown. Backed
// by the GET /api/risk/stress-scenarios route which is open to
// every authenticated user — mutations remain admin-only.
export async function listStressScenariosForFund(opts: {
  category?: import("@fundai/api-client").StressCategory;
} = {}): Promise<{
  scenarios: import("@fundai/api-client").StressScenario[];
}> {
  const qs = opts.category ? `?category=${encodeURIComponent(opts.category)}` : "";
  return apiGet<{
    scenarios: import("@fundai/api-client").StressScenario[];
  }>(`/api/risk/stress-scenarios${qs}`);
}

// ---------------------------------------------------------------------------
// Brinson attribution (S7 / P3-4)
// ---------------------------------------------------------------------------

export type {
  BrinsonBucketDimension,
  BrinsonBucket,
  BrinsonComposition,
  BrinsonBenchmarkSummary,
  BrinsonBucketAttribution,
  BrinsonResult,
  BrinsonHistoryEntry,
} from "@fundai/api-client";
export {
  ALL_BRINSON_DIMENSIONS,
} from "@fundai/api-client";

// Admin: list benchmark compositions.
export async function listAdminBrinsonCompositions(opts: {
  benchmarkId?: string;
  dimension?: import("@fundai/api-client").BrinsonBucketDimension;
  limit?: number;
} = {}): Promise<{
  compositions: import("@fundai/api-client").BrinsonComposition[];
  dimensions: import("@fundai/api-client").BrinsonBucketDimension[];
}> {
  const qs = new URLSearchParams();
  if (opts.benchmarkId) qs.set("benchmarkId", opts.benchmarkId);
  if (opts.dimension) qs.set("dimension", opts.dimension);
  if (opts.limit !== undefined) qs.set("limit", String(opts.limit));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<{
    compositions: import("@fundai/api-client").BrinsonComposition[];
    dimensions: import("@fundai/api-client").BrinsonBucketDimension[];
  }>(`/api/admin/brinson-compositions${tail}`);
}

export interface UpsertBrinsonCompositionInput {
  benchmark_id: string;
  dimension: import("@fundai/api-client").BrinsonBucketDimension;
  asof: string; // YYYY-MM-DD
  buckets: import("@fundai/api-client").BrinsonBucket[];
  note?: string;
}

export async function upsertAdminBrinsonComposition(
  input: UpsertBrinsonCompositionInput,
): Promise<{ composition: import("@fundai/api-client").BrinsonComposition }> {
  return apiPost<{
    composition: import("@fundai/api-client").BrinsonComposition;
  }>(`/api/admin/brinson-compositions`, input);
}

export async function deleteAdminBrinsonComposition(
  id: string,
): Promise<{ deleted: boolean }> {
  return apiDelete<{ deleted: boolean }>(
    `/api/admin/brinson-compositions/${encodeURIComponent(id)}`,
  );
}

// Catalog of saved benchmarks for fund operators. Authenticated
// users only — admins still own the mutations.
export async function listBrinsonBenchmarksForFund(): Promise<{
  benchmarks: import("@fundai/api-client").BrinsonBenchmarkSummary[];
  dimensions: import("@fundai/api-client").BrinsonBucketDimension[];
}> {
  return apiGet<{
    benchmarks: import("@fundai/api-client").BrinsonBenchmarkSummary[];
    dimensions: import("@fundai/api-client").BrinsonBucketDimension[];
  }>(`/api/brinson/benchmarks`);
}

// Per-fund Brinson run. POST so it's never cached.
// Note: the api-contract validator's inferMethod only scans the
// 2 lines above each URL literal, so we keep apiPost< T > and
// the URL on adjacent lines here.
export interface BrinsonRunResponse {
  result: import("@fundai/api-client").BrinsonResult;
  composition_id: string;
  persist_error?: string;
}
export async function runFundBrinsonAttribution(
  fundId: string,
  opts: {
    benchmarkId: string;
    dimension: import("@fundai/api-client").BrinsonBucketDimension;
    compositionId?: string;
    asof?: string;
    persist?: boolean;
  },
): Promise<BrinsonRunResponse> {
  return apiPost<BrinsonRunResponse>(`/api/funds/${encodeURIComponent(fundId)}/brinson/run`,
    {
      benchmark_id: opts.benchmarkId,
      dimension: opts.dimension,
      composition_id: opts.compositionId,
      asof: opts.asof,
      persist: !!opts.persist,
    });
}

export async function fetchFundBrinsonHistory(
  fundId: string,
  opts: {
    benchmarkId?: string;
    dimension?: import("@fundai/api-client").BrinsonBucketDimension;
    limit?: number;
  } = {},
): Promise<{
  results: import("@fundai/api-client").BrinsonHistoryEntry[];
}> {
  const qs = new URLSearchParams();
  if (opts.benchmarkId) qs.set("benchmarkId", opts.benchmarkId);
  if (opts.dimension) qs.set("dimension", opts.dimension);
  if (opts.limit !== undefined) qs.set("limit", String(opts.limit));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<{
    results: import("@fundai/api-client").BrinsonHistoryEntry[];
  }>(`/api/funds/${encodeURIComponent(fundId)}/brinson/history${tail}`);
}

// ---------------------------------------------------------------------------
// S8.1 — Analyst panel (fundamentals / sentiment / news / technical)
// ---------------------------------------------------------------------------
//
// Re-export the wire types so web component imports don't have
// to know about @fundai/api-client.
export type {
  AnalystCategory,
  AnalystDirection,
  AnalystDataPoint,
  AnalystReport,
  AnalystPanelReport,
  AnalystQualityScoreInput,
  AnalystFundamentalsInput,
  AnalystSentimentAggregateInput,
  AnalystSentimentItemInput,
  AnalystSentimentInput,
  AnalystNewsHeadlineInput,
  AnalystNewsInput,
  AnalystQuantSnapshotInput,
  AnalystTechnicalInput,
  AnalystRunRequest,
} from "@fundai/api-client";
export { ALL_ANALYST_CATEGORIES } from "@fundai/api-client";

// Per-fund analyst panel run. POST so the persist side-effect
// isn't cached. Keep apiPost< T > and the URL on adjacent lines
// so the api-contract validator's inferMethod resolves POST.
export interface AnalystPanelRunResponse {
  panel: import("@fundai/api-client").AnalystPanelReport;
  persist_error?: string;
}
export async function runFundAnalystPanel(
  fundId: string,
  body: import("@fundai/api-client").AnalystRunRequest,
): Promise<AnalystPanelRunResponse> {
  return apiPost<AnalystPanelRunResponse>(`/api/funds/${encodeURIComponent(fundId)}/analysts/run`,
    body);
}

export async function listFundAnalystPanels(
  fundId: string,
  opts: {
    symbol?: string;
    from?: string;
    to?: string;
    limit?: number;
    includeChildren?: boolean;
  } = {},
): Promise<{
  panels: import("@fundai/api-client").AnalystPanelReport[];
}> {
  const qs = new URLSearchParams();
  if (opts.symbol) qs.set("symbol", opts.symbol);
  if (opts.from) qs.set("from", opts.from);
  if (opts.to) qs.set("to", opts.to);
  if (opts.limit !== undefined) qs.set("limit", String(opts.limit));
  if (opts.includeChildren) qs.set("include", "children");
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<{
    panels: import("@fundai/api-client").AnalystPanelReport[];
  }>(`/api/funds/${encodeURIComponent(fundId)}/analysts/panels${tail}`);
}

export async function getFundAnalystPanel(
  fundId: string,
  panelId: string,
): Promise<{
  panel: import("@fundai/api-client").AnalystPanelReport;
}> {
  return apiGet<{
    panel: import("@fundai/api-client").AnalystPanelReport;
  }>(`/api/funds/${encodeURIComponent(fundId)}/analysts/panels/${encodeURIComponent(panelId)}`);
}

// ---------------------------------------------------------------------------
// S8.2 — Bull / Bear debate
// ---------------------------------------------------------------------------

export type {
  AdvocateStance,
  DebateArgument,
  DebateVerdict,
  DebateTranscript,
  DebateRunRequest,
} from "@fundai/api-client";
export { ALL_ADVOCATE_STANCES } from "@fundai/api-client";

// Per-fund Bull/Bear debate run. Same flatten pattern as the
// analyst panel runner so the api-contract validator's
// inferMethod resolves POST.
export interface DebateRunResponse {
  debate: import("@fundai/api-client").DebateTranscript;
  persist_error?: string;
}
export async function runFundDebate(
  fundId: string,
  body: import("@fundai/api-client").DebateRunRequest,
): Promise<DebateRunResponse> {
  return apiPost<DebateRunResponse>(`/api/funds/${encodeURIComponent(fundId)}/debates/run`,
    body);
}

export async function listFundDebates(
  fundId: string,
  opts: {
    symbol?: string;
    from?: string;
    to?: string;
    limit?: number;
  } = {},
): Promise<{
  debates: import("@fundai/api-client").DebateTranscript[];
}> {
  const qs = new URLSearchParams();
  if (opts.symbol) qs.set("symbol", opts.symbol);
  if (opts.from) qs.set("from", opts.from);
  if (opts.to) qs.set("to", opts.to);
  if (opts.limit !== undefined) qs.set("limit", String(opts.limit));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<{
    debates: import("@fundai/api-client").DebateTranscript[];
  }>(`/api/funds/${encodeURIComponent(fundId)}/debates${tail}`);
}

export async function getFundDebate(
  fundId: string,
  debateId: string,
): Promise<{
  debate: import("@fundai/api-client").DebateTranscript;
}> {
  return apiGet<{
    debate: import("@fundai/api-client").DebateTranscript;
  }>(`/api/funds/${encodeURIComponent(fundId)}/debates/${encodeURIComponent(debateId)}`);
}

// ---------------------------------------------------------------------------
// S8.4 — Agent reputation ledger
// ---------------------------------------------------------------------------

export type {
  AgentReputationKind,
  AgentReputationStats,
  AgentReputationOutcome,
  AgentReputationRebuildRequest,
  AgentReputationRebuildResponse,
} from "@fundai/api-client";
export { ALL_AGENT_REPUTATION_KINDS } from "@fundai/api-client";

export async function listFundAgentReputationStats(
  fundId: string,
  opts: {
    kind?: import("@fundai/api-client").AgentReputationKind;
    limit?: number;
  } = {},
): Promise<{
  stats: import("@fundai/api-client").AgentReputationStats[];
}> {
  const qs = new URLSearchParams();
  if (opts.kind) qs.set("kind", opts.kind);
  if (opts.limit !== undefined) qs.set("limit", String(opts.limit));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<{
    stats: import("@fundai/api-client").AgentReputationStats[];
  }>(`/api/funds/${encodeURIComponent(fundId)}/agent-reputation/stats${tail}`);
}

export async function listFundAgentReputationOutcomes(
  fundId: string,
  opts: {
    agentId?: string;
    symbol?: string;
    limit?: number;
  } = {},
): Promise<{
  outcomes: import("@fundai/api-client").AgentReputationOutcome[];
}> {
  const qs = new URLSearchParams();
  if (opts.agentId) qs.set("agent_id", opts.agentId);
  if (opts.symbol) qs.set("symbol", opts.symbol);
  if (opts.limit !== undefined) qs.set("limit", String(opts.limit));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<{
    outcomes: import("@fundai/api-client").AgentReputationOutcome[];
  }>(`/api/funds/${encodeURIComponent(fundId)}/agent-reputation/outcomes${tail}`);
}

export async function listAdminAgentReputationStats(opts: {
  fundId?: string;
  kind?: import("@fundai/api-client").AgentReputationKind;
  limit?: number;
} = {}): Promise<{
  stats: import("@fundai/api-client").AgentReputationStats[];
}> {
  const qs = new URLSearchParams();
  if (opts.fundId) qs.set("fund_id", opts.fundId);
  if (opts.kind) qs.set("kind", opts.kind);
  if (opts.limit !== undefined) qs.set("limit", String(opts.limit));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<{
    stats: import("@fundai/api-client").AgentReputationStats[];
  }>(`/api/admin/agent-reputation/stats${tail}`);
}

export async function rebuildAgentReputation(
  body: import("@fundai/api-client").AgentReputationRebuildRequest = {},
): Promise<import("@fundai/api-client").AgentReputationRebuildResponse> {
  return apiPost<import("@fundai/api-client").AgentReputationRebuildResponse>(
    `/api/admin/agent-reputation/rebuild`,
    body,
  );
}

// ---------------------------------------------------------------------------
// Workflow checkpoints (S9.2)
// ---------------------------------------------------------------------------

export async function listWorkflowCheckpoints(opts: {
  runId?: string;
  fundId?: string;
  tradingDate?: string;
} = {}): Promise<import("@fundai/api-client").ListWorkflowCheckpointsResponse> {
  const qs = new URLSearchParams();
  if (opts.runId) qs.set("run_id", opts.runId);
  if (opts.fundId) qs.set("fund_id", opts.fundId);
  if (opts.tradingDate) qs.set("trading_date", opts.tradingDate);
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<import("@fundai/api-client").ListWorkflowCheckpointsResponse>(
    `/api/admin/workflow-checkpoints${tail}`,
  );
}

export async function resumeWorkflowCheckpoint(
  body: import("@fundai/api-client").ResumeWorkflowCheckpointRequest,
): Promise<import("@fundai/api-client").ResumeWorkflowCheckpointResponse> {
  return apiPost<import("@fundai/api-client").ResumeWorkflowCheckpointResponse>(
    `/api/admin/workflow-checkpoints/resume`,
    body,
  );
}

// listFundWorkflowCheckpoints is the read-only sibling of the admin
// endpoint, scoped to a single fund and authenticated via the
// fund-ownership chain (see fund_workflow_checkpoints_handler.go).
// Resume is intentionally NOT exposed — re-firing a workflow step
// can re-spend LLM budget and submit broker instructions, so that
// privilege stays with platform operators (fund owners route those
// requests through support, who then call the admin endpoint).
export async function listFundWorkflowCheckpoints(opts: {
  fundId: string;
  tradingDate: string;
}): Promise<import("@fundai/api-client").ListWorkflowCheckpointsResponse> {
  const qs = new URLSearchParams({ trading_date: opts.tradingDate }).toString();
  return apiGet<import("@fundai/api-client").ListWorkflowCheckpointsResponse>(
    `/api/funds/${encodeURIComponent(opts.fundId)}/workflow-checkpoints?${qs}`,
  );
}

export type {
  WorkflowCheckpoint,
  WorkflowCheckpointStatus,
  ListWorkflowCheckpointsResponse,
  ResumeWorkflowCheckpointRequest,
  ResumeWorkflowCheckpointResponse,
} from "@fundai/api-client";

// ---------------------------------------------------------------------------
// Model A/B experiments (S10.3 / S10.4)
// ---------------------------------------------------------------------------

export async function listModelABExperiments(opts: {
  status?: string;
} = {}): Promise<import("@fundai/api-client").ListModelABExperimentsResponse> {
  const qs = new URLSearchParams();
  if (opts.status) qs.set("status", opts.status);
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<import("@fundai/api-client").ListModelABExperimentsResponse>(
    `/api/admin/model-ab/experiments${tail}`,
  );
}

export async function getModelABExperiment(
  id: string,
): Promise<import("@fundai/api-client").ModelABExperiment> {
  return apiGet<import("@fundai/api-client").ModelABExperiment>(
    `/api/admin/model-ab/experiments/${encodeURIComponent(id)}`,
  );
}

export async function getModelABReport(
  id: string,
  opts: { from?: string; to?: string } = {},
): Promise<import("@fundai/api-client").ModelABReport> {
  const qs = new URLSearchParams();
  if (opts.from) qs.set("from", opts.from);
  if (opts.to) qs.set("to", opts.to);
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<import("@fundai/api-client").ModelABReport>(
    `/api/admin/model-ab/experiments/${encodeURIComponent(id)}/report${tail}`,
  );
}

export async function createModelABExperiment(
  body: import("@fundai/api-client").CreateModelABExperimentRequest,
): Promise<import("@fundai/api-client").ModelABExperiment> {
  return apiPost<import("@fundai/api-client").ModelABExperiment>(
    `/api/admin/model-ab/experiments`,
    body,
  );
}

export async function setModelABExperimentStatus(
  id: string,
  body: import("@fundai/api-client").SetModelABStatusRequest,
): Promise<import("@fundai/api-client").ModelABExperiment> {
  return apiPatch<import("@fundai/api-client").ModelABExperiment>(
    `/api/admin/model-ab/experiments/${encodeURIComponent(id)}/status`,
    body,
  );
}

export async function updateModelABExperiment(
  id: string,
  body: import("@fundai/api-client").UpdateModelABExperimentRequest,
): Promise<import("@fundai/api-client").ModelABExperiment> {
  return apiPatch<import("@fundai/api-client").ModelABExperiment>(
    `/api/admin/model-ab/experiments/${encodeURIComponent(id)}`,
    body,
  );
}

export async function cloneModelABExperiment(
  id: string,
  body?: import("@fundai/api-client").CloneModelABExperimentRequest,
): Promise<import("@fundai/api-client").ModelABExperiment> {
  return apiPost<import("@fundai/api-client").ModelABExperiment>(
    `/api/admin/model-ab/experiments/${encodeURIComponent(id)}/clone`,
    body ?? {},
  );
}

export async function bulkSetModelABStatus(
  body: import("@fundai/api-client").BulkSetModelABStatusRequest,
): Promise<import("@fundai/api-client").BulkSetModelABStatusResponse> {
  return apiPost<import("@fundai/api-client").BulkSetModelABStatusResponse>(
    `/api/admin/model-ab/experiments/bulk-status`,
    body,
  );
}

export type {
  ModelABArm,
  ModelABExperiment,
  ModelABExperimentStatus,
  ModelABScope,
  ListModelABExperimentsResponse,
  CreateModelABExperimentRequest,
  SetModelABStatusRequest,
  UpdateModelABExperimentRequest,
  CloneModelABExperimentRequest,
  BulkSetModelABStatusRequest,
  BulkSetModelABStatusResponse,
  ModelABReport,
  ModelABArmMetric,
} from "@fundai/api-client";

// ---------------------------------------------------------------------------
// Sprint 11.4 — LLM health admin endpoints.
// ---------------------------------------------------------------------------

export async function fetchLLMHealthSummary(
  windowHours = 24,
): Promise<import("@fundai/api-client").LLMHealthSummary> {
  return apiGet<import("@fundai/api-client").LLMHealthSummary>(
    `/api/admin/llm-health/summary?window_hours=${encodeURIComponent(String(windowHours))}`,
  );
}

export async function fetchLLMHealthRecentFallbacks(
  windowHours = 24,
  limit = 50,
): Promise<import("@fundai/api-client").LLMHealthRecentFallbacksResponse> {
  return apiGet<import("@fundai/api-client").LLMHealthRecentFallbacksResponse>(
    `/api/admin/llm-health/recent-fallbacks?window_hours=${encodeURIComponent(
      String(windowHours),
    )}&limit=${encodeURIComponent(String(limit))}`,
  );
}

// W9-2 — admin JSON view onto the W6-1 memory re-embed queue.
//
// Lives next to the LLM-health helpers because both feed admin
// observability panels rather than the public app surface. The
// shape mirrors `memReembedStatus` in
// server/cmd/server/memreembed_handler.go — keep them in sync.
export interface AdminMemReembedStatus {
  enabled: boolean;
  pending: number;
  embeddedTotal: number;
  retriedTotal: number;
  deadLetterTotal: number;
  // Both are optional because they are omitted server-side when
  // the queue has never recorded an error (omitempty in Go).
  lastErrorUnix?: number;
  lastErrorTime?: string;
  observedAt: string;
}

export async function fetchAdminMemReembedStatus(): Promise<AdminMemReembedStatus> {
  return apiGet<AdminMemReembedStatus>("/api/admin/memreembed/status");
}

// W13-1 — admin JSON view onto the database connection pool.
// Mirror of `dbPoolStatus` in
// server/cmd/server/db_pool_handler.go — keep in sync.
//
// `utilizationPct` and `waitAvgSeconds` come back as -1 when the
// underlying value is undefined (no MaxOpen configured, or no
// waits observed yet). Render those as "—" rather than "-1%" /
// "-1.0 s" — the Go side intentionally negative-encodes "unknown"
// so the JSON shape stays a number for graphing libraries.
export interface AdminDBPoolStatus {
  openConnections: number;
  inUseConnections: number;
  idleConnections: number;
  maxOpenConnections: number;
  maxIdleConnsConfig: number;
  connMaxLifetimeNs: number;
  connMaxLifetime: string;
  utilizationPct: number;
  waitAvgSeconds: number;
  waitCount: number;
  waitDurationNs: number;
  waitDurationHuman: string;
  maxIdleClosedTotal: number;
  maxIdleTimeClosedTotal: number;
  maxLifetimeClosedTotal: number;
  observedAt: string;
}

export async function fetchAdminDBPoolStatus(): Promise<AdminDBPoolStatus> {
  return apiGet<AdminDBPoolStatus>("/api/admin/db-pool/status");
}

// W11-1 — admin JSON view onto the embedquota.Limiter.
// Mirror of `embedQuotaStatus` in
// server/cmd/server/embed_quota_handler.go — keep in sync.
//
// Field names map 1:1 to the Go JSON tags. The histogram-derived
// fields (acquireWaitP99Seconds / callTokensP99) are estimates
// snapped to a bucket boundary; treat as "tail trend" indicators,
// not exact percentiles. For real percentile math the Grafana
// Prometheus dashboard remains canonical.
// W12-3 — paired with /api/admin/embed-quota/status's
// `tokenHistory` field. The Go side guarantees exactly 7
// entries (or omits the field entirely when the limiter is
// disabled) sorted ascending by day, with today last and any
// gaps zero-filled. The UI sparkline reads index n-1 as "today".
export interface EmbedQuotaTokenDay {
  day: string;
  tokens: number;
}

export interface AdminEmbedQuotaStatus {
  enabled: boolean;
  status: string;
  tokensTodayUsed: number;
  tokensDailyMax: number;
  tokensTodayShare: number;
  callsLastMinute: number;
  callsPerMinuteMax: number;
  softLimitFraction: number;
  throttledTotal: number;
  exhaustedTotal: number;
  acquireWaitCount: number;
  acquireWaitSumSeconds: number;
  acquireWaitP99Seconds: number;
  callTokensCount: number;
  callTokensSum: number;
  callTokensP99: number;
  tokenHistory?: EmbedQuotaTokenDay[];
  observedAt: string;
}

export async function fetchAdminEmbedQuotaStatus(): Promise<AdminEmbedQuotaStatus> {
  return apiGet<AdminEmbedQuotaStatus>("/api/admin/embed-quota/status");
}

// W14-3 — wire shape for /api/admin/embed-quota/per-fund.
// Mirrors embedQuotaPerFundResponse / embedQuotaFundEntry on
// the Go side. The bucket arrays are intentionally NOT
// included — Prometheus owns histogram detail; this surface is
// optimised for "which fund is the worst offender right now".
export interface AdminEmbedQuotaFundEntry {
  fundId: string;
  throttledTotal: number;
  exhaustedTotal: number;
  waitCount: number;
  acquireWaitSumSeconds: number;
  acquireWaitP99Seconds: number;
  callTokensCount: number;
  callTokensSum: number;
  callTokensP99: number;
  tokensTodayUsed: number;
  lastSeenAt: string;
}

export interface AdminEmbedQuotaPerFundStatus {
  enabled: boolean;
  funds: AdminEmbedQuotaFundEntry[];
  observedAt: string;
}

export async function fetchAdminEmbedQuotaPerFund(): Promise<AdminEmbedQuotaPerFundStatus> {
  return apiGet<AdminEmbedQuotaPerFundStatus>(
    "/api/admin/embed-quota/per-fund",
  );
}

export type {
  LLMHealthSourceRow,
  LLMHealthCategoryRow,
  LLMHealthSummary,
  LLMHealthRecentFallback,
  LLMHealthRecentFallbacksResponse,
} from "@fundai/api-client";

// ---------------------------------------------------------------------------
// Sprint 12.3 — alert events.
// ---------------------------------------------------------------------------

export async function fetchAdminAlerts(opts: {
  status?: "firing" | "resolved";
  limit?: number;
} = {}): Promise<import("@fundai/api-client").ListAdminAlertsResponse> {
  const qs = new URLSearchParams();
  if (opts.status) qs.set("status", opts.status);
  if (opts.limit) qs.set("limit", String(opts.limit));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<import("@fundai/api-client").ListAdminAlertsResponse>(
    `/api/admin/alerts${tail}`,
  );
}

export async function acknowledgeAdminAlert(
  id: string,
  body: import("@fundai/api-client").AcknowledgeAlertRequest = {},
): Promise<{ ok: boolean }> {
  return apiPatch<{ ok: boolean }>(
    `/api/admin/alerts/${encodeURIComponent(id)}/ack`,
    body,
  );
}

export type {
  AdminAlertEvent,
  AlertSeverity,
  AlertStatus,
  AcknowledgeAlertRequest,
  ListAdminAlertsResponse,
} from "@fundai/api-client";

// ---------------------------------------------------------------------------
// Sprint 13 — model A/B promotion drafts.
// ---------------------------------------------------------------------------

export async function fetchModelABPromotionDrafts(opts: {
  status?: "pending" | "applied" | "rejected" | "superseded";
  limit?: number;
} = {}): Promise<import("@fundai/api-client").ListModelABPromotionDraftsResponse> {
  const qs = new URLSearchParams();
  if (opts.status) qs.set("status", opts.status);
  if (opts.limit) qs.set("limit", String(opts.limit));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<import("@fundai/api-client").ListModelABPromotionDraftsResponse>(
    `/api/admin/model-ab/promotion-drafts${tail}`,
  );
}

export async function fetchModelABPromotionDraft(
  id: string,
): Promise<import("@fundai/api-client").ModelABPromotionDraft> {
  return apiGet<import("@fundai/api-client").ModelABPromotionDraft>(
    `/api/admin/model-ab/promotion-drafts/${encodeURIComponent(id)}`,
  );
}

export async function scanModelABPromotionDrafts(): Promise<import("@fundai/api-client").ScanModelABPromotionsResponse> {
  return apiPost<import("@fundai/api-client").ScanModelABPromotionsResponse>(
    `/api/admin/model-ab/promotion-drafts/scan`,
    {},
  );
}

export async function applyModelABPromotionDraft(
  id: string,
): Promise<import("@fundai/api-client").ApplyModelABPromotionResponse> {
  return apiPatch<import("@fundai/api-client").ApplyModelABPromotionResponse>(
    `/api/admin/model-ab/promotion-drafts/${encodeURIComponent(id)}/apply`,
    {},
  );
}

export async function rejectModelABPromotionDraft(
  id: string,
  body: import("@fundai/api-client").RejectModelABPromotionRequest = {},
): Promise<{ ok: boolean }> {
  return apiPatch<{ ok: boolean }>(
    `/api/admin/model-ab/promotion-drafts/${encodeURIComponent(id)}/reject`,
    body,
  );
}

export type {
  ModelABPromotionDraft,
  ModelABPromotionStatus,
  ListModelABPromotionDraftsResponse,
  ApplyModelABPromotionResponse,
  RejectModelABPromotionRequest,
  ScanModelABPromotionsResponse,
} from "@fundai/api-client";

// ---------------------------------------------------------------------------
// 2FA / TOTP (P0-6)
// ---------------------------------------------------------------------------

// TwoFAStatusResponse mirrors GET /api/auth/2fa/status. Default
// shape (404 → all-false) is normalised inside getTwoFAStatus so
// callers don't have to special-case "no row".
export interface TwoFAStatusResponse {
  enabled: boolean;
  enrolmentPending: boolean;
  lastVerifiedAt?: string;
  lastUsedRecoveryAt?: string;
}

// TwoFASetupResponse is the one-shot payload returned by
// /api/auth/2fa/setup. The frontend is responsible for showing
// secret + recoveryCodes EXACTLY ONCE — they cannot be retrieved
// later.
export interface TwoFASetupResponse {
  secret: string;
  provisioningUri: string;
  recoveryCodes: string[];
  issuer: string;
  accountLabel: string;
  digits: number;
  period: number;
  algorithm: string;
}

export async function getTwoFAStatus(): Promise<TwoFAStatusResponse> {
  return apiGet<TwoFAStatusResponse>("/api/auth/2fa/status");
}

export async function setupTwoFA(): Promise<TwoFASetupResponse> {
  return apiPost<TwoFASetupResponse>("/api/auth/2fa/setup", {});
}

export async function verifyTwoFA(code: string): Promise<{ enabled: boolean }> {
  return apiPost<{ enabled: boolean }>("/api/auth/2fa/verify", { code });
}

export async function disableTwoFA(params: {
  password: string;
  code?: string;
  recoveryCode?: string;
}): Promise<{ disabled: boolean }> {
  const body: Record<string, string> = { password: params.password };
  if (params.code) body.code = params.code;
  if (params.recoveryCode) body.recoveryCode = params.recoveryCode;
  return apiPost<{ disabled: boolean }>("/api/auth/2fa/disable", body);
}

// TwoFAChallengeResponse is what /api/auth/login returns when the
// user has 2FA enabled. The frontend stashes the challenge,
// presents the TOTP prompt, then forwards (challenge, code) to
// /api/auth/2fa/challenge to receive the actual session.
export interface TwoFAChallengeResponse {
  requires_2fa: true;
  challenge: string;
  expires_at: string;
}

// SessionTokenResponse mirrors the legacy /login success body. We
// keep the snake_case keys to match the wire protocol — the rest
// of the platform reads them as-is.
export interface SessionTokenResponse {
  token: string;
  user_id: string;
  email: string;
  display_name: string;
  role: string;
  kyc_status?: string;
  kyc_level?: string;
  expires_at: string;
}

export async function exchangeTwoFAChallenge(params: {
  challenge: string;
  code?: string;
  recoveryCode?: string;
}): Promise<LoginResponse> {
  const body: Record<string, string> = { challenge: params.challenge };
  if (params.code) body.code = params.code;
  if (params.recoveryCode) body.recoveryCode = params.recoveryCode;
  const resp = await apiPost<SessionTokenResponse>("/api/auth/2fa/challenge", body);
  // Reuse the same persistence path as classic login so cookies +
  // local storage end up in the same shape downstream code expects.
  return persistLogin(resp as LoginResponse);
}

export interface BacktestComparisonDiff {
  cumulativeReturnDelta: number;
  annualizedReturnDelta: number;
  volatilityDelta: number;
  sharpeDelta: number;
  maxDrawdownDelta: number;
  winRateDelta: number;
  tradeCountDelta: number;
  finalNavDelta: number;
  sameWindow: boolean;
  sameUniverse: boolean;
}

export interface BacktestComparison {
  a: BacktestJob;
  b: BacktestJob;
  diff: BacktestComparisonDiff;
}

export async function compareBacktests(fundId: string, jobIdA: string, jobIdB: string): Promise<BacktestComparison> {
  const qs = new URLSearchParams({ a: jobIdA, b: jobIdB }).toString();
  return apiGet<BacktestComparison>(`/api/funds/${encodeURIComponent(fundId)}/backtests/compare?${qs}`);
}

export interface SweepAxisInput {
  name: string;
  values: string[];
}

export interface SweepSubmitInput {
  fundId?: string;
  name?: string;
  base: BacktestSubmitInput;
  axes: SweepAxisInput[];
}

export interface BacktestSweepChild {
  job: BacktestJob;
  axisValues: Record<string, string>;
}

export interface BacktestSweep {
  id: string;
  fundId: string;
  name: string;
  status: "queued" | "running" | "completed" | "failed" | string;
  totalCells: number;
  doneCells: number;
  createdAt: string;
  base?: BacktestRequestEcho;
  axes: SweepAxisInput[];
  children?: BacktestSweepChild[];
}

export async function submitSweep(fundId: string, body: SweepSubmitInput): Promise<BacktestSweep> {
  return apiPost<BacktestSweep>(`/api/funds/${encodeURIComponent(fundId)}/backtests/sweeps`, body);
}

export async function listSweeps(fundId: string): Promise<BacktestSweep[]> {
  return apiGet<BacktestSweep[]>(`/api/funds/${encodeURIComponent(fundId)}/backtests/sweeps`);
}

export async function getSweep(fundId: string, sweepId: string): Promise<BacktestSweep> {
  return apiGet<BacktestSweep>(`/api/funds/${encodeURIComponent(fundId)}/backtests/sweeps/${encodeURIComponent(sweepId)}`);
}

export async function getSweepAxisCatalog(): Promise<{ axes: string[] }> {
  return apiGet<{ axes: string[] }>(`/api/backtests/sweeps/axes`);
}

// -----------------------------------------------------------------------------
// Phase 2J/K/L — Strategy Promotion / Shadow Trading / Decay Monitor
// -----------------------------------------------------------------------------

// PromotionStatus enumerates every state in the lifecycle. Keep
// the union open with `string` so the UI never hard-fails on a
// server that adds a new state ahead of the frontend.
export type PromotionStatus =
  | "pending_review"
  | "approved"
  | "shadow"
  | "active"
  | "superseded"
  | "rejected"
  | "rolled_back"
  | "decayed"
  | string;

export interface PromotionBaseline {
  cumulativeReturn: number;
  annualizedReturn: number;
  sharpeRatio: number;
  volatility: number;
  maxDrawdown: number;
  winRate: number;
  tradeCount: number;
  oosReturn?: number;
  oosSharpe?: number;
}

export interface Promotion {
  id: string;
  fundId: string;
  proposedBy: string;
  basisJobId: string;
  engineKind: string;
  engineParams: Record<string, unknown>;
  baselineMetrics: PromotionBaseline;
  status: PromotionStatus;
  shadowDays: number;
  decayRatio: number;
  approvedBy?: string;
  approvedAt?: string;
  rejectedBy?: string;
  rejectedAt?: string;
  rejectedReason?: string;
  shadowStartedAt?: string;
  shadowCompletedAt?: string;
  activatedAt?: string;
  deactivatedAt?: string;
  deactivatedReason?: string;
  notes?: string;
  createdAt: string;
  updatedAt: string;
}

export interface PromotionEvent {
  id: string;
  eventType: string;
  actorUserId?: string;
  payload?: Record<string, unknown>;
  createdAt: string;
}

export interface PromotionShadowDiff {
  id: string;
  tradingDate: string;
  shadowDecision: Record<string, unknown>;
  activeDecision: Record<string, unknown>;
  agreement: boolean;
  createdAt: string;
}

export interface PromotionHealth {
  id: string;
  snapshotAt: string;
  windowDays: number;
  actualSharpe?: number;
  actualReturn?: number;
  actualMaxDrawdown?: number;
  actualTradeCount: number;
  sharpeDecayRatio?: number;
  decayFlag: boolean;
  notes?: string;
}

export interface PromotionDetail {
  promotion: Promotion;
  events: PromotionEvent[];
  shadowDiffs: PromotionShadowDiff[];
  health: PromotionHealth[];
  agreementRatio: number;
  agreementSamples: number;
}

export interface ProposePromotionInput {
  basisJobId: string;
  engineParams?: Record<string, unknown>;
  shadowDays?: number;
  decayRatio?: number;
  notes?: string;
}

export async function listPromotions(fundId: string): Promise<Promotion[]> {
  return apiGet<Promotion[]>(`/api/funds/${encodeURIComponent(fundId)}/promotions`);
}

export async function getPromotion(fundId: string, promotionId: string): Promise<PromotionDetail> {
  return apiGet<PromotionDetail>(
    `/api/funds/${encodeURIComponent(fundId)}/promotions/${encodeURIComponent(promotionId)}`,
  );
}

export async function proposePromotion(fundId: string, body: ProposePromotionInput): Promise<Promotion> {
  return apiPost<Promotion>(`/api/funds/${encodeURIComponent(fundId)}/promotions`, body);
}

export async function approvePromotion(fundId: string, promotionId: string): Promise<Promotion> {
  return apiPost<Promotion>(
    `/api/funds/${encodeURIComponent(fundId)}/promotions/${encodeURIComponent(promotionId)}/approve`,
  );
}

export async function rejectPromotion(
  fundId: string,
  promotionId: string,
  reason: string,
): Promise<Promotion> {
  return apiPost<Promotion>(
    `/api/funds/${encodeURIComponent(fundId)}/promotions/${encodeURIComponent(promotionId)}/reject`,
    { reason },
  );
}

export async function activatePromotion(fundId: string, promotionId: string): Promise<Promotion> {
  return apiPost<Promotion>(
    `/api/funds/${encodeURIComponent(fundId)}/promotions/${encodeURIComponent(promotionId)}/activate`,
  );
}

export async function rollbackPromotion(
  fundId: string,
  promotionId: string,
  reason: string,
): Promise<Promotion> {
  return apiPost<Promotion>(
    `/api/funds/${encodeURIComponent(fundId)}/promotions/${encodeURIComponent(promotionId)}/rollback`,
    { reason },
  );
}

export interface DecisionTraceStep {
  step?: string;
  status?: string;
  startedAt?: string;
  endedAt?: string;
  updatedAt?: string;
  error?: string;
}

export interface DecisionTraceRun {
  state?: string;
  step?: string;
  startedAt?: string;
  completedAt?: string;
  steps?: DecisionTraceStep[];
  runId?: string;
}

export interface PlanSummary {
  id: string;
  fundId: string;
  tradingDate?: string;
  status: string;
  reasoning?: string;
  reasoningZh?: string;
  reasoningEn?: string;
  riskScore?: number;
  expectedReturn?: number;
  riskReview?: unknown;
  roundtableId?: string;
  pmAgentId?: string;
  actions?: Array<{
    id?: string;
    instrumentKey?: string;
    action: string;
    symbol: string;
    market?: string;
    exchange?: string;
    assetClass?: string;
    instrumentType?: string;
    positionSide?: string;
    openClose?: string;
    quantity?: number;
    price?: number;
    amount?: number;
    stopLoss?: number;
    takeProfit?: number;
    reasoning?: string;
    reasoningZh?: string;
    reasoningEn?: string;
    confidence?: number;
    supportedBy?: string[];
    opposedBy?: string[];
    executionStatus?: string;
    sortOrder?: number;
    quoteCurrency?: string;
    settlementCurrency?: string;
    marginMode?: string;
    leverage?: number;
    contractMultiplier?: number;
    expiryDate?: string;
    reduceOnly?: boolean;
    // RFC 3339 timestamp of the most recent successful refresh-quote
    // call applied to this action. Absent when the action still holds
    // its plan-generation quote. UI surfaces it as a "refreshed N min
    // ago" badge so users know whether the displayed prices are live.
    quoteRefreshedAt?: string;
  }>;
  createdAt: string;
  updatedAt: string;
  // Sprint 11.3 — provenance chip data. decisionSource is one of
  // llm_pm / llm_three_stage / fallback_no_llm /
  // fallback_after_llm_error / fallback_empty_plan / legacy.
  // fallbackReason is populated only on fallback_* rows and carries
  // the redacted category + provider — the technical summary is
  // never sent to non-admin users.
  decisionSource?: DecisionSource;
  fallbackReason?: PlanFallbackReason;
}

export type DecisionSource =
  | "llm_pm"
  | "llm_three_stage"
  | "fallback_no_llm"
  | "fallback_after_llm_error"
  | "fallback_empty_plan"
  | "legacy";

export type DecisionFallbackCategory =
  | "rate_limited"
  | "service_unavailable"
  | "auth_failed"
  | "context_length_exceeded"
  | "invalid_request"
  | "schema_validation_failed"
  | "network_timeout"
  | "budget_exceeded"
  | "empty_response"
  | "cancelled"
  | "unknown";

export interface PlanFallbackReason {
  category: DecisionFallbackCategory;
  provider?: string;
  at?: string;
}

export interface DecisionTraceDiscussion {
  reasoning?: string;
  reasoningZh?: string;
  reasoningEn?: string;
  summary?: string;
  summaryZh?: string;
  summaryEn?: string;
  consensus?: string[];
  consensusZh?: string[];
  consensusEn?: string[];
  snapshot?: unknown;
  hasSnapshot?: boolean;
}

export interface DecisionTraceTrade {
  id: string;
  fundId: string;
  planId?: string;
  planActionId?: string;
  instrumentKey?: string;
  symbol: string;
  market?: string;
  exchange?: string;
  assetClass?: string;
  instrumentType?: string;
  side: string;
  positionSide?: string;
  openClose?: string;
  orderType: string;
  quantity: number;
  price?: number;
  amount?: number;
  filledQty: number;
  filledPrice?: number;
  feeCommission: number;
  feeStampTax: number;
  feeTransfer: number;
  tradingMode: string;
  brokerOrderId?: string;
  mcpServerId?: string;
  status: string;
  executedAt?: string;
  quoteCurrency?: string;
  settlementCurrency?: string;
  marginMode?: string;
  leverage?: number | null;
  contractMultiplier?: number | null;
  expiryDate?: string;
  reduceOnly?: boolean | null;
  // Realised slippage as a signed fraction: (filledPrice - price)/price.
  // Absent for sells and for trades that predate the SlippageGuard
  // rollout. UI surfaces this on the trade-detail view as a "drift +0.42%"
  // chip so users can spot trades that filled materially off the plan.
  slippagePct?: number | null;
  // Execution strategy the PM direct-fill path chose for this row.
  // One of "immediate" / "limit" / "twap" / "vwap" / "iceberg" / "pov".
  // Empty (undefined) on legacy rows (pre-088 migration). All children
  // of a TWAP parent share the parent's value.
  strategy?: string;
  // Set when this row is one slice of a multi-child execution (TWAP /
  // VWAP / iceberg / POV); points back at the parent trade ID. Empty
  // means this row IS the parent (or the trade pre-dates child
  // splitting). UI list views should hide rows where this is set so
  // the trade list shows one aggregated parent per plan_action;
  // detail views can drill into children via a follow-up endpoint.
  // Distinct from any OCO / bracket parent linkage.
  strategyParentTradeId?: string;
  createdAt?: string;
}

export interface DecisionTraceActionExecution {
  planActionId?: string;
  symbol?: string;
  action?: string;
  executionStatus?: string;
  trades?: DecisionTraceTrade[];
}

export interface DecisionTraceExecution {
  status?: string;
  actionExecutions?: DecisionTraceActionExecution[];
  trades?: DecisionTraceTrade[];
}

export interface DecisionTraceReviewEntry {
  id: string;
  agentId?: string;
  title?: string;
  content: string;
  layer: string;
  tradingDate?: string;
  tags?: string[];
  createdAt: string;
  updatedAt: string;
  // i18n contract (server migration 085). See lessonRenderer.ts.
  templateKey?: string;
  payload?: Record<string, unknown>;
}

export interface DecisionTraceReview {
  entries?: DecisionTraceReviewEntry[];
}

export interface CommitteeParticipant {
  agentId?: string;
  role?: string;
  name?: string;
}

export interface CommitteeAgentView {
  agentId?: string;
  role?: string;
  stance?: string;
  symbols?: string[];
  viewpoint?: string;
  evidence?: string[];
}

export interface CommitteeFinalDecision {
  status?: string;
  pm?: string;
  reasoning?: string;
  actions?: string[];
}

export interface CommitteeRiskOpinion {
  verdict?: string;
  summary?: string;
  warnings?: string[];
  rejections?: string[];
  suggestions?: string[];
}

export interface CommitteeTraderAction {
  planActionId?: string;
  symbol?: string;
  action?: string;
  instruction?: string;
  supportedBy?: string[];
  opposedBy?: string[];
}

export interface CommitteeTraceLink {
  label?: string;
  target?: string;
}

export interface CommitteeMemo {
  title?: string;
  summary?: string;
  marketBackground?: string;
  participants?: CommitteeParticipant[];
  agentViews?: CommitteeAgentView[];
  consensus?: string[];
  contentions?: string[];
  finalDecision?: CommitteeFinalDecision;
  riskOpinion?: CommitteeRiskOpinion;
  traderSuggestions?: CommitteeTraderAction[];
  traceLinks?: CommitteeTraceLink[];
}

export interface RiskCheckExplanation {
  ruleCode?: string;
  ruleName?: string;
  status?: string;
  severity?: string;
  current?: number | null;
  threshold?: number | null;
  explanation?: string;
  userImpact?: string;
  adjustmentHint?: string;
}

export interface RiskExplanation {
  verdict?: string;
  severity?: string;
  summary?: string;
  blockingReasons?: string[];
  warnings?: string[];
  suggestions?: string[];
  adjustmentAdvice?: string[];
  checks?: RiskCheckExplanation[];
}

export interface AgentLearningScope {
  fundIds?: string[];
  markets?: string[];
  assetClasses?: string[];
  themes?: string[];
  instruments?: string[];
  styleHints?: string[];
  memoryScope?: string;
}

export interface AgentLearningRecord {
  id: string;
  fundId?: string;
  tradingDate?: string;
  title?: string;
  summary?: string;
  hits?: string[];
  misses?: string[];
  lessons?: string[];
  adjustments?: string[];
  tags?: string[];
  dailyReturn?: number;
  createdAt: string;
  revoked?: boolean;
  revokedReason?: string;
  revokedAt?: string;
  // i18n contract (server migration 085): present iff this record was
  // emitted by the structured lesson pipeline. Renderers should prefer
  // these over `title` / `summary` and fall back to them when missing.
  // `payload` is the raw value Object the template interpolates against —
  // see lessonMessages in @fundai/api-client for the per-template schema.
  templateKey?: string;
  payload?: Record<string, unknown>;
}

export interface AgentLearningStatus {
  agentId: string;
  agentName?: string;
  role?: string;
  focus?: string;
  enabled: boolean;
  autoApplyAdjustments: boolean;
  maxLessonsPerDay: number;
  scope?: AgentLearningScope;
  recentLessons?: string[];
  lastLearningSummary?: string;
  lastLearningDate?: string;
  lastLearningTags?: string[];
  lastAdjustments?: string[];
  lastDailyReturn?: number;
  learningUpdatedAt?: string;
  revokedAt?: string;
  revokedReason?: string;
  records?: AgentLearningRecord[];
}

export interface AgentLearningConfigInput {
  autoApplyAdjustments?: boolean;
  maxLessonsPerDay?: number;
  scope?: AgentLearningScope;
}

export interface AgentLineageNode {
  agentId: string;
  agentName?: string;
  role?: string;
  focus?: string;
  ownerUserId?: string;
  derivedVia?: string;
  sourceListingId?: string;
  createdAt?: string;
  ancestors?: AgentLineageNode[];
}

export interface AgentLineageTree {
  agentId: string;
  root: AgentLineageNode;
  ancestorCount: number;
  maxDepth: number;
  matryoshkaRisk: boolean;
  riskExplanation?: string;
}

export interface LLMUsageBreakdown {
  key: string;
  label?: string;
  agentId?: string;
  totalCalls: number;
  inputTokens: number;
  outputTokens: number;
  totalTokens: number;
  costCents: number;
  priceCents: number;
  customKeyCalls: number;
}

export interface LLMUsageCall {
  id: string;
  agentId?: string;
  agentName?: string;
  stepName: string;
  modelProvider: string;
  modelName: string;
  inputTokens: number;
  outputTokens: number;
  totalTokens: number;
  costCents: number;
  priceCents: number;
  isCustomKey: boolean;
  createdAt: string;
}

export interface LLMUsageVisibility {
  fundId: string;
  from: string;
  to: string;
  totalCalls: number;
  inputTokens: number;
  outputTokens: number;
  totalTokens: number;
  costCents: number;
  priceCents: number;
  customKeyCalls: number;
  byAgent: LLMUsageBreakdown[];
  byStep: LLMUsageBreakdown[];
  byModel: LLMUsageBreakdown[];
  recentCalls: LLMUsageCall[];
}

export interface AuditLogEntry {
  id: string;
  actorUserId?: string;
  action: string;
  resourceType: string;
  resourceId: string;
  details: Record<string, unknown>;
  createdAt: string;
}

export interface AuditLogResponse {
  entries: AuditLogEntry[];
  limit: number;
}

export interface AdminMarketDataProviderHealth {
  totalCalls: number;
  totalSuccesses: number;
  totalFailures: number;
  consecutiveFailures: number;
  circuitOpenUntil?: string;
  lastError?: string;
  lastSuccessAt?: string;
  lastFailureAt?: string;
  lastLatencyMs?: number;
  emaLatencyMs?: number;
}

export interface AdminMarketDataHealthResponse {
  providers: Record<string, AdminMarketDataProviderHealth>;
}

// F7 — workflow scheduler observability + manual trigger.
//
// FundSchedulerSnapshot mirrors the Go struct exposed by
// GET /api/admin/workflow/scheduler. Used by the Admin dashboard to
// render the per-fund daily-workflow schedule and surface any errors.
export interface FundSchedulerStatus {
  fundId: string;
  fundName?: string;
  calendarCode?: string;
  timeZone?: string;
  nextTradingDay?: string;
  nextTriggerAt?: string;
  due: boolean;
  started: boolean;
  lastStatus?: string;
  skipReason?: string;
  error?: string;
}

export interface FundSchedulerSnapshot {
  lastPollAt?: string;
  isLeader: boolean;
  nextPollAt?: string;
  totalActive: number;
  triggeredCount: number;
  funds: FundSchedulerStatus[];
  warnings?: string[];
  lastError?: string;
}

export interface AdminTriggerResult {
  fundId: string;
  tradingDate: string;
  state: string;
  step?: string;
}

export async function fetchWorkflowSchedulerSnapshot(): Promise<FundSchedulerSnapshot> {
  return await apiGet<FundSchedulerSnapshot>("/api/admin/workflow/scheduler");
}

export async function triggerFundWorkflow(fundId: string, tradingDate?: string): Promise<AdminTriggerResult> {
  return await apiPost<AdminTriggerResult>(`/api/admin/workflow/scheduler/trigger/${encodeURIComponent(fundId)}`, tradingDate ? { tradingDate } : {});
}

export interface AdminKYCApplication {
  id: string;
  user_id: string;
  user_email?: string;
  user_display_name?: string;
  kyc_level: string;
  status: string;
  full_name: string;
  id_document_type: string;
  id_document_number: string;
  document_image_urls?: string[];
  rejection_reason?: string;
  created_at: string;
  updated_at: string;
}

export interface AdminKYCDecisionResponse {
  status: string;
  application_id: string;
  new_status: string;
}

export interface AccountKYCApplication {
  id: string;
  user_id: string;
  kyc_level: string;
  status: string;
  full_name: string;
  id_document_type: string;
  id_document_number: string;
  document_image_urls?: string[];
  rejection_reason?: string;
  created_at: string;
  updated_at: string;
}

export interface AccountKYCStatus {
  kyc_status: string;
  kyc_level: string;
  applications: AccountKYCApplication[];
}

export interface SubmitKYCInput {
  kyc_level: string;
  full_name: string;
  id_document_type: string;
  id_document_number: string;
  document_image_urls?: string[];
}

export interface SubmitKYCResponse {
  application: AccountKYCApplication;
}

export interface PnLAttributionBucket {
  key: string;
  label?: string;
  realizedPnl: number;
  unrealizedPnl: number;
  feeDrag: number;
  totalPnl: number;
  tradeCount: number;
  exposure: number;
  weight: number;
}

export interface PnLAttributionDailyPoint {
  date: string;
  dailyReturn: number;
  totalAssets: number;
  dailyPnl: number;
}

export interface PnLAttribution {
  fundId: string;
  from?: string;
  to?: string;
  beginningAssets: number;
  endingAssets: number;
  totalPnl: number;
  realizedPnl: number;
  unrealizedPnl: number;
  feeDrag: number;
  returnPct: number;
  bySymbol: PnLAttributionBucket[];
  byAssetClass: PnLAttributionBucket[];
  daily: PnLAttributionDailyPoint[];
}

export interface DecisionTrace {
  fundId: string;
  tradingDate?: string;
  plan?: PlanSummary;
  run?: DecisionTraceRun;
  memo?: CommitteeMemo;
  risk?: RiskExplanation;
  discussion?: DecisionTraceDiscussion;
  execution?: DecisionTraceExecution;
  review?: DecisionTraceReview;
  research?: MarketResearch[];
}

function buildSymbolQuery(symbols: string[]): string {
  return symbols.map((symbol) => symbol.trim()).filter(Boolean).join(",");
}

export async function fetchFundMarketQuotes(fundId: string, symbols: string[]): Promise<FundMarketQuotes> {
  const normalized = buildSymbolQuery(symbols);
  if (!normalized) {
    return { fundId, quotes: [] };
  }
  return apiGet<FundMarketQuotes>(`/api/funds/${fundId}/market/quotes?symbols=${encodeURIComponent(normalized)}`);
}

export function fetchFundMarketResearch(fundId: string, symbol: string, limit = 5): Promise<MarketResearch> {
  return apiGet<MarketResearch>(`/api/funds/${fundId}/market/research?symbol=${encodeURIComponent(symbol.trim())}&limit=${limit}`);
}

export async function fetchFundMarketNewsDigest(fundId: string, symbols: string[], limit = 10): Promise<MarketNewsDigest> {
  const normalized = buildSymbolQuery(symbols);
  if (!normalized) {
    return { fundId, items: [], generatedAt: new Date().toISOString() };
  }
  return apiGet<MarketNewsDigest>(`/api/funds/${fundId}/market/news/digest?symbols=${encodeURIComponent(normalized)}&limit=${limit}`);
}

export function fetchFundDecisionTrace(fundId: string, tradingDate?: string, planId?: string): Promise<DecisionTrace> {
  const params = new URLSearchParams();
  if (tradingDate?.trim()) {
    params.set("date", tradingDate.trim());
  }
  if (planId?.trim()) {
    params.set("planId", planId.trim());
  }
  const query = params.toString();
  return apiGet<DecisionTrace>(`/api/funds/${fundId}/decision-trace${query ? `?${query}` : ""}`);
}

export function fetchAgentLearning(agentId: string): Promise<AgentLearningStatus> {
  return apiGet<AgentLearningStatus>(`/api/agents/${agentId}/learning`);
}

export function enableAgentLearning(agentId: string, input: AgentLearningConfigInput = {}): Promise<AgentLearningStatus> {
  return apiPut<AgentLearningStatus>(`/api/agents/${agentId}/learning/enable`, input);
}

export function disableAgentLearning(agentId: string): Promise<AgentLearningStatus> {
  return apiPut<AgentLearningStatus>(`/api/agents/${agentId}/learning/disable`);
}

export function updateAgentLearningScope(agentId: string, scope: AgentLearningScope): Promise<AgentLearningStatus> {
  return apiPut<AgentLearningStatus>(`/api/agents/${agentId}/learning/scope`, scope);
}

export function revokeAgentLearning(agentId: string, reason?: string): Promise<AgentLearningStatus> {
  return apiPost<AgentLearningStatus>(`/api/agents/${agentId}/learning/revoke`, reason ? { reason } : {});
}

export function fetchAgentLineage(agentId: string): Promise<AgentLineageTree> {
  return apiGet<AgentLineageTree>(`/api/agents/${agentId}/lineage`);
}

export function fetchFundLLMUsage(fundId: string, from?: string, to?: string): Promise<LLMUsageVisibility> {
  const params = new URLSearchParams();
  if (from?.trim()) {
    params.set("from", from.trim());
  }
  if (to?.trim()) {
    params.set("to", to.trim());
  }
  const query = params.toString();
  return apiGet<LLMUsageVisibility>(`/api/funds/${fundId}/llm-usage${query ? `?${query}` : ""}`);
}

export function fetchFundAuditLogs(fundId: string, limit = 50): Promise<AuditLogResponse> {
  return apiGet<AuditLogResponse>(`/api/funds/${fundId}/audit?limit=${limit}`);
}

export function exportFundAuditLogsCSV(fundId: string, limit = 200): Promise<string> {
  return apiGet<string>(`/api/funds/${fundId}/audit?limit=${limit}&format=csv`);
}

export function fetchAdminKYCApplications(status = "pending", limit = 50, offset = 0): Promise<AdminKYCApplication[]> {
  const params = new URLSearchParams();
  params.set("status", status);
  params.set("limit", String(limit));
  params.set("offset", String(offset));
  return apiGet<AdminKYCApplication[]>(`/api/admin/kyc-applications?${params.toString()}`);
}

export function decideAdminKYCApplication(
  applicationId: string,
  action: "approve" | "reject",
  rejectionReason?: string,
): Promise<AdminKYCDecisionResponse> {
  return apiPost<AdminKYCDecisionResponse>(`/api/admin/kyc-applications/${encodeURIComponent(applicationId)}/decision`, {
    action,
    rejection_reason: rejectionReason?.trim() || undefined,
  });
}

export function fetchAccountKYCStatus(): Promise<AccountKYCStatus> {
  return apiGet<AccountKYCStatus>("/api/account/kyc");
}

export function submitAccountKYC(input: SubmitKYCInput): Promise<SubmitKYCResponse> {
  return apiPost<SubmitKYCResponse>("/api/account/kyc", input);
}

// FundTodayPnL mirrors api.TodayPnL on the server. The dashboard
// "今日盈亏" tile reads this instead of computing (live - latest
// NAV) on the client, because today's intra-day NAV snapshot can
// be rewritten by a settle/PM-plan run mid-day, and a missing
// yesterday-snapshot silently turns the "today" delta into a
// multi-day delta. BaselineFresh + PriorCloseDate let the UI
// surface that ambiguity rather than show a wrong number.
export interface FundTodayPnL {
  fundId: string;
  realisedPnl: number;
  currentUnrealisedPnl: number;
  priorCloseUnrealisedPnl: number;
  priorCloseDate: string;
  baselineFresh: boolean;
  todayPnl: number;
  asOf: string;
}

export function fetchFundTodayPnL(fundId: string): Promise<FundTodayPnL> {
  return apiGet<FundTodayPnL>(`/api/funds/${fundId}/today-pnl`);
}

// ---------------------------------------------------------------------------
// Corporate actions (split / cash dividend / stock dividend / combined)
// ---------------------------------------------------------------------------
//
// The wire shape lives in `@fundai/api-client` so Android can reuse it
// verbatim. We re-export the types here so consumers don't have to
// import from two places.

export type CorpActionApplication = SharedCorpActionApplication;
export type CorpActionListResponse = SharedCorpActionListResponse;

/** Fetch the corp-action timeline for a fund. The server caps limit
 *  at 200; non-numeric / out-of-range values fall back to the server
 *  default (50). */
export function fetchFundCorpActions(
  fundId: string,
  limit?: number,
): Promise<CorpActionListResponse> {
  const qs = limit && limit > 0 ? `?limit=${limit}` : "";
  return apiGet<CorpActionListResponse>(`/api/funds/${fundId}/corp-actions${qs}`);
}

// ---------------------------------------------------------------------------
// Benchmark history — fund vs market overlay (Card B).
// ---------------------------------------------------------------------------

/** Re-export shared benchmark types so consumers don't have to dual-import. */
export type BenchmarkPoint = SharedBenchmarkPoint;
export type BenchmarkSeries = SharedBenchmarkSeries;
export type BenchmarkCatalogItem = SharedBenchmarkCatalogItem;
export type BenchmarkPartialFailure = SharedBenchmarkPartialFailure;
export type BenchmarkHistoryResponse = SharedBenchmarkHistoryResponse;
export type BenchmarkHoldingOverlap = SharedBenchmarkHoldingOverlap;

/** Fetch fund vs benchmark history.
 *
 * Empty / undefined `seriesIds` lets the server choose defaults from
 * the fund's universe. The server soft-clamps `days` to [7, 1825]
 * and ignores unknown ids (surfaces them in `partialFailures`), so
 * callers don't need to guard the input shape. */
export function fetchFundBenchmarkHistory(
  fundId: string,
  days?: number,
  seriesIds?: string[],
): Promise<BenchmarkHistoryResponse> {
  const params = new URLSearchParams();
  if (typeof days === "number" && Number.isFinite(days)) {
    params.set("days", String(Math.trunc(days)));
  }
  if (Array.isArray(seriesIds) && seriesIds.length > 0) {
    params.set("series", seriesIds.join(","));
  }
  const qs = params.toString();
  return apiGet<BenchmarkHistoryResponse>(
    `/api/funds/${fundId}/benchmark-history${qs ? `?${qs}` : ""}`,
  );
}

// ---------------------------------------------------------------------------
// Holdings series (P1-2).
// ---------------------------------------------------------------------------

export type HoldingSeries = SharedHoldingSeries;
export type HoldingsSeriesResponse = SharedHoldingsSeriesResponse;

/** Fetch the per-holding normalized-price grid. Same soft-clamp /
 *  partial-failure semantics as fetchFundBenchmarkHistory. */
export function fetchFundHoldingsSeries(
  fundId: string,
  days?: number,
): Promise<HoldingsSeriesResponse> {
  const qs = typeof days === "number" && Number.isFinite(days)
    ? `?days=${Math.trunc(days)}`
    : "";
  return apiGet<HoldingsSeriesResponse>(
    `/api/funds/${fundId}/holdings/series${qs}`,
  );
}

// ---------------------------------------------------------------------------
// A/B test shadow comparison (Card D).
// ---------------------------------------------------------------------------
//
// The wire shapes live in `@fundai/api-client` so Android can reuse them
// verbatim once the AB compare screen lands on mobile. We re-export
// here so consumers don't need to dual-import.

export type ABTestShadowAgent = SharedABTestShadowAgent;
export type ABTestShadowAgentVariant = SharedABTestShadowAgentVariant;
export type ABTestShadowAgentResponse = SharedABTestShadowAgentResponse;
export type ABTestShadowAgentDay = SharedABTestShadowAgentDay;
export type ABTestShadowMemory = SharedABTestShadowMemory;
export type ABEvolutionConfigDiff = SharedABEvolutionConfigDiff;
export type ABAttributionTotals = SharedABAttributionTotals;
export type ABAttributionSymbolRow = SharedABAttributionSymbolRow;
export type ABTestOperationalAttribution = SharedABTestOperationalAttribution;

/** Fetch per-variant shadow-agent learning timeline for an AB test.
 *  Surfaces what the alternative strategy's agents thought during
 *  the shadow run so the comparison page can render A vs B
 *  lessons / adjustments / proposed evolution-config diffs. */
export function fetchABShadowAgents(
  testId: string,
): Promise<ABTestShadowAgentResponse> {
  return apiGet<ABTestShadowAgentResponse>(
    `/api/abtests/${testId}/shadow-agents`,
  );
}

/** Fetch per-symbol A vs B operational attribution table (PnL,
 *  turnover, gap). Bounded server-side to top 50 rows by |gap|. */
export function fetchABOperationalAttribution(
  testId: string,
): Promise<ABTestOperationalAttribution> {
  return apiGet<ABTestOperationalAttribution>(
    `/api/abtests/${testId}/operational-attribution`,
  );
}

export function fetchFundPnLAttribution(fundId: string, from?: string, to?: string): Promise<PnLAttribution> {
  const params = new URLSearchParams();
  if (from?.trim()) {
    params.set("from", from.trim());
  }
  if (to?.trim()) {
    params.set("to", to.trim());
  }
  const query = params.toString();
  return apiGet<PnLAttribution>(`/api/funds/${fundId}/pnl-attribution${query ? `?${query}` : ""}`);
}

// FundNAVPoint mirrors api.NAVPoint on the server (see fund_handler.go).
// One row per trading day with the unit NAV, total assets, daily &
// cumulative return, and the cash/market-value split. The
// performance page uses this for the equity curve + a rough
// daily-return distribution.
export interface FundNAVPoint {
  date: string;
  nav: number;
  totalAssets: number;
  dailyReturn: number;
  totalReturn: number;
  availableCash: number;
  totalMarketValue: number;
}

// fetchFundNavHistory pulls the NAV time-series. Optional bounds
// MUST be RFC 3339 timestamps (e.g. 2026-04-22T00:00:00Z); the
// server uses strict parseOptionalTime and 400s on plain
// YYYY-MM-DD. An empty window returns the full history.
export function fetchFundNavHistory(fundId: string, from?: string, to?: string): Promise<FundNAVPoint[]> {
  const params = new URLSearchParams();
  if (from?.trim()) {
    params.set("from", from.trim());
  }
  if (to?.trim()) {
    params.set("to", to.trim());
  }
  const query = params.toString();
  return apiGet<FundNAVPoint[]>(`/api/funds/${fundId}/nav${query ? `?${query}` : ""}`);
}

// Team Live Activity — REST backfill + SSE stream wiring for F2.4.
export interface TeamActivityItem {
  seq: number;
  type: string;
  role: string;
  step?: string;
  fundId: string;
  runId?: string;
  tradingDate?: string;
  timestamp: string;
  message: string;
  error?: string;
}

export interface TeamActivityResponse {
  fundId: string;
  items: TeamActivityItem[];
}

export interface TeamActivityFetchOptions {
  limit?: number;
  sinceSeq?: number;
  // `before` requests the page of events strictly older than this ISO
  // timestamp, newest-first. Drives the "load earlier" infinite-scroll
  // path so the panel can browse historical events that have fallen
  // out of the in-memory ring. Server-side validation is RFC3339.
  // Mutually exclusive with sinceSeq — when both are set the server
  // ignores sinceSeq.
  before?: string;
}

// fetchTeamActivity reads workflow activity for a fund.
//   * No options: initial page-load backfill (newest N).
//   * sinceSeq=K: catch up after an SSE disconnect (events with seq > K).
//   * before=ISO: load the next historical page (events older than ISO).
export function fetchTeamActivity(fundId: string, opts: TeamActivityFetchOptions = {}): Promise<TeamActivityResponse> {
  const params = new URLSearchParams();
  if (typeof opts.limit === "number" && Number.isFinite(opts.limit) && opts.limit > 0) {
    params.set("limit", String(Math.floor(opts.limit)));
  }
  if (typeof opts.before === "string" && opts.before.length > 0) {
    params.set("before", opts.before);
  } else if (typeof opts.sinceSeq === "number" && Number.isFinite(opts.sinceSeq) && opts.sinceSeq > 0) {
    params.set("sinceSeq", String(Math.floor(opts.sinceSeq)));
  }
  const query = params.toString();
  return apiGet<TeamActivityResponse>(`/api/funds/${fundId}/team/activity${query ? `?${query}` : ""}`);
}

// buildTeamActivityStreamUrl returns the absolute URL for the SSE endpoint.
// EventSource auth relies on the `fundai_session` cookie because the SSE spec
// does not allow custom headers; the cookie is set on login and replayed by
// the browser when withCredentials=true.
export function buildTeamActivityStreamUrl(fundId: string): string {
  return buildUrl(`/api/funds/${fundId}/team/activity/stream`);
}

// buildPortfolioQuotesStreamUrl mirrors buildTeamActivityStreamUrl for the
// PR-4 quote stream. Same cookie-based auth applies; the optional
// `intervalMs` overrides the backend's default 2s cadence (the server
// clamps to [500ms, 30s] so a hostile client can't ask for 100 frames/sec).
export function buildPortfolioQuotesStreamUrl(fundId: string, intervalMs?: number): string {
  let url = `/api/funds/${fundId}/quotes/stream`;
  if (typeof intervalMs === "number" && intervalMs > 0) {
    url += `?interval=${intervalMs}ms`;
  }
  return buildUrl(url);
}

// buildWorkflowStreamUrl is the U4 step-1 SSE companion to
// /api/funds/{fundId}/workflow/status. The server pushes a frame when
// the fingerprinted fields (state / step / progress / counts / completedAt)
// change, plus 15s heartbeats; cookie auth, same clamp range as
// buildPortfolioQuotesStreamUrl. Closes itself when the workflow
// reaches a terminal state so the client doesn't hold an idle socket
// open after the daily run finishes.
export function buildWorkflowStreamUrl(fundId: string, intervalMs?: number): string {
  let url = `/api/funds/${fundId}/workflow/stream`;
  if (typeof intervalMs === "number" && intervalMs > 0) {
    url += `?interval=${intervalMs}ms`;
  }
  return buildUrl(url);
}

// W4-25 multiplex companion. Subscribes to many funds over a
// single EventSource; the server pushes envelope frames tagged
// with `fundId` and (when in a terminal state) `terminal: true`.
// Browsers cap concurrent EventSource handles at ~6 per origin,
// so dashboards tracking 10+ funds otherwise queue half their
// streams; this URL is what `useWorkflowStreamMulti` opens.
export function buildWorkflowStreamMultiUrl(fundIds: string[], intervalMs?: number): string {
  const ids = fundIds
    .map((id) => id.trim())
    .filter((id) => id.length > 0);
  let url = `/api/funds/workflow/stream?fundIds=${encodeURIComponent(ids.join(","))}`;
  if (typeof intervalMs === "number" && intervalMs > 0) {
    url += `&interval=${intervalMs}ms`;
  }
  return buildUrl(url);
}

// WorkflowStatusFrame is the JSON shape pushed on the `workflow` SSE
// event. Mirrors api.WorkflowStatus on the backend with optional
// fields kept optional so the typing handles the reduced payload that
// /workflow/stream emits when nothing has changed since the last
// frame.
export interface WorkflowStatusFrame {
  fundId: string;
  tradingDate?: string;
  state: string;
  step?: string;
  startedAt?: string;
  completedAt?: string;
  runningForMs?: number;
  progressPercent?: number;
  completedSteps?: number;
  failedSteps?: number;
  totalSteps?: number;
}

// PortfolioQuote mirrors the api.PortfolioQuote payload pushed over the
// quotes SSE stream. The keys match the Position freshness fields so a
// single React row-patch handler can update both data sources.
export interface PortfolioQuote {
  instrumentKey: string;
  symbol: string;
  market?: string;
  assetClass?: string;
  currentPrice: number;
  marketValue?: number;
  priceAsOf?: string;
  priceSource?: string;
  isStale?: boolean;
}

export interface PortfolioQuotesFrame {
  quotes: PortfolioQuote[];
}

// Long-term reflections (F3.4) — the read side of memory.Reflect. Returned
// oldest-first so the UI can render them as an upward-flowing timeline that
// matches the existing activity log convention.
export interface ReflectionItem {
  id: string;
  fundId: string;
  theme: string;
  title: string;
  content: string;
  tags?: string[];
  tradingDate?: string;
  createdAt: string;
}

export interface ReflectionListResponse {
  fundId: string;
  items: ReflectionItem[];
  generatedAt: string;
}

export function fetchReflections(fundId: string, limit = 50): Promise<ReflectionListResponse> {
  const params = new URLSearchParams();
  if (Number.isFinite(limit) && limit > 0) {
    params.set("limit", String(Math.floor(limit)));
  }
  const query = params.toString();
  return apiGet<ReflectionListResponse>(`/api/funds/${fundId}/reflections${query ? `?${query}` : ""}`);
}

// ---------------------------------------------------------------------------
// Phase 3A-5: strategy attribution / closed-loop learning rail
// ---------------------------------------------------------------------------

// IMPORTANT: backend JSON tags are `totalPnl` / `avgPnlPct`
// (lowercase trailing letter — see api.SleeveStat in
// server/internal/api/fund_handler.go). These interfaces used to
// declare `totalPnL` / `avgPnLPct`, which at runtime resolved to
// `undefined` and crashed the StrategyAttributionPanel render
// (`undefined.toLocaleString(...)` blank-paged the entire
// /funds/:id/performance route).
export interface AttributionSleeveStat {
  sleeve: string;
  tradeCount: number;
  winCount: number;
  lossCount: number;
  totalPnl: number;
  avgPnlPct: number;
  winRate: number;
  medianHoldDays: number;
}

export interface AttributionRegimeStat {
  regime: string;
  tradeCount: number;
  winCount: number;
  lossCount: number;
  totalPnl: number;
  avgPnlPct: number;
  winRate: number;
}

export interface AttributionSleeveRegimeStat {
  sleeve: string;
  regime: string;
  tradeCount: number;
  winCount: number;
  lossCount: number;
  totalPnl: number;
  avgPnlPct: number;
  winRate: number;
  avgHoldingDays: number;
}

// Severity / kind enums are emitted by the backend lesson
// generator. Keeping them as wide string unions lets a future
// backend extension surface a new kind without breaking the
// frontend build.
export type AttributionLessonKind = "sleeve_regime_loser" | "sleeve_regime_winner" | "insufficient_data" | string;
export type AttributionLessonSeverity = "info" | "warning" | "critical" | string;

export interface AttributionLesson {
  kind: AttributionLessonKind;
  severity: AttributionLessonSeverity;
  title: string;
  body: string;
  tags: string[];
  createdAt: string;
  // i18n contract (server migration 085, S15): same template_key +
  // payload that MemoryEntry / AgentLearningRecord carry. Optional;
  // legacy rows leave both unset and the panel falls back to the
  // English title/body. See web/src/lib/lessonRenderer.ts.
  templateKey?: string;
  payload?: Record<string, unknown>;
}

export interface AttributionResponse {
  fundId: string;
  windowDays: number;
  since: string;
  generatedAt: string;
  bySleeve: AttributionSleeveStat[];
  byRegime: AttributionRegimeStat[];
  bySleeveRegime: AttributionSleeveRegimeStat[];
  lessons: AttributionLesson[];
}

function attributionUrl(fundId: string, days?: number): string {
  const params = new URLSearchParams();
  if (Number.isFinite(days) && days && days > 0) {
    params.set("days", String(Math.floor(days)));
  }
  const query = params.toString();
  return `/api/funds/${fundId}/strategy-attribution${query ? `?${query}` : ""}`;
}

// fetchStrategyAttribution is the read-only path: pulls the
// latest cross-tab + persisted lessons without forcing a new
// run. Cheap; safe to call on tab focus.
export function fetchStrategyAttribution(fundId: string, days?: number): Promise<AttributionResponse> {
  return apiGet<AttributionResponse>(attributionUrl(fundId, days));
}

// refreshStrategyAttribution forces the backend attribution
// service to rebuild the report AND persist any new lessons
// immediately. Use it from the "refresh now" button on the
// dashboard so the operator sees the current state without
// waiting for the daily review hook.
export function refreshStrategyAttribution(fundId: string, days?: number): Promise<AttributionResponse> {
  const params = new URLSearchParams();
  if (Number.isFinite(days) && days && days > 0) {
    params.set("days", String(Math.floor(days)));
  }
  const query = params.toString();
  return apiPost<AttributionResponse>(`/api/funds/${fundId}/strategy-attribution/refresh${query ? `?${query}` : ""}`);
}

// NextWorkflowRun is the wall-clock schedule preview surfaced on the
// Decision Center / Agent Learning banners so users can answer
// "when does the agent next run?" without reading logs. Mirrors
// api.NextWorkflowRun on the server. All timestamps are RFC3339 UTC;
// callers format them in the user's locale.
export interface NextWorkflowRun {
  fundId: string;
  tradingDate: string;
  timezone?: string;
  nextTriggerAt: string;
  currentlyInWindow: boolean;
  // Legacy single-shot 10-step schedule. Present only when the fund
  // is NOT running in interval mode.
  steps?: {
    macroBrief: string;
    researchParallel: string;
    quantSignals: string;
    roundtable: string;
    pmPlan: string;
    riskReview: string;
    userApproval: string;
    tradeExecution: string;
    settlement: string;
    dailyReview: string;
  };
  // Per-day decision-trigger slots for interval mode (e.g. every
  // 30 minutes). Present only when intervalMinutes is set.
  slots?: string[];
  // Active per-fund decision cadence in minutes. Present only when
  // interval mode is configured for this fund.
  intervalMinutes?: number;
}

// fetchNextWorkflowRun returns when the scheduler will next wake up
// for the fund. The frontend uses this for the "下次自动决策"
// banner; null is returned when the calendar service is unavailable
// (503) so the banner can fall back to a placeholder. Other errors
// still throw so the page can surface them to the operator.
export async function fetchNextWorkflowRun(fundId: string): Promise<NextWorkflowRun | null> {
  try {
    return await apiGet<NextWorkflowRun>(`/api/funds/${fundId}/workflow/next-run`);
  } catch (err) {
    if (err instanceof ApiError && err.status === 503) {
      return null;
    }
    throw err;
  }
}

// Agent skill library (F4) — read + approve/reject the candidate skills
// produced by the reflection engine. Approved entries flow into the prompt
// resolver immediately; rejected entries are removed from the library and
// will only reappear if a future reflection regenerates them.
export interface AgentSkillEntry {
  key: string;
  name: string;
  description?: string;
  content?: string;
  status: string;
  source?: string;
  enabled: boolean;
  priority: number;
  roles?: string[];
  focuses?: string[];
  proposedAt?: string;
  approvedAt?: string;
}

export interface AgentSkillListResponse {
  agentId: string;
  skills: AgentSkillEntry[];
}

export function fetchAgentSkills(agentId: string): Promise<AgentSkillListResponse> {
  return apiGet<AgentSkillListResponse>(`/api/agents/${agentId}/skills`);
}

export function approveAgentSkill(agentId: string, skillKey: string): Promise<AgentSkillEntry> {
  return apiRequest<AgentSkillEntry>(`/api/agents/${agentId}/skills/${encodeURIComponent(skillKey)}/approve`, { method: "POST" });
}

export function rejectAgentSkill(agentId: string, skillKey: string): Promise<void> {
  return apiRequest<void>(`/api/agents/${agentId}/skills/${encodeURIComponent(skillKey)}`, { method: "DELETE" });
}

// Marketplace auctions (F5) — English ascending + wallet hold escrow +
// anti-sniping. The server returns the same listing shape regardless of
// viewer; sensitive fields (reserve, current bidder) are filled only when
// the viewer is the seller (the API redacts them otherwise).
export interface AuctionTrustSignals {
  score: number;
  level: string;
  badges?: string[];
  evidence?: string[];
  learningRecords?: number;
  publicMemoryRecords?: number;
  lastLearningAt?: string;
  lastDailyReturn?: number;
  modelConfigured?: boolean;
  profileCompleteness?: number;
  listingAgeDays?: number;
}

export interface AuctionListing {
  id: string;
  sellerUserId?: string;
  sourceFundId: string;
  sourceAgentId: string;
  agentName: string;
  agentRole: string;
  agentFocus?: string;
  latestLearningSummary?: string;
  mode: string;
  status: string;
  currency: string;
  startingPriceMinor: number;
  reserveMinor?: number;
  minIncrementMinor: number;
  antiSnipeSeconds: number;
  currentBidMinor?: number;
  currentBidderUserId?: string;
  currentBidId?: string;
  minNextBidMinor: number;
  startsAt?: string;
  endsAt?: string;
  settledAt?: string;
  winningBidId?: string;
  winnerUserId?: string;
  snapshotPayload?: unknown;
  trust?: AuctionTrustSignals;
  createdAt: string;
  updatedAt: string;
}

export interface AuctionBid {
  id: string;
  listingId: string;
  bidderUserId?: string;
  bidPriceMinor: number;
  currency: string;
  status: string;
  createdAt: string;
  updatedAt: string;
}

export interface AuctionSettlementResult {
  listingId: string;
  outcome: "sold" | "reserve_not_met" | "no_bids" | string;
  winningBidId?: string;
  winnerUserId?: string;
  finalBidMinor?: number;
  order?: {
    id: string;
    deliveredAgentId: string;
    amountMinor: number;
    currency: string;
    status: string;
  };
}

export interface PlaceAuctionBidResponse {
  bid: AuctionBid;
  auction?: AuctionListing;
}

export interface CreateAuctionInput {
  fundId: string;
  agentId: string;
  startingPriceMinor: number;
  reserveMinor?: number;
  minIncrementMinor?: number;
  antiSnipeSeconds?: number;
  currency?: string;
  startsAt?: string;
  endsAt: string;
}

export function fetchAuctions(limit = 50): Promise<AuctionListing[]> {
  const params = new URLSearchParams();
  if (Number.isFinite(limit) && limit > 0) {
    params.set("limit", String(Math.floor(limit)));
  }
  const query = params.toString();
  return apiGet<AuctionListing[]>(`/api/marketplace/auctions${query ? `?${query}` : ""}`);
}

export function fetchAuction(listingId: string): Promise<AuctionListing> {
  return apiGet<AuctionListing>(`/api/marketplace/auctions/${encodeURIComponent(listingId)}`);
}

export function createAuction(input: CreateAuctionInput): Promise<AuctionListing> {
  return apiPost<AuctionListing>(`/api/marketplace/auctions`, input);
}

export function placeAuctionBid(listingId: string, bidPriceMinor: number, currency?: string): Promise<PlaceAuctionBidResponse> {
  return apiPost<PlaceAuctionBidResponse>(
    `/api/marketplace/auctions/${encodeURIComponent(listingId)}/bids`,
    { bidPriceMinor, currency },
  );
}

export function settleAuction(listingId: string): Promise<AuctionSettlementResult> {
  return apiPost<AuctionSettlementResult>(
    `/api/marketplace/auctions/${encodeURIComponent(listingId)}/settle`,
  );
}

export function formatApiError(error: unknown, fallback: string): string {
  if (error instanceof ApiError) {
    return error.detail ? `${error.message}：${error.detail}` : error.message;
  }
  if (error instanceof Error) {
    return error.message;
  }
  return fallback;
}

// ---------------------------------------------------------------------------
// Fund Assist (LLM-backed natural-language fund + team creation)
// ---------------------------------------------------------------------------
//
// The "describe a fund + team in plain Chinese / English and we'll
// create it" flow that backs <FundAssistDialog />. The wire-shape
// below mirrors api.FundAssistRequest / FundAssistResponse on the
// server side; if you change it, also touch
// server/internal/api/fund_assist.go and re-run the API tests.

export interface FundAssistPlanUniverse {
  mode?: string;
  symbols?: string[];
  themes?: string[];
}

export interface FundAssistPlanSpecialization {
  markets?: string[];
  assetClasses?: string[];
  themes?: string[];
  instruments?: string[];
  styleHints?: string[];
}

export interface FundAssistPlanFund {
  name: string;
  description?: string;
  market: string;
  exchange?: string;
  assetClass?: string;
  baseCurrency?: string;
  benchmarkSymbol?: string;
  primaryDirection?: string;
  initialCapital?: number;
  universe?: FundAssistPlanUniverse;
  specialization?: FundAssistPlanSpecialization;
}

export interface FundAssistPlanAgent {
  role: string;
  name?: string;
  focus?: string;
  systemPrompt?: string;
}

export interface FundAssistPlan {
  fund: FundAssistPlanFund;
  agents: FundAssistPlanAgent[];
  rationale?: string;
}

export interface FundAssistAgentResult {
  id: string;
  role: string;
  focus?: string;
}

export interface FundAssistResponse {
  fundId?: string;
  fund?: { id: string; name: string; market?: string };
  agents?: FundAssistAgentResult[];
  plan: FundAssistPlan;
  warnings?: string[];
}

export interface FundAssistPlanIssue {
  field: string;
  code: string;
  message: string;
}

// FundAssistRejectedError mirrors the 422 response shape from the
// /assist endpoint: a structured "plan rejected" result the UI can
// render as a list of corrections rather than a generic toast. We
// extract the typed payload at the api boundary so callers can do
// `if (err instanceof FundAssistRejectedError) { ... }` without
// re-parsing the JSON.
export class FundAssistRejectedError extends Error {
  readonly issues: FundAssistPlanIssue[];
  readonly plan?: FundAssistPlan;
  readonly warnings: string[];
  constructor(payload: { detail?: string; issues?: FundAssistPlanIssue[]; plan?: FundAssistPlan; warnings?: string[] }) {
    super(payload.detail ?? i18n.t("apiErrors:planValidationFailed"));
    this.name = "FundAssistRejectedError";
    this.issues = payload.issues ?? [];
    this.plan = payload.plan;
    this.warnings = payload.warnings ?? [];
  }
}

export async function assistCreateFund(
  companyId: string,
  body: { prompt: string; dryRun?: boolean; languageHint?: string },
): Promise<FundAssistResponse> {
  try {
    return await apiPost<FundAssistResponse>(
      `/api/companies/${encodeURIComponent(companyId)}/funds:assist`,
      body,
    );
  } catch (err) {
    // 422 plan_rejected is the one error path that carries
    // structured payload the UI needs verbatim — re-throw as a
    // typed error so the caller can render the issues list. Any
    // other ApiError (400 / 502 / 503 / 500) bubbles up as-is and
    // is rendered with formatApiError.
    if (err instanceof ApiError && err.status === 422 && err.payload) {
      const payload = err.payload as { detail?: string; issues?: FundAssistPlanIssue[]; plan?: FundAssistPlan; warnings?: string[] };
      throw new FundAssistRejectedError(payload);
    }
    throw err;
  }
}

// ---------------------------------------------------------------------------
// S13 — Platform LLM Provider Admin
// ---------------------------------------------------------------------------
//
// Hot-reloaded CRUD for the platform_llm_providers table. The admin
// UI uses these four (technically five) calls; the Model A/B form
// also pulls from listAdminLLMProviders to populate its provider
// dropdown so the user can't accidentally pick an unconfigured one.
// API keys NEVER round-trip — the response carries only the
// fingerprint + masked preview.

export interface AdminLLMProvider {
  id: string;
  provider: string;
  label: string;
  model_tier?: string;
  model_name: string;
  base_url: string;
  api_key_fingerprint: string;
  api_key_masked_preview: string;
  api_key_configured: boolean;
  max_tokens: number;
  temperature: number;
  input_price_per_1m?: number;
  output_price_per_1m?: number;
  cost_per_1m?: number;
  status: "active" | "disabled" | "draft";
  is_platform_default: boolean;
  last_health_check_at?: string;
  last_health_check_result?: {
    ok?: boolean;
    latency_ms?: number;
    http_status?: number;
    message?: string;
    echoed_model?: string;
    checked_at?: string;
  };
  source: string;
  created_at: string;
  updated_at: string;
}

export interface AdminLLMProvidersListResponse {
  providers: AdminLLMProvider[];
  reload_generation: number;
  router_active_keys: Record<string, boolean> | null;
}

export interface UpsertAdminLLMProviderRequest {
  id?: string;
  provider: string;
  label: string;
  model_tier?: string;
  model_name: string;
  base_url: string;
  api_key?: string;
  max_tokens?: number;
  temperature?: number;
  input_price_per_1m?: number;
  output_price_per_1m?: number;
  cost_per_1m?: number;
  status?: "active" | "disabled" | "draft";
}

export interface TestAdminLLMProviderRequest {
  id?: string;
  provider: string;
  model_name: string;
  base_url: string;
  api_key?: string;
}

export interface TestAdminLLMProviderResponse {
  ok: boolean;
  latency_ms: number;
  http_status?: number;
  message?: string;
  echoed_model?: string;
}

export async function listAdminLLMProviders(opts: {
  provider?: string;
  status?: "active" | "disabled" | "draft";
  tier?: string;
} = {}): Promise<AdminLLMProvidersListResponse> {
  const qs = new URLSearchParams();
  if (opts.provider) qs.set("provider", opts.provider);
  if (opts.status) qs.set("status", opts.status);
  if (opts.tier) qs.set("tier", opts.tier);
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<AdminLLMProvidersListResponse>(`/api/admin/llm-providers${tail}`);
}

export async function upsertAdminLLMProvider(
  body: UpsertAdminLLMProviderRequest,
): Promise<AdminLLMProvider> {
  return apiPut<AdminLLMProvider>(`/api/admin/llm-providers`, body);
}

export async function deleteAdminLLMProvider(id: string): Promise<{ ok: boolean; deleted_id: string }> {
  return apiDelete<{ ok: boolean; deleted_id: string }>(
    `/api/admin/llm-providers/${encodeURIComponent(id)}`,
  );
}

export async function setAdminLLMProviderDefault(id: string): Promise<AdminLLMProvider> {
  return apiPost<AdminLLMProvider>(
    `/api/admin/llm-providers/${encodeURIComponent(id)}/default`,
  );
}

export async function testAdminLLMProvider(
  body: TestAdminLLMProviderRequest,
): Promise<TestAdminLLMProviderResponse> {
  return apiPost<TestAdminLLMProviderResponse>(`/api/admin/llm-providers/test`, body);
}

// ===== S14.A — provider observability ============================
// These endpoints feed the admin "看板" tab. The backend returns
// stable shapes (empty arrays, not 404s) so the UI doesn't need a
// "not yet initialised" branch on first boot.

export interface ProviderHealthSparklinePoint {
  checked_at: string;
  ok: boolean;
  latency_ms: number;
}

export interface ProviderHealthDashboardRow {
  provider_id: string;
  provider: string;
  label: string;
  checks: number;
  successes: number;
  failures: number;
  success_rate: number;
  latency_p50_ms: number;
  latency_p95_ms: number;
  latency_max_ms: number;
  last_checked_at?: string;
  last_ok?: boolean;
  sparkline: ProviderHealthSparklinePoint[];
}

export interface ProviderHealthDashboardResponse {
  window_start: string;
  window_end: string;
  probe_ticks_since_boot: number;
  rows: ProviderHealthDashboardRow[];
}

export interface ProviderCostTotal {
  provider: string;
  calls: number;
  total_tokens: number;
  cost_cents: number;
  cost_usd: number;
  days_in_window: number;
}

export interface ProviderCostDaily {
  provider: string;
  model_name: string;
  day: string; // YYYY-MM-DD
  calls: number;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  cost_cents: number;
  cost_usd: number;
  last_rolled_at: string;
}

export interface ProviderCostDashboardResponse {
  window_start_day: string;
  window_end_day: string;
  rollup_ticks_since_boot: number;
  totals: ProviderCostTotal[];
  daily: ProviderCostDaily[];
}

export interface ProviderHistoryRow {
  id: string;
  checked_at: string;
  ok: boolean;
  latency_ms: number;
  http_status: number;
  message?: string;
  model_name?: string;
}

export interface ProviderHistoryResponse {
  provider_id: string;
  window_start: string;
  window_end: string;
  rows: ProviderHistoryRow[];
}

export async function getAdminProviderHealthDashboard(
  range: '6h' | '24h' | '7d' | '30d' = '24h',
): Promise<ProviderHealthDashboardResponse> {
  return apiGet<ProviderHealthDashboardResponse>(
    `/api/admin/llm-providers/health?range=${range}`,
  );
}

export async function getAdminProviderCostDashboard(
  range: '24h' | '7d' | '30d' = '7d',
  provider?: string,
): Promise<ProviderCostDashboardResponse> {
  const params = new URLSearchParams({ range });
  if (provider && provider.trim()) params.set('provider', provider.trim());
  return apiGet<ProviderCostDashboardResponse>(
    `/api/admin/llm-providers/cost?${params.toString()}`,
  );
}

export async function getAdminProviderHistory(
  providerId: string,
  range: '6h' | '24h' | '7d' | '30d' = '24h',
  limit = 500,
): Promise<ProviderHistoryResponse> {
  return apiGet<ProviderHistoryResponse>(
    `/api/admin/llm-providers/${encodeURIComponent(providerId)}/history?range=${range}&limit=${limit}`,
  );
}

// ===== S14.B — per-fund LLM provider overrides ===================
// These endpoints live under /api/funds/{fundId}/... (not /admin/)
// because the strategy owner — not necessarily a platform admin —
// manages them. The server enforces fund-ownership via authorizeFundAccess.

export interface FundLLMOverride {
  id: string;
  fund_id: string;
  agent_id?: string | null;
  role?: string;
  model_tier?: string;
  provider: string;
  label?: string;
  model_name?: string;
  effective_provider?: string;
  effective_label?: string;
  effective_model_name?: string;
  enabled: boolean;
  note?: string;
  specificity: number;
  created_at?: string;
  updated_at?: string;
}

export interface ListFundLLMOverridesResponse {
  overrides: FundLLMOverride[];
}

export interface UpsertFundLLMOverrideRequest {
  id?: string;
  agent_id?: string | null;
  role?: string;
  model_tier?: string;
  provider: string;
  label?: string;
  model_name?: string;
  enabled: boolean;
  note?: string;
}

export async function listFundLLMOverrides(fundId: string): Promise<ListFundLLMOverridesResponse> {
  return apiGet<ListFundLLMOverridesResponse>(
    `/api/funds/${encodeURIComponent(fundId)}/llm-overrides`,
  );
}

export async function upsertFundLLMOverride(
  fundId: string,
  body: UpsertFundLLMOverrideRequest,
): Promise<FundLLMOverride> {
  return apiPut<FundLLMOverride>(
    `/api/funds/${encodeURIComponent(fundId)}/llm-overrides`,
    body,
  );
}

export async function deleteFundLLMOverride(
  fundId: string,
  overrideId: string,
): Promise<{ ok: boolean; deleted_id: string }> {
  return apiDelete<{ ok: boolean; deleted_id: string }>(
    `/api/funds/${encodeURIComponent(fundId)}/llm-overrides/${encodeURIComponent(overrideId)}`,
  );
}

export interface FundLLMCatalogEntry {
  provider: string;
  label?: string;
  model_tier?: string;
  model_name?: string;
  is_platform_default?: boolean;
}

export interface ListFundLLMCatalogResponse {
  providers: FundLLMCatalogEntry[];
}

// Read-only catalog of LLM providers/models a fund's owner is
// allowed to pick from. Backed by GET /api/funds/{fundId}/llm-catalog
// which projects platform_llm_providers (status='enabled') without
// exposing API key / fingerprint / pricing surface. Used by the A/B
// test creation modal so operators don't have to memorise provider
// strings.
export async function listFundLLMCatalog(fundId: string): Promise<ListFundLLMCatalogResponse> {
  return apiGet<ListFundLLMCatalogResponse>(
    `/api/funds/${encodeURIComponent(fundId)}/llm-catalog`,
  );
}

// ---------------------------------------------------------------------------
// Advisor (大师团队咨询) — migration 098
// ---------------------------------------------------------------------------
//
// Separate from the fund/team subsystem: all endpoints hang off
// /api/advisor/* and carry no fundId. Auth uses the same session cookie.
// The whole surface can be 503'd by toggling the `advisor_mode` feature
// flag in the admin console.

export type AdvisorVerdict =
  | "STRONG_BUY"
  | "BUY"
  | "HOLD"
  | "AVOID"
  | "SHORT"
  | "MIXED"
  | "SKIP"
  | "PASS";

export type AdvisorPresetKind = "masters" | "tactics" | "mixed" | "empty";

export interface AdvisorPreset {
  preset_key: string;
  label_zh: string;
  label_en: string;
  description_zh: string;
  description_en: string;
  master_keys: string[];
  tactic_keys: string[];
  kind: AdvisorPresetKind;
  sort_order: number;
}

export interface AdvisorMasterReport {
  master_key: string;
  master_name_zh: string;
  master_name_en: string;
  verdict: AdvisorVerdict;
  confidence: number;
  thesis: string;
  key_reasons: string[];
  key_risks: string[];
  master_specific?: Record<string, unknown>;
  red_lines_hit?: string[];
  red_lines_hit_en?: string[];
  llm_model?: string;
  generated_at: string;
}

export interface AdvisorTacticReport {
  tactic_key: string;
  tactic_name_zh: string;
  tactic_name_en: string;
  verdict: string;
  confidence: number;
  thesis: string;
  entry_price_low?: number | null;
  entry_price_high?: number | null;
  stop_loss_price?: number | null;
  target_t1?: number | null;
  target_t3?: number | null;
  expected_holding_days?: number | null;
  score: number;
  key_reasons: string[];
  key_risks: string[];
  red_lines_hit?: string[];
  market_regime_pass: boolean;
  market_regime_reason?: string;
  generated_at: string;
}

/**
 * Price-action / momentum / volatility snapshot the master
 * panel saw when it formed its verdict. Mirrors the Go
 * agent.MasterTechnicalBlock — see master_agent.go for the
 * compliance contract (rule 9: model can QUOTE these values
 * but never project them into price targets / signals).
 *
 * Omitted when the wiring layer's OHLC fetcher couldn't reach
 * the symbol (Yahoo throttled, market unsupported, etc.). The
 * detail-modal renderer handles the absent case by simply
 * not showing the technical section.
 */
export interface MasterTechnicalBlock {
  /** UTC RFC3339 of the latest bar used in the computation. */
  asof?: string;
  bars_used?: number;
  last_close?: number;
  /** Decimals (0.05 = +5%), not percent. */
  pct_change_1d?: number;
  pct_change_5d?: number;
  pct_change_20d?: number;
  /** Negative when below the 52-week high; 0 at a fresh high. */
  pct_change_from_52w_high?: number;
  sma20?: number;
  sma50?: number;
  sma200?: number;
  /** "bullish" | "bearish" | "mixed". */
  ma_alignment?: string;
  rsi14?: number;
  /** "overbought" | "oversold" | "neutral". */
  rsi14_zone?: string;
  macd_line?: number;
  macd_signal?: number;
  macd_hist?: number;
  /** "bullish" | "bearish" | "" — fresh cross at latest bar. */
  macd_cross?: string;
  atr14_pct_of_price?: number;
  kdj_k?: number;
  kdj_d?: number;
  kdj_j?: number;
  volume?: number;
  /** Latest bar / 20-bar SMA. >1 = above average. */
  relative_volume?: number;
  support?: number;
  resistance?: number;
  sr_window?: number;
  /** "above_resistance" | "below_support" | "near_resistance" | "near_support" | "". */
  breakout_state?: string;
  /** Pre-formatted bullet items, safe to render as-is. */
  tags?: string[];
}

export interface AdvisorConsultResponse {
  consultation_id: string;
  symbol: string;
  /**
   * Issuer's short Chinese / English name (e.g. "德科立",
   * "Apple Inc."). Omitted when the upstream data provider
   * could not resolve a name; UI falls back to bare symbol.
   */
  symbol_name?: string;
  preset_key: string;
  aggregate_verdict: AdvisorVerdict;
  aggregate_confidence: number;
  consensus_score: number;
  master_reports: AdvisorMasterReport[];
  tactic_reports: AdvisorTacticReport[];
  /**
   * Technical snapshot fed into the master panel. Lands on
   * daily_picks.result_json so the detail-modal renders the
   * same table on every reader's screen.
   */
  technical?: MasterTechnicalBlock;
  created_at: string;
}

export interface AdvisorConsultRequest {
  symbol: string;
  preset_key: string;
  market?: string;
  asset_class?: string;
  custom_master_keys?: string[];
  custom_tactic_keys?: string[];
  notes?: string;
  price_last?: number;
  price_change?: number;
  currency?: string;
}

export interface AdvisorConsultationSummary {
  id: string;
  symbol: string;
  /** See AdvisorConsultResponse.symbol_name. */
  symbol_name?: string;
  market?: string;
  preset_key: string;
  aggregate_verdict: AdvisorVerdict;
  aggregate_confidence: number;
  consensus_score: number;
  master_count: number;
  tactic_count: number;
  created_at: string;
}

export interface AdvisorHistoryResponse {
  consultations: AdvisorConsultationSummary[];
  details?: AdvisorConsultationDetail[];
}

export interface AdvisorConsultationDetail {
  id: string;
  symbol: string;
  /** See AdvisorConsultResponse.symbol_name. */
  symbol_name?: string;
  market?: string;
  asset_class?: string;
  preset_key: string;
  aggregate_verdict: AdvisorVerdict;
  aggregate_confidence: number;
  consensus_score: number;
  notes?: string;
  price_at_consult?: number | null;
  master_reports: AdvisorMasterReport[];
  tactic_reports: AdvisorTacticReport[];
  created_at: string;
}

export interface AdvisorHealthResponse {
  status: string;
  masters_loaded: boolean;
  tactics_loaded: boolean;
  server_time: string;
}

export async function fetchAdvisorHealth(): Promise<AdvisorHealthResponse> {
  return apiGet<AdvisorHealthResponse>("/api/advisor/health");
}

export async function listAdvisorPresets(): Promise<{ presets: AdvisorPreset[] }> {
  return apiGet<{ presets: AdvisorPreset[] }>("/api/advisor/presets");
}

export async function consultAdvisor(req: AdvisorConsultRequest): Promise<AdvisorConsultResponse> {
  return apiPost<AdvisorConsultResponse>("/api/advisor/consult", req);
}

export async function listAdvisorHistory(params?: {
  limit?: number;
  symbol?: string;
  preset_key?: string;
  include_children?: boolean;
}): Promise<AdvisorHistoryResponse> {
  const query = new URLSearchParams();
  if (params?.limit) query.set("limit", String(params.limit));
  if (params?.symbol) query.set("symbol", params.symbol);
  if (params?.preset_key) query.set("preset_key", params.preset_key);
  if (params?.include_children) query.set("include", "children");
  const qs = query.toString();
  return apiGet<AdvisorHistoryResponse>(`/api/advisor/history${qs ? `?${qs}` : ""}`);
}

export async function getAdvisorConsultation(id: string): Promise<AdvisorConsultationDetail> {
  return apiGet<AdvisorConsultationDetail>(
    `/api/advisor/consultations/${encodeURIComponent(id)}`,
  );
}

// ============================================================================
// /api/daily-picks — publisher-mode shared cache surface
// ============================================================================

/**
 * A single row in the daily picks browse grid. The shape is a
 * PROJECTION of the underlying daily_picks row — the full
 * ConsultResponse only ships on the detail endpoint to keep the
 * list response bounded (50 rows × full panel JSON would be ~1MB).
 */
export interface DailyPickRow {
  symbol: string;
  /** Issuer's short name, e.g. "Apple Inc.". Optional. */
  symbol_name?: string;
  market: string;
  preset_key: string;
  /** ISO yyyy-mm-dd. */
  pick_date: string;
  aggregate_verdict: AdvisorVerdict;
  /**
   * 0-100 (or NEGATIVE for AVOID/SHORT) — see backend
   * advisor.PublishConsult comment. Browse grid sorts DESC so
   * strongest BUY floats to the top.
   */
  aggregate_score: number;
  consensus: number;
  /** Verbatim thesis sentence of the highest-confidence master. */
  headline_thesis?: string;
  /** True when the publisher run for this cell failed; UI greys out. */
  has_error?: boolean;
}

export interface DailyPicksListResponse {
  picks: DailyPickRow[];
  total_count: number;
  /** Subscriber tier of the requester. */
  tier: string;
  /** Days of lag applied to the free tier. */
  free_lag_days: number;
  /** ISO yyyy-mm-dd of the newest row in the DB across all tiers. */
  newest_available_date?: string;
  /** ISO yyyy-mm-dd of the newest row this tier can see. */
  newest_for_tier_date?: string;
  /**
   * True when today's set exists but the requester's tier can't
   * see it. UI uses this to render an upgrade overlay over the
   * "today" tab. False when there's nothing to upgrade for.
   */
  upgrade_required_for_today: boolean;
}

/**
 * Detail-endpoint response. `pick` is the full ConsultResponse
 * JSON shape (master_reports, tactic_reports, etc.) so the
 * existing MasterVerdictCard / TacticVerdictCard render it
 * unchanged.
 */
// ============================================================================
// /api/trending — compliance-friendly market observation lists
// ============================================================================

/**
 * One row in the Most Active by Volume list. Pure data — no
 * verdicts, no subjective text. The ordering criterion is
 * disclosed in the response payload (criteria_disclosed).
 */
export interface TrendingMostActiveRow {
  rank: number;
  symbol: string;
  symbol_name?: string;
  last_close: number;
  /** Decimal, not percent (0.012 = +1.2%). */
  pct_change_1d: number;
  volume: number;
  /** Latest volume / 20-bar SMA volume. >1 = above average. */
  vol_20d_ratio: number;
  /** UTC RFC3339 of the latest bar. */
  asof?: string;
}

export interface TrendingMostActiveResponse {
  list_name: string;
  /** Publicly disclosed objective criteria — the algorithmic-output proof. */
  criteria_disclosed: string[];
  market: string;
  generated_at: string;
  universe_size: number;
  results: TrendingMostActiveRow[];
  /** Bottom-of-page legal disclaimer. */
  disclaimer: string;
}

export async function getTrendingMostActive(params?: {
  market?: string;
  limit?: number;
}): Promise<TrendingMostActiveResponse> {
  const q = new URLSearchParams();
  if (params?.market) q.set("market", params.market);
  if (params?.limit) q.set("limit", String(params.limit));
  const qs = q.toString();
  return apiGet<TrendingMostActiveResponse>(
    `/api/trending/most-active${qs ? `?${qs}` : ""}`,
  );
}

export interface DailyPickDetailResponse {
  pick: AdvisorConsultResponse;
  symbol: string;
  symbol_name?: string;
  market: string;
  preset_key: string;
  pick_date: string;
  tier: string;
  /**
   * How many distinct stock-day pairs this user has opened today.
   * Re-opens of the SAME (stock, day) do not increment — the
   * counter is "what did you read", not "how many requests did
   * you fire".
   */
  quota_used_today: number;
  /** -1 = unlimited. */
  quota_cap_today: number;
}

export interface DailyPicksStatusPresetView {
  preset: string;
  market: string;
  total: number;
  done: number;
  error_count: number;
  last_run_at?: string;
  status: "pending" | "running" | "stalled" | "completed";
}

export interface DailyPicksStatusResponse {
  today: string; // YYYY-MM-DD
  overall: "pending" | "running" | "stalled" | "completed";
  total_all: number;
  done_all: number;
  presets: DailyPicksStatusPresetView[];
}

/** GET /api/daily-picks/status — 进度面板用，建议每 30s 刷新 */
export async function getDailyPicksStatus(): Promise<DailyPicksStatusResponse> {
  return apiGet<DailyPicksStatusResponse>(`/api/daily-picks/status`);
}

export async function listDailyPicks(params?: {
  market?: string;
  preset?: string;
  date?: string;
  limit?: number;
  offset?: number;
}): Promise<DailyPicksListResponse> {
  const query = new URLSearchParams();
  if (params?.market) query.set("market", params.market);
  if (params?.preset) query.set("preset", params.preset);
  if (params?.date) query.set("date", params.date);
  if (params?.limit) query.set("limit", String(params.limit));
  if (params?.offset) query.set("offset", String(params.offset));
  const qs = query.toString();
  return apiGet<DailyPicksListResponse>(
    `/api/daily-picks${qs ? `?${qs}` : ""}`,
  );
}

export async function getDailyPickDetail(
  date: string,
  symbol: string,
  params?: { market?: string; preset?: string },
): Promise<DailyPickDetailResponse> {
  const query = new URLSearchParams();
  if (params?.market) query.set("market", params.market);
  if (params?.preset) query.set("preset", params.preset);
  const qs = query.toString();
  return apiGet<DailyPickDetailResponse>(
    `/api/daily-picks/${encodeURIComponent(date)}/${encodeURIComponent(symbol)}${qs ? `?${qs}` : ""}`,
  );
}

/**
 * Admin-only: trigger one full publisher wave synchronously. Used
 * for ops and the e2e smoke test. Returns the number of picks
 * written.
 */
export async function adminRunDailyPicksOnce(): Promise<{ picks_written: number }> {
  return apiPost<{ picks_written: number }>("/api/daily-picks/_admin/run-once");
}

// --- Phase 5: advisor track record ----------------------------------------

export interface AdvisorTrackRecordRow {
  agent_id: string;
  agent_name: string;
  agent_kind: "master" | "tactic" | string;
  category: string;
  decisions_count: number;
  hits_count: number;
  misses_count: number;
  hit_rate: number;
  avg_alpha: number;
  avg_confidence: number;
  last_decision_at?: string;
  updated_at: string;
}

export interface AdvisorTrackRecordResponse {
  masters: AdvisorTrackRecordRow[];
  tactics: AdvisorTrackRecordRow[];
}

export async function fetchAdvisorTrackRecord(
  limit?: number,
): Promise<AdvisorTrackRecordResponse> {
  const qs = typeof limit === "number" ? `?limit=${limit}` : "";
  return apiGet<AdvisorTrackRecordResponse>(`/api/advisor/track-record${qs}`);
}

// ============================================================================
// Phase A/C — advisor billing summary
// ============================================================================

export interface AdvisorBillingSummary {
  plan_tier: string;
  year_month: string;
  deep_limit: number;
  deep_used: number;
  deep_remaining: number;
  quick_limit: number;
  quick_used: number;
  quick_remaining: number;
  next_reset_at: string;
  allow_advisor_byok: boolean;
  upgrade_suggested?: string;
  credit_deep_balance?: number;
  credit_quick_balance?: number;
  total_purchased_cents?: number;
}

export async function fetchAdvisorBillingSummary(): Promise<AdvisorBillingSummary> {
  return apiGet<AdvisorBillingSummary>("/api/advisor/billing/summary");
}

// ============================================================================
// Phase B — BYOK key CRUD
// ============================================================================

export interface AdvisorByokKey {
  id: string;
  provider: string;
  label: string;
  api_key_fingerprint: string;
  api_key_preview: string;
  base_url?: string;
  model_name?: string;
  monthly_budget_cents_usd: number;
  is_active: boolean;
  last_used_at?: string;
  last_verified_at?: string;
  revoked_at?: string;
  revoked_reason?: string;
  created_at: string;
}

export interface AdvisorByokListResponse {
  keys: AdvisorByokKey[];
}

export interface AdvisorByokCreateRequest {
  provider: string;
  label?: string;
  api_key: string;
  base_url?: string;
  model_name?: string;
  monthly_budget_cents_usd?: number;
}

export async function listAdvisorByokKeys(): Promise<AdvisorByokListResponse> {
  return apiGet<AdvisorByokListResponse>("/api/advisor/byok/keys");
}

export async function createAdvisorByokKey(
  req: AdvisorByokCreateRequest,
): Promise<AdvisorByokKey> {
  return apiPost<AdvisorByokKey>("/api/advisor/byok/keys", req);
}

export async function updateAdvisorByokBudget(
  keyId: string,
  monthlyBudgetCentsUSD: number,
): Promise<{ ok: boolean }> {
  return apiPut<{ ok: boolean }>(`/api/advisor/byok/keys/${encodeURIComponent(keyId)}/budget`, {
    monthly_budget_cents_usd: monthlyBudgetCentsUSD,
  });
}

export async function setAdvisorByokActive(
  keyId: string,
  isActive: boolean,
): Promise<{ ok: boolean; is_active: boolean }> {
  return apiPut<{ ok: boolean; is_active: boolean }>(
    `/api/advisor/byok/keys/${encodeURIComponent(keyId)}/active`,
    { is_active: isActive },
  );
}

export async function deleteAdvisorByokKey(
  keyId: string,
  reason?: string,
): Promise<{ ok: boolean }> {
  return apiRequest<{ ok: boolean }>(`/api/advisor/byok/keys/${encodeURIComponent(keyId)}`, {
    method: "DELETE",
    body: JSON.stringify({ reason: reason ?? "user_revoked" }),
  });
}

// ============================================================================
// Phase C — credit packs + checkout + order history
// ============================================================================

export interface AdvisorCreditPack {
  sku: string;
  label_zh: string;
  label_en: string;
  description_zh: string;
  description_en: string;
  deep_units: number;
  quick_units: number;
  price_cents_usd: number;
  sort_order: number;
  available: boolean;
}

export interface AdvisorCreditPackListResponse {
  packs: AdvisorCreditPack[];
  checkout_enabled: boolean;
}

export interface AdvisorCheckoutResponse {
  order_id: string;
  checkout_url: string;
  pack_sku: string;
}

export interface AdvisorCreditOrder {
  id: string;
  pack_sku: string;
  deep_units_granted: number;
  quick_units_granted: number;
  price_cents_usd: number;
  currency: string;
  status: "pending" | "paid" | "refunded" | "failed";
  lemonsqueezy_order_id?: string;
  checkout_url?: string;
  paid_at?: string;
  refunded_at?: string;
  created_at: string;
}

export interface AdvisorOrderListResponse {
  orders: AdvisorCreditOrder[];
}

export async function listAdvisorCreditPacks(): Promise<AdvisorCreditPackListResponse> {
  return apiGet<AdvisorCreditPackListResponse>("/api/advisor/credits/packs");
}

export async function startAdvisorCheckout(
  sku: string,
  opts?: { successUrl?: string; cancelUrl?: string; email?: string },
): Promise<AdvisorCheckoutResponse> {
  const qs = new URLSearchParams();
  if (opts?.successUrl) qs.set("success_url", opts.successUrl);
  if (opts?.cancelUrl) qs.set("cancel_url", opts.cancelUrl);
  if (opts?.email) qs.set("email", opts.email);
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiPost<AdvisorCheckoutResponse>(
    `/api/advisor/credits/packs/${encodeURIComponent(sku)}/checkout${tail}`,
  );
}

export async function listAdvisorOrders(limit?: number): Promise<AdvisorOrderListResponse> {
  const qs = typeof limit === "number" ? `?limit=${limit}` : "";
  return apiGet<AdvisorOrderListResponse>(`/api/advisor/billing/orders${qs}`);
}

export interface AdvisorBillingCall {
  id: string;
  symbol: string;
  preset_key: string;
  aggregate_verdict: string;
  service_unit_source: string;
  service_unit_cost: number;
  models_used: string[];
  byok_used: boolean;
  created_at: string;
}

export interface AdvisorBillingCallsResponse {
  calls: AdvisorBillingCall[];
}

export async function listAdvisorBillingCalls(opts?: {
  byokOnly?: boolean;
  limit?: number;
}): Promise<AdvisorBillingCallsResponse> {
  const qs = new URLSearchParams();
  if (opts?.byokOnly) qs.set("byok", "1");
  if (typeof opts?.limit === "number") qs.set("limit", String(opts.limit));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<AdvisorBillingCallsResponse>(`/api/advisor/billing/calls${tail}`);
}

export interface AdvisorByokInfo {
  egress_ip: string;
  support_email: string;
  encrypted_at_rest: boolean;
  providers_supported: string[];
}

export async function fetchAdvisorByokInfo(): Promise<AdvisorByokInfo> {
  return apiGet<AdvisorByokInfo>("/api/advisor/byok/info");
}

// --- Admin user-management console (read-only) -----------------------------
//
// Mirrors the wire shapes returned by server/cmd/server/admin_users_handler.go.
// Endpoints are admin-gated server-side; the client does its own role guard
// to redirect non-admins before they ever fire these requests, but the
// guard is convenience only — the server is the source of truth.

export interface AdminUsersListItem {
  id: string;
  username: string;
  displayName: string;
  email: string;
  role: string;
  status: string;
  kycStatus: string;
  createdAt: string;
  lastLoginAt?: string;
  currentTier: string;
  tierUntil?: string;
  lifetimeLLMCostCents: number;
  lifetimeLLMCalls: number;
}

export interface AdminUsersListResponse {
  users: AdminUsersListItem[];
  total: number;
  page: number;
  size: number;
}

export interface AdminUsersListParams {
  q?: string;
  tier?: string;
  page?: number;
  size?: number;
}

export interface AdminDailyCount {
  date: string;
  count: number;
}

export interface AdminTierCount {
  tier: string;
  count: number;
}

export interface AdminUsersStatsResponse {
  totalUsers: number;
  activeUsers7d: number;
  newUsers30d: AdminDailyCount[];
  tierDistribution: AdminTierCount[];
  mrrCents: number;
  asOf: string;
}

export interface AdminUserDetailProfile {
  id: string;
  username: string;
  displayName: string;
  email: string;
  phone?: string;
  role: string;
  status: string;
  kycStatus: string;
  kycLevel: string;
  emailVerified: boolean;
  createdAt: string;
  lastLoginAt?: string;
}

export interface AdminUserSubscription {
  planTier: string;
  status: string;
  startDate: string;
  endDate: string;
  paymentMethod?: string;
  autoRenew: boolean;
}

export interface AdminUsageBreakdown {
  key: string;
  calls: number;
  costCents: number;
}

export interface AdminUsageDayPoint {
  date: string;
  calls: number;
  costCents: number;
}

export interface AdminUserUsageSummary {
  lifetimeCalls: number;
  lifetimeCostCents: number;
  byStep: AdminUsageBreakdown[];
  byProvider: AdminUsageBreakdown[];
  last30d: AdminUsageDayPoint[];
}

export interface AdminUserDetailResponse {
  profile: AdminUserDetailProfile;
  subscriptions: AdminUserSubscription[];
  usageSummary: AdminUserUsageSummary;
  walletBalanceCents: number;
}

export async function fetchAdminUsersStats(): Promise<AdminUsersStatsResponse> {
  return apiGet<AdminUsersStatsResponse>("/api/admin/users/stats");
}

export async function fetchAdminUsersList(
  params: AdminUsersListParams = {},
): Promise<AdminUsersListResponse> {
  const qs = new URLSearchParams();
  if (params.q && params.q.trim() !== "") qs.set("q", params.q.trim());
  if (params.tier && params.tier.trim() !== "") qs.set("tier", params.tier.trim());
  if (typeof params.page === "number" && params.page > 0) qs.set("page", String(params.page));
  if (typeof params.size === "number" && params.size > 0) qs.set("size", String(params.size));
  const tail = qs.toString() ? `?${qs.toString()}` : "";
  return apiGet<AdminUsersListResponse>(`/api/admin/users${tail}`);
}

export async function fetchAdminUserDetail(userId: string): Promise<AdminUserDetailResponse> {
  return apiGet<AdminUserDetailResponse>(`/api/admin/users/${encodeURIComponent(userId)}`);
}
