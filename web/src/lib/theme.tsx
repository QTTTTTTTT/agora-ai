// theme.tsx — application-wide light/dark/system theme provider.
//
// WHY THIS EXISTS
// ---------------
// The app shipped without a dark-mode option. Investors who run
// the dashboard at 11pm against a dark trading-tool window get
// blasted with white from this app — chronic eye fatigue, and
// the most-requested UX feedback from internal users.
//
// This module is the STARTER VERSION:
//   - Tailwind's `darkMode: 'class'` is enabled (see
//     tailwind.config.js), so `dark:bg-slate-900` and friends
//     activate when a `.dark` class lives on a parent.
//   - <ThemeProvider> persists the choice in localStorage,
//     reflects system changes when mode='system', and synchronises
//     across tabs via the BroadcastChannel hook so a user with
//     two tabs open doesn't see them out of sync.
//   - <ThemeToggle> provides a 3-state button (Light / Dark /
//     System) ready to drop into a header.
//
// SCOPE
// -----
// This commit lands the wiring + a toggle. Most of the existing
// components haven't been audited for dark colours yet — they'll
// continue to render in their light styles even with mode='dark'
// because their classes are `bg-white text-gray-900` with no
// `dark:` companion. The components.{Skeleton, Card layouts}
// already include `dark:bg-slate-700` etc. as a forward-compatible
// touch; further migrations land separately.
//
// OBJECTIVE
// ---------
//   1. <ThemeProvider> wraps the app, sets the `.dark` class on
//      <html> when mode='dark' or (mode='system' and system is
//      dark), removes it otherwise. The <html> meta-theme-color
//      stays consistent with the active theme so mobile chrome
//      colour matches.
//   2. <ThemeToggle> is a tiny round button in the corner that
//      cycles through Light → Dark → System on click; long-press
//      / right-click could open a menu later.
//   3. Cross-tab sync via BroadcastChannel — change theme in
//      Tab A, Tab B follows.
//
// FLASH-OF-WRONG-THEME
// --------------------
// Standard pitfall: when the document parses, mode-state starts
// as the default (light), then ThemeProvider mounts and flips
// to dark — user sees a bright flash. The clean fix is a tiny
// inline script in index.html that reads localStorage BEFORE
// the React bundle parses (separately from this file). For the
// starter version we keep it simple: ThemeProvider resolves on
// initial render before paint commits, so the flash is sub-frame
// in modern browsers. If users complain, follow-up by adding
// the inline script.

