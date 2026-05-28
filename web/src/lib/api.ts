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
} from "@fundai/api-client";

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

  constructor(message: string, status = 500, detail?: string, requestId?: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.detail = detail;
    this.requestId = requestId;
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
  return "当前会话缺少访问凭证，请先登录。";
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

  const response = await fetch(buildUrl(path), {
    ...init,
    credentials: init.credentials ?? "include",
    headers,
  });

  const responseRequestId = response.headers.get("X-Request-ID") ?? requestId;
  const contentType = response.headers.get("content-type") ?? "";
  const isJSON = contentType.includes("application/json");
  const payload = isJSON ? await response.json().catch(() => null) : await response.text().catch(() => "");

  if (!response.ok) {
    const fallback = typeof payload === "string" && payload ? payload : `请求失败，状态码 ${response.status}`;
    const normalized = normalizeErrorMessage(payload, fallback);
    if (response.status === 401) {
      clearApiToken();
      throw new ApiError("登录状态已失效，请重新登录后再试。", response.status, normalized.detail, responseRequestId);
    }
    throw new ApiError(normalized.message, response.status, normalized.detail, responseRequestId);
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
  expires_at: string;
  request_id?: string;
};

export type SessionResponse = SharedSessionResponse & {
  authenticated: boolean;
  kyc_status?: string;
  kyc_level?: string;
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

async function submitAuth(path: string, body: AuthPayload): Promise<LoginResponse> {
  const response = await fetch(buildUrl(path), {
    method: "POST",
    credentials: "include",
    headers: {
      "Content-Type": "application/json",
      "X-Request-ID": createRequestId(),
    },
    body: JSON.stringify(body),
  });
  const payload = (await response.json().catch(() => null)) as LoginResponse | null;
  if (!response.ok || !payload?.token || !payload.user_id) {
    const fallback = `登录失败，状态码 ${response.status}`;
    const normalized = normalizeErrorMessage(payload, fallback);
    throw new ApiError(normalized.message, response.status, normalized.detail, payload?.request_id);
  }
  return persistLogin(payload);
}

export function loginWithPassword(payload: AuthPayload): Promise<LoginResponse> {
  return submitAuth("/api/auth/login", payload);
}

export function registerWithPassword(payload: AuthPayload): Promise<LoginResponse> {
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
    const fallback = `请求失败，状态码 ${response.status}`;
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
    const fallback = `会话请求失败，状态码 ${response.status}`;
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
}

export interface BacktestResultView {
  initialCash: number;
  finalNav: number;
  navCurve: BacktestNavPoint[];
  trades: BacktestTradeEvent[];
  metrics: BacktestMetricsView;
  completedAt?: string;
  walkForward?: WalkForwardResultView;
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

export async function getBacktest(fundId: string, jobId: string): Promise<BacktestJob> {
  return apiGet<BacktestJob>(`/api/funds/${encodeURIComponent(fundId)}/backtests/${encodeURIComponent(jobId)}`);
}

export async function cancelBacktest(fundId: string, jobId: string): Promise<void> {
  await apiPost<{ cancelled: boolean }>(`/api/funds/${encodeURIComponent(fundId)}/backtests/${encodeURIComponent(jobId)}/cancel`);
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
