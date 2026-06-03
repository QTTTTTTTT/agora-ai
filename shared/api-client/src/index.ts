/**
 * @fundai/api-client — shared HTTP API client for web + Android RN.
 *
 * 设计意图：
 *
 *  1. 单一类型定义点。Endpoint 响应 / 请求体的 TS 类型定义只在这里 — 任何
 *     端（web / android）漂移都通过 npm workspace 直接破构编译，迫使作者
 *     回这里更新签名，杜绝 "web 改了字段名忘记同步 RN" 的产线漂移。
 *
 *  2. fetch 适配。web 用浏览器内建 fetch；RN 也用全局 fetch（RN >= 0.60 自带）。
 *     抹平差异，调用方只需要在 createClient(...) 时注入 baseUrl 和 tokenProvider。
 *
 *  3. 不依赖 react / dom / 任何 UI 库。Pure data layer.
 *
 *  4. 故意不引入 axios — 我们只需要 fetch + JSON, axios 的 interceptors / dispatch
 *     额外配置会让 RN 上的 metro bundler 体积膨胀。
 *
 * 不涵盖：
 *  - admin / wallet / marketplace / KYC / backtest 等"web 重业务" endpoint —
 *    暂时保留在 web/src/lib/api.ts 里。本包只覆盖 5 个核心页（Home / Team /
 *    Decision / Memory / More）真正用到的 endpoint。Android 第二阶段需要再扩。
 */

// ---------------------------------------------------------------------------
// 错误模型
// ---------------------------------------------------------------------------

/**
 * ApiError 是 server-side 4xx/5xx 的统一封装。
 *
 * 字段对齐 server/internal/api 的 errorEnvelope：
 *   code: HTTP status mirror（方便日志聚合）
 *   message: 给用户看的简短描述
 *   detail: 给开发者看的可选细节（"missing field foo"）
 */
export class ApiError extends Error {
  readonly code: number;
  readonly detail: string;
  readonly raw: unknown;

  constructor(code: number, message: string, detail: string, raw?: unknown) {
    super(message);
    this.name = "ApiError";
    this.code = code;
    this.detail = detail;
    this.raw = raw;
  }
}

// ---------------------------------------------------------------------------
// Client factory
// ---------------------------------------------------------------------------

export interface ClientOptions {
  /** API 根，必填。例 "https://fund.example.com" 或 RN dev 时 "http://10.0.2.2:8080". */
  baseUrl: string;
  /**
   * 提供 bearer token 的回调。同步即可 — 大多数实现是 keychain / AsyncStorage 一次性
   * 读出，我们不强制 async；可空表示当前未登录。
   */
  getToken?: () => string | null | undefined;
  /**
   * 401 时被调用 — 通常清掉本地 token 并跳登录页。client 自身不会重试，由调用方
   * 决定是 redirect 还是 silent re-login。
   */
  onUnauthorized?: () => void | Promise<void>;
  /** 自定义 fetch；默认用全局 fetch。RN 0.60+ 内建。 */
  fetchImpl?: typeof fetch;
  /** 每个请求的超时秒数；默认 30s。 */
  timeoutSeconds?: number;
}

interface CallOptions {
  method?: "GET" | "POST" | "PUT" | "DELETE" | "PATCH";
  body?: unknown;
  signal?: AbortSignal;
  /** 跳过 401 hook（auth endpoint 自己处理）。 */
  skipUnauthorizedHook?: boolean;
  /**
   * Extra headers to merge on top of the default Content-Type +
   * Authorization. P0-7 uses this to pipe X-Step-Up-Token through
   * to high-risk endpoints without polluting the function
   * signature of every wrapper. Existing keys win on collision —
   * NEVER let a caller override Authorization (the auth layer is
   * the single source of truth for that).
   */
  headers?: Record<string, string>;
}

export interface ApiClient {
  // 通用
  request<T>(path: string, opts?: CallOptions): Promise<T>;

  // ---- Auth ----
  login(input: LoginInput): Promise<LoginResponse>;
  // loginOutcome wraps `login` and surfaces the 2FA challenge
  // response shape — preferred for new callers. Returning the union
  // here keeps backwards compatibility for old consumers that still
  // call `login()` and expect a flat LoginResponse.
  loginOutcome(input: LoginInput): Promise<LoginOutcome>;
  session(): Promise<SessionResponse>;
  logout(): Promise<void>;
  wechatLogin(input: { code: string }): Promise<LoginResponse>;
  forgotPassword(input: { email: string }): Promise<{ ok: boolean }>;
  resetPassword(input: { token: string; new_password: string }): Promise<{ ok: boolean }>;
  changePassword(input: { current_password: string; new_password: string }): Promise<{ ok: boolean }>;

  // ---- 2FA / TOTP (P0-6) ----
  twoFAStatus(): Promise<TwoFAStatusResponse>;
  twoFASetup(): Promise<TwoFASetupResponse>;
  twoFAVerify(input: { code: string }): Promise<{ enabled: boolean }>;
  twoFADisable(input: { password: string; code?: string; recoveryCode?: string }): Promise<{ disabled: boolean }>;
  twoFAChallenge(input: { challenge: string; code?: string; recoveryCode?: string }): Promise<LoginResponse>;

  // ---- Step-up (P0-7) ----
  // Mints a short-lived step-up token following a fresh biometric
  // assertion on the device. Callers attach the token via
  // X-Step-Up-Token on subsequent high-risk action requests.
  stepUp(input?: { biometricKind?: string }): Promise<StepUpResponse>;

  // ---- Live trading hard gate (P0-9) ----
  // Returns the per-fund readiness picture: which of the four
  // pillars (KYC, broker_link, 2FA, step-up) currently pass.
  // The UI uses this to render a checklist on a fund detail page
  // BEFORE the user attempts a cancel/replace, so a 403 from the
  // mutation endpoint is rare and recoverable.
  //
  // Pass stepUpToken when polling readiness right after a
  // biometric prompt — the server folds it into the StepUpOK
  // pillar so the UI can show "all pillars green" without an
  // extra round-trip.
  liveReadiness(input: { fundId: string; stepUpToken?: string }): Promise<LiveReadinessResponse>;

  // ---- Broker links (P1-6) ----
  //
  // Self-service broker-account binding for the fund owner. New
  // requests start as 'pending' and only become 'active' after a
  // super_admin approves them via the admin UI (4-eye check
  // enforced server-side).
  //
  // Concurrency: the server's partial UNIQUE
  // broker_links_one_active_per_fund_idx guarantees at most one
  // ACTIVE row per fund, so callers can race two requests
  // without corrupting state — the second one will fail with
  // 409.
  listBrokerLinks(fundId: string): Promise<BrokerLinkRow[]>;
  requestBrokerLink(input: {
    fundId: string;
    brokerId: string;
    accountId: string;
    metadata?: Record<string, unknown>;
  }): Promise<{ link_id: string; status: BrokerLinkStatus }>;
  revokeBrokerLink(input: {
    fundId: string;
    linkId: string;
  }): Promise<{ link_id: string; status: BrokerLinkStatus }>;

  // ---- Funding requests (P1-2) ----
  //
  // Deposit / withdrawal flow. Self-service create + cancel for
  // the fund owner; approve/reject are admin-only and live on
  // /api/admin/funding-requests/* (not surfaced through this
  // shared client because the admin UI is web-only and uses
  // web/src/lib/api.ts directly).
  listFundingRequests(input: {
    fundId: string;
    statuses?: FundingStatus[];
    limit?: number;
  }): Promise<FundingRequestRow[]>;
  createFundingRequest(input: {
    fundId: string;
    direction: FundingDirection;
    amount: number;
    method: FundingMethod;
    currency?: string;
    externalReference?: string;
    notes?: string;
  }): Promise<{ id: string; status: FundingStatus }>;
  cancelFundingRequest(input: {
    fundId: string;
    requestId: string;
  }): Promise<{ status: FundingStatus }>;

  // ---- Funds + companies ----
  listCompanies(): Promise<CompanyListResponse>;
  getFund(fundId: string): Promise<FundDetail>;

  // ---- Team ----
  listTeam(fundId: string): Promise<TeamListResponse>;

  // ---- Plans / decisions ----
  listPlans(fundId: string): Promise<PlanListResponse>;
  getPlan(planId: string): Promise<PlanDetail>;
  /** Approve the plan and let it advance to execution. Returns the
   *  refreshed plan with updated status. Surfaces 4xx from the server
   *  (e.g. risk-gate veto, fund auto-execute policy mismatch) as
   *  ApiError so mobile / web can show the reason verbatim. */
  approvePlan(planId: string): Promise<PlanDetail>;
  /** Reject the plan with a free-text reason that becomes part of
   *  the audit log + lesson lineage. Reason should be 1-200 chars; the
   *  server trims and validates. */
  rejectPlan(planId: string, reason: string): Promise<PlanDetail>;
  /** Re-quote every action in the plan against live market data and
   *  return the updated PlanDetail. Used by web's "Refresh quote"
   *  button before approving a stale plan. */
  refreshPlanQuote(planId: string): Promise<PlanDetail>;
  getDecisionTrace(fundId: string, tradingDate: string, planId: string): Promise<DecisionTrace>;

  // ---- Order Cancel / Replace (P0-5) ----

  /** Cancel an open order. Backend rejects terminal orders with 409
   *  (mapped to ApiError.code 409 here). Reason is one of the
   *  canonical short tags ("user_requested", "ttl", "risk_breach",
   *  "system"); anything else is rewritten server-side. */
  cancelOrder(
    fundId: string,
    tradeId: string,
    options?: { reason?: string; note?: string; stepUpToken?: string },
  ): Promise<OrderActionResponse>;

  /** Replace one or more modifiable fields of an open order. At
   *  least one of (quantity, limitPrice, stopPrice, trailAmount,
   *  trailPercent, displayQty) must be set, or the backend returns
   *  400. The note field is captured into the audit metadata only;
   *  it does NOT count as a "field change" so a note-only replace
   *  is rejected. */
  replaceOrder(
    fundId: string,
    tradeId: string,
    payload: ReplaceOrderPayload,
    options?: { stepUpToken?: string },
  ): Promise<OrderActionResponse>;

  // ---- Memory ----
  getMemory(fundId: string, layer?: MemoryLayer): Promise<MemoryListResponse>;
  listReflections(fundId: string, limit?: number): Promise<ReflectionListResponse>;

  // ---- Portfolio / NAV ----
  getPortfolio(fundId: string): Promise<PortfolioSnapshot>;
  getNavHistory(fundId: string, days?: number): Promise<NavHistoryResponse>;

  /** Returns the most recent N trades for the fund. Used by the
   *  Orders screen to surface open / recently-cancelled orders for
   *  cancel / replace flows. Defaults to 200 and clamped to 1000
   *  server-side. */
  listTrades(fundId: string, limit?: number): Promise<TradeListResponse>;

