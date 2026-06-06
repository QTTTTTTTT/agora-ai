// useWorkflowStream — React hook that consumes the U4 step-1 SSE
// endpoint at /api/funds/{fundId}/workflow/stream. Returns the latest
// frame plus a connected/disconnected flag the UI can use to show a
// "live" badge.
//
// Lifecycle:
//
//   - On mount (or when fundId / enabled changes), open an EventSource.
//   - Server emits ":connected" → setConnected(true).
//   - Server emits "event: workflow" frames → setStatus(parsed).
//   - Server may emit "event: heartbeat" — ignored for state but
//     proves the channel is alive (proxies sometimes silently kill
//     idle connections; the heartbeat keeps that from happening).
//   - On error (network blip, server kill) the browser auto-reconnects
//     after a few seconds; we just flip `connected` so the UI can
//     show a stale-data badge.
//   - On unmount, close the EventSource.
//
// Auth: EventSource cannot set Authorization headers, so the SSE
// endpoint relies on the `fundai_session` cookie. We pass
// `withCredentials: true` so the browser replays the cookie on
// cross-origin requests (relevant when web/dev is on localhost:5173
// and the API is on localhost:8080).
//
// Future work (NOT in this hook): when the LLM call protocol gets
// rebuilt to emit token chunks, add an `event: decisionToken` listener
// alongside the `event: workflow` listener and expose a separate
// `tokens` slice. The diff-only frame protocol stays compatible with
// that addition.

import { useEffect, useMemo, useRef, useState } from "react";
import {
  buildWorkflowStreamMultiUrl,
  buildWorkflowStreamUrl,
  type WorkflowStatusFrame,
} from "./api";

export interface UseWorkflowStreamResult {
  status: WorkflowStatusFrame | null;
  connected: boolean;
  // `terminal` flips true once a `state ∈ {completed, failed}` frame
  // arrives. The server closes the stream itself at that point, but
  // the hook tracks it so the UI can render a definitive "done"
  // banner and stop showing the live badge.
  terminal: boolean;
  // `error` carries the last EventSource error so the UI can show
  // "reconnecting…" rather than going completely silent during a
  // network blip.
  error: string | null;
}

export interface UseWorkflowStreamOptions {
  fundId: string;
  enabled?: boolean;
  intervalMs?: number;
}

export function useWorkflowStream(opts: UseWorkflowStreamOptions): UseWorkflowStreamResult {
  const { fundId, enabled = true, intervalMs } = opts;
  const [status, setStatus] = useState<WorkflowStatusFrame | null>(null);
  const [connected, setConnected] = useState(false);
  const [terminal, setTerminal] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!enabled || !fundId) {
      setConnected(false);
      return;
    }

    const url = buildWorkflowStreamUrl(fundId, intervalMs);
    const source = new EventSource(url, { withCredentials: true });

    const handleOpen = () => {
      setConnected(true);
      setError(null);
    };

    const handleWorkflow = (event: MessageEvent) => {
      try {
        const frame = JSON.parse(event.data) as WorkflowStatusFrame;
        setStatus(frame);
        if (frame.state === "completed" || frame.state === "failed") {
          setTerminal(true);
        }
      } catch (parseErr) {
        // We don't surface this as a user-facing error: a malformed
        // frame is a server bug and the next well-formed frame will
        // overwrite this one. Emit a console warning so the symptom
        // is at least visible during dev / on the operator's
        // browser console.
        console.warn("workflow stream: dropped malformed frame", parseErr);
      }
    };

    const handleError = () => {
      setConnected(false);
      setError("connection lost — retrying");
    };

    source.addEventListener("open", handleOpen);
    source.addEventListener("workflow", handleWorkflow as EventListener);
    source.addEventListener("error", handleError);

    return () => {
      source.removeEventListener("open", handleOpen);
      source.removeEventListener("workflow", handleWorkflow as EventListener);
      source.removeEventListener("error", handleError);
      source.close();
    };
  }, [fundId, enabled, intervalMs]);

  return { status, connected, terminal, error };
}

