import React, { useEffect, useState } from "react";
import { getSupportContact, type SupportContactView } from "../lib/api";
import { useAppPreferences } from "../lib/preferences";

/**
 * SupportContactButton — globally-mounted floating "Get help" button.
 *
 * Renders nothing when the platform admin has either disabled the
 * support-contact feature OR not configured any contact channel
 * (no Discord URL AND no QR image), so an out-of-the-box deploy
 * stays uncluttered.
 *
 * Click → opens a small popover anchored above the button containing
 * the configured Discord invite link and / or scan-QR image plus the
 * admin's optional message.
 *
 * Positioning: `fixed bottom-20 right-4` so it sits one row above
 * the PreferenceDock (which lives at `bottom-4 right-4`). Both share
 * `z-40` and stay out of the modal stack.
 */
const SupportContactButton: React.FC = () => {
  const { language } = useAppPreferences();
  const isEnglish = language === "en-US";
  const copy = isEnglish
    ? {
        button: "Need help?",
        title: "Contact us",
        discordCta: "Open our Discord",
        qrHint: "Or scan the QR code",
        close: "Close",
        empty: "No contact channel configured.",
      }
    : {
        button: "遇到问题？",
        title: "联系我们",
        discordCta: "加入 Discord 社群",
        qrHint: "或扫码联系",
        close: "关闭",
        empty: "暂未配置联系渠道",
      };

  const [contact, setContact] = useState<SupportContactView | null>(null);
  const [open, setOpen] = useState(false);

  useEffect(() => {
    let cancelled = false;
    getSupportContact()
      .then((data) => {
        if (!cancelled) setContact(data);
      })
      .catch(() => {
        // Silent fail — the support button is non-critical UX.
        if (!cancelled) setContact(null);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (!contact || !contact.enabled) {
    return null;
  }
  const hasDiscord = contact.discordUrl.trim() !== "";
  const hasQR = contact.qrImageUrl.trim() !== "";
  const hasMessage = contact.message.trim() !== "";
  // Once enabled=true the button always shows so the admin gets
  // immediate visual confirmation that the toggle reached the
  // SPA. When all three channels are blank the popover degrades
  // to the `empty` placeholder copy instead of hiding silently.

  return (
    <div className="fixed bottom-20 right-4 z-40 flex flex-col items-end gap-2">
      {open ? (
        <div className="w-72 rounded-2xl border border-slate-200 bg-white/95 p-4 shadow-xl backdrop-blur dark:border-slate-700 dark:bg-slate-800/95">
          <div className="mb-2 flex items-center justify-between gap-3">
            <h3 className="text-sm font-semibold text-slate-900 dark:text-slate-100">
              {copy.title}
            </h3>
            <button
              type="button"
              onClick={() => setOpen(false)}
              aria-label={copy.close}
              className="rounded-md px-2 py-0.5 text-xs text-slate-500 transition hover:bg-slate-100 hover:text-slate-700 dark:text-slate-400 dark:hover:bg-slate-700 dark:hover:text-slate-200"
            >
              ×
            </button>
          </div>

          {contact.message.trim() ? (
            <p className="mb-3 whitespace-pre-line text-xs text-slate-600 dark:text-slate-300">
              {contact.message}
            </p>
          ) : null}

          {!hasDiscord && !hasQR && !hasMessage ? (
            <p className="mb-2 text-xs text-slate-500 dark:text-slate-400">
              {copy.empty}
            </p>
          ) : null}

          {hasDiscord ? (
            <a
              href={contact.discordUrl}
              target="_blank"
              rel="noopener noreferrer"
              className="mb-3 flex w-full items-center justify-center gap-2 rounded-lg bg-indigo-600 px-3 py-2 text-xs font-medium text-white transition hover:bg-indigo-500"
            >
              <svg viewBox="0 0 24 24" className="h-4 w-4 fill-current" aria-hidden>
                <path d="M20.317 4.369A19.79 19.79 0 0 0 16.558 3a13.5 13.5 0 0 0-.617 1.276 18.27 18.27 0 0 0-5.487 0A12.59 12.59 0 0 0 9.83 3 19.736 19.736 0 0 0 6.07 4.372C2.41 9.84 1.412 15.171 1.91 20.435a19.92 19.92 0 0 0 6.073 3.063c.491-.665.928-1.37 1.305-2.111a12.93 12.93 0 0 1-2.057-.99c.173-.127.342-.26.505-.395a14.21 14.21 0 0 0 12.527 0c.165.137.334.27.506.395a12.86 12.86 0 0 1-2.06.99c.376.74.815 1.446 1.306 2.111a19.91 19.91 0 0 0 6.075-3.063c.585-6.108-1.001-11.397-4.196-16.066ZM9.345 16.05c-1.182 0-2.157-1.083-2.157-2.418 0-1.336.954-2.42 2.157-2.42 1.203 0 2.178 1.084 2.157 2.42 0 1.335-.954 2.418-2.157 2.418Zm5.31 0c-1.182 0-2.157-1.083-2.157-2.418 0-1.336.954-2.42 2.157-2.42 1.203 0 2.178 1.084 2.157 2.42 0 1.335-.953 2.418-2.157 2.418Z" />
              </svg>
              {copy.discordCta}
            </a>
          ) : null}

          {hasQR ? (
            <div>
              {hasDiscord ? (
                <p className="mb-1 text-[11px] uppercase tracking-wide text-slate-500 dark:text-slate-400">
                  {copy.qrHint}
                </p>
              ) : null}
              <div className="flex justify-center rounded-lg border border-slate-200 bg-white p-2 dark:border-slate-600 dark:bg-slate-100">
                <img
                  src={contact.qrImageUrl}
                  alt="contact QR"
                  className="h-40 w-40 object-contain"
                />
              </div>
            </div>
          ) : null}
        </div>
      ) : null}

      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex items-center gap-2 rounded-full bg-slate-900 px-4 py-2 text-xs font-medium text-white shadow-lg transition hover:bg-slate-700 dark:bg-indigo-600 dark:hover:bg-indigo-500"
      >
        <svg viewBox="0 0 24 24" className="h-4 w-4 fill-none stroke-current stroke-2" aria-hidden>
          <path d="M12 18.5a8 8 0 0 1-3.5-.8L4 19l1.3-4.2A8 8 0 1 1 12 18.5Z" />
          <path d="M9.5 10h.01M12 10h.01M14.5 10h.01" strokeLinecap="round" />
        </svg>
        {copy.button}
      </button>
    </div>
  );
};

export default SupportContactButton;
