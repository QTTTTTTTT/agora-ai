import React, { useEffect, useMemo, useState } from "react";
import { Navigate, useLocation } from "react-router-dom";
import { fetchSession, getStoredSession, storeSession } from "../lib/api";
import { useAppPreferences } from "../lib/preferences";

export const RequireRole: React.FC<{ children: React.ReactNode; roles: string[] }> = ({ children, roles }) => {
  const location = useLocation();
  const session = getStoredSession();
  const role = session?.role?.trim() ?? "";

  if (!roles.includes(role)) {
    // Send insufficient-role visitors to the new Master Team hub
    // rather than /companies — the latter is itself gated behind
    // agent_team_mode and would just bounce them through a second
    // redirect on the way to /masters.
    return <Navigate to="/masters" replace state={{ from: location }} />;
  }

  return <>{children}</>;
};

export const AdminGate: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  return <RequireRole roles={["super_admin"]}>{children}</RequireRole>;
};

const AuthGate: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  const location = useLocation();
  const { language } = useAppPreferences();
  const [ready, setReady] = useState(false);
  const [authenticated, setAuthenticated] = useState(false);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            checkingSession: "Checking your session...",
          }
        : {
            checkingSession: "正在校验登录会话...",
          },
    [language],
  );

  useEffect(() => {
    let cancelled = false;
    void fetchSession()
      .then((session) => {
        if (cancelled) {
          return;
        }
        if (session.authenticated && session.user_id) {
          storeSession("", {
            userId: session.user_id,
            email: session.email ?? "",
            displayName: session.display_name ?? "",
            role: session.role ?? "",
            kycStatus: session.kyc_status,
            kycLevel: session.kyc_level,
          });
        }
        setAuthenticated(Boolean(session.authenticated));
        setReady(true);
      })
      .catch(() => {
        if (cancelled) {
          return;
        }
        setAuthenticated(false);
        setReady(true);
      });

    return () => {
      cancelled = true;
    };
  }, []);

  if (!ready) {
    return (
      <div className="flex min-h-screen items-center justify-center bg-gray-50 px-6">
        <div className="rounded-2xl border border-gray-200 bg-white px-6 py-5 text-sm text-gray-500 shadow-sm">{copy.checkingSession}</div>
      </div>
    );
  }

  if (!authenticated) {
    return <Navigate to="/login" replace state={{ from: location }} />;
  }

  return <>{children}</>;
};

export default AuthGate;
