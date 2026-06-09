// BlackPillButton — the deep-charcoal pill CTA from the
// reference screenshots ("装备 →", "去回测验证"). Inverted color
// (black fill, white text), 999px radius, optional trailing
// arrow. The ghost variant is a transparent pill with ink
// border — used for secondary actions that sit beside it.

import React from "react";

export interface BlackPillButtonProps
  extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  /** Compact (h-9) vs default (h-11) vs large (h-12 / sticky bottom CTA). */
  size?: "sm" | "md" | "lg";
  /** Show a trailing arrow glyph "→" — matches "装备 →" in spec. */
  withArrow?: boolean;
  /** Optional leading icon node (e.g. a chart glyph for "去回测验证"). */
  leadingIcon?: React.ReactNode;
  /** Stretch to full width, used for the bottom sticky bar. */
  block?: boolean;
}

const sizeClass: Record<NonNullable<BlackPillButtonProps["size"]>, string> = {
  sm: "h-9  px-4 text-sm",
  md: "h-11 px-5 text-sm",
  lg: "h-12 px-6 text-[15px]",
};

export const BlackPillButton: React.FC<BlackPillButtonProps> = ({
  size = "md",
  withArrow = false,
  leadingIcon,
  block = false,
  className = "",
  children,
  ...rest
}) => {
  return (
    <button
      type="button"
      className={[
        "inline-flex select-none items-center justify-center gap-2 rounded-full",
        "bg-ink-900 text-white font-semibold",
        "shadow-pill-ink ring-1 ring-ink-700/40",
        "transition hover:bg-ink-700 active:translate-y-px",
        "disabled:cursor-not-allowed disabled:opacity-40",
        block ? "w-full" : "",
        sizeClass[size],
        className,
      ].join(" ")}
      {...rest}
    >
      {leadingIcon ? <span className="-ml-0.5">{leadingIcon}</span> : null}
      <span className="truncate">{children}</span>
      {withArrow ? (
        <span aria-hidden="true" className="-mr-0.5 text-base leading-none">→</span>
      ) : null}
    </button>
  );
};

export const GhostPillButton: React.FC<BlackPillButtonProps> = ({
  size = "md",
  withArrow = false,
  leadingIcon,
  block = false,
  className = "",
  children,
  ...rest
}) => {
  return (
    <button
      type="button"
      className={[
        "inline-flex select-none items-center justify-center gap-2 rounded-full",
        "bg-cream-0 text-ink-700 font-semibold",
        "shadow-pill ring-1 ring-ink-200/80",
        "transition hover:bg-cream-50 active:translate-y-px",
        "dark:bg-slate-800 dark:text-slate-100 dark:ring-slate-600",
        "disabled:cursor-not-allowed disabled:opacity-40",
        block ? "w-full" : "",
        sizeClass[size],
        className,
      ].join(" ")}
      {...rest}
    >
      {leadingIcon ? <span className="-ml-0.5">{leadingIcon}</span> : null}
      <span className="truncate">{children}</span>
      {withArrow ? (
        <span aria-hidden="true" className="-mr-0.5 text-base leading-none">→</span>
      ) : null}
    </button>
  );
};
