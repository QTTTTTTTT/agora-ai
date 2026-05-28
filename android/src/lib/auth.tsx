/**
 * AuthContext — single source of truth for "am I signed in".
 *
 * 三态：
 *   loading       — 启动时检查 keychain + biometrics 期间
 *   unauthenticated — 渲染 LoginScreen
 *   authenticated — 渲染 main tabs
 *
 * 我们故意不在这里管 access token refresh — server 端用单一长 token，
 * 401 由 apiClient 的 onUnauthorized hook 触发 signOut。
 *
 * Biometrics gate：startup 时如果 keychain 有 token，就要求生物识别再
 * 进入；失败 → 回 unauthenticated 状态（强制重新输密码）。当设备没
 * 支持生物识别时，simplePrompt 返回 null，此时直接放行（仍然走 token）。
 */

import React, { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react';

import { ApiError, type LoginResponse } from '@fundai/api-client';
import { apiClient, bootstrapApi, getInMemoryToken, onUnauthorized, setSessionToken } from './api';
import { requireBiometrics } from './secureStore';
import { isBiometricEnabled } from './userPrefs';

export type AuthState =
  | { status: 'loading' }
  | { status: 'unauthenticated'; reason?: 'logout' | 'biometrics' | 'unauthorized' | 'session_error' }
  | { status: 'authenticated'; user: LoginResponse };

interface AuthContextValue {
  state: AuthState;
  login(email: string, password: string): Promise<void>;
  logout(): Promise<void>;
  requireReauth(reason: string): Promise<boolean>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: React.ReactNode }): JSX.Element {
  const [state, setState] = useState<AuthState>({ status: 'loading' });

  useEffect(() => {
    let cancelled = false;
    (async () => {
      await bootstrapApi();
      const token = getInMemoryToken();
      if (!token) {
        if (!cancelled) setState({ status: 'unauthenticated' });
        return;
      }
      // Biometric gate is opt-out — default true to keep historical
      // behaviour. Users who explicitly toggle it off in MoreScreen
      // (e.g. devices without enrolled fingerprints, dev builds) get
      // a clean path to the main tabs without the system prompt.
      if (isBiometricEnabled()) {
        const ok = await requireBiometrics('Unlock FundAI');
        if (ok === false) {
          await setSessionToken(null);
          if (!cancelled) setState({ status: 'unauthenticated', reason: 'biometrics' });
          return;
        }
      }
      try {
        const session = await apiClient.session();
        if (!cancelled) {
          // SessionResponse.user_id is optional in the shared
          // wire type (the unauthenticated branch returns an
          // empty body). If the server says we have no user
          // even though our token loaded, treat it as a stale
          // token and force re-auth.
          if (!session.user_id) {
            await setSessionToken(null);
            setState({ status: 'unauthenticated', reason: 'unauthorized' });
            return;
          }
          setState({
            status: 'authenticated',
            user: {
              token,
              user_id: session.user_id,
              email: session.email,
              display_name: session.display_name,
              role: session.role,
            },
          });
        }
      } catch (err) {
        if (err instanceof ApiError && err.code === 401) {
          await setSessionToken(null);
          if (!cancelled) setState({ status: 'unauthenticated', reason: 'unauthorized' });
        } else if (!cancelled) {
          // Network or 5xx during /api/session bootstrap. Historically we
          // optimistically dropped the user into the main tabs with an
          // empty user_id; the subagent QA flagged this because every
          // downstream tab then renders blank/loading-forever as soon
          // as a real query arrives (we have no fund, no user metadata,
          // no role). Now we keep the user signed in (token still in
          // keychain) but explicitly mark them unauthenticated with the
          // session_error reason so the LoginScreen can present an
          // "offline / try again" CTA instead of silent failure. The
          // keychain token survives so re-login is a no-op when the
          // network recovers.
          setState({ status: 'unauthenticated', reason: 'session_error' });
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    onUnauthorized(() => setState({ status: 'unauthenticated', reason: 'unauthorized' }));
  }, []);

  const login = useCallback(async (email: string, password: string) => {
    const resp = await apiClient.login({ email, password });
    await setSessionToken(resp.token, resp.user_id);
    setState({ status: 'authenticated', user: resp });
  }, []);

  const logout = useCallback(async () => {
    try {
      await apiClient.logout();
    } catch {
      /* swallow; we still want to clear local state */
    }
    await setSessionToken(null);
    setState({ status: 'unauthenticated', reason: 'logout' });
  }, []);

  const requireReauth = useCallback(async (reason: string) => {
    const ok = await requireBiometrics(reason);
    if (ok === false) return false;
    return true;
  }, []);

  const value = useMemo<AuthContextValue>(
    () => ({ state, login, logout, requireReauth }),
    [state, login, logout, requireReauth],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const v = useContext(AuthContext);
  if (!v) throw new Error('useAuth must be used inside an <AuthProvider>');
  return v;
}
