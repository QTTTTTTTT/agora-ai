// master_panel.go — fan-out runner for N MasterAgents.
//
// Mirrors AnalystPanel's pattern: take a set of agents, run them
// in parallel, aggregate to a single panel verdict. The key
// differences:
//
//   * MasterAgents are keyed by their persona key (string) rather
//     than a fixed AnalystCategory enum. A panel can carry any
//     subset of the 10 personas (so PersonaPreset → list of keys
//     → instantiated panel).
//
//   * Vote aggregation is verdict-weighted: STRONG_BUY=+2, BUY=+1,
//     HOLD=0, AVOID=-1, SHORT=-2. We then collapse to one of
//     STRONG_BUY/BUY/HOLD/AVOID/SHORT/MIXED based on weighted
//     average × confidence.
//
//   * Consensus score (0..100) measures inter-master agreement;
//     low consensus → the UI renders a yellow "市场不明朗" badge
//     rather than a confident verdict.

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// MasterPanelReport bundles every per-master report plus the
// blended aggregate for one consultation.
type MasterPanelReport struct {
	Symbol      string
	AsOf        time.Time
	GeneratedAt time.Time
	Reports     []MasterReport
	Aggregate   MasterAggregateView
}

// MasterAggregateView is the panel-level verdict.
type MasterAggregateView struct {
	Verdict       string  // STRONG_BUY / BUY / HOLD / AVOID / SHORT / MIXED
	Confidence    int     // 0..100
	Consensus     float64 // 0..100 ("100" = all masters agree)
	MasterCount   int
	BuyCount      int
	HoldCount     int
	AvoidCount    int
	StrongBuyCount int
	ShortCount    int
}

// Validate enforces panel invariants before persistence.
func (p MasterPanelReport) Validate() error {
	if strings.TrimSpace(p.Symbol) == "" {
		return errors.New("master_panel: Symbol required")
	}
	if len(p.Reports) == 0 {
		return errors.New("master_panel: no reports")
	}
	for i, r := range p.Reports {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("master_panel: report[%d]: %w", i, err)
		}
	}
	return nil
}

// MasterPanel is a sharable collection of MasterAgents that runs
// them in parallel against a single MasterInput.
type MasterPanel struct {
	mu       sync.RWMutex
	logger   *slog.Logger
	now      func() time.Time
	parallel bool
	timeout  time.Duration
	agents   map[string]*MasterAgent
}

// MasterPanelOption configures construction.
type MasterPanelOption func(*MasterPanel)

// WithMasterPanelLogger overrides the logger.
func WithMasterPanelLogger(l *slog.Logger) MasterPanelOption {
	return func(p *MasterPanel) {
		if l != nil {
			p.logger = l
		}
	}
}

// WithMasterPanelClock injects a deterministic clock.
func WithMasterPanelClock(now func() time.Time) MasterPanelOption {
	return func(p *MasterPanel) {
		if now != nil {
			p.now = now
		}
	}
}

// WithMasterPanelTimeout sets a per-agent timeout. Zero means
// "no override, use the parent context only".
func WithMasterPanelTimeout(d time.Duration) MasterPanelOption {
	return func(p *MasterPanel) {
		if d > 0 {
			p.timeout = d
		}
	}
}

// WithMasterPanelSerial forces sequential execution. Default is
// parallel; serial is useful for deterministic tests.
func WithMasterPanelSerial() MasterPanelOption {
	return func(p *MasterPanel) { p.parallel = false }
}

// NewMasterPanel constructs a panel around an explicit list of
// agents. Duplicate keys are quietly de-duplicated (last wins) so
// PersonaPreset.MasterKeys + a custom override don't double-vote.
func NewMasterPanel(agents []*MasterAgent, opts ...MasterPanelOption) *MasterPanel {
	p := &MasterPanel{
		logger:   slog.Default(),
		now:      time.Now,
		parallel: true,
		agents:   make(map[string]*MasterAgent, len(agents)),
	}
	for _, a := range agents {
		if a == nil {
			continue
		}
		p.agents[a.Key()] = a
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Keys returns the agent keys in stable sorted order. Useful for
// the UI's "voted by" pill row.
func (p *MasterPanel) Keys() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.agents))
	for k := range p.agents {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Size returns the number of agents in the panel.
func (p *MasterPanel) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.agents)
}

