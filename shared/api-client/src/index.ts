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
