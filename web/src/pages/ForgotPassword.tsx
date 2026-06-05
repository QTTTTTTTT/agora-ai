import React, { useState } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { ApiError, formatApiError, requestPasswordReset } from "../lib/api";
import {
  composeValidators,
  useFieldValidation,
  validators,
} from "../lib/useFieldValidation";

// U6 reference migration — the first page on the codebase to use the
// `react-i18next` `useTranslation` hook instead of the legacy hand-rolled
// `const copy = useMemo(() => language === 'en-US' ? {...} : {...}, [language])`
// pattern. See web/src/i18n/index.ts for the bootstrap and the
// "MIGRATION GUIDANCE" comment for the recipe to migrate other pages.
//
// Also the reference page for the `useFieldValidation` hook
// (web/src/lib/useFieldValidation.ts) — onBlur-aware field validation
// instead of "wait for submit, then yell". When the user types a typo
// email and tabs away, the inline error appears immediately; on submit,
// the handler asks the field to mark itself touched so any unblurred
// errors surface before the API call runs.

const ForgotPassword: React.FC = () => {
  const { t } = useTranslation("forgotPassword");
  const { t: tValidation } = useTranslation("validation");
  const [email, setEmail] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [submitted, setSubmitted] = useState(false);
  const [devLink, setDevLink] = useState<string | null>(null);

  const emailField = useFieldValidation(
    email,
    composeValidators(
      validators.required(tValidation),
      validators.email(tValidation),
    ),
  );

  async function handleSubmit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setSubmitError(null);

    // Surface any pending field errors (the user might submit without ever
    // blurring the field — e.g. hits Enter while still focused).
    emailField.markTouched();
    if (emailField.error) return;

    setSubmitting(true);
    try {
      const response = await requestPasswordReset(email.trim());
      setSubmitted(true);
      setDevLink(response.dev_reset_link ?? null);
    } catch (err) {
      const fallback = t("errors.failed");
      const message = err instanceof ApiError ? formatApiError(err, fallback) : fallback;
      setSubmitError(message);
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

        {submitted ? (
          <div className="rounded-2xl border border-emerald-500/30 bg-emerald-500/10 px-4 py-4 text-sm leading-6 text-emerald-200">
            <p className="font-semibold text-emerald-100">{t("successTitle")}</p>
            <p className="mt-2 text-emerald-200/80">{t("successBody")}</p>
            {devLink ? (
              <div className="mt-3 rounded-xl border border-emerald-400/30 bg-emerald-900/40 p-3 text-xs text-emerald-100">
                <p className="font-semibold">{t("devLinkTitle")}</p>
                <a className="mt-1 block break-all text-emerald-200 underline" href={devLink}>
                  {devLink}
                </a>
              </div>
            ) : null}
          </div>
        ) : (
          <form className="space-y-5" onSubmit={handleSubmit} noValidate>
            <label className="block text-sm font-medium text-slate-200">
              {t("labels.email")}
              <input
                type="email"
                value={email}
                onChange={(event) => setEmail(event.target.value)}
                onBlur={emailField.onBlur}
                placeholder={t("placeholders.email")}
                autoComplete="email"
                aria-invalid={Boolean(emailField.showError)}
                aria-describedby={emailField.showError ? "email-error" : undefined}
                className={`mt-2 w-full rounded-2xl border bg-slate-900/80 px-4 py-3 text-sm text-white outline-none transition focus:ring-2 ${
                  emailField.showError
                    ? "border-red-500/60 focus:border-red-400 focus:ring-red-500/30"
                    : "border-white/10 focus:border-indigo-400 focus:ring-indigo-500/40"
                }`}
              />
              {emailField.showError ? (
                <p id="email-error" className="mt-2 text-xs text-red-300">
                  {emailField.showError}
                </p>
              ) : null}
            </label>

            {submitError ? (
              <div className="rounded-2xl border border-red-500/30 bg-red-500/10 px-4 py-3 text-sm text-red-200">{submitError}</div>
            ) : null}

            <button
              type="submit"
              disabled={submitting}
              className="w-full rounded-2xl bg-indigo-500 px-4 py-3 text-sm font-semibold text-white transition hover:bg-indigo-400 disabled:cursor-not-allowed disabled:opacity-60"
            >
              {submitting ? t("actions.submitting") : t("actions.submit")}
            </button>
          </form>
        )}

        <div className="mt-6 text-center text-xs text-slate-400">
          <Link to="/login" className="transition hover:text-white">
            {t("actions.backLogin")}
          </Link>
        </div>
      </div>
    </div>
  );
};

export default ForgotPassword;
