// AdminWSFeedSection — admin UI for the S6.5 WebSocket
// real-time market-data plumbing.
//
// Capability surface
//
//   - Status card: enabled / healthy providers / subscriptions
//     / cached symbols / total ticks / dropped events.
//   - Connections table: per-provider state, ticks, reconnect
//     count, last error.
//   - Subscriptions table: who's subscribed to which symbol
//     plus consumer count.
//   - Cache table: last-tick snapshot per symbol with bid/ask
//     and stale flag; quick per-row evict + "evict all".
//   - Manual subscribe / unsubscribe form for ad-hoc symbols.
//   - "Reconcile subscriptions" button: force the bridge to
//     re-diff held positions against current subscriptions.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  evictAdminWSFeedCache,
  formatApiError,
  getAdminWSFeedStatus,
  listAdminWSFeedCache,
  listAdminWSFeedConnections,
  listAdminWSFeedSubscriptions,
  reconcileAdminWSFeed,
  subscribeAdminWSFeed,
  unsubscribeAdminWSFeed,
} from "../lib/api";
import type {
  WSFeedConnection,
  WSFeedCacheListResponse,
  WSFeedState,
  WSFeedStatus,
  WSFeedSubscription,
} from "@fundai/api-client";

type Language = "zh-CN" | "en-US";

interface Messages {
  panelTitle: string;
  panelSubtitle: string;
  disabled: string;
  refresh: string;
  reconcile: string;
  reconcileSubmitting: string;
  statusEnabled: string;
  statusHealthyProviders: string;
  statusSubscriptions: string;
  statusCacheSymbols: string;
  statusTotalTicks: string;
  statusDroppedEvents: string;
  connectionsTitle: string;
  connectionsEmpty: string;
  colProvider: string;
  colState: string;
  colTickCount: string;
  colReconnects: string;
  colLastTick: string;
  colConnectedAt: string;
  colLastError: string;
  subscriptionsTitle: string;
  subscriptionsEmpty: string;
  colSymbol: string;
  colMarket: string;
  colConsumers: string;
  cacheTitle: string;
  cacheStats: string;
  cacheEmpty: string;
  colLast: string;
  colBid: string;
  colAsk: string;
  colAsOf: string;
  colStale: string;
  subscribeTitle: string;
  subscribeSymbolPlaceholder: string;
  subscribeMarketPlaceholder: string;
  subscribeSubmit: string;
  subscribeSubmitting: string;
  unsubscribeButton: string;
  evictCacheButton: string;
  evictCacheAllButton: string;
  error: string;
  yes: string;
  no: string;
}

