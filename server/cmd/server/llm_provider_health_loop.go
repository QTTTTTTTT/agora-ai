// llm_provider_health_loop.go — S14.A: 5-minute probe loop that
// pings every active platform LLM provider and persists results to
// platform_llm_provider_health_history + last_health_check_*.
//
// Why a dedicated loop (not on-demand probes only):
//   * S13's "Test Connection" button only fires when an operator
//     clicks it. The observability dashboard needs continuous
//     health signal so an outage today shows up on tomorrow's
//     retro without anyone clicking anything.
//   * 5-minute resolution is enough to catch upstream provider
//     outages (claude went down for 30 min once — we'd have ≥6
//     data points) without dollar-burn (one 1-token ping per
//     provider every 5 min costs ~$0.0001/day/provider).
//
// Failure handling: probe errors are logged and persisted (the
// whole point is to record failures); DB write errors are logged
// but never bubble up — we don't want a transient DB hiccup to
// kill the loop. The loop survives the lifetime of the process.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fundai/server/internal/repository"
	"github.com/google/uuid"
)

// llmHealthProbeLoop scans active platform LLM providers and pings
// each one on a fixed schedule. Safe to construct with any subset of
// nil dependencies — Start() degrades to a no-op in that case so the
// server can boot in minimal-deps test environments.
type llmHealthProbeLoop struct {
	providerRepo *repository.PlatformLLMProviderRepo
	historyRepo  *repository.ProviderHealthHistoryRepo
	interval     time.Duration
	retention    time.Duration
	probeTimeout time.Duration
	logger       *slog.Logger
	// Probes counts completed iterations of the loop body, exposed
	// via the admin /health endpoint payload for sanity checks
	// ("is the loop even running?").
	probes atomic.Int64
}

// newLLMHealthProbeLoop wires the loop with production defaults.
// All durations can be overridden via env (LLM_HEALTH_PROBE_INTERVAL,
// LLM_HEALTH_RETENTION) but we keep defaults conservative.
func newLLMHealthProbeLoop(providerRepo *repository.PlatformLLMProviderRepo, historyRepo *repository.ProviderHealthHistoryRepo, logger *slog.Logger) *llmHealthProbeLoop {
	if logger == nil {
		logger = slog.Default()
	}
	return &llmHealthProbeLoop{
		providerRepo: providerRepo,
		historyRepo:  historyRepo,
		interval:     5 * time.Minute,
		retention:    30 * 24 * time.Hour,
		probeTimeout: 8 * time.Second,
		logger:       logger,
	}
}

// Start kicks off the loop in a goroutine. Returns immediately. If
// any required dependency is nil, logs once and returns — the rest
// of the system stays functional.
func (l *llmHealthProbeLoop) Start(ctx context.Context) {
	if l == nil || l.providerRepo == nil || l.historyRepo == nil {
		slog.Info("llm_health_probe_loop: skipped (deps unwired)")
		return
	}
	// First retention sweep at startup so a pod that's been off for
	// a month doesn't drag old rows around.
	go func() {
		cutoff := time.Now().Add(-l.retention)
		if n, err := l.historyRepo.DeleteOlderThan(ctx, cutoff); err == nil && n > 0 {
			l.logger.Info("llm_health_probe_loop: pruned old rows", "deleted", n, "cutoff", cutoff)
		}
	}()
	go l.run(ctx)
}

// Probes returns the cumulative number of tick iterations completed.
// Used by tests + the admin observability endpoint.
func (l *llmHealthProbeLoop) Probes() int64 {
	if l == nil {
		return 0
	}
	return l.probes.Load()
}

func (l *llmHealthProbeLoop) run(ctx context.Context) {
	// Stagger the first probe by 10s after boot so the server has
	// time to warm caches; otherwise startup logs look like a wall
	// of LLM activity.
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	dailyCleanup := time.NewTicker(24 * time.Hour)
	defer dailyCleanup.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			l.tick(ctx)
			l.probes.Add(1)
			timer.Reset(l.interval)
		case <-dailyCleanup.C:
			cutoff := time.Now().Add(-l.retention)
			if n, err := l.historyRepo.DeleteOlderThan(ctx, cutoff); err == nil && n > 0 {
				l.logger.Info("llm_health_probe_loop: daily prune", "deleted", n, "cutoff", cutoff)
			}
		}
	}
}

