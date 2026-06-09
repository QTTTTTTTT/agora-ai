// EnvelopeCard — the white "lifted paper" card used everywhere in
// the cream redesign. Has the signature 24px radius, a barely-
// there shadow that warms toward the page background, and an
// optional `tone` band (left edge tint) for highlighting "needs
// attention" cards without resorting to a heavy border.
//
// We ship two variants:
//   <EnvelopeCard>       — standalone card, full styling
//   <EnvelopeSection>    — sectional group with title + body slot
//
// Both deliberately accept arbitrary children so existing pages
// can drop them in without rewriting the body content.

import React from "react";

export interface EnvelopeCardProps extends React.HTMLAttributes<HTMLDivElement> {
  /** Subtle accent stripe along the left edge — reserved for
   *  cards that need to telegraph status (e.g. "需处理" coral). */
  tone?: "neutral" | "sage" | "coral" | "risk";
  /** When true, wraps content with reduced padding for compact
   *  use inside grids. Defaults to false (generous padding). */
  compact?: boolean;
}

const toneStripe: Record<NonNullable<EnvelopeCardProps["tone"]>, string> = {
  neutral: "",
  sage:    "before:bg-sage-300",
  coral:   "before:bg-coral-300",
  risk:    "before:bg-risk-300",
};

export const EnvelopeCard: React.FC<EnvelopeCardProps> = ({
  tone = "neutral",
  compact = false,
  className = "",
  children,
  ...rest
}) => {
  const padding = compact ? "p-4 sm:p-5" : "p-5 sm:p-7";
  const stripe = tone !== "neutral"
    ? `relative overflow-hidden before:absolute before:left-0 before:top-6 before:bottom-6 before:w-1.5 before:rounded-r-full ${toneStripe[tone]}`
    : "";
  return (
    <div
      className={[
        "rounded-envelope bg-cream-0 shadow-envelope ring-1 ring-ink-100/60",
        "dark:bg-slate-900 dark:ring-slate-700/60 dark:shadow-none",
        "transition hover:shadow-envelope-hover",
        padding,
        stripe,
        className,
      ].join(" ")}
      {...rest}
    >
      {children}
    </div>
  );
};

// Drops the native HTMLAttributes `title` (which is a plain
// string used for tooltips) so we can re-purpose `title` as a
// rich React node for the section header. Callers that still
// need the HTML tooltip can pass `aria-label` instead.
export interface EnvelopeSectionProps extends Omit<EnvelopeCardProps, "title"> {
  /** Tiny eyebrow label (small caps gray) above the title. */
  eyebrow?: React.ReactNode;
  /** Bold display title. */
  title?: React.ReactNode;
  /** Optional subtitle directly under the title. */
  subtitle?: React.ReactNode;
  /** Optional right-aligned action node (button/menu) on the
   *  same row as the title — typical "去回测" / "整备详情" CTAs. */
  action?: React.ReactNode;
}

export const EnvelopeSection: React.FC<EnvelopeSectionProps> = ({
  eyebrow,
  title,
  subtitle,
  action,
  children,
  ...cardProps
}) => {
  return (
    <EnvelopeCard {...cardProps}>
      {(eyebrow || title || action) && (
        <div className="mb-4 flex items-start justify-between gap-3">
          <div className="min-w-0">
            {eyebrow ? (
              <div className="mb-1 text-xs font-medium uppercase tracking-wide text-ink-300 dark:text-slate-400">
                {eyebrow}
              </div>
            ) : null}
            {title ? (
              <div className="truncate text-xl font-bold text-ink-900 dark:text-slate-100">
                {title}
              </div>
            ) : null}
            {subtitle ? (
              <div className="mt-1 text-sm text-ink-300 dark:text-slate-400">
                {subtitle}
              </div>
            ) : null}
          </div>
          {action ? <div className="shrink-0">{action}</div> : null}
        </div>
      )}
      {children}
    </EnvelopeCard>
  );
};
