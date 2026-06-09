import React, { Component, ReactNode, Suspense, useMemo } from "react";
import {
  BrowserRouter,
  Routes,
  Route,
  Navigate,
  Link,
  useRouteError,
  isRouteErrorResponse,
} from "react-router-dom";
import AuthGate, { AdminGate } from "./components/AuthGate";
import AgentTeamGate from "./components/AgentTeamGate";
import FundLayout from "./components/FundLayout";
import PreferenceDock from "./components/PreferenceDock";
import SessionExpiryWatcher from "./components/SessionExpiryWatcher";
import RouteFallback from "./components/RouteFallback";
import CommandPalette from "./components/CommandPalette";
import AnnouncementCenter from "./components/AnnouncementCenter";
import { FeatureFlagsProvider } from "./lib/featureFlags";
import { ComplianceProvider, type ComplianceLocale } from "./lib/compliance";
import { useAppPreferences } from "./lib/preferences";
import { lazyWithRetry } from "./lib/lazyWithRetry";
import { ToastViewport, toast } from "./lib/toast";

// Every page goes through `lazyWithRetry` instead of the bare React.lazy.
// Naked `lazy(() => import(...))` has zero protection against the well-known
// "Failed to fetch dynamically imported module" race (network blip, route
// switch cancelling the in-flight fetch, or a stale entry chunk after a
// redeploy). `lazyWithRetry` retries the import a few times with backoff,
// and as a last resort issues a single `location.reload()` so the user
// picks up the fresh `index.html` (and its updated chunk hash map). See
// the file header in `./lib/lazyWithRetry.ts` for the full rationale.
const Companies = lazyWithRetry(() => import("./pages/Companies"));
const MultiFundOverview = lazyWithRetry(() => import("./pages/MultiFundOverview"));
const Login = lazyWithRetry(() => import("./pages/Login"));
const ForgotPassword = lazyWithRetry(() => import("./pages/ForgotPassword"));
const ResetPassword = lazyWithRetry(() => import("./pages/ResetPassword"));
const VerifyEmail = lazyWithRetry(() => import("./pages/VerifyEmail"));
const AccountSecurity = lazyWithRetry(() => import("./pages/AccountSecurity"));
const Dashboard = lazyWithRetry(() => import("./pages/Dashboard"));
const TeamManagement = lazyWithRetry(() => import("./pages/TeamManagement"));
const DecisionCenter = lazyWithRetry(() => import("./pages/DecisionCenter"));
const ABTestCompare = lazyWithRetry(() => import("./pages/ABTestCompare"));
const ForwardGate = lazyWithRetry(() => import("./pages/ForwardGate"));
const Backtest = lazyWithRetry(() => import("./pages/Backtest"));
const FundPerformance = lazyWithRetry(() => import("./pages/FundPerformance"));
const AgentLearning = lazyWithRetry(() => import("./pages/AgentLearning"));
const AgentLineage = lazyWithRetry(() => import("./pages/AgentLineage"));
const AuditLog = lazyWithRetry(() => import("./pages/AuditLog"));
const MemoryCenter = lazyWithRetry(() => import("./pages/MemoryCenter"));
const TradeHistory = lazyWithRetry(() => import("./pages/TradeHistory"));
const FundSettings = lazyWithRetry(() => import("./pages/FundSettings"));
const CashLedger = lazyWithRetry(() => import("./pages/CashLedger"));
const Subscription = lazyWithRetry(() => import("./pages/Subscription"));
const ModelConfig = lazyWithRetry(() => import("./pages/ModelConfig"));
const Usage = lazyWithRetry(() => import("./pages/Usage"));
const Admin = lazyWithRetry(() => import("./pages/Admin"));
const SkillInbox = lazyWithRetry(() => import("./pages/SkillInbox"));
const Wallet = lazyWithRetry(() => import("./pages/Wallet"));
const KYC = lazyWithRetry(() => import("./pages/KYC"));
const Marketplace = lazyWithRetry(() => import("./pages/Marketplace"));
const Auctions = lazyWithRetry(() => import("./pages/Auctions"));
const Promotions = lazyWithRetry(() => import("./pages/Promotions"));
const FundWorkflow = lazyWithRetry(() => import("./pages/FundWorkflow"));
// /style-preview is the design-system showcase that mirrors the
// 2026 cream / sage / black-pill refresh. Auth-gated so we don't
// leak it to anonymous landing visitors, but otherwise free of
// fund / company context — safe to open anywhere.
const StylePreview = lazyWithRetry(() => import("./pages/StylePreview"));
const PaperTrading = lazyWithRetry(() => import("./pages/PaperTrading"));
const CNIntraday = lazyWithRetry(() => import("./pages/CNIntraday"));
// /welcome + /advisor — the new "master team consultation" mode
// introduced alongside migration 098. Both lazy-loaded so the main
// /companies bundle stays the same size for existing users who
// don't navigate to the new flow.
const Welcome = lazyWithRetry(() => import("./pages/Welcome"));
const Advisor = lazyWithRetry(() => import("./pages/Advisor"));
const DailyPicks = lazyWithRetry(() => import("./pages/DailyPicks"));
// MastersHub is the new authenticated landing page that consolidates
// the per-stock advisor, daily picks, paper trading, and trending
// surfaces under a single navigable shell. See pages/MastersHub.tsx
// for the layout rationale. Replaces /companies as the post-login
// default; /companies + the /funds/* subtree are now gated behind
// AgentTeamGate (see components/AgentTeamGate.tsx).
const MastersHub = lazyWithRetry(() => import("./pages/MastersHub"));
const TrendingMostActive = lazyWithRetry(() => import("./pages/TrendingMostActive"));
const SettingsByok = lazyWithRetry(() => import("./pages/SettingsByok"));

