// useBroadcastChannel.ts — cross-tab/window state sync.
//
// WHY THIS EXISTS
// ---------------
// Several pieces of state in the app live in localStorage today
// (language preference, display currency, fund tab selections) and
// are perfectly happy until a user opens a second tab. Then:
//
//   - Tab A switches language to en-US.
//   - Tab B continues showing zh-CN until you reload it.
//
// Worse, the user clicks "approve" on a decision in Tab A and Tab B
// still shows it as pending. Eventually they double-approve, the
// API rejects the second request, and the user thinks the system
// is broken.
//
// The native fix is BroadcastChannel — a per-origin broadcast
// primitive that posts a message to every other tab on the same
// origin instantly. This hook wraps it with two safety nets:
//
//   1. A `storage`-event fallback for browsers that don't ship
//      BroadcastChannel (older mobile webviews, ancient Safari).
//      The fallback uses localStorage writes as the transport;
//      the storage event fires in OTHER tabs only, so we get the
//      same "broadcast to peers" semantics with worse latency.
//
//   2. The handler is held in a ref so callers don't have to
//      memoise it; otherwise every render with an inline arrow
//      function would tear down and re-create the channel.
//
// USAGE PATTERN
// -------------
//
//   const { post } = useBroadcastChannel<{ language: string }>(
//     "fundai.preferences",
//     (msg) => setLanguage(msg.language),
//   );
//
//   // when local state changes:
//   post({ language: nextLanguage });
//
// The hook does NOT echo back to the originating tab — broadcasts
// are peer-only. That matches BroadcastChannel's native behaviour
// and matches the "I changed it, you sync" mental model.
//
// SAFETY
// ------
// - Channels are SCOPED PER ORIGIN. We never expose them across
//   different deployments / accounts; that's the browser's job.
// - Payloads are structured-clone-able by BroadcastChannel and
//   JSON-stringifiable by the storage fallback. Don't put DOM
//   nodes / functions / class instances in the payload.
// - We DO NOT use this for auth state (login token leakage between
//   tabs of the same origin is fine — same browser profile = same
//   user — but listening for "logout in other tab" is implemented
//   separately because it requires server-side session revocation
//   and is out of scope for this hook).

import { useCallback, useEffect, useRef } from "react";

interface BroadcastEnvelope<T> {
  v: 1;
  payload: T;
  ts: number;
  // A per-tab nonce. Useful if a future caller wants to detect
  // self-echoes (currently neither transport echoes, but a defensive
  // nonce costs ~4 bytes and saves debugging time later).
  origin: string;
}

const ORIGIN_NONCE = Math.random().toString(36).slice(2);

const STORAGE_FALLBACK_PREFIX = "__broadcast_channel_fallback__:";

export interface BroadcastChannelHook<T> {
  /** Broadcast a payload to every other tab. Returns synchronously. */
  post: (payload: T) => void;
  /** True if the native BroadcastChannel API is in use; false for storage-event fallback. */
  isNative: boolean;
}

export function useBroadcastChannel<T>(
  channelName: string,
  onMessage: (payload: T) => void,
): BroadcastChannelHook<T> {
  const handlerRef = useRef(onMessage);
  handlerRef.current = onMessage;

  const isNative = typeof window !== "undefined" && "BroadcastChannel" in window;

  useEffect(() => {
    if (typeof window === "undefined") return;

    if (isNative) {
      const channel = new BroadcastChannel(channelName);
      const listener = (event: MessageEvent<BroadcastEnvelope<T>>) => {
        const env = event.data;
        if (!env || env.v !== 1 || env.origin === ORIGIN_NONCE) return;
        try {
          handlerRef.current(env.payload);
        } catch (err) {
          console.error("[broadcast-channel] handler threw:", err);
        }
      };
      channel.addEventListener("message", listener);
      return () => {
        channel.removeEventListener("message", listener);
        channel.close();
      };
    }

    // Storage-event fallback. We write a value to a sentinel key,
    // then immediately delete it; OTHER tabs (only) receive a
    // `storage` event with the parsed payload.
    const storageListener = (event: StorageEvent) => {
      if (event.key !== `${STORAGE_FALLBACK_PREFIX}${channelName}`) return;
      if (!event.newValue) return; // ignore the delete
      try {
        const env: BroadcastEnvelope<T> = JSON.parse(event.newValue);
        if (env.v !== 1 || env.origin === ORIGIN_NONCE) return;
        handlerRef.current(env.payload);
      } catch (err) {
        console.error("[broadcast-channel] fallback parse failed:", err);
      }
    };
    window.addEventListener("storage", storageListener);
    return () => window.removeEventListener("storage", storageListener);
  }, [channelName, isNative]);

  const post = useCallback(
    (payload: T) => {
      if (typeof window === "undefined") return;
      const envelope: BroadcastEnvelope<T> = {
        v: 1,
        payload,
        ts: Date.now(),
        origin: ORIGIN_NONCE,
      };
      if (isNative) {
        try {
          const ch = new BroadcastChannel(channelName);
          ch.postMessage(envelope);
          ch.close();
        } catch (err) {
          console.error("[broadcast-channel] post failed:", err);
        }
        return;
      }
      // Storage-event fallback — write then immediately delete so the
      // sentinel key doesn't leak into the user's localStorage.
      const key = `${STORAGE_FALLBACK_PREFIX}${channelName}`;
      try {
        window.localStorage.setItem(key, JSON.stringify(envelope));
        window.localStorage.removeItem(key);
      } catch (err) {
        console.error("[broadcast-channel] fallback post failed:", err);
      }
    },
    [channelName, isNative],
  );

  return { post, isNative };
}