// Run drives every agent against the same input and assembles a
// MasterPanelReport. Agents that error return a fallback report
// (their Analyze contract handles this internally), so Run never
// surfaces partial panels.
func (p *MasterPanel) Run(ctx context.Context, in MasterInput) (MasterPanelReport, error) {
	if strings.TrimSpace(in.Symbol) == "" {
		return MasterPanelReport{}, errors.New("master_panel: input.Symbol required")
	}
	p.mu.RLock()
	agents := make([]*MasterAgent, 0, len(p.agents))
	for _, a := range p.agents {
		agents = append(agents, a)
	}
	p.mu.RUnlock()
	if len(agents) == 0 {
		return MasterPanelReport{}, errors.New("master_panel: no agents configured")
	}

	var reports []MasterReport
	if p.parallel {
		reports = p.runParallel(ctx, agents, in)
	} else {
		reports = p.runSerial(ctx, agents, in)
	}
	if len(reports) == 0 {
		return MasterPanelReport{}, errors.New("master_panel: every agent failed")
	}
	// Stable order for the UI: alphabetical by master key.
	sort.Slice(reports, func(i, j int) bool {
		return reports[i].MasterKey < reports[j].MasterKey
	})
	out := MasterPanelReport{
		Symbol:      strings.ToUpper(strings.TrimSpace(in.Symbol)),
		AsOf:        in.AsOf,
		GeneratedAt: p.now(),
		Reports:     reports,
		Aggregate:   AggregateMasterReports(reports),
	}
	if err := out.Validate(); err != nil {
		return MasterPanelReport{}, err
	}
	return out, nil
}

func (p *MasterPanel) runSerial(ctx context.Context, agents []*MasterAgent, in MasterInput) []MasterReport {
	out := make([]MasterReport, 0, len(agents))
	for _, a := range agents {
		rep, err := p.runOne(ctx, a, in)
		if err != nil {
			p.logger.Warn("master_panel: agent failed",
				"master", a.Key(), "symbol", in.Symbol, "err", err)
			continue
		}
		out = append(out, rep)
	}
	return out
}

func (p *MasterPanel) runParallel(ctx context.Context, agents []*MasterAgent, in MasterInput) []MasterReport {
	type result struct {
		rep MasterReport
		err error
	}
	resCh := make(chan result, len(agents))
	var wg sync.WaitGroup
	for _, a := range agents {
		wg.Add(1)
		go func(a *MasterAgent) {
			defer wg.Done()
			rep, err := p.runOne(ctx, a, in)
			resCh <- result{rep: rep, err: err}
		}(a)
	}
	wg.Wait()
	close(resCh)

	out := make([]MasterReport, 0, len(agents))
	for r := range resCh {
		if r.err != nil {
			p.logger.Warn("master_panel: agent failed",
				"symbol", in.Symbol, "err", r.err)
			continue
		}
		out = append(out, r.rep)
	}
	return out
}

func (p *MasterPanel) runOne(ctx context.Context, a *MasterAgent, in MasterInput) (MasterReport, error) {
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	// Derive a persona-scoped MasterInput: each persona declares
	// its own must_have_criteria, so the deterministic rule_prior
	// block must be recomputed per agent. The base FundamentalsBlock
	// (snapshot + history) is shared; only the RulePrior pointer
	// differs per call. Cheap to compute (no I/O) so we always do
	// it rather than caching.
	scoped := in
	if in.Fundamentals != nil {
		fb := *in.Fundamentals
		fb.RulePrior = BuildMasterRulePrior(a.Persona(), &fb)
		scoped.Fundamentals = &fb
	}
	return a.Analyze(ctx, scoped)
}

