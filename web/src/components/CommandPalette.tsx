// CommandPalette.tsx — global Cmd+K / Ctrl+K command palette.
//
// WHY THIS EXISTS
// ---------------
// Power users expect Cmd+K. Our app has lots of routes (companies,
// fund dashboards, decision center, AB compare, audit, memory,
// admin, settings, …) and the only way to navigate today is by
// clicking links nested 3-4 levels deep in the layout sidebar.
// For a daily user that's hundreds of unnecessary clicks. The
// palette gives every page a one-keystroke entrypoint.
//
// SCOPE — STARTER VERSION
// -----------------------
// This commit lands a minimal viable palette:
//   - global hotkey: Cmd+K (mac) / Ctrl+K (others) opens it,
//     Esc closes,
//   - typing filters a hard-coded list of navigation commands
//     by case-insensitive substring,
//   - up/down arrows move the highlight, Enter triggers,
//   - the trigger is `navigate(path)` for nav commands; the
//     framework supports arbitrary `action()` callbacks for
//     future commands ("approve all open decisions",
//     "export current view as CSV", "switch theme to dark").
//
// What it does NOT do (yet, on purpose):
//   - search across DATA (e.g. "find fund X by name") — that
//     needs a per-page registration mechanism that future
//     iterations can hang off `useCommandRegistry`,
//   - history of recently-used commands,
//   - fuzzy / typo-tolerant matching (current is plain
//     case-insensitive includes — fine for ~30 commands),
//   - dependency on `cmdk` package — the palette is small
//     enough that pulling another dep + theme integration
//     wasn't worth it for the starter version.
//
// Future-proofing: command list is an array of CommandEntry
// objects so adding a new command is a one-liner; the
// `useCommandRegistry` extension surface (placeholder) is
// where pages will register their own commands once we move
// past navigation.

import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useAppPreferences } from "../lib/preferences";
import { useTheme } from "../lib/theme";

interface CommandEntry {
  id: string;
  /** Localised label (zh-CN / en-US picked at render time). */
  label: { zh: string; en: string };
  /** Optional descriptive subtitle, e.g. category / shortcut hint. */
  hint?: { zh: string; en: string };
  /** Either a route path to navigate to, or an arbitrary action. */
  action: { type: "nav"; to: string } | { type: "fn"; run: () => void };
}

interface CommandPaletteProps {
  /** Optional override for the static command list — use to inject page-specific commands. */
  extraCommands?: CommandEntry[];
}

const STATIC_COMMANDS: CommandEntry[] = [
  // Top-level entry points.
  {
    id: "go-companies",
    label: { zh: "我的公司", en: "My companies" },
    hint: { zh: "首页 / 切换公司", en: "Home / switch company" },
    action: { type: "nav", to: "/companies" },
  },
  {
    id: "go-portfolio-overview",
    label: { zh: "多基金总览", en: "Multi-fund overview" },
    hint: { zh: "聚合所有基金状态", en: "Aggregate state across funds" },
    action: { type: "nav", to: "/portfolio-overview" },
  },
  {
    id: "go-marketplace",
    label: { zh: "市场广场", en: "Marketplace" },
    action: { type: "nav", to: "/marketplace" },
  },
  {
    id: "go-account",
    label: { zh: "账户安全", en: "Account security" },
    action: { type: "nav", to: "/account-security" },
  },
  {
    id: "go-wallet",
    label: { zh: "钱包", en: "Wallet" },
    action: { type: "nav", to: "/wallet" },
  },
  {
    id: "go-kyc",
    label: { zh: "KYC", en: "KYC" },
    action: { type: "nav", to: "/kyc" },
  },
  {
    id: "go-subscription",
    label: { zh: "订阅与套餐", en: "Subscription & plans" },
    action: { type: "nav", to: "/subscription" },
  },
  {
    id: "go-usage",
    label: { zh: "用量明细", en: "Usage" },
    action: { type: "nav", to: "/usage" },
  },
  {
    id: "go-skill-inbox",
    label: { zh: "技能审批 inbox", en: "Skill inbox" },
    action: { type: "nav", to: "/skill-inbox" },
  },
  {
    id: "go-admin",
    label: { zh: "管理后台", en: "Admin console" },
    hint: { zh: "需 admin 角色", en: "Admin role required" },
    action: { type: "nav", to: "/admin" },
  },
];

const THEME_COMMANDS = (cycleTheme: () => void): CommandEntry[] => [
  {
    id: "theme-cycle",
    label: { zh: "切换主题（浅色 / 深色 / 跟随系统）", en: "Cycle theme (Light / Dark / System)" },
    action: { type: "fn", run: cycleTheme },
  },
];

const LANGUAGE_COMMANDS = (
  current: "zh-CN" | "en-US",
  setLanguage: (l: "zh-CN" | "en-US") => void,
): CommandEntry[] => [
  {
    id: "lang-zh",
    label: { zh: "切换为中文", en: "Switch to Chinese" },
    hint: current === "zh-CN" ? { zh: "当前", en: "current" } : undefined,
    action: { type: "fn", run: () => setLanguage("zh-CN") },
  },
  {
    id: "lang-en",
    label: { zh: "切换为英文", en: "Switch to English" },
    hint: current === "en-US" ? { zh: "当前", en: "current" } : undefined,
    action: { type: "fn", run: () => setLanguage("en-US") },
  },
];

