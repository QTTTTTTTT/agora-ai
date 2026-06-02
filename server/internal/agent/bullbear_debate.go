// bullbear_debate.go — S8.2 orchestrator for the forced
// Bull-vs-Bear debate.
//
// Runs N rounds where Bull and Bear alternate, each round
// fed:
//   - the four S8.1 analyst reports (constant across rounds), and
//   - the opponent's most recent argument (round >= 2).
//
// The output is a complete DebateTranscript: an ordered list of
// AdvocateArgument rows + a synthesised AggregateVerdict the PM
// can read. Each argument is later projected into a
// workflow.ResearcherOpinion so the existing DebateGraph code
// keeps consuming it without changes.

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// DebateConfig tunes the orchestrator.
type DebateConfig struct {
	// MaxRounds is the upper bound on round count. Each round
	// runs Bull and Bear once (so 3 MaxRounds → 6 arguments).
	// Defaults to 2.
	MaxRounds int

	// PerArgumentTimeout, when > 0, bounds each individual
	// Argue call. Falls through to the parent context otherwise.
	PerArgumentTimeout time.Duration

	// EarlyExitOnAgreement, when true, stops the debate as soon
	// as Bull and Bear converge on the same direction
	// (impossible by construction since they're forced; left
	// here as a future-proof knob). Default false.
	EarlyExitOnAgreement bool
}

// DebateTranscript is the full record produced by Debate.Run.
type DebateTranscript struct {
	Symbol      string
	FundID      string
	AsOf        time.Time
	GeneratedAt time.Time

	// Arguments lists every advocate's argument across all
	// rounds in chronological order: r1-bull, r1-bear, r2-bull,
	// r2-bear, …
	Arguments []AdvocateArgument

	// Verdict is the orchestrator's read of the debate.
	Verdict DebateVerdict
}

// DebateVerdict is the orchestrator's synthesis of the debate.
// It is intentionally narrower than workflow.Verdict so the PM
// can read it directly without the DebateGraph machinery; the
// wiring layer is free to also run BuildDebateGraph on the
// projected opinions when it wants the edge-level detail.
type DebateVerdict struct {
	Direction        Direction
	Confidence       int
	BullConfidence   int
	BearConfidence   int
	Contested        bool
	WinnerStance     AdvocateStance
	WinningSummary   string
	LosingSummary    string
}

// Validate enforces the must-have fields a transcript needs to
// be persisted.
func (t DebateTranscript) Validate() error {
	if strings.TrimSpace(t.Symbol) == "" {
		return errors.New("debate: transcript.Symbol required")
	}
	if len(t.Arguments) == 0 {
		return errors.New("debate: transcript.Arguments empty")
	}
	for i, a := range t.Arguments {
		if err := a.Validate(); err != nil {
			return fmt.Errorf("debate: argument %d invalid: %w", i, err)
		}
	}
	return nil
}

// Debate is the orchestrator. Construct via NewDebate; safe for
// concurrent use after construction.
type Debate struct {
	mu     sync.RWMutex
	bull   *BullResearcher
	bear   *BearResearcher
	cfg    DebateConfig
	logger *slog.Logger
	now    func() time.Time
}

// DebateOption configures a Debate orchestrator.
type DebateOption func(*Debate)

// WithDebateLogger overrides the default logger.
func WithDebateLogger(l *slog.Logger) DebateOption {
	return func(d *Debate) {
		if l != nil {
			d.logger = l
		}
	}
}

// WithDebateClock injects a deterministic clock for tests.
func WithDebateClock(now func() time.Time) DebateOption {
	return func(d *Debate) {
		if now != nil {
			d.now = now
		}
	}
}