func (l *llmHealthProbeLoop) tick(ctx context.Context) {
	rows, err := l.providerRepo.ListAll(ctx, repository.ListFilters{Status: "active"})
	if err != nil {
		l.logger.Warn("llm_health_probe_loop: list providers", "err", err)
		return
	}
	for i := range rows {
		l.probeOne(ctx, &rows[i])
	}
}

// probeOne runs the probe + persists both the per-tick history row
// AND the last_health_check_* snapshot. We intentionally don't
// short-circuit on a transient transport error — we still record
// the failure so the dashboard sees it.
func (l *llmHealthProbeLoop) probeOne(ctx context.Context, row *repository.PlatformLLMProviderRow) {
	plain, err := row.PlainAPIKey()
	if err != nil {
		l.persistResult(ctx, row, testLLMProviderResponse{
			OK:      false,
			Message: "decrypt failed: " + truncateMessage(err.Error()),
		})
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, l.probeTimeout)
	defer cancel()
	result := runProviderPing(probeCtx, row.Provider, row.BaseURL, row.ModelName, plain)
	l.persistResult(ctx, row, result)
}

// persistResult writes the history row + updates the snapshot. Each
// step is best-effort; a write failure on one doesn't block the
// other.
func (l *llmHealthProbeLoop) persistResult(ctx context.Context, row *repository.PlatformLLMProviderRow, result testLLMProviderResponse) {
	if err := l.historyRepo.Insert(ctx, repository.ProviderHealthRow{
		ProviderID: row.ID,
		Provider:   row.Provider,
		Label:      row.Label,
		CheckedAt:  time.Now().UTC(),
		OK:         result.OK,
		LatencyMS:  int(result.LatencyMS),
		HTTPStatus: result.HTTPStatus,
		Message:    nullStringFromMessage(result.Message),
		ModelName:  sql.NullString{Valid: row.ModelName != "", String: row.ModelName},
	}); err != nil {
		l.logger.Warn("llm_health_probe_loop: insert history",
			"err", err, "provider", row.Provider, "label", row.Label)
	}
	snap := map[string]any{
		"ok":           result.OK,
		"latency_ms":   result.LatencyMS,
		"http_status":  result.HTTPStatus,
		"message":      result.Message,
		"echoed_model": result.EchoedModel,
		"checked_at":   time.Now().UTC().Format(time.RFC3339),
		"source":       "probe_loop",
	}
	if err := l.providerRepo.TouchHealth(ctx, row.ID, snap); err != nil {
		l.logger.Warn("llm_health_probe_loop: touch_health",
			"err", err, "provider", row.Provider, "label", row.Label)
	}
}

// nullStringFromMessage avoids storing "" in TEXT columns (it
// makes "select where message is null" misleading). Empty stays
// NULL; everything else is truncated to fit the column.
func nullStringFromMessage(s string) sql.NullString {
	s = strings.TrimSpace(s)
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{Valid: true, String: truncateMessage(s)}
}

// truncateMessage keeps stored messages bounded so a verbose
// HTML error page from an HTTP proxy can't blow up a row.
func truncateMessage(s string) string {
	if len(s) <= 200 {
		return s
	}
	return s[:200] + "…"
}

// --- helpers used by admin endpoints for ad-hoc snapshot decoding ---

// healthSnapshot decodes the last_health_check_result JSONB column
// into a stable map shape. Returns nil + nil error when the column
// is empty/NULL so the caller can render "—" in the UI.
func healthSnapshot(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode health snapshot: %w", err)
	}
	return out, nil
}

// errProbeLoopUnwired is returned by helpers when the loop hasn't
// been started — exported as a sentinel for the unit tests.
var errProbeLoopUnwired = errors.New("llm_health_probe_loop: unwired")

// providerIDFromString is a small helper so the admin handler can
// translate path params without importing uuid in every file.
func providerIDFromString(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(s))
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid provider id: %w", err)
	}
	return id, nil
}
