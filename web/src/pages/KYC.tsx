import React, { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import {
  fetchAccountKYCStatus,
  fetchSession,
  formatApiError,
  submitAccountKYC,
  type AccountKYCApplication,
  type AccountKYCStatus,
} from "../lib/api";
import { formatDateTimeForLanguage, useAppPreferences } from "../lib/preferences";

const initialForm = {
  kyc_level: "tier1_basic",
  full_name: "",
  id_document_type: "id_card",
  id_document_number: "",
  document_image_urls: "",
};

function statusTone(status?: string): string {
  switch ((status ?? "").trim()) {
    case "verified":
    case "approved":
      return "bg-emerald-50 text-emerald-700 ring-1 ring-emerald-200";
    case "pending":
      return "bg-amber-50 text-amber-700 ring-1 ring-amber-200";
    case "rejected":
      return "bg-rose-50 text-rose-700 ring-1 ring-rose-200";
    default:
      return "bg-gray-100 text-gray-700 ring-1 ring-gray-200";
  }
}

function kycLevelRank(level?: string): number {
  switch ((level ?? "").trim()) {
    case "tier3_enterprise":
      return 3;
    case "tier2_advanced":
      return 2;
    case "tier1_basic":
      return 1;
    default:
      return 0;
  }
}

const KYC: React.FC = () => {
  const { language } = useAppPreferences();
  // W4-26 — react-i18next migration. Catalog lives in
  // web/src/i18n/locales/{en-US,zh-CN}/kyc.ts.
  const { t } = useTranslation("kyc");
  const [status, setStatus] = useState<AccountKYCStatus | null>(null);
  const [form, setForm] = useState(initialForm);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setStatus(await fetchAccountKYCStatus());
      await fetchSession().catch(() => undefined);
    } catch (err) {
      setError(formatApiError(err, t("loadError")));
    } finally {
      setLoading(false);
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  const submissionBlockedReason = useMemo(() => {
    const currentStatus = status?.kyc_status?.trim() ?? "";
    const hasPendingApplication = (status?.applications ?? []).some((application) => application.status === "pending");
    if (currentStatus === "pending" || hasPendingApplication) {
      return t("pendingBlock");
    }
    if (currentStatus === "verified" && kycLevelRank(status?.kyc_level) >= kycLevelRank(form.kyc_level)) {
      return t("verifiedBlock");
    }
    return "";
  }, [t, form.kyc_level, status]);

  const handleSubmit = useCallback(async () => {
    if (submissionBlockedReason) {
      setError(submissionBlockedReason);
      setSuccess(null);
      return;
    }
    if (!form.full_name.trim()) {
      setError(t("fullNameRequired"));
      setSuccess(null);
      return;
    }
    if (!form.id_document_number.trim()) {
      setError(t("documentRequired"));
      setSuccess(null);
      return;
    }
    setSaving(true);
    setError(null);
    setSuccess(null);
    try {
      await submitAccountKYC({
        kyc_level: form.kyc_level,
        full_name: form.full_name.trim(),
        id_document_type: form.id_document_type,
        id_document_number: form.id_document_number.trim(),
        document_image_urls: form.document_image_urls.split("\n").map((value) => value.trim()).filter(Boolean),
      });
      setSuccess(t("submitSuccess"));
      setForm(initialForm);
      await load();
    } catch (err) {
      setError(formatApiError(err, t("submitError")));
    } finally {
      setSaving(false);
    }
  }, [t, form, load, submissionBlockedReason]);

  return (
    <div className="min-h-screen bg-gray-50 px-4 py-8">
      <main className="mx-auto max-w-5xl space-y-6">
        <section className="rounded-2xl border border-gray-200 bg-white px-6 py-6 shadow-sm">
          <Link to="/companies" className="text-sm font-medium text-indigo-600 hover:text-indigo-700">← {t("back")}</Link>
          <h1 className="mt-4 text-3xl font-bold text-gray-900">{t("title")}</h1>
          <p className="mt-2 max-w-3xl text-sm text-gray-500">{t("subtitle")}</p>
        </section>

        {loading ? (
          <div className="rounded-2xl border border-gray-200 bg-white p-8 text-sm text-gray-500 shadow-sm">{t("loading")}</div>
        ) : (
          <div className="grid grid-cols-1 gap-6 lg:grid-cols-[360px_minmax(0,1fr)]">
            <aside className="space-y-4">
              <section className="rounded-2xl border border-gray-200 bg-white px-5 py-5 shadow-sm">
                <p className="text-xs font-semibold uppercase tracking-wider text-gray-500">{t("currentStatus")}</p>
                <span className={`mt-3 inline-flex rounded-full px-3 py-1 text-sm font-medium ${statusTone(status?.kyc_status)}`}>{status?.kyc_status ?? "-"}</span>
                <p className="mt-4 text-xs font-semibold uppercase tracking-wider text-gray-500">{t("currentLevel")}</p>
                <p className="mt-2 text-sm font-medium text-gray-900">{status?.kyc_level ?? "-"}</p>
              </section>

              <section className="rounded-2xl border border-gray-200 bg-white px-5 py-5 shadow-sm">
                <h2 className="text-lg font-semibold text-gray-900">{t("history")}</h2>
                <div className="mt-4 space-y-3">
                  {(status?.applications ?? []).length === 0 ? (
                    <p className="text-sm text-gray-500">{t("noHistory")}</p>
                  ) : (
                    (status?.applications ?? []).map((application: AccountKYCApplication) => (
                      <article key={application.id} className="rounded-xl border border-gray-200 bg-gray-50 p-3 text-sm">
                        <div className="flex items-center justify-between gap-2">
                          <span className={`rounded-full px-2.5 py-1 text-[11px] font-medium ${statusTone(application.status)}`}>{application.status}</span>
                          <span className="text-xs text-gray-500">{application.kyc_level}</span>
                        </div>
                        <p className="mt-2 text-xs text-gray-500">{t("submittedAt")} · {formatDateTimeForLanguage(application.created_at, language)}</p>
                        {(application.document_image_urls ?? []).length > 0 ? (
                          <div className="mt-2 text-xs text-gray-500">
                            <span>{t("attachments")}: </span>
                            <div className="mt-1 flex flex-wrap gap-1.5">
                              {(application.document_image_urls ?? []).slice(0, 3).map((url, index) => (
                                <a
                                  key={`${application.id}-${url}-${index}`}
                                  href={url}
                                  target="_blank"
                                  rel="noreferrer"
                                  className="rounded-full bg-indigo-50 px-2 py-1 text-[11px] font-medium text-indigo-700 ring-1 ring-indigo-100 hover:bg-indigo-100"
                                >
                                  #{index + 1}
                                </a>
                              ))}
                              {(application.document_image_urls ?? []).length > 3 ? (
                                <span className="rounded-full bg-gray-100 px-2 py-1 text-[11px] text-gray-500">+{(application.document_image_urls ?? []).length - 3}</span>
                              ) : null}
                            </div>
                          </div>
                        ) : null}
                        {application.rejection_reason ? <p className="mt-2 text-xs text-rose-600">{t("rejectionReason")}: {application.rejection_reason}</p> : null}
                      </article>
                    ))
                  )}
                </div>
              </section>
            </aside>

            <section className="rounded-2xl border border-gray-200 bg-white px-6 py-6 shadow-sm">
              <h2 className="text-xl font-semibold text-gray-900">{t("submitTitle")}</h2>
              {submissionBlockedReason ? (
                <div className="mt-4 rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-800">
                  {submissionBlockedReason}
                </div>
              ) : null}
              <div className="mt-5 grid grid-cols-1 gap-4 sm:grid-cols-2">
                <label className="block text-sm font-medium text-gray-700">
                  {t("fullName")}
                  <input value={form.full_name} onChange={(event) => setForm((current) => ({ ...current, full_name: event.target.value }))} className="mt-2 w-full rounded-xl border border-gray-200 px-4 py-2.5 text-sm outline-none focus:border-indigo-500" />
                </label>
                <label className="block text-sm font-medium text-gray-700">
                  {t("level")}
                  <select value={form.kyc_level} onChange={(event) => setForm((current) => ({ ...current, kyc_level: event.target.value }))} className="mt-2 w-full rounded-xl border border-gray-200 px-4 py-2.5 text-sm outline-none focus:border-indigo-500">
                    <option value="tier1_basic">Tier 1 basic</option>
                    <option value="tier2_advanced">Tier 2 advanced</option>
                    <option value="tier3_enterprise">Tier 3 enterprise</option>
                  </select>
                </label>
                <label className="block text-sm font-medium text-gray-700">
                  {t("documentType")}
                  <select value={form.id_document_type} onChange={(event) => setForm((current) => ({ ...current, id_document_type: event.target.value }))} className="mt-2 w-full rounded-xl border border-gray-200 px-4 py-2.5 text-sm outline-none focus:border-indigo-500">
                    <option value="id_card">ID card</option>
                    <option value="passport">Passport</option>
                    <option value="driver_license">Driver license</option>
                  </select>
                </label>
                <label className="block text-sm font-medium text-gray-700">
                  {t("documentNumber")}
                  <input value={form.id_document_number} onChange={(event) => setForm((current) => ({ ...current, id_document_number: event.target.value }))} className="mt-2 w-full rounded-xl border border-gray-200 px-4 py-2.5 text-sm outline-none focus:border-indigo-500" />
                </label>
                <label className="block text-sm font-medium text-gray-700 sm:col-span-2">
                  {t("documentUrls")}
                  <textarea value={form.document_image_urls} onChange={(event) => setForm((current) => ({ ...current, document_image_urls: event.target.value }))} rows={4} className="mt-2 w-full rounded-xl border border-gray-200 px-4 py-2.5 text-sm outline-none focus:border-indigo-500" />
                </label>
              </div>
              <div className="mt-5 flex flex-wrap items-center gap-3">
                <button onClick={() => void handleSubmit()} disabled={saving || Boolean(submissionBlockedReason)} className="rounded-xl bg-indigo-600 px-5 py-2.5 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60">
                  {saving ? t("submitting") : t("submit")}
                </button>
                {error ? <p className="text-sm text-red-600">{error}</p> : null}
                {success ? <p className="text-sm text-emerald-700">{success}</p> : null}
              </div>
            </section>
          </div>
        )}
      </main>
    </div>
  );
};

export default KYC;
