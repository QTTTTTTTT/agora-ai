// workflow_stream.go — Server-Sent Events endpoint for the per-fund
// workflow status. Lifted out of fund_handler.go (which was already
// 4,800+ lines) into its own file so the SSE concern has a stable
// boundary; new streaming endpoints (decision streams, agent log
// streams) can join this file rather than further bloating
// fund_handler.go.
//
// Endpoint:  GET /api/funds/{fundId}/workflow/stream
// Event protocol:
//
//	event: workflow
//	data: {"fundId":"…","state":"running","step":"agent_round_3","progressPercent":40,…}
//
//	event: heartbeat
//	data: 2026-06-05T08:30:00Z
//
// Tick cadence is governed by the optional `?interval=` query param
// (clamped to [500ms, 30s] via parseStreamInterval, which was already
// shared with /quotes/stream). Diff-only emission: only push frames
// when at least one of `state`, `step`, `progressPercent`,
// `completedSteps`, `failedSteps`, or `completedAt` changed since
// the last push, so an idle workflow doesn't burn bytes.
//
// Auth: same as the rest of the fund handler — cookie-based session
// because EventSource cannot set Authorization headers.
//
// What this is NOT:
//
//   - This does NOT change the LLM call protocol. The full "U4 — PM
//     decision SSE streaming" item from the production-readiness
//     review still requires re-plumbing each LLM call to emit token
//     chunks and wiring those into a real-time pipeline. This handler
//     polls the existing `WorkflowService.GetStatus` method on a
//     timer and pushes diffs — that's good enough to drive a "live
//     workflow timeline" UI surface today, without months of LLM
//     plumbing work, and it gives the frontend a stable SSE channel
//     that future LLM-token streams can graft onto via additional
//     `event:` types.
//
//   - This does NOT add a new WorkflowService method. The interface
//     stays identical so unit tests for fund_handler.go and the
//     wiring-adapter implementation don't have to learn a new method.

package api

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// StreamWorkflowStatus opens an SSE stream for a fund's workflow run.
//
// The handler shape deliberately mirrors StreamPortfolioQuotes:
//   - validate auth + fundId,
//   - perform a one-shot status read up front so we surface 4xx errors
//     synchronously rather than via the SSE error channel,
//   - emit ":" comment line so proxies open the stream,
//   - tick + heartbeat,
//   - diff-only frame push.
func (h *FundHandler) StreamWorkflowStatus(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}

	flusher, isFlusher := w.(http.Flusher)
	if !isFlusher {
		writeError(w, http.StatusInternalServerError, "sse unsupported", "response writer does not implement http.Flusher")
		return
	}

	// Up-front auth + initial frame source. If the user can't read
	// the workflow status they shouldn't be able to subscribe.
	initial, err := h.workflow.GetStatus(userID, fundID)
	if err != nil {
		handleServiceError(w, err, "workflow stream")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	// Disable buffering by reverse-proxies (nginx etc.) so frames
	// reach the client immediately rather than batching at the proxy.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if _, err := io.WriteString(w, ": connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	ctx := r.Context()
	interval := parseStreamInterval(r.URL.Query().Get("interval"), 2*time.Second)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	encoder := json.NewEncoder(w)

	// `lastFingerprint` captures the small handful of fields whose
	// change should trigger a frame. We deliberately do not hash the
	// full status — the larger structures (Steps slice, StepResults
	// map) churn on every tick during normal execution and would
	// defeat the diff filter. The fingerprint covers the surfaces a
	// "what's happening now?" UI cares about.
	type fingerprint struct {
		state           string
		step            string
		progressPercent int
		completedSteps  int
		failedSteps     int
		completedAt     string
	}
	makeFingerprint := func(s *WorkflowStatus) fingerprint {
		if s == nil {
			return fingerprint{}
		}
		return fingerprint{
			state:           s.State,
			step:            s.Step,
			progressPercent: s.ProgressPercent,
			completedSteps:  s.CompletedSteps,
			failedSteps:     s.FailedSteps,
			completedAt:     s.CompletedAt,
		}
	}

	pushFrame := func(status *WorkflowStatus) bool {
		if _, err := io.WriteString(w, "event: workflow\ndata: "); err != nil {
			return false
		}
		if err := encoder.Encode(status); err != nil {
			return false
		}
		// json.Encoder appends a trailing \n; SSE needs an extra
		// blank line as the message terminator.
		if _, err := io.WriteString(w, "\n"); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Push an immediate first frame so the client doesn't wait a
	// full tick to see initial state. The connection has only just
	// opened so we always emit, fingerprint comes from the actual
	// payload below.
	if !pushFrame(initial) {
		return
	}
	last := makeFingerprint(initial)

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, "event: heartbeat\ndata: "+time.Now().UTC().Format(time.RFC3339)+"\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-tick.C:
			status, err := h.workflow.GetStatus(userID, fundID)
			if err != nil {
				// Soft-fail — a transient error shouldn't tear down
				// the connection. Re-try next tick.
				continue
			}
			fp := makeFingerprint(status)
			if fp == last {
				continue
			}
			last = fp
			if !pushFrame(status) {
				return
			}
			// If the workflow has reached a terminal state there's
			// no more changes to surface. Close the connection so
			// the client doesn't keep an idle socket open
			// indefinitely. The frontend hook treats a clean close
			// after a "completed" / "failed" event as an expected
			// outcome and stops reconnecting.
			if status != nil && (status.State == "completed" || status.State == "failed") {
				return
			}
		}
	}
}
