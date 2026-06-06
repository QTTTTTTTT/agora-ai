// memreembed_handler.go — admin-only JSON view onto the W6-1
// memory re-embed queue.
//
// WHY THIS EXISTS
// ---------------
// The /api/metrics endpoint already emits the queue stats in
// Prometheus text format (W7-1: pending / embedded_total /
// retried_total / dead_letter_total / status). That's the right
// shape for scraping but a lousy shape for an operator who has
// SSH'd into the box at 2 AM and wants to know "is the queue
// piling up?". A small JSON endpoint that returns the SAME
// numbers, plus the configured queue limits and a freshness
// timestamp, gives that operator a one-liner answer without
// having to grep Prometheus text.
//
// The endpoint is super-admin gated (same gate as the DB-pool
// status handler in db_pool_handler.go). It is read-only — no
// mutation; the queue knobs are environment-controlled at boot.
// A future PR could add a "purge dead-letter" mutation, but
// that's a separate concern with its own audit trail.

package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/fundai/server/internal/memreembed"
)

type memReembedHandler struct {
	queue *memreembed.Queue
}

func newMemReembedHandler(svc *Services) *memReembedHandler {
	if svc == nil {
		return nil
	}
	// Allow a nil queue: handler still registers and reports
	// `enabled=false` so the Admin UI panel can render a "re-embed
	// disabled" state instead of a 404 it would have to special-case.
	return &memReembedHandler{queue: svc.MemReembedQueue}
}

func (h *memReembedHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/memreembed/status", h.handleStatus)
}

// memReembedStatus is the wire shape served by the handler. The
// `Enabled` discriminator lets the UI render a disabled-state
// card without having to inspect each numeric field for a
// sentinel value.
type memReembedStatus struct {
	Enabled bool `json:"enabled"`

	// Live counters. `Pending` is a gauge (rises and falls);
	// the *_Total fields are monotonic counters since process
	// start, matching the corresponding Prometheus counter
	// metrics surfaced by exportMemReembedPrometheus.
	Pending          int    `json:"pending"`
	EmbeddedTotal    int64  `json:"embeddedTotal"`
	RetriedTotal     int64  `json:"retriedTotal"`
	DeadLetterTotal  int64  `json:"deadLetterTotal"`
	LastErrorUnix    int64  `json:"lastErrorUnix,omitempty"`
	LastErrorTimeISO string `json:"lastErrorTime,omitempty"`

	// Server-side meta. `ObservedAt` is RFC3339 UTC so the UI
	// can render "as of <time>" without a separate clock fetch.
	ObservedAt time.Time `json:"observedAt"`
}

func (h *memReembedHandler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}

	out := memReembedStatus{
		Enabled:    h != nil && h.queue != nil,
		ObservedAt: time.Now().UTC(),
	}
	if h != nil && h.queue != nil {
		stats := h.queue.Stats()
		out.Pending = stats.Pending
		out.EmbeddedTotal = stats.Embedded
		out.RetriedTotal = stats.Retried
		out.DeadLetterTotal = stats.DeadLetter
		if !stats.LastErrTime.IsZero() {
			out.LastErrorUnix = stats.LastErrTime.Unix()
			out.LastErrorTimeISO = stats.LastErrTime.UTC().Format(time.RFC3339)
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
	}
}
