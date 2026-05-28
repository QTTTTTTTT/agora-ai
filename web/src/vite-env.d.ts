/// <reference types="vite/client" />

// Vite's preload error event is dispatched on `window` (not document) and
// surfaces failures from `<link rel="modulepreload">` chunks that the
// browser tried to fetch during initial page load. We listen for it in
// `main.tsx` to trigger a single auto-reload — see that file for the full
// rationale. This ambient declaration just teaches TypeScript that the
// event name is real so the `addEventListener` call type-checks cleanly.
//
// The event's `payload` field carries the original Vite error object and
// the chunk URL; we don't introspect them today but expose the shape for
// future diagnostics.
declare global {
  interface VitePreloadErrorPayload {
    message?: string;
  }
  interface VitePreloadErrorEvent extends Event {
    payload: VitePreloadErrorPayload;
  }
  interface WindowEventMap {
    "vite:preloadError": VitePreloadErrorEvent;
  }
}

export {};