// W4-25 — multiplexed workflow stream.
//
// useWorkflowStreamMulti subscribes to many funds over a single
// EventSource. Browsers cap concurrent EventSource handles per
// origin (commonly 6) and a portfolio dashboard tracking 10+
// funds would otherwise queue half its streams indefinitely. The
// multiplex endpoint (`GET /api/funds/workflow/stream?fundIds=…`)
// fans frames out server-side and tags each one with `fundId`;
// the hook here splits them back into a `Record<fundId,
// WorkflowStatusFrame|null>` for the consumer.
//
// We share connections across hook instances at the module level:
// two components that subscribe to the same set of fund ids will
// transparently re-use one socket. Subscriptions are reference-
// counted so the socket closes itself once nobody is listening.
//
// What this hook is NOT:
//
//   - It does not retry-with-backoff on its own. EventSource has
//     a built-in reconnect timer and that's good enough for the
//     drift-tolerant "live workflow" surface; finer control would
//     warrant a dedicated reconnect policy and is deferred.
//
//   - It does not de-duplicate fund ids across separate hook
//     calls with overlapping (but not identical) sets. Two cards
//     subscribing to {a,b} and {b,c} respectively will open two
//     connections; that's still 2× better than 5 connections
//     and the dedupe-of-supersets logic is more complex than it
//     is worth at the current scale.
export interface UseWorkflowStreamMultiOptions {
  fundIds: string[];
  enabled?: boolean;
  intervalMs?: number;
}

export interface UseWorkflowStreamMultiResult {
  statuses: Record<string, WorkflowStatusFrame | null>;
  // `connected` flips true on the first ":connected" line and
  // back to false on any error. Mirrors the single-fund hook.
  connected: boolean;
  // `terminal` reports per-fund whether the server emitted a
  // `terminal:true` frame, so the consumer can stop animating
  // a "live" badge for that card without losing the last frame.
  terminal: Record<string, boolean>;
  // `forbidden` lists the fund ids the server reported as
  // unauthorized — the multiplex endpoint emits a one-shot
  // `event: error` frame for each so the dashboard can render
  // a "no access" placeholder instead of a permanently-empty card.
  forbidden: string[];
  error: string | null;
}

interface MuxFrame {
  fundId: string;
  status: WorkflowStatusFrame | null;
  terminal?: boolean;
}

interface MuxErrorFrame {
  fundId: string;
  error: string;
}

interface MuxSubscriber {
  onFrame: (frame: MuxFrame) => void;
  onError: (frame: MuxErrorFrame) => void;
  onConnect: (ok: boolean) => void;
  onConnError: (err: string | null) => void;
}

interface MuxConnection {
  source: EventSource;
  subscribers: Set<MuxSubscriber>;
  // Memoised last-known state so a late-arriving subscriber for
  // the same connection key gets seeded immediately rather than
  // waiting for the next tick (which can be up to 30s with the
  // clamped-up interval).
  lastFrames: Record<string, MuxFrame>;
  lastErrors: Record<string, MuxErrorFrame>;
  lastConnected: boolean;
  lastConnError: string | null;
}

const muxConnections = new Map<string, MuxConnection>();

function muxConnectionKey(fundIds: string[], intervalMs?: number): string {
  // Sorting first so {a,b} and {b,a} share a connection. We do
  // not include `enabled` in the key — disabled hooks short-
  // circuit before reaching here.
  const sorted = [...fundIds].sort();
  return `${sorted.join(",")}|${intervalMs ?? ""}`;
}

