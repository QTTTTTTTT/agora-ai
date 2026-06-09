// SectionLabel — small caps eyebrow heading used above a card
// title or a section-of-cards cluster ("团队驾驶舱" / "数据整备" /
// "阵容"). Carries the muted gray + 12px tracking that gives the
// reference design its sectional rhythm.

import React from "react";

export interface SectionLabelProps extends React.HTMLAttributes<HTMLDivElement> {
  /** Bold count or status pill aligned to the right of the
   *  label, e.g. "5 / 5" on the 阵容 page. */
  trailing?: React.ReactNode;
}

export const SectionLabel: React.FC<SectionLabelProps> = ({
  trailing,
  className = "",
  children,
  ...rest
}) => {
  return (
    <div
      className={[
        "flex items-center justify-between gap-3 px-1",
        className,
      ].join(" ")}
      {...rest}
    >
      <div className="text-[11px] font-semibold uppercase tracking-[0.14em] text-ink-300 dark:text-slate-400">
        {children}
      </div>
      {trailing ? (
        <div className="text-sm font-semibold text-sage-700 dark:text-sage-300">
          {trailing}
        </div>
      ) : null}
    </div>
  );
};
