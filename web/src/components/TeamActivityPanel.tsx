import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  TeamActivityItem,
  buildTeamActivityStreamUrl,
  fetchTeamActivity,
  formatApiError,
} from "../lib/api";
import { AppLanguage, formatDateTimeForLanguage, useAppPreferences } from "../lib/preferences";
import Pagination from "./Pagination";

interface TeamActivityPanelProps {
  fundId: string;
  // maxItems caps the visible buffer; older events are evicted to keep DOM
  // small even after a long-running fund leaves the panel open. Defaults to
  // 200 (mirrors the server-side ring buffer).
  maxItems?: number;
}

interface PanelCopy {
  title: string;
  subtitle: string;
  status: {
    connecting: string;
    live: string;
    reconnecting: string;
    error: string;
    offline: string;
  };
  empty: string;
  loadError: string;
  retry: string;
  trailingHint: (count: number) => string;
  loadMore: string;
  loading: string;
  noMore: string;
  loadMoreError: string;
  newEventsHint: (count: number) => string;
  jumpToLatest: string;
  roleLabels: Record<string, string>;
}

const copyByLanguage: Record<AppLanguage, PanelCopy> = {
  "zh-CN": {
    title: "团队实时活动",
    subtitle: "实时显示团队 Agent 在每日工作流中的执行步骤，便于确认团队正在运作。",
    status: {
      connecting: "正在连接事件流…",
      live: "实时连接已建立",
      reconnecting: "连接已断开，正在自动重连…",
      error: "事件流连接失败",
      offline: "事件流未连接",
    },
    empty: "近期未观察到团队活动。等待下一次工作流触发或手动启动每日工作流。",
    loadError: "拉取近期活动失败，请稍后重试。",
    retry: "重试",
    trailingHint: (count) => (count > 0 ? `本次会话内已接收 ${count} 条事件` : ""),
    loadMore: "加载更早的活动",
    loading: "加载中…",
    noMore: "已经是最早的活动了",
    loadMoreError: "加载更早活动失败，请稍后重试。",
    newEventsHint: (count) => `${count} 条新事件，点击回到最新`,
    jumpToLatest: "回到最新",
    roleLabels: {
      pm: "组合经理",
      researcher: "研究员",
      trader: "交易员",
      risk: "风控",
      team: "团队",
      user: "用户",
      system: "系统",
    },
  },
  "en-US": {
    title: "Team Live Activity",
    subtitle: "Live stream of every workflow step the team executes, so you can see the agents are actually working.",
    status: {
      connecting: "Connecting to event stream…",
      live: "Live",
      reconnecting: "Connection dropped, retrying…",
      error: "Event stream failed",
      offline: "Stream not connected",
    },
    empty: "No team activity yet. Wait for the next workflow tick or trigger the daily workflow manually.",
    loadError: "Failed to load recent activity. Please retry.",
    retry: "Retry",
    trailingHint: (count) => (count > 0 ? `Received ${count} events in this session` : ""),
    loadMore: "Load earlier activity",
    loading: "Loading…",
    noMore: "No earlier events",
    loadMoreError: "Failed to load earlier activity. Please retry.",
    newEventsHint: (count) => `${count} new event${count === 1 ? "" : "s"} — jump to latest`,
    jumpToLatest: "Jump to latest",
    roleLabels: {
      pm: "Portfolio Manager",
      researcher: "Researcher",
      trader: "Trader",
      risk: "Risk",
      team: "Team",
      user: "User",
      system: "System",
    },
  },
};

const roleColorMap: Record<string, { bg: string; text: string; dot: string }> = {
  pm: { bg: "bg-purple-50", text: "text-purple-700", dot: "bg-purple-500" },
  researcher: { bg: "bg-sky-50", text: "text-sky-700", dot: "bg-sky-500" },
  trader: { bg: "bg-amber-50", text: "text-amber-700", dot: "bg-amber-500" },
  risk: { bg: "bg-rose-50", text: "text-rose-700", dot: "bg-rose-500" },
  team: { bg: "bg-indigo-50", text: "text-indigo-700", dot: "bg-indigo-500" },
  user: { bg: "bg-emerald-50", text: "text-emerald-700", dot: "bg-emerald-500" },
  system: { bg: "bg-gray-100", text: "text-gray-700", dot: "bg-gray-400" },
};

