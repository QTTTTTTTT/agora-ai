// compliance.tsx — front-end compliance scaffolding.
//
// Three responsibilities, kept in one module so legal review
// only has to look at one file:
//
//   1. ComplianceProvider — React context that exposes the
//      server's current compliance mode + the localised
//      disclosure text bundle. Fetched once on app boot;
//      consumed by ComplianceBanner, ComplianceAckModal, and
//      individual surface components (PaperTrading, Advisor,
//      Backtest) that need their per-surface text.
//
//   2. AdviceVerb / formatModelAction — the wording layer.
//      The backend's enum-style verdicts (BUY / HOLD / AVOID /
//      STRONG_BUY) and order actions (BUY / SELL / REBALANCE)
//      are valid internal values but MUST NOT be rendered to
//      users as bare imperatives under Publisher mode. This
//      module maps them to compliant phrasing such as "Model
//      action: signal active" / "Model allocation".
//
//   3. complianceApi — thin wrappers around the four
//      /api/compliance/* endpoints exposed by the backend.
//
// The whole module is safe to import in any environment:
//   - Server-rendered (no window access in module body).
//   - Test environments where fetch isn't wired (the provider
//     defaults to the strictest Publisher-mode bundle and the
//     UI still renders).
//
// IMPORTANT: every default falls back to the Publisher posture.
// Forgetting to wire a server call must NEVER drop a render to
// the laxer RIA mode (which would skip the "(model action)"
// labels and the disclaimer modal).

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

import { apiRequest } from "./api";

export type ComplianceMode = "publisher" | "ria_registered";
export type ComplianceSurface =
  | "advisor"
  | "paper_trading"
  | "backtest"
  | "cn_intraday"
  | "daily_picks"
  | "global";
export type ComplianceLocale = "zh-CN" | "en-US";

// ServerDisclosureBundle is exactly what
// GET /api/compliance/disclosure returns. Kept narrow so the
// shape change story is visible at a diff.
export type ServerDisclosureBundle = {
  mode: ComplianceMode;
  surface: ComplianceSurface;
  locale: string;
  disclosure: string;
  acknowledgmentText: string;
  hypotheticalPerformanceDisclaimer: string;
};

export type ComplianceAck = {
  id: string;
  surface: ComplianceSurface;
  mode: ComplianceMode;
  locale: string;
  acknowledgedAt: string;
  acknowledgedText?: string;
  textVersion: number;
};

export type ComplianceAckInput = {
  surface: ComplianceSurface;
  mode?: ComplianceMode;
  locale?: string;
  acknowledgedText: string;
  textVersion?: number;
};

// --- API wrappers ---------------------------------------------------------

export const complianceApi = {
  async getDisclosure(
    surface: ComplianceSurface,
    locale: ComplianceLocale,
  ): Promise<ServerDisclosureBundle> {
    const params = new URLSearchParams({ surface, locale });
    return apiRequest<ServerDisclosureBundle>(
      `/api/compliance/disclosure?${params.toString()}`,
    );
  },

  async recordAcknowledgment(input: ComplianceAckInput): Promise<ComplianceAck> {
    return apiRequest<ComplianceAck>(`/api/compliance/acknowledgments`, {
      method: "POST",
      body: JSON.stringify(input),
    });
  },

  async listAcknowledgments(): Promise<ComplianceAck[]> {
    return apiRequest<ComplianceAck[]>(`/api/compliance/acknowledgments`);
  },
};

// --- Static default bundle (used before the server responds) -------------

const fallbackPublisherEN: ServerDisclosureBundle = {
  mode: "publisher",
  surface: "advisor",
  locale: "en",
  disclosure:
    "⚠ This service is NOT a registered investment adviser. All content is impersonal, general market analysis under named investment frameworks (Buffett / Lynch / etc.) and is provided for education only. Nothing here is a recommendation to buy or sell any security. You are solely responsible for your investment decisions.",
  acknowledgmentText:
    "I understand that this platform is NOT a registered investment adviser, that all content is general market analysis made available to every subscriber equally, that it is NOT tailored to my individual circumstances, and that I am solely responsible for my investment decisions.",
  hypotheticalPerformanceDisclaimer:
    "Backtest results are hypothetical and do NOT represent actual trading. Past performance does not guarantee future results.",
};

const fallbackPublisherZH: ServerDisclosureBundle = {
  ...fallbackPublisherEN,
  locale: "zh",
  disclosure:
    "⚠ 本服务非注册投资顾问。所有内容为基于公开数据的非个性化框架分析（如巴菲特/林奇等投资框架），仅供教育与研究用途，不构成对任何证券的买入或卖出建议。投资决策由您本人独立做出。",
  acknowledgmentText:
    "我理解本平台并非注册投资顾问，所有内容为通用市场分析与教育，不构成针对个人的投资建议；所有订阅用户看到的是相同的非个性化内容，本人为投资决策的唯一负责人。",
  hypotheticalPerformanceDisclaimer:
    "回测业绩为基于历史数据的假设性结果，未实际执行交易。过往业绩不代表未来收益。",
};

