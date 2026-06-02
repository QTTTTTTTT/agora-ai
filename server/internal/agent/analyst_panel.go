// analyst_panel.go — S8.1 AnalystPanel.
//
// The four AnalystAgents are fan-out workers; AnalystPanel is the
// fan-in coordinator that drives them in parallel and assembles
// one PanelReport per symbol. This is the artefact the new
// Bull/Bear researchers (S8.2) and the existing PMAgent consume.
//
// Why a separate type instead of one big constructor + free
// function? Persisting the panel mapping (fund_id → 4 agents) in
// a single struct lets the wiring layer build it once at boot,
// run AsOf-by-AsOf in a loop, and not re-resolve agents per call.

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// PanelReport bundles the four per-category reports plus the
// blended aggregate the panel computed. The Bull/Bear and PM
// downstream can either work off the aggregate or unpick the
// per-category reports.
type PanelReport struct {
	Symbol      string
	FundID      string
	AsOf        time.Time
	GeneratedAt time.Time

	Reports map[AnalystCategory]AnalystReport

	// Aggregate is the panel-level direction + confidence,
	// computed as the conviction-weighted average vote of the
	// four analysts. It is *not* a substitute for the per-
	// category reports — it's a one-liner the PM can quote.
	Aggregate AggregateView
}

// AggregateView is the blended verdict across analysts.
type AggregateView struct {
	Direction  Direction
	Confidence int

	// CategoriesVoted is the count of categories that returned
	// a non-neutral direction. A panel where 4/4 categories
	// vote bullish is much higher conviction than 1/4.
	CategoriesVoted int

	// PerCategoryVotes is the {-1, 0, +1} vote per category,
	// keyed for UI rendering.
	PerCategoryVotes map[AnalystCategory]int
}

// Validate enforces the must-have fields a panel report needs to
// be persisted.
func (p PanelReport) Validate() error {
	if strings.TrimSpace(p.Symbol) == "" {
		return errors.New("panel: PanelReport.Symbol required")
	}
	if len(p.Reports) == 0 {
		return errors.New("panel: PanelReport.Reports empty")
	}
	for cat, r := range p.Reports {
		if r.Category != cat {
			return fmt.Errorf("panel: report keyed under %q has category %q", cat, r.Category)
		}
		if err := r.Validate(); err != nil {
			return fmt.Errorf("panel: report %q invalid: %w", cat, err)
		}
	}
	return nil
}

// AnalystPanel runs the four analyst roles for a single fund.
// Concurrency: each panel is safe to share across goroutines, but
// individual analysts must be Start()'d before use.
type AnalystPanel struct {
	mu        sync.RWMutex
	fundID    string
	logger    *slog.Logger
	now       func() time.Time
	parallel  bool
	analysts  map[AnalystCategory]AnalystAgent
	timeout   time.Duration
}

// PanelOption configures an AnalystPanel.
type PanelOption func(*AnalystPanel)

// WithPanelLogger overrides the panel's logger.
func WithPanelLogger(l *slog.Logger) PanelOption {
	return func(p *AnalystPanel) {
		if l != nil {
			p.logger = l
		}
	}
}

// WithPanelClock injects a deterministic clock.
func WithPanelClock(now func() time.Time) PanelOption {
	return func(p *AnalystPanel) {
		if now != nil {
			p.now = now
		}
	}
}

// WithPanelTimeout sets a per-analyst timeout. Zero means no
// per-analyst timeout (parent context still applies).
func WithPanelTimeout(d time.Duration) PanelOption {
	return func(p *AnalystPanel) {
		if d > 0 {
			p.timeout = d
		}
	}
}

// WithPanelSerial forces serial execution. The default is
// parallel; serial mode is useful for deterministic tests.
func WithPanelSerial() PanelOption {
	return func(p *AnalystPanel) { p.parallel = false }
}

