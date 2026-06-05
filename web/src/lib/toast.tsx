// lib/toast.tsx
//
// Lightweight global toast system. Three pieces:
//
//   1. Module-level queue (`pushToast` / `dismissToast`).
//      Lives outside React so non-component modules — most importantly
//      `lib/api.ts` — can emit toasts without holding a hook reference.
//      Subscribers (the React `<ToastViewport />`) re-render on every
//      mutation. State is in-memory, single-tab; the SPA never
//      hydrates from SSR so a module-level mutable cache is fine.
//
//   2. Imperative `toast.{info,success,warn,error}` helpers for
//      callers that don't want to spell out the kind.
//
//   3. `<ToastViewport />` React component that App.tsx mounts once
//      under <BrowserRouter>. It subscribes to the queue, renders a
//      stack of toasts top-right, applies the fund-app's amber/red/
//      green/blue palette consistent with SessionExpiryWatcher, and
//      auto-dismisses each toast after its `duration` (4500ms default
//      for info/success, 6000ms for warn/error since they need more
//      reading time).
//
// i18n: every toast title/message is either a plain string (used in
// both languages) or a `{zh, en}` pair resolved at render time by
// `useAppPreferences().language`. This is the same pattern
// SessionExpiryWatcher uses, so adding toast doesn't introduce a new
// localisation idiom.
//
// Why no Context / no react-toast lib:
//
//   - The repo has zero UI library deps (just react + react-router +
//     tailwind + recharts). Adding a toast lib would mean either a new
//     bundle dep (bundle hit, supply-chain question) or a peer-dep
//     mismatch with React 18. A 200-line in-repo implementation has
//     none of those concerns and the surface is small enough that the
//     ergonomic gain from a third-party lib doesn't justify it.
//   - Context would force every emitter to be inside the provider's
//     subtree. lib/api.ts is not, and importing a hook there would be
//     a layering violation. Module state + a setState-style fan-out
//     keeps the API symmetric for React and non-React callers.

import { useEffect, useState } from "react";
import { useAppPreferences } from "./preferences";

export type ToastKind = "info" | "success" | "warn" | "error";

// Localised string: either a plain string (used in both languages) or
// a {zh, en} pair resolved at render time. Keep the union narrow so
// consumers don't accidentally pass arbitrary objects.
export type Localized = string | { zh: string; en: string };

export interface ToastAction {
  // Localised label rendered as a button at the right side of the toast.
  label: Localized;
  // Click handler. The toast is automatically dismissed after the
  // handler runs so the action button doesn't sit there stale.
  onClick: () => void;
}

export interface ToastInput {
  // Defaults to "info" if omitted.
  kind?: ToastKind;
  title: Localized;
  message?: Localized;
  // 0 disables auto-dismiss (the toast sticks until the user clicks ×
  // or the action button). Negative values are treated as 0.
  // Default: 4500ms for info/success, 6000ms for warn/error.
  duration?: number;
  // Optional inline action (e.g. "Retry"). Clicking dismisses the toast.
  action?: ToastAction;
}

export interface ToastItem {
  id: number;
  kind: ToastKind;
  title: Localized;
  message: Localized;
  duration: number;
  action?: ToastAction;
  // Wall-clock timestamp at push time. Used for dedup and stable
  // ordering when the queue is rendered.
  pushedAt: number;
}

// Module-level queue. Single source of truth for all toasts in the
// SPA; subscribers (currently <ToastViewport />) get a callback on
// every mutation.
const subscribers = new Set<(items: ToastItem[]) => void>();
let queue: ToastItem[] = [];
let nextId = 1;

// Hardcoded ceiling so a runaway loop emitting toasts can't OOM the
// browser. The oldest toast is evicted when the cap is hit. Tuned to
// be high enough that legitimate fan-out (a workflow with 8 steps
// each emitting a status toast) fits comfortably.
const QUEUE_CAP = 8;