  // ---- Corporate actions ----
  /** Returns the per-fund timeline of split / dividend events that
   *  have been mathematically applied to the fund's holdings. Used
   *  by the holding-detail and fund-overview UIs to surface
   *  "what corp action moved my cost basis on date X?".
   *
   *  Server-side: fund-membership enforced; non-members get 403.
   *  Limit defaults to 50; values > 200 are silently clamped. */
  getCorpActions(fundId: string, limit?: number): Promise<CorpActionListResponse>;

  // ---- Benchmark history ----
  /** Fund NAV + selected market benchmarks rebased to start = 100
   *  over the trailing N days.
   *
   *  Used by the dashboard's "vs market" chart panel.
   *   - Empty / undefined seriesIds lets the server pick defaults
   *     based on the fund's universe (Recommend rules).
   *   - The `available` array on the response is the full catalog
   *     so the UI can populate its picker without an extra round
   *     trip.
   *   - `partialFailures` is non-empty when one or more benchmark
   *     IDs couldn't be fetched; surface a small toast rather than
   *     failing the whole panel. */
  getBenchmarkHistory(
    fundId: string,
    days?: number,
    seriesIds?: string[],
  ): Promise<BenchmarkHistoryResponse>;

  /** Holdings series — per-holding daily price line, normalized to
   *  start = 100 over the trailing N days. Used by the dashboard's
   *  per-holding mini-chart grid (P1-2). Same nil-safety contract
   *  as benchmarks (server returns 503 when ohlc is disabled). */
  getHoldingsSeries(
    fundId: string,
    days?: number,
  ): Promise<HoldingsSeriesResponse>;

  // ---- A/B shadow comparison (Card D) ----
  /** Returns the per-variant shadow-agent learning timeline (lessons,
   *  adjustments, summaries, proposed evolution-config diff) for the
   *  given AB test. Surfaces what *another* strategy's agents
   *  thought during the shadow run, so the comparison page can show
   *  "what did B's agents learn vs A's?" without forcing a dry-run
   *  promotion first.
   *
   *  Server-side: caller must be a member of BOTH the control and
   *  treatment funds (matches the rest of the AB endpoints).
   *  Variants always returns 2 elements (A, B); empty side uses
   *  agents = []. */
  getABShadowAgents(testId: string): Promise<ABTestShadowAgentResponse>;

  /** Returns the per-symbol A vs B operational attribution table
   *  (PnL, turnover, gap) for the given AB test. Bounded to the top
   *  50 rows by |gap| server-side; tie-breaks alphabetically.
   *
   *  Same dual-fund auth contract as getABShadowAgents. */
  getABOperationalAttribution(testId: string): Promise<ABTestOperationalAttribution>;
}

export function createClient(options: ClientOptions): ApiClient {
  const baseUrl = options.baseUrl.replace(/\/$/, "");
  const fetchImpl = options.fetchImpl ?? (globalThis.fetch as typeof fetch);
  const timeoutMs = (options.timeoutSeconds ?? 30) * 1000;

  async function request<T>(path: string, opts: CallOptions = {}): Promise<T> {
    const url = path.startsWith("http") ? path : `${baseUrl}${path}`;
    const token = options.getToken?.();

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeoutMs);
    const signal = opts.signal ?? controller.signal;

    try {
      const resp = await fetchImpl(url, {
        method: opts.method ?? "GET",
        headers: {
          "Content-Type": "application/json",
          // Caller-supplied headers go BEFORE auth so we cannot
          // be tricked into overriding our own Authorization
          // header (the spread order is important).
          ...(opts.headers ?? {}),
          ...(token ? { Authorization: `Bearer ${token}` } : {}),
        },
        body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
        signal,
      });
      const text = await resp.text();
      const payload = text ? safeJsonParse(text) : null;
      if (!resp.ok) {
        if (resp.status === 401 && !opts.skipUnauthorizedHook) {
          await options.onUnauthorized?.();
        }
        const envelope = (payload ?? {}) as { code?: number; message?: string; detail?: string };
        throw new ApiError(
          resp.status,
          envelope.message ?? `HTTP ${resp.status}`,
          envelope.detail ?? "",
          payload,
        );
      }
      return payload as T;
    } finally {
      clearTimeout(timer);
    }
  }

  return {
    request,

    // Auth
    login: (input) =>
      request<LoginResponse>("/api/auth/login", { method: "POST", body: input, skipUnauthorizedHook: true }),
    // loginOutcome surfaces the discriminated union (session vs.
    // 2FA challenge). Implemented as a wrapper around the same
    // endpoint so legacy `login()` callers don't break — they
    // simply see the challenge response throw "missing token" via
    // their existing error path.
    loginOutcome: async (input) => {
      // We deliberately bypass the typed `login` helper here so we
      // can inspect the raw payload before TS narrows it to
      // LoginResponse. The server returns one of two shapes
      // depending on whether 2FA is enabled.
      const raw = await request<LoginResponse | TwoFAChallengeResponse>(
        "/api/auth/login",
        { method: "POST", body: input, skipUnauthorizedHook: true },
      );
      if ((raw as TwoFAChallengeResponse).requires_2fa) {
        const ch = raw as TwoFAChallengeResponse;
        return { kind: "challenge", challenge: ch.challenge, expiresAt: ch.expires_at };
      }
      return { kind: "session", payload: raw as LoginResponse };
    },
    session: () => request<SessionResponse>("/api/auth/session"),
    logout: async () => {
      await request<void>("/api/auth/logout", { method: "POST", body: {} });
    },
    wechatLogin: (input) =>
      request<LoginResponse>("/api/auth/wechat-login", { method: "POST", body: input, skipUnauthorizedHook: true }),
    forgotPassword: (input) =>
      request("/api/auth/forgot-password", { method: "POST", body: input, skipUnauthorizedHook: true }),
    resetPassword: (input) =>
      request("/api/auth/reset-password", { method: "POST", body: input, skipUnauthorizedHook: true }),
    changePassword: (input) =>
      request("/api/auth/change-password", { method: "POST", body: input }),

    // ---- 2FA / TOTP ----
    twoFAStatus: () => request<TwoFAStatusResponse>("/api/auth/2fa/status"),
    twoFASetup: () => request<TwoFASetupResponse>("/api/auth/2fa/setup", { method: "POST", body: {} }),
    twoFAVerify: (input) =>
      request<{ enabled: boolean }>("/api/auth/2fa/verify", { method: "POST", body: input }),
    twoFADisable: (input) =>
      request<{ disabled: boolean }>("/api/auth/2fa/disable", { method: "POST", body: input }),
    twoFAChallenge: (input) =>
      // skipUnauthorizedHook so a wrong code doesn't drop the user
      // out of the half-completed login flow. The handler still
      // surfaces 401, but we leave the on-401 logout path to the
      // caller's UI.
      request<LoginResponse>("/api/auth/2fa/challenge", { method: "POST", body: input, skipUnauthorizedHook: true }),

    // Companies + funds
    listCompanies: () => request<CompanyListResponse>("/api/companies"),
    getFund: (fundId) => request<FundDetail>(`/api/funds/${encodeURIComponent(fundId)}`),

    // Team
    listTeam: (fundId) => request<TeamListResponse>(`/api/funds/${encodeURIComponent(fundId)}/team`),

    // Plans
    listPlans: (fundId) => request<PlanListResponse>(`/api/funds/${encodeURIComponent(fundId)}/plans`),
    getPlan: (planId) => request<PlanDetail>(`/api/plans/${encodeURIComponent(planId)}`),
    approvePlan: (planId) =>
      request<PlanDetail>(`/api/plans/${encodeURIComponent(planId)}/approve`, { method: "POST", body: {} }),
    rejectPlan: (planId, reason) =>
      request<PlanDetail>(`/api/plans/${encodeURIComponent(planId)}/reject`, {
        method: "POST",
        body: { reason },
      }),
    refreshPlanQuote: (planId) =>
      request<PlanDetail>(`/api/plans/${encodeURIComponent(planId)}/refresh-quote`, { method: "POST", body: {} }),
    getDecisionTrace: (fundId, tradingDate, planId) =>
      request<DecisionTrace>(
        `/api/funds/${encodeURIComponent(fundId)}/decision-trace?tradingDate=${encodeURIComponent(tradingDate)}&planId=${encodeURIComponent(planId)}`,
      ),

    cancelOrder: async (fundId, tradeId, options) => {
      const body: Record<string, string> = {};
      if (options?.reason) body.reason = options.reason;
      if (options?.note) body.note = options.note;
      const resp = await request<{ order: OrderActionResponse }>(
        `/api/funds/${encodeURIComponent(fundId)}/orders/${encodeURIComponent(tradeId)}/cancel`,
        {
          method: "POST",
          body: Object.keys(body).length > 0 ? body : {},
          headers: options?.stepUpToken ? { "X-Step-Up-Token": options.stepUpToken } : undefined,
        },
      );
      return resp.order;
    },

    replaceOrder: async (fundId, tradeId, payload, options) => {
      const resp = await request<{ order: OrderActionResponse }>(
        `/api/funds/${encodeURIComponent(fundId)}/orders/${encodeURIComponent(tradeId)}/replace`,
        {
          method: "POST",
          body: payload,
          headers: options?.stepUpToken ? { "X-Step-Up-Token": options.stepUpToken } : undefined,
        },
      );
      return resp.order;
    },

    stepUp: (input) =>
      // Body is intentionally optional — the server tolerates a
      // missing/empty body. The biometricKind hint is a future
      // hook for audit reporting.
      request<StepUpResponse>("/api/auth/step-up", {
        method: "POST",
        body: input ?? {},
      }),

    liveReadiness: ({ fundId, stepUpToken }) => {
      // Honour the step-up token if the caller passed one
      // (typically right after a fresh biometric prompt). Use
      // the query param rather than the header so the request
      // works through caches that strip non-standard headers.
      const qs = stepUpToken
        ? `?step_up_token=${encodeURIComponent(stepUpToken)}`
        : "";
      return request<LiveReadinessResponse>(
        `/api/funds/${encodeURIComponent(fundId)}/live-readiness${qs}`,
      );
    },

    listBrokerLinks: async (fundId) => {
      const resp = await request<{ links: BrokerLinkRow[] }>(
        `/api/funds/${encodeURIComponent(fundId)}/broker-links`,
      );
      return resp.links ?? [];
    },
    requestBrokerLink: ({ fundId, brokerId, accountId, metadata }) =>
      request<{ link_id: string; status: BrokerLinkStatus }>(
        `/api/funds/${encodeURIComponent(fundId)}/broker-links`,
        {
          method: "POST",
          body: { brokerId, accountId, metadata },
        },
      ),
    revokeBrokerLink: ({ fundId, linkId }) =>
      request<{ link_id: string; status: BrokerLinkStatus }>(
        `/api/funds/${encodeURIComponent(fundId)}/broker-links/${encodeURIComponent(linkId)}/revoke`,
        { method: "POST", body: {} },
      ),

    // ---- Funding requests (P1-2) ----
    listFundingRequests: async ({ fundId, statuses, limit }) => {
      const params = new URLSearchParams();
      if (statuses && statuses.length > 0) {
        for (const s of statuses) params.append("status", s);
      }
      if (limit) params.set("limit", String(limit));
      const qs = params.toString();
      const resp = await request<{ requests: FundingRequestRow[] }>(
        `/api/funds/${encodeURIComponent(fundId)}/funding-requests${qs ? `?${qs}` : ""}`,
      );
      return resp.requests;
    },
    createFundingRequest: ({ fundId, ...body }) =>
      request<{ id: string; status: FundingStatus }>(
        `/api/funds/${encodeURIComponent(fundId)}/funding-requests`,
        { method: "POST", body },
      ),
    cancelFundingRequest: ({ fundId, requestId }) =>
      request<{ status: FundingStatus }>(
        `/api/funds/${encodeURIComponent(fundId)}/funding-requests/${encodeURIComponent(requestId)}/cancel`,
        { method: "POST", body: {} },
      ),

    // Memory
    getMemory: (fundId, layer) =>
      request<MemoryListResponse>(
        `/api/funds/${encodeURIComponent(fundId)}/memory${layer ? `?layer=${encodeURIComponent(layer)}` : ""}`,
      ),
    listReflections: (fundId, limit) =>
      request<ReflectionListResponse>(
        `/api/funds/${encodeURIComponent(fundId)}/reflections${limit ? `?limit=${limit}` : ""}`,
      ),

    // Portfolio
    getPortfolio: (fundId) =>
      request<PortfolioSnapshot>(`/api/funds/${encodeURIComponent(fundId)}/portfolio`),
    getNavHistory: (fundId, days) =>
      request<NavHistoryResponse>(
        `/api/funds/${encodeURIComponent(fundId)}/nav${days ? `?days=${days}` : ""}`,
      ),

    listTrades: async (fundId, limit) => {
      // The server returns a bare array; we wrap it in a typed
      // envelope so future pagination metadata (page tokens, total)
      // can be added without churning the consumer.
      const trades = await request<TradeRecord[]>(
        `/api/funds/${encodeURIComponent(fundId)}/trades?limit=${limit ?? 200}`,
      );
      return { trades: Array.isArray(trades) ? trades : [] };
    },

    // Corporate actions — per-fund timeline of split / dividend
    // events that have been applied to holdings. Read-only, with
    // fund-membership enforced server-side. Default limit is 50;
    // server caps any non-numeric / out-of-range value at 200.
    getCorpActions: (fundId, limit) =>
      request<CorpActionListResponse>(
        `/api/funds/${encodeURIComponent(fundId)}/corp-actions${limit ? `?limit=${limit}` : ""}`,
      ),

    // Benchmark history — fund NAV + market benchmarks normalized
    // to start = 100. seriesIds is comma-joined when provided so
    // the server can ignore unknown ids and surface them in
    // partialFailures, rather than 400.
    getBenchmarkHistory: (fundId, days, seriesIds) => {
      const params: string[] = [];
      if (typeof days === "number" && Number.isFinite(days)) {
        params.push(`days=${Math.trunc(days)}`);
      }
      if (Array.isArray(seriesIds) && seriesIds.length > 0) {
        params.push(`series=${encodeURIComponent(seriesIds.join(","))}`);
      }
      const qs = params.length > 0 ? `?${params.join("&")}` : "";
      return request<BenchmarkHistoryResponse>(
        `/api/funds/${encodeURIComponent(fundId)}/benchmark-history${qs}`,
      );
    },

    // Holdings series — per-holding daily price line, normalized.
    // Single days param; same soft-clamp / nil-safety as
    // benchmark history.
    getHoldingsSeries: (fundId, days) => {
      const qs = typeof days === "number" && Number.isFinite(days)
        ? `?days=${Math.trunc(days)}`
        : "";
      return request<HoldingsSeriesResponse>(
        `/api/funds/${encodeURIComponent(fundId)}/holdings/series${qs}`,
      );
    },

    // A/B shadow agents — read-only snapshot of what each
    // variant's agents learned during shadow execution. The
    // request takes no query string today; server caps the
    // memory list at 20 per agent and lessons/adjustments at
    // 12 per agent so the response stays bounded.
    getABShadowAgents: (testId) =>
      request<ABTestShadowAgentResponse>(
        `/api/abtests/${encodeURIComponent(testId)}/shadow-agents`,
      ),

    // A/B operational attribution — per-symbol PnL gap A vs B.
    // No query params; rows are pre-sorted by |gap| desc and
    // capped at 50 server-side.
    getABOperationalAttribution: (testId) =>
      request<ABTestOperationalAttribution>(
        `/api/abtests/${encodeURIComponent(testId)}/operational-attribution`,
      ),
  };
}

