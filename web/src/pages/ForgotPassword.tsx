import React, { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { ApiError, formatApiError, requestPasswordReset } from "../lib/api";
import { useAppPreferences } from "../lib/preferences";

const ForgotPassword: React.FC = () => {
  const { language } = useAppPreferences();
  const [email, setEmail] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [submitted, setSubmitted] = useState(false);
  const [devLink, setDevLink] = useState<string | null>(null);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            title: "Forgot password",
            subtitle:
              "Enter the email tied to your account. If it matches an active user we'll send a one-time reset link valid for 1 hour.",
            labels: { email: "Account email" },
            placeholders: { email: "you@example.com" },
            actions: {
              submitting: "Sending...",
              submit: "Send reset link",
              backLogin: "Back to sign in",
            },
            errors: {
              invalidEmail: "Enter a valid email address.",
              failed: "Could not request reset",
            },
            successTitle: "Check your inbox",
            successBody:
              "If the email matches an active account, you'll receive a reset link shortly. Don't see it? Check spam and wait 1–2 minutes.",
            devLinkTitle: "Dev mode reset link (SMTP unconfigured):",
          }
        : {
            title: "忘记密码",
            subtitle: "填写注册邮箱，匹配到的活跃账号将收到 1 小时内有效的一次性重置链接。",
            labels: { email: "账号邮箱" },
            placeholders: { email: "you@example.com" },
            actions: {
              submitting: "发送中...",
              submit: "发送重置链接",
              backLogin: "返回登录",
            },
            errors: {
              invalidEmail: "请输入合法的邮箱地址。",
              failed: "重置请求失败",
            },
            successTitle: "请前往邮箱查收",
            successBody: "若该邮箱对应活跃账号，您将很快收到重置链接。若未收到，请检查垃圾邮件或稍候 1–2 分钟重试。",
            devLinkTitle: "开发模式直接重置链接（SMTP 未配置）：",
          },
    [language],
  );

  async function handleSubmit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(null);

    const trimmed = email.trim();
    if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(trimmed)) {
      setError(copy.errors.invalidEmail);
      return;
    }
    setSubmitting(true);
    try {
      const response = await requestPasswordReset(trimmed);
      setSubmitted(true);
      setDevLink(response.dev_reset_link ?? null);
    } catch (err) {
      const message = err instanceof ApiError ? formatApiError(err, copy.errors.failed) : copy.errors.failed;
      setError(message);
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

        {submitted ? (
          <div className="rounded-2xl border border-emerald-500/30 bg-emerald-500/10 px-4 py-4 text-sm leading-6 text-emerald-200">
            <p className="font-semibold text-emerald-100">{copy.successTitle}</p>
            <p className="mt-2 text-emerald-200/80">{copy.successBody}</p>
            {devLink ? (
              <div className="mt-3 rounded-xl border border-emerald-400/30 bg-emerald-900/40 p-3 text-xs text-emerald-100">
                <p className="font-semibold">{copy.devLinkTitle}</p>
                <a className="mt-1 block break-all text-emerald-200 underline" href={devLink}>
                  {devLink}
                </a>
              </div>
            ) : null}
          </div>
        ) : (
          <form className="space-y-5" onSubmit={handleSubmit}>
            <label className="block text-sm font-medium text-slate-200">
              {copy.labels.email}
              <input
                type="email"
                value={email}
                onChange={(event) => setEmail(event.target.value)}
                placeholder={copy.placeholders.email}
                autoComplete="email"
                className="mt-2 w-full rounded-2xl border border-white/10 bg-slate-900/80 px-4 py-3 text-sm text-white outline-none transition focus:border-indigo-400 focus:ring-2 focus:ring-indigo-500/40"
              />
            </label>

            {error ? (
              <div className="rounded-2xl border border-red-500/30 bg-red-500/10 px-4 py-3 text-sm text-red-200">{error}</div>
            ) : null}

            <button
              type="submit"
              disabled={submitting}
              className="w-full rounded-2xl bg-indigo-500 px-4 py-3 text-sm font-semibold text-white transition hover:bg-indigo-400 disabled:cursor-not-allowed disabled:opacity-60"
            >
              {submitting ? copy.actions.submitting : copy.actions.submit}
            </button>
          </form>
        )}

        <div className="mt-6 text-center text-xs text-slate-400">
          <Link to="/login" className="transition hover:text-white">
            {copy.actions.backLogin}
          </Link>
        </div>
      </div>
    </div>
  );
};

export default ForgotPassword;
