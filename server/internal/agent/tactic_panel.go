// tactic_panel.go — fan-out runner for A-share short-term tactics.
// Mirrors master_panel.go: parallel execution by default, alphabetic
// stable order for the UI, and an aggregate that computes a consensus
// score across the per-tactic verdicts.
//
// The aggregate verdict semantics differ from masters:
//   * BUY (any flavour: BUY / BUY_DIP / BUY_TAIL / CHASE_LIMIT_UP) counts +1
//   * WAIT_FOR_WINDOW or WAIT_FOR_CONFIRMATION counts 0
//   * SKIP counts -1
// Weighted by confidence, then collapsed via thresholds:
//   * >=0.5  → BUY  (highest BUY-flavour from the contributing reports)
//   * >=0.0  → MIXED
//   * <0     → SKIP

package agent

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// TacticPanel is the fan-out runner. Construct via NewTacticPanel;
// safe to share across goroutines.
type TacticPanel struct {
	agents   []*TacticAgent
	parallel bool
	timeout  time.Duration
	logger   *slog.Logger
	now      func() time.Time
	mu       sync.RWMutex
}

// TacticPanelOption configures construction.
type TacticPanelOption func(*TacticPanel)

// WithTacticPanelTimeout caps per-agent execution. 0 disables.
func WithTacticPanelTimeout(d time.Duration) TacticPanelOption {
	return func(p *TacticPanel) {
		if d > 0 {
			p.timeout = d
		}
	}
}

// WithTacticPanelSerial disables parallel execution (used by tests).
func WithTacticPanelSerial() TacticPanelOption {
	return func(p *TacticPanel) { p.parallel = false }
}

// WithTacticPanelLogger swaps the default logger.
func WithTacticPanelLogger(l *slog.Logger) TacticPanelOption {
	return func(p *TacticPanel) {
		if l != nil {
			p.logger = l
		}
	}
}

// NewTacticPanel constructs a panel that fans out to the supplied
// agents. Agents are evaluated in alphabetical order of key for
// stable rendering.
func NewTacticPanel(agents []*TacticAgent, opts ...TacticPanelOption) *TacticPanel {
	p := &TacticPanel{
		agents:   append([]*TacticAgent(nil), agents...),
		parallel: true,
		logger:   slog.Default(),
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Keys returns the keys of the underlying agents in the panel's
// natural order. Useful for the admin probe + logs.
func (p *TacticPanel) Keys() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.agents))
	for _, a := range p.agents {
		out = append(out, a.Key())
	}
	return out
}

// Size returns the number of agents.
func (p *TacticPanel) Size() int {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.agents)
}

// Run dispatches the input to every agent and aggregates.
func (p *TacticPanel) Run(ctx context.Context, in TacticInput) (TacticPanelReport, error) {
	if strings.TrimSpace(in.Symbol) == "" {
		return TacticPanelReport{}, errors.New("tactic_panel: input.Symbol required")
	}
	p.mu.RLock()
	agents := append([]*TacticAgent(nil), p.agents...)
	p.mu.RUnlock()
	if len(agents) == 0 {
		return TacticPanelReport{}, ErrTacticNotReady
	}

	var reports []TacticReport
	if p.parallel {
		reports = p.runParallel(ctx, agents, in)
	} else {
		reports = p.runSerial(ctx, agents, in)
	}
	if len(reports) == 0 {
		return TacticPanelReport{}, errors.New("tactic_panel: every agent failed")
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].TacticKey < reports[j].TacticKey })

	out := TacticPanelReport{
		Symbol:      strings.ToUpper(strings.TrimSpace(in.Symbol)),
		AsOf:        in.AsOf,
		GeneratedAt: p.now(),
		Reports:     reports,
		Aggregate:   AggregateTacticReports(reports),
	}
	return out, nil
}

