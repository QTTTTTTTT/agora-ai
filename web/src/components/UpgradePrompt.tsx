import React from "react";
import { useNavigate } from "react-router-dom";
import { useAppPreferences } from "../lib/preferences";

/**
 * UpgradePrompt — 嵌入到锁定内容旁的小型升级 CTA。
 *
 * Variants:
 *   - inline: 一句话内嵌，最不打扰，配在被锁内容旁
 *   - card:   占整行，配在大块内容上方
 *
 * 点击行为：跳 /pricing?source=<source>&tier=<suggestedTier>，便于
 * 后续做漏斗分析。
 */
export type UpgradePromptVariant = "inline" | "card";

export interface UpgradePromptProps {
  source: string;
  variant?: UpgradePromptVariant;
  suggestedTier?: "pro" | "premium" | "enterprise";
  /** 可选 — 直接覆盖按钮 / 标题文案；未提供时按 source 走默认文案表 */
  copyOverride?: { title?: string; cta?: string };
}

const DEFAULT_COPY: Record<
  string,
  { zh: { title: string; cta: string }; en: { title: string; cta: string } }
> = {
  daily_picks_indicators: {
    zh: { title: "升级查看 RSI / MACD / 支撑位等指标", cta: "升级 $14.9/月" },
    en: { title: "See RSI / MACD / support levels", cta: "Upgrade $14.9/mo" },
  },
  daily_picks_lag: {
    zh: { title: "升级解锁实时榜单（免费版延迟 3 个交易日）", cta: "升级 $14.9/月" },
    en: { title: "Unlock real-time picks (free is T+3)", cta: "Upgrade $14.9/mo" },
  },
  ask_ai_quota_exhausted: {
    zh: { title: "AI 提问次数已用完，升级解锁不限次", cta: "升级 $14.9/月" },
    en: { title: "Out of AI questions today — go unlimited", cta: "Upgrade $14.9/mo" },
  },
  preset_locked_value: {
    zh: { title: "解锁价值 / 成长 / 宏观全部 4 个策略", cta: "升级 $14.9/月" },
    en: { title: "Unlock Value / Growth / Macro strategies", cta: "Upgrade $14.9/mo" },
  },
  csv_export: {
    zh: { title: "导出 CSV / Excel — 升级到 Pro 套餐", cta: "升级 $29/月" },
    en: { title: "Export to CSV / Excel — upgrade to Pro", cta: "Upgrade $29/mo" },
  },
  paper_trading: {
    zh: { title: "无风险试跑大师团队策略", cta: "升级 $14.9/月" },
    en: { title: "Paper-trade strategies risk-free", cta: "Upgrade $14.9/mo" },
  },
  default: {
    zh: { title: "升级解锁全部功能", cta: "查看方案" },
    en: { title: "Unlock all features", cta: "See plans" },
  },
};

const UpgradePrompt: React.FC<UpgradePromptProps> = ({
  source,
  variant = "inline",
  suggestedTier = "pro",
  copyOverride,
}) => {
  const { language } = useAppPreferences();
  const isEn = language === "en-US";
  const navigate = useNavigate();

  const bucket = DEFAULT_COPY[source] ?? DEFAULT_COPY.default;
  const text = isEn ? bucket.en : bucket.zh;
  const title = copyOverride?.title ?? text.title;
  const cta = copyOverride?.cta ?? text.cta;

  const handleClick = () => {
    const qs = new URLSearchParams({ source, tier: suggestedTier });
    navigate(`/pricing?${qs.toString()}`);
  };

  if (variant === "card") {
    return (
      <div className="flex flex-col items-start gap-2 rounded-xl border border-indigo-200 bg-indigo-50 p-4 sm:flex-row sm:items-center sm:justify-between">
        <p className="text-sm font-medium text-indigo-900">{title}</p>
        <button
          type="button"
          onClick={handleClick}
          className="rounded-md bg-indigo-600 px-3 py-1.5 text-xs font-semibold text-white hover:bg-indigo-500"
        >
          {cta}
        </button>
      </div>
    );
  }

  // inline
  return (
    <button
      type="button"
      onClick={handleClick}
      className="inline-flex items-center gap-1 rounded-md border border-indigo-200 bg-indigo-50 px-2 py-1 text-[11px] font-medium text-indigo-700 transition hover:border-indigo-400 hover:text-indigo-900"
    >
      <span>🔒</span>
      <span>{title}</span>
      <span className="ml-1 text-[10px] text-indigo-500">→ {cta}</span>
    </button>
  );
};

export default UpgradePrompt;
