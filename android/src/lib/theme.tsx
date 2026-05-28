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

export interface ThemeColors {
  bg: string;
  surface: string;
  surfaceAlt: string;
  text: string;
  textMuted: string;
  accent: string;
  danger: string;
  amber: string;
  success: string;
  border: string;
}

const lightColors: ThemeColors = {
  bg: '#f3f4f6',
  surface: '#ffffff',
  surfaceAlt: '#f9fafb',
  text: '#111827',
  textMuted: '#6b7280',
  accent: '#4f46e5',
  danger: '#dc2626',
  amber: '#fed7aa',
  success: '#059669',
  border: '#d1d5db',
};

const darkColors: ThemeColors = {
  bg: '#0b0d12',
  surface: '#161922',
  surfaceAlt: '#1f2330',
  text: '#f3f4f6',
  textMuted: '#9ca3af',
  accent: '#818cf8',
  danger: '#f87171',
  amber: '#f59e0b',
  success: '#34d399',
  border: '#374151',
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
