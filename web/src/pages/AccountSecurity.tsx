import React, { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { ApiError, changePassword, formatApiError, requestEmailVerification } from "../lib/api";
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

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            title: "Account security",
            subtitle: "Rotate your password, re-issue email verification, and review high-risk operations from one place.",
            sections: { password: "Change password", verification: "Re-send email verification" },
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
          }
        : {
            title: "账户安全",
            subtitle: "更换密码、重发邮箱验证码以及查看高风险操作，全部在这里一站完成。",
            sections: { password: "修改密码", verification: "重新发送邮箱验证码" },
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

        <div className="mt-8 text-center text-xs text-slate-400">
          <Link to="/companies" className="transition hover:text-white">
            {copy.actions.backHome}
          </Link>
        </div>
      </div>
    </div>
  );
};

export default AccountSecurity;