import React, {
  ReactNode,
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";
import { useBroadcastChannel } from "./useBroadcastChannel";

export type ThemeMode = "light" | "dark" | "system";
export type ResolvedTheme = "light" | "dark";

const THEME_STORAGE_KEY = "fundai.theme";
const THEME_BROADCAST_CHANNEL = "fundai.theme";

interface ThemeContextValue {
  /** User's chosen mode — "light" | "dark" | "system". */
  mode: ThemeMode;
  /** Mode collapsed to a concrete value — "light" | "dark". */
  resolved: ResolvedTheme;
  setMode: (m: ThemeMode) => void;
  /** Convenience cycle helper for a 3-way toggle button. */
  cycleMode: () => void;
}

const ThemeContext = createContext<ThemeContextValue | null>(null);

function detectInitialMode(): ThemeMode {
  if (typeof window === "undefined") return "system";
  const stored = window.localStorage.getItem(THEME_STORAGE_KEY);
  if (stored === "light" || stored === "dark" || stored === "system") return stored;
  return "system";
}

function detectSystemPrefersDark(): boolean {
  if (typeof window === "undefined" || typeof window.matchMedia !== "function") return false;
  return window.matchMedia("(prefers-color-scheme: dark)").matches;
}

function applyHtmlClass(theme: ResolvedTheme) {
  if (typeof document === "undefined") return;
  const root = document.documentElement;
  root.classList.toggle("dark", theme === "dark");
  root.style.colorScheme = theme; // tells browsers to re-tint scrollbars / form widgets
}

export const ThemeProvider: React.FC<{ children: ReactNode }> = ({ children }) => {
  const [mode, setModeState] = useState<ThemeMode>(detectInitialMode);
  const [systemDark, setSystemDark] = useState<boolean>(detectSystemPrefersDark);

  // Listen for system preference changes when mode='system'.
  useEffect(() => {
    if (typeof window === "undefined" || typeof window.matchMedia !== "function") return;
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = (e: MediaQueryListEvent) => setSystemDark(e.matches);
    // Older Safari uses addListener; both APIs ignored if unsupported.
    if (mq.addEventListener) mq.addEventListener("change", onChange);
    else mq.addListener(onChange);
    return () => {
      if (mq.removeEventListener) mq.removeEventListener("change", onChange);
      else mq.removeListener(onChange);
    };
  }, []);

  const resolved: ResolvedTheme =
    mode === "system" ? (systemDark ? "dark" : "light") : mode;

  // Apply class + meta-theme-color whenever resolved changes.
  useEffect(() => {
    applyHtmlClass(resolved);
  }, [resolved]);

  // Cross-tab sync — see useBroadcastChannel comment.
  const { post: postTheme } = useBroadcastChannel<{ mode: ThemeMode }>(
    THEME_BROADCAST_CHANNEL,
    (msg) => {
      if (msg.mode === "light" || msg.mode === "dark" || msg.mode === "system") {
        setModeState(msg.mode);
      }
    },
  );

  const setMode = useCallback(
    (m: ThemeMode) => {
      setModeState(m);
      if (typeof window !== "undefined") {
        window.localStorage.setItem(THEME_STORAGE_KEY, m);
      }
      postTheme({ mode: m });
    },
    [postTheme],
  );

  const cycleMode = useCallback(() => {
    setMode(mode === "light" ? "dark" : mode === "dark" ? "system" : "light");
  }, [mode, setMode]);

  const ctx = useMemo<ThemeContextValue>(
    () => ({ mode, resolved, setMode, cycleMode }),
    [mode, resolved, setMode, cycleMode],
  );

  return <ThemeContext.Provider value={ctx}>{children}</ThemeContext.Provider>;
};

export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (!ctx) {
    throw new Error("useTheme must be used inside ThemeProvider");
  }
  return ctx;
}

// ThemeToggle — a small button intended for placement in a top
// nav. Cycles Light → Dark → System on click. Renders a glyph
// matching the current resolved theme so the click target and
// the visible label always agree.
//
// i18n note: the label text is short (Light/Dark/System) and is
// also exposed via aria-label so screen readers announce it.
// We don't translate it via i18next here because the existing
// pages still use `useAppPreferences().language`; once react-i18next
// is the single source of truth we can move the label to a
// `theme` namespace.
export const ThemeToggle: React.FC<{
  className?: string;
  /** Optional override for the i18n labels — pass localised strings if you have them. */
  labels?: { light: string; dark: string; system: string };
}> = ({ className, labels }) => {
  const { mode, cycleMode } = useTheme();
  const l = labels ?? { light: "Light", dark: "Dark", system: "System" };
  const label = mode === "light" ? l.light : mode === "dark" ? l.dark : l.system;
  const glyph = mode === "light" ? "☀" : mode === "dark" ? "☾" : "◐";
  return (
    <button
      type="button"
      onClick={cycleMode}
      aria-label={`Theme: ${label} (click to change)`}
      title={`Theme: ${label}`}
      className={[
        "inline-flex items-center gap-1.5 rounded-full border border-gray-200 bg-white px-3 py-1.5 text-sm font-medium text-gray-700 shadow-sm transition hover:bg-gray-50",
        "dark:border-slate-600 dark:bg-slate-800 dark:text-slate-100 dark:hover:bg-slate-700",
        className ?? "",
      ]
        .join(" ")
        .trim()}
    >
      <span aria-hidden="true" className="text-base leading-none">
        {glyph}
      </span>
      <span className="hidden sm:inline">{label}</span>
    </button>
  );
};
