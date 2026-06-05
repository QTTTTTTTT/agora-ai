// components/RouteFallback.tsx
//
// Suspense fallback used while a route's chunk is being fetched.
// Replaces the previous one-line "Loading…" placeholder with three
// stacked affordances so the user can tell the page is making
// progress instead of being stuck:
//
//   1. An indeterminate progress bar pinned to the top of the
//      viewport (1.2s slide loop). Fast routes flash and disappear;
//      slow ones still feel "alive" because the bar is moving.
//   2. A central spinning indicator with the localised "Loading…"
//      caption — same colour family (indigo / brand) as the rest of
//      the app shell so it doesn't visually clash.
//   3. Three pulsing skeleton lines below the spinner that hint at
//      "content is coming" without committing to a specific layout
//      (every page has different content, so the skeleton stays
//      generic on purpose).
//
// Accessibility:
//
//   - The wrapper carries `role="status"` + `aria-live="polite"` so
//     screen readers announce the loading state without
//     interrupting the user's current focus.
//   - The progress bar is exposed as `role="progressbar"` with the
//     localised `aria-label`. We deliberately don't set an
//     aria-valuenow because the bar is indeterminate; per ARIA
//     spec, omitting valuenow on an indeterminate progressbar is
//     the correct signal.
//
// i18n contract: the caller passes the resolved `loading` string
// (App.tsx already has the language-aware copy bag so we don't
// re-hit useAppPreferences here, keeping the component a pure
// presentation primitive).

import React from "react";

interface RouteFallbackProps {
  loadingText: string;
}

const RouteFallback: React.FC<RouteFallbackProps> = ({ loadingText }) => (
  <>
    <div
      role="progressbar"
      aria-label={loadingText}
      className="fixed left-0 top-0 z-[9997] h-0.5 w-full overflow-hidden bg-indigo-100"
    >
      <div className="h-full w-1/4 animate-progress-slide bg-indigo-500" />
    </div>
    <div
      role="status"
      aria-live="polite"
      className="flex min-h-screen flex-col items-center justify-center gap-5 bg-gray-50 p-8"
    >
      <svg
        className="h-9 w-9 animate-spin text-indigo-600"
        xmlns="http://www.w3.org/2000/svg"
        fill="none"
        viewBox="0 0 24 24"
        aria-hidden="true"
      >
        <circle
          className="opacity-25"
          cx="12"
          cy="12"
          r="10"
          stroke="currentColor"
          strokeWidth="4"
        />
        <path
          className="opacity-75"
          fill="currentColor"
          d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"
        />
      </svg>
      <p className="text-sm font-medium text-gray-600">{loadingText}</p>
      <div className="flex w-full max-w-md flex-col gap-2" aria-hidden="true">
        <div className="h-3 w-5/6 animate-pulse rounded-md bg-gray-200" />
        <div className="h-3 w-4/6 animate-pulse rounded-md bg-gray-200" />
        <div className="h-3 w-3/6 animate-pulse rounded-md bg-gray-200" />
      </div>
    </div>
  </>
);

export default RouteFallback;
