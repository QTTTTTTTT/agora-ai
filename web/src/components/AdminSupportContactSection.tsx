import React, { useEffect, useState } from "react";
import {
  formatApiError,
  getSupportContact,
  updateSupportContact,
  type SupportContactInput,
  type SupportContactView,
} from "../lib/api";
import { useAppPreferences } from "../lib/preferences";

/**
 * AdminSupportContactSection — super_admin form for the floating
 * "Get help" button that ships globally to every page in the SPA.
 *
 * One row in support_contact (migration 115). Fields:
 *
 *   - enabled       — master switch; when off the button is hidden
 *   - discordUrl    — http(s) URL the button links to
 *   - qrImageUrl    — image to render under the link (URL or data:image/...)
 *   - message       — optional free-text message shown above both
 */
const AdminSupportContactSection: React.FC = () => {
  const { language } = useAppPreferences();
  const isEn = language === "en-US";
  const copy = isEn
    ? {
        title: "Support contact (floating help button)",
        hint: "Configure the global \"Need help?\" button shown on every page. Disable to hide everywhere.",
        enabled: "Enable button",
        discord: "Discord invite URL",
        qr: "QR image URL (or data:image/...)",
        message: "Optional message (shown above both)",
        save: "Save",
        saving: "Saving...",
        success: "Saved.",
      }
    : {
        title: "联系我们浮钮配置",
        hint: "配置所有页面右下角浮动的「遇到问题？」按钮。关闭则全站隐藏。",
        enabled: "启用按钮",
        discord: "Discord 邀请链接",
        qr: "二维码图片 URL（或 data:image/... 内嵌）",
        message: "可选附加说明（显示在按钮弹出框上方）",
        save: "保存",
        saving: "保存中…",
        success: "已保存。",
      };

  const [draft, setDraft] = useState<SupportContactInput>({
    enabled: false,
    discordUrl: "",
    qrImageUrl: "",
    message: "",
  });
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    getSupportContact()
      .then((data: SupportContactView) => {
        if (cancelled) return;
        setDraft({
          enabled: data.enabled,
          discordUrl: data.discordUrl,
          qrImageUrl: data.qrImageUrl,
          message: data.message,
        });
      })
      .catch((e) => {
        if (!cancelled) setError(formatApiError(e, isEn ? "Load failed" : "加载失败"));
      });
    return () => {
      cancelled = true;
    };
  }, [isEn]);

  const handleSave = async () => {
    setSaving(true);
    setError(null);
    setSuccess(null);
    try {
      const out = await updateSupportContact(draft);
      setDraft({
        enabled: out.enabled,
        discordUrl: out.discordUrl,
        qrImageUrl: out.qrImageUrl,
        message: out.message,
      });
      setSuccess(copy.success);
    } catch (e) {
      setError(formatApiError(e, isEn ? "Save failed" : "保存失败"));
    } finally {
      setSaving(false);
    }
  };

  return (
    <section className="rounded-2xl border border-purple-200 bg-purple-50 px-5 py-5 shadow-sm">
      <div>
        <h2 className="text-lg font-semibold text-purple-900">{copy.title}</h2>
        <p className="mt-2 text-sm text-purple-700">{copy.hint}</p>
      </div>
      <div className="mt-4 space-y-4">
        <label className="flex items-center gap-3 text-sm font-medium text-purple-900">
          <input
            type="checkbox"
            checked={draft.enabled}
            onChange={(e) => setDraft((d) => ({ ...d, enabled: e.target.checked }))}
            className="h-4 w-4"
          />
          <span>{copy.enabled}</span>
        </label>
        <label className="block text-sm font-medium text-purple-900">
          {copy.discord}
          <input
            type="url"
            value={draft.discordUrl}
            placeholder="https://discord.gg/xxxx"
            onChange={(e) => setDraft((d) => ({ ...d, discordUrl: e.target.value }))}
            className="mt-2 w-full rounded-xl border border-purple-200 bg-white px-4 py-2.5 text-sm text-gray-900 outline-none focus:border-purple-500"
          />
        </label>
        <label className="block text-sm font-medium text-purple-900">
          {copy.qr}
          <input
            type="text"
            value={draft.qrImageUrl}
            placeholder="https://... or data:image/png;base64,..."
            onChange={(e) => setDraft((d) => ({ ...d, qrImageUrl: e.target.value }))}
            className="mt-2 w-full rounded-xl border border-purple-200 bg-white px-4 py-2.5 text-sm text-gray-900 outline-none focus:border-purple-500"
          />
        </label>
        {draft.qrImageUrl ? (
          <div className="rounded-xl border border-purple-200 bg-white p-3">
            <img
              src={draft.qrImageUrl}
              alt="QR preview"
              className="mx-auto h-40 w-40 object-contain"
            />
          </div>
        ) : null}
        <label className="block text-sm font-medium text-purple-900">
          {copy.message}
          <textarea
            value={draft.message}
            rows={3}
            onChange={(e) => setDraft((d) => ({ ...d, message: e.target.value }))}
            className="mt-2 w-full rounded-xl border border-purple-200 bg-white px-4 py-2.5 text-sm text-gray-900 outline-none focus:border-purple-500"
          />
        </label>
      </div>
      <div className="mt-4 flex flex-wrap items-center gap-3">
        <button
          onClick={() => void handleSave()}
          disabled={saving}
          className="rounded-xl bg-purple-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-purple-700 disabled:cursor-not-allowed disabled:opacity-60"
        >
          {saving ? copy.saving : copy.save}
        </button>
        {error ? <p className="text-sm text-red-600">{error}</p> : null}
        {success ? <p className="text-sm text-emerald-700">{success}</p> : null}
      </div>
    </section>
  );
};

export default AdminSupportContactSection;
