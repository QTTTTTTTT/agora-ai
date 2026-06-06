import React, { useState } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { ApiError, confirmEmailVerification, formatApiError, requestEmailVerification } from "../lib/api";

const VerifyEmail: React.FC = () => {
  // W4-26 — react-i18next migration. Catalog lives in
  // web/src/i18n/locales/{en-US,zh-CN}/verifyEmail.ts.
  const { t } = useTranslation("verifyEmail");
  const [code, setCode] = useState("");
  const [sending, setSending] = useState(false);
  const [verifying, setVerifying] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [info, setInfo] = useState<string | null>(null);
  const [verified, setVerified] = useState(false);
  const [devCode, setDevCode] = useState<string | null>(null);

  async function handleSend() {
    setError(null);
    setInfo(null);
    setSending(true);
    try {
      const response = await requestEmailVerification();
      setInfo(t("sentInfo"));
      setDevCode(response.dev_code ?? null);
    } catch (err) {
      const fallback = t("errors.sendFailed");
      const message = err instanceof ApiError ? formatApiError(err, fallback) : fallback;
      setError(message);
    } finally {
      setSending(false);
    }
  }

  async function handleVerify(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(null);
    setInfo(null);
    const trimmed = code.trim();
    if (trimmed.length !== 6) {
      setError(t("errors.shortCode"));
      return;
    }
    setVerifying(true);
    try {
      await confirmEmailVerification(trimmed);
      setVerified(true);
    } catch (err) {
      const fallback = t("errors.verifyFailed");
      const message = err instanceof ApiError ? formatApiError(err, fallback) : fallback;
      setError(message);
    } finally {
      setVerifying(false);
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

        {verified ? (
          <div className="rounded-2xl border border-emerald-500/30 bg-emerald-500/10 px-4 py-4 text-sm text-emerald-200">
            {t("verified")}
          </div>
        ) : (
          <>
            <button
              type="button"
              onClick={handleSend}
              disabled={sending}
              className="mb-5 w-full rounded-2xl border border-indigo-400/40 bg-indigo-500/10 px-4 py-3 text-sm font-medium text-indigo-200 transition hover:bg-indigo-500/20 disabled:cursor-not-allowed disabled:opacity-60"
            >
              {sending ? t("actions.sending") : t("actions.send")}
            </button>

            <form className="space-y-5" onSubmit={handleVerify}>
              <label className="block text-sm font-medium text-slate-200">
                {t("labels.code")}
                <input
                  value={code}
                  onChange={(event) => setCode(event.target.value)}
                  placeholder={t("placeholders.code")}
                  inputMode="numeric"
                  pattern="[0-9]*"
                  maxLength={6}
                  className="mt-2 w-full rounded-2xl border border-white/10 bg-slate-900/80 px-4 py-3 text-center font-mono text-lg tracking-[0.32em] text-white outline-none transition focus:border-indigo-400 focus:ring-2 focus:ring-indigo-500/40"
                />
              </label>

              {info ? (
                <div className="rounded-2xl border border-emerald-500/30 bg-emerald-500/10 px-4 py-3 text-sm text-emerald-200">{info}</div>
              ) : null}

              {devCode ? (
                <div className="rounded-2xl border border-amber-400/30 bg-amber-500/10 px-4 py-3 text-xs text-amber-200">
                  <p className="font-semibold">{t("devCodeTitle")}</p>
                  <p className="mt-1 font-mono text-base tracking-[0.4em]">{devCode}</p>
                </div>
              ) : null}

              {error ? (
                <div className="rounded-2xl border border-red-500/30 bg-red-500/10 px-4 py-3 text-sm text-red-200">{error}</div>
              ) : null}

              <button
                type="submit"
                disabled={verifying}
                className="w-full rounded-2xl bg-indigo-500 px-4 py-3 text-sm font-semibold text-white transition hover:bg-indigo-400 disabled:cursor-not-allowed disabled:opacity-60"
              >
                {verifying ? t("actions.verifying") : t("actions.verify")}
              </button>
            </form>
          </>
        )}

        <div className="mt-6 text-center text-xs text-slate-400">
          <Link to="/companies" className="transition hover:text-white">
            {t("actions.backHome")}
          </Link>
        </div>
      </div>
    </div>
  );
};

export default VerifyEmail;
