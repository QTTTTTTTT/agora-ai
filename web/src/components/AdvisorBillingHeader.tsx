import React, { useEffect, useState } from "react";
import { Link } from "react-router-dom";

import {
  fetchAdvisorBillingSummary,
  formatApiError,
  listAdvisorByokKeys,
  type AdvisorBillingSummary,
  type AdvisorByokKey,
} from "../lib/api";

// AdvisorBillingHeader — Phase D-4 strip rendered at the top of
// the /advisor console.
//
// One-line snapshot of:
//   - how many deep / quick consults the user has left this month
//   - credit balance (if any)
//   - whether BYOK is active right now (and which provider)
//   - estimated dollar savings vs. platform pool (BYOK only)
//
// Goals:
//   * "out of quota" must be impossible to miss (we colour-code
//     the bar amber/red when remaining ≤ 20%).
//   * power users should feel proud of their savings — the
//     "saved this month" number is the most clickable surface.
//   * the strip degrades to "—" silently if the billing endpoint
//     isn't wired, so it never blocks the console from rendering.

interface Props {
  lang: "zh" | "en";
}

const COPY = {
  zh: {
    deepLabel: "深度",
    quickLabel: "快速",
    creditTag: "credit",
    byokLabelActive: "BYOK active",
    byokLabelOff: "走平台池",
    savedTitle: "本月已省",
    savedAlt: "本月省¥",
    upgrade: "升级套餐",
    manageByok: "管理 BYOK",
    buyMore: "购买 credit",
    unlimited: "无限",
    quotaTitle: "本月配额",
    loadError: "加载计费信息失败",
  },
  en: {
    deepLabel: "Deep",
    quickLabel: "Quick",
    creditTag: "credit",
    byokLabelActive: "BYOK active",
    byokLabelOff: "Platform pool",
    savedTitle: "Saved this month",
    savedAlt: "Saved $",
    upgrade: "Upgrade",
    manageByok: "Manage BYOK",
    buyMore: "Buy credits",
    unlimited: "unlimited",
    quotaTitle: "Monthly quota",
    loadError: "Failed to load billing info",
  },
};

// Estimated platform-pool service cost per consult. Used to render
// the "saved this month" number for BYOK users — we tell the user
// "you would have paid $0.30 / consult on our pool, you paid $0.15
// service fee instead, ×N consults = $X saved".
const PLATFORM_DEEP_COST_USD = 0.30;
const PLATFORM_QUICK_COST_USD = 0.05;
const BYOK_DEEP_SERVICE_FEE_USD = 0.15;
const BYOK_QUICK_SERVICE_FEE_USD = 0.02;

