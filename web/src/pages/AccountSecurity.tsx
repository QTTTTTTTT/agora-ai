import React, { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import {
  ApiError,
  changePassword,
  disableTwoFA,
  formatApiError,
  getTwoFAStatus,
  requestEmailVerification,
  setupTwoFA,
  TwoFASetupResponse,
  TwoFAStatusResponse,
  verifyTwoFA,
} from "../lib/api";
import { useAppPreferences } from "../lib/preferences";

const AccountSecurity: React.FC = () => {
  const { language } = useAppPreferences();
  const [oldPassword, setOldPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);
  const [resending, setResending] = useState(false);
  // Split resend feedback into success / error so error responses
  // don't paint into the emerald success panel (and vice versa).
  const [resendSuccess, setResendSuccess] = useState<string | null>(null);
  const [resendError, setResendError] = useState<string | null>(null);

  // P0-6 — 2FA state. status is loaded on mount + after every
  // mutation so the UI reflects the persisted truth without a
  // round-trip to a refresh button. setup is the in-flight enrol
  // payload (QR / recovery codes) shown EXACTLY ONCE — once the
  // user verifies the first code we drop it.
  const [twoFAStatus, setTwoFAStatus] = useState<TwoFAStatusResponse | null>(null);
  const [twoFASetup, setTwoFASetup] = useState<TwoFASetupResponse | null>(null);
  const [twoFACode, setTwoFACode] = useState("");
  const [twoFAError, setTwoFAError] = useState<string | null>(null);
  const [twoFAInfo, setTwoFAInfo] = useState<string | null>(null);
  const [twoFABusy, setTwoFABusy] = useState(false);
  // Disable form fields. Password is mandatory; the user picks
  // between TOTP code and recovery code.
  const [disablePwd, setDisablePwd] = useState("");
  const [disableCode, setDisableCode] = useState("");
  const [disableRecovery, setDisableRecovery] = useState("");
  const [disableMode, setDisableMode] = useState<"code" | "recovery">("code");

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            title: "Account security",
            subtitle: "Rotate your password, re-issue email verification, and review high-risk operations from one place.",
            sections: { password: "Change password", verification: "Re-send email verification", twoFA: "Two-factor authentication" },
            labels: { old: "Current password", new: "New password", confirm: "Confirm new password" },
            placeholders: { old: "Current password", new: "At least 8 characters", confirm: "Re-enter the new password" },
            actions: {
              changing: "Updating...",
              change: "Update password",
              resending: "Sending...",
              resend: "Send a new verification code",
              backHome: "Back to console",
            },
            errors: {
              missingOld: "Enter your current password to confirm the change.",
              shortPassword: "Password must be at least 8 characters.",
              passwordMismatch: "Passwords do not match.",
              changeFailed: "Could not change password",
              resendFailed: "Could not resend verification",
            },
            successPassword: "Password updated. A security notice has been emailed to you.",
            verificationDescription:
              "If you didn't receive the original verification email, request a new code here. The verify form is on /verify-email.",
            resendInfo: "Code sent. Check your inbox (and spam).",
            twoFA: {
              statusLoading: "Loading 2FA status...",
              enabled: "Two-factor authentication is ENABLED.",
              disabled: "Two-factor authentication is NOT enabled. Setting it up adds a second factor on top of your password.",
              enableButton: "Set up 2FA",
              disableButton: "Disable 2FA",
              cancelEnrol: "Cancel setup",
              setupIntro:
                "1. Open your authenticator app (Google Authenticator, 1Password, Authy, ...). 2. Scan the QR or paste the secret. 3. Enter the 6-digit code below.",
              scanQR: "Scan this QR code",
              manualSecret: "Or enter the secret manually:",
              recoveryHeader: "Recovery codes — save these somewhere safe",
              recoveryDescription:
                "Each code can be used ONCE if you lose your authenticator. They are shown EXACTLY ONCE — copy them now.",
              codePlaceholder: "6-digit code",
              verifyButton: "Verify and enable",
              verifying: "Verifying...",
              passwordPlaceholder: "Current password",
              recoveryCodePlaceholder: "Recovery code",
              modeCode: "Use authenticator code",
              modeRecovery: "Use a recovery code",
              disableExplain: "Disabling 2FA requires both your password AND a current code (or one of your recovery codes).",
              disableConfirm: "Disable 2FA",
              disabling: "Disabling...",
              disabledNotice: "2FA disabled. You can re-enable it any time.",
              enabledNotice: "2FA enabled successfully.",
              enrolFailed: "Could not start 2FA setup",
              verifyFailed: "Could not verify 2FA code",
              disableFailed: "Could not disable 2FA",
              statusFailed: "Could not load 2FA status",
              copyHint: "(click to copy)",
              copied: "Copied",
              lastVerified: (when: string) => `Last verified: ${when}`,
            },
          }
        : {
            title: "账户安全",
            subtitle: "更换密码、重发邮箱验证码以及查看高风险操作，全部在这里一站完成。",
            sections: { password: "修改密码", verification: "重新发送邮箱验证码", twoFA: "二次验证（2FA）" },
            labels: { old: "当前密码", new: "新密码", confirm: "确认新密码" },
            placeholders: { old: "当前密码", new: "至少 8 位", confirm: "请再次输入新密码" },
            actions: {
              changing: "更新中...",
              change: "更新密码",
              resending: "发送中...",
              resend: "发送新的验证码",
              backHome: "返回控制台",
            },
            errors: {
              missingOld: "请输入当前密码以确认操作。",
              shortPassword: "密码长度至少为 8 位。",
              passwordMismatch: "两次输入的密码不一致。",
              changeFailed: "密码修改失败",
              resendFailed: "验证码发送失败",
            },
            successPassword: "密码已更新，安全通知已发送至您的邮箱。",
            verificationDescription:
              "如果未收到初次邮箱验证邮件，可在此重新申请验证码，然后到 /verify-email 页面完成验证。",
            resendInfo: "验证码已发送，请前往邮箱（含垃圾邮件）查收。",
            twoFA: {
              statusLoading: "正在加载二次验证状态...",
              enabled: "二次验证已开启。",
              disabled: "二次验证未启用。开启后登录时除密码外还需输入动态验证码。",
              enableButton: "开启二次验证",
              disableButton: "关闭二次验证",
              cancelEnrol: "取消设置",
              setupIntro:
                "1. 打开身份验证器 App（Google Authenticator / 1Password / Authy 等）。2. 扫描二维码或手动输入密钥。3. 输入下方 6 位验证码完成绑定。",
              scanQR: "扫描下方二维码",
              manualSecret: "或手动输入密钥：",
              recoveryHeader: "恢复码 — 请妥善保存",
              recoveryDescription:
                "每个恢复码仅可使用一次，用于丢失身份验证器时登录。本次显示后将不再展示，请立即复制保存。",
              codePlaceholder: "6 位验证码",
              verifyButton: "验证并启用",
              verifying: "验证中...",
              passwordPlaceholder: "当前密码",
              recoveryCodePlaceholder: "恢复码",
              modeCode: "使用验证器代码",
              modeRecovery: "使用恢复码",
              disableExplain: "关闭二次验证需要同时提供密码与一次有效验证码（或恢复码）。",
              disableConfirm: "确认关闭",
              disabling: "处理中...",
              disabledNotice: "二次验证已关闭，可随时重新开启。",
              enabledNotice: "二次验证已成功启用。",
              enrolFailed: "无法开始二次验证设置",
              verifyFailed: "二次验证代码校验失败",
              disableFailed: "关闭二次验证失败",
              statusFailed: "二次验证状态加载失败",
              copyHint: "（点击复制）",
              copied: "已复制",
              lastVerified: (when: string) => `上次验证：${when}`,
            },
          },
    [language],
  );

  async function handleChangePassword(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(null);
    setSuccess(null);
    if (!oldPassword.trim()) {
      setError(copy.errors.missingOld);
      return;
    }
    if (newPassword.trim().length < 8) {
      setError(copy.errors.shortPassword);
      return;
    }
    if (newPassword !== confirmPassword) {
      setError(copy.errors.passwordMismatch);
      return;
    }
    setSubmitting(true);
    try {
      await changePassword(oldPassword, newPassword);
      setSuccess(copy.successPassword);
      setOldPassword("");
      setNewPassword("");
      setConfirmPassword("");
    } catch (err) {
      const message = err instanceof ApiError ? formatApiError(err, copy.errors.changeFailed) : copy.errors.changeFailed;
      setError(message);
    } finally {
      setSubmitting(false);
    }
  }

  async function handleResendVerification() {
    setResendSuccess(null);
    setResendError(null);
    setResending(true);
    try {
      await requestEmailVerification();
      setResendSuccess(copy.resendInfo);
    } catch (err) {
      const message = err instanceof ApiError ? formatApiError(err, copy.errors.resendFailed) : copy.errors.resendFailed;
      setResendError(message);
    } finally {
      setResending(false);
    }
  }

  // P0-6 — load status on mount. We tolerate failures: a 404 / 503
  // (TOTP_ENCRYPTION_KEY not set) leaves the section visible but
  // disabled, and we surface the error inline so the operator can
  // see what's wrong rather than guessing.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const s = await getTwoFAStatus();
        if (!cancelled) setTwoFAStatus(s);
      } catch (err) {
        if (cancelled) return;
        if (err instanceof ApiError && (err.status === 404 || err.status === 405)) {
          // 2FA endpoint not registered (no TOTP_ENCRYPTION_KEY).
          setTwoFAStatus({ enabled: false, enrolmentPending: false });
          return;
        }
        const message = err instanceof ApiError ? formatApiError(err, copy.twoFA.statusFailed) : copy.twoFA.statusFailed;
        setTwoFAError(message);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [copy.twoFA.statusFailed]);

  async function handleStartTwoFASetup() {
    setTwoFAError(null);
    setTwoFAInfo(null);
    setTwoFABusy(true);
    try {
      const setup = await setupTwoFA();
      setTwoFASetup(setup);
      setTwoFACode("");
    } catch (err) {
      const message = err instanceof ApiError ? formatApiError(err, copy.twoFA.enrolFailed) : copy.twoFA.enrolFailed;
      setTwoFAError(message);
    } finally {
      setTwoFABusy(false);
    }
  }

  async function handleVerifyTwoFA() {
    if (!twoFACode.trim()) return;
    setTwoFAError(null);
    setTwoFABusy(true);
    try {
      await verifyTwoFA(twoFACode.trim());
      setTwoFASetup(null);
      setTwoFACode("");
      setTwoFAInfo(copy.twoFA.enabledNotice);
      // refresh status so the UI flips to "enabled".
      try {
        const s = await getTwoFAStatus();
        setTwoFAStatus(s);
      } catch {
        // best-effort; the explicit success notice is the source of truth.
      }
    } catch (err) {
      const message = err instanceof ApiError ? formatApiError(err, copy.twoFA.verifyFailed) : copy.twoFA.verifyFailed;
      setTwoFAError(message);
    } finally {
      setTwoFABusy(false);
    }
  }

  function handleCancelTwoFASetup() {
    setTwoFASetup(null);
    setTwoFACode("");
    setTwoFAError(null);
  }

  async function handleDisableTwoFA() {
    if (!disablePwd.trim()) return;
    if (disableMode === "code" && !disableCode.trim()) return;
    if (disableMode === "recovery" && !disableRecovery.trim()) return;
    setTwoFAError(null);
    setTwoFABusy(true);
    try {
      await disableTwoFA({
        password: disablePwd,
        code: disableMode === "code" ? disableCode.trim() : undefined,
        recoveryCode: disableMode === "recovery" ? disableRecovery.trim() : undefined,
      });
      setDisablePwd("");
      setDisableCode("");
      setDisableRecovery("");
      setTwoFAInfo(copy.twoFA.disabledNotice);
      setTwoFAStatus({ enabled: false, enrolmentPending: false });
    } catch (err) {
      const message = err instanceof ApiError ? formatApiError(err, copy.twoFA.disableFailed) : copy.twoFA.disableFailed;
      setTwoFAError(message);
    } finally {
      setTwoFABusy(false);
    }
  }

  function copyToClipboard(text: string) {
    if (!navigator.clipboard) return;
    navigator.clipboard.writeText(text).catch(() => undefined);
  }

  return (
    <div className="min-h-screen bg-slate-950 px-6 py-12 text-white">
      <div className="mx-auto w-full max-w-3xl">
        <div className="mb-10">
          <p className="text-sm font-medium text-indigo-300">FundAI</p>
          <h1 className="mt-2 text-3xl font-semibold">{copy.title}</h1>
          <p className="mt-3 text-sm leading-6 text-slate-300">{copy.subtitle}</p>
        </div>

        <section className="mb-8 rounded-3xl border border-white/10 bg-white/5 p-8 shadow-xl backdrop-blur">
          <h2 className="text-xl font-semibold text-slate-100">{copy.sections.password}</h2>
          <form className="mt-6 space-y-5" onSubmit={handleChangePassword}>
            <label className="block text-sm font-medium text-slate-200">
              {copy.labels.old}
              <input
                type="password"
                value={oldPassword}
                onChange={(event) => setOldPassword(event.target.value)}
                placeholder={copy.placeholders.old}
                autoComplete="current-password"
                className="mt-2 w-full rounded-2xl border border-white/10 bg-slate-900/80 px-4 py-3 text-sm text-white outline-none transition focus:border-indigo-400 focus:ring-2 focus:ring-indigo-500/40"
              />
            </label>
            <label className="block text-sm font-medium text-slate-200">
              {copy.labels.new}
              <input
                type="password"
                value={newPassword}
                onChange={(event) => setNewPassword(event.target.value)}
                placeholder={copy.placeholders.new}
                autoComplete="new-password"
                className="mt-2 w-full rounded-2xl border border-white/10 bg-slate-900/80 px-4 py-3 text-sm text-white outline-none transition focus:border-indigo-400 focus:ring-2 focus:ring-indigo-500/40"
              />
            </label>
            <label className="block text-sm font-medium text-slate-200">
              {copy.labels.confirm}
              <input
                type="password"
                value={confirmPassword}
                onChange={(event) => setConfirmPassword(event.target.value)}
                placeholder={copy.placeholders.confirm}
                autoComplete="new-password"
                className="mt-2 w-full rounded-2xl border border-white/10 bg-slate-900/80 px-4 py-3 text-sm text-white outline-none transition focus:border-indigo-400 focus:ring-2 focus:ring-indigo-500/40"
              />
            </label>

            {error ? (
              <div className="rounded-2xl border border-red-500/30 bg-red-500/10 px-4 py-3 text-sm text-red-200">{error}</div>
            ) : null}
            {success ? (
              <div className="rounded-2xl border border-emerald-500/30 bg-emerald-500/10 px-4 py-3 text-sm text-emerald-200">
                {success}
              </div>
            ) : null}

            <button
              type="submit"
              disabled={submitting}
              className="w-full rounded-2xl bg-indigo-500 px-4 py-3 text-sm font-semibold text-white transition hover:bg-indigo-400 disabled:cursor-not-allowed disabled:opacity-60"
            >
              {submitting ? copy.actions.changing : copy.actions.change}
            </button>
          </form>
        </section>

        <section className="rounded-3xl border border-white/10 bg-white/5 p-8 shadow-xl backdrop-blur">
          <h2 className="text-xl font-semibold text-slate-100">{copy.sections.verification}</h2>
          <p className="mt-3 text-sm leading-6 text-slate-300">{copy.verificationDescription}</p>
          <button
            type="button"
            onClick={handleResendVerification}
            disabled={resending}
            className="mt-5 rounded-2xl border border-indigo-400/40 bg-indigo-500/10 px-4 py-3 text-sm font-medium text-indigo-200 transition hover:bg-indigo-500/20 disabled:cursor-not-allowed disabled:opacity-60"
          >
            {resending ? copy.actions.resending : copy.actions.resend}
          </button>
          {resendSuccess ? (
            <div className="mt-4 rounded-2xl border border-emerald-500/30 bg-emerald-500/10 px-4 py-3 text-sm text-emerald-200">
              {resendSuccess}
            </div>
          ) : null}
          {resendError ? (
            <div className="mt-4 rounded-2xl border border-rose-500/40 bg-rose-500/10 px-4 py-3 text-sm text-rose-200">
              {resendError}
            </div>
          ) : null}
        </section>

        {/* P0-6 — Two-factor authentication */}
        <section className="mt-8 rounded-3xl border border-white/10 bg-white/5 p-8 shadow-xl backdrop-blur">
          <h2 className="text-xl font-semibold text-slate-100">{copy.sections.twoFA}</h2>
          {twoFAStatus === null ? (
            <p className="mt-3 text-sm text-slate-300">{copy.twoFA.statusLoading}</p>
          ) : twoFASetup ? (
            <div className="mt-5 space-y-5">
              <p className="text-sm leading-6 text-slate-300">{copy.twoFA.setupIntro}</p>
              <div className="grid gap-5 md:grid-cols-2">
                <div>
                  <p className="text-xs uppercase tracking-wide text-slate-400">{copy.twoFA.scanQR}</p>
                  <div className="mt-2 rounded-2xl bg-white p-3">
                    <img
                      src={`https://api.qrserver.com/v1/create-qr-code/?size=200x200&data=${encodeURIComponent(twoFASetup.provisioningUri)}`}
                      alt="2FA QR code"
                      className="h-48 w-48"
                    />
                  </div>
                </div>
                <div className="flex flex-col gap-3">
                  <p className="text-xs uppercase tracking-wide text-slate-400">{copy.twoFA.manualSecret}</p>
                  <button
                    type="button"
                    onClick={() => copyToClipboard(twoFASetup.secret)}
                    className="break-all rounded-2xl border border-white/10 bg-slate-900/60 px-4 py-3 text-left font-mono text-sm text-emerald-200 transition hover:border-emerald-400/40"
                    title={copy.twoFA.copyHint}
                  >
                    {twoFASetup.secret}
                  </button>
                  <p className="text-xs text-slate-400">{copy.twoFA.copyHint}</p>
                </div>
              </div>
              <div className="rounded-2xl border border-amber-500/30 bg-amber-500/10 p-5">
                <p className="text-sm font-semibold text-amber-200">{copy.twoFA.recoveryHeader}</p>
                <p className="mt-2 text-xs leading-5 text-amber-100/80">{copy.twoFA.recoveryDescription}</p>
                <ul className="mt-3 grid grid-cols-2 gap-2 text-sm font-mono text-amber-100">
                  {twoFASetup.recoveryCodes.map((c) => (
                    <li key={c} className="rounded-xl border border-amber-500/30 bg-amber-500/5 px-3 py-2">
                      {c}
                    </li>
                  ))}
                </ul>
              </div>
              <div className="flex flex-col gap-3 sm:flex-row sm:items-center">
                <input
                  type="text"
                  inputMode="numeric"
                  maxLength={8}
                  value={twoFACode}
                  onChange={(e) => setTwoFACode(e.target.value.replace(/[^0-9]/g, ""))}
                  placeholder={copy.twoFA.codePlaceholder}
                  className="w-full rounded-2xl border border-white/10 bg-slate-900/80 px-4 py-3 font-mono text-lg tracking-widest text-white outline-none transition focus:border-indigo-400 focus:ring-2 focus:ring-indigo-500/40 sm:w-48"
                />
                <button
                  type="button"
                  disabled={twoFABusy || twoFACode.length < 6}
                  onClick={handleVerifyTwoFA}
                  className="rounded-2xl bg-indigo-500 px-4 py-3 text-sm font-semibold text-white transition hover:bg-indigo-400 disabled:cursor-not-allowed disabled:opacity-60"
                >
                  {twoFABusy ? copy.twoFA.verifying : copy.twoFA.verifyButton}
                </button>
                <button
                  type="button"
                  onClick={handleCancelTwoFASetup}
                  className="rounded-2xl border border-white/10 bg-transparent px-4 py-3 text-sm text-slate-300 transition hover:border-white/20"
                >
                  {copy.twoFA.cancelEnrol}
                </button>
              </div>
            </div>
          ) : twoFAStatus.enabled ? (
            <div className="mt-5 space-y-5">
              <div className="rounded-2xl border border-emerald-500/30 bg-emerald-500/10 px-4 py-3 text-sm text-emerald-200">
                {copy.twoFA.enabled}
                {twoFAStatus.lastVerifiedAt ? (
                  <span className="ml-2 text-xs text-emerald-100/80">{copy.twoFA.lastVerified(twoFAStatus.lastVerifiedAt)}</span>
                ) : null}
              </div>
              <p className="text-sm leading-6 text-slate-300">{copy.twoFA.disableExplain}</p>
              <div className="grid gap-3 md:grid-cols-2">
                <input
                  type="password"
                  value={disablePwd}
                  onChange={(e) => setDisablePwd(e.target.value)}
                  placeholder={copy.twoFA.passwordPlaceholder}
                  autoComplete="current-password"
                  className="rounded-2xl border border-white/10 bg-slate-900/80 px-4 py-3 text-sm text-white outline-none transition focus:border-indigo-400 focus:ring-2 focus:ring-indigo-500/40"
                />
                <div className="flex flex-col gap-2">
                  <div className="flex gap-3 text-xs text-slate-300">
                    <label className="flex items-center gap-1">
                      <input
                        type="radio"
                        checked={disableMode === "code"}
                        onChange={() => setDisableMode("code")}
                      />
                      {copy.twoFA.modeCode}
                    </label>
                    <label className="flex items-center gap-1">
                      <input
                        type="radio"
                        checked={disableMode === "recovery"}
                        onChange={() => setDisableMode("recovery")}
                      />
                      {copy.twoFA.modeRecovery}
                    </label>
                  </div>
                  {disableMode === "code" ? (
                    <input
                      type="text"
                      inputMode="numeric"
                      maxLength={8}
                      value={disableCode}
                      onChange={(e) => setDisableCode(e.target.value.replace(/[^0-9]/g, ""))}
                      placeholder={copy.twoFA.codePlaceholder}
                      className="rounded-2xl border border-white/10 bg-slate-900/80 px-4 py-3 font-mono text-sm tracking-widest text-white outline-none transition focus:border-indigo-400 focus:ring-2 focus:ring-indigo-500/40"
                    />
                  ) : (
                    <input
                      type="text"
                      value={disableRecovery}
                      onChange={(e) => setDisableRecovery(e.target.value)}
                      placeholder={copy.twoFA.recoveryCodePlaceholder}
                      className="rounded-2xl border border-white/10 bg-slate-900/80 px-4 py-3 font-mono text-sm tracking-widest text-white outline-none transition focus:border-indigo-400 focus:ring-2 focus:ring-indigo-500/40"
                    />
                  )}
                </div>
              </div>
              <button
                type="button"
                onClick={handleDisableTwoFA}
                disabled={twoFABusy}
                className="rounded-2xl border border-rose-400/40 bg-rose-500/10 px-4 py-3 text-sm font-semibold text-rose-200 transition hover:bg-rose-500/20 disabled:cursor-not-allowed disabled:opacity-60"
              >
                {twoFABusy ? copy.twoFA.disabling : copy.twoFA.disableConfirm}
              </button>
            </div>
          ) : (
            <div className="mt-5 space-y-4">
              <p className="text-sm leading-6 text-slate-300">{copy.twoFA.disabled}</p>
              <button
                type="button"
                onClick={handleStartTwoFASetup}
                disabled={twoFABusy}
                className="rounded-2xl bg-indigo-500 px-4 py-3 text-sm font-semibold text-white transition hover:bg-indigo-400 disabled:cursor-not-allowed disabled:opacity-60"
              >
                {twoFABusy ? copy.actions.changing : copy.twoFA.enableButton}
              </button>
            </div>
          )}
          {twoFAInfo ? (
            <div className="mt-4 rounded-2xl border border-emerald-500/30 bg-emerald-500/10 px-4 py-3 text-sm text-emerald-200">
              {twoFAInfo}
            </div>
          ) : null}
          {twoFAError ? (
            <div className="mt-4 rounded-2xl border border-rose-500/40 bg-rose-500/10 px-4 py-3 text-sm text-rose-200">
              {twoFAError}
            </div>
          ) : null}
        </section>

        <div className="mt-8 text-center text-xs text-slate-400">
          <Link to="/masters" className="transition hover:text-white">
            {copy.actions.backHome}
          </Link>
        </div>
      </div>
    </div>
  );
};

export default AccountSecurity;
