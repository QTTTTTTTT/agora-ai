// TabPills — the dual-state segmented tab from the reference's
// "团队总览 / 编辑配置". The active tab is a black filled pill;
// the inactive one is a flat ghost label. Designed for horizontal
// scrolls of 2-4 tabs; for longer sets prefer a real <Tabs/>.

import React from "react";

export interface TabPillItem<K extends string = string> {
  key: K;
  label: React.ReactNode;
}

export interface TabPillsProps<K extends string = string> {
  tabs: ReadonlyArray<TabPillItem<K>>;
  active: K;
  onChange: (key: K) => void;
  /** Visually balance the active pill width so a long label
   *  doesn't shrink the inactive one. Defaults to true. */
  equalWidth?: boolean;
  className?: string;
}

export function TabPills<K extends string = string>({
  tabs,
  active,
  onChange,
  equalWidth = true,
  className = "",
}: TabPillsProps<K>) {
  return (
    <div
      role="tablist"
      className={[
        "inline-flex items-center rounded-full bg-cream-100/60 p-1",
        "ring-1 ring-ink-100/70",
        "dark:bg-slate-800/60 dark:ring-slate-700",
        className,
      ].join(" ")}
    >
      {tabs.map((t) => {
        const isActive = t.key === active;
        return (
          <button
            key={t.key}
            type="button"
            role="tab"
            aria-selected={isActive}
            onClick={() => onChange(t.key)}
            className={[
              "rounded-full px-5 py-2 text-sm font-semibold transition",
              equalWidth ? "min-w-[110px]" : "",
              isActive
                ? "bg-ink-900 text-white shadow-pill-ink"
                : "text-ink-300 hover:text-ink-700 dark:text-slate-400 dark:hover:text-slate-200",
            ].join(" ")}
          >
            {t.label}
          </button>
        );
      })}
    </div>
  );
}
