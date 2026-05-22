import React, { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
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
  const [status, setStatus] = useState<AccountKYCStatus | null>(null);
  const [form, setForm] = useState(initialForm);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            title: "Identity verification (KYC)",
            subtitle: "Submit or review your verification status. KYC is required before live trading, marketplace publishing, and wallet recharge.",
            back: "Back to companies",
            loading: "Loading KYC status...",
            loadError: "Failed to load KYC status",
            currentStatus: "Current status",
            currentLevel: "Current level",
            submitTitle: "Submit application",
            fullName: "Full legal name",
            level: "Requested level",
            documentType: "Document type",
            documentNumber: "Document number",
            documentUrls: "Document image URLs (optional, one per line)",
            submit: "Submit for review",
            submitting: "Submitting...",
            submitError: "Failed to submit KYC application",
            submitSuccess: "KYC application submitted for admin review.",
            pendingBlock: "You already have a pending KYC application. Please wait for admin review before submitting another one.",
            verifiedBlock: "Your account already has this KYC level or higher. Choose a higher level only if you need an upgrade.",
            fullNameRequired: "Please enter your full legal name.",
            documentRequired: "Please enter your document number.",
            history: "Application history",
            noHistory: "No KYC applications yet.",
            attachments: "Attachments",
            rejectionReason: "Rejection reason",
            submittedAt: "Submitted",
          }
        : {
            title: "实名认证（KYC）",
            subtitle: "提交或查看当前认证状态。实盘交易、市场发布与钱包充值前需要完成 KYC。",
            back: "返回公司列表",
            loading: "正在加载 KYC 状态...",
            loadError: "加载 KYC 状态失败",
            currentStatus: "当前状态",
            currentLevel: "当前等级",
            submitTitle: "提交认证申请",
            fullName: "真实姓名",
            level: "申请等级",
            documentType: "证件类型",
            documentNumber: "证件号码",
            documentUrls: "证件图片 URL（可选，每行一个）",
            submit: "提交审核",
            submitting: "提交中...",
            submitError: "提交 KYC 申请失败",
            submitSuccess: "KYC 申请已提交，等待管理员审核。",
            pendingBlock: "你已有待审核的 KYC 申请，请等待管理员处理后再提交新的申请。",
            verifiedBlock: "当前账号已具备该等级或更高等级认证；如需升级，请选择更高等级。",
            fullNameRequired: "请输入真实姓名。",
            documentRequired: "请输入证件号码。",
            history: "申请历史",
            noHistory: "暂无 KYC 申请记录。",
            attachments: "附件",
            rejectionReason: "拒绝原因",
            submittedAt: "提交于",
          },
    [language],
  );

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setStatus(await fetchAccountKYCStatus());
      await fetchSession().catch(() => undefined);
    } catch (err) {
      setError(formatApiError(err, copy.loadError));
    } finally {
      setLoading(false);
    }
  }, [copy.loadError]);

  useEffect(() => {
    void load();
  }, [load]);

  const submissionBlockedReason = useMemo(() => {
    const currentStatus = status?.kyc_status?.trim() ?? "";
    const hasPendingApplication = (status?.applications ?? []).some((application) => application.status === "pending");
    if (currentStatus === "pending" || hasPendingApplication) {
      return copy.pendingBlock;
    }
    if (currentStatus === "verified" && kycLevelRank(status?.kyc_level) >= kycLevelRank(form.kyc_level)) {
      return copy.verifiedBlock;
    }
    return "";
  }, [copy.pendingBlock, copy.verifiedBlock, form.kyc_level, status]);

  const handleSubmit = useCallback(async () => {
    if (submissionBlockedReason) {
      setError(submissionBlockedReason);
      setSuccess(null);
      return;
    }
    if (!form.full_name.trim()) {
      setError(copy.fullNameRequired);
      setSuccess(null);
      return;
    }
    if (!form.id_document_number.trim()) {
      setError(copy.documentRequired);
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
      setSuccess(copy.submitSuccess);
      setForm(initialForm);
      await load();
    } catch (err) {
      setError(formatApiError(err, copy.submitError));
    } finally {
      setSaving(false);
    }
  }, [copy.documentRequired, copy.fullNameRequired, copy.submitError, copy.submitSuccess, form, load, submissionBlockedReason]);

  return (
    <div className="min-h-screen bg-gray-50 px-4 py-8">
      <main className="mx-auto max-w-5xl space-y-6">
        <section className="rounded-2xl border border-gray-200 bg-white px-6 py-6 shadow-sm">
          <Link to="/companies" className="text-sm font-medium text-indigo-600 hover:text-indigo-700">← {copy.back}</Link>
          <h1 className="mt-4 text-3xl font-bold text-gray-900">{copy.title}</h1>
          <p className="mt-2 max-w-3xl text-sm text-gray-500">{copy.subtitle}</p>
        </section>

        {loading ? (
          <div className="rounded-2xl border border-gray-200 bg-white p-8 text-sm text-gray-500 shadow-sm">{copy.loading}</div>
        ) : (
          <div className="grid grid-cols-1 gap-6 lg:grid-cols-[360px_minmax(0,1fr)]">
            <aside className="space-y-4">
              <section className="rounded-2xl border border-gray-200 bg-white px-5 py-5 shadow-sm">
                <p className="text-xs font-semibold uppercase tracking-wider text-gray-500">{copy.currentStatus}</p>
                <span className={`mt-3 inline-flex rounded-full px-3 py-1 text-sm font-medium ${statusTone(status?.kyc_status)}`}>{status?.kyc_status ?? "-"}</span>
                <p className="mt-4 text-xs font-semibold uppercase tracking-wider text-gray-500">{copy.currentLevel}</p>
                <p className="mt-2 text-sm font-medium text-gray-900">{status?.kyc_level ?? "-"}</p>
              </section>

              <section className="rounded-2xl border border-gray-200 bg-white px-5 py-5 shadow-sm">
                <h2 className="text-lg font-semibold text-gray-900">{copy.history}</h2>
                <div className="mt-4 space-y-3">
                  {(status?.applications ?? []).length === 0 ? (
                    <p className="text-sm text-gray-500">{copy.noHistory}</p>
                  ) : (
                    (status?.applications ?? []).map((application: AccountKYCApplication) => (
                      <article key={application.id} className="rounded-xl border border-gray-200 bg-gray-50 p-3 text-sm">
                        <div className="flex items-center justify-between gap-2">
                          <span className={`rounded-full px-2.5 py-1 text-[11px] font-medium ${statusTone(application.status)}`}>{application.status}</span>
                          <span className="text-xs text-gray-500">{application.kyc_level}</span>
                        </div>
                        <p className="mt-2 text-xs text-gray-500">{copy.submittedAt} · {formatDateTimeForLanguage(application.created_at, language)}</p>
                        {(application.document_image_urls ?? []).length > 0 ? (
                          <div className="mt-2 text-xs text-gray-500">
                            <span>{copy.attachments}: </span>
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
                        {application.rejection_reason ? <p className="mt-2 text-xs text-rose-600">{copy.rejectionReason}: {application.rejection_reason}</p> : null}
                      </article>
                    ))
                  )}
                </div>
              </section>
            </aside>

            <section className="rounded-2xl border border-gray-200 bg-white px-6 py-6 shadow-sm">
              <h2 className="text-xl font-semibold text-gray-900">{copy.submitTitle}</h2>
              {submissionBlockedReason ? (
                <div className="mt-4 rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-800">
                  {submissionBlockedReason}
                </div>
              ) : null}
              <div className="mt-5 grid grid-cols-1 gap-4 sm:grid-cols-2">
                <label className="block text-sm font-medium text-gray-700">
                  {copy.fullName}
                  <input value={form.full_name} onChange={(event) => setForm((current) => ({ ...current, full_name: event.target.value }))} className="mt-2 w-full rounded-xl border border-gray-200 px-4 py-2.5 text-sm outline-none focus:border-indigo-500" />
                </label>
                <label className="block text-sm font-medium text-gray-700">
                  {copy.level}
                  <select value={form.kyc_level} onChange={(event) => setForm((current) => ({ ...current, kyc_level: event.target.value }))} className="mt-2 w-full rounded-xl border border-gray-200 px-4 py-2.5 text-sm outline-none focus:border-indigo-500">
                    <option value="tier1_basic">Tier 1 basic</option>
                    <option value="tier2_advanced">Tier 2 advanced</option>
                    <option value="tier3_enterprise">Tier 3 enterprise</option>
                  </select>
                </label>
                <label className="block text-sm font-medium text-gray-700">
                  {copy.documentType}
                  <select value={form.id_document_type} onChange={(event) => setForm((current) => ({ ...current, id_document_type: event.target.value }))} className="mt-2 w-full rounded-xl border border-gray-200 px-4 py-2.5 text-sm outline-none focus:border-indigo-500">
                    <option value="id_card">ID card</option>
                    <option value="passport">Passport</option>
                    <option value="driver_license">Driver license</option>
                  </select>
                </label>
                <label className="block text-sm font-medium text-gray-700">
                  {copy.documentNumber}
                  <input value={form.id_document_number} onChange={(event) => setForm((current) => ({ ...current, id_document_number: event.target.value }))} className="mt-2 w-full rounded-xl border border-gray-200 px-4 py-2.5 text-sm outline-none focus:border-indigo-500" />
                </label>
                <label className="block text-sm font-medium text-gray-700 sm:col-span-2">
                  {copy.documentUrls}
                  <textarea value={form.document_image_urls} onChange={(event) => setForm((current) => ({ ...current, document_image_urls: event.target.value }))} rows={4} className="mt-2 w-full rounded-xl border border-gray-200 px-4 py-2.5 text-sm outline-none focus:border-indigo-500" />
                </label>
              </div>
              <div className="mt-5 flex flex-wrap items-center gap-3">
                <button onClick={() => void handleSubmit()} disabled={saving || Boolean(submissionBlockedReason)} className="rounded-xl bg-indigo-600 px-5 py-2.5 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60">
                  {saving ? copy.submitting : copy.submit}
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