// Burst dedup: if the previous toast has the SAME title+message+kind
// AND was pushed within DEDUP_WINDOW_MS, drop the duplicate. This is
// the common shape of "page mount fires 5 parallel queries, all 502".
const DEDUP_WINDOW_MS = 1500;

function emit() {
  for (const sub of subscribers) sub(queue);
}

function localizedKey(value: Localized): string {
  if (typeof value === "string") return value;
  return `${value.zh}\u0001${value.en}`;
}

function isDuplicateOfRecent(input: ToastInput): boolean {
  if (queue.length === 0) return false;
  const last = queue[queue.length - 1];
  if (last.kind !== (input.kind ?? "info")) return false;
  if (localizedKey(last.title) !== localizedKey(input.title)) return false;
  const incomingMessage = input.message ?? "";
  if (localizedKey(last.message) !== localizedKey(incomingMessage)) return false;
  return Date.now() - last.pushedAt < DEDUP_WINDOW_MS;
}

export function pushToast(input: ToastInput): number {
  if (isDuplicateOfRecent(input)) {
    return queue[queue.length - 1].id;
  }
  const kind = input.kind ?? "info";
  const id = nextId++;
  const item: ToastItem = {
    id,
    kind,
    title: input.title,
    message: input.message ?? "",
    duration: typeof input.duration === "number" ? Math.max(0, input.duration) : kind === "warn" || kind === "error" ? 6000 : 4500,
    action: input.action,
    pushedAt: Date.now(),
  };
  queue = [...queue, item];
  if (queue.length > QUEUE_CAP) {
    queue = queue.slice(queue.length - QUEUE_CAP);
  }
  emit();
  if (item.duration > 0) {
    window.setTimeout(() => dismissToast(id), item.duration);
  }
  return id;
}

export function dismissToast(id: number): void {
  const before = queue.length;
  queue = queue.filter((t) => t.id !== id);
  if (queue.length !== before) emit();
}

export function clearToasts(): void {
  if (queue.length === 0) return;
  queue = [];
  emit();
}

// Imperative facade for non-React modules (lib/api.ts is the canonical
// caller). Each method accepts a title, an optional message, and
// optional toast options. Returns the id so the caller can manually
// dismiss if the underlying operation completes early.
export const toast = {
  info(title: Localized, message?: Localized, opts?: Pick<ToastInput, "duration" | "action">): number {
    return pushToast({ kind: "info", title, message, ...opts });
  },
  success(title: Localized, message?: Localized, opts?: Pick<ToastInput, "duration" | "action">): number {
    return pushToast({ kind: "success", title, message, ...opts });
  },
  warn(title: Localized, message?: Localized, opts?: Pick<ToastInput, "duration" | "action">): number {
    return pushToast({ kind: "warn", title, message, ...opts });
  },
  error(title: Localized, message?: Localized, opts?: Pick<ToastInput, "duration" | "action">): number {
    return pushToast({ kind: "error", title, message, ...opts });
  },
};

// React-side hook for components that prefer not to import the
// imperative facade. Returns a stable show/dismiss/list triple.
export function useToast(): {
  toasts: ToastItem[];
  show: (input: ToastInput) => number;
  dismiss: (id: number) => void;
} {
  const [items, setItems] = useState<ToastItem[]>(() => queue);
  useEffect(() => {
    subscribers.add(setItems);
    return () => {
      subscribers.delete(setItems);
    };
  }, []);
  return { toasts: items, show: pushToast, dismiss: dismissToast };
}

// Internal: subscribe-only hook for ToastViewport. Identical to
// useToast minus the show/dismiss helpers (the viewport uses the
// module helpers directly to avoid the indirection).
function useToastList(): ToastItem[] {
  const [items, setItems] = useState<ToastItem[]>(() => queue);
  useEffect(() => {
    subscribers.add(setItems);
    return () => {
      subscribers.delete(setItems);
    };
  }, []);
  return items;
}