// NewAnalystPanel builds a panel with the given analyst set.
// Categories with no analyst configured are silently skipped at
// runtime — operators can deploy a 3-of-4 panel during rollout.
func NewAnalystPanel(fundID string, analysts []AnalystAgent, opts ...PanelOption) *AnalystPanel {
	p := &AnalystPanel{
		fundID:   fundID,
		logger:   slog.Default(),
		now:      time.Now,
		parallel: true,
		analysts: make(map[AnalystCategory]AnalystAgent, len(analysts)),
	}
	for _, a := range analysts {
		if a == nil {
			continue
		}
		p.analysts[a.Category()] = a
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// FundID returns the panel's fund identifier.
func (p *AnalystPanel) FundID() string { return p.fundID }

// Categories lists the categories the panel can serve, in stable
// order.
func (p *AnalystPanel) Categories() []AnalystCategory {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]AnalystCategory, 0, len(p.analysts))
	for c := range p.analysts {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// RunSymbol fans out input to all configured analysts and assembles
// a PanelReport. A failing analyst does not abort the panel —
// the panel surfaces the successful reports + logs the failures.
// An empty panel returns an error.
func (p *AnalystPanel) RunSymbol(ctx context.Context, input AnalystInput) (PanelReport, error) {
	if strings.TrimSpace(input.Symbol) == "" {
		return PanelReport{}, errors.New("panel: input.Symbol required")
	}
	p.mu.RLock()
	analysts := make([]AnalystAgent, 0, len(p.analysts))
	for _, a := range p.analysts {
		analysts = append(analysts, a)
	}
	p.mu.RUnlock()
	if len(analysts) == 0 {
		return PanelReport{}, errors.New("panel: no analysts configured")
	}

	reports := make(map[AnalystCategory]AnalystReport, len(analysts))
	if p.parallel {
		reports = p.runParallel(ctx, analysts, input)
	} else {
		reports = p.runSerial(ctx, analysts, input)
	}
	if len(reports) == 0 {
		return PanelReport{}, errors.New("panel: every analyst failed")
	}

	out := PanelReport{
		Symbol:      input.Symbol,
		FundID:      p.fundID,
		AsOf:        input.AsOf,
		GeneratedAt: p.now(),
		Reports:     reports,
		Aggregate:   aggregateReports(reports),
	}
	if err := out.Validate(); err != nil {
		return PanelReport{}, err
	}
	return out, nil
}

func (p *AnalystPanel) runSerial(ctx context.Context, analysts []AnalystAgent, input AnalystInput) map[AnalystCategory]AnalystReport {
	out := make(map[AnalystCategory]AnalystReport, len(analysts))
	for _, a := range analysts {
		rep, err := p.runOne(ctx, a, input)
		if err != nil {
			p.logger.Warn("panel: analyst failed",
				"category", a.Category(), "symbol", input.Symbol, "err", err)
			continue
		}
		out[a.Category()] = rep
	}
	return out
}

func (p *AnalystPanel) runParallel(ctx context.Context, analysts []AnalystAgent, input AnalystInput) map[AnalystCategory]AnalystReport {
	type result struct {
		cat AnalystCategory
		rep AnalystReport
		err error
	}
	resCh := make(chan result, len(analysts))
	var wg sync.WaitGroup
	for _, a := range analysts {
		wg.Add(1)
		go func(a AnalystAgent) {
			defer wg.Done()
			rep, err := p.runOne(ctx, a, input)
			resCh <- result{cat: a.Category(), rep: rep, err: err}
		}(a)
	}
	wg.Wait()
	close(resCh)

	out := make(map[AnalystCategory]AnalystReport, len(analysts))
	for r := range resCh {
		if r.err != nil {
			p.logger.Warn("panel: analyst failed",
				"category", r.cat, "symbol", input.Symbol, "err", r.err)
			continue
		}
		out[r.cat] = r.rep
	}
	return out
}

func (p *AnalystPanel) runOne(ctx context.Context, a AnalystAgent, input AnalystInput) (AnalystReport, error) {
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	return a.Analyze(ctx, input)
}

// aggregateReports blends per-category reports into a single
// directional vote. Vote weight is the analyst's stated
// confidence (0–100): a high-conviction bullish call counts more
// than a low-conviction one. Direction is then thresholded:
//
//	weighted_sum / weight_total ≥ +0.2 → bullish
//	weighted_sum / weight_total ≤ -0.2 → bearish
//	otherwise                           → neutral
//
// Aggregate confidence is the unweighted average of the
// participating analysts' confidence, scaled by how many
// categories actually voted (a 4-of-4 panel boosts conviction
// vs. a 1-of-4 panel).
func aggregateReports(reports map[AnalystCategory]AnalystReport) AggregateView {
	if len(reports) == 0 {
		return AggregateView{Direction: DirectionNeutral, Confidence: 20, PerCategoryVotes: map[AnalystCategory]int{}}
	}
	votes := make(map[AnalystCategory]int, len(reports))
	weightedScore := 0.0
	totalWeight := 0.0
	categoriesVoted := 0
	confSum := 0
	for cat, r := range reports {
		v := 0
		switch r.Direction {
		case DirectionBullish:
			v = 1
		case DirectionBearish:
			v = -1
		}
		votes[cat] = v
		if v != 0 {
			categoriesVoted++
		}
		weightedScore += float64(v) * float64(r.Confidence)
		totalWeight += float64(r.Confidence)
		confSum += r.Confidence
	}
	dir := DirectionNeutral
	if totalWeight > 0 {
		ratio := weightedScore / totalWeight
		switch {
		case ratio >= 0.2:
			dir = DirectionBullish
		case ratio <= -0.2:
			dir = DirectionBearish
		}
	}
	avgConf := confSum / len(reports)
	// Conviction boost: if 4/4 voted (and same direction), nudge
	// confidence up by 10. If 1/4 voted, dampen by 10.
	switch categoriesVoted {
	case 4:
		avgConf += 10
	case 1:
		avgConf -= 10
	}
	return AggregateView{
		Direction:        dir,
		Confidence:       clampConfidence(avgConf),
		CategoriesVoted:  categoriesVoted,
		PerCategoryVotes: votes,
	}
}