function openMuxConnection(fundIds: string[], intervalMs?: number): MuxConnection {
  const url = buildWorkflowStreamMultiUrl(fundIds, intervalMs);
  const source = new EventSource(url, { withCredentials: true });
  const conn: MuxConnection = {
    source,
    subscribers: new Set(),
    lastFrames: {},
    lastErrors: {},
    lastConnected: false,
    lastConnError: null,
  };

  source.addEventListener("open", () => {
    conn.lastConnected = true;
    conn.lastConnError = null;
    conn.subscribers.forEach((s) => s.onConnect(true));
    conn.subscribers.forEach((s) => s.onConnError(null));
  });

  source.addEventListener("workflow", ((event: MessageEvent) => {
    try {
      const parsed = JSON.parse(event.data) as MuxFrame;
      if (!parsed?.fundId) return;
      conn.lastFrames[parsed.fundId] = parsed;
      conn.subscribers.forEach((s) => s.onFrame(parsed));
    } catch (err) {
      console.warn("workflow mux: dropped malformed frame", err);
    }
  }) as EventListener);

  source.addEventListener("error", ((event: Event) => {
    // Two cases: SSE transport error (no .data) → reconnect
    // signal, OR an `event: error` envelope (has .data) →
    // per-fund authorization failure.
    const me = event as MessageEvent;
    if (typeof me.data === "string" && me.data.length > 0) {
      try {
        const parsed = JSON.parse(me.data) as MuxErrorFrame;
        if (parsed?.fundId) {
          conn.lastErrors[parsed.fundId] = parsed;
          conn.subscribers.forEach((s) => s.onError(parsed));
          return;
        }
      } catch (err) {
        console.warn("workflow mux: dropped malformed error frame", err);
      }
    }
    conn.lastConnected = false;
    conn.lastConnError = "connection lost — retrying";
    conn.subscribers.forEach((s) => s.onConnect(false));
    conn.subscribers.forEach((s) => s.onConnError("connection lost — retrying"));
  }) as EventListener);

  return conn;
}

function subscribeMux(
  fundIds: string[],
  intervalMs: number | undefined,
  sub: MuxSubscriber,
): () => void {
  const key = muxConnectionKey(fundIds, intervalMs);
  let conn = muxConnections.get(key);
  if (!conn) {
    conn = openMuxConnection(fundIds, intervalMs);
    muxConnections.set(key, conn);
  }
  conn.subscribers.add(sub);

  // Replay last-known state synchronously so a remount within the
  // same SPA session doesn't briefly render a blank card.
  Object.values(conn.lastFrames).forEach((frame) => sub.onFrame(frame));
  Object.values(conn.lastErrors).forEach((errFrame) => sub.onError(errFrame));
  sub.onConnect(conn.lastConnected);
  sub.onConnError(conn.lastConnError);

  return () => {
    if (!conn) return;
    conn.subscribers.delete(sub);
    if (conn.subscribers.size === 0) {
      conn.source.close();
      muxConnections.delete(key);
    }
  };
}

export function useWorkflowStreamMulti(
  opts: UseWorkflowStreamMultiOptions,
): UseWorkflowStreamMultiResult {
  const { fundIds, enabled = true, intervalMs } = opts;

  // We memoise the sorted-id key so the effect below only re-runs
  // when the actual subscription changes, not when the consumer
  // passes a new array reference with the same contents (a very
  // common pattern when fundIds derive from `funds.map(f => f.id)`).
  const subscriptionKey = useMemo(
    () => fundIds.slice().sort().join(","),
    [fundIds],
  );

  const [statuses, setStatuses] = useState<Record<string, WorkflowStatusFrame | null>>({});
  const [terminal, setTerminal] = useState<Record<string, boolean>>({});
  const [forbidden, setForbidden] = useState<string[]>([]);
  const [connected, setConnected] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Mutable ref for the unsubscribe handle so re-renders during
  // mount don't double-subscribe. The actual subscribe call lives
  // inside the effect; the ref is just a parking spot.
  const cleanupRef = useRef<(() => void) | null>(null);

  useEffect(() => {
    if (!enabled || fundIds.length === 0) {
      setStatuses({});
      setTerminal({});
      setForbidden([]);
      setConnected(false);
      return;
    }

    setStatuses({});
    setTerminal({});
    setForbidden([]);

    const sub: MuxSubscriber = {
      onFrame: (frame) => {
        setStatuses((prev) => ({ ...prev, [frame.fundId]: frame.status }));
        if (frame.terminal) {
          setTerminal((prev) => ({ ...prev, [frame.fundId]: true }));
        }
      },
      onError: (frame) => {
        setForbidden((prev) =>
          prev.includes(frame.fundId) ? prev : [...prev, frame.fundId],
        );
      },
      onConnect: setConnected,
      onConnError: setError,
    };

    cleanupRef.current = subscribeMux(fundIds, intervalMs, sub);
    return () => {
      cleanupRef.current?.();
      cleanupRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [subscriptionKey, enabled, intervalMs]);

  return { statuses, connected, terminal, forbidden, error };
}
