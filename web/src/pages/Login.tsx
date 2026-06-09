import React, { useEffect, useMemo, useState } from "react";
import { Link, Navigate, useLocation, useNavigate } from "react-router-dom";
import {
  ApiError,
  exchangeTwoFAChallenge,
  fetchSession,
  formatApiError,
  loginWithPassword,
  registerWithPassword,
} from "../lib/api";
import { useAppPreferences } from "../lib/preferences";
import { BlackPillButton, MascotAvatar, TabPills } from "../theme";

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
  // P0-6 — when the server responds with `requires_2fa` we stash
  // the challenge here and flip the form into the 2FA prompt step.
  // The `mode` toggle (login / register) is hidden during the
  // challenge to keep the user focused.
  const [twoFAChallenge, setTwoFAChallenge] = useState<string | null>(null);
  const [twoFACode, setTwoFACode] = useState("");
  const [twoFARecovery, setTwoFARecovery] = useState("");
  const [twoFAMode, setTwoFAMode] = useState<"code" | "recovery">("code");

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
            twoFA: {
              title: "Two-factor verification",
              subtitle: "Enter the 6-digit code from your authenticator app to finish signing in.",
              codePlaceholder: "6-digit code",
              recoveryPlaceholder: "Recovery code",
              modeCode: "Authenticator code",
              modeRecovery: "Recovery code",
              submit: "Verify and continue",
              submitting: "Verifying...",
              cancel: "Use a different account",
              failed: "Invalid code, please try again.",
            },
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
            twoFA: {
              title: "二次验证",
              subtitle: "请输入身份验证器 App 中显示的 6 位验证码以完成登录。",
              codePlaceholder: "6 位验证码",
              recoveryPlaceholder: "恢复码",
              modeCode: "验证器代码",
              modeRecovery: "恢复码",
              submit: "验证并登录",
              submitting: "验证中...",
              cancel: "更换账号",
              failed: "验证码无效，请重试。",
            },
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
      <div className="flex min-h-screen items-center justify-center px-6 py-12">
        <div className="rounded-envelope bg-cream-0 px-6 py-5 text-sm text-ink-300 shadow-envelope ring-1 ring-ink-100/60">
          {copy.checkingSession}
        </div>
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
        const outcome = await loginWithPassword({
          email: form.email.trim(),
          password: form.password,
        });
        // Server may demand a second factor — flip into the 2FA
        // step. The challenge token expires server-side; if the
        // user dawdles they'll get a 401 and we'll bounce them
        // back to the password step via handleCancelTwoFA.
        if (outcome.kind === "challenge") {
          setTwoFAChallenge(outcome.challenge);
          setTwoFACode("");
          setTwoFARecovery("");
          setTwoFAMode("code");
          return;
        }
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

  async function handle2FASubmit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!twoFAChallenge) return;
    setSubmitting(true);
    setError(null);
    try {
      await exchangeTwoFAChallenge({
        challenge: twoFAChallenge,
        code: twoFAMode === "code" ? twoFACode.trim() : undefined,
        recoveryCode: twoFAMode === "recovery" ? twoFARecovery.trim() : undefined,
      });
      navigate(redirectTo, { replace: true });
    } catch (err) {
      const message = err instanceof ApiError ? formatApiError(err, copy.twoFA.failed) : copy.twoFA.failed;
      setError(message);
    } finally {
      setSubmitting(false);
    }
  }

  function handleCancelTwoFA() {
    setTwoFAChallenge(null);
    setTwoFACode("");
    setTwoFARecovery("");
    setError(null);
  }

  // Cream redesign: shared field styles + the inline TabPills
  // tabset for login/register. Keep tw classes hoisted so the
  // 80-line JSX stays readable.
  const fieldClass =
    "mt-2 w-full rounded-2xl bg-cream-50 px-4 py-3 text-sm text-ink-900 outline-none ring-1 ring-ink-100/80 transition focus:ring-2 focus:ring-sage-500/60 placeholder:text-ink-300";
  const errorBox =
    "rounded-2xl bg-risk-50 px-4 py-3 text-sm text-risk-500 ring-1 ring-risk-100";

  return (
    <div className="flex min-h-screen items-center justify-center px-6 py-12">
      <div className="w-full max-w-md rounded-envelope-lg bg-cream-0 p-8 shadow-envelope ring-1 ring-ink-100/60">
        {twoFAChallenge ? (
          <div>
            <div className="mb-7 flex items-center gap-3">
              <MascotAvatar role="risk" size={56} animated />
              <div>
                <p className="text-[11px] font-semibold uppercase tracking-[0.16em] text-sage-700">
                  FundAI
                </p>
                <h1 className="mt-1 text-2xl font-extrabold text-ink-900">
                  {copy.twoFA.title}
                </h1>
              </div>
            </div>
            <p className="mb-6 text-sm leading-6 text-ink-300">
              {copy.twoFA.subtitle}
            </p>
            <form className="space-y-5" onSubmit={handle2FASubmit}>
              <div className="flex">
                <TabPills
                  tabs={[
                    { key: "code",     label: copy.twoFA.modeCode },
                    { key: "recovery", label: copy.twoFA.modeRecovery },
                  ] as const}
                  active={twoFAMode}
                  onChange={(k) => setTwoFAMode(k as "code" | "recovery")}
                />
              </div>
              {twoFAMode === "code" ? (
                <input
                  autoFocus
                  type="text"
                  inputMode="numeric"
                  maxLength={8}
                  value={twoFACode}
                  onChange={(e) => setTwoFACode(e.target.value.replace(/[^0-9]/g, ""))}
                  placeholder={copy.twoFA.codePlaceholder}
                  className={`${fieldClass} text-center font-mono text-2xl tracking-widest`}
                />
              ) : (
                <input
                  autoFocus
                  type="text"
                  value={twoFARecovery}
                  onChange={(e) => setTwoFARecovery(e.target.value.toUpperCase())}
                  placeholder={copy.twoFA.recoveryPlaceholder}
                  className={`${fieldClass} text-center font-mono text-lg tracking-widest`}
                />
              )}
              {error ? <div className={errorBox}>{error}</div> : null}
              <BlackPillButton
                type="submit"
                size="lg"
                block
                disabled={submitting || (twoFAMode === "code" ? twoFACode.length < 6 : !twoFARecovery.trim())}
              >
                {submitting ? copy.twoFA.submitting : copy.twoFA.submit}
              </BlackPillButton>
              <button
                type="button"
                onClick={handleCancelTwoFA}
                className="w-full rounded-full bg-cream-50 px-4 py-3 text-sm font-medium text-ink-300 ring-1 ring-ink-100 transition hover:text-ink-700"
              >
                {copy.twoFA.cancel}
              </button>
            </form>
          </div>
        ) : (
          <>
            <div className="mb-7 flex items-center gap-3">
              <MascotAvatar role="captain" size={56} animated />
              <div>
                <p className="text-[11px] font-semibold uppercase tracking-[0.16em] text-sage-700">
                  FundAI
                </p>
                <h1 className="mt-1 text-2xl font-extrabold text-ink-900">
                  {mode === "login" ? copy.titleLogin : copy.titleRegister}
                </h1>
              </div>
            </div>
            <p className="mb-6 text-sm leading-6 text-ink-300">
              {copy.subtitle}
            </p>

            <div className="mb-6 flex">
              <TabPills
                tabs={[
                  { key: "login",    label: copy.tabs.login },
                  { key: "register", label: copy.tabs.register },
                ] as const}
                active={mode}
                onChange={(k) => {
                  setMode(k as AuthMode);
                  setError(null);
                }}
              />
            </div>

            <form className="space-y-5" onSubmit={handleSubmit}>
              <label className="block text-sm font-medium text-ink-700">
                {copy.labels.email}
                <input
                  value={form.email}
                  onChange={(event) => updateField("email", event.target.value)}
                  placeholder={copy.placeholders.email}
                  autoComplete="email"
                  className={fieldClass}
                />
              </label>

              {mode === "register" ? (
                <label className="block text-sm font-medium text-ink-700">
                  {copy.labels.displayName}
                  <input
                    value={form.displayName}
                    onChange={(event) => updateField("displayName", event.target.value)}
                    placeholder={copy.placeholders.displayName}
                    autoComplete="nickname"
                    className={fieldClass}
                  />
                </label>
              ) : null}

              <label className="block text-sm font-medium text-ink-700">
                {copy.labels.password}
                <input
                  type="password"
                  value={form.password}
                  onChange={(event) => updateField("password", event.target.value)}
                  placeholder={copy.placeholders.password}
                  autoComplete={mode === "login" ? "current-password" : "new-password"}
                  className={fieldClass}
                />
              </label>

              {mode === "register" ? (
                <label className="block text-sm font-medium text-ink-700">
                  {copy.labels.confirmPassword}
                  <input
                    type="password"
                    value={form.confirmPassword}
                    onChange={(event) => updateField("confirmPassword", event.target.value)}
                    placeholder={copy.placeholders.confirmPassword}
                    autoComplete="new-password"
                    className={fieldClass}
                  />
                </label>
              ) : null}

              {error ? <div className={errorBox}>{error}</div> : null}

              <BlackPillButton type="submit" size="lg" block disabled={submitting} withArrow={!submitting}>
                {submitting
                  ? mode === "login" ? copy.actions.signingIn : copy.actions.registering
                  : mode === "login" ? copy.actions.signIn : copy.actions.register}
              </BlackPillButton>
            </form>

            {mode === "login" ? (
              <>
                <div className="mt-4 text-center text-xs">
                  <Link to="/forgot-password" className="text-sage-700 transition hover:text-sage-500">
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
                <div className="mt-6 flex flex-col items-center gap-2 rounded-2xl bg-sage-50 px-4 py-4 text-xs text-sage-700 ring-1 ring-sage-200/70">
                  <button
                    type="button"
                    onClick={() => window.alert(copy.actions.wechatHint)}
                    className="rounded-full bg-cream-0 px-4 py-2 text-sm font-semibold text-sage-700 ring-1 ring-sage-200 transition hover:bg-sage-50"
                  >
                    {copy.actions.wechat}
                  </button>
                  <p className="text-center text-[11px] text-sage-700/80">
                    {copy.actions.wechatHint}
                  </p>
                </div>
              </>
            ) : null}

            <div className="mt-8 rounded-2xl bg-cream-50 p-4 text-xs leading-6 text-ink-300 ring-1 ring-ink-100/70">
              <p className="font-semibold text-ink-700">{copy.minimumAccountTitle}</p>
              <ul className="mt-2 list-disc space-y-1 pl-5">
                {copy.minimumAccountItems.map((item) => (
                  <li key={item}>{item}</li>
                ))}
              </ul>
            </div>

            <div className="mt-6 text-center text-xs">
              <Link to="/masters" className="text-ink-300 transition hover:text-ink-700">
                {copy.backHome}
              </Link>
            </div>
          </>
        )}
      </div>
    </div>
  );
};

export default Login;