const messages: Record<Language, Messages> = {
  "zh-CN": {
    panelTitle: "S6.5 · WebSocket 实时行情",
    panelSubtitle:
      "把 broker 撮合 + 持仓刷新的报价来源从 REST 轮询换成 push tick。配置 WSFEED_ENABLED=true 并配上 provider（mock / 真实券商）后，所有热路径优先读 WS-cache，cache miss 或 stale 时自动回退 REST。",
    disabled: "当前禁用：",
    refresh: "刷新",
    reconcile: "立即对齐订阅",
    reconcileSubmitting: "对齐中…",
    statusEnabled: "运行中",
    statusHealthyProviders: "健康 provider",
    statusSubscriptions: "订阅数",
    statusCacheSymbols: "缓存合约",
    statusTotalTicks: "累计 tick 数",
    statusDroppedEvents: "丢弃事件",
    connectionsTitle: "上游连接",
    connectionsEmpty: "未注册任何 provider",
    colProvider: "Provider",
    colState: "状态",
    colTickCount: "Tick 数",
    colReconnects: "重连",
    colLastTick: "最近 tick",
    colConnectedAt: "连接时间",
    colLastError: "最近错误",
    subscriptionsTitle: "当前订阅",
    subscriptionsEmpty: "当前没有任何订阅",
    colSymbol: "合约",
    colMarket: "市场",
    colConsumers: "订阅方",
    cacheTitle: "Quote Cache",
    cacheStats: "命中 / 错失 / 过期 / 淘汰",
    cacheEmpty: "缓存为空",
    colLast: "最新",
    colBid: "Bid",
    colAsk: "Ask",
    colAsOf: "时间",
    colStale: "过期",
    subscribeTitle: "手动订阅",
    subscribeSymbolPlaceholder: "合约（如 AAPL）",
    subscribeMarketPlaceholder: "市场（如 US）",
    subscribeSubmit: "订阅",
    subscribeSubmitting: "提交中…",
    unsubscribeButton: "退订",
    evictCacheButton: "清理本行",
    evictCacheAllButton: "清空缓存",
    error: "加载失败",
    yes: "是",
    no: "否",
  },
  "en-US": {
    panelTitle: "S6.5 · Real-time market data (WS)",
    panelSubtitle:
      "Replace REST polling on the broker / position-refresh hot paths with pushed ticks. Set WSFEED_ENABLED=true plus a provider; cache misses fall back to REST transparently.",
    disabled: "Currently disabled: ",
    refresh: "Refresh",
    reconcile: "Reconcile subscriptions",
    reconcileSubmitting: "Reconciling…",
    statusEnabled: "Running",
    statusHealthyProviders: "Healthy providers",
    statusSubscriptions: "Subscriptions",
    statusCacheSymbols: "Cached symbols",
    statusTotalTicks: "Total ticks",
    statusDroppedEvents: "Dropped events",
    connectionsTitle: "Upstream connections",
    connectionsEmpty: "No providers registered",
    colProvider: "Provider",
    colState: "State",
    colTickCount: "Ticks",
    colReconnects: "Reconnects",
    colLastTick: "Last tick",
    colConnectedAt: "Connected at",
    colLastError: "Last error",
    subscriptionsTitle: "Active subscriptions",
    subscriptionsEmpty: "No active subscriptions",
    colSymbol: "Symbol",
    colMarket: "Market",
    colConsumers: "Consumers",
    cacheTitle: "Quote cache",
    cacheStats: "Hits / Misses / Stale / Evicts",
    cacheEmpty: "Cache is empty",
    colLast: "Last",
    colBid: "Bid",
    colAsk: "Ask",
    colAsOf: "As of",
    colStale: "Stale",
    subscribeTitle: "Manual subscribe",
    subscribeSymbolPlaceholder: "Symbol (e.g. AAPL)",
    subscribeMarketPlaceholder: "Market (e.g. US)",
    subscribeSubmit: "Subscribe",
    subscribeSubmitting: "Submitting…",
    unsubscribeButton: "Unsubscribe",
    evictCacheButton: "Evict row",
    evictCacheAllButton: "Evict all",
    error: "Failed to load",
    yes: "yes",
    no: "no",
  },
};

interface AdminWSFeedSectionProps {
  language?: Language;
}

function fmtIsoOrDash(iso?: string): string {
  if (!iso) return "—";
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}

function fmtNumber(n: number | undefined, digits = 2): string {
  if (n == null || !Number.isFinite(n)) return "—";
  return n.toFixed(digits);
}

function stateColor(state: WSFeedState): string {
  switch (state) {
    case "connected":
      return "bg-green-100 text-green-800";
    case "connecting":
    case "reconnecting":
      return "bg-yellow-100 text-yellow-800";
    case "backoff":
    case "disconnected":
      return "bg-orange-100 text-orange-800";
    case "closed":
      return "bg-gray-200 text-gray-700";
    default:
      return "bg-gray-100 text-gray-600";
  }
}

