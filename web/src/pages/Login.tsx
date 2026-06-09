import React, { useEffect, useMemo, useState } from "react";
import { Link, Navigate, useLocation, useNavigate } from "react-router-dom";
import {
  ApiError,
  completeTwoFAEnrollment,
  exchangeTwoFAChallenge,
  fetchSession,
  formatApiError,
  loginWithPassword,
  registerWithPassword,
  startTwoFAEnrollment,
  type TwoFAEnrollmentStartResponse,
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
  // A5 — when the server responds with `requires_2fa_enrollment`
  // (super_admin first-login flow) we hold the grant token here
  // and pivot the screen into the enrollment wizard. `enrollData`
  // is populated after /enroll-start returns the QR + recovery
  // codes; until then we show a small "we need to set up 2FA"
  // intro screen so the user knows what is happening.
  const [enrollGrant, setEnrollGrant] = useState<string | null>(null);
  const [enrollData, setEnrollData] = useState<TwoFAEnrollmentStartResponse | null>(null);
  const [enrollCode, setEnrollCode] = useState("");

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
            enroll: {
              introTitle: "Two-factor authentication required",
              introBody:
                "Your account has elevated administrative privileges. The platform requires you to register an authenticator app before issuing a session. This is a one-time setup that takes about a minute.",
              introCTA: "Begin setup",
              starting: "Generating setup material...",
              qrTitle: "Scan or paste into your authenticator",
              qrBody:
                "Open Google Authenticator, 1Password, Authy or any TOTP-compatible app. Scan the QR code or paste the secret manually, then enter the first six-digit code below.",
              uriLabel: "Provisioning URI",
              secretLabel: "Secret",
              recoveryTitle: "Recovery codes",
              recoveryBody:
                "Save these recovery codes somewhere safe (a password manager, a sealed envelope, an encrypted note). Each code can be used exactly once if you lose access to your authenticator. They WILL NOT be shown again.",
              codePlaceholder: "First 6-digit code",
              submit: "Verify and finish setup",
              submitting: "Verifying...",
              cancel: "Cancel and sign in as someone else",
              failed: "Code is invalid or expired; try the next one your authenticator shows.",
              expired: "Enrollment session expired. Please sign in again to restart.",
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
            enroll: {
              introTitle: "需要先完成二次验证设置",
              introBody:
                "你的账号是平台高权限管理员，必须先绑定身份验证器才能签发会话。这是一次性操作，大约一分钟即可完成。",
              introCTA: "开始设置",
              starting: "正在生成设置信息……",
              qrTitle: "扫码或粘贴至身份验证器",
              qrBody:
                "打开 Google Authenticator、1Password、Authy 或任何支持 TOTP 的 App，扫码或手动粘贴下方密钥，然后在最下方输入首个 6 位动态码。",
              uriLabel: "Provisioning URI",
              secretLabel: "密钥",
              recoveryTitle: "恢复码",
              recoveryBody:
                "请将以下恢复码保存到安全地方（密码管理器、加密笔记或封存信封等）。每个恢复码仅可使用一次；如果你失去身份验证器访问权限，将通过它们恢复登录。它们不会再次显示。",
              codePlaceholder: "首个 6 位动态码",
              submit: "验证并完成设置",
              submitting: "验证中...",
              cancel: "取消并切换登录账号",
              failed: "动态码无效或已过期，请输入下一个验证器显示的动态码。",
              expired: "设置会话已过期，请重新登录后重试。",
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
        // A5 — super_admin needs to enroll a TOTP factor before
        // we issue a session. Stash the grant; the wizard panel
        // calls /enroll-start lazily when the user clicks
        // "Begin setup" so the password screen does not flash a
        // big block of secrets if the user wandered off.
        if (outcome.kind === "enrollment_required") {
          setEnrollGrant(outcome.grant);
          setEnrollData(null);
          setEnrollCode("");
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

  async function handleBeginEnrollment() {
    if (!enrollGrant) return;
    setSubmitting(true);
    setError(null);
    try {
      const data = await startTwoFAEnrollment(enrollGrant);
      setEnrollData(data);
    } catch (err) {
      const message = err instanceof ApiError && err.status === 401
        ? copy.enroll.expired
        : formatApiError(err, copy.enroll.failed);
      setError(message);
      if (err instanceof ApiError && err.status === 401) {
        handleCancelEnrollment();
      }
    } finally {
      setSubmitting(false);
    }
  }

  async function handleSubmitEnrollment(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!enrollGrant) return;
    setSubmitting(true);
    setError(null);
    try {
      await completeTwoFAEnrollment(enrollGrant, enrollCode.trim());
      navigate(redirectTo, { replace: true });
    } catch (err) {
      const message = err instanceof ApiError && err.status === 401
        ? copy.enroll.failed
        : formatApiError(err, copy.enroll.failed);
      setError(message);
    } finally {
      setSubmitting(false);
    }
  }

  function handleCancelEnrollment() {
    setEnrollGrant(null);
    setEnrollData(null);
    setEnrollCode("");
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
        {enrollGrant ? (
          <div>
            <div className="mb-7 flex items-center gap-3">
              <MascotAvatar role="risk" size={56} animated />
              <div>
                <p className="text-[11px] font-semibold uppercase tracking-[0.16em] text-sage-700">
                  FundAI · A5
                </p>
                <h1 className="mt-1 text-2xl font-extrabold text-ink-900">
                  {copy.enroll.introTitle}
                </h1>
              </div>
            </div>
            {!enrollData ? (
              <>
                <p className="mb-6 text-sm leading-6 text-ink-300">{copy.enroll.introBody}</p>
                {error ? <div className={`${errorBox} mb-4`}>{error}</div> : null}
                <BlackPillButton
                  type="button"
                  size="lg"
                  block
                  onClick={handleBeginEnrollment}
                  disabled={submitting}
                >
                  {submitting ? copy.enroll.starting : copy.enroll.introCTA}
                </BlackPillButton>
                <button
                  type="button"
                  onClick={handleCancelEnrollment}
                  className="mt-3 w-full rounded-full bg-cream-50 px-4 py-3 text-sm font-medium text-ink-300 ring-1 ring-ink-100 transition hover:text-ink-700"
                >
                  {copy.enroll.cancel}
                </button>
              </>
            ) : (
              <form className="space-y-5" onSubmit={handleSubmitEnrollment}>
                <div>
                  <p className="text-sm font-semibold text-ink-700">{copy.enroll.qrTitle}</p>
                  <p className="mt-1 text-sm leading-6 text-ink-300">{copy.enroll.qrBody}</p>
                </div>
                <label className="block text-xs font-medium uppercase tracking-wide text-ink-300">
                  {copy.enroll.uriLabel}
                  <textarea
                    readOnly
                    value={enrollData.provisioningUri}
                    onFocus={(e) => e.currentTarget.select()}
                    rows={3}
                    className={`${fieldClass} mt-2 break-all font-mono text-[11px] leading-5`}
                  />
                </label>
                <label className="block text-xs font-medium uppercase tracking-wide text-ink-300">
                  {copy.enroll.secretLabel}
                  <input
                    readOnly
                    value={enrollData.secret}
                    onFocus={(e) => e.currentTarget.select()}
                    className={`${fieldClass} mt-2 font-mono text-sm`}
                  />
                </label>
                <div className="rounded-2xl bg-cream-50 p-4 ring-1 ring-ink-100">
                  <p className="text-sm font-semibold text-ink-700">{copy.enroll.recoveryTitle}</p>
                  <p className="mt-1 text-xs leading-5 text-ink-300">{copy.enroll.recoveryBody}</p>
                  <ul className="mt-3 grid grid-cols-2 gap-2 font-mono text-[12px] text-ink-900">
                    {enrollData.recoveryCodes.map((c) => (
                      <li key={c} className="rounded-md bg-cream-0 px-2 py-1 ring-1 ring-ink-100">{c}</li>
                    ))}
                  </ul>
                </div>
                <input
                  autoFocus
                  type="text"
                  inputMode="numeric"
                  maxLength={8}
                  value={enrollCode}
                  onChange={(e) => setEnrollCode(e.target.value.replace(/[^0-9]/g, ""))}
                  placeholder={copy.enroll.codePlaceholder}
                  className={`${fieldClass} text-center font-mono text-2xl tracking-widest`}
                />
                {error ? <div className={errorBox}>{error}</div> : null}
                <BlackPillButton
                  type="submit"
                  size="lg"
                  block
                  disabled={submitting || enrollCode.length < 6}
                >
                  {submitting ? copy.enroll.submitting : copy.enroll.submit}
                </BlackPillButton>
                <button
                  type="button"
                  onClick={handleCancelEnrollment}
                  className="w-full rounded-full bg-cream-50 px-4 py-3 text-sm font-medium text-ink-300 ring-1 ring-ink-100 transition hover:text-ink-700"
                >
                  {copy.enroll.cancel}
                </button>
              </form>
            )}
          </div>
        ) : twoFAChallenge ? (
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