function fallbackBundle(locale: ComplianceLocale): ServerDisclosureBundle {
  return locale === "zh-CN" ? fallbackPublisherZH : fallbackPublisherEN;
}

// --- Context --------------------------------------------------------------

type ComplianceContextValue = {
  mode: ComplianceMode;
  ready: boolean;
  bundle: (surface: ComplianceSurface) => ServerDisclosureBundle;
  recordAck: (input: ComplianceAckInput) => Promise<ComplianceAck>;
  isAcknowledged: (surface: ComplianceSurface) => boolean;
  refresh: () => Promise<void>;
};

const ComplianceContext = createContext<ComplianceContextValue | undefined>(
  undefined,
);

export type ComplianceProviderProps = {
  children: ReactNode;
  locale: ComplianceLocale;
};

const LOCAL_ACK_KEY = "fundai.compliance.acks";

function readLocalAcks(): Record<string, true> {
  if (typeof window === "undefined") return {};
  try {
    const raw = window.localStorage.getItem(LOCAL_ACK_KEY);
    if (!raw) return {};
    const parsed = JSON.parse(raw) as Record<string, true>;
    return parsed && typeof parsed === "object" ? parsed : {};
  } catch {
    return {};
  }
}

function writeLocalAck(surface: ComplianceSurface) {
  if (typeof window === "undefined") return;
  try {
    const current = readLocalAcks();
    current[surface] = true;
    current.global = true;
    window.localStorage.setItem(LOCAL_ACK_KEY, JSON.stringify(current));
  } catch {
    // ignore quota errors — the server-side ack is the source of truth
  }
}

export function ComplianceProvider({ children, locale }: ComplianceProviderProps) {
  const [mode, setMode] = useState<ComplianceMode>("publisher");
  const [bundles, setBundles] = useState<
    Partial<Record<ComplianceSurface, ServerDisclosureBundle>>
  >({});
  const [ready, setReady] = useState(false);
  const [localAcks, setLocalAcks] = useState<Record<string, true>>(() =>
    readLocalAcks(),
  );

  const fetchBundle = useCallback(
    async (surface: ComplianceSurface) => {
      try {
        const b = await complianceApi.getDisclosure(surface, locale);
        setBundles((prev) => ({ ...prev, [surface]: b }));
        if (b.mode === "ria_registered" || b.mode === "publisher") {
          setMode(b.mode);
        }
        return b;
      } catch {
        // Fall back to the bundled defaults — server is unreachable
        // or auth-rejected; we still want the page to render with
        // a safe Publisher-mode disclosure.
        const b = fallbackBundle(locale);
        setBundles((prev) => ({ ...prev, [surface]: b }));
        return b;
      }
    },
    [locale],
  );

  // We sequence the 4 surface fetches one-after-another instead of
  // Promise.all'ing them so the dev server (and proxies) only see
  // one in-flight request at a time per page mount. Promise.all
  // turned out to fan out to a flood whenever the locale-derived
  // ref re-stabilised, which made the server log look like a
  // hot-loop. Sequential is fine here — the four disclosures are
  // tiny and cached.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      const surfaces: ComplianceSurface[] = [
        "advisor",
        "paper_trading",
        "backtest",
        "cn_intraday",
        "daily_picks",
      ];
      for (const s of surfaces) {
        if (cancelled) return;
        await fetchBundle(s);
      }
      if (!cancelled) setReady(true);
    })();
    return () => {
      cancelled = true;
    };
  }, [fetchBundle]);

  const bundle = useCallback(
    (surface: ComplianceSurface): ServerDisclosureBundle => {
      const found = bundles[surface];
      if (found) return found;
      return { ...fallbackBundle(locale), surface };
    },
    [bundles, locale],
  );

  const recordAck = useCallback(
    async (input: ComplianceAckInput) => {
      const b = bundles[input.surface] ?? fallbackBundle(locale);
      const payload: ComplianceAckInput = {
        ...input,
        mode: input.mode ?? mode,
        locale: input.locale ?? b.locale,
        acknowledgedText: input.acknowledgedText || b.acknowledgmentText,
      };
      const ack = await complianceApi.recordAcknowledgment(payload);
      writeLocalAck(input.surface);
      setLocalAcks((prev) => ({ ...prev, [input.surface]: true, global: true }));
      return ack;
    },
    [bundles, locale, mode],
  );

  const isAcknowledged = useCallback(
    (surface: ComplianceSurface) => {
      if (mode === "ria_registered") return true;
      return Boolean(localAcks[surface] || localAcks.global);
    },
    [localAcks, mode],
  );

  const refresh = useCallback(async () => {
    await Promise.all(
      (
        ["advisor", "paper_trading", "backtest", "cn_intraday", "daily_picks"] as const
      ).map((s) => fetchBundle(s)),
    );
  }, [fetchBundle]);

  const value = useMemo<ComplianceContextValue>(
    () => ({ mode, ready, bundle, recordAck, isAcknowledged, refresh }),
    [mode, ready, bundle, recordAck, isAcknowledged, refresh],
  );

  return (
    <ComplianceContext.Provider value={value}>
      {children}
    </ComplianceContext.Provider>
  );
}

