import React, { useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { ApiError, confirmPasswordReset, formatApiError } from "../lib/api";

const ResetPassword: React.FC = () => {
  // W4-26 — react-i18next migration. Catalog lives in
  // web/src/i18n/locales/{en-US,zh-CN}/resetPassword.ts.
  const { t } = useTranslation("resetPassword");
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

  async function handleSubmit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(null);

    if (!token.trim()) {
      setError(t("errors.missingToken"));
      return;
    }
    if (newPassword.trim().length < 8) {
      setError(t("errors.shortPassword"));
      return;
    }
    if (newPassword !== confirmPassword) {
      setError(t("errors.passwordMismatch"));
      return;
    }

    setSubmitting(true);
    try {
      await confirmPasswordReset(token.trim(), newPassword);
      setError(null);
      setSuccess(t("successDetail"));
      window.setTimeout(() => navigate("/login", { replace: true }), 1500);
    } catch (err) {
      const fallback = t("errors.failed");
      const message = err instanceof ApiError ? formatApiError(err, fallback) : fallback;
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
          <h1 className="mt-2 text-3xl font-semibold">{t("title")}</h1>
          <p className="mt-3 text-sm leading-6 text-slate-300">{t("subtitle")}</p>
        </div>

        <form className="space-y-5" onSubmit={handleSubmit}>
          <label className="block text-sm font-medium text-slate-200">
            {t("labels.token")}
            <input
              value={token}
              onChange={(event) => setToken(event.target.value)}
              placeholder={t("placeholders.token")}
              className="mt-2 w-full rounded-2xl border border-white/10 bg-slate-900/80 px-4 py-3 font-mono text-xs text-white outline-none transition focus:border-indigo-400 focus:ring-2 focus:ring-indigo-500/40"
            />
          </label>

          <label className="block text-sm font-medium text-slate-200">
            {t("labels.password")}
            <input
              type="password"
              value={newPassword}
              onChange={(event) => setNewPassword(event.target.value)}
              placeholder={t("placeholders.password")}
              autoComplete="new-password"
              className="mt-2 w-full rounded-2xl border border-white/10 bg-slate-900/80 px-4 py-3 text-sm text-white outline-none transition focus:border-indigo-400 focus:ring-2 focus:ring-indigo-500/40"
            />
          </label>

          <label className="block text-sm font-medium text-slate-200">
            {t("labels.confirm")}
            <input
              type="password"
              value={confirmPassword}
              onChange={(event) => setConfirmPassword(event.target.value)}
              placeholder={t("placeholders.confirm")}
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
            {submitting ? t("actions.submitting") : t("actions.submit")}
          </button>
        </form>

        <div className="mt-6 text-center text-xs text-slate-400">
          <Link to="/login" className="transition hover:text-white">
            {t("actions.backLogin")}
          </Link>
        </div>
      </div>
    </div>
  );
};

export default ResetPassword;
