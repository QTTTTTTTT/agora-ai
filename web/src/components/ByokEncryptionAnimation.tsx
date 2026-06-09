import React, { useEffect, useState } from "react";

// ByokEncryptionAnimation — Phase D-2.
//
// A 5-frame, ~12-second overlay that visualizes what happens to a
// freshly-pasted API key when the user clicks "Encrypt & save":
//
//   1. INPUT       — sk-…1234 enters our SPA in plaintext
//   2. SHA256      — fingerprint is computed (deterministic, public-safe)
//   3. AES-256-GCM — body is sealed with the server-side env secret
//   4. WRITE       — only the sealed bytes + fingerprint hit Postgres
//   5. CONFIRM     — plaintext is dropped from memory, we're done
//
// Why we built this in-house instead of pulling in framer-motion:
// the page is mostly settings forms — adding a 50kB animation
// library for one 12-second overlay would inflate the BYOK bundle
// pointlessly. CSS keyframes + a state machine handle the timing
// just as cleanly.

interface Props {
  lang: "zh" | "en";
}

const FRAMES_ZH = [
  { id: 1, label: "粘贴明文", body: "你的 sk-… key 进入浏览器内存", icon: "📝" },
  { id: 2, label: "SHA-256 指纹", body: "生成可公开的指纹（不可逆）", icon: "#" },
  { id: 3, label: "AES-256-GCM 加密", body: "用服务器 env 密钥封装", icon: "🔒" },
  { id: 4, label: "写入数据库", body: "只有密文 + 指纹会落盘", icon: "💾" },
  { id: 5, label: "完成 — 明文已丢弃", body: "浏览器与服务器内存都不留明文", icon: "✓" },
];

const FRAMES_EN = [
  { id: 1, label: "Paste plaintext", body: "Your sk-… key enters the browser only", icon: "📝" },
  { id: 2, label: "SHA-256 fingerprint", body: "Public-safe fingerprint (irreversible)", icon: "#" },
  { id: 3, label: "AES-256-GCM seal", body: "Sealed with the server-side env secret", icon: "🔒" },
  { id: 4, label: "Write to database", body: "Only ciphertext + fingerprint hit disk", icon: "💾" },
  { id: 5, label: "Done — plaintext dropped", body: "No plaintext lingers in client or server", icon: "✓" },
];

const FRAME_DURATION_MS = 2400; // 5 frames × 2.4s ≈ 12s

const ByokEncryptionAnimation: React.FC<Props> = ({ lang }) => {
  const frames = lang === "zh" ? FRAMES_ZH : FRAMES_EN;
  const [activeIdx, setActiveIdx] = useState(0);

  useEffect(() => {
    const id = setInterval(() => {
      setActiveIdx((prev) => (prev + 1) % frames.length);
    }, FRAME_DURATION_MS);
    return () => clearInterval(id);
  }, [frames.length]);

  const title = lang === "zh" ? "正在加密 …" : "Encrypting …";
  const subtitle =
    lang === "zh"
      ? "为你展示这把 key 经过的全部流程，确认没有任何明文离开浏览器。"
      : "Here's the full pipeline your key goes through — no plaintext ever leaves the browser.";

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-slate-900/60 backdrop-blur-sm">
      <div className="w-full max-w-xl rounded-2xl bg-white p-6 shadow-2xl">
        <div className="flex items-baseline justify-between">
          <h3 className="text-lg font-semibold text-slate-900">{title}</h3>
          <span className="text-xs text-slate-500">
            {activeIdx + 1} / {frames.length}
          </span>
        </div>
        <p className="mt-1 text-xs text-slate-500">{subtitle}</p>

        <ol className="mt-5 space-y-3">
          {frames.map((frame, idx) => {
            const isActive = idx === activeIdx;
            const isDone = idx < activeIdx;
            return (
              <li
                key={frame.id}
                className={`flex items-start gap-3 rounded-lg border p-3 transition-all duration-300 ${
                  isActive
                    ? "border-blue-300 bg-blue-50 ring-2 ring-blue-200"
                    : isDone
                    ? "border-emerald-200 bg-emerald-50/60 opacity-90"
                    : "border-slate-200 bg-white opacity-50"
                }`}
              >
                <span
                  className={`flex h-9 w-9 shrink-0 items-center justify-center rounded-full text-lg font-semibold ${
                    isActive
                      ? "bg-blue-600 text-white"
                      : isDone
                      ? "bg-emerald-500 text-white"
                      : "bg-slate-200 text-slate-500"
                  }`}
                >
                  {isDone ? "✓" : frame.icon}
                </span>
                <div className="min-w-0 flex-1">
                  <div className="text-sm font-medium text-slate-900">{frame.label}</div>
                  <div className="text-xs text-slate-600">{frame.body}</div>
                  {isActive ? (
                    <div className="mt-2 h-1 w-full overflow-hidden rounded-full bg-blue-100">
                      <div
                        className="byok-progress-bar h-full bg-blue-600"
                        style={{ animationDuration: `${FRAME_DURATION_MS}ms` }}
                      />
                    </div>
                  ) : null}
                </div>
              </li>
            );
          })}
        </ol>

        <style>{`
          @keyframes byokProgress {
            from { width: 0%; }
            to { width: 100%; }
          }
          .byok-progress-bar {
            animation: byokProgress linear forwards;
          }
        `}</style>
      </div>
    </div>
  );
};

export default ByokEncryptionAnimation;
