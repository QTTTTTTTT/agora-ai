package modelab

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Resolver is the hot-path entry point the ModelRouter calls on
// every LLM request. Given a request's routing tuple it returns:
//
//   - the experiment the call is participating in (or nil if no
//     experiment matches)
//   - the index + label of the arm this call is bound to
//   - the ArmConfig the router should overlay onto the request
//
// The resolver caches the list of running experiments in-memory
// with a short TTL so the hot path doesn't hit the DB on every
// LLM call. Background refresh is best-effort — a stale cache
// just means new experiments take up to RefreshInterval to
// propagate, which is acceptable for this feature.
//
// Sticky-arm semantics. The (run_id, step, agent_id) tuple is
// hashed deterministically AND persisted into model_ab_assignments
// on the first call. Subsequent calls for the same tuple read
// the existing row and reuse its arm, so changing the
// experiment's traffic_split mid-day does NOT split a running
// workflow across two models.
type Resolver struct {
	repo *Repo

	// RefreshInterval is the upper bound on stale cached
	// experiments. Defaults to 30s.
	RefreshInterval time.Duration

	// Logger receives structured events (cache refresh, miss
	// reasons, persistence failures). Nil falls back to slog.Default.
	Logger *slog.Logger

	mu      sync.RWMutex
	cache   []*Experiment
	expires time.Time
}

// NewResolver constructs a Resolver. A nil repo is permitted —
// the resolver then always returns Decision{} ("no match"), so
// callers without a DB in scope (smoke tests, the unit test
// build) degrade cleanly.
func NewResolver(repo *Repo) *Resolver {
	return &Resolver{
		repo:            repo,
		RefreshInterval: 30 * time.Second,
	}
}

// Decision is the resolver's output. When InExperiment is false
// the other fields are meaningless and the router should fall
// through to its existing tier/agent default resolution.
type Decision struct {
	InExperiment bool
	Experiment   *Experiment
	ArmIndex     int
	Arm          ArmConfig
	Assignment   *Assignment
}

// Resolve looks up the experiment for a given LLM call. The
// routing tuple parts:
//
//	fundID    the fund the call belongs to
//	agentID   the AgentID column on agents row (or "" for synthetic
//	          callers like sentiment-scorer)
//	agentRole the role of the agent (PM / risk / researcher / trader)
//	step      the StepName the request carries (pm_decision, debate, ...)
//	runID     the workflow_run identifier — required for sticky arms,
//	          empty fallbacks to a per-call random key, which destroys
//	          stickiness, so callers SHOULD supply a stable string
//	          (e.g. workflow_run_id + trading_date).
//
// Resolve is safe to call with all-empty inputs; it just returns
// Decision{}.
func (r *Resolver) Resolve(ctx context.Context, fundID, agentID, agentRole, step, runID string) Decision {
	if r == nil || r.repo == nil {
		return Decision{}
	}
	exps := r.snapshot(ctx)
	if len(exps) == 0 {
		return Decision{}
	}
	// Match the first applicable experiment. We intentionally
	// pick the FIRST hit (which is newest by created_at because
	// ListRunningMatching orders DESC) so operators can
	// "override" a global experiment with a fund-scoped one by
	// creating the fund one later.
	var matched *Experiment
	for _, e := range exps {
		if !e.Match(fundID, agentID, agentRole, step) {
			continue
		}
		if e.BudgetExhausted() {
			r.logger().Info("modelab.budget_exhausted",
				"experiment_id", e.ID,
				"tokens_used", e.TokensUsed,
				"max_total_tokens", e.MaxTotalTokens,
			)
			continue
		}
		matched = e
		break
	}
	if matched == nil {
		return Decision{}
	}

	armIdx := PickArm(runID, step, agentID, matched.TrafficSplit)
	if armIdx < 0 || armIdx >= len(matched.Arms) {
		return Decision{}
	}
	arm := matched.Arms[armIdx]

	// Try to record the sticky assignment. Failures here are
	// non-fatal — we still return the arm so the call proceeds.
	// The next call for the same tuple will retry the upsert.
	assignment := &Assignment{
		ExperimentID: matched.ID,
		RunID:        runID,
		Step:         step,
		AgentID:      agentID,
		FundID:       fundID,
		ArmIndex:     armIdx,
		ArmName:      arm.Name,
	}
	persisted, err := r.repo.UpsertAssignment(ctx, assignment)
	if err != nil {
		r.logger().Warn("modelab.upsert_assignment_failed",
			"experiment_id", matched.ID,
			"run_id", runID,
			"step", step,
			"err", err,
		)
	} else {
		// If the row already existed, the returned arm wins —
		// honour the stickiness invariant by re-reading the
		// persisted arm rather than the one we just computed.
		assignment = persisted
		if persisted.ArmIndex >= 0 && persisted.ArmIndex < len(matched.Arms) {
			armIdx = persisted.ArmIndex
			arm = matched.Arms[armIdx]
		}
	}

	return Decision{
		InExperiment: true,
		Experiment:   matched,
		ArmIndex:     armIdx,
		Arm:          arm,
		Assignment:   assignment,
	}
}

// Invalidate clears the cached experiment list. CRUD handlers
// should call this after creating / pausing / completing an
// experiment so the change propagates without waiting for the
// next TTL refresh.
func (r *Resolver) Invalidate() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.cache = nil
	r.expires = time.Time{}
	r.mu.Unlock()
}

func (r *Resolver) snapshot(ctx context.Context) []*Experiment {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	if time.Now().Before(r.expires) && r.cache != nil {
		out := r.cache
		r.mu.RUnlock()
		return out
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Now().Before(r.expires) && r.cache != nil {
		// Another goroutine refreshed while we were waiting.
		return r.cache
	}
	exps, err := r.repo.ListExperiments(ctx, []ExperimentStatus{StatusRunning}, 100)
	if err != nil {
		r.logger().Warn("modelab.cache_refresh_failed", "err", err)
		// Keep the old cache (if any) so a transient DB blip
		// doesn't flip every call back to baseline.
		if r.cache != nil {
			return r.cache
		}
		return nil
	}
	r.cache = exps
	ttl := r.RefreshInterval
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	r.expires = time.Now().Add(ttl)
	return r.cache
}

func (r *Resolver) logger() *slog.Logger {
	if r == nil || r.Logger == nil {
		return slog.Default()
	}
	return r.Logger
}

// SanitizeAgentRole normalises whatever the caller passed (PM /
// pm / Pm / "portfolio_manager") into the canonical lower-case
// role the Match function compares against. Exposed so the
// router can do the normalisation once and not in every business
// caller.
func SanitizeAgentRole(role string) string {
	r := strings.ToLower(strings.TrimSpace(role))
	switch r {
	case "portfolio_manager", "portfoliomanager":
		return "pm"
	case "risk_officer", "riskofficer":
		return "risk"
	default:
		return r
	}
}

// ErrNoRunID is returned by Resolve when called with an empty
// runID. The current caller chain ALWAYS has a runID in scope
// (workflow runs synthesise one before the first agent call), so
// an empty runID signals a wiring bug worth surfacing in tests.
var ErrNoRunID = errors.New("modelab: runID is required for sticky-arm assignment")
