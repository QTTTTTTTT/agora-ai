// featureFlags.tsx — client-side mirror of GET /api/feature-flags.
//
// Mounting:
//   <FeatureFlagsProvider> wraps the authenticated app shell. It
//   fetches the flag set on mount, polls every 60 seconds, and
//   exposes the current snapshot via React context.
//
// Consumption:
//   const enabled = useFeatureFlag("ab_test_compare");  // boolean
//   if (!enabled) return null;
//
// Defaults:
//   Unknown / not-yet-fetched flags resolve to TRUE (the feature
//   is on). This means a fresh deploy that hasn't seen its first
//   /api/feature-flags response renders the full UI rather than a
//   stripped one — better UX than briefly hiding everything until
//   the boot fetch lands.

import React, { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";
import { apiGet } from "./api";

type FlagMap = Record<string, boolean>;

interface FeatureFlagContextValue {
  flags: FlagMap;
  loading: boolean;
  refresh: () => Promise<void>;
}

const FeatureFlagContext = createContext<FeatureFlagContextValue>({
  flags: {},
  loading: true,
  refresh: async () => {},
});

interface PublicFeatureFlag {
  key: string;
  enabled: boolean;
}

interface FeatureFlagsResponse {
  flags: PublicFeatureFlag[];
}

// REFRESH_INTERVAL_MS keeps the SPA in sync with admin toggles
// without hammering the server. 60s is fast enough that a "pause
// this feature" change shows up while the operator is still on the
// admin page — they typically refresh after flipping anyway.
const REFRESH_INTERVAL_MS = 60_000;

export const FeatureFlagsProvider: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  const [flags, setFlags] = useState<FlagMap>({});
  const [loading, setLoading] = useState(true);
  // Hold the latest setFlags identity in a ref so the polling timer
  // can call into it without a stale-closure trap.
  const cancelledRef = useRef(false);

  const fetchOnce = useCallback(async () => {
    try {
      const resp = await apiGet<FeatureFlagsResponse>("/api/feature-flags");
      if (cancelledRef.current) return;
      const next: FlagMap = {};
      for (const f of resp?.flags ?? []) {
        if (f && typeof f.key === "string") {
          next[f.key] = !!f.enabled;
        }
      }
      setFlags(next);
    } catch {
      // Soft-fail: keep the previous map. The default-true semantic
      // means a transient outage doesn't strip the UI for the user.
    } finally {
      if (!cancelledRef.current) {
        setLoading(false);
      }
    }
  }, []);

  useEffect(() => {
    cancelledRef.current = false;
    void fetchOnce();
    const id = window.setInterval(() => {
      void fetchOnce();
    }, REFRESH_INTERVAL_MS);
    return () => {
      cancelledRef.current = true;
      window.clearInterval(id);
    };
  }, [fetchOnce]);

  const value = useMemo<FeatureFlagContextValue>(
    () => ({ flags, loading, refresh: fetchOnce }),
    [flags, loading, fetchOnce],
  );

  return <FeatureFlagContext.Provider value={value}>{children}</FeatureFlagContext.Provider>;
};

// useFeatureFlag returns the current value for `key`. Unknown keys
// resolve to TRUE (the feature is on) — this is the safer default
// for "I forgot to seed this flag" scenarios. Pass `defaultValue`
// to override (e.g. flags that are dark-launched OFF by default).
export function useFeatureFlag(key: string, defaultValue = true): boolean {
  const { flags } = useContext(FeatureFlagContext);
  if (Object.prototype.hasOwnProperty.call(flags, key)) {
    return flags[key];
  }
  return defaultValue;
}

// useFeatureFlagsLoading lets a top-level shell delay rendering
// gated routes until the first fetch lands, if it cares to. Most
// pages should *not* gate on this — the default-true semantics
// already produce a sensible first render.
export function useFeatureFlagsLoading(): boolean {
  return useContext(FeatureFlagContext).loading;
}

// useRefreshFeatureFlags is exposed so the admin console can force
// a refetch right after flipping a flag — saves the operator the
// 60-second wait for the polling tick.
export function useRefreshFeatureFlags(): () => Promise<void> {
  return useContext(FeatureFlagContext).refresh;
}
