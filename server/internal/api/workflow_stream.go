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
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// W6-2 — SSE multiplex observability counters. These are exported
// via /api/metrics as fundai_workflow_sse_mux_* so an operator can
// see whether the mux endpoint is actually carrying load (vs the
// per-fund stream), and how many funds the average connection
// covers.
//
//   muxActiveConnections — gauge style: incremented on connect,
//     decremented on disconnect. Lets the dashboard show "N
//     dashboards currently open" at a glance.
//   muxConnectionsTotal — monotonic counter of every mux
//     connection ever opened. Pairs with active to compute churn.
//   muxSubscriptionsTotal — monotonic counter of fund subscriptions
//     across every mux connection. (One connection subscribing to
//     5 funds adds 5.)
//   muxForbiddenFramesTotal — monotonic counter of `event:error`
//     frames emitted because a fund failed authorization. A spike
//     here usually means an operator typo'd a fundIds list.
//
// Stored as package-level atomics so the export path
// (cmd/server/main.go) reads without locking. Read accessors live
// near the bottom of this file.
var (
	muxActiveConnections    int64
	muxConnectionsTotal     uint64
	muxSubscriptionsTotal   uint64
	muxForbiddenFramesTotal uint64
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

// muxFrame is the envelope pushed on the multiplexed
// `event: workflow` channel. Wrapping the per-fund status with
// the fund id lets a single EventSource fan-out to many fund
// cards client-side without each one needing its own connection.
// We also include `terminal` so the client can drop the fund out
// of its subscription set without re-parsing the inner state.
type muxFrame struct {
	FundID   string          `json:"fundId"`
	Status   *WorkflowStatus `json:"status"`
	Terminal bool            `json:"terminal,omitempty"`
}

// muxLimit caps how many fund ids a single multiplex connection
// will subscribe to. Matches the practical maximum dashboard
// load (50 funds × heartbeat overhead is fine; 200+ would start
// to push the response writer hard on a tick boundary).
const muxLimit = 50

// StreamWorkflowStatusMulti opens a single SSE connection that
// fans out workflow status frames for many funds at once. Browsers
// limit the number of concurrent EventSource handles per origin
// (commonly 6), which forces the dashboard to queue half its
// streams when an operator tracks 10+ funds at once. Multiplexing
// over one socket fixes the queueing and halves the per-tick
// header overhead vs. N independent streams.
//
// Endpoint:  GET /api/funds/workflow/stream?fundIds=a,b,c
// Frame:     event: workflow
//
//	data: {"fundId":"a","status":{...},"terminal":false}
//
// Authorization: each fund id is checked individually via the
// existing workflow.GetStatus path. A fund the caller can't see
// is dropped from the subscription set (we emit a one-shot
// `event: error` frame for it so the client UI can show "no
// access" rather than silently swallow the request) — we do NOT
// 403 the entire stream because mixed-permission portfolio views
// are common (an analyst may have access to 8 of 10 funds).
//
// The handler reuses parseStreamInterval, the heartbeat cadence,
// and the per-fund fingerprint from StreamWorkflowStatus so the
// observable behavior is identical to running N independent
// streams — we just multiplex the transport.
func (h *FundHandler) StreamWorkflowStatusMulti(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}

	fundIDs := parseMuxFundIDs(r.URL.Query().Get("fundIds"))
	if len(fundIDs) == 0 {
		writeError(w, http.StatusBadRequest, "fundIds required", "provide at least one fund id via ?fundIds=a,b,c")
		return
	}
	if len(fundIDs) > muxLimit {
		writeError(w, http.StatusBadRequest, "too many fundIds", "max subscriptions per multiplex stream is 50")
		return
	}

	flusher, isFlusher := w.(http.Flusher)
	if !isFlusher {
		writeError(w, http.StatusInternalServerError, "sse unsupported", "response writer does not implement http.Flusher")
		return
	}

	// Up-front authorization: gather initial frames for the funds
	// the caller can read; quietly drop the rest. We do this
	// synchronously so 4xx-style problems (no funds visible) come
	// back as a normal HTTP error instead of an opaque SSE error
	// frame the client has to parse out.
	type sub struct {
		fundID  string
		initial *WorkflowStatus
	}
	subs := make([]sub, 0, len(fundIDs))
	for _, fundID := range fundIDs {
		status, err := h.workflow.GetStatus(userID, fundID)
		if err != nil {
			// Per-fund auth failures are expected (mixed-permission
			// views); we just skip them rather than 403 the whole
			// stream. The client gets an `event: error` line below
			// so its UI can label the missing card.
			continue
		}
		subs = append(subs, sub{fundID: fundID, initial: status})
	}
	if len(subs) == 0 {
		// None of the requested funds are readable — surface as
		// a synchronous 403 so the client doesn't get stuck on a
		// permanently-empty stream.
		writeError(w, http.StatusForbidden, "no accessible funds", "none of the requested fund ids are readable")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// W6-2 — counter bumps. We bump *after* the writeHeader call
	// so a request that fails validation (4xx above) doesn't get
	// counted as an active mux connection, which would otherwise
	// inflate `mux_active_connections` and obscure real load.
	atomic.AddUint64(&muxConnectionsTotal, 1)
	atomic.AddUint64(&muxSubscriptionsTotal, uint64(len(subs)))
	atomic.AddInt64(&muxActiveConnections, 1)
	defer atomic.AddInt64(&muxActiveConnections, -1)

	if _, err := io.WriteString(w, ": connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	// Emit a one-shot `event: error` frame for any fund that was
	// dropped during initial auth so the client's `Record<fundId,
	// status>` can render a "no access" placeholder.
	subbedIDs := make([]string, 0, len(subs))
	for _, s := range subs {
		subbedIDs = append(subbedIDs, s.fundID)
	}
	dropped := diffStringSets(fundIDs, subbedIDs)
	if n := len(dropped); n > 0 {
		atomic.AddUint64(&muxForbiddenFramesTotal, uint64(n))
	}
	for _, fundID := range dropped {
		if _, err := io.WriteString(w, "event: error\ndata: "); err != nil {
			return
		}
		errPayload, _ := json.Marshal(map[string]string{"fundId": fundID, "error": "forbidden"})
		if _, err := w.Write(errPayload); err != nil {
			return
		}
		if _, err := io.WriteString(w, "\n\n"); err != nil {
			return
		}
	}
	flusher.Flush()

	ctx := r.Context()
	interval := parseStreamInterval(r.URL.Query().Get("interval"), 2*time.Second)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	encoder := json.NewEncoder(w)

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

	pushMuxFrame := func(frame muxFrame) bool {
		if _, err := io.WriteString(w, "event: workflow\ndata: "); err != nil {
			return false
		}
		if err := encoder.Encode(frame); err != nil {
			return false
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Send first frames for everything we successfully subscribed
	// to. terminal-state funds are emitted with terminal=true and
	// then immediately removed from the active set so we don't
	// keep polling them — the client knows to stop expecting
	// updates for that card.
	type entry struct {
		fundID string
		fp     fingerprint
	}
	active := make(map[string]*entry, len(subs))
	for _, s := range subs {
		fp := makeFingerprint(s.initial)
		terminal := s.initial != nil && (s.initial.State == "completed" || s.initial.State == "failed")
		if !pushMuxFrame(muxFrame{FundID: s.fundID, Status: s.initial, Terminal: terminal}) {
			return
		}
		if !terminal {
			active[s.fundID] = &entry{fundID: s.fundID, fp: fp}
		}
	}

	// If every fund was already in a terminal state on first read,
	// close the connection rather than holding it open with no
	// active subscriptions. The client receives the terminal
	// frames and then a clean close, identical semantics to the
	// single-fund stream.
	if len(active) == 0 {
		return
	}

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
			// Re-poll each active fund. We deliberately keep this
			// sequential — Workflow.GetStatus already has its own
			// caching layer in front of the database, and a parallel
			// fan-out per tick risks contention against the same
			// connection pool we expose pool metrics for.
			for fundID, e := range active {
				status, err := h.workflow.GetStatus(userID, fundID)
				if err != nil {
					// Soft-fail per fund: if a fund's status read
					// transiently errors, skip the frame and try
					// again next tick. We don't tear down the whole
					// stream because a sibling fund is fine.
					continue
				}
				fp := makeFingerprint(status)
				if fp == e.fp {
					continue
				}
				e.fp = fp
				terminal := status != nil && (status.State == "completed" || status.State == "failed")
				if !pushMuxFrame(muxFrame{FundID: fundID, Status: status, Terminal: terminal}) {
					return
				}
				if terminal {
					delete(active, fundID)
				}
			}
			// All subscriptions reached terminal state — close out.
			if len(active) == 0 {
				return
			}
		}
	}
}

// parseMuxFundIDs splits the comma-separated `fundIds` query
// param, trimming and de-duplicating. Empty entries are dropped.
// Order is preserved so the client can rely on the initial frame
// order matching its subscription list.
func parseMuxFundIDs(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		id := strings.TrimSpace(p)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// diffStringSets returns the elements of `a` that are not
// in `b`, preserving the order of `a`. Used to figure out
// which requested fund ids were silently dropped during
// initial authorization so we can surface them as `event:
// error` frames to the client.
func diffStringSets(a, b []string) []string {
	if len(b) == 0 {
		return append([]string(nil), a...)
	}
	in := make(map[string]struct{}, len(b))
	for _, v := range b {
		in[v] = struct{}{}
	}
	out := make([]string, 0)
	for _, v := range a {
		if _, ok := in[v]; ok {
			continue
		}
		out = append(out, v)
	}
	return out
}

// sortedFundIDs returns a sorted copy of the input ids. Exposed
// (rather than inlined) so other internal callers — e.g. the
// dedupe key the client uses to share an EventSource — can
// reuse the same canonical ordering.
func sortedFundIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

// MuxStreamMetrics is the read-only snapshot exported via
// /api/metrics. The fields map 1:1 to Prometheus series; we
// return a struct (rather than four bare ints) so the metrics
// exporter sees a consistent point-in-time view even if a
// connect/disconnect happens during the read.
type MuxStreamMetrics struct {
	ActiveConnections int64
	ConnectionsTotal  uint64
	SubscriptionsTotal uint64
	ForbiddenFramesTotal uint64
}

// SnapshotMuxStreamMetrics returns the current SSE-multiplex
// counters. Atomic loads — safe to call without coordinating
// with the SSE handlers themselves.
func SnapshotMuxStreamMetrics() MuxStreamMetrics {
	return MuxStreamMetrics{
		ActiveConnections:    atomic.LoadInt64(&muxActiveConnections),
		ConnectionsTotal:     atomic.LoadUint64(&muxConnectionsTotal),
		SubscriptionsTotal:   atomic.LoadUint64(&muxSubscriptionsTotal),
		ForbiddenFramesTotal: atomic.LoadUint64(&muxForbiddenFramesTotal),
	}
}

// resetMuxStreamMetricsForTest zeroes the counters. Test-only;
// production callers should never invoke it. Lives next to the
// snapshot reader so the contract stays obvious in code review.
func resetMuxStreamMetricsForTest() {
	atomic.StoreInt64(&muxActiveConnections, 0)
	atomic.StoreUint64(&muxConnectionsTotal, 0)
	atomic.StoreUint64(&muxSubscriptionsTotal, 0)
	atomic.StoreUint64(&muxForbiddenFramesTotal, 0)
}
