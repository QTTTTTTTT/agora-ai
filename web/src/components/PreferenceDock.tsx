import React, { useMemo, useState } from "react";
import { useAppPreferences } from "../lib/preferences";
import { ThemeToggle, useTheme } from "../lib/theme";

const PreferenceDock: React.FC = () => {
  const { language, setLanguage, displayCurrency, setDisplayCurrency } = useAppPreferences();
  const { mode } = useTheme();
  const [open, setOpen] = useState(false);
  const isEnglish = language === "en-US";
  const copy = useMemo(
    () =>
      isEnglish
        ? {
            toggle: "Preferences",
            language: "Language",
            display: "Display",
            theme: "Theme",
            themeLight: "Light",
            themeDark: "Dark",
            themeSystem: "System",
            close: "Close preferences",
          }
        : {
            toggle: "偏好",
            language: "语言",
            display: "显示币种",
            theme: "主题",
            themeLight: "浅色",
            themeDark: "深色",
            themeSystem: "跟随系统",
            close: "收起偏好设置",
          },
    [isEnglish],
  );
  const themeLabel = mode === "light" ? copy.themeLight : mode === "dark" ? copy.themeDark : copy.themeSystem;

  return (
    <div className="fixed bottom-4 right-4 z-40 flex flex-col items-end gap-2">
      {open ? (
        <div className="rounded-2xl border border-gray-200 bg-white/95 px-3 py-2 shadow-lg backdrop-blur dark:border-slate-700 dark:bg-slate-800/95">
          <div className="mb-2 flex items-center justify-between gap-3">
            <p className="text-xs font-semibold text-gray-500 dark:text-slate-400">{copy.toggle}</p>
            <button
              type="button"
              onClick={() => setOpen(false)}
              aria-label={copy.close}
              className="rounded-md px-2 py-1 text-xs text-gray-500 transition hover:bg-gray-100 hover:text-gray-700 dark:text-slate-400 dark:hover:bg-slate-700 dark:hover:text-slate-200"
            >
              ×
            </button>
          </div>
          <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
            <label className="flex items-center gap-2 text-xs font-medium text-gray-600 dark:text-slate-300">
              <span>{copy.language}</span>
              <select
                value={language}
                onChange={(event) => setLanguage(event.target.value as "zh-CN" | "en-US")}
                className="rounded-lg border border-gray-300 bg-white px-2 py-1 text-xs text-gray-700 outline-none focus:border-indigo-500 dark:border-slate-600 dark:bg-slate-700 dark:text-slate-100"
              >
                <option value="zh-CN">中文</option>
                <option value="en-US">English</option>
              </select>
            </label>
            <label className="flex items-center gap-2 text-xs font-medium text-gray-600 dark:text-slate-300">
              <span>{copy.display}</span>
              <select
                value={displayCurrency}
                onChange={(event) => setDisplayCurrency(event.target.value as "USD" | "CNY")}
                className="rounded-lg border border-gray-300 bg-white px-2 py-1 text-xs text-gray-700 outline-none focus:border-indigo-500 dark:border-slate-600 dark:bg-slate-700 dark:text-slate-100"
              >
                <option value="USD">USD</option>
                <option value="CNY">CNY</option>
              </select>
            </label>
            <label className="flex items-center gap-2 text-xs font-medium text-gray-600 dark:text-slate-300">
              <span>{copy.theme}</span>
              <ThemeToggle labels={{ light: copy.themeLight, dark: copy.themeDark, system: copy.themeSystem }} />
            </label>
          </div>
        </div>
      ) : null}
      <button
        type="button"
        onClick={() => setOpen((value) => !value)}
        className="rounded-full border border-gray-200 bg-white/90 px-3 py-2 text-xs font-medium text-gray-600 shadow-md backdrop-blur transition hover:bg-white hover:text-gray-900 dark:border-slate-700 dark:bg-slate-800/90 dark:text-slate-300 dark:hover:bg-slate-800 dark:hover:text-slate-100"
      >
        {copy.toggle} · {language === "en-US" ? "EN" : "中"} · {displayCurrency} · {themeLabel}
      </button>
    </div>
  );
};

export default PreferenceDock;
