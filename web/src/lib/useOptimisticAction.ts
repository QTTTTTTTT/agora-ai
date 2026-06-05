// useOptimisticAction.ts — generic optimistic-UI mutation pattern.
//
// WHY THIS EXISTS
// ---------------
// Every action button in the app today reads:
//
//   const [busy, setBusy] = useState(false)
//   async function approve() {
//     setBusy(true)
//     try {
//       await api.approve(id)
//       await refresh()             // re-fetch to update the UI
//     } catch (err) {
//       toast.error(err.message)
//     } finally {
//       setBusy(false)
//     }
//   }
//
// Watch what the user feels:
//   1. click → the button shows a spinner for 200-800ms (the API
//      round-trip),
//   2. the API returns,
//   3. the page re-fetches (another 200-800ms),
//   4. finally the row updates and the spinner stops.
//
// That's 0.5-1.5 seconds of "did anything happen?" feedback gap
// for actions the user expects to be instantaneous (approve a
// decision, mark a notification read, toggle a checkbox).
//
// Optimistic UI flips the order:
//
//   1. click → IMMEDIATELY mutate local state to the expected
//      result (the row turns green, the button changes to
//      "Undo");
//   2. fire the API call in the background;
//   3a. on success: keep the local mutation (or replace with
//       the real server response);
//   3b. on failure: roll back the local mutation, show an
//       error toast, the row reverts to its old state.
//
// Done right, the user feels the action as instant. Done wrong,
// you ship a UX where 1% of clicks visibly flicker as the
// rollback fires.
//
// USAGE
// -----
//
//   const approve = useOptimisticAction({
//     mutate: (id: string) => api.approveDecision(id),
//     onApply: (id) => setDecisions((d) =>
//       d.map((x) => x.id === id ? { ...x, status: "approved" } : x)
//     ),
//     onRevert: (id, _err, prev) => setDecisions(prev),  // restore
//     getSnapshot: () => decisions,                      // for revert
//   })
//
//   <button onClick={() => approve.run(id)} disabled={approve.pending}>
//     {approve.pending ? "Approving…" : "Approve"}
//   </button>
//
// Where the contract is:
//   - `mutate` is the actual API call (async).
//   - `onApply(arg)` immediately applies the optimistic mutation
//     to local state.
//   - `getSnapshot()` returns whatever needs to be restored if
//     the API fails — usually the array / object before
//     onApply ran.
//   - `onRevert(arg, err, snapshot)` restores the snapshot and
//     surfaces the error (toast / inline message).
//
// Why this shape: it keeps the call site explicit. We don't
// hide the rollback inside a "magic" library because finance
// data has a higher accuracy bar than typical SaaS — the UI
// must NEVER show a stale "approved" badge if the server
// rejected it.
//
// DEDUP / GUARD
// -------------
// Concurrent calls are guarded: while a previous run is still
// in-flight, additional `run()` calls are ignored (and a
// `pending` getter is exposed so callers can disable the
// trigger). This prevents the "user clicks approve 5 times
// because the API is slow" footgun.

import { useCallback, useRef, useState } from "react";

export interface OptimisticActionOptions<Arg, Snapshot, Result> {
  /** The API call to perform. Throwing or rejecting triggers rollback. */
  mutate: (arg: Arg) => Promise<Result>;
  /** Capture the current state so rollback can restore it. Called BEFORE onApply. */
  getSnapshot: (arg: Arg) => Snapshot;
  /** Apply the optimistic update locally. Called BEFORE the API request. */
  onApply: (arg: Arg) => void;
  /** Roll back the optimistic update on failure. Receives the snapshot from getSnapshot. */
  onRevert: (arg: Arg, error: unknown, snapshot: Snapshot) => void;
  /** Optional success hook — replace optimistic state with server response when it differs. */
  onSettled?: (arg: Arg, result: Result) => void;
}

export interface OptimisticActionResult<Arg, Result> {
  /** True while a mutation is in flight. Disable the trigger to prevent duplicate clicks. */
  pending: boolean;
  /** Last error from a failed call. Cleared on the next run. */
  error: unknown;
  /** Trigger the action. Returns the API result (or null on failure / dedup). */
  run: (arg: Arg) => Promise<Result | null>;
}

export function useOptimisticAction<Arg, Snapshot, Result>(
  opts: OptimisticActionOptions<Arg, Snapshot, Result>,
): OptimisticActionResult<Arg, Result> {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<unknown>(null);

  // Use a ref for in-flight tracking so the dedup decision doesn't
  // race with a stale closure. setState's async update can let two
  // clicks both observe pending=false momentarily.
  const inFlight = useRef(false);

  // Stable callback refs — callers usually pass inline arrows that
  // close over component state, and we want each `run()` to use
  // the LATEST handlers, not the ones from the render where the
  // hook was called. (Same trick useBroadcastChannel uses.)
  const optsRef = useRef(opts);
  optsRef.current = opts;

  const run = useCallback(
    async (arg: Arg): Promise<Result | null> => {
      if (inFlight.current) return null;
      inFlight.current = true;
      setPending(true);
      setError(null);

      // Capture snapshot BEFORE applying the optimistic mutation.
      const snapshot = optsRef.current.getSnapshot(arg);
      try {
        optsRef.current.onApply(arg);
      } catch (applyErr) {
        // If onApply itself throws, treat it as a hard failure
        // and don't dispatch the network request.
        inFlight.current = false;
        setPending(false);
        setError(applyErr);
        return null;
      }

      try {
        const result = await optsRef.current.mutate(arg);
        optsRef.current.onSettled?.(arg, result);
        return result;
      } catch (apiErr) {
        // Roll back. We swallow any error from onRevert itself —
        // there's no useful recovery from "rollback failed too",
        // and we don't want it to mask the original API error
        // shown to the user.
        try {
          optsRef.current.onRevert(arg, apiErr, snapshot);
        } catch {
          /* eslint-disable-line no-empty */
        }
        setError(apiErr);
        return null;
      } finally {
        inFlight.current = false;
        setPending(false);
      }
    },
    [],
  );

  return { pending, error, run };
}
