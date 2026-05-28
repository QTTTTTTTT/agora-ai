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
import FundLayout from "./components/FundLayout";
import PreferenceDock from "./components/PreferenceDock";
import SessionExpiryWatcher from "./components/SessionExpiryWatcher";
import { useAppPreferences } from "./lib/preferences";
import { lazyWithRetry } from "./lib/lazyWithRetry";

// Every page goes through `lazyWithRetry` instead of the bare React.lazy.
// Naked `lazy(() => import(...))` has zero protection against the well-known
// "Failed to fetch dynamically imported module" race (network blip, route
// switch cancelling the in-flight fetch, or a stale entry chunk after a
// redeploy). `lazyWithRetry` retries the import a few times with backoff,
// and as a last resort issues a single `location.reload()` so the user
// picks up the fresh `index.html` (and its updated chunk hash map). See
// the file header in `./lib/lazyWithRetry.ts` for the full rationale.
const Companies = lazyWithRetry(() => import("./pages/Companies"));
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

interface ErrorBoundaryCopy {
  unexpectedError: string;
  pageError: string;
  routeError: string;
  backToCompanies: string;
  unknownError: string;
  loading: string;
}

interface ErrorBoundaryState {
  hasError: boolean;
  error: Error | null;
}

class ErrorBoundary extends Component<
  { children: ReactNode; copy: ErrorBoundaryCopy },
  ErrorBoundaryState
> {
  state: ErrorBoundaryState = { hasError: false, error: null };

  static getDerivedStateFromError(error: Error): ErrorBoundaryState {
    return { hasError: true, error };
  }

  componentDidCatch(error: Error, info: React.ErrorInfo) {
    console.error("[ErrorBoundary]", error, info.componentStack);
  }

  render() {
    if (this.state.hasError) {
      return (
        <div className="flex min-h-screen items-center justify-center bg-gray-50 p-8">
          <div className="max-w-md rounded-lg bg-white p-8 text-center shadow-lg">
            <h1 className="mb-2 text-xl font-bold text-red-600">{this.props.copy.pageError}</h1>
            <p className="mb-4 text-sm text-gray-500">{this.state.error?.message ?? this.props.copy.unexpectedError}</p>
            <Link to="/companies" className="rounded-md bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-700">
              {this.props.copy.backToCompanies}
            </Link>
          </div>
        </div>
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

  return (
    <div className="flex min-h-screen items-center justify-center bg-gray-50 p-8">
      <div className="max-w-md rounded-lg bg-white p-8 text-center shadow-lg">
        <h1 className="mb-2 text-xl font-bold text-red-600">{copy.routeError}</h1>
        <p className="mb-4 text-sm text-gray-500">{message}</p>
        <Link to="/companies" className="rounded-md bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-700">
          {copy.backToCompanies}
        </Link>
      </div>
    </div>
  );
};

const RouteFallback: React.FC<{ copy: ErrorBoundaryCopy }> = ({ copy }) => (
  <div className="flex min-h-screen items-center justify-center bg-gray-50 p-8 text-sm text-gray-500">
    {copy.loading}
  </div>
);

const AppRoutes: React.FC = () => {
  const { language } = useAppPreferences();
  const copy = useMemo<ErrorBoundaryCopy>(
    () =>
      language === "en-US"
        ? {
            unexpectedError: "An unexpected error occurred.",
            pageError: "Page error",
            routeError: "Route error",
            backToCompanies: "Back to companies",
            unknownError: "Unknown error",
            loading: "Loading…",
          }
        : {
            unexpectedError: "发生了未预期的异常。",
            pageError: "页面发生错误",
            routeError: "路由异常",
            backToCompanies: "返回公司列表",
            unknownError: "未知错误",
            loading: "正在加载…",
          },
    [language],
  );

  return (
    <BrowserRouter>
      <PreferenceDock />
      {/* Single global listener for `fundai:session-expired` events
          dispatched from api.ts whenever a request hits a 401. Lives
          inside <BrowserRouter> so it can call useNavigate. See
          components/SessionExpiryWatcher.tsx for the dedup + carve-out
          rationale. Rendered as a portal-style toast, no layout impact
          when idle. */}
      <SessionExpiryWatcher />
      <ErrorBoundary copy={copy}>
        <Suspense fallback={<RouteFallback copy={copy} />}>
          <Routes>
            <Route path="/" element={<Navigate to="/companies" replace />} />
            <Route path="/login" element={<Login />} />
            <Route path="/forgot-password" element={<ForgotPassword />} />
            <Route path="/reset-password" element={<ResetPassword />} />
            <Route path="/verify-email" element={<AuthGate><VerifyEmail /></AuthGate>} />
            <Route path="/account/security" element={<AuthGate><AccountSecurity /></AuthGate>} />
            <Route path="/companies" element={<AuthGate><Companies /></AuthGate>} />
            <Route path="/wallet" element={<AuthGate><Wallet /></AuthGate>} />
            <Route path="/kyc" element={<AuthGate><KYC /></AuthGate>} />
            <Route path="/marketplace" element={<AuthGate><Marketplace /></AuthGate>} />
            <Route path="/auctions" element={<AuthGate><Auctions /></AuthGate>} />
            <Route path="/admin" element={<AuthGate><AdminGate><Admin /></AdminGate></AuthGate>} />
            <Route path="/admin/skills/inbox" element={<AuthGate><AdminGate><SkillInbox /></AdminGate></AuthGate>} />
            <Route
              path="/funds/:fundId"
              element={<AuthGate><FundLayout /></AuthGate>}
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
              <Route path="settings" element={<FundSettings />} />
              <Route path="subscription" element={<Subscription />} />
              <Route path="models" element={<ModelConfig />} />
              <Route path="usage" element={<Usage />} />
              <Route path="audit" element={<AuditLog />} />
            </Route>
            <Route path="*" element={<Navigate to="/companies" replace />} />
          </Routes>
        </Suspense>
      </ErrorBoundary>
    </BrowserRouter>
  );
};

const App: React.FC = () => <AppRoutes />;

export default App;