// useCompliance is the consumer hook. Throws if used outside a
// provider — that's intentional, every page that renders should
// be wrapped at the App level.
export function useCompliance(): ComplianceContextValue {
  const ctx = useContext(ComplianceContext);
  if (!ctx) {
    throw new Error("useCompliance must be used inside ComplianceProvider");
  }
  return ctx;
}

// --- Wording layer --------------------------------------------------------

// Verdict / order-action enums the backend may hand us. Keep the
// union narrow so a typo in a translation map gets caught.
export type VerdictEnum =
  | "STRONG_BUY"
  | "BUY"
  | "HOLD"
  | "AVOID"
  | "STRONG_AVOID"
  | "SHORT"
  | "PASS"
  | "SKIP";

export type OrderActionEnum = "BUY" | "SELL" | "REBALANCE";

// formatModelVerdict renders a verdict in a Publisher-safe way.
// In Publisher mode the verb-style enums are wrapped in
// "Model rating: …" / "本模型评级：…" so they read as ratings
// not recommendations. In RIA mode the bare verdict is fine.
export function formatModelVerdict(
  verdict: string,
  locale: ComplianceLocale,
  mode: ComplianceMode,
): string {
  const raw = (verdict || "").toUpperCase();
  if (!raw) return "";
  if (mode === "ria_registered") return raw;
  if (locale === "zh-CN") {
    const map: Record<string, string> = {
      STRONG_BUY: "强买入信号",
      BUY: "买入信号",
      HOLD: "观望信号",
      AVOID: "回避信号",
      STRONG_AVOID: "强回避信号",
      SHORT: "看空信号",
      PASS: "不满足模型条件",
      SKIP: "跳过 / 数据不足",
    };
    const label = map[raw] ?? raw;
    return `本模型评级：${label}`;
  }
  const map: Record<string, string> = {
    STRONG_BUY: "strong-buy signal",
    BUY: "buy signal",
    HOLD: "hold signal",
    AVOID: "avoid signal",
    STRONG_AVOID: "strong-avoid signal",
    SHORT: "short signal",
    PASS: "does not meet model criteria",
    SKIP: "skipped (insufficient data)",
  };
  const label = map[raw] ?? raw.toLowerCase();
  return `Model rating: ${label}`;
}

// formatModelAction is the order-side analog for the Paper
// Trading order ledger.
export function formatModelAction(
  action: string,
  locale: ComplianceLocale,
  mode: ComplianceMode,
): string {
  const raw = (action || "").toUpperCase();
  if (!raw) return "";
  if (mode === "ria_registered") return raw;
  if (locale === "zh-CN") {
    const map: Record<string, string> = {
      BUY: "模型动作：建仓",
      SELL: "模型动作：减仓",
      REBALANCE: "模型动作：再平衡",
    };
    return map[raw] ?? `模型动作：${raw}`;
  }
  const map: Record<string, string> = {
    BUY: "Model action: open",
    SELL: "Model action: close",
    REBALANCE: "Model action: rebalance",
  };
  return map[raw] ?? `Model action: ${raw.toLowerCase()}`;
}

// formatPriceLabel renames "Target price" / "Stop loss" in
// Publisher mode so the value reads as a model-state marker.
// Pass the raw English/Chinese label intent and the function
// picks the compliant phrasing.
export type PriceLabelKind = "target" | "stopLoss" | "entryLow" | "entryHigh";

export function formatPriceLabel(
  kind: PriceLabelKind,
  locale: ComplianceLocale,
  mode: ComplianceMode,
): string {
  if (mode === "ria_registered") {
    if (locale === "zh-CN") {
      return {
        target: "目标价",
        stopLoss: "止损价",
        entryLow: "入场价下限",
        entryHigh: "入场价上限",
      }[kind];
    }
    return {
      target: "Target price",
      stopLoss: "Stop loss",
      entryLow: "Entry low",
      entryHigh: "Entry high",
    }[kind];
  }
  if (locale === "zh-CN") {
    return {
      target: "模型目标位",
      stopLoss: "模型减仓触发位",
      entryLow: "模型建仓下沿",
      entryHigh: "模型建仓上沿",
    }[kind];
  }
  return {
    target: "Model target level",
    stopLoss: "Model stop-loss trigger",
    entryLow: "Model entry-low level",
    entryHigh: "Model entry-high level",
  }[kind];
}
