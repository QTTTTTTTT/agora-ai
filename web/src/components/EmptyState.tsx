// components/EmptyState.tsx
//
// Shared "no rows yet" placeholder used across pages where the
// list / table / dashboard would otherwise render to a blank area
// or a single line of grey text. The component takes an
// illustration kind, a localised title, an optional description,
// and 0-2 calls to action.
//
// The illustrations are inline SVG so:
//   - the bundle doesn't have to load an external image at runtime
//   - colours follow the indigo / brand palette without a separate
//     asset pass when we re-skin
//   - the figure is tagged role="img" + aria-label so screen
//     readers announce a meaningful description, not "graphic"
//
// Why not a third-party empty-state library: every option in the
// React ecosystem ships either lottie animations (bundle weight)
// or a tightly opinionated layout that doesn't match this app's
// rounded-2xl / dashed-border / centered-CTA aesthetic. A 200-line
// in-repo component costs less than the dependency review.

import React from "react";

export type EmptyKind = "list" | "marketplace" | "search" | "alarm" | "wallet" | "people";

export interface EmptyStateAction {
  label: React.ReactNode;
  onClick: () => void;
  // Variant defaults to "primary" (indigo CTA). "secondary" renders
  // an outlined button useful for "Learn more" / "Read the docs"
  // alongside the main action.
  variant?: "primary" | "secondary";
  // Optional anchor href; when provided the component renders an <a>
  // instead of a <button> so target="_blank" / docs links work.
  href?: string;
  external?: boolean;
}

export interface EmptyStateProps {
  kind?: EmptyKind;
  title: React.ReactNode;
  description?: React.ReactNode;
  // Up to two actions. The first is the primary CTA, the second is
  // the secondary alternative. Pass an empty array (or omit) for a
  // pure informational empty state.
  actions?: EmptyStateAction[];
  // Tip / next-step hint shown below the actions in muted text.
  // Used for onboarding flows ("after creating your first company,
  // you'll be redirected straight to the fund workspace…").
  hint?: React.ReactNode;
  // Override the surface — pages with their own card wrapper
  // (Companies has a rounded-2xl dashed-border one) can pass
  // bare="true" to skip the inner card and rely on the outer.
  bare?: boolean;
  className?: string;
}

const Illustration: React.FC<{ kind: EmptyKind; ariaLabel: string }> = ({ kind, ariaLabel }) => {
  // All illustrations are framed in a 160×160 viewBox at 96px
  // wide so they sit comfortably above a 56px CTA without
  // dominating the empty card. Colours come from the indigo and
  // gray scales that match the app's surface palette.
  switch (kind) {
    case "marketplace":
      return (
        <svg
          role="img"
          aria-label={ariaLabel}
          viewBox="0 0 160 160"
          width="96"
          height="96"
          className="text-indigo-500"
        >
          <rect x="20" y="50" width="120" height="80" rx="8" fill="currentColor" fillOpacity="0.1" stroke="currentColor" strokeWidth="2" />
          <rect x="32" y="62" width="36" height="28" rx="3" fill="currentColor" fillOpacity="0.2" />
          <rect x="76" y="62" width="36" height="28" rx="3" fill="currentColor" fillOpacity="0.2" />
          <rect x="32" y="98" width="36" height="22" rx="3" fill="currentColor" fillOpacity="0.2" />
          <rect x="76" y="98" width="36" height="22" rx="3" fill="currentColor" fillOpacity="0.2" />
          <path d="M40 40 L80 25 L120 40" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" fill="none" />
          <circle cx="80" cy="25" r="4" fill="currentColor" />
        </svg>
      );
    case "search":
      return (
        <svg
          role="img"
          aria-label={ariaLabel}
          viewBox="0 0 160 160"
          width="96"
          height="96"
          className="text-indigo-500"
        >
          <circle cx="70" cy="70" r="35" stroke="currentColor" strokeWidth="3" fill="currentColor" fillOpacity="0.1" />
          <line x1="96" y1="96" x2="125" y2="125" stroke="currentColor" strokeWidth="6" strokeLinecap="round" />
          <path d="M55 70 L70 85 L90 60" stroke="currentColor" strokeWidth="3" strokeLinecap="round" strokeLinejoin="round" fill="none" opacity="0.5" />
        </svg>
      );
    case "alarm":
      return (
        <svg
          role="img"
          aria-label={ariaLabel}
          viewBox="0 0 160 160"
          width="96"
          height="96"
          className="text-indigo-500"
        >
          <path d="M80 35 C100 35 116 51 116 71 V100 H44 V71 C44 51 60 35 80 35 Z" fill="currentColor" fillOpacity="0.1" stroke="currentColor" strokeWidth="2" />
          <rect x="55" y="100" width="50" height="8" rx="2" fill="currentColor" fillOpacity="0.2" />
          <line x1="80" y1="115" x2="80" y2="125" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
          <circle cx="80" cy="70" r="4" fill="currentColor" />
        </svg>
      );
    case "wallet":
      return (
        <svg
          role="img"
          aria-label={ariaLabel}
          viewBox="0 0 160 160"
          width="96"
          height="96"
          className="text-indigo-500"
        >
          <rect x="25" y="55" width="110" height="70" rx="10" fill="currentColor" fillOpacity="0.1" stroke="currentColor" strokeWidth="2" />
          <rect x="95" y="80" width="40" height="22" rx="4" fill="currentColor" fillOpacity="0.2" />
          <circle cx="115" cy="91" r="5" fill="currentColor" />
        </svg>
      );
    case "people":
      return (
        <svg
          role="img"
          aria-label={ariaLabel}
          viewBox="0 0 160 160"
          width="96"
          height="96"
          className="text-indigo-500"
        >
          <circle cx="60" cy="65" r="18" fill="currentColor" fillOpacity="0.15" stroke="currentColor" strokeWidth="2" />
          <circle cx="100" cy="65" r="18" fill="currentColor" fillOpacity="0.15" stroke="currentColor" strokeWidth="2" />
          <path d="M30 120 C30 100 45 90 60 90 C75 90 90 100 90 120" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
          <path d="M70 120 C70 100 85 90 100 90 C115 90 130 100 130 120" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
        </svg>
      );
    case "list":
    default:
      return (
        <svg
          role="img"
          aria-label={ariaLabel}
          viewBox="0 0 160 160"
          width="96"
          height="96"
          className="text-indigo-500"
        >
          <rect x="30" y="35" width="100" height="90" rx="8" fill="currentColor" fillOpacity="0.1" stroke="currentColor" strokeWidth="2" />
          <line x1="46" y1="58" x2="114" y2="58" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
          <line x1="46" y1="78" x2="100" y2="78" stroke="currentColor" strokeWidth="3" strokeLinecap="round" opacity="0.7" />
          <line x1="46" y1="98" x2="90" y2="98" stroke="currentColor" strokeWidth="3" strokeLinecap="round" opacity="0.4" />
        </svg>
      );
  }
};

