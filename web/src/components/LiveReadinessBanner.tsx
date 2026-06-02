// LiveReadinessBanner — P0-9 web component.
//
// Renders a per-fund "live trading prerequisites" checklist at
// the top of any page that mutates orders on a real-money fund
// (TradeHistory + DecisionCenter today). Polls
// /api/funds/{fundId}/live-readiness once per mount; the response
// drives a four-row pillar list.
//
// Design choices
//
//   - Component is intentionally pure-display: it does NOT block
//     the page even when ready=false. The cancel/replace mutation
//     handlers already enforce the gate server-side and surface
//     a 403 with first_failing in the body, so the UI's job is
//     purely to TELEGRAPH the requirement before the user wastes
//     a click.
//   - Hides itself on simulation/paper funds so we don't add
//     noise to the 95% of users who never trade live.
//   - On gate_enforced=false we render a soft banner labelled
//     "dev mode" so an operator visually notices a misconfigured
//     deploy.
//
// Why not a hook
//
// The hook variant would force every consumer to wire its own
// loading-and-error UI. A self-contained component minimises
// duplication across the few pages that need it.

import { useEffect, useState } from "react";
import { ApiError, formatApiError, getLiveReadiness, type LiveReadinessResponse } from "../lib/api";

type Language = "zh-CN" | "en-US";

interface LiveReadinessBannerProps {
  fundId: string;
  language: Language;
  // Optional: when the user has just minted a step-up token
  // (e.g. via biometric prompt) the parent can pass it so the
  // banner re-evaluates StepUpOK without a separate refresh.
  stepUpToken?: string;
  // refreshKey is bumped by the parent (e.g. after a successful
  // cancel/replace) to force a re-fetch — we read it as a useEffect
  // dependency.
  refreshKey?: number;
}

// pillar label dictionary — separate object so a future i18n
// migration can drop in a `t()` call without churning JSX.
const messages: Record<Language, {
  title: string;
  subtitle: string;
  enforced: string;
  bypass: string;
  pillars: { kyc: string; brokerLink: string; twoFA: string; stepUp: string };
  ok: string;
  pending: string;
  blocked: { kyc: string; broker: string; twofa: string; stepUp: string };
  loading: string;
  errorPrefix: string;
}> = {
  "zh-CN": {
    title: "实盘前置条件",
    subtitle: "该基金为实盘模式，需四项校验全部通过后才能下单/改单/撤单。",
    enforced: "硬门禁已开启",
    bypass: "硬门禁未开启（开发模式）",
    pillars: {
      kyc: "KYC 实名认证",
      brokerLink: "券商账户绑定",
      twoFA: "2FA / TOTP",
      stepUp: "生物识别确认",
    },
    ok: "已通过",
    pending: "待完成",
    blocked: {
      kyc: "请先完成 KYC 实名认证",
      broker: "请先绑定券商账户",
      twofa: "请先开启 2FA / TOTP",
      stepUp: "请先通过生物识别确认",
    },
    loading: "正在加载实盘门禁状态…",
    errorPrefix: "无法获取门禁状态：",
  },
  "en-US": {
    title: "Live trading prerequisites",
    subtitle:
      "This fund is in live mode. All four checks must pass before placing, replacing, or cancelling orders.",
    enforced: "Hard gate enabled",
    bypass: "Hard gate off (dev mode)",
    pillars: {
      kyc: "KYC verification",
      brokerLink: "Broker account link",
      twoFA: "2FA / TOTP",
      stepUp: "Biometric confirmation",
    },
    ok: "Passed",
    pending: "Action required",
    blocked: {
      kyc: "Please complete KYC verification first",
      broker: "Please link a broker account first",
      twofa: "Please enable 2FA / TOTP first",
      stepUp: "Please confirm with biometrics first",
    },
    loading: "Loading live trading readiness…",
    errorPrefix: "Failed to load readiness: ",
  },
};

export default function LiveReadinessBanner({ fundId, language, stepUpToken, refreshKey }: LiveReadinessBannerProps) {
  const [data, setData] = useState<LiveReadinessResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const m = messages[language];

  useEffect(() => {
    if (!fundId) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    getLiveReadiness(fundId, stepUpToken)
      .then((resp) => {
        if (cancelled) return;
        setData(resp);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        // 403/404 from a fund the user doesn't own — keep the
        // banner hidden rather than dump a noisy error on top.
        if (err instanceof ApiError && (err.status === 403 || err.status === 404)) {
          setData(null);
          return;
        }
        setError(formatApiError(err, m.errorPrefix));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fundId, stepUpToken, refreshKey]);

  if (loading) {
    return (
      <div className="rounded-2xl border border-amber-200 bg-amber-50 p-4 text-sm text-amber-800">
        {m.loading}
      </div>
    );
  }

  if (error) {
    return (
      <div className="rounded-2xl border border-red-200 bg-red-50 p-4 text-sm text-red-700">
        {m.errorPrefix}
        {error}
      </div>
    );
  }

  // Hide the banner entirely on non-live funds — no noise where
  // it isn't needed.
  if (!data || data.trading_mode !== "live") {
    return null;
  }

  const pillarRow = (label: string, ok: boolean, hint: string) => (
    <li className="flex items-center justify-between gap-3 py-2 text-sm">
      <span className="flex items-center gap-2 text-gray-700">
        <span
          aria-hidden
          className={`inline-block h-2.5 w-2.5 rounded-full ${ok ? "bg-emerald-500" : "bg-amber-500"}`}
        />
        <span className="font-medium">{label}</span>
      </span>
      <span className={ok ? "text-emerald-700" : "text-amber-700"}>
        {ok ? m.ok : m.pending}
        {!ok ? <span className="ml-2 text-gray-500">— {hint}</span> : null}
      </span>
    </li>
  );

  return (
    <div
      className={`rounded-2xl border p-5 shadow-sm ${
        data.ready
          ? "border-emerald-200 bg-emerald-50"
          : "border-amber-200 bg-amber-50"
      }`}
    >
      <div className="flex items-start justify-between gap-3">
        <div>
          <h2 className="text-base font-semibold text-gray-900">{m.title}</h2>
          <p className="mt-1 text-sm text-gray-600">{m.subtitle}</p>
        </div>
        <span
          className={`whitespace-nowrap rounded-full px-3 py-1 text-xs font-medium ${
            data.gate_enforced
              ? "bg-emerald-200 text-emerald-900"
              : "bg-gray-200 text-gray-700"
          }`}
        >
          {data.gate_enforced ? m.enforced : m.bypass}
        </span>
      </div>
      <ul className="mt-3 divide-y divide-amber-200/60">
        {pillarRow(m.pillars.kyc, data.kyc_ok, m.blocked.kyc)}
        {pillarRow(m.pillars.brokerLink, data.broker_link_ok, m.blocked.broker)}
        {pillarRow(m.pillars.twoFA, data.two_fa_ok, m.blocked.twofa)}
        {pillarRow(m.pillars.stepUp, data.step_up_ok, m.blocked.stepUp)}
      </ul>
    </div>
  );
}