// NewDebate builds a Debate orchestrator. Both bull and bear
// are required.
func NewDebate(bull *BullResearcher, bear *BearResearcher, cfg DebateConfig, opts ...DebateOption) *Debate {
	if cfg.MaxRounds <= 0 {
		cfg.MaxRounds = 2
	}
	d := &Debate{
		bull:   bull,
		bear:   bear,
		cfg:    cfg,
		logger: slog.Default(),
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Run drives the debate for one symbol fed by the panel input.
// Both researchers must be non-nil; otherwise an error is
// returned without making any LLM calls.
func (d *Debate) Run(ctx context.Context, fundID string, panel PanelReport, notes string) (DebateTranscript, error) {
	if d.bull == nil || d.bear == nil {
		return DebateTranscript{}, errors.New("debate: both Bull and Bear researchers required")
	}
	if strings.TrimSpace(panel.Symbol) == "" {
		return DebateTranscript{}, errors.New("debate: panel.Symbol required")
	}

	var args []AdvocateArgument
	var bullPrev, bearPrev AdvocateArgument

	for round := 1; round <= d.cfg.MaxRounds; round++ {
		// Bull speaks first each round, with the bear's
		// previous round (if any) as opponent context.
		bullArg, err := d.argueWithTimeout(ctx, d.bull, AdvocateInput{
			Symbol:     panel.Symbol,
			AssetClass: "", // wiring fills if needed
			Market:     "",
			AsOf:       panel.AsOf,
			Round:      round,
			Panel:      panel,
			Opponent:   bearPrev,
			Notes:      notes,
		})
		if err != nil {
			d.logger.Warn("debate: bull round failed", "round", round, "err", err)
			// Bull is allowed to fail one round; if it fails
			// twice in a row the debate has nothing useful to
			// say. We still produce a transcript with the bear
			// arguments collected so far rather than erroring out.
			if round == 1 {
				return DebateTranscript{}, fmt.Errorf("debate: bull round 1 failed: %w", err)
			}
			break
		}
		args = append(args, bullArg)
		bullPrev = bullArg

		bearArg, err := d.argueWithTimeout(ctx, d.bear, AdvocateInput{
			Symbol:   panel.Symbol,
			AsOf:     panel.AsOf,
			Round:    round,
			Panel:    panel,
			Opponent: bullPrev,
			Notes:    notes,
		})
		if err != nil {
			d.logger.Warn("debate: bear round failed", "round", round, "err", err)
			if round == 1 {
				return DebateTranscript{}, fmt.Errorf("debate: bear round 1 failed: %w", err)
			}
			break
		}
		args = append(args, bearArg)
		bearPrev = bearArg

		if d.cfg.EarlyExitOnAgreement && bullPrev.Direction == bearPrev.Direction {
			break
		}
	}

	transcript := DebateTranscript{
		Symbol:      panel.Symbol,
		FundID:      fundID,
		AsOf:        panel.AsOf,
		GeneratedAt: d.now(),
		Arguments:   args,
		Verdict:     synthesiseVerdict(args),
	}
	if err := transcript.Validate(); err != nil {
		return DebateTranscript{}, err
	}
	return transcript, nil
}

// argueWithTimeout wraps an Argue call with the configured
// per-argument timeout.
func (d *Debate) argueWithTimeout(ctx context.Context, a AdvocateAgent, in AdvocateInput) (AdvocateArgument, error) {
	if d.cfg.PerArgumentTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.cfg.PerArgumentTimeout)
		defer cancel()
	}
	return a.Argue(ctx, in)
}

// synthesiseVerdict picks a winner from the debate by summing
// each side's per-round confidence. The latest round counts the
// most: round N is weighted N (so a strong rebuttal in the
// final round can flip the verdict).
//
// We don't reuse the workflow.DebateGraph here because it
// requires a Roundtable + workflow types and would create an
// import cycle. The graph machinery is still used downstream
// when the wiring layer projects these arguments into
// workflow.ResearcherOpinion; this verdict is the lightweight
// "what should the PM read first" answer.
func synthesiseVerdict(args []AdvocateArgument) DebateVerdict {
	bullScore, bearScore := 0, 0
	bullConf, bearConf := 0, 0
	bullCount, bearCount := 0, 0
	var lastBull, lastBear AdvocateArgument
	for _, a := range args {
		w := a.Round
		if w < 1 {
			w = 1
		}
		switch a.Stance {
		case StanceBull:
			bullScore += a.Confidence * w
			bullConf += a.Confidence
			bullCount++
			lastBull = a
		case StanceBear:
			bearScore += a.Confidence * w
			bearConf += a.Confidence
			bearCount++
			lastBear = a
		}
	}
	avgBull := 0
	if bullCount > 0 {
		avgBull = bullConf / bullCount
	}
	avgBear := 0
	if bearCount > 0 {
		avgBear = bearConf / bearCount
	}

	v := DebateVerdict{
		BullConfidence: avgBull,
		BearConfidence: avgBear,
	}

	if bullScore == 0 && bearScore == 0 {
		v.Direction = DirectionNeutral
		v.Confidence = 20
		return v
	}
	totalScore := bullScore + bearScore
	switch {
	case bullScore > bearScore:
		v.Direction = DirectionBullish
		v.WinnerStance = StanceBull
		v.Confidence = clampAdvocateConfidence(int(float64(bullScore) / float64(totalScore) * 100))
		v.WinningSummary = lastBull.Thesis
		v.LosingSummary = lastBear.Thesis
	case bearScore > bullScore:
		v.Direction = DirectionBearish
		v.WinnerStance = StanceBear
		v.Confidence = clampAdvocateConfidence(int(float64(bearScore) / float64(totalScore) * 100))
		v.WinningSummary = lastBear.Thesis
		v.LosingSummary = lastBull.Thesis
	default:
		v.Direction = DirectionNeutral
		v.Confidence = 40
		v.Contested = true
		v.WinningSummary = lastBull.Thesis + " // " + lastBear.Thesis
		return v
	}

	// Contested when the margin between bull and bear scores is
	// less than 20% of the larger side. This signals to the PM
	// that the verdict is close.
	larger := bullScore
	if bearScore > larger {
		larger = bearScore
	}
	margin := bullScore - bearScore
	if margin < 0 {
		margin = -margin
	}
	if larger > 0 && float64(margin)/float64(larger) < 0.20 {
		v.Contested = true
	}
	return v
}