const AdvisorBillingHeader: React.FC<Props> = ({ lang }) => {
  const copy = COPY[lang];

  const [summary, setSummary] = useState<AdvisorBillingSummary | null>(null);
  const [keys, setKeys] = useState<AdvisorByokKey[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let active = true;
    void (async () => {
      try {
        const [sum, byok] = await Promise.all([
          fetchAdvisorBillingSummary(),
          listAdvisorByokKeys().catch(() => ({ keys: [] })),
        ]);
        if (!active) return;
        setSummary(sum);
        setKeys(byok.keys);
      } catch (err) {
        if (active) setError(formatApiError(err, copy.loadError));
      } finally {
        if (active) setLoading(false);
      }
    })();
    return () => {
      active = false;
    };
  }, [copy.loadError]);

  if (loading && !summary) {
    return (
      <div className="rounded-xl border border-slate-200 bg-white px-4 py-3 text-xs text-slate-500 shadow-sm">
        …
      </div>
    );
  }
  if (error || !summary) {
    return (
      <div className="rounded-xl border border-rose-200 bg-rose-50 px-4 py-3 text-xs text-rose-700">
        {error ?? copy.loadError}
      </div>
    );
  }

  const activeKey = keys.find((k) => k.is_active && !k.revoked_at);
  const byokActive = Boolean(activeKey);

  const deepRemainingPct =
    summary.deep_limit > 0
      ? Math.max(0, Math.min(100, (summary.deep_remaining / summary.deep_limit) * 100))
      : 100;
  const quickRemainingPct =
    summary.quick_limit > 0
      ? Math.max(0, Math.min(100, (summary.quick_remaining / summary.quick_limit) * 100))
      : 100;

  const lowOnDeep = summary.deep_limit > 0 && summary.deep_remaining > 0 && deepRemainingPct <= 20;
  const lowOnQuick =
    summary.quick_limit > 0 && summary.quick_remaining > 0 && quickRemainingPct <= 20;
  const outOfDeep =
    summary.deep_limit > 0 && summary.deep_remaining === 0 && summary.credit_deep_balance === 0;
  const outOfQuick =
    summary.quick_limit > 0 && summary.quick_remaining === 0 && summary.credit_quick_balance === 0;

  // Rough month savings estimate. We use:
  //   deep_used × (platform - byok_fee) + quick_used × (platform - byok_fee)
  // and only show it when BYOK is currently the routing target —
  // users still on the platform pool wouldn't actually be saving.
  const savedUsd = byokActive
    ? summary.deep_used * (PLATFORM_DEEP_COST_USD - BYOK_DEEP_SERVICE_FEE_USD) +
      summary.quick_used * (PLATFORM_QUICK_COST_USD - BYOK_QUICK_SERVICE_FEE_USD)
    : 0;

  const renderQuotaCell = (
    label: string,
    remaining: number,
    limit: number,
    credits: number,
    low: boolean,
    out: boolean,
  ) => {
    const display =
      limit === -1
        ? copy.unlimited
        : limit === 0
        ? credits > 0
          ? `0 / 0 (+${credits} ${copy.creditTag})`
          : "0 / 0"
        : `${remaining} / ${limit}`;
    return (
      <div className="flex flex-col">
        <span className="text-[10px] uppercase tracking-wider text-slate-500">{label}</span>
        <span
          className={`text-sm font-semibold ${
            out ? "text-rose-700" : low ? "text-amber-700" : "text-slate-900"
          }`}
        >
          {display}
        </span>
        {credits > 0 && limit !== -1 && limit !== 0 ? (
          <span className="text-[10px] text-emerald-700">
            +{credits} {copy.creditTag}
          </span>
        ) : null}
      </div>
    );
  };

  return (
    <div
      className={`rounded-xl border bg-white shadow-sm ${
        outOfDeep || outOfQuick ? "border-rose-300" : lowOnDeep || lowOnQuick ? "border-amber-300" : "border-slate-200"
      }`}
    >
      <div className="flex flex-wrap items-center justify-between gap-4 px-4 py-3">
        <div className="flex flex-wrap items-center gap-5">
          {renderQuotaCell(
            copy.deepLabel,
            summary.deep_remaining,
            summary.deep_limit,
            summary.credit_deep_balance ?? 0,
            lowOnDeep,
            outOfDeep,
          )}
          <span className="text-slate-300">·</span>
          {renderQuotaCell(
            copy.quickLabel,
            summary.quick_remaining,
            summary.quick_limit,
            summary.credit_quick_balance ?? 0,
            lowOnQuick,
            outOfQuick,
          )}
          <span className="text-slate-300">·</span>
          <div className="flex flex-col">
            <span className="text-[10px] uppercase tracking-wider text-slate-500">LLM</span>
            <span
              className={`text-sm font-semibold ${
                byokActive ? "text-emerald-700" : "text-slate-700"
              }`}
            >
              {byokActive ? copy.byokLabelActive : copy.byokLabelOff}
            </span>
            {byokActive && activeKey ? (
              <span className="text-[10px] text-slate-500">
                {activeKey.provider} · {activeKey.api_key_preview}
              </span>
            ) : null}
          </div>
          {byokActive && savedUsd > 0.01 ? (
            <>
              <span className="text-slate-300">·</span>
              <div className="flex flex-col">
                <span className="text-[10px] uppercase tracking-wider text-emerald-600">
                  {copy.savedTitle}
                </span>
                <span className="text-sm font-semibold text-emerald-700">
                  ${savedUsd.toFixed(2)}
                </span>
              </div>
            </>
          ) : null}
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {(outOfDeep || outOfQuick) && summary.upgrade_suggested ? (
            <Link
              to="/subscription"
              className="rounded-md bg-rose-600 px-2.5 py-1 text-[11px] font-medium text-white hover:bg-rose-700"
            >
              {copy.upgrade}
            </Link>
          ) : null}
          <Link
            to="/settings/byok"
            className={`rounded-md border px-2.5 py-1 text-[11px] font-medium ${
              byokActive
                ? "border-emerald-300 bg-emerald-50 text-emerald-800 hover:bg-emerald-100"
                : "border-slate-200 bg-white text-slate-700 hover:bg-slate-50"
            }`}
          >
            {copy.manageByok}
          </Link>
          {!byokActive ? (
            <Link
              to="/settings/byok#credits"
              className="rounded-md border border-slate-200 bg-white px-2.5 py-1 text-[11px] font-medium text-slate-700 hover:bg-slate-50"
            >
              {copy.buyMore}
            </Link>
          ) : null}
        </div>
      </div>
    </div>
  );
};

export default AdvisorBillingHeader;
