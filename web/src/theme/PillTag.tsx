// PillTag — the small status / tag chips that sit above a card
// title (需处理 / 已整备 / 风控优先 / 未验证 ...). Always 999px
// radius, soft pastel background, dark text. The tone enum is
// canonical: callers pick a semantic name, not a raw color.

import React from "react";

export type PillTagToneName =
  | "coral"   // 需处理 / pending / warning
  | "sage"    // 已整备 / ready / positive
  | "risk"    // 风控优先 / critical
  | "ink"     // 官方 / system / neutral-strong
  | "muted"   // 未验证 / draft / disabled
  | "info";   // 提示 / info-blue

export const PillTagTone: Record<PillTagToneName, string> = {
  coral:
    "bg-coral-100 text-coral-500 dark:bg-coral-500/20 dark:text-coral-200",
  sage:
    "bg-sage-100 text-sage-700 dark:bg-sage-500/20 dark:text-sage-300",
  risk:
    "bg-risk-100 text-risk-500 dark:bg-risk-500/20 dark:text-risk-200",
  ink:
    "bg-ink-100 text-ink-700 dark:bg-slate-700 dark:text-slate-200",
  muted:
    "bg-cream-100 text-ink-300 dark:bg-slate-700/40 dark:text-slate-400",
  info:
    "bg-brand-50 text-brand-700 dark:bg-brand-500/20 dark:text-brand-200",
};

export interface PillTagProps extends React.HTMLAttributes<HTMLSpanElement> {
  tone?: PillTagToneName;
  /** Tighter footprint, used inline in tables / dense lists. */
  size?: "sm" | "md";
  /** Optional leading dot (•) for "active" tone. */
  dot?: boolean;
}

export const PillTag: React.FC<PillTagProps> = ({
  tone = "muted",
  size = "md",
  dot = false,
  className = "",
  children,
  ...rest
}) => {
  const sizing =
    size === "sm"
      ? "px-2 py-0.5 text-[11px]"
      : "px-3 py-1 text-xs";
  return (
    <span
      className={[
        "inline-flex items-center gap-1 rounded-full font-medium leading-none",
        sizing,
        PillTagTone[tone],
        className,
      ].join(" ")}
      {...rest}
    >
      {dot ? (
        <span className="h-1.5 w-1.5 rounded-full bg-current opacity-70" />
      ) : null}
      {children}
    </span>
  );
};
