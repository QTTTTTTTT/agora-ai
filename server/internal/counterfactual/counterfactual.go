// Package counterfactual produces leave-one-out (LOO)
// attribution reports for the W1-4 decision provenance.
//
// MOTIVATION
// ----------
// The provenance row records every input that shaped a plan
// (signal blocks, lessons, skills, agent panel composition).
// The plan_outcome row records the realised performance. The
// missing question: "which inputs *moved the needle* on the
// realised alpha?". Two-decimal alpha attribution to lessons
// and skills is the loop-closing signal for self-learning.
//
// W3-17 introduces a counterfactual scorer:
//
//   * For each input I that was present, ask: "if we had run
//     the same decision pipeline without I, what would the
//     outcome have been?"
//   * The counterfactual outcome is estimated by a Replayer
//     (the wiring layer supplies one — backtest, shadow agent,
//     or simple historical-mean baseline).
//   * The contribution of I is realised_alpha − cf_alpha[I].
//
// SCALE
// -----
// Re-running an LLM panel for every leave-one-out check would
// be ruinous. The package exposes the Replayer interface so a
// concrete implementation can be any of:
//
//   * A backtest replay (recompute the strategy_action with one
//     fewer block in the prompt). Most expensive, most accurate.
//   * A historical baseline ("plans of similar shape without I
//     averaged X% alpha"). Cheap, less accurate.
//   * A shadow-agent run (W3-19). Medium cost, medium accuracy.
//
// SCOPE
// -----
//   * Owns the Input value type (the unit of attribution),
//     Attribution row, Report struct, Run() function.
//   * Does NOT own the replay implementation. Wiring layer
//     selects which Replayer to wire based on cost / latency
//     budget at runtime.
package counterfactual

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// InputKind labels what kind of decision input is being
// attributed. Same set as the W1-4 provenance vocabulary so
// downstream UIs render with consistent icons.
type InputKind string

const (
	InputLesson      InputKind = "lesson"
	InputSkill       InputKind = "skill"
	InputSignalBlock InputKind = "signal_block"
	InputAgent       InputKind = "agent"
)

// Input is one attribution unit — typically a lesson, skill,
// signal block, or agent.
type Input struct {
	Kind   InputKind         `json:"kind"`
	ID     string            `json:"id"`
	Label  string            `json:"label,omitempty"`
	Tags   []string          `json:"tags,omitempty"`
	Extra  map[string]string `json:"extra,omitempty"`
}

// Replayer answers "what would alpha have been without input I?".
// Implementations:
//
//   * BacktestReplayer: reruns the strategy action.
//   * HistoricalBaselineReplayer: returns the mean alpha of
//     plans that didn't have this input.
//   * NullReplayer (test default): returns zero contribution.
type Replayer interface {
	// CounterfactualAlpha returns the estimated alpha if the
	// given input had been absent. The bool is false when the
	// implementation cannot produce an estimate (e.g. no
	// historical comparator data) — the caller treats that as
	// "skip this input".
	CounterfactualAlpha(ctx context.Context, planID string, missing Input) (float64, bool, error)
}

// Attribution is one row in the LOO report.
type Attribution struct {
	Input        Input   `json:"input"`
	WithAlpha    float64 `json:"withAlpha"`    // realised
	WithoutAlpha float64 `json:"withoutAlpha"` // counterfactual
	Contribution float64 `json:"contribution"` // with - without
	Confidence   string  `json:"confidence"`   // high | low | unavailable
}

// Report is the aggregate LOO report for a single plan.
type Report struct {
	PlanID         string        `json:"planId"`
	RealisedAlpha  float64       `json:"realisedAlpha"`
	Attributions   []Attribution `json:"attributions"`
	NetContribution float64      `json:"netContribution"`
	Generated       time.Time    `json:"generated"`
}

// Config tunes the runner. Currently just a parallelism cap.
type Config struct {
	// MaxConcurrency caps the number of replayer calls in
	// flight at once. 0 falls back to 4.
	MaxConcurrency int
	// Timeout per replayer call. 0 means no timeout (rely on
	// caller context).
	PerCallTimeout time.Duration
}