export const CommandPalette: React.FC<CommandPaletteProps> = ({ extraCommands = [] }) => {
  const navigate = useNavigate();
  const { language, setLanguage } = useAppPreferences();
  const { cycleMode } = useTheme();
  const isEnglish = language === "en-US";
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [highlight, setHighlight] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);

  const allCommands = useMemo<CommandEntry[]>(
    () => [
      ...STATIC_COMMANDS,
      ...THEME_COMMANDS(cycleMode),
      ...LANGUAGE_COMMANDS(language, setLanguage),
      ...extraCommands,
    ],
    [cycleMode, extraCommands, language, setLanguage],
  );

  const filtered = useMemo(() => {
    const needle = query.trim().toLowerCase();
    if (!needle) return allCommands;
    return allCommands.filter((c) => {
      const label = `${c.label.zh} ${c.label.en} ${c.hint?.zh ?? ""} ${c.hint?.en ?? ""}`.toLowerCase();
      return label.includes(needle);
    });
  }, [query, allCommands]);

  // Reset highlight when filter changes — otherwise typing a query
  // that shrinks the list to fewer items than `highlight` leaves
  // the marker pointing at thin air.
  useEffect(() => {
    setHighlight(0);
  }, [query, open]);

  // Global Cmd+K / Ctrl+K listener. We DON'T bind '/' because it's
  // the universal "find in page" affordance and stealing it confuses
  // power users who expect ctrl+f / cmd+f / '/' to behave as
  // browser-native.
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      const isMod = e.metaKey || e.ctrlKey;
      if (isMod && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setOpen((v) => !v);
      } else if (e.key === "Escape" && open) {
        e.preventDefault();
        setOpen(false);
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [open]);

  // Auto-focus the input when the palette opens. We delay one
  // microtask so the input has actually rendered before we try to
  // focus it — without the rAF, focus() on a not-yet-mounted node
  // is a no-op and the user lands on the modal background.
  useEffect(() => {
    if (open) {
      requestAnimationFrame(() => inputRef.current?.focus());
    } else {
      setQuery("");
    }
  }, [open]);

  const trigger = useCallback(
    (cmd: CommandEntry) => {
      setOpen(false);
      if (cmd.action.type === "nav") {
        navigate(cmd.action.to);
      } else {
        cmd.action.run();
      }
    },
    [navigate],
  );

  const onInputKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLInputElement>) => {
      if (e.key === "ArrowDown") {
        e.preventDefault();
        setHighlight((h) => (filtered.length === 0 ? 0 : (h + 1) % filtered.length));
      } else if (e.key === "ArrowUp") {
        e.preventDefault();
        setHighlight((h) => (filtered.length === 0 ? 0 : (h - 1 + filtered.length) % filtered.length));
      } else if (e.key === "Enter") {
        e.preventDefault();
        const cmd = filtered[highlight];
        if (cmd) trigger(cmd);
      }
    },
    [filtered, highlight, trigger],
  );

  if (!open) return null;

  // Render outside React's normal flow so the modal isn't constrained
  // by any z-index / overflow:hidden ancestor. Tailwind's `fixed
  // inset-0` is good enough; we don't need a Portal here.
  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-label={isEnglish ? "Command palette" : "命令面板"}
      className="fixed inset-0 z-[10000] flex items-start justify-center bg-black/40 px-4 pt-[15vh] backdrop-blur-sm"
      onClick={() => setOpen(false)}
    >
      <div
        // Stop click bubbling so clicks inside the panel don't dismiss it.
        onClick={(e) => e.stopPropagation()}
        className="w-full max-w-xl overflow-hidden rounded-2xl border border-gray-200 bg-white shadow-2xl dark:border-slate-700 dark:bg-slate-900"
      >
        <div className="border-b border-gray-100 px-4 py-3 dark:border-slate-700">
          <input
            ref={inputRef}
            type="text"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={onInputKeyDown}
            placeholder={isEnglish ? "Search commands…  (Esc to close)" : "搜索命令…  (Esc 关闭)"}
            className="w-full bg-transparent text-sm text-gray-900 outline-none placeholder:text-gray-400 dark:text-slate-100 dark:placeholder:text-slate-500"
          />
        </div>
        <ul className="max-h-[50vh] overflow-y-auto py-2" role="listbox">
          {filtered.length === 0 ? (
            <li className="px-4 py-6 text-center text-sm text-gray-500 dark:text-slate-400">
              {isEnglish ? "No matching commands." : "没有匹配的命令。"}
            </li>
          ) : (
            filtered.map((cmd, i) => {
              const label = isEnglish ? cmd.label.en : cmd.label.zh;
              const hint = cmd.hint ? (isEnglish ? cmd.hint.en : cmd.hint.zh) : undefined;
              const active = i === highlight;
              return (
                <li
                  key={cmd.id}
                  role="option"
                  aria-selected={active}
                  className={`flex cursor-pointer items-center justify-between gap-3 px-4 py-2 text-sm transition ${
                    active
                      ? "bg-indigo-50 text-indigo-900 dark:bg-indigo-900/30 dark:text-indigo-100"
                      : "text-gray-700 hover:bg-gray-50 dark:text-slate-200 dark:hover:bg-slate-800"
                  }`}
                  onMouseEnter={() => setHighlight(i)}
                  onClick={() => trigger(cmd)}
                >
                  <span className="truncate font-medium">{label}</span>
                  {hint ? (
                    <span className="ml-3 shrink-0 text-xs text-gray-400 dark:text-slate-500">{hint}</span>
                  ) : null}
                </li>
              );
            })
          )}
        </ul>
        <div className="border-t border-gray-100 px-4 py-2 text-[10px] uppercase tracking-wider text-gray-400 dark:border-slate-700 dark:text-slate-500">
          {isEnglish ? "↑ ↓ navigate · ↵ select · esc close" : "↑ ↓ 选择 · ↵ 执行 · esc 关闭"}
        </div>
      </div>
    </div>
  );
};

export default CommandPalette;