type StreamState = "connecting" | "live" | "reconnecting" | "error" | "offline";

/**
 * TeamActivityPanel shows a live timeline of workflow events for a fund's team.
 *
 * Data flow:
 *   1. On mount we fetch /api/funds/:fundId/team/activity for the initial
 *      backfill — covers the most recent events without waiting for the next
 *      SSE message.
 *   2. We open EventSource(`/api/funds/:fundId/team/activity/stream`). Auth
 *      relies on the `fundai_session` cookie because EventSource cannot send
 *      Authorization headers; the cookie is set during login.
 *   3. On connection drop we wait an exponential backoff (max 30s), reopen,
 *      then call fetchTeamActivity with sinceSeq = lastSeen to fill any gap.
 *   4. The panel maintains a bounded (`maxItems`) list newest-first so the
 *      DOM stays cheap even after long observation windows.
 */
const TeamActivityPanel: React.FC<TeamActivityPanelProps> = ({ fundId, maxItems = 200 }) => {
  const { language } = useAppPreferences();
  const copy = copyByLanguage[language] ?? copyByLanguage["en-US"];

  const [items, setItems] = useState<TeamActivityItem[]>([]);
  const [streamState, setStreamState] = useState<StreamState>("connecting");
  const [loadError, setLoadError] = useState<string | null>(null);
  const [reloadKey, setReloadKey] = useState(0);

  // Pagination state for the "load earlier" path. `loadingMore` gates
  // the network call so a fast double-click does not stack requests;
  // `noMore` is set when the server returns an empty page (we've
  // reached the start of the retention window). `loadMoreError` is
  // rendered inline below the timeline so a transient failure doesn't
  // tear down the whole panel.
  const [loadingMore, setLoadingMore] = useState(false);
  const [noMore, setNoMore] = useState(false);
  const [loadMoreError, setLoadMoreError] = useState<string | null>(null);

  // Pure client-side pagination over the in-memory buffer. Page 0 is
  // "newest 10 events" — that's the steady state for someone watching
  // a workflow live. When the user advances past the last buffered
  // page we automatically fall back to the server's REST cursor so
  // pages 2+ still resolve smoothly even though `items` started small.
  const PAGE_SIZE = 10;
  const [page, setPage] = useState(0);
  // newEventsBuffered counts SSE arrivals while the user is on a
  // historical page. We refuse to silently bump them back to page 0
  // mid-read, but we surface the count so they can choose to jump.
  const [newEventsBuffered, setNewEventsBuffered] = useState(0);
  // Mirror `page` into a ref so the SSE handler can read the latest
  // value without re-binding the EventSource every time the operator
  // pages forward or backward. Without this, paginating tears down
  // and reopens the stream — which is both wasteful and would cause
  // the live "live" badge to flicker through "connecting" for ~1s
  // on every page click.
  const pageRef = useRef(0);
  useEffect(() => {
    pageRef.current = page;
  }, [page]);

  // sessionCountRef accumulates how many live events we've shown this session
  // (since mount or last reload). We track it via a ref so the SSE handler
  // can update it without triggering rerenders on every event.
  const sessionCountRef = useRef(0);
  const [sessionCount, setSessionCount] = useState(0);

  const seenSeqsRef = useRef<Set<number>>(new Set());
  const lastSeqRef = useRef<number>(0);
  // Cap the total list size at maxItems * 6 so a very long load-more
  // session doesn't bloat the DOM indefinitely. 6 × 200 = 1200 events
  // is well over a normal trading week's worth of workflow steps and
  // still inside what mobile browsers render smoothly.
  const renderCap = maxItems * 6;

  const ingest = useCallback(
    (incoming: TeamActivityItem[], { reset, append }: { reset?: boolean; append?: boolean } = {}) => {
      if (incoming.length === 0 && !reset) {
        return;
      }
      const seen = reset ? new Set<number>() : seenSeqsRef.current;
      const seqAccumulator: number[] = [];
      const accepted: TeamActivityItem[] = [];
      for (const evt of incoming) {
        if (!evt || typeof evt.seq !== "number" || evt.seq <= 0) {
          continue;
        }
        if (seen.has(evt.seq)) {
          continue;
        }
        seen.add(evt.seq);
        seqAccumulator.push(evt.seq);
        accepted.push(evt);
      }
      seenSeqsRef.current = seen;
      if (seqAccumulator.length > 0) {
        lastSeqRef.current = Math.max(lastSeqRef.current, ...seqAccumulator);
      }
      setItems((prev) => {
        const base = reset ? [] : prev;
        // `append`-mode preserves the ordering of older events at the
        // tail of the list (the "load earlier" path); the default flow
        // (live SSE / sinceSeq backfill) keeps the newest-first
        // invariant. We always re-sort by seq desc at the end so
        // out-of-order arrivals can't break the timeline.
        const merged = append ? [...base, ...accepted] : [...accepted, ...base];
        merged.sort((a, b) => b.seq - a.seq);
        const cap = append ? renderCap : maxItems;
        if (merged.length > cap) {
          merged.length = cap;
        }
        return merged;
      });
    },
    [maxItems, renderCap],
  );

  // Initial backfill (and refresh on retry button).
  useEffect(() => {
    if (!fundId) {
      return;
    }
    let cancelled = false;
    setLoadError(null);
    setNoMore(false);
    setLoadMoreError(null);
    fetchTeamActivity(fundId, { limit: Math.min(maxItems, 100) })
      .then((resp) => {
        if (cancelled) return;
        // Server returns oldest-first; ingest handles ordering. Reset so a
        // manual reload doesn't keep stale items from a previous fund.
        ingest(resp.items, { reset: true });
        sessionCountRef.current = 0;
        setSessionCount(0);
      })
      .catch((err) => {
        if (cancelled) return;
        setLoadError(formatApiError(err, copy.loadError));
      });
    return () => {
      cancelled = true;
    };
  }, [fundId, maxItems, ingest, reloadKey, copy.loadError]);

  // loadMore fetches the next historical page. Uses the oldest visible
  // event's timestamp as the cursor so the server returns events
  // strictly older than what's already on screen. We expose the
  // returned promise so the pagination effect can chain "advance to
  // the next page once the new rows are merged".
  const loadMore = useCallback((): Promise<{ added: number }> => {
    if (!fundId || loadingMore || noMore) {
      return Promise.resolve({ added: 0 });
    }
    const oldest = items[items.length - 1];
    if (!oldest || !oldest.timestamp) {
      // Without any events on screen there's nothing to anchor the
      // cursor against; fall back to a plain refresh.
      setReloadKey((k) => k + 1);
      return Promise.resolve({ added: 0 });
    }
    setLoadingMore(true);
    setLoadMoreError(null);
    return fetchTeamActivity(fundId, { before: oldest.timestamp, limit: 50 })
      .then((resp) => {
        if (!resp.items || resp.items.length === 0) {
          setNoMore(true);
          return { added: 0 };
        }
        ingest(resp.items, { append: true });
        return { added: resp.items.length };
      })
      .catch((err) => {
        setLoadMoreError(formatApiError(err, copy.loadMoreError));
        return { added: 0 };
      })
      .finally(() => {
        setLoadingMore(false);
      });
  }, [fundId, items, ingest, loadingMore, noMore, copy.loadMoreError]);

  // EventSource lifecycle with reconnect + gap-fill.
  useEffect(() => {
    if (!fundId) {
      return;
    }
    let cancelled = false;
    let attempt = 0;
    let source: EventSource | null = null;
    let reconnectTimer: number | null = null;

    const scheduleReconnect = () => {
      if (cancelled) return;
      const delayMs = Math.min(30_000, 1_000 * 2 ** Math.min(attempt, 5));
      attempt += 1;
      setStreamState("reconnecting");
      reconnectTimer = window.setTimeout(() => {
        if (cancelled) return;
        // Try to gap-fill via REST first so users immediately see what they
        // missed; the SSE reconnect then continues delivering new events.
        if (lastSeqRef.current > 0) {
          fetchTeamActivity(fundId, { sinceSeq: lastSeqRef.current, limit: maxItems })
            .then((resp) => {
              if (cancelled) return;
              ingest(resp.items);
            })
            .catch(() => {
              /* gap-fill best-effort; SSE may catch up regardless */
            });
        }
        open();
      }, delayMs);
    };

    const open = () => {
      if (cancelled) return;
      setStreamState("connecting");
      source = new EventSource(buildTeamActivityStreamUrl(fundId), { withCredentials: true });
      source.onopen = () => {
        if (cancelled) return;
        attempt = 0;
        setStreamState("live");
      };
      source.addEventListener("activity", (msgEvent) => {
        if (cancelled) return;
        try {
          const parsed = JSON.parse((msgEvent as MessageEvent).data) as TeamActivityItem;
          ingest([parsed]);
          sessionCountRef.current += 1;
          setSessionCount(sessionCountRef.current);
          // If the operator is on a historical page, queue the
          // arrival count instead of yanking them back to page 0.
          // The badge in the header lets them opt in. We read
          // the page from the ref so this effect doesn't have to
          // depend on `page` (which would reopen the stream on
          // every pagination click).
          setNewEventsBuffered((prev) => (pageRef.current > 0 ? prev + 1 : prev));
        } catch {
          // Drop malformed payloads silently; the server's contract is JSON.
        }
      });
      source.addEventListener("heartbeat", () => {
        if (cancelled) return;
        // Heartbeats keep proxies awake; we just bump the state to live in
        // case it was stuck on connecting/reconnecting.
        setStreamState("live");
      });
      source.onerror = () => {
        if (cancelled) return;
        source?.close();
        source = null;
        scheduleReconnect();
      };
    };

    open();
    return () => {
      cancelled = true;
      if (reconnectTimer !== null) {
        window.clearTimeout(reconnectTimer);
      }
      source?.close();
      setStreamState("offline");
    };
  }, [fundId, ingest, maxItems, reloadKey]);

  // Compute which slice of items to render for the current page.
  // We re-derive instead of caching so pagination stays in lockstep
  // with the live SSE buffer (new arrivals at index 0 just push
  // older items toward subsequent pages — we never have to "rebalance").
  const totalItems = items.length;
  const pageCount = totalItems === 0 ? 0 : Math.ceil(totalItems / PAGE_SIZE);
  const safePage = pageCount === 0 ? 0 : Math.min(page, pageCount - 1);
  const visibleItems = useMemo(() => {
    if (totalItems === 0) return [] as TeamActivityItem[];
    const start = safePage * PAGE_SIZE;
    return items.slice(start, start + PAGE_SIZE);
  }, [items, safePage, totalItems]);

  // Auto-clamp & auto-fetch: when the user clicks "next" and they're
  // already on the last buffered page, we transparently fetch the
  // next history page from the server before letting them advance.
  // This keeps pagination feeling like a single uniform list even
  // though the buffer grows on demand.
  const handlePageChange = useCallback(
    (next: number) => {
      const upperBound = pageCount - 1;
      // Going back is always safe and instantaneous.
      if (next <= safePage) {
        setPage(Math.max(0, next));
        if (next === 0) {
          setNewEventsBuffered(0);
        }
        return;
      }
      // Going forward but still within the current buffer — straight slice.
      if (next <= upperBound) {
        setPage(next);
        return;
      }
      // Forward past the buffer's end: pull the next history page
      // from the server first, then advance once it lands. We only
      // attempt this when noMore is false; otherwise we clamp at the
      // last buffered page so the "Next" button effectively becomes
      // a no-op on truly exhausted streams.
      if (noMore) {
        setPage(upperBound);
        return;
      }
      void loadMore().then((res) => {
        if (res.added > 0) {
          setPage(next);
        } else {
          setPage(upperBound);
        }
      });
    },
    [pageCount, safePage, noMore, loadMore],
  );

  const handleJumpToLatest = useCallback(() => {
    setPage(0);
    setNewEventsBuffered(0);
  }, []);

  const statusBadge = useMemo(() => {
    const palette: Record<StreamState, { bg: string; text: string; dot: string }> = {
      connecting: { bg: "bg-amber-50", text: "text-amber-700", dot: "bg-amber-400 animate-pulse" },
      live: { bg: "bg-emerald-50", text: "text-emerald-700", dot: "bg-emerald-500 animate-pulse" },
      reconnecting: { bg: "bg-amber-50", text: "text-amber-700", dot: "bg-amber-400 animate-pulse" },
      error: { bg: "bg-rose-50", text: "text-rose-700", dot: "bg-rose-500" },
      offline: { bg: "bg-gray-100", text: "text-gray-600", dot: "bg-gray-400" },
    };
    const label = copy.status[streamState];
    const styles = palette[streamState];
    return (
      <span className={`inline-flex items-center gap-2 rounded-full px-3 py-1 text-xs font-medium ${styles.bg} ${styles.text}`}>
        <span className={`h-2 w-2 rounded-full ${styles.dot}`} />
        {label}
      </span>
    );
  }, [copy.status, streamState]);

  return (
    <section className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
      <div className="flex flex-col gap-2 md:flex-row md:items-start md:justify-between">
        <div>
          <h2 className="text-lg font-semibold text-gray-900">{copy.title}</h2>
          <p className="mt-1 text-sm text-gray-500">{copy.subtitle}</p>
        </div>
        <div className="flex flex-col items-end gap-2">
          {statusBadge}
          {sessionCount > 0 ? <p className="text-xs text-gray-400">{copy.trailingHint(sessionCount)}</p> : null}
        </div>
      </div>

      {loadError ? (
        <div className="mt-4 flex items-center justify-between rounded-xl border border-rose-200 bg-rose-50 px-4 py-3 text-sm text-rose-700">
          <span>{loadError}</span>
          <button
            type="button"
            onClick={() => {
              setLoadError(null);
              setReloadKey((k) => k + 1);
            }}
            className="rounded-md bg-rose-600 px-3 py-1 text-xs font-medium text-white hover:bg-rose-700"
          >
            {copy.retry}
          </button>
        </div>
      ) : null}

      <div className="mt-4">
        {items.length === 0 ? (
          <div className="rounded-xl border border-dashed border-gray-200 bg-gray-50 px-4 py-8 text-center text-sm text-gray-500">{copy.empty}</div>
        ) : (
          <>
            {newEventsBuffered > 0 && safePage > 0 ? (
              <button
                type="button"
                onClick={handleJumpToLatest}
                className="mb-3 inline-flex w-full items-center justify-center gap-2 rounded-xl border border-emerald-200 bg-emerald-50 px-4 py-2 text-xs font-medium text-emerald-700 transition hover:border-emerald-300 hover:bg-emerald-100"
              >
                <span className="h-2 w-2 animate-pulse rounded-full bg-emerald-500" />
                {copy.newEventsHint(newEventsBuffered)}
              </button>
            ) : null}
            <ol className="relative ml-3 space-y-3 border-l border-gray-200 pl-5">
              {visibleItems.map((item) => {
                const palette = roleColorMap[item.role] ?? roleColorMap.system;
                const roleLabel = copy.roleLabels[item.role] ?? item.role;
                return (
                  <li key={item.seq} className="relative">
                    <span className={`absolute -left-[1.55rem] top-1.5 h-3 w-3 rounded-full border-2 border-white shadow ${palette.dot}`} />
                    <div className="rounded-xl border border-gray-100 bg-gray-50 px-3 py-2">
                      <div className="flex flex-wrap items-center gap-2">
                        <span className={`rounded-full px-2 py-0.5 text-[10px] font-medium ${palette.bg} ${palette.text}`}>{roleLabel}</span>
                        {item.step ? <span className="rounded-full bg-white px-2 py-0.5 text-[10px] font-medium text-gray-600">{item.step}</span> : null}
                        {item.tradingDate ? <span className="text-[10px] text-gray-400">{item.tradingDate}</span> : null}
                        <span className="ml-auto text-[10px] text-gray-400">{formatDateTimeForLanguage(item.timestamp, language)}</span>
                      </div>
                      <p className="mt-1 text-sm text-gray-700">{item.message}</p>
                      {item.error ? <p className="mt-1 text-xs text-rose-600">{item.error}</p> : null}
                    </div>
                  </li>
                );
              })}
            </ol>
            <div className="mt-4 space-y-2">
              <Pagination
                page={safePage}
                pageCount={pageCount}
                pageSize={PAGE_SIZE}
                totalItems={totalItems}
                language={language}
                onPageChange={handlePageChange}
                align="between"
              />
              <div className="flex items-center justify-end gap-2 text-xs">
                {loadingMore ? (
                  <span className="text-gray-400">{copy.loading}</span>
                ) : noMore ? (
                  <span className="text-gray-400">{copy.noMore}</span>
                ) : null}
                {loadMoreError ? <span className="text-rose-600">{loadMoreError}</span> : null}
              </div>
            </div>
          </>
        )}
      </div>
    </section>
  );
};

export default TeamActivityPanel;
