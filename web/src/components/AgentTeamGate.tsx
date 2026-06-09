// AgentTeamGate — SPA-side soft gate for the "AI team manages a
// fund" surface (the /companies + /funds/:fundId/* route tree).
//
// Why a soft gate, not the server enforce_server_gate path?
//   The fund/team REST surface is the same one our mobile app +
//   admin tooling depend on. We want to hide the *entrance* from
//   ordinary users without breaking those callers — see the
//   description on the `agent_team_mode` flag in migration 110.
//
// Behaviour
//   1. Flag fetch still in flight → render nothing (avoids a
//      flash of either state). We keep this brief — the provider
//      polls every 60s and the first fetch lands in well under a
//      second on local + prod.
//   2. Flag ON, OR session is super_admin → render children.
//      Super_admin always passes so operators can audit / clean
//      up the legacy data even when the feature is paused for
//      everyone else.
//   3. Otherwise → redirect to /masters (replace, so back-button
//      doesn't bounce them right back into a redirect loop).
//
// Defense in depth
//   The default landing has already moved to /masters, so the
//   only way to land on a gated route is a bookmark or a manual
//   URL. The redirect above silently catches those.

import React from "react";
import { Navigate } from "react-router-dom";
import { getStoredSession } from "../lib/api";
import { useFeatureFlag, useFeatureFlagsLoading } from "../lib/featureFlags";

interface AgentTeamGateProps {
  children: React.ReactNode;
}

const AgentTeamGate: React.FC<AgentTeamGateProps> = ({ children }) => {
  // defaultValue=false is deliberate — until we've heard from
  // /api/feature-flags we behave as if the surface is OFF. This
  // matches the production intent (the surface is opt-in) and
  // means a transient flag-fetch failure can't accidentally
  // expose it.
  const enabled = useFeatureFlag("agent_team_mode", false);
  const loading = useFeatureFlagsLoading();
  const session = getStoredSession();
  const isAdmin = session?.role === "super_admin";

  if (loading) {
    return null;
  }
  if (enabled || isAdmin) {
    return <>{children}</>;
  }
  return <Navigate to="/masters" replace />;
};

export default AgentTeamGate;
