// db_pool_handler.go — admin-only JSON view onto the DB connection pool.
//
// WHY THIS EXISTS
// ---------------
// The existing /api/metrics endpoint already emits the pool stats in
// Prometheus text format (open / in_use / idle / wait_count / churn).
// That's the right shape for scraping but a terrible shape for an
// operator who has SSH'd into the box at 2 AM and just wants to know
// "is the pool saturated right now?". A small JSON endpoint that
// returns the SAME numbers plus the configured limits and computed
// utilization gives that operator a one-liner answer without having
// to grep Prometheus text.
//
// The endpoint is super-admin gated (same gate as
// /api/admin/stop-trigger/status). It is read-only — no mutation; the
// pool tuning knobs are environment-controlled at boot. A future PR
// could add hot-reload of pool limits, but that's a separate concern.

package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"
)

type dbPoolHandler struct {
	db  *sql.DB
	cfg *Config
}

func newDBPoolHandler(svc *Services, cfg *Config) *dbPoolHandler {
	if svc == nil || svc.DB == nil || cfg == nil {
		return nil
	}
	return &dbPoolHandler{db: svc.DB, cfg: cfg}
}

func (h *dbPoolHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/db-pool/status", h.handleStatus)
}

// dbPoolStatus is the wire shape served by the handler. Mirrors
// sql.DBStats with computed convenience fields tagged on top.
type dbPoolStatus struct {
	// Live pool state.
	OpenConnections int `json:"openConnections"`
	InUse           int `json:"inUseConnections"`
	Idle            int `json:"idleConnections"`

	// Configured limits, surfaced so the operator doesn't have to
	// remember which env vars wired the pool.
	MaxOpenConnections   int           `json:"maxOpenConnections"`
	MaxIdleConnsConfig   int           `json:"maxIdleConnsConfig"`
	ConnMaxLifetime      time.Duration `json:"connMaxLifetimeNs"`
	ConnMaxLifetimeHuman string        `json:"connMaxLifetime"`

	// Computed signals: utilization-pct + average wait. Negative
	// values signal "undefined" (see exportRuntimePrometheus
	// rationale).
	UtilizationPct float64 `json:"utilizationPct"`
	WaitAvgSeconds float64 `json:"waitAvgSeconds"`

	// Cumulative counters since process start.
	WaitCount         int64         `json:"waitCount"`
	WaitDuration      time.Duration `json:"waitDurationNs"`
	WaitDurationHuman string        `json:"waitDurationHuman"`
	MaxIdleClosed     int64         `json:"maxIdleClosedTotal"`
	MaxIdleTimeClosed int64         `json:"maxIdleTimeClosedTotal"`
	MaxLifetimeClosed int64         `json:"maxLifetimeClosedTotal"`

	// Server-side meta. observedAt lets the UI show "as of <time>"
	// without needing a separate clock fetch.
	ObservedAt time.Time `json:"observedAt"`
}

func (h *dbPoolHandler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	stats := h.db.Stats()

	utilization := -1.0
	if stats.MaxOpenConnections > 0 {
		utilization = 100.0 * float64(stats.InUse) / float64(stats.MaxOpenConnections)
	}
	waitAvg := -1.0
	if stats.WaitCount > 0 {
		waitAvg = stats.WaitDuration.Seconds() / float64(stats.WaitCount)
	}

	out := dbPoolStatus{
		OpenConnections:      stats.OpenConnections,
		InUse:                stats.InUse,
		Idle:                 stats.Idle,
		MaxOpenConnections:   stats.MaxOpenConnections,
		MaxIdleConnsConfig:   h.cfg.DBMaxIdleConns,
		ConnMaxLifetime:      h.cfg.DBConnMaxLife,
		ConnMaxLifetimeHuman: h.cfg.DBConnMaxLife.String(),
		UtilizationPct:       utilization,
		WaitAvgSeconds:       waitAvg,
		WaitCount:            stats.WaitCount,
		WaitDuration:         stats.WaitDuration,
		WaitDurationHuman:    stats.WaitDuration.String(),
		MaxIdleClosed:        stats.MaxIdleClosed,
		MaxIdleTimeClosed:    stats.MaxIdleTimeClosed,
		MaxLifetimeClosed:    stats.MaxLifetimeClosed,
		ObservedAt:           time.Now().UTC(),
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
	}
}