// DefaultConfig is the production-safe baseline.
func DefaultConfig() Config {
	return Config{
		MaxConcurrency: 4,
		PerCallTimeout: 30 * time.Second,
	}
}

// Run produces a LOO report.
//
// inputs is the list of attribution units (typically the
// lessons + skills + agents from the plan's provenance).
// realised is the observed alpha. cfg controls concurrency.
func Run(ctx context.Context, planID string, realised float64, inputs []Input, replayer Replayer, cfg Config) (Report, error) {
	if planID == "" {
		return Report{}, fmt.Errorf("counterfactual: planID required")
	}
	if replayer == nil {
		return Report{}, fmt.Errorf("counterfactual: replayer required")
	}
	cfg = normalise(cfg)
	report := Report{
		PlanID:        planID,
		RealisedAlpha: realised,
		Generated:     time.Now().UTC(),
	}
	if len(inputs) == 0 {
		return report, nil
	}

	// Bounded concurrency over inputs. Each call may be
	// expensive; do them in parallel up to MaxConcurrency.
	type result struct {
		index int
		row   Attribution
	}
	resultCh := make(chan result, len(inputs))
	semaphore := make(chan struct{}, cfg.MaxConcurrency)
	var wg sync.WaitGroup

	for i, input := range inputs {
		i, input := i, input
		wg.Add(1)
		go func() {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			callCtx := ctx
			if cfg.PerCallTimeout > 0 {
				var cancel context.CancelFunc
				callCtx, cancel = context.WithTimeout(ctx, cfg.PerCallTimeout)
				defer cancel()
			}

			cf, ok, err := replayer.CounterfactualAlpha(callCtx, planID, input)
			row := Attribution{Input: input, WithAlpha: realised}
			switch {
			case err != nil || !ok:
				row.Confidence = "unavailable"
			default:
				row.WithoutAlpha = cf
				row.Contribution = realised - cf
				row.Confidence = confidenceFor(row.Contribution)
			}
			resultCh <- result{index: i, row: row}
		}()
	}
	wg.Wait()
	close(resultCh)

	rows := make([]Attribution, len(inputs))
	for r := range resultCh {
		rows[r.index] = r.row
	}
	report.Attributions = rows
	for _, r := range rows {
		if r.Confidence != "unavailable" {
			report.NetContribution += r.Contribution
		}
	}
	// Sort by contribution descending for the operator's view.
	sort.SliceStable(report.Attributions, func(i, j int) bool {
		return report.Attributions[i].Contribution > report.Attributions[j].Contribution
	})
	return report, nil
}

func confidenceFor(contribution float64) string {
	abs := contribution
	if abs < 0 {
		abs = -abs
	}
	if abs >= 0.005 {
		return "high"
	}
	return "low"
}

func normalise(c Config) Config {
	d := DefaultConfig()
	if c.MaxConcurrency <= 0 {
		c.MaxConcurrency = d.MaxConcurrency
	}
	if c.PerCallTimeout < 0 {
		c.PerCallTimeout = 0
	}
	return c
}

// NullReplayer returns an "unavailable" answer for every input.
// Useful in tests / pre-wiring builds.
type NullReplayer struct{}

// CounterfactualAlpha implements Replayer.
func (NullReplayer) CounterfactualAlpha(ctx context.Context, planID string, missing Input) (float64, bool, error) {
	return 0, false, nil
}

// HistoricalBaselineReplayer is a simple Replayer backed by a
// caller-supplied lookup of "mean alpha of plans without input".
//
// The wiring layer typically populates the BaselineByInput map
// from a SQL aggregate at startup.
type HistoricalBaselineReplayer struct {
	BaselineByInput map[string]float64 // key = input.Kind + "/" + input.ID
}

// CounterfactualAlpha implements Replayer.
func (h HistoricalBaselineReplayer) CounterfactualAlpha(ctx context.Context, planID string, missing Input) (float64, bool, error) {
	key := string(missing.Kind) + "/" + missing.ID
	v, ok := h.BaselineByInput[key]
	return v, ok, nil
}