const ActionButton: React.FC<{ action: EmptyStateAction; isPrimary: boolean }> = ({ action, isPrimary }) => {
  const variant = action.variant ?? (isPrimary ? "primary" : "secondary");
  const className =
    variant === "primary"
      ? "rounded-lg bg-indigo-600 px-5 py-2.5 text-sm font-medium text-white shadow-sm hover:bg-indigo-700"
      : "rounded-lg border border-gray-300 bg-white px-5 py-2.5 text-sm font-medium text-gray-700 hover:bg-gray-50";
  if (action.href) {
    return (
      <a
        href={action.href}
        target={action.external ? "_blank" : undefined}
        rel={action.external ? "noopener noreferrer" : undefined}
        onClick={(e) => {
          if (!action.href) {
            e.preventDefault();
            action.onClick();
          }
        }}
        className={className}
      >
        {action.label}
      </a>
    );
  }
  return (
    <button type="button" onClick={action.onClick} className={className}>
      {action.label}
    </button>
  );
};

export const EmptyState: React.FC<EmptyStateProps> = ({
  kind = "list",
  title,
  description,
  actions,
  hint,
  bare = false,
  className = "",
}) => {
  // Compose the figure first so we can pass an aria-label derived
  // from the title for screen readers. React.ReactNode might not
  // be a string — fall back to a generic label in that case.
  const ariaLabel = typeof title === "string" ? title : "Empty state illustration";
  const inner = (
    <div className="flex flex-col items-center text-center">
      <div className="mb-4">
        <Illustration kind={kind} ariaLabel={ariaLabel} />
      </div>
      <h3 className="text-lg font-semibold text-gray-900">{title}</h3>
      {description ? (
        <p className="mx-auto mt-2 max-w-xl text-sm leading-6 text-gray-500">{description}</p>
      ) : null}
      {actions && actions.length > 0 ? (
        <div className="mt-5 flex flex-wrap items-center justify-center gap-3">
          {actions.slice(0, 2).map((action, idx) => (
            <ActionButton key={idx} action={action} isPrimary={idx === 0} />
          ))}
        </div>
      ) : null}
      {hint ? <p className="mt-4 text-xs text-gray-400">{hint}</p> : null}
    </div>
  );
  if (bare) {
    return <div className={className}>{inner}</div>;
  }
  return (
    <div
      className={`rounded-2xl border border-dashed border-gray-200 bg-white p-10 shadow-sm ${className}`}
    >
      {inner}
    </div>
  );
};

export default EmptyState;