func (p *TacticPanel) runSerial(ctx context.Context, agents []*TacticAgent, in TacticInput) []TacticReport {
	out := make([]TacticReport, 0, len(agents))
	for _, a := range agents {
		rep, err := p.runOne(ctx, a, in)
		if err != nil {
			p.logger.Warn("tactic_panel: agent failed",
				"tactic", a.Key(), "symbol", in.Symbol, "err", err)
			continue
		}
		out = append(out, rep)
	}
	return out
}

func (p *TacticPanel) runParallel(ctx context.Context, agents []*TacticAgent, in TacticInput) []TacticReport {
	type result struct {
		rep TacticReport
		err error
	}
	resCh := make(chan result, len(agents))
	var wg sync.WaitGroup
	for _, a := range agents {
		wg.Add(1)
		go func(a *TacticAgent) {
			defer wg.Done()
			rep, err := p.runOne(ctx, a, in)
			resCh <- result{rep: rep, err: err}
		}(a)
	}
	wg.Wait()
	close(resCh)

	out := make([]TacticReport, 0, len(agents))
	for r := range resCh {
		if r.err != nil {
			p.logger.Warn("tactic_panel: agent failed",
				"symbol", in.Symbol, "err", r.err)
			continue
		}
		out = append(out, r.rep)
	}
	return out
}

func (p *TacticPanel) runOne(ctx context.Context, a *TacticAgent, in TacticInput) (TacticReport, error) {
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	return a.Analyze(ctx, in)
}

// ---------------------------------------------------------------------------
// Aggregation
// ---------------------------------------------------------------------------

// AggregateTacticReports collapses N TacticReports into a single
// verdict + confidence + consensus score.
func AggregateTacticReports(reports []TacticReport) TacticAggregateView {
	if len(reports) == 0 {
		return TacticAggregateView{Verdict: "SKIP", Confidence: 20}
	}
	weightedSum := 0.0
	totalWeight := 0.0
	confSum := 0
	view := TacticAggregateView{}
	scores := make([]float64, 0, len(reports))
	var firstBuyVerdict string

	for _, r := range reports {
		s := tacticVerdictScore(r.Verdict)
		w := float64(r.Confidence)
		if w <= 0 {
			w = 20
		}
		weightedSum += s * w
		totalWeight += w
		confSum += r.Confidence
		scores = append(scores, s)
		up := strings.ToUpper(r.Verdict)
		switch {
		case strings.HasPrefix(up, "BUY") || strings.HasPrefix(up, "CHASE"):
			view.BuyCount++
			if firstBuyVerdict == "" {
				firstBuyVerdict = up
			}
		case strings.HasPrefix(up, "WAIT"):
			view.WaitCount++
		default:
			view.SkipCount++
		}
	}
	avg := weightedSum / totalWeight
	view.Confidence = clampConfidence(confSum / len(reports))
	view.Consensus = computeConsensusScore(scores)

	switch {
	case avg >= 0.5 && firstBuyVerdict != "":
		view.Verdict = firstBuyVerdict
	case avg >= 0:
		view.Verdict = "MIXED"
	default:
		view.Verdict = "SKIP"
	}
	return view
}

func tacticVerdictScore(v string) float64 {
	up := strings.ToUpper(strings.TrimSpace(v))
	switch {
	case strings.HasPrefix(up, "BUY") || strings.HasPrefix(up, "CHASE"):
		return 1
	case strings.HasPrefix(up, "WAIT"):
		return 0
	default:
		return -1
	}
}

// computeConsensusScore returns a 0-100 score where 100 means every
// agent agrees and 0 means equal split BUY / SKIP. Mirrors the
// master panel's consensus formula.
func computeConsensusScore(scores []float64) float64 {
	if len(scores) <= 1 {
		return 100
	}
	var mean float64
	for _, s := range scores {
		mean += s
	}
	mean /= float64(len(scores))
	var variance float64
	for _, s := range scores {
		variance += (s - mean) * (s - mean)
	}
	variance /= float64(len(scores))
	std := math.Sqrt(variance)
	// std=0 → 100; std=1 → 0.
	return clampUnit(1.0-std) * 100
}