// Tailwind palettes per kind. Defined inline so a tree-shake doesn't
// drop the classes (Tailwind JIT scans source files for class name
// strings). Hex values intentionally mirror SessionExpiryWatcher's
// amber palette for "warn" so the visual language is consistent.
const KIND_STYLES: Record<ToastKind, { wrap: string; dot: string; title: string; message: string; dismissBtn: string; actionBtn: string }> = {
  info: {
    wrap: "border-blue-200 bg-blue-50",
    dot: "bg-blue-500",
    title: "text-blue-900",
    message: "text-blue-800",
    dismissBtn: "text-blue-700 hover:bg-blue-100",
    actionBtn: "bg-blue-600 text-white hover:bg-blue-700",
  },
  success: {
    wrap: "border-emerald-200 bg-emerald-50",
    dot: "bg-emerald-500",
    title: "text-emerald-900",
    message: "text-emerald-800",
    dismissBtn: "text-emerald-700 hover:bg-emerald-100",
    actionBtn: "bg-emerald-600 text-white hover:bg-emerald-700",
  },
  warn: {
    wrap: "border-amber-200 bg-amber-50",
    dot: "bg-amber-500",
    title: "text-amber-900",
    message: "text-amber-800",
    dismissBtn: "text-amber-700 hover:bg-amber-100",
    actionBtn: "bg-amber-600 text-white hover:bg-amber-700",
  },
  error: {
    wrap: "border-rose-200 bg-rose-50",
    dot: "bg-rose-500",
    title: "text-rose-900",
    message: "text-rose-800",
    dismissBtn: "text-rose-700 hover:bg-rose-100",
    actionBtn: "bg-rose-600 text-white hover:bg-rose-700",
  },
};

function resolveLocalized(value: Localized, language: string): string {
  if (typeof value === "string") return value;
  return language === "en-US" ? value.en : value.zh;
}

interface ToastViewLabels {
  dismiss: string;
}

function viewLabels(language: string): ToastViewLabels {
  return language === "en-US" ? { dismiss: "Dismiss" } : { dismiss: "关闭" };
}

export function ToastViewport(): JSX.Element | null {
  const items = useToastList();
  const { language } = useAppPreferences();
  if (items.length === 0) return null;
  const labels = viewLabels(language);
  return (
    <div
      className="pointer-events-none fixed right-4 top-4 z-[9998] flex w-full max-w-sm flex-col gap-2"
      role="region"
      aria-label={language === "en-US" ? "Notifications" : "通知"}
    >
      {items.map((item) => {
        const styles = KIND_STYLES[item.kind];
        const titleText = resolveLocalized(item.title, language);
        const messageText = resolveLocalized(item.message, language);
        return (
          <div
            key={item.id}
            role={item.kind === "error" ? "alert" : "status"}
            aria-live={item.kind === "error" ? "assertive" : "polite"}
            className={`pointer-events-auto flex items-start gap-3 rounded-xl border ${styles.wrap} px-4 py-3 shadow-lg`}
          >
            <div className={`mt-1 h-2 w-2 shrink-0 rounded-full ${styles.dot}`} aria-hidden="true" />
            <div className="flex-1 overflow-hidden">
              <p className={`text-sm font-semibold ${styles.title} truncate`}>{titleText}</p>
              {messageText ? <p className={`mt-1 text-xs ${styles.message} break-words`}>{messageText}</p> : null}
              {item.action ? (
                <button
                  type="button"
                  onClick={() => {
                    try {
                      item.action?.onClick();
                    } finally {
                      dismissToast(item.id);
                    }
                  }}
                  className={`mt-2 rounded-md px-3 py-1 text-xs font-medium ${styles.actionBtn}`}
                >
                  {resolveLocalized(item.action.label, language)}
                </button>
              ) : null}
            </div>
            <button
              type="button"
              onClick={() => dismissToast(item.id)}
              className={`rounded-md px-2 py-1 text-xs font-medium ${styles.dismissBtn}`}
              aria-label={labels.dismiss}
            >
              ×
            </button>
          </div>
        );
      })}
    </div>
  );
}
