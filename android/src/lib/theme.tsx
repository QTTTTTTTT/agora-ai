/**
 * theme — 颜色 token + 用户偏好（system / light / dark）。
 *
 * - useColorScheme() 读系统外观；当 preference=system 时跟随。
 * - 用户选了 light/dark 后存 MMKV，启动恢复。
 * - 暴露 useTheme()：返回 { mode, colors, setPreference } 供 UI 用。
 *
 * 我们故意不引 react-native-paper / tamagui 主题切换 — Sprint 3
 * skeleton 用 StyleSheet 平铺，主题只是几个 token：bg / surface /
 * text / textMuted / accent / danger / amber / success / border。
 * 全 app 改 dark mode 走"读 useTheme().colors 并把 StyleSheet 改成
 * style 函数"的渐进迁移路径，足够。
 */

import React, { createContext, useContext, useEffect, useMemo, useState } from 'react';
import { useColorScheme } from 'react-native';

export type ThemePreference = 'system' | 'light' | 'dark';
export type ThemeMode = 'light' | 'dark';

// Cream / sage / ink design refresh.
//
// Existing fields (`bg`, `surface`, `text`, `accent`...) keep
// the same role they had pre-refresh so screens that haven't been
// converted yet automatically pick up the lighter palette. New
// fields are surface-typed companion tokens used by the cream
// components (`pillSage`, `pillCoral`, `ink`, `cream`, etc.)
// — these power the rounded-envelope cards, pill chips, and
// black-pill CTAs that mirror the web/miniapp refresh.
export interface ThemeColors {
  bg: string;          // page background
  surface: string;     // primary card surface (white-ish)
  surfaceAlt: string;  // tertiary surface (cream paper for inner panels)
  text: string;        // ink-900 (display headlines)
  textMuted: string;   // ink-300 (caption / supporting text)
  accent: string;      // sage-500 (primary brand color in refresh)
  danger: string;      // risk-400 (red)
  amber: string;       // coral-300 (warning)
  success: string;     // sage-500
  border: string;      // ink-100 hairline
  // Refresh additions
  ink: string;         // primary CTA fill (#111110)
  inkAlt: string;      // secondary CTA / hover state
  cream: string;       // soft envelope paper
  sage: string;        // accent green chip bg
  sageStrong: string;  // sage-700 text on chip
  coral: string;       // accent coral chip bg
  coralStrong: string; // coral-500 text on chip
  risk: string;        // risk chip bg
  riskStrong: string;  // risk-500 text on chip
}

const lightColors: ThemeColors = {
  bg: '#f4f2ee',
  surface: '#ffffff',
  surfaceAlt: '#faf7f1',
  text: '#1f1d18',
  textMuted: '#7a766c',
  accent: '#1faa64',
  danger: '#e64949',
  amber: '#f7a05d',
  success: '#1faa64',
  border: '#e5e3dd',
  ink: '#111110',
  inkAlt: '#1f1d18',
  cream: '#f4f2ee',
  sage: '#dcebd6',
  sageStrong: '#0f6a3f',
  coral: '#fbe1d1',
  coralStrong: '#dc6f24',
  risk: '#fad1d3',
  riskStrong: '#c5343a',
};

const darkColors: ThemeColors = {
  bg: '#0b0d12',
  surface: '#161922',
  surfaceAlt: '#1f2330',
  text: '#f3f4f6',
  textMuted: '#9ca3af',
  accent: '#34d399',
  danger: '#f87171',
  amber: '#f59e0b',
  success: '#34d399',
  border: '#374151',
  ink: '#0a0a0c',
  inkAlt: '#161922',
  cream: '#1f2330',
  sage: '#1f3a32',
  sageStrong: '#9ec99a',
  coral: '#3c2a20',
  coralStrong: '#f7a05d',
  risk: '#3a1f22',
  riskStrong: '#f87171',
};

interface MmkvLike {
  getString(key: string): string | undefined;
  set(key: string, value: string): void;
}
interface MmkvCtor {
  new (cfg: { id: string }): MmkvLike;
}

let mmkvCtor: MmkvCtor | null = null;
try {
  mmkvCtor = (require('react-native-mmkv') as { MMKV: MmkvCtor }).MMKV;
} catch {
  mmkvCtor = null;
}
const KEY = 'themePreference';
let store: MmkvLike | null = null;
function getStore(): MmkvLike | null {
  if (store) return store;
  if (!mmkvCtor) return null;
  store = new mmkvCtor({ id: 'fundai-prefs' });
  return store;
}

function loadPref(): ThemePreference {
  const s = getStore();
  const raw = s?.getString(KEY);
  if (raw === 'light' || raw === 'dark' || raw === 'system') return raw;
  return 'system';
}

function savePref(value: ThemePreference): void {
  const s = getStore();
  s?.set(KEY, value);
}

interface ThemeContextValue {
  preference: ThemePreference;
  mode: ThemeMode;
  colors: ThemeColors;
  setPreference: (p: ThemePreference) => void;
}

const ThemeContext = createContext<ThemeContextValue | null>(null);

export function ThemeProvider({ children }: { children: React.ReactNode }): JSX.Element {
  const systemScheme = useColorScheme();
  const [pref, setPref] = useState<ThemePreference>(() => loadPref());

  useEffect(() => {
    savePref(pref);
  }, [pref]);

  const mode: ThemeMode = pref === 'system' ? (systemScheme === 'dark' ? 'dark' : 'light') : pref;
  const colors = mode === 'dark' ? darkColors : lightColors;

  const value = useMemo<ThemeContextValue>(
    () => ({ preference: pref, mode, colors, setPreference: setPref }),
    [pref, mode, colors],
  );

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

export function useTheme(): ThemeContextValue {
  const v = useContext(ThemeContext);
  if (!v) throw new Error('useTheme must be used inside a <ThemeProvider>');
  return v;
}