interface ErrorBoundaryCopy {
  unexpectedError: string;
  pageError: string;
  routeError: string;
  backToCompanies: string;
  unknownError: string;
  loading: string;
  retry: string;
  copyError: string;
  copied: string;
  copyFailed: string;
  showDetails: string;
  hideDetails: string;
}

// buildErrorReport emits a single multi-line string the user copies
// to a support channel / bug tracker. The shape is intentionally
// stable so an operator can paste a chunk of these into a tool and
// regex out fields if needed:
//
//   FundAI error report
//   url: https://fund.ai/funds/abc/decisions
//   when: 2026-06-05T07:14:22.491Z
//   message: Cannot read properties of undefined (reading 'plan')
//   stack:
//     <error.stack>
//   componentStack:
//     <react info.componentStack>
//
// We do NOT include localStorage / sessionStorage / cookies — those
// often carry tokens. requestId / fundId / userId are NOT included
// either; the request_id is already on the server logs and pairing
// the report by timestamp+url is enough. If we ever decide to
// embed user_id we should mask it before copying.
function buildErrorReport(error: Error | null, componentStack: string | null): string {
  const url = typeof window !== "undefined" ? window.location.href : "";
  const ua = typeof navigator !== "undefined" ? navigator.userAgent : "";
  const lines = [
    "FundAI error report",
    `url: ${url}`,
    `when: ${new Date().toISOString()}`,
    `userAgent: ${ua}`,
    `message: ${error?.message ?? "(none)"}`,
  ];
  if (error?.stack) {
    lines.push("stack:");
    lines.push(error.stack);
  }
  if (componentStack) {
    lines.push("componentStack:");
    lines.push(componentStack.trimEnd());
  }
  return lines.join("\n");
}

async function copyToClipboard(text: string): Promise<boolean> {
  // navigator.clipboard requires HTTPS / localhost. In the rare
  // production case where it's unavailable (older WebViews, embedded
  // mobile contexts) we fall back to the legacy execCommand path so
  // the user still gets a "copied" outcome instead of a silent
  // failure.
  if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // fall through to execCommand fallback
    }
  }
  if (typeof document === "undefined") return false;
  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.setAttribute("readonly", "");
  textarea.style.position = "fixed";
  textarea.style.left = "-9999px";
  document.body.appendChild(textarea);
  textarea.select();
  try {
    const ok = document.execCommand("copy");
    return ok;
  } catch {
    return false;
  } finally {
    document.body.removeChild(textarea);
  }
}