export default function AdminWSFeedSection({
  language = "zh-CN",
}: AdminWSFeedSectionProps) {
  const t = useMemo(() => messages[language] ?? messages["zh-CN"], [language]);

  const [status, setStatus] = useState<WSFeedStatus | null>(null);
  const [connections, setConnections] = useState<WSFeedConnection[]>([]);
  const [subscriptions, setSubscriptions] = useState<WSFeedSubscription[]>([]);
  const [cacheList, setCacheList] = useState<WSFeedCacheListResponse | null>(
    null,
  );
  const [error, setError] = useState<string | null>(null);
  const [reconciling, setReconciling] = useState(false);
  const [subscribeSym, setSubscribeSym] = useState("");
  const [subscribeMarket, setSubscribeMarket] = useState("");
  const [subscribing, setSubscribing] = useState(false);

  const refresh = useCallback(async () => {
    setError(null);
    try {
      const [s, c, sub, cache] = await Promise.all([
        getAdminWSFeedStatus().catch(() => null),
        listAdminWSFeedConnections().catch(() => ({ connections: [] })),
        listAdminWSFeedSubscriptions().catch(() => ({ subscriptions: [] })),
        listAdminWSFeedCache().catch(() => null),
      ]);
      setStatus(s);
      setConnections(c?.connections ?? []);
      setSubscriptions(sub?.subscriptions ?? []);
      setCacheList(cache);
    } catch (e) {
      setError(formatApiError(e, "request failed"));
    }
  }, []);

  useEffect(() => {
    void refresh();
    const id = setInterval(refresh, 15000);
    return () => clearInterval(id);
  }, [refresh]);

  const onReconcile = useCallback(async () => {
    setReconciling(true);
    try {
      await reconcileAdminWSFeed();
      await refresh();
    } catch (e) {
      setError(formatApiError(e, "request failed"));
    } finally {
      setReconciling(false);
    }
  }, [refresh]);

  const onSubscribe = useCallback(async () => {
    const sym = subscribeSym.trim();
    if (!sym) return;
    setSubscribing(true);
    try {
      await subscribeAdminWSFeed(sym, subscribeMarket.trim() || undefined);
      setSubscribeSym("");
      setSubscribeMarket("");
      await refresh();
    } catch (e) {
      setError(formatApiError(e, "request failed"));
    } finally {
      setSubscribing(false);
    }
  }, [subscribeSym, subscribeMarket, refresh]);

  const onUnsubscribe = useCallback(
    async (symbol: string) => {
      try {
        await unsubscribeAdminWSFeed(symbol);
        await refresh();
      } catch (e) {
        setError(formatApiError(e, "request failed"));
      }
    },
    [refresh],
  );

  const onEvictOne = useCallback(
    async (symbol: string) => {
      try {
        await evictAdminWSFeedCache(symbol);
        await refresh();
      } catch (e) {
        setError(formatApiError(e, "request failed"));
      }
    },
    [refresh],
  );

  const onEvictAll = useCallback(async () => {
    try {
      await evictAdminWSFeedCache("*");
      await refresh();
    } catch (e) {
      setError(formatApiError(e, "request failed"));
    }
  }, [refresh]);

  const disabled = status && !status.enabled;

  return (
    <section className="rounded-2xl border border-slate-200 bg-white p-6 shadow-sm">
      <div className="mb-4 flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold text-slate-900">
            {t.panelTitle}
          </h2>
          <p className="mt-1 max-w-3xl text-sm text-slate-600">
            {t.panelSubtitle}
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          <button
            type="button"
            onClick={() => void refresh()}
            className="rounded-md border border-slate-300 bg-white px-3 py-1.5 text-sm font-medium text-slate-700 shadow-sm hover:bg-slate-50"
          >
            {t.refresh}
          </button>
          <button
            type="button"
            onClick={() => void onReconcile()}
            disabled={reconciling || !!disabled}
            className="rounded-md border border-indigo-300 bg-indigo-50 px-3 py-1.5 text-sm font-medium text-indigo-700 shadow-sm hover:bg-indigo-100 disabled:opacity-50"
          >
            {reconciling ? t.reconcileSubmitting : t.reconcile}
          </button>
        </div>
      </div>

      {error ? (
        <div className="mb-4 rounded-md border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700">
          {t.error}: {error}
        </div>
      ) : null}

      {disabled ? (
        <div className="rounded-md border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-800">
          <strong>{t.disabled}</strong>
          {" "}
          {status?.reason ?? "WSFEED_ENABLED=false"}
        </div>
      ) : (
        <>
          {/* status card */}
          {status ? (
            <div className="mb-6 grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
              <Stat label={t.statusHealthyProviders}>
                {status.healthy_providers}/{status.total_providers}
              </Stat>
              <Stat label={t.statusSubscriptions}>{status.subscriptions}</Stat>
              <Stat label={t.statusCacheSymbols}>{status.cache_symbols}</Stat>
              <Stat label={t.statusTotalTicks}>{status.total_ticks.toLocaleString()}</Stat>
              <Stat
                label={t.statusDroppedEvents}
                className={status.dropped_events > 0 ? "text-red-700" : ""}
              >
                {status.dropped_events.toLocaleString()}
              </Stat>
            </div>
          ) : null}

          {/* connections */}
          <SubSection title={t.connectionsTitle}>
            {connections.length === 0 ? (
              <Empty text={t.connectionsEmpty} />
            ) : (
              <table className="min-w-full text-sm">
                <thead className="bg-slate-50 text-xs uppercase text-slate-500">
                  <tr>
                    <Th>{t.colProvider}</Th>
                    <Th>{t.colState}</Th>
                    <Th className="text-right">{t.colTickCount}</Th>
                    <Th className="text-right">{t.colReconnects}</Th>
                    <Th>{t.colLastTick}</Th>
                    <Th>{t.colConnectedAt}</Th>
                    <Th>{t.colLastError}</Th>
                  </tr>
                </thead>
                <tbody>
                  {connections.map((c) => (
                    <tr key={c.provider} className="border-t border-slate-100">
                      <Td>{c.provider}</Td>
                      <Td>
                        <span
                          className={`inline-flex rounded-full px-2 py-0.5 text-xs font-medium ${stateColor(c.state)}`}
                        >
                          {c.state}
                        </span>
                      </Td>
                      <Td className="text-right">{c.tick_count.toLocaleString()}</Td>
                      <Td className="text-right">{c.reconnect_count}</Td>
                      <Td>{fmtIsoOrDash(c.last_tick_at)}</Td>
                      <Td>{fmtIsoOrDash(c.connected_at)}</Td>
                      <Td className="text-red-700">{c.last_error || "—"}</Td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </SubSection>

          {/* subscribe form */}
          <SubSection title={t.subscribeTitle}>
            <div className="flex flex-wrap items-center gap-2">
              <input
                type="text"
                value={subscribeSym}
                onChange={(e) => setSubscribeSym(e.target.value)}
                placeholder={t.subscribeSymbolPlaceholder}
                className="rounded-md border border-slate-300 px-2 py-1 text-sm"
              />
              <input
                type="text"
                value={subscribeMarket}
                onChange={(e) => setSubscribeMarket(e.target.value)}
                placeholder={t.subscribeMarketPlaceholder}
                className="rounded-md border border-slate-300 px-2 py-1 text-sm"
              />
              <button
                type="button"
                onClick={() => void onSubscribe()}
                disabled={subscribing || !subscribeSym.trim()}
                className="rounded-md bg-indigo-600 px-3 py-1.5 text-sm font-medium text-white shadow-sm hover:bg-indigo-700 disabled:opacity-50"
              >
                {subscribing ? t.subscribeSubmitting : t.subscribeSubmit}
              </button>
            </div>
          </SubSection>

          {/* subscriptions */}
          <SubSection title={t.subscriptionsTitle}>
            {subscriptions.length === 0 ? (
              <Empty text={t.subscriptionsEmpty} />
            ) : (
              <table className="min-w-full text-sm">
                <thead className="bg-slate-50 text-xs uppercase text-slate-500">
                  <tr>
                    <Th>{t.colSymbol}</Th>
                    <Th>{t.colMarket}</Th>
                    <Th className="text-right">{t.colConsumers}</Th>
                    <Th>{t.colLastTick}</Th>
                    <Th></Th>
                  </tr>
                </thead>
                <tbody>
                  {subscriptions.map((s) => (
                    <tr key={s.symbol} className="border-t border-slate-100">
                      <Td className="font-mono">{s.symbol}</Td>
                      <Td>{s.market || "—"}</Td>
                      <Td className="text-right">{s.consumers}</Td>
                      <Td>{fmtIsoOrDash(s.last_tick_at)}</Td>
                      <Td>
                        <button
                          type="button"
                          onClick={() => void onUnsubscribe(s.symbol)}
                          className="text-xs text-red-700 hover:underline"
                        >
                          {t.unsubscribeButton}
                        </button>
                      </Td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </SubSection>

          {/* cache */}
          <SubSection
            title={t.cacheTitle}
            right={
              cacheList && cacheList.snapshots.length > 0 ? (
                <button
                  type="button"
                  onClick={() => void onEvictAll()}
                  className="text-xs text-red-700 hover:underline"
                >
                  {t.evictCacheAllButton}
                </button>
              ) : null
            }
          >
            {cacheList ? (
              <p className="mb-2 text-xs text-slate-500">
                {t.cacheStats}: {cacheList.stats.hits.toLocaleString()} /{" "}
                {cacheList.stats.misses.toLocaleString()} /{" "}
                {cacheList.stats.stales.toLocaleString()} /{" "}
                {cacheList.stats.evicts.toLocaleString()}
              </p>
            ) : null}
            {!cacheList || cacheList.snapshots.length === 0 ? (
              <Empty text={t.cacheEmpty} />
            ) : (
              <table className="min-w-full text-sm">
                <thead className="bg-slate-50 text-xs uppercase text-slate-500">
                  <tr>
                    <Th>{t.colSymbol}</Th>
                    <Th>{t.colMarket}</Th>
                    <Th className="text-right">{t.colLast}</Th>
                    <Th className="text-right">{t.colBid}</Th>
                    <Th className="text-right">{t.colAsk}</Th>
                    <Th>{t.colAsOf}</Th>
                    <Th>{t.colStale}</Th>
                    <Th></Th>
                  </tr>
                </thead>
                <tbody>
                  {cacheList.snapshots.map((s) => (
                    <tr key={s.symbol} className="border-t border-slate-100">
                      <Td className="font-mono">{s.symbol}</Td>
                      <Td>{s.market || "—"}</Td>
                      <Td className="text-right">{fmtNumber(s.last)}</Td>
                      <Td className="text-right">{fmtNumber(s.bid)}</Td>
                      <Td className="text-right">{fmtNumber(s.ask)}</Td>
                      <Td>{fmtIsoOrDash(s.as_of)}</Td>
                      <Td>{s.stale ? t.yes : t.no}</Td>
                      <Td>
                        <button
                          type="button"
                          onClick={() => void onEvictOne(s.symbol)}
                          className="text-xs text-red-700 hover:underline"
                        >
                          {t.evictCacheButton}
                        </button>
                      </Td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </SubSection>
        </>
      )}
    </section>
  );
}

// ---- presentational helpers ----

function Stat({
  label,
  children,
  className = "",
}: {
  label: string;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div className="rounded-lg border border-slate-200 bg-slate-50 px-3 py-2">
      <div className="text-xs uppercase tracking-wide text-slate-500">
        {label}
      </div>
      <div className={`mt-1 text-xl font-semibold text-slate-900 ${className}`}>
        {children}
      </div>
    </div>
  );
}

function SubSection({
  title,
  right,
  children,
}: {
  title: string;
  right?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <div className="mb-6">
      <div className="mb-2 flex items-center justify-between">
        <h3 className="text-sm font-semibold text-slate-800">{title}</h3>
        {right}
      </div>
      <div className="overflow-x-auto">{children}</div>
    </div>
  );
}

function Th({
  children,
  className = "",
}: {
  children?: React.ReactNode;
  className?: string;
}) {
  return (
    <th className={`px-3 py-2 text-left font-medium ${className}`}>
      {children}
    </th>
  );
}

function Td({
  children,
  className = "",
}: {
  children?: React.ReactNode;
  className?: string;
}) {
  return <td className={`px-3 py-2 align-middle ${className}`}>{children}</td>;
}

function Empty({ text }: { text: string }) {
  return (
    <p className="rounded-md border border-dashed border-slate-200 bg-slate-50 px-3 py-4 text-center text-sm text-slate-500">
      {text}
    </p>
  );
}
