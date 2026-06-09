import React, { useEffect, useState } from "react";

import { fetchAdvisorByokInfo, formatApiError, type AdvisorByokInfo } from "../lib/api";

// ByokSecurityGuide — Phase D-3 (tutorial half).
//
// A 4-step walk-through the user can read before deciding to add
// a key. The big differentiator is the IP whitelist step: we pull
// our actual prod egress IP from /api/advisor/byok/info so the
// user can copy-paste it into the OpenAI / Anthropic dashboard
// instead of guessing. Without this step, BYOK security is purely
// reputational; with it, the user can lock their key down to
// "only our infra can use it" via the provider's native IP allow-
// list — and we feel much more comfortable holding the secret.

interface Props {
  lang: "zh" | "en";
}

const COPY = {
  zh: {
    title: "BYOK 安全教程",
    sub: "4 步把你的 key 锁在我们服务器的出口 IP 上 — 即使数据库泄露，攻击者也用不了。",
    steps: [
      {
        n: 1,
        title: "在服务商生成新 key",
        body: "不要用你日常的全权限 key — 在 OpenAI / Anthropic 控制台为「fundai-advisor」建一张专用 key，便于将来吊销。",
      },
      {
        n: 2,
        title: "限制 key 的能力",
        body: "如果服务商支持，设置：仅允许 Chat Completions / Messages 接口、每月预算上限、只读模式。",
      },
      {
        n: 3,
        title: "锁定我们的出口 IP",
        body: "在服务商的 key 设置里，把以下 IP 加入「允许调用」白名单。锁定后，即便 key 泄露，未授权 IP 也无法使用：",
      },
      {
        n: 4,
        title: "提交并验证",
        body: "回到 fundai，提交 key — 我们会立刻发一次 ping，验证连接成功后才入库。任何时候点「撤销」即可永久删除。",
      },
    ],
    egressLabel: "fundai 出口 IP（请复制）：",
    encryptedTrue: "✓ 已确认：服务器已启用 AES-256-GCM 加密，明文 key 不会落盘。",
    encryptedFalse: "⚠ 当前部署未配置加密密钥（MODEL_CONFIG_API_KEY_SECRET），请联系运维确认。",
    supportLabel: "有疑问？联系：",
    loadError: "加载安全信息失败",
    loading: "加载中…",
  },
  en: {
    title: "BYOK security guide",
    sub: "4 steps to lock your key to our server's egress IP — even if our database leaks, attackers can't use it.",
    steps: [
      {
        n: 1,
        title: "Generate a new key at your provider",
        body: "Don't use your daily-driver key — create a dedicated \"fundai-advisor\" key in the OpenAI / Anthropic dashboard so revocation is easy.",
      },
      {
        n: 2,
        title: "Restrict the key's capabilities",
        body: "If your provider supports it: limit to Chat Completions / Messages only, set a monthly cap, enable read-only mode.",
      },
      {
        n: 3,
        title: "Lock to our egress IP",
        body: "In your provider's key settings, add the IP below to the \"allowed callers\" whitelist. Even if the key leaks, unauthorised IPs can't use it:",
      },
      {
        n: 4,
        title: "Submit and verify",
        body: "Come back to fundai and submit. We immediately ping the provider to verify the connection before persisting. Click \"revoke\" any time for permanent deletion.",
      },
    ],
    egressLabel: "fundai egress IP (copy this):",
    encryptedTrue: "✓ Confirmed: AES-256-GCM at-rest encryption is active. Plaintext keys are never written to disk.",
    encryptedFalse:
      "⚠ This deployment has no encryption secret configured (MODEL_CONFIG_API_KEY_SECRET). Please ask ops to confirm.",
    supportLabel: "Questions? Email:",
    loadError: "Failed to load security info",
    loading: "Loading…",
  },
};

const ByokSecurityGuide: React.FC<Props> = ({ lang }) => {
  const copy = COPY[lang];
  const [info, setInfo] = useState<AdvisorByokInfo | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    let active = true;
    fetchAdvisorByokInfo()
      .then((res) => {
        if (active) setInfo(res);
      })
      .catch((err) => {
        if (active) setError(formatApiError(err, copy.loadError));
      });
    return () => {
      active = false;
    };
  }, [copy.loadError]);

  const handleCopy = async () => {
    if (!info?.egress_ip) return;
    try {
      await navigator.clipboard.writeText(info.egress_ip);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      // clipboard may be disallowed in non-secure contexts; fall back silently
      setCopied(false);
    }
  };

  return (
    <section className="rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
      <h2 className="text-sm font-semibold text-slate-800">{copy.title}</h2>
      <p className="mt-0.5 text-xs text-slate-500">{copy.sub}</p>

      {error ? (
        <div className="mt-3 rounded-md border border-rose-200 bg-rose-50 p-2 text-xs text-rose-700">
          {error}
        </div>
      ) : null}

      <ol className="mt-4 space-y-3">
        {copy.steps.map((step) => (
          <li key={step.n} className="flex gap-3">
            <span className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-blue-100 text-xs font-semibold text-blue-700">
              {step.n}
            </span>
            <div className="min-w-0 flex-1">
              <div className="text-sm font-medium text-slate-900">{step.title}</div>
              <div className="mt-0.5 text-xs text-slate-600">{step.body}</div>
              {step.n === 3 ? (
                <div className="mt-2 flex items-center gap-2 rounded-md border border-slate-200 bg-slate-50 px-2 py-1.5">
                  <code className="flex-1 font-mono text-xs text-slate-900">
                    {info?.egress_ip ?? copy.loading}
                  </code>
                  <button
                    type="button"
                    onClick={() => void handleCopy()}
                    disabled={!info?.egress_ip}
                    className="rounded-md border border-slate-200 bg-white px-2 py-1 text-[11px] font-medium text-slate-700 hover:bg-slate-50 disabled:opacity-50"
                  >
                    {copied ? (lang === "zh" ? "✓ 已复制" : "✓ Copied") : lang === "zh" ? "复制" : "Copy"}
                  </button>
                </div>
              ) : null}
            </div>
          </li>
        ))}
      </ol>

      {info ? (
        <div
          className={`mt-4 rounded-md border p-3 text-xs ${
            info.encrypted_at_rest
              ? "border-emerald-200 bg-emerald-50 text-emerald-800"
              : "border-amber-200 bg-amber-50 text-amber-800"
          }`}
        >
          {info.encrypted_at_rest ? copy.encryptedTrue : copy.encryptedFalse}
        </div>
      ) : null}

      {info ? (
        <p className="mt-3 text-[11px] text-slate-500">
          {copy.supportLabel}{" "}
          <a href={`mailto:${info.support_email}`} className="text-blue-600 hover:underline">
            {info.support_email}
          </a>
        </p>
      ) : null}
    </section>
  );
};

export default ByokSecurityGuide;