interface ErrorPanelProps {
  copy: ErrorBoundaryCopy;
  title: string;
  message: string;
  error: Error | null;
  componentStack: string | null;
  onRetry?: () => void;
}

const ErrorPanel: React.FC<ErrorPanelProps> = ({
  copy,
  title,
  message,
  error,
  componentStack,
  onRetry,
}) => {
  const [showDetails, setShowDetails] = React.useState(false);
  const handleCopy = React.useCallback(async () => {
    const report = buildErrorReport(error, componentStack);
    const ok = await copyToClipboard(report);
    if (ok) {
      toast.success(copy.copied);
    } else {
      toast.error(copy.copyFailed);
    }
  }, [error, componentStack, copy.copied, copy.copyFailed]);
  return (
    <div className="flex min-h-screen items-center justify-center bg-gray-50 p-8">
      <div className="w-full max-w-md rounded-lg bg-white p-8 shadow-lg">
        <h1 className="mb-2 text-center text-xl font-bold text-red-600">{title}</h1>
        <p className="mb-5 break-words text-center text-sm text-gray-500">{message}</p>
        <div className="flex flex-col gap-2">
          {onRetry ? (
            <button
              type="button"
              onClick={onRetry}
              className="rounded-md bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-700"
            >
              {copy.retry}
            </button>
          ) : null}
          <button
            type="button"
            onClick={handleCopy}
            className="rounded-md border border-gray-300 bg-white px-4 py-2 text-sm font-medium text-gray-700 hover:bg-gray-50"
          >
            {copy.copyError}
          </button>
          <Link
            to="/masters"
            className="rounded-md border border-gray-300 bg-white px-4 py-2 text-center text-sm font-medium text-gray-700 hover:bg-gray-50"
          >
            {copy.backToCompanies}
          </Link>
        </div>
        {error?.stack || componentStack ? (
          <div className="mt-5 border-t border-gray-200 pt-3">
            <button
              type="button"
              onClick={() => setShowDetails((v) => !v)}
              className="text-xs font-medium text-gray-500 hover:text-gray-700"
              aria-expanded={showDetails}
            >
              {showDetails ? copy.hideDetails : copy.showDetails}
            </button>
            {showDetails ? (
              <pre className="mt-2 max-h-60 overflow-auto whitespace-pre-wrap break-words rounded-md bg-gray-50 p-3 text-[11px] leading-snug text-gray-600">
                {buildErrorReport(error, componentStack)}
              </pre>
            ) : null}
          </div>
        ) : null}
      </div>
    </div>
  );
};

interface ErrorBoundaryState {
  hasError: boolean;
  error: Error | null;
  componentStack: string | null;
}

class ErrorBoundary extends Component<
  { children: ReactNode; copy: ErrorBoundaryCopy },
  ErrorBoundaryState
> {
  state: ErrorBoundaryState = { hasError: false, error: null, componentStack: null };

  static getDerivedStateFromError(error: Error): Partial<ErrorBoundaryState> {
    return { hasError: true, error };
  }

  componentDidCatch(error: Error, info: React.ErrorInfo) {
    console.error("[ErrorBoundary]", error, info.componentStack);
    // Capture the React component stack so the "Copy error" button
    // can include it. info.componentStack is a string (or null);
    // setState here is safe because getDerivedStateFromError already
    // flipped hasError synchronously, so this is just an enrichment.
    this.setState({ componentStack: info.componentStack ?? null });
  }

  // reset is invoked by the "Retry" button. Returning to a clean
  // state lets <Suspense> re-mount the route's children, which is
  // enough to recover from transient render errors (a stale chunk,
  // an over-eager useMemo, a one-off network glitch). For
  // persistent errors the user can still fall back to "Back to
  // companies".
  reset = () => {
    this.setState({ hasError: false, error: null, componentStack: null });
  };

  render() {
    if (this.state.hasError) {
      return (
        <ErrorPanel
          copy={this.props.copy}
          title={this.props.copy.pageError}
          message={this.state.error?.message ?? this.props.copy.unexpectedError}
          error={this.state.error}
          componentStack={this.state.componentStack}
          onRetry={this.reset}
        />
      );
    }
    return this.props.children;
  }
}

