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
}

export interface ApiClient {
  // 通用
  request<T>(path: string, opts?: CallOptions): Promise<T>;

  // ---- Auth ----
  login(input: LoginInput): Promise<LoginResponse>;
  session(): Promise<SessionResponse>;
  logout(): Promise<void>;
  wechatLogin(input: { code: string }): Promise<LoginResponse>;
  forgotPassword(input: { email: string }): Promise<{ ok: boolean }>;
  resetPassword(input: { token: string; new_password: string }): Promise<{ ok: boolean }>;
  changePassword(input: { current_password: string; new_password: string }): Promise<{ ok: boolean }>;

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

  // ---- Memory ----
  getMemory(fundId: string, layer?: MemoryLayer): Promise<MemoryListResponse>;
  listReflections(fundId: string, limit?: number): Promise<ReflectionListResponse>;

  // ---- Portfolio / NAV ----
  getPortfolio(fundId: string): Promise<PortfolioSnapshot>;
  getNavHistory(fundId: string, days?: number): Promise<NavHistoryResponse>;

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
