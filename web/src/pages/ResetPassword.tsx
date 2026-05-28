import React, { useMemo, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { ApiError, confirmPasswordReset, formatApiError } from "../lib/api";
import { useAppPreferences } from "../lib/preferences";

const ResetPassword: React.FC = () => {
  const { language } = useAppPreferences();
  const [params] = useSearchParams();
  const navigate = useNavigate();

  const initialToken = params.get("token") ?? "";
  const [token, setToken] = useState(initialToken);
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Separate success state so we don't reuse the error banner styling
  // for "password updated" — earlier UI showed the success message in
  // the same indigo "error" tone and users thought something failed.
  const [success, setSuccess] = useState<string | null>(null);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            title: "Reset password",
            subtitle: "Paste the token from your email and choose a new password. Token expires 1 hour after issue.",
            labels: { token: "Reset token", password: "New password", confirm: "Confirm new password" },
            placeholders: { token: "Paste token here", password: "At least 8 characters", confirm: "Re-enter the password" },
            actions: { submitting: "Updating...", submit: "Update password", backLogin: "Back to sign in" },
            errors: {
              missingToken: "The reset link is missing the token parameter.",
              shortPassword: "Password must be at least 8 characters.",
              passwordMismatch: "Passwords do not match.",
              failed: "Could not reset password",
            },
            successDetail: "Password updated. Redirecting you to sign in...",
          }
        : {
            title: "重置密码",
            subtitle: "粘贴邮件中的令牌并设置新密码。令牌发出后 1 小时内有效。",
            labels: { token: "重置令牌", password: "新密码", confirm: "确认新密码" },
            placeholders: { token: "请粘贴令牌", password: "至少 8 位", confirm: "请再次输入新密码" },
            actions: { submitting: "更新中...", submit: "更新密码", backLogin: "返回登录" },
            errors: {
              missingToken: "重置链接缺少令牌参数。",
              shortPassword: "密码长度至少为 8 位。",
              passwordMismatch: "两次输入的密码不一致。",
              failed: "密码重置失败",
            },
            successDetail: "密码已更新，正在跳转登录页...",
          },
    [language],
  );

  async function handleSubmit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(null);

    if (!token.trim()) {
      setError(copy.errors.missingToken);
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
      await confirmPasswordReset(token.trim(), newPassword);
      setError(null);
      setSuccess(copy.successDetail);
      window.setTimeout(() => navigate("/login", { replace: true }), 1500);
    } catch (err) {
      const message = err instanceof ApiError ? formatApiError(err, copy.errors.failed) : copy.errors.failed;
      setError(message);
      setSuccess(null);
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-slate-950 px-6 py-12 text-white">
      <div className="w-full max-w-md rounded-3xl border border-white/10 bg-white/5 p-8 shadow-2xl backdrop-blur">
        <div className="mb-8">
          <p className="text-sm font-medium text-indigo-300">FundAI</p>
          <h1 className="mt-2 text-3xl font-semibold">{copy.title}</h1>
          <p className="mt-3 text-sm leading-6 text-slate-300">{copy.subtitle}</p>
        </div>

        <form className="space-y-5" onSubmit={handleSubmit}>
          <label className="block text-sm font-medium text-slate-200">
            {copy.labels.token}
            <input
              value={token}
              onChange={(event) => setToken(event.target.value)}
              placeholder={copy.placeholders.token}
              className="mt-2 w-full rounded-2xl border border-white/10 bg-slate-900/80 px-4 py-3 font-mono text-xs text-white outline-none transition focus:border-indigo-400 focus:ring-2 focus:ring-indigo-500/40"
            />
          </label>

          <label className="block text-sm font-medium text-slate-200">
            {copy.labels.password}
            <input
              type="password"
              value={newPassword}
              onChange={(event) => setNewPassword(event.target.value)}
              placeholder={copy.placeholders.password}
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
            <div className="rounded-2xl border border-rose-500/40 bg-rose-500/10 px-4 py-3 text-sm text-rose-200">{error}</div>
          ) : null}
          {success ? (
            <div className="rounded-2xl border border-emerald-500/40 bg-emerald-500/10 px-4 py-3 text-sm text-emerald-200">{success}</div>
          ) : null}

          <button
            type="submit"
            disabled={submitting}
            className="w-full rounded-2xl bg-indigo-500 px-4 py-3 text-sm font-semibold text-white transition hover:bg-indigo-400 disabled:cursor-not-allowed disabled:opacity-60"
          >
            {submitting ? copy.actions.submitting : copy.actions.submit}
          </button>
        </form>

        <div className="mt-6 text-center text-xs text-slate-400">
          <Link to="/login" className="transition hover:text-white">
            {copy.actions.backLogin}
          </Link>
        </div>
      </div>
    </div>
  );
};

export default ResetPassword;