const RouteErrorElement: React.FC<{ copy: ErrorBoundaryCopy }> = ({ copy }) => {
  const error = useRouteError();
  const message = isRouteErrorResponse(error)
    ? `${error.status} — ${error.statusText}`
    : error instanceof Error
      ? error.message
      : copy.unknownError;
  const errObj = error instanceof Error ? error : null;
  return (
    <ErrorPanel
      copy={copy}
      title={copy.routeError}
      message={message}
      error={errObj}
      componentStack={null}
      // No onRetry: the route's loader / data error is owned by
      // react-router, which already exposes its own
      // useRouteError() reset semantics outside this component
      // tree. Adding a no-op retry button would mislead the user.
    />
  );
};

const AppRoutes: React.FC = () => {
  const { language } = useAppPreferences();
  const copy = useMemo<ErrorBoundaryCopy>(
    () =>
      language === "en-US"
        ? {
            unexpectedError: "An unexpected error occurred.",
            pageError: "Page error",
            routeError: "Route error",
            backToCompanies: "Back to Master Team",
            unknownError: "Unknown error",
            loading: "Loading…",
            retry: "Retry",
            copyError: "Copy error report",
            copied: "Error report copied",
            copyFailed: "Could not access clipboard",
            showDetails: "Show details",
            hideDetails: "Hide details",
          }
        : {
            unexpectedError: "发生了未预期的异常。",
            pageError: "页面发生错误",
            routeError: "路由异常",
            backToCompanies: "返回大师团队",
            unknownError: "未知错误",
            loading: "正在加载…",
            retry: "重试",
            copyError: "复制错误报告",
            copied: "错误报告已复制",
            copyFailed: "无法访问剪贴板",
            showDetails: "查看详情",
            hideDetails: "收起详情",
          },
    [language],
  );

  return (
    <BrowserRouter>
      {/* AnnouncementCenter sits at the very top so any sticky
          banner is always above the rest of the app shell. It is
          self-gating: when there's no session or no unread items
          it renders nothing. */}
      <AnnouncementCenter />
      <PreferenceDock />
      {/* Single global listener for `fundai:session-expired` events
          dispatched from api.ts whenever a request hits a 401. Lives
          inside <BrowserRouter> so it can call useNavigate. See
          components/SessionExpiryWatcher.tsx for the dedup + carve-out
          rationale. Rendered as a portal-style toast, no layout impact
          when idle. */}
      <SessionExpiryWatcher />
      {/* Global Cmd+K / Ctrl+K command palette. Lives inside
          <BrowserRouter> so its action handlers can call
          useNavigate(). The palette is rendered unconditionally
          but invisible until triggered — open is internal state.
          See components/CommandPalette.tsx for the command list
          and extension points. */}
      <CommandPalette />
      {/* Global toast viewport. Subscribes to the queue in lib/toast.tsx
          and renders a stack of dismissable toasts top-right. Mounted
          here so non-React modules (lib/api.ts, error boundaries) can
          emit via the imperative `toast.error(...)` facade without
          holding a hook reference. Stays just under SessionExpiryWatcher
          in z-index so the auth toast sits on top during a 401. */}
      <ToastViewport />
      <ErrorBoundary copy={copy}>
        <Suspense fallback={<RouteFallback loadingText={copy.loading} />}>
          <Routes>
            {/* Post-login default lands on /masters (the new
                Master Team Hub). The legacy /companies surface is
                gated behind AgentTeamGate below — direct hits
                redirect to /masters unless the user is super_admin
                or the agent_team_mode flag is flipped ON. */}
            <Route path="/" element={<Navigate to="/masters" replace />} />
            <Route path="/login" element={<Login />} />
            <Route path="/forgot-password" element={<ForgotPassword />} />
            <Route path="/reset-password" element={<ResetPassword />} />
            <Route path="/verify-email" element={<AuthGate><VerifyEmail /></AuthGate>} />
            <Route path="/account/security" element={<AuthGate><AccountSecurity /></AuthGate>} />
            <Route path="/welcome" element={<AuthGate><Welcome /></AuthGate>} />
            <Route path="/masters" element={<AuthGate><MastersHub /></AuthGate>} />
            <Route path="/advisor" element={<AuthGate><Advisor /></AuthGate>} />
            <Route path="/daily-picks" element={<AuthGate><DailyPicks /></AuthGate>} />
            <Route path="/trending" element={<Navigate to="/trending/most-active" replace />} />
            <Route path="/trending/most-active" element={<AuthGate><TrendingMostActive /></AuthGate>} />
            <Route path="/settings/byok" element={<AuthGate><SettingsByok /></AuthGate>} />
            <Route path="/companies" element={<AuthGate><AgentTeamGate><Companies /></AgentTeamGate></AuthGate>} />
            <Route path="/portfolio-overview" element={<AuthGate><MultiFundOverview /></AuthGate>} />
            <Route path="/style-preview" element={<AuthGate><StylePreview /></AuthGate>} />
            <Route path="/papertrading" element={<AuthGate><PaperTrading /></AuthGate>} />
            <Route path="/cnintraday" element={<AuthGate><CNIntraday /></AuthGate>} />
            <Route path="/wallet" element={<AuthGate><Wallet /></AuthGate>} />
            <Route path="/kyc" element={<AuthGate><KYC /></AuthGate>} />
            <Route path="/marketplace" element={<AuthGate><Marketplace /></AuthGate>} />
            <Route path="/auctions" element={<AuthGate><Auctions /></AuthGate>} />
            <Route path="/admin" element={<AuthGate><AdminGate><Admin /></AdminGate></AuthGate>} />
            <Route path="/admin/skills/inbox" element={<AuthGate><AdminGate><SkillInbox /></AdminGate></AuthGate>} />
            <Route
              path="/funds/:fundId"
              element={<AuthGate><AgentTeamGate><FundLayout /></AgentTeamGate></AuthGate>}
              errorElement={<RouteErrorElement copy={copy} />}
            >
              <Route index element={<Dashboard />} />
              <Route path="performance" element={<FundPerformance />} />
              <Route path="team" element={<TeamManagement />} />
              <Route path="decisions" element={<DecisionCenter />} />
              <Route path="compare" element={<ABTestCompare />} />
              <Route path="forward-gate" element={<ForwardGate />} />
              <Route path="backtests" element={<Backtest />} />
              <Route path="promotions" element={<Promotions />} />
              <Route path="promotions/:promotionId" element={<Promotions />} />
              <Route path="learning" element={<AgentLearning />} />
              <Route path="lineage" element={<AgentLineage />} />
              <Route path="memory" element={<MemoryCenter />} />
              <Route path="trades" element={<TradeHistory />} />
              <Route path="cash-ledger" element={<CashLedger />} />
              <Route path="workflow" element={<FundWorkflow />} />
              <Route path="settings" element={<FundSettings />} />
              <Route path="subscription" element={<Subscription />} />
              <Route path="models" element={<ModelConfig />} />
              <Route path="usage" element={<Usage />} />
              <Route path="audit" element={<AuditLog />} />
            </Route>
            <Route path="*" element={<Navigate to="/masters" replace />} />
          </Routes>
        </Suspense>
      </ErrorBoundary>
    </BrowserRouter>
  );
};

const App: React.FC = () => {
  // The compliance locale matches whatever the rest of the app
  // resolved via the preferences hook. We can't useAppPreferences
  // here without re-implementing the resolution, so we read the
  // localStorage value directly with a safe en-US fallback. The
  // ComplianceProvider re-fetches its bundles whenever locale
  // changes — flipping language in the PreferenceDock will rotate
  // every disclosure block.
  const locale: ComplianceLocale =
    typeof window !== "undefined" &&
    window.localStorage.getItem("fundai.language") === "en-US"
      ? "en-US"
      : "zh-CN";
  return (
    <FeatureFlagsProvider>
      <ComplianceProvider locale={locale}>
        <AppRoutes />
      </ComplianceProvider>
    </FeatureFlagsProvider>
  );
};

export default App;
