import React, { useEffect, useMemo, useState } from "react";
import { Link, Navigate, useLocation, useNavigate } from "react-router-dom";
import { ApiError, fetchSession, formatApiError, loginWithPassword, registerWithPassword } from "../lib/api";
import { useAppPreferences } from "../lib/preferences";

type AuthMode = "login" | "register";

interface FormState {
  email: string;
  password: string;
  confirmPassword: string;
  displayName: string;
}

const initialFormState: FormState = {
  email: "",
  password: "",
  confirmPassword: "",
  displayName: "",
};

function isValidEmail(value: string): boolean {
  return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(value.trim());
}

const Login: React.FC = () => {
  const navigate = useNavigate();
  const location = useLocation();
  const { language } = useAppPreferences();
  const from = (location.state as { from?: { pathname?: string } } | null)?.from?.pathname;
  // SessionExpiryWatcher attaches the user's pre-401 deep link as
  // `?next=<path>`. Honour it first so a user kicked off
  // `/funds/abc/decisions` returns to exactly that page after re-auth
  // — falling back to the legacy `location.state.from` pattern (used by
  // AuthGate redirects), and finally to /companies for cold sign-ins.
  const nextParam = useMemo(() => {
    const raw = new URLSearchParams(location.search).get("next");
    if (!raw) return null;
    // Only allow same-origin relative paths to avoid an open-redirect.
    // Absolute URLs (http://evil.com/...) and protocol-relative
    // (//evil.com) are rejected — the canonical XSS-via-redirect class.
    if (!raw.startsWith("/") || raw.startsWith("//")) return null;
    if (raw === "/login") return null;
    return raw;
  }, [location.search]);
  const redirectTo = useMemo(() => {
    if (nextParam) return nextParam;
    if (from && from !== "/login") return from;
    return "/companies";
  }, [nextParam, from]);

  const [mode, setMode] = useState<AuthMode>("login");
  const [form, setForm] = useState<FormState>(initialFormState);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [checkingSession, setCheckingSession] = useState(true);
  const [authenticated, setAuthenticated] = useState(false);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            checkingSession: "Checking your existing session...",
            titleLogin: "Sign in to console",
            titleRegister: "Create your first account",
            subtitle:
              "Sign in with email and password to establish a real session. On success, the browser stores both a Bearer token and an HttpOnly session cookie.",
            tabs: {
              login: "Sign in",
              register: "Register",
            },
            labels: {
              email: "Email",
              displayName: "Display name",
              password: "Password",
              confirmPassword: "Confirm password",
            },
            placeholders: {
              email: "you@example.com",
              displayName: "e.g. Operations admin",
              password: "Enter at least 8 characters",
              confirmPassword: "Enter password again",
            },
            validation: {
              invalidEmail: "Enter a valid email address.",
              shortPassword: "Password must be at least 8 characters.",
              passwordMismatch: "Passwords do not match.",
            },
            actions: {
              signingIn: "Signing in...",
              registering: "Registering...",
              signIn: "Sign in and continue",
              register: "Register and continue",
              wechat: "Sign in with WeChat",
              wechatHint: "Scan in the WeChat app (miniapp users can also tap from inside their account).",
            },
            errors: {
              loginFailed: "Sign in failed",
              registerFailed: "Registration failed",
              invalidCredentials: "Incorrect email or password.",
            },
            minimumAccountTitle: "Current minimum account model",
            minimumAccountItems: [
              "The first registered account automatically becomes the super admin.",
              "All later registrations default to regular user.",
              "When the session expires, the frontend clears local credentials and redirects back to login.",
            ],
            backHome: "Back to home",
            forgotPassword: "Forgot your password?",
          }
        : {
            checkingSession: "正在检查现有登录会话...",
            titleLogin: "登录控制台",
            titleRegister: "注册首个账号",
            subtitle:
              "使用邮箱和密码建立真实登录会话。登录成功后，浏览器会同时保存 Bearer Token 与 HttpOnly Session Cookie。",
            tabs: {
              login: "登录",
              register: "注册",
            },
            labels: {
              email: "邮箱",
              displayName: "显示名称",
              password: "密码",
              confirmPassword: "确认密码",
            },
            placeholders: {
              email: "you@example.com",
              displayName: "例如：运营管理员",
              password: "请输入至少 8 位密码",
              confirmPassword: "请再次输入密码",
            },
            validation: {
              invalidEmail: "请输入合法的邮箱地址。",
              shortPassword: "密码长度至少为 8 位。",
              passwordMismatch: "两次输入的密码不一致。",
            },
            actions: {
              signingIn: "登录中...",
              registering: "注册中...",
              signIn: "登录并进入系统",
              register: "注册并进入系统",
              wechat: "使用微信登录",
              wechatHint: "请在微信中扫码（小程序用户也可在小程序内直接点击进入）。",
            },
            errors: {
              loginFailed: "登录失败",
              registerFailed: "注册失败",
              invalidCredentials: "邮箱或密码错误。",
            },
            minimumAccountTitle: "当前最小账号体系",
            minimumAccountItems: [
              "首个注册账号会自动成为超级管理员。",
              "后续注册账号默认为普通用户。",
              "如果会话失效，前端会清空本地凭证并跳回登录页。",
            ],
            backHome: "返回首页",
            forgotPassword: "忘记密码？",
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
        setAuthenticated(Boolean(session.authenticated));
        setCheckingSession(false);
      })
      .catch(() => {
        if (cancelled) {
          return;
        }
        setAuthenticated(false);
        setCheckingSession(false);
      });

    return () => {
      cancelled = true;
    };
  }, []);

  if (checkingSession) {
    return (
      <div className="flex min-h-screen items-center justify-center bg-slate-950 px-6 py-12 text-white">
        <div className="rounded-2xl border border-white/10 bg-white/5 px-6 py-5 text-sm text-slate-300">{copy.checkingSession}</div>
      </div>
    );
  }

  if (authenticated) {
    return <Navigate to={redirectTo} replace />;
  }

  function updateField<K extends keyof FormState>(field: K, value: FormState[K]) {
    setForm((current) => ({ ...current, [field]: value }));
  }

  function validateForm(): string | null {
    const email = form.email.trim();
    const password = form.password;
    const confirmPassword = form.confirmPassword;

    if (!isValidEmail(email)) {
      return copy.validation.invalidEmail;
    }
    if (password.trim().length < 8) {
      return copy.validation.shortPassword;
    }
    if (mode === "register" && password !== confirmPassword) {
      return copy.validation.passwordMismatch;
    }
    return null;
  }

  async function handleSubmit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();

    const validationError = validateForm();
    if (validationError) {
      setError(validationError);
      return;
    }

    setSubmitting(true);
    setError(null);
    try {
      if (mode === "login") {
        await loginWithPassword({
          email: form.email.trim(),
          password: form.password,
        });
      } else {
        await registerWithPassword({
          email: form.email.trim(),
          password: form.password,
          displayName: form.displayName.trim(),
        });
        // Route new accounts to /verify-email so they're nudged to
        // confirm their address before going deep into the product.
        // We pass the email forward so the page can pre-fill the
        // resend field; users who skip can still reach /companies via
        // the "Skip for now" link on that page.
        navigate(`/verify-email?email=${encodeURIComponent(form.email.trim())}`, { replace: true });
        return;
      }
      navigate(redirectTo, { replace: true });
    } catch (err) {
      const message = formatApiError(err, mode === "login" ? copy.errors.loginFailed : copy.errors.registerFailed);
      setError(err instanceof ApiError && err.status === 401 ? copy.errors.invalidCredentials : message);
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-slate-950 px-6 py-12 text-white">
      <div className="w-full max-w-md rounded-3xl border border-white/10 bg-white/5 p-8 shadow-2xl backdrop-blur">
        <div className="mb-8">
          <p className="text-sm font-medium text-indigo-300">FundAI</p>
          <h1 className="mt-2 text-3xl font-semibold">{mode === "login" ? copy.titleLogin : copy.titleRegister}</h1>
          <p className="mt-3 text-sm leading-6 text-slate-300">{copy.subtitle}</p>
        </div>

        <div className="mb-6 grid grid-cols-2 rounded-2xl border border-white/10 bg-slate-900/60 p-1 text-sm">
          <button
            type="button"
            onClick={() => {
              setMode("login");
              setError(null);
            }}
            className={`rounded-xl px-4 py-2 transition ${mode === "login" ? "bg-indigo-500 text-white" : "text-slate-300 hover:text-white"}`}
          >
            {copy.tabs.login}
          </button>
          <button
            type="button"
            onClick={() => {
              setMode("register");
              setError(null);
            }}
            className={`rounded-xl px-4 py-2 transition ${mode === "register" ? "bg-indigo-500 text-white" : "text-slate-300 hover:text-white"}`}
          >
            {copy.tabs.register}
          </button>
        </div>

        <form className="space-y-5" onSubmit={handleSubmit}>
          <label className="block text-sm font-medium text-slate-200">
            {copy.labels.email}
            <input
              value={form.email}
              onChange={(event) => updateField("email", event.target.value)}
              placeholder={copy.placeholders.email}
              autoComplete="email"
              className="mt-2 w-full rounded-2xl border border-white/10 bg-slate-900/80 px-4 py-3 text-sm text-white outline-none transition focus:border-indigo-400 focus:ring-2 focus:ring-indigo-500/40"
            />
          </label>

          {mode === "register" ? (
            <label className="block text-sm font-medium text-slate-200">
              {copy.labels.displayName}
              <input
                value={form.displayName}
                onChange={(event) => updateField("displayName", event.target.value)}
                placeholder={copy.placeholders.displayName}
                autoComplete="nickname"
                className="mt-2 w-full rounded-2xl border border-white/10 bg-slate-900/80 px-4 py-3 text-sm text-white outline-none transition focus:border-indigo-400 focus:ring-2 focus:ring-indigo-500/40"
              />
            </label>
          ) : null}

          <label className="block text-sm font-medium text-slate-200">
            {copy.labels.password}
            <input
              type="password"
              value={form.password}
              onChange={(event) => updateField("password", event.target.value)}
              placeholder={copy.placeholders.password}
              autoComplete={mode === "login" ? "current-password" : "new-password"}
              className="mt-2 w-full rounded-2xl border border-white/10 bg-slate-900/80 px-4 py-3 text-sm text-white outline-none transition focus:border-indigo-400 focus:ring-2 focus:ring-indigo-500/40"
            />
          </label>

          {mode === "register" ? (
            <label className="block text-sm font-medium text-slate-200">
              {copy.labels.confirmPassword}
              <input
                type="password"
                value={form.confirmPassword}
                onChange={(event) => updateField("confirmPassword", event.target.value)}
                placeholder={copy.placeholders.confirmPassword}
                autoComplete="new-password"
                className="mt-2 w-full rounded-2xl border border-white/10 bg-slate-900/80 px-4 py-3 text-sm text-white outline-none transition focus:border-indigo-400 focus:ring-2 focus:ring-indigo-500/40"
              />
            </label>
          ) : null}

          {error ? <div className="rounded-2xl border border-red-500/30 bg-red-500/10 px-4 py-3 text-sm text-red-200">{error}</div> : null}

          <button
            type="submit"
            disabled={submitting}
            className="w-full rounded-2xl bg-indigo-500 px-4 py-3 text-sm font-semibold text-white transition hover:bg-indigo-400 disabled:cursor-not-allowed disabled:opacity-60"
          >
            {submitting ? (mode === "login" ? copy.actions.signingIn : copy.actions.registering) : mode === "login" ? copy.actions.signIn : copy.actions.register}
          </button>
        </form>

        {mode === "login" ? (
          <>
            <div className="mt-4 text-center text-xs text-slate-400">
              <Link to="/forgot-password" className="text-indigo-300 transition hover:text-indigo-200">
                {copy.forgotPassword}
              </Link>
            </div>

            {/*
              * WeChat login entry. The product surface lists WeChat sign-in
              * as a supported path on both web and miniapp; previously web
              * only exposed email/password and the WeChat box was missing
              * entirely. Until the OAuth-redirect handler is wired on the
              * server we keep this as an *information* button that explains
              * how to complete the WeChat flow today (via the miniapp's
              * code2session, which already shares the JWT keyring with web
              * via the auth_wechat handler). Replace the click handler with
              * the OAuth redirect once /api/auth/wechat-redirect ships.
              */}
            <div className="mt-6 flex flex-col items-center gap-2 rounded-2xl border border-emerald-500/20 bg-emerald-500/5 px-4 py-4 text-xs text-emerald-200">
              <button
                type="button"
                onClick={() =>
                  window.alert(copy.actions.wechatHint)
                }
                className="rounded-xl border border-emerald-400/40 bg-emerald-500/10 px-4 py-2 text-sm font-medium text-emerald-100 transition hover:bg-emerald-500/20"
              >
                {copy.actions.wechat}
              </button>
              <p className="text-center text-[11px] text-emerald-200/80">{copy.actions.wechatHint}</p>
            </div>
          </>
        ) : null}

        <div className="mt-8 rounded-2xl border border-white/10 bg-slate-900/60 p-4 text-xs leading-6 text-slate-300">
          <p className="font-medium text-slate-100">{copy.minimumAccountTitle}</p>
          <ul className="mt-2 list-disc space-y-1 pl-5">
            {copy.minimumAccountItems.map((item) => (
              <li key={item}>{item}</li>
            ))}
          </ul>
        </div>

        <div className="mt-6 text-center text-xs text-slate-400">
          <Link to="/companies" className="transition hover:text-white">
            {copy.backHome}
          </Link>
        </div>
      </div>
    </div>
  );
};

export default Login;
