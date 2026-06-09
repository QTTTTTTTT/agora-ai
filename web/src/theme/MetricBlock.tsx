// MetricBlock — the labeled-number tile used inside the team
// dashboard card (组合净值 ¥122,582.22 / 滚动收益 +22.58% / 待
// 执行 1 项 / 持仓/交易 0/0). Pulls value alignment, label
// styling, and the subtle positive/negative tone into one
// component so callers don't repeat the layout.

import React from "react";

export interface MetricBlockProps {
  label: React.ReactNode;
  value: React.ReactNode;
  /** Color the value: positive → sage green, negative → risk
   *  red, neutral → ink. Defaults to neutral. */
  tone?: "neutral" | "positive" | "negative";
  /** Optional secondary line below value (e.g. "vs sh-index"). */
  hint?: React.ReactNode;
  /** Tighter padding for grid use (4-up). */
  compact?: boolean;
  className?: string;
}

const toneToValueClass: Record<NonNullable<MetricBlockProps["tone"]>, string> = {
  neutral:  "text-ink-900 dark:text-slate-100",
  positive: "text-sage-500 dark:text-sage-300",
  negative: "text-risk-500 dark:text-risk-300",
};

export const MetricBlock: React.FC<MetricBlockProps> = ({
  label,
  value,
  tone = "neutral",
  hint,
  compact = false,
  className = "",
}) => {
  return (
    <div className={["min-w-0", compact ? "" : "py-1", className].join(" ")}>
      <div className="text-xs font-medium text-ink-300 dark:text-slate-400">
        {label}
      </div>
      <div
        className={[
          "mt-1 truncate font-bold leading-tight",
          compact ? "text-xl" : "text-2xl sm:text-[26px]",
          toneToValueClass[tone],
        ].join(" ")}
      >
        {value}
      </div>
      {hint ? (
        <div className="mt-1 text-xs text-ink-300 dark:text-slate-500">{hint}</div>
      ) : null}
    </div>
  );
};