// ---------------------------------------------------------------------------
// Aggregation
// ---------------------------------------------------------------------------

// AggregateMasterReports collapses N MasterReports into a single
// verdict + a 0..100 consensus score. Exposed so the wiring layer
// can recompute the aggregate after manual edits (e.g. when a UI
// lets the user re-weight masters).
func AggregateMasterReports(reports []MasterReport) MasterAggregateView {
	if len(reports) == 0 {
		return MasterAggregateView{Verdict: "HOLD", Confidence: 20}
	}
	weightedSum := 0.0
	totalWeight := 0.0
	confSum := 0
	view := MasterAggregateView{MasterCount: len(reports)}
	scores := make([]float64, 0, len(reports))

	for _, r := range reports {
		s := verdictScore(r.Verdict)
		w := float64(r.Confidence)
		if w <= 0 {
			w = 20 // floor — never give a zero-confidence vote zero weight
		}
		weightedSum += s * w
		totalWeight += w
		confSum += r.Confidence
		scores = append(scores, s)
		switch strings.ToUpper(r.Verdict) {
		case "STRONG_BUY":
			view.StrongBuyCount++
			view.BuyCount++ // STRONG_BUY counts as a Buy too for the rollup
		case "BUY":
			view.BuyCount++
		case "HOLD":
			view.HoldCount++
		case "AVOID", "PASS", "SKIP":
			view.AvoidCount++
		case "SHORT":
			view.ShortCount++
		}
	}

	avgConf := confSum / len(reports)
	avgScore := 0.0
	if totalWeight > 0 {
		avgScore = weightedSum / totalWeight
	}
	view.Verdict = collapseAggregate(avgScore, scores)
	view.Confidence = clampConfidence(avgConf)
	view.Consensus = computeConsensus(scores)
	return view
}

// verdictScore maps a verbal verdict onto a numeric line so we
// can compute a weighted mean.
func verdictScore(v string) float64 {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "STRONG_BUY":
		return 2.0
	case "BUY":
		return 1.0
	case "HOLD":
		return 0.0
	case "AVOID", "PASS", "SKIP":
		return -1.0
	case "SHORT":
		return -2.0
	default:
		return 0.0
	}
}

// collapseAggregate maps a weighted-average score back into a
// verdict. When the per-master scores spread across both buy and
// sell sides we report MIXED so the UI can flag it.
func collapseAggregate(avg float64, scores []float64) string {
	hasBuy := false
	hasSell := false
	for _, s := range scores {
		if s > 0 {
			hasBuy = true
		}
		if s < 0 {
			hasSell = true
		}
	}
	if hasBuy && hasSell {
		// Split decision: still emit a verdict but the
		// caller should look at Consensus to know it's noisy.
	}
	switch {
	case avg >= 1.5:
		return "STRONG_BUY"
	case avg >= 0.5:
		return "BUY"
	case avg <= -1.5:
		return "SHORT"
	case avg <= -0.5:
		return "AVOID"
	}
	// avg in (-0.5, 0.5): HOLD by default, MIXED if both sides voted.
	if hasBuy && hasSell {
		return "MIXED"
	}
	return "HOLD"
}

// computeConsensus returns a 0..100 score measuring agreement
// across master scores. We use 1 - (stddev / max_possible_stddev)
// where max_possible_stddev is 2 (the range from -2 to +2 split
// half-half). Higher = more agreement.
func computeConsensus(scores []float64) float64 {
	if len(scores) <= 1 {
		return 100.0
	}
	mean := 0.0
	for _, s := range scores {
		mean += s
	}
	mean /= float64(len(scores))
	variance := 0.0
	for _, s := range scores {
		d := s - mean
		variance += d * d
	}
	variance /= float64(len(scores))
	stddev := math.Sqrt(variance)
	// Max possible stddev for a range of [-2, 2] is 2.
	const maxStddev = 2.0
	ratio := stddev / maxStddev
	if ratio > 1 {
		ratio = 1
	}
	return math.Round((1 - ratio) * 100.0)
}
