// ComplianceBanner.tsx — surface-specific disclosure rendered
// IMMEDIATELY ABOVE the user-visible advisory content.
//
// Marketing Rule 206(4)-1 and SEC enforcement guidance require
// disclosures to be "clear and prominent" and to "precede or
// be contemporaneous with" the marketed material. A page-
// footer disclaimer is insufficient. This component is
// designed to be rendered ABOVE the panel it pertains to, in
// a colour scheme that catches the eye but doesn't visually
// shout (which the SEC has criticised as performative
// rather than informational).
//
// The component is intentionally narrow: it shows ONE
// disclosure paragraph + an optional "expand to read more"
// affordance for the hypothetical-performance footnote (only
// rendered for the backtest surface). Other compliance
// surfaces — modals, audit logs, ack persistence — live in
// adjacent files.

import { useState } from "react";

import { useCompliance, type ComplianceSurface } from "../lib/compliance";

export type ComplianceBannerProps = {
  surface: ComplianceSurface;
  // showHypothetical toggles the extra Marketing Rule
  // hypothetical-performance disclosure block. Default: only
  // on for the backtest surface.
  showHypothetical?: boolean;
  // tone="soft" trades the prominent yellow border for an
  // understated grey one — useful when the banner is one of
  // many in a dashboard. Default "loud" follows SEC's
  // "clear and prominent" intent.
  tone?: "loud" | "soft";
  className?: string;
};

export function ComplianceBanner({
  surface,
  showHypothetical,
  tone = "loud",
  className,
}: ComplianceBannerProps) {
  const { bundle, ready } = useCompliance();
  const [expanded, setExpanded] = useState(false);

  const b = bundle(surface);
  const enableHypothetical = showHypothetical ?? surface === "backtest";

  const baseClass =
    tone === "soft"
      ? "rounded-md border border-zinc-300 bg-zinc-50 text-zinc-700 dark:border-zinc-700 dark:bg-zinc-900/40 dark:text-zinc-300"
      : "rounded-md border-l-4 border-amber-500 bg-amber-50 text-amber-900 dark:border-amber-400 dark:bg-amber-950/40 dark:text-amber-100";

  return (
    <aside
      role="note"
      aria-live="polite"
      data-testid={`compliance-banner-${surface}`}
      className={[baseClass, "px-3 py-2 text-xs leading-5", className ?? ""]
        .filter(Boolean)
        .join(" ")}
    >
      {/* Show the bundled fallback even before the server bundle lands;
         this avoids a layout shift and keeps the page protected
         during boot. */}
      <p className="whitespace-pre-wrap">{b.disclosure}</p>
      {enableHypothetical ? (
        <div className="mt-1">
          <button
            type="button"
            className="text-[11px] underline-offset-2 hover:underline focus:outline-none"
            onClick={() => setExpanded((v) => !v)}
            aria-expanded={expanded}
          >
            {expanded ? hideMoreText(b.locale) : showMoreText(b.locale)}
          </button>
          {expanded ? (
            <p className="mt-1 whitespace-pre-wrap text-[11px] text-zinc-700 dark:text-zinc-300">
              {b.hypotheticalPerformanceDisclaimer}
            </p>
          ) : null}
        </div>
      ) : null}
      {!ready ? (
        <span className="sr-only">compliance banner loading</span>
      ) : null}
    </aside>
  );
}

function showMoreText(locale: string) {
  return locale.startsWith("zh") ? "查看回测假设性业绩披露" : "Show hypothetical performance disclosure";
}

function hideMoreText(locale: string) {
  return locale.startsWith("zh") ? "收起" : "Hide";
}
