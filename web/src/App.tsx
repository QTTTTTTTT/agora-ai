import React, { Component, ReactNode, Suspense, lazy, useMemo } from "react";
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
import { useAppPreferences } from "./lib/preferences";

const Companies = lazy(() => import("./pages/Companies"));
const Login = lazy(() => import("./pages/Login"));
const Dashboard = lazy(() => import("./pages/Dashboard"));
const TeamManagement = lazy(() => import("./pages/TeamManagement"));
const DecisionCenter = lazy(() => import("./pages/DecisionCenter"));
const ABTestCompare = lazy(() => import("./pages/ABTestCompare"));
const ForwardGate = lazy(() => import("./pages/ForwardGate"));
const Backtest = lazy(() => import("./pages/Backtest"));
const FundPerformance = lazy(() => import("./pages/FundPerformance"));
const AgentLearning = lazy(() => import("./pages/AgentLearning"));
const AgentLineage = lazy(() => import("./pages/AgentLineage"));
const AuditLog = lazy(() => import("./pages/AuditLog"));
const MemoryCenter = lazy(() => import("./pages/MemoryCenter"));
const TradeHistory = lazy(() => import("./pages/TradeHistory"));
const FundSettings = lazy(() => import("./pages/FundSettings"));
const Subscription = lazy(() => import("./pages/Subscription"));
const ModelConfig = lazy(() => import("./pages/ModelConfig"));
const Usage = lazy(() => import("./pages/Usage"));
const Admin = lazy(() => import("./pages/Admin"));
const Wallet = lazy(() => import("./pages/Wallet"));
const KYC = lazy(() => import("./pages/KYC"));
const Marketplace = lazy(() => import("./pages/Marketplace"));
const Auctions = lazy(() => import("./pages/Auctions"));
const Promotions = lazy(() => import("./pages/Promotions"));

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
      <ErrorBoundary copy={copy}>
        <Suspense fallback={<RouteFallback copy={copy} />}>
          <Routes>
            <Route path="/" element={<Navigate to="/companies" replace />} />
            <Route path="/login" element={<Login />} />
            <Route path="/companies" element={<AuthGate><Companies /></AuthGate>} />
            <Route path="/wallet" element={<AuthGate><Wallet /></AuthGate>} />
            <Route path="/kyc" element={<AuthGate><KYC /></AuthGate>} />
            <Route path="/marketplace" element={<AuthGate><Marketplace /></AuthGate>} />
            <Route path="/auctions" element={<AuthGate><Auctions /></AuthGate>} />
            <Route path="/admin" element={<AuthGate><AdminGate><Admin /></AdminGate></AuthGate>} />
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