function safeJsonParse(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------------------
// Domain types (subset shared between web + android core flows)
// ---------------------------------------------------------------------------

export interface LoginInput {
  email: string;
  password: string;
}

export interface LoginResponse {
  token: string;
  user_id: string;
  email?: string;
  display_name?: string;
  role?: string;
}

// TwoFAChallengeResponse is what /api/auth/login returns when the
// user has 2FA enabled — instead of a session token we get a
// short-lived challenge that has to be exchanged for a session via
// /api/auth/2fa/challenge. Lives at the top level so consumers can
// import it without reaching into nested paths.
export interface TwoFAChallengeResponse {
  requires_2fa: true;
  challenge: string;
  expires_at: string;
}

// LoginOutcome is the discriminated union the platform's login
// helpers return. The `kind` field tells the caller which branch
// they're on; TS will refuse to compile a missing case.
export type LoginOutcome =
  | { kind: "session"; payload: LoginResponse }
  | { kind: "challenge"; challenge: string; expiresAt: string };

// 2FA enrolment payloads — matched 1:1 to the server's
// totp_handler.go responses.
export interface TwoFAStatusResponse {
  enabled: boolean;
  enrolmentPending?: boolean;
  lastVerifiedAt?: string;
  lastUsedRecoveryAt?: string;
}

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

// StepUpResponse is what /api/auth/step-up returns. Token is the
// JWT to attach via X-Step-Up-Token; ttl_seconds + expires_at let
// the client cache the token without round-tripping back here for
// every action. P0-7.
export interface StepUpResponse {
  token: string;
  expires_at: string;
  ttl_seconds: number;
}

// LiveReadinessResponse mirrors GET /api/funds/{fundId}/live-readiness.
// Trading mode and per-pillar bools come straight from the
// backend's LiveReadiness struct; first_failing names the first
// pillar the user must complete (in the natural KYC →
// broker_link → 2FA → step-up order). gate_enforced=false means
// either the fund isn't 'live' OR LIVE_TRADING_GATE_ENABLED is
// off in the deployment — UI should NOT block the user in that
// case, but MAY surface a soft "would-be" warning. P0-9.
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

// Broker-link wire types (P1-6). Mirrors the projection in
// server/cmd/server/broker_link_handler.go — note that
// `accountId` is ALREADY redacted server-side, so a client
// printing it directly leaks no PII.
export type BrokerLinkStatus = "pending" | "active" | "suspended" | "revoked";

export interface BrokerLinkRow {
  id: string;
  fundId: string;
  userId: string;
  brokerId: string;
  accountId: string; // redacted (e.g. "••••4567")
  status: BrokerLinkStatus;
  approvedBy?: string;
  approvedAt?: string;
  createdAt: string;
  updatedAt: string;
}

// ---------------------------------------------------------------------------
// Funding requests (P1-2)
// ---------------------------------------------------------------------------
//
// Deposit / withdrawal queue. Every state-changing operation
// (create, cancel, approve, reject) is hash-chained into the
// audit log. UI renders the status badge with these constants —
// keeping them as a TS union means a typo at the call site fails
// the type check.

export type FundingDirection = "deposit" | "withdrawal";

export type FundingStatus =
  | "pending"
  | "approved"
  | "rejected"
  | "cancelled"
  | "posted";

export type FundingMethod =
  | "wire"
  | "ach"
  | "sepa"
  | "check"
  | "internal_transfer"
  | "manual";

export interface FundingRequestRow {
  id: string;
  fundId: string;
  direction: FundingDirection;
  amount: number;
  currency: string;
  method: FundingMethod;
  externalReference?: string;
  status: FundingStatus;
  requestedBy: string;
  approvedBy?: string;
  approvedAt?: string;
  rejectedBy?: string;
  rejectedAt?: string;
  rejectionReason?: string;
  cancelledAt?: string;
  cashLedgerEntryId?: string;
  notes?: string;
  createdAt: string;
  updatedAt: string;
}

// FXRateRow is the on-wire shape returned by GET /api/admin/fx-rates
// (P1-4). One row per (base, quote, rate_at, source) tuple.
//
// Mirrors fxAdminWire on the server side. Note the on-wire snake_case
// for rate_at to match the rest of the admin surface; everything else
// stays camelCase for parity with positions/orders.
export interface FXRateRow {
  base: string;
  quote: string;
  rate: number;
  rate_at: string;
  source: string;
}

// Closed vocabulary the server's funds.base_currency CHECK accepts.
// Kept here so fund-settings UIs can render a <select> without an
// extra round-trip. If the server's allowlist grows, bump this.
export const SUPPORTED_BASE_CURRENCIES = [
  "USD",
  "CNY",
  "HKD",
  "EUR",
  "JPY",
  "GBP",
  "SGD",
] as const;

export type SupportedBaseCurrency = (typeof SUPPORTED_BASE_CURRENCIES)[number];

// ReconciliationRun is the on-wire shape returned by
// GET /api/admin/reconciliation/runs (P1-3). One row per executed
// diff. break_count_* fields let the UI render a roll-up without
// pulling the full break list.
export interface ReconciliationRun {
  id: string;
  fund_id: string;
  statement_id: string;
  run_date: string; // YYYY-MM-DD
  triggered_by?: string;
  trigger_source: "manual" | "scheduled" | "replay";
  status: "pending" | "completed" | "failed";
  break_count_total: number;
  break_count_critical: number;
  break_count_warning: number;
  break_count_info: number;
  summary?: Record<string, unknown>;
  started_at: string; // RFC3339
  completed_at?: string;
  error_message?: string;
}

// ReconciliationBreak is the on-wire shape for one diff break.
// internal_value / broker_value / diff_value are nullable because
// some break types only have one side (e.g. position_missing_internal
// has broker_value but no internal_value), and diff_percent is null
// when the divisor would be zero.
export interface ReconciliationBreak {
  id: string;
  run_id: string;
  fund_id: string;
  break_type: ReconciliationBreakType;
  severity: "info" | "warning" | "critical";
  symbol?: string;
  currency?: string;
  internal_value?: number | null;
  broker_value?: number | null;
  diff_value?: number | null;
  diff_percent?: number | null;
  description?: string;
  metadata?: Record<string, unknown>;
  status: "open" | "acknowledged" | "resolved" | "ignored";
  resolution_note?: string;
  resolved_by?: string;
  resolved_at?: string;
  created_at: string;
}

// ReconciliationBreakType is the closed vocabulary the engine
// emits. Matches `recon.BreakType` on the server side.
export type ReconciliationBreakType =
  | "position_quantity_mismatch"
  | "position_avg_cost_mismatch"
  | "position_missing_internal"
  | "position_missing_broker"
  | "cash_balance_mismatch"
  | "cash_currency_missing_internal"
  | "cash_currency_missing_broker"
  | "trade_missing_internal"
  | "trade_missing_broker"
  | "trade_quantity_mismatch"
  | "trade_price_mismatch"
  | "trade_side_mismatch";

// SurveillanceEvent is the on-wire shape returned by
// GET /api/admin/surveillance/events (P1-7). One row per pattern
// detection. trade_ids are the contributing trade_executions ids
// (same shape as the audit chain references). metadata is
// rule-specific — the UI renders the keys it knows and falls
// back to a JSON dump otherwise.
export interface SurveillanceEvent {
  id: string;
  fund_id: string;
  rule_code: SurveillanceRuleCode;
  severity: "info" | "warning" | "critical";
  symbol?: string;
  instrument_key?: string;
  window_start: string;
  window_end: string;
  trade_ids: string[];
  summary: string;
  metadata?: Record<string, unknown>;
  status: SurveillanceEventStatus;
  review_note?: string;
  reviewed_by?: string;
  reviewed_at?: string;
  detected_at: string;
  detector_version?: string;
  fingerprint: string;
}

// SurveillanceEventStatus is the lifecycle vocabulary; matches
// the CHECK on `surveillance_events.status`.
export type SurveillanceEventStatus =
  | "open"
  | "reviewing"
  | "cleared"
  | "escalated";

// SurveillanceRuleCode is the closed rule vocabulary. Adding a
// new rule requires updating BOTH this union AND the
// surveillance_events_rule_chk on the DB; mismatch means the
// frontend will render an unknown rule with the raw string.
export type SurveillanceRuleCode =
  | "wash_trade"
  | "marking_close"
  | "self_trade_pair"
  | "rapid_fire_reversal"
  | "layering_suspect";

// SurveillanceRun is the bookkeeping row from
// GET /api/admin/surveillance/runs. One per scan invocation;
// event_count_* fields let the UI render a roll-up without
// pulling the full events list.
export interface SurveillanceRun {
  id: string;
  fund_id?: string;
  triggered_by?: string;
  trigger_source: "manual" | "scheduled" | "replay";
  window_start: string;
  window_end: string;
  trade_count: number;
  event_count_total: number;
  event_count_critical: number;
  event_count_warning: number;
  event_count_info: number;
  duration_ms: number;
  status: "pending" | "completed" | "failed";
  error_message?: string;
  summary?: Record<string, unknown>;
  started_at: string;
  completed_at?: string;
}

// DrawdownAction is the closed action vocabulary the engine emits.
// Matches `drawdown.Action` on the server side.
export type DrawdownAction =
  | "trim_proportional"
  | "flatten"
  | "defensive_only";

// DrawdownEventStatus mirrors `drawdown.Status`.
export type DrawdownEventStatus =
  | "proposed"
  | "approved"
  | "executed"
  | "dismissed"
  | "superseded";

// DrawdownTier is one row of `drawdown_policies` (P3-5).
// dd_pct is a NEGATIVE fraction, e.g. -0.05 for "fire when DD
// reaches -5%".
export interface DrawdownTier {
  tier: number;
  dd_pct: number;
  action: DrawdownAction;
  trim_ratio: number;
  cooldown_hours: number;
  auto_execute: boolean;
  note?: string;
}

// DrawdownPolicy is the per-fund tier list.
export interface DrawdownPolicy {
  fund_id: string;
  tiers: DrawdownTier[];
}

// DrawdownTrimPlanItem mirrors `drawdown.TrimPlanItem`. Always
// side="sell" today; defensive_only emits an empty plan.
export interface DrawdownTrimPlanItem {
  symbol: string;
  instrument_key?: string;
  side: "sell";
  quantity: number;
  reason: string;
}

// DrawdownEvent is the on-wire shape returned by
// GET /api/admin/drawdown/events. One row per detected breach.
export interface DrawdownEvent {
  id: string;
  fund_id: string;
  tier: number;
  current_dd_pct: number;
  peak_nav: number;
  current_nav: number;
  action: DrawdownAction;
  trim_plan: DrawdownTrimPlanItem[];
  trade_ids?: string[];
  status: DrawdownEventStatus;
  review_note?: string;
  reviewed_by?: string;
  reviewed_at?: string;
  nav_snapshot_id?: string;
  detected_at: string;
  detector_version?: string;
  metadata?: Record<string, unknown>;
  created_at: string;
}

// DrawdownStatus is the live preview returned by
// GET /api/admin/drawdown/funds/{fundId}/status. Combines the
// current peak/NAV/DD with the configured tiers and an optional
// "what would the engine fire right now" preview.
export interface DrawdownStatus {
  fund_id: string;
  peak_nav: number;
  current_nav: number;
  current_dd_pct: number;
  has_policy: boolean;
  tiers: DrawdownTier[];
  breached_tier?: number;
  breached_action?: DrawdownAction;
  would_emit?: DrawdownEvent;
}

// MarketStatusInstrumentState is the closed status vocabulary
// for `instrument_market_status.status` (S6.1).
export type MarketStatusInstrumentState =
  | "trading"
  | "halted"
  | "suspended";

// MarketStatusRuleCode mirrors `marketstatus.RuleCode`.
export type MarketStatusRuleCode =
  | "halted"
  | "suspended"
  | "price_limit"
  | "stale_quote"
  | "market_closed"
  | "half_day_closed";

// MarketStatusDecision mirrors `marketstatus.Decision`.
export type MarketStatusDecision = "allow" | "warn" | "reject";

// MarketStatusInstrument is one row of `instrument_market_status`.
export interface MarketStatusInstrument {
  instrument_key: string;
  symbol: string;
  market: string;
  status: MarketStatusInstrumentState;
  halt_reason?: string;
  halt_started_at?: string;
  halt_until?: string;
  lower_limit?: number;
  upper_limit?: number;
  last_quote_at?: string;
  last_quote_price?: number;
  asset_class: string;
  staleness_budget_seconds?: number;
  note?: string;
  updated_at: string;
}

// MarketStatusCalendarDay is one row of `trading_calendar`.
export interface MarketStatusCalendarDay {
  market: string;
  trading_date: string;
  is_open: boolean;
  open_local: string;
  close_local: string;
  market_tz: string;
  half_day: boolean;
  note?: string;
}

// MarketStatusEvent is one row of `marketstatus_events` —
// the audit trail of every reject / warn the gate emitted.
export interface MarketStatusEvent {
  id: string;
  fund_id?: string;
  instrument_key: string;
  symbol?: string;
  decision: MarketStatusDecision;
  rule_code: MarketStatusRuleCode;
  summary?: string;
  metadata?: Record<string, unknown>;
  client_order_id?: string;
  detected_at: string;
}

// MarketImpactCalibrationSource mirrors `instrument_liquidity.calibration_source`.
export type MarketImpactCalibrationSource =
  | "manual"
  | "historical"
  | "broker_reported";

// MarketImpactInstrument is one row of `instrument_liquidity`
// (S6.2). All adv/volatility fields are optional because a
// partially-calibrated row is valid — the engine fills missing
// fields from asset-class defaults.
export interface MarketImpactInstrument {
  instrument_key: string;
  symbol: string;
  market: string;
  asset_class: string;
  adv_shares?: number;
  adv_notional?: number;
  adv_window_days: number;
  daily_volatility?: number;
  impact_coefficient: number;
  impact_exponent: number;
  min_slippage_bps: number;
  max_slippage_bps: number;
  last_calibrated_at?: string;
  calibration_source: MarketImpactCalibrationSource;
  note?: string;
  updated_at: string;
}

// MarketImpactEstimate mirrors `marketimpact.Estimate`. UI
// surfaces it on the preview panel and on per-fill rows.
export interface MarketImpactEstimate {
  adverse_bps: number;
  temp_impact_bps: number;
  perm_impact_bps?: number;
  used_defaults: boolean;
  used_adv_fallback: boolean;
  reason?: string;
  detector_version?: string;
  applied_at?: string;
}

// MarketImpactPreviewResponse is what POST /preview returns.
export interface MarketImpactPreviewResponse {
  estimate: MarketImpactEstimate;
  reference_px: number;
  implied_fill: number;
  notional: number;
  impact_cost: number;
  impact_cost_pct: number;
}

// MarketImpactCacheStats is what GET /cache returns.
export interface MarketImpactCacheStats {
  size: number;
  last_refresh?: string;
}

// LockupReason mirrors `lockup.LockupReason`.
export type LockupReason =
  | "ipo"
  | "private_placement"
  | "rsu"
  | "restricted"
  | "employee_grant"
  | "block_sale"
  | "other";

// LockupStatus is the derived state the admin handler attaches
// for UI filtering — saves the frontend from re-implementing
// the active/expired/released classification.
export type LockupStatus = "active" | "expired" | "released";

// ----- Securities-borrow / locate (S6.4) -----

// BorrowAvailability mirrors the SQL CHECK enum.
export type BorrowAvailability = "easy" | "hard" | "restricted" | "unavailable";

// BorrowCalibrationSource matches the source enum.
export type BorrowCalibrationSource =
  | "manual"
  | "broker_quote"
  | "agent_lender"
  | "historical_calibration"
  | "public_feed";

// BorrowRate is one row of security_borrow_rates.
export interface BorrowRate {
  instrument_key: string;
  symbol: string;
  market: string;
  asset_class: string;
  borrow_rate_bps_annual: number;
  locate_fee_bps: number;
  availability: BorrowAvailability;
  available_shares?: number;
  min_locate_qty?: number;
  max_locate_qty?: number;
  source: BorrowCalibrationSource;
  last_calibrated_at: string;
  note?: string;
  updated_at: string;
}

// BorrowLocateDecisionKind matches the closed verdict enum.
export type BorrowLocateDecisionKind =
  | "allow"
  | "reject_unavailable"
  | "reject_insufficient"
  | "reject_below_min"
  | "reject_above_max"
  | "no_calibration"
  | "fail_open";

// BorrowLocatePreviewResponse is what /locate/preview returns.
export interface BorrowLocatePreviewResponse {
  decision: BorrowLocateDecisionKind;
  allowed: boolean;
  requested_qty: number;
  intended_price: number;
  notional: number;
  borrow_rate_bps: number;
  locate_fee_bps: number;
  locate_fee_amount: number;
  available_shares?: number;
  reason: string;
  source: BorrowCalibrationSource;
}

// BorrowLocateEvent is one audit-log row.
export interface BorrowLocateEvent {
  id: string;
  fund_id: string;
  instrument_key: string;
  symbol: string;
  requested_qty: number;
  decision: BorrowLocateDecisionKind;
  rate_bps_annual?: number;
  locate_fee_bps?: number;
  locate_fee_amount?: number;
  intended_price?: number;
  notional?: number;
  reason?: string;
  client_order_id?: string;
  created_at: string;
}

// BorrowLedgerEntry is one daily fee row.
export interface BorrowLedgerEntry {
  id: string;
  fund_id: string;
  instrument_key: string;
  symbol: string;
  accrual_date: string;
  short_qty: number;
  market_price: number;
  notional: number;
  rate_bps_annual: number;
  day_count_basis: number;
  fee_amount: number;
  cash_ledger_entry_id?: string;
  created_at: string;
}

// BorrowCacheStats is what GET /cache returns.
export interface BorrowCacheStats {
  size: number;
  last_refresh?: string;
}

// ----- WebSocket real-time market data (S6.5) -----

// WSFeedState mirrors the closed enum from internal/wsfeed.
// Order matches StateUnknown → StateClosed in the Go source.
export type WSFeedState =
  | "unknown"
  | "connecting"
  | "connected"
  | "reconnecting"
  | "backoff"
  | "disconnected"
  | "closed";

// WSFeedStatus is the GET /api/admin/wsfeed/status response.
// Used by the dashboard cards.
export interface WSFeedStatus {
  enabled: boolean;
  reason?: string; // present when enabled=false
  healthy_providers: number;
  total_providers: number;
  subscriptions: number;
  cache_symbols: number;
  dropped_events: number;
  total_ticks: number;
}

// WSFeedConnection is one row from
// GET /api/admin/wsfeed/connections.
export interface WSFeedConnection {
  provider: string;
  state: WSFeedState;
  connected_at?: string;
  disconnected_at?: string;
  last_tick_at?: string;
  tick_count: number;
  reconnect_count: number;
  last_error?: string;
  subscriptions: number;
}

// WSFeedSubscription is one row from
// GET /api/admin/wsfeed/subscriptions.
export interface WSFeedSubscription {
  symbol: string;
  market?: string;
  consumers: number;
  last_tick_at?: string;
}

// WSFeedCacheSnapshot is one row from
// GET /api/admin/wsfeed/cache (and the body returned by
// GET /api/admin/wsfeed/cache/{symbol}, less the surrounding
// envelope).
export interface WSFeedCacheSnapshot {
  symbol: string;
  display?: string;
  market?: string;
  provider?: string;
  last: number;
  bid: number;
  ask: number;
  volume: number;
  as_of?: string;
  received_at?: string;
  update_kind?: string;
  stale?: boolean;
}

// WSFeedCacheListResponse is the full body of
// GET /api/admin/wsfeed/cache. Stats live alongside the rows
// so the UI doesn't need a second call to populate the
// hit/miss/stale counters.
export interface WSFeedCacheListResponse {
  snapshots: WSFeedCacheSnapshot[];
  stats: {
    symbols: number;
    hits: number;
    misses: number;
    stales: number;
    evicts: number;
  };
}

// LockupRecord is one row of `position_lockups` (S6.3).
export interface LockupRecord {
  id: string;
  fund_id: string;
  instrument_key: string;
  symbol: string;
  locked_qty: number;
  locked_until: string;
  reason: LockupReason;
  source_lot_id?: string;
  note?: string;
  released_at?: string;
  released_reason?: string;
  released_by?: string;
  created_by?: string;
  created_at: string;
  updated_at: string;
  status: LockupStatus;
}

// ---------------------------------------------------------------------------
// S7 / P3-1 — Factor exposure
// ---------------------------------------------------------------------------

// Factor enumerates the six canonical factor names the backend
// understands. The string values match `internal/factorexposure.Factor`
// and the CHECK constraint on `instrument_factor_loadings.factor`.
export type Factor =
  | "size"
  | "value"
  | "momentum"
  | "quality"
  | "lowvol"
  | "market_beta";

// All six factors in the canonical render order used by both the
// engine and the admin UI. Exported as a tuple so callers can
// switch on it without leaking magic strings.
export const ALL_FACTORS: readonly Factor[] = [
  "size",
  "value",
  "momentum",
  "quality",
  "lowvol",
  "market_beta",
] as const;

// LoadingSource enumerates the upstream that wrote a loading row;
// mirrors the CHECK constraint and the backend `LoadingSource`.
export type LoadingSource =
  | "manual"
  | "eastmoney"
  | "msci"
  | "computed"
  | "override";

// InstrumentFactorLoading is one row of `instrument_factor_loadings`.
// asof is the calibration vintage (YYYY-MM-DD); updated_at the
// write timestamp in RFC3339Nano UTC.
export interface InstrumentFactorLoading {
  instrument_key: string;
  factor: Factor;
  asof: string;
  loading: number;
  source: LoadingSource;
  note?: string;
  updated_at: string;
}

// FactorExposureRow is one row of a portfolio-level snapshot.
// loadings_asof is the most-recent calibration date among the
// loadings that contributed; absent when no holding had a
// loading for this factor.
export interface FactorExposureRow {
  factor: Factor;
  net_exposure: number;
  gross_exposure: number;
  capital_pct: number;
  holding_count: number;
  loadings_asof?: string;
}

// FactorExposureSnapshot is the live read response. NAV is the
// gross MV (sum of |market_value|). holdings_total / _covered
// surface "this read covered X of Y holdings"; the UI shows a
// coverage warning when covered < total.
export interface FactorExposureSnapshot {
  fund_id: string;
  generated_at: string;
  nav: number;
  holdings_total: number;
  holdings_covered: number;
  oldest_loading_asof?: string;
  exposures: FactorExposureRow[];
}

// FactorExposureTrendPoint is one historical snapshot row used by
// the trend chart. Same shape as a FactorExposureRow but with the
// calculation timestamp prepended.
export interface FactorExposureTrendPoint {
  calculated_at: string;
  factor: Factor;
  net_exposure: number;
  gross_exposure: number;
  capital_pct: number;
  holding_count: number;
  loadings_asof: string;
}

// =====================================================================
// S7 / P3-2 — Value-at-Risk + Conditional VaR (Expected Shortfall).
// =====================================================================

// VaRMethod mirrors the CHECK constraint on
// portfolio_var_snapshots.method. monte_carlo uses a normal
// distribution today; the type is open to future variants
// (Student-t etc.) but kept narrow on the wire for now.
export type VaRMethod = "historical" | "parametric" | "monte_carlo";

// ALL_VAR_METHODS in the order the UI renders tiles.
export const ALL_VAR_METHODS: readonly VaRMethod[] = [
  "historical",
  "parametric",
  "monte_carlo",
] as const;

// VaRConfidence is the constrained set the backend accepts. The
// UI's confidence dropdown reads from this tuple so changes here
// propagate end-to-end without code drift.
export type VaRConfidence = 0.9 | 0.95 | 0.99;

export const ALL_VAR_CONFIDENCES: readonly VaRConfidence[] = [
  0.9,
  0.95,
  0.99,
] as const;

// VaRResult is one (method × confidence) tile in the snapshot.
// var_pct / cvar_pct are NEGATIVE fractions of NAV. Example:
// var_pct = -0.023 means "we are 95% confident the one-day loss
// won't exceed 2.3% of NAV".
export interface VaRResult {
  method: VaRMethod;
  confidence: number;
  horizon: number;
  var_pct: number;
  cvar_pct: number;
  monte_carlo_seed?: number;
  monte_carlo_paths?: number;
}

// VaRSnapshot is the live read response.
export interface VaRSnapshot {
  fund_id: string;
  generated_at: string;
  horizon: number;
  lookback_days: number;
  sample_size: number;
  mean_daily_return: number;
  stdev_daily_return: number;
  sample_window_start?: string;
  sample_window_end?: string;
  results: VaRResult[];
}

// VaRTrendPoint is one archived snapshot row used by the
// dashboard sparkline.
export interface VaRTrendPoint {
  id: number;
  calculated_at: string;
  method: VaRMethod;
  confidence: number;
  horizon_days: number;
  var_pct: number;
  cvar_pct: number;
  sample_size: number;
  lookback_days: number;
}

// =====================================================================
// S7 / P3-3 — Stress scenarios.
// =====================================================================

// StressCategory enumerates the canonical buckets the admin UI
// groups scenarios under. Matches the CHECK constraint on
// stress_scenarios.category.
export type StressCategory = "historical" | "hypothetical" | "regulatory";

export const ALL_STRESS_CATEGORIES: readonly StressCategory[] = [
  "historical",
  "hypothetical",
  "regulatory",
] as const;

// StressShockTargetType picks how a shock matches holdings.
// Priority (highest to lowest): instrument > market > asset_class
// > factor > wildcard.
export type StressShockTargetType =
  | "instrument"
  | "market"
  | "asset_class"
  | "factor"
  | "wildcard";

export const ALL_STRESS_TARGET_TYPES: readonly StressShockTargetType[] = [
  "instrument",
  "market",
  "asset_class",
  "factor",
  "wildcard",
] as const;

// StressShock is one element of a scenario's shock list.
// `value` is a signed decimal fraction (-0.20 = "-20% return");
// for factor shocks the applied return per holding is
// value * loading, capped by the engine if |applied| > 1.
export interface StressShock {
  target_type: StressShockTargetType;
  target_key: string;
  value: number;
}

// StressScenario is the admin-managed library row.
export interface StressScenario {
  id: string;
  name: string;
  category: StressCategory;
  description: string;
  shocks: StressShock[];
  created_by?: string;
  created_at: string;
  updated_at: string;
}

// StressImpact is one row of the per-holding drill-down. PnL
// signed (negative = loss). applied_shock_* are empty when the
// holding didn't match any shock.
export interface StressImpact {
  instrument_key: string;
  symbol: string;
  asset_class?: string;
  market_value_before: number;
  market_value_after: number;
  pnl: number;
  applied_return: number;
  applied_shock_type?: string;
  applied_shock_key?: string;
}

// StressResult is the engine's output for one (fund, scenario)
// run. nav_* are gross MV (sum of |market_value|); pnl_total
// is signed; pnl_pct is the signed fraction of nav_before.
export interface StressResult {
  fund_id: string;
  scenario_id: string;
  calculated_at: string;
  nav_before: number;
  nav_after: number;
  pnl_total: number;
  pnl_pct: number;
  holding_count: number;
  shocked_count: number;
  impacts: StressImpact[];
}

// ---- Brinson attribution (S7 / P3-4) ------------------------------
// Decomposes a fund's active return vs benchmark into three effects
// per bucket: allocation, selection, interaction.

// BucketDimension picks the grouping axis for both portfolio and
// benchmark compositions. "sector" is reserved for a future
// sector-classification table and may return an empty portfolio
// composition today.
export type BrinsonBucketDimension = "asset_class" | "market" | "sector";

export const ALL_BRINSON_DIMENSIONS: readonly BrinsonBucketDimension[] = [
  "asset_class",
  "market",
  "sector",
];

// BrinsonBucket is one (key, weight, return_pct) row inside a
// composition. Weights are fractions summing to ~1.
export interface BrinsonBucket {
  key: string;
  weight: number;
  return_pct: number;
}

// BrinsonComposition is an admin-managed benchmark composition row.
export interface BrinsonComposition {
  id: string;
  benchmark_id: string;
  dimension: BrinsonBucketDimension;
  asof: string; // YYYY-MM-DD
  buckets: BrinsonBucket[];
  note?: string;
  created_by?: string;
  created_at: string;
  updated_at: string;
}

// BrinsonBenchmarkSummary is the deduped catalog row returned to
// fund operators (just the metadata they need to pick one).
export interface BrinsonBenchmarkSummary {
  benchmark_id: string;
  dimension: BrinsonBucketDimension;
  latest_asof: string;
}

// BrinsonBucketAttribution is the per-bucket output row.
export interface BrinsonBucketAttribution {
  key: string;
  portfolio_weight: number;
  benchmark_weight: number;
  portfolio_return: number;
  benchmark_return: number;
  allocation: number;
  selection: number;
  interaction: number;
  total_effect: number;
}

// BrinsonResult is the engine's full output for one run.
export interface BrinsonResult {
  fund_id: string;
  benchmark_id: string;
  dimension: BrinsonBucketDimension;
  composition_id?: string;
  calculated_at: string;
  portfolio_return: number;
  benchmark_return: number;
  active_return: number;
  allocation_total: number;
  selection_total: number;
  interaction_total: number;
  bucket_count: number;
  buckets: BrinsonBucketAttribution[];
}

// BrinsonHistoryEntry is one archived run as returned by
// /brinson/history. Buckets are optional because thin trend rows
// might choose to omit them in a future API revision.
export interface BrinsonHistoryEntry {
  id: number;
  fund_id: string;
  benchmark_id: string;
  dimension: BrinsonBucketDimension;
  composition_id: string;
  calculated_at: string;
  active_return: number;
  portfolio_return: number;
  benchmark_return: number;
  allocation_total: number;
  selection_total: number;
  interaction_total: number;
  bucket_count: number;
  buckets?: BrinsonBucketAttribution[];
}

// ---------------------------------------------------------------------------
// S8.1 — analyst panel (fundamentals / sentiment / news / technical)
// ---------------------------------------------------------------------------
//
// Four specialised analysts vote on one symbol; the panel
// aggregates them into a single bullish / bearish / neutral
// verdict. Each per-category report carries its own thesis,
// findings, risks, and the deterministic data points it cited.
//
// The wire shapes mirror server/cmd/server/analyst_panel_handler.go.

export type AnalystCategory = "fundamentals" | "sentiment" | "news" | "technical";

export const ALL_ANALYST_CATEGORIES: readonly AnalystCategory[] = [
  "fundamentals",
  "sentiment",
  "news",
  "technical",
];

export type AnalystDirection = "bullish" | "bearish" | "neutral";

export interface AnalystDataPoint {
  name: string;
  value: string;
  source?: string;
}

export interface AnalystReport {
  id?: string;
  agent_id: string;
  agent_name: string;
  category: AnalystCategory;
  symbol: string;
  asof: string;
  generated_at: string;
  direction: AnalystDirection;
  confidence: number; // 0..100
  thesis: string;
  key_findings: string[];
  risks: string[];
  data_points?: AnalystDataPoint[];
  sources?: string[];
  prompt_tokens?: number;
  completion_tokens?: number;
  llm_model?: string;
}

export interface AnalystPanelReport {
  id?: string;
  fund_id: string;
  symbol: string;
  asof: string;
  generated_at: string;
  aggregate_direction: AnalystDirection;
  aggregate_confidence: number; // 0..100
  categories_voted: number; // 0..4
  per_category_votes: Record<string, number>;
  reports: AnalystReport[];
}

// AnalystQualityScoreInput is the typed quality block the caller
// can feed the panel run; fields mirror internal/quality.Score.
export interface AnalystQualityScoreInput {
  profitability_z: number;
  growth_z: number;
  safety_z: number;
  composite_z: number;
  quartile: number;
}

export interface AnalystFundamentalsInput {
  quality_score?: AnalystQualityScoreInput;
  metrics?: Record<string, number>;
  industry_peers?: string[];
  filings_url?: string;
}

export interface AnalystSentimentAggregateInput {
  average: number;
  count: number;
  polarity: string;
}

export interface AnalystSentimentItemInput {
  title: string;
  source: string;
  score: number;
  published_at?: string;
  url?: string;
}

export interface AnalystSentimentInput {
  aggregate: AnalystSentimentAggregateInput;
  recent_items?: AnalystSentimentItemInput[];
  source_breakdown?: Record<string, number>;
}

export interface AnalystNewsHeadlineInput {
  title: string;
  source: string;
  summary?: string;
  published_at?: string;
  url?: string;
  language?: string;
}

export interface AnalystNewsInput {
  headlines?: AnalystNewsHeadlineInput[];
  material_event_tags?: string[];
}

export interface AnalystQuantSnapshotInput {
  regime: string;
  close: number;
  atr14: number;
  atr_pct: number;
  position_size_ceiling_pct: number;
}

export interface AnalystTechnicalInput {
  snapshot: AnalystQuantSnapshotInput;
  signals?: Record<string, number>;
  price_history_spark?: number[];
}

// AnalystRunRequest is the body for POST
// /api/funds/{fundId}/analysts/run. Every block is optional —
// the analyst whose block is missing falls back to its
// "sitting out" path with a neutral / floor verdict.
export interface AnalystRunRequest {
  symbol: string;
  asset_class?: string;
  market?: string;
  asof?: string;
  notes?: string;
  persist?: boolean;
  price_last?: number;
  price_change?: number;
  volume?: number;
  avg_volume?: number;
  fundamentals?: AnalystFundamentalsInput;
  sentiment?: AnalystSentimentInput;
  news?: AnalystNewsInput;
  technical?: AnalystTechnicalInput;
}

// ---------------------------------------------------------------------------
// S8.2 — Bull / Bear debate
// ---------------------------------------------------------------------------
//
// The debate orchestrator runs N rounds where Bull and Bear
// (forced personas) take turns arguing for / against the same
// symbol. The four analyst reports from S8.1 are the input
// they share.

export type AdvocateStance = "bull" | "bear";

export const ALL_ADVOCATE_STANCES: readonly AdvocateStance[] = ["bull", "bear"];

export interface DebateArgument {
  id?: string;
  agent_id: string;
  agent_name: string;
  stance: AdvocateStance;
  symbol: string;
  round: number;
  asof: string;
  generated_at: string;
  direction: AnalystDirection;
  confidence: number; // 0..100
  thesis: string;
  support_points: string[];
  rebuttals: string[];
  cited_reports?: string[];
  llm_model?: string;
}

export interface DebateVerdict {
  direction: AnalystDirection;
  confidence: number; // 0..100
  winner_stance?: AdvocateStance;
  bull_confidence: number;
  bear_confidence: number;
  contested: boolean;
  winning_summary?: string;
  losing_summary?: string;
}

export interface DebateTranscript {
  id?: string;
  fund_id: string;
  panel_id?: string;
  symbol: string;
  asof: string;
  generated_at: string;
  verdict: DebateVerdict;
  arguments: DebateArgument[];
  panel?: AnalystPanelReport;
}

// DebateRunRequest extends AnalystRunRequest with debate-only
// knobs. The handler runs the panel + debate in one call.
export interface DebateRunRequest extends AnalystRunRequest {
  rounds?: number;
}

// ---------------------------------------------------------------------------
// S8.4 — Agent reputation ledger
// ---------------------------------------------------------------------------
//
// The reputation ledger keeps a rolling per-agent realised-alpha
// record. The backfill driver (server) reads each analyst-panel
// and debate-transcript, multiplies the agent's direction by
// the symbol's forward return, and writes one outcome per
// (agent, symbol, asof, horizon). The aggregate stats table
// (decisions, hits, avg_alpha, hit_rate) is then recomputed.

export type AgentReputationKind = "analyst" | "advocate" | "pm" | "researcher";

export const ALL_AGENT_REPUTATION_KINDS: readonly AgentReputationKind[] = [
  "analyst",
  "advocate",
  "pm",
  "researcher",
];

export interface AgentReputationStats {
  fund_id: string;
  agent_id: string;
  agent_name: string;
  agent_kind: AgentReputationKind;
  category: string;
  decisions_count: number;
  hits_count: number;
  misses_count: number;
  hit_rate: number; // 0..1
  avg_alpha: number; // realised - benchmark, averaged
  sum_alpha: number;
  avg_confidence: number; // 0..100
  last_decision_at?: string;
  updated_at: string;
}

export interface AgentReputationOutcome {
  id: string;
  fund_id: string;
  agent_id: string;
  agent_name: string;
  agent_kind: AgentReputationKind;
  category: string;
  symbol: string;
  asof: string;
  direction: AnalystDirection;
  confidence: number; // 0..100
  realised_return: number; // forward window return fraction
  benchmark_return: number;
  alpha: number; // realised - benchmark
  horizon_days: number;
  source_panel_id?: string;
  source_debate_id?: string;
  note?: string;
  created_at: string;
}

export interface AgentReputationRebuildRequest {
  fund_id?: string; // empty = all funds
}

export interface AgentReputationRebuildResponse {
  outcomes_written: number;
  status: string;
}

// ---------------------------------------------------------------------------
// Workflow checkpoints (S9.2)
// ---------------------------------------------------------------------------

export type WorkflowCheckpointStatus =
  | "success"
  | "failed"
  | "skipped"
  | "pending"
  | "paused";

export interface WorkflowCheckpoint {
  id: string;
  run_id: string;
  fund_id: string;
  trading_date: string; // YYYY-MM-DD (UTC)
  step: string;
  status: WorkflowCheckpointStatus;
  attempts: number;
  started_at: string; // RFC3339
  ended_at: string;
  duration_ms: number;
  error_text?: string;
  payload?: unknown;
  created_at: string;
  updated_at: string;
}

export interface ListWorkflowCheckpointsResponse {
  checkpoints: WorkflowCheckpoint[];
}

export interface ResumeWorkflowCheckpointRequest {
  run_id: string;
  step?: string; // optional — defaults to latest failed/paused
}

export interface ResumeWorkflowCheckpointResponse {
  run_id: string;
  step: string;
  status: string;
}

// ---------------------------------------------------------------------------
// Model A/B experiments (S10.3 / S10.4)
// ---------------------------------------------------------------------------

/**
 * 模型 A/B 实验的一条 arm 配置。镜像 server/internal/modelab/types.go
 * 的 ArmConfig。API Key NOT included — the server resolves system keys
 * at hook time.
 */
export interface ModelABArm {
  name: string;
  provider: string;
  model_name: string;
  base_url?: string;
  model_tier?: string;
  temperature?: number;
  max_tokens?: number;
}

export type ModelABExperimentStatus =
  | "draft"
  | "running"
  | "paused"
  | "completed"
  | "archived";

export type ModelABScope = "global" | "fund" | "agent_role" | "agent_id";

export interface ModelABExperiment {
  id: string;
  name: string;
  description?: string;
  scope: ModelABScope;
  scope_target?: string;
  step_filter: string[];
  arms: ModelABArm[];
  traffic_split: number[];
  status: ModelABExperimentStatus;
  start_at?: string;
  end_at?: string;
  max_total_tokens?: number;
  tokens_used: number;
  created_by?: string;
  created_at: string;
  updated_at: string;
}

export interface ListModelABExperimentsResponse {
  experiments: ModelABExperiment[];
}

export interface CreateModelABExperimentRequest {
  name: string;
  description?: string;
  scope: ModelABScope;
  scope_target?: string;
  step_filter?: string[];
  arms: ModelABArm[];
  traffic_split: number[];
  max_total_tokens?: number;
  start_immediate?: boolean;
}

export interface SetModelABStatusRequest {
  status: ModelABExperimentStatus;
}

// UpdateModelABExperimentRequest mirrors CreateModelABExperimentRequest
// minus start_immediate. The server only honours edits while the
// target experiment is still in draft state; otherwise it returns
// HTTP 409 with error code "not_editable".
export interface UpdateModelABExperimentRequest {
  name: string;
  description?: string;
  scope: ModelABScope;
  scope_target?: string;
  step_filter?: string[];
  arms: ModelABArm[];
  traffic_split: number[];
  max_total_tokens?: number;
}

export interface CloneModelABExperimentRequest {
  name?: string;
}

export interface BulkSetModelABStatusRequest {
  ids: string[];
  status: ModelABExperimentStatus;
}

export interface BulkSetModelABStatusResponse {
  updated: number;
}

export interface ModelABReport {
  experiment: {
    id: string;
    name: string;
    scope: string;
    scope_target?: string;
    status: string;
    started_at?: string;
    ended_at?: string;
  };
  window: { from?: string; to?: string };
  arms: ModelABArmMetric[];
}

export interface ModelABArmMetric {
  arm_index: number;
  arm_name: string;
  arm_label: string;
  primary_count: number;
  shadow_count: number;
  error_count: number;
  avg_latency_ms: number;
  total_input_tokens: number;
  total_output_tokens: number;
  total_cost_micro: number;
  agreement_with_primary_pct: number; // -1 if not computed
}

// ---------------------------------------------------------------------------
// Sprint 11.4 — LLM health admin endpoints.
// ---------------------------------------------------------------------------

// LLMHealthSourceRow mirrors one row of /api/admin/llm-health/summary
// sources[]. Source is the decision_source enum (mirrors the Go side).
// Count is the number of plans in the window keyed by that source.
export interface LLMHealthSourceRow {
  source: string;
  count: number;
}

export interface LLMHealthCategoryRow {
  category: string;
  provider?: string;
  count: number;
}

export interface LLMHealthSummary {
  window_hours: number;
  sources: LLMHealthSourceRow[];
  categories: LLMHealthCategoryRow[];
}

// LLMHealthRecentFallback is the admin-only shape that EXPLICITLY
// includes the raw provider summary (Summary). Non-admin client
// projections (PlanFallbackReason in lib/api.ts) strip this field —
// see attachDecisionSource in the Go wiring layer.
export interface LLMHealthRecentFallback {
  plan_id: string;
  fund_id: string;
  source: string;
  category?: string;
  provider?: string;
  model?: string;
  summary?: string;
  created_at: string;
}

export interface LLMHealthRecentFallbacksResponse {
  window_hours: number;
  items: LLMHealthRecentFallback[];
}

// ---------------------------------------------------------------------------
// Sprint 12.3 — alertmanager-ingested events.
// ---------------------------------------------------------------------------

export type AlertSeverity = "info" | "warning" | "critical";
export type AlertStatus = "firing" | "resolved";

export interface AdminAlertEvent {
  id: string;
  fingerprint: string;
  alertName: string;
  severity: AlertSeverity | string;
  component?: string;
  status: AlertStatus | string;
  summary?: string;
  description?: string;
  labels: Record<string, string>;
  annotations: Record<string, string>;
  startsAt: string;
  endsAt?: string;
  receivedAt: string;
  acknowledgedBy?: string;
  acknowledgedAt?: string;
  acknowledgementNote?: string;
}

export interface ListAdminAlertsResponse {
  events: AdminAlertEvent[];
}

export interface AcknowledgeAlertRequest {
  note?: string;
}

// ---------------------------------------------------------------------------
// Sprint 13 — model A/B promotion drafts.
// ---------------------------------------------------------------------------

export type ModelABPromotionStatus = "pending" | "applied" | "rejected" | "superseded";

export interface ModelABPromotionDraft {
  id: string;
  experiment_id: string;
  recommended_arm_index: number;
  recommended_arm_label: string;
  primary_arm_index: number;
  primary_arm_label: string;
  streak_days: number;
  evaluated_at: string;
  window_from?: string;
  window_to?: string;
  criteria_payload?: Record<string, unknown>;
  // Only present on the detail endpoint; the list endpoint omits the
  // full report snapshot to keep payloads small.
  report_snapshot?: Record<string, unknown>;
  status: ModelABPromotionStatus | string;
  applied_by?: string;
  applied_at?: string;
  rejection_reason?: string;
  created_at: string;
}

export interface ListModelABPromotionDraftsResponse {
  items: ModelABPromotionDraft[];
}

export interface ApplyModelABPromotionResponse {
  ok: boolean;
  draft_id: string;
  experiment_id: string;
  experiment_closed: boolean;
  warning?: string;
}

export interface RejectModelABPromotionRequest {
  reason?: string;
}

export interface ScanModelABPromotionsResponse {
  drafts_upserted: number;
}

// SessionResponse mirrors GET /api/auth/session. Every field is
// optional because the unauthenticated path returns
// { authenticated: false } with nothing else; callers must guard
// on `authenticated` (web) or treat missing user_id as logged-out
// (android).
export interface SessionResponse {
  user_id?: string;
  email?: string;
  display_name?: string;
  role?: string;
  expires_at?: string;
}

export interface CompanySummary {
  id: string;
  name: string;
  description?: string;
}

export interface FundSummary {
  id: string;
  name: string;
  status: string;
  nav: number;
  total_assets: number;
  base_currency?: string;
  market?: string;
}

export interface CompanyWithFunds extends CompanySummary {
  funds: FundSummary[];
}

export interface CompanyListResponse {
  companies: CompanyWithFunds[];
}

export interface FundDetail extends FundSummary {
  exchange?: string;
  description?: string;
  team_member_count?: number;
  config?: Record<string, unknown>;
  daily_return?: number;
  benchmark_symbol?: string;
}

export interface TeamMemberSummary {
  agent_id: string;
  member_id: string;
  role: string;
  focus?: string;
  status: string;
  joined_at: string;
  name?: string;
  model_provider?: string;
  model_name?: string;
}

export interface TeamListResponse {
  members: TeamMemberSummary[];
}

export interface PlanSummary {
  id: string;
  fund_id: string;
  status: string;
  trading_date: string;
  created_at: string;
  action_count: number;
  reasoning?: string;
}

export interface PlanListResponse {
  plans: PlanSummary[];
}

export interface PlanAction {
  id: string;
  symbol: string;
  action: string;
  amount?: number;
  qty?: number;
  execution_status?: string;
  strategy?: string;
  reasoning?: string;
}

export interface PlanDetail extends PlanSummary {
  actions: PlanAction[];
}

export interface DecisionTrace {
  plan_id: string;
  fund_id: string;
  trading_date: string;
  inputs?: Record<string, unknown>;
  reasoning?: string;
  cited_blocks?: string[];
}

// ---------------------------------------------------------------------------
// Order cancel / replace (P0-5)
// ---------------------------------------------------------------------------

/**
 * OrderActionResponse mirrors the trim wire shape returned by the
 * cancel/replace endpoints (cmd/server/order_actions_handler.go
 * orderResponse). Numeric fields are 0 when unset on the underlying
 * order — UIs should treat 0 as "no value" and only render the row
 * when truthy.
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

/**
 * ReplaceOrderPayload — every numeric field is optional; nil-as-no-
 * change at the wire. The server enforces (a) at least one numeric
 * change is set, (b) each numeric is > 0, and (c) trailPercent is
 * in (0, 1). The note field is captured into the audit metadata
 * only and does NOT count as a field change for the "at least one
 * change" rule.
 */
export interface ReplaceOrderPayload {
  quantity?: number;
  limitPrice?: number;
  stopPrice?: number;
  trailAmount?: number;
  trailPercent?: number;
  displayQty?: number;
  note?: string;
}

// MemoryLayer mirrors the server-side CHECK constraint on
// memories.layer (see migrations/039_attribution_memory_layer.sql
// and wiring_adapters.go). Adding a layer here without adding the
// matching SQL CHECK constraint will break write paths; adding it
// to SQL without here will surface as silent type drift in web /
// android. The union must stay in lock-step with the server.
export type MemoryLayer =
  | "long_term"
  | "daily"
  | "dreams"
  | "agent"
  | "analysis"
  | "attribution";

export interface MemoryItem {
  id: string;
  fund_id: string;
  agent_id?: string;
  layer: MemoryLayer;
  title?: string;
  content: string;
  trading_date?: string;
  tags?: string[];
  created_at: string;
}

export interface MemoryListResponse {
  items: MemoryItem[];
}

export interface ReflectionItem {
  id: string;
  fund_id: string;
  theme: string;
  title: string;
  content: string;
  tags?: string[];
  trading_date?: string;
  created_at: string;
}

export interface ReflectionListResponse {
  fund_id: string;
  items: ReflectionItem[];
  generated_at: string;
}

export interface PortfolioPosition {
  symbol: string;
  qty: number;
  cost_price: number;
  current_price: number;
  market_value: number;
  unrealized_pnl: number;
  weight_pct?: number;
}

export interface PortfolioSnapshot {
  fund_id: string;
  total_assets: number;
  available_cash: number;
  positions: PortfolioPosition[];
  as_of: string;
}

/**
 * TradeRecord mirrors the api.Trade shape returned by
 * GET /api/funds/{fundId}/trades. Lifecycle / order-action fields
 * (status, replaceCount, cancelReason) are present in the JSON
 * even when the underlying row hasn't been touched, so the Orders
 * UI can decide based on status alone whether to render the
 * Cancel / Modify buttons.
 *
 * Field naming is camelCase here to match the JSON encoder; the
 * legacy mobile screens use snake_case (e.g. trading_date) so we
 * deliberately keep TradeRecord on its own naming style rather
 * than retro-fitting older types.
 */
export interface TradeRecord {
  id: string;
  fundId: string;
  symbol: string;
  instrumentKey?: string;
  side: string;
  orderType: string;
  status: string;
  quantity: number;
  filledQty: number;
  price: number;
  filledPrice?: number;
  amount?: number;
  tradingMode: string;
  executedAt?: string;
  createdAt: string;
  market?: string;
  exchange?: string;
  feeCommission?: number;
  feeStampTax?: number;
  feeTransfer?: number;
  stopPrice?: number;
  trailAmount?: number;
  trailPercent?: number;
  displayQty?: number;
  cancelReason?: string;
  replaceCount?: number;
}

export interface TradeListResponse {
  trades: TradeRecord[];
}

export interface NavPoint {
  trading_date: string;
  nav: number;
  daily_return?: number;
}

export interface NavHistoryResponse {
  fund_id: string;
  points: NavPoint[];
}

// ---------------------------------------------------------------------------
// Corporate actions — split / cash dividend / stock dividend / combined
// ---------------------------------------------------------------------------

export type CorpActionType =
  | "split"
  | "cash_dividend"
  | "stock_dividend"
  | "combined";

/** A single corp-action receipt. Wire shape matches
 *  server's `CorpActionApplicationDTO`; see
 *  `server/internal/api/corp_action_handler.go`. */
export interface CorpActionApplication {
  instrumentKey: string;
  /** ISO date — corporate action ex-date / 除权除息日. */
  exDate: string;
  actionType: CorpActionType;
  /** new_shares / old_shares. 1.0 means no share change. A 10:1
   *  forward split is 10.0; a 1:5 reverse split is 0.2. */
  splitRatio: number;
  /** Gross dividend per OLD share, in the fund's base currency. */
  cashDividend: number;
  appliedAt: string;
  preQuantity: number;
  postQuantity: number;
  preCostPrice: number;
  postCostPrice: number;
  /** Total cash credited to the fund (= preQuantity × cashDividend). */
  cashCredit: number;
}

export interface CorpActionListResponse {
  items: CorpActionApplication[];
  count: number;
}

// ---------------------------------------------------------------------------
// Benchmark history — fund vs market overlay.
// ---------------------------------------------------------------------------

/**
 * BenchmarkPoint — one (date, normalized value) sample.
 *
 * `value` is the rebased index where the FIRST point in the series is
 * exactly 100. Two series can be plotted on the same Y-axis without
 * extra transformation.
 */
export interface BenchmarkPoint {
  /** ISO yyyy-mm-dd in UTC calendar. */
  date: string;
  value: number;
}

/**
 * BenchmarkSeries — a single line on the fund-vs-market chart.
 *
 * The Fund's own NAV curve uses `id = "fund:<fundId>"` so the UI can
 * style it differently (thicker stroke, fund's accent color). All
 * other series come from the curated catalog.
 */
export interface BenchmarkSeries {
  id: string;
  label: string;
  symbol: string;
  market: string;
  currency?: string;
  points: BenchmarkPoint[];
}

/**
 * BenchmarkCatalogItem — an entry in the picker's "available
 * benchmarks" list. Lighter-weight than BenchmarkSeries because the
 * picker doesn't need price points until the user opts in.
 */
export interface BenchmarkCatalogItem {
  id: string;
  label: string;
  symbol: string;
  market: string;
}

/**
 * BenchmarkPartialFailure — a series the user asked for that
 * couldn't be fetched. Reason is a stable code (not raw provider
 * error text) so the UI can localize it.
 *
 * Known reasons:
 *   "unknown"            — id not in catalog
 *   "no-data"            — provider returned an empty bar slice
 *   "unsupported-market" — no provider claims this market
 *   "timeout"            — context deadline / cancel
 *   "fetch-failed"       — provider returned a non-data error
 *   "no-data-in-range"   — bars existed but all were before `from`
 */
export interface BenchmarkPartialFailure {
  id: string;
  reason:
    | "unknown"
    | "no-data"
    | "unsupported-market"
    | "timeout"
    | "fetch-failed"
    | "no-data-in-range"
    | string;
}

/** Envelope for GET /api/funds/:fundId/benchmark-history. */
export interface BenchmarkHistoryResponse {
  fundId: string;
  /** ISO yyyy-mm-dd. */
  from: string;
  /** ISO yyyy-mm-dd. */
  to: string;
  /** The fund's own normalized NAV curve. */
  fund: BenchmarkSeries;
  /** Successfully fetched benchmarks, in the order requested. */
  benchmarks: BenchmarkSeries[];
  /** Default benchmark IDs for this fund's universe (recommendation). */
  recommended: string[];
  /** Full picker catalog so the UI doesn't need a duplicate list. */
  available: BenchmarkCatalogItem[];
  /** Series the caller asked for that couldn't be fetched. */
  partialFailures?: BenchmarkPartialFailure[];
  /**
   * holdingOverlap — set when the fund's positions overlap a
   * rendered benchmark (e.g. a futures fund 100% in BTCUSDT
   * while the chart is overlaying btc_usdt). The compare line
   * chart will then show two lines tracking each other near-
   * perfectly, which is hard to read; the UI uses this hint to
   * surface a banner pointing the user at the Alpha view.
   */
  holdingOverlap?: BenchmarkHoldingOverlap;
}

/**
 * BenchmarkHoldingOverlap — server hint that the fund's holdings
 * structurally overlap one of the rendered benchmarks. See the
 * `holdingOverlap` field on BenchmarkHistoryResponse for the
 * UX-level rationale.
 *
 * `overlapStrength`:
 *   "dominant" — the matched holding is the largest in the fund
 *               (the Alpha view banner is the strong nudge).
 *   "partial"  — the matched holding exists but isn't dominant
 *               (the banner uses softer copy).
 */
export interface BenchmarkHoldingOverlap {
  primaryBenchmark: string;
  overlapStrength: "dominant" | "partial" | string;
  matchedSymbols?: string[];
}

// ---------------------------------------------------------------------------
// Holdings series — per-holding mini-charts (P1-2).
// ---------------------------------------------------------------------------

/**
 * HoldingSeries — one row in the holdings-series envelope. Same
 * point shape as BenchmarkSeries (rebased to start = 100), with
 * extra holding-specific metadata (entryPrice, name) so the UI
 * can render a "vs entry" annotation.
 */
export interface HoldingSeries {
  instrumentKey: string;
  symbol: string;
  name?: string;
  market?: string;
  entryPrice: number;
  points: BenchmarkPoint[];
}

/** Envelope for GET /api/funds/:fundId/holdings/series. */
export interface HoldingsSeriesResponse {
  fundId: string;
  from: string;
  to: string;
  items: HoldingSeries[];
  partialFailures?: BenchmarkPartialFailure[];
}

// ---------------------------------------------------------------------------
// A/B test shadow comparison (Card D).
// ---------------------------------------------------------------------------

/**
 * ABTestShadowAgent — one agent's aggregated shadow learning across
 * the test window. Lessons / adjustments are deduped and capped
 * server-side (12 each); summaries kept to top 5; memories to 20.
 *
 * The proposedEvolutionDiff is only present when the shadow run
 * actually proposes changes vs. the agent's CURRENT live config —
 * a no-op shadow yields `undefined` so the UI hides the diff section.
 */
export interface ABTestShadowAgent {
  agentId: string;
  agentName?: string;
  role?: string;
  eventCount: number;
  /** ISO yyyy-mm-dd of the most recent shadow learning event. */
  latestTradingDate?: string;
  lessons?: string[];
  adjustments?: string[];
  /** Most recent summaries first; up to 5. */
  summaries?: string[];
  specializationLearning?: Record<string, unknown>[];
  proposedEvolutionDiff?: ABEvolutionConfigDiff;
  memories?: ABTestShadowMemory[];
  timeline?: ABTestShadowAgentDay[];
}

/**
 * ABEvolutionConfigDiff — projected delta vs. the agent's CURRENT
 * live evolution_config. `changed` values are tuples
 * [previous, proposed] surfaced as 2-element arrays in JSON.
 *
 * Empty maps/slices are omitted by the server, so client code
 * should treat `undefined` and "empty" identically.
 */
export interface ABEvolutionConfigDiff {
  added?: Record<string, unknown>;
  changed?: Record<string, [unknown, unknown]>;
  removed?: string[];
}

/**
 * ABTestShadowAgentDay — one day's collapsed view in the agent
 * timeline. Used by the comparison panel to draw a side-by-side
 * "what did A learn on date X vs what did B learn on date X" feed.
 */
export interface ABTestShadowAgentDay {
  /** ISO yyyy-mm-dd. */
  tradingDate: string;
  summary?: string;
  lessons?: string[];
  adjustments?: string[];
}

/**
 * ABTestShadowMemory — one row from ab_test_variant_memory.
 * Layer is the memory bucket (e.g. "shadow", "long_term"), content
 * is the raw payload passed through unchanged so the UI can render
 * whatever shape the writer used.
 */
export interface ABTestShadowMemory {
  memoryKey: string;
  layer: string;
  /** ISO yyyy-mm-dd; absent for memories without a trading-date anchor. */
  tradingDate?: string;
  content?: Record<string, unknown>;
}

/**
 * ABTestShadowAgentVariant — A or B side of the comparison.
 * StrategyConfig echoes what the variant was configured with at
 * start-time so the UI can label the column with the original
 * B-side parameter delta.
 */
export interface ABTestShadowAgentVariant {
  variantKey: "A" | "B" | string;
  variantName?: string;
  strategyConfig?: Record<string, unknown>;
  agents: ABTestShadowAgent[];
}

/**
 * ABTestShadowAgentResponse — envelope for
 * GET /api/abtests/:testId/shadow-agents.
 *
 * `variants` is always exactly 2 elements (A, B). Sides with no
 * shadow learning still appear with `agents: []` so the client
 * never needs to nil-check the array.
 */
export interface ABTestShadowAgentResponse {
  testId: string;
  variants: ABTestShadowAgentVariant[];
}

/**
 * ABAttributionTotals — variant-level rollup. WinTradeRate is in
 * [0, 1]; AvgPnL is realized PnL / trade count (0 when no trades).
 * Turnover is the absolute notional sum so partial fills don't
 * collapse to zero.
 */
export interface ABAttributionTotals {
  tradeCount: number;
  turnover: number;
  realizedPnL: number;
  winTradeRate: number;
  avgPnL: number;
}

/**
 * ABAttributionSymbolRow — one symbol's A vs B side-by-side row.
 * pnlGap is `B − A` (positive = B did better); winner is the
 * canonical "A" / "B" / "tie" string the UI uses for badges.
 *
 * gapPctOfNotional is the absolute gap divided by max(turnoverA,
 * turnoverB), expressed as a percent. This prevents tiny-notional
 * trades from rocketing to the top of the |gap| sort.
 */
export interface ABAttributionSymbolRow {
  symbol: string;
  tradeCountA: number;
  tradeCountB: number;
  realizedPnLA: number;
  realizedPnLB: number;
  turnoverA: number;
  turnoverB: number;
  pnlGap: number;
  gapPctOfNotional: number;
  winner: "A" | "B" | "tie" | string;
}

/**
 * ABTestOperationalAttribution — envelope for
 * GET /api/abtests/:testId/operational-attribution.
 *
 * bySymbol is server-bounded to the top 50 rows by |pnlGap|,
 * tie-broken alphabetically by symbol so the order is stable
 * across requests.
 */
export interface ABTestOperationalAttribution {
  testId: string;
  totalA: ABAttributionTotals;
  totalB: ABAttributionTotals;
  bySymbol: ABAttributionSymbolRow[];
}

// ---------------------------------------------------------------------------
// Locale / formatting helpers — pure, share between web and RN.
// ---------------------------------------------------------------------------

export function formatPercent(value: number, digits = 2): string {
  if (!Number.isFinite(value)) return "—";
  return `${(value * 100).toFixed(digits)}%`;
}

export { messages, resolveMessage } from "./i18n";
export type { LocaleId, Messages } from "./i18n";

export function formatMoney(value: number, currency = "CNY"): string {
  if (!Number.isFinite(value)) return "—";
  try {
    return new Intl.NumberFormat("zh-CN", {
      style: "currency",
      currency,
      maximumFractionDigits: 2,
    }).format(value);
  } catch {
    return `${value.toFixed(2)} ${currency}`;
  }
}
