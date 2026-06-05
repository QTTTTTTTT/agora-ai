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

import { useEffect, useState } from "react";
import { buildWorkflowStreamUrl, type WorkflowStatusFrame } from "./api";

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
