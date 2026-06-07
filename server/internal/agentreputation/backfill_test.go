package agentreputation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

type fakePanelSource struct {
	rows []PanelRow
	err  error
}

func (f *fakePanelSource) ListPanelsForBackfill(_ context.Context, _ string, _ time.Time, _ int) ([]PanelRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

type fakeDebateSource struct {
	rows []DebateRow
	err  error
}

func (f *fakeDebateSource) ListDebatesForBackfill(_ context.Context, _ string, _ time.Time, _ int) ([]DebateRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func staticReturns(realised, benchmark float64, ok bool) RealisedReturnFn {
	return func(_ context.Context, _, _ string, _ time.Time, _ int) (float64, float64, bool, error) {
		return realised, benchmark, ok, nil
	}
}

func TestBackfillConfig_Defaults(t *testing.T) {
	c := BackfillConfig{}
	if got := c.horizons(); len(got) != 2 || got[0] != 1 || got[1] != 5 {
		t.Errorf("default horizons = %v", got)
	}
	if got := c.limit(); got != 500 {
		t.Errorf("default limit = %d", got)
	}
}

// W16-2 audit: AlphaForDirection enforces the direction-aware
// "skill alpha" semantics. Pinned tests so a future refactor that
// silently regresses bearish credit (the original bug) breaks
// CI immediately.
func TestAlphaForDirection_Bullish(t *testing.T) {
	// Long thesis: alpha = realised - benchmark (excess return).
	got := AlphaForDirection(DirBullish, 0.05, 0.01)
	if want := 0.04; absDelta(got, want) > 1e-9 {
		t.Errorf("bullish 5%% sym vs 1%% bench → alpha = %v, want %v", got, want)
	}
	got = AlphaForDirection(DirBullish, -0.02, 0.0)
	if want := -0.02; absDelta(got, want) > 1e-9 {
		t.Errorf("bullish -2%% sym vs flat bench → alpha = %v, want %v (negative credit, the call missed)", got, want)
	}
}

func TestAlphaForDirection_BearishPaysWhenSymbolFalls(t *testing.T) {
	// The original bug: a correct bearish call (sym -5%, bench 0%)
	// used to emit alpha = realised - bench = -5%, and the leaderboard
	// ranked the most-accurate bearish researcher at the bottom. The
	// post-fix value is +5% — the agent's "short sym, long bench"
	// trade printed +5% excess return.
	got := AlphaForDirection(DirBearish, -0.05, 0.0)
	if want := 0.05; absDelta(got, want) > 1e-9 {
		t.Errorf("bearish -5%% sym vs flat bench → alpha = %v, want %v (this used to be -5%% pre-W16-2)", got, want)
	}
	// Bearish call when sym RISES: doubly wrong. -realised - bench.
	got = AlphaForDirection(DirBearish, 0.04, 0.01)
	if want := -0.05; absDelta(got, want) > 1e-9 {
		t.Errorf("bearish +4%% sym vs +1%% bench → alpha = %v, want %v", got, want)
	}
	// Bearish call when sym falls less than bench: still positive
	// (relative outperformance).
	got = AlphaForDirection(DirBearish, -0.01, 0.02)
	if want := -0.01; absDelta(got, want) > 1e-9 {
		t.Errorf("bearish -1%% sym vs +2%% bench → alpha = %v, want %v", got, want)
	}
}

func TestAlphaForDirection_NeutralIsZero(t *testing.T) {
	if got := AlphaForDirection(DirNeutral, 0.05, 0.01); got != 0 {
		t.Errorf("neutral direction must produce alpha=0, got %v", got)
	}
	if got := AlphaForDirection(DirNeutral, -0.10, 0.0); got != 0 {
		t.Errorf("neutral direction must produce alpha=0 even on big realised swings, got %v", got)
	}
	// Unknown / empty direction degrades to zero so a malformed row
	// never poisons avg_alpha.
	if got := AlphaForDirection(Direction("garbage"), 0.5, 0.0); got != 0 {
		t.Errorf("unknown direction must produce alpha=0, got %v", got)
	}
}

// Backfill must persist direction-adjusted alpha. A bearish row
// with a winning realised drop should land with positive Alpha so
// the leaderboard ranks the correct call up, not down.
func TestBackfill_BearishCorrectCallEmitsPositiveAlpha(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	debates := &fakeDebateSource{rows: []DebateRow{{
		ID: "d1", FundID: "f1", Symbol: "AAPL", AsOf: time.Now(),
		Arguments: []DebateArgumentRow{{
			AgentID: "bear_researcher", AgentName: "Bear", Stance: "bear", Round: 1,
			Direction: "bearish", Confidence: 70,
		}},
	}}}
	mock.ExpectBegin()
	mock.ExpectPrepare("INSERT INTO agent_reputation_outcomes")
	// Capture both inserted Alpha values via WithArgs anchors —
	// position 12 in the column list (1-indexed: fund, agent_id,
	// agent_name, agent_kind, category, symbol, asof, direction,
	// confidence, realised, benchmark, alpha, horizon, source_panel,
	// source_debate, note). The args matcher uses sqlmock.AnyArg for
	// everything except the alpha column we want to pin.
	mock.ExpectExec("INSERT INTO agent_reputation_outcomes").
		WithArgs(
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), -0.05, 0.0, 0.05,
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO agent_reputation_stats").WillReturnResult(sqlmock.NewResult(0, 1))

	// realised = -5%, bench = 0% → bearish skill alpha = +5%.
	b := NewBackfill(r, nil, debates, staticReturns(-0.05, 0.0, true))
	n, err := b.Run(context.Background(), "f1", BackfillConfig{Horizons: []int{1}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("want 1 outcome, got %d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func absDelta(a, b float64) float64 {
	d := a - b
	if d < 0 {
		return -d
	}
	return d
}

func TestBackfill_Nil(t *testing.T) {
	var b *Backfill
	if _, err := b.Run(context.Background(), "f1", BackfillConfig{}); err == nil {
		t.Error("expected error from nil backfill")
	}
}

func TestBackfill_MissingFundID(t *testing.T) {
	r, _, done := newMockRepo(t)
	defer done()
	b := NewBackfill(r, nil, nil, staticReturns(0.01, 0.0, true))
	if _, err := b.Run(context.Background(), "", BackfillConfig{}); err == nil {
		t.Error("expected fundID error")
	}
}

func TestBackfill_PanelsHappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	now := time.Now()
	panels := &fakePanelSource{rows: []PanelRow{{
		ID:     "p1",
		FundID: "f1",
		Symbol: "AAPL",
		AsOf:   now,
		Reports: []PanelReportRow{{
			AgentID: "fund_analyst", AgentName: "F", Category: "fundamentals",
			Direction: "bullish", Confidence: 65,
		}},
	}}}
	// 2 horizons -> 2 outcomes; one batched UpsertOutcomes (tx) + RecomputeStats.
	mock.ExpectBegin()
	mock.ExpectPrepare("INSERT INTO agent_reputation_outcomes")
	mock.ExpectExec("INSERT INTO agent_reputation_outcomes").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO agent_reputation_outcomes").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO agent_reputation_stats").WillReturnResult(sqlmock.NewResult(0, 1))

	b := NewBackfill(r, panels, nil, staticReturns(0.02, 0.005, true))
	n, err := b.Run(context.Background(), "f1", BackfillConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("want 2 outcomes, got %d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestBackfill_SkipsWhenReturnsMissing(t *testing.T) {
	r, _, done := newMockRepo(t)
	defer done()
	panels := &fakePanelSource{rows: []PanelRow{{
		ID: "p1", FundID: "f1", Symbol: "AAPL", AsOf: time.Now(),
		Reports: []PanelReportRow{{AgentID: "a", AgentName: "A", Direction: "bullish", Confidence: 50}},
	}}}
	b := NewBackfill(r, panels, nil, staticReturns(0, 0, false))
	n, err := b.Run(context.Background(), "f1", BackfillConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("want 0 outcomes, got %d", n)
	}
}

func TestBackfill_DebatesHappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	debates := &fakeDebateSource{rows: []DebateRow{{
		ID: "d1", FundID: "f1", Symbol: "AAPL", AsOf: time.Now(),
		Arguments: []DebateArgumentRow{{
			AgentID: "bull_researcher", AgentName: "Bull", Stance: "bull", Round: 1,
			Direction: "bullish", Confidence: 70,
		}},
	}}}
	mock.ExpectBegin()
	mock.ExpectPrepare("INSERT INTO agent_reputation_outcomes")
	mock.ExpectExec("INSERT INTO agent_reputation_outcomes").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO agent_reputation_outcomes").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO agent_reputation_stats").WillReturnResult(sqlmock.NewResult(0, 1))

	b := NewBackfill(r, nil, debates, staticReturns(0.03, 0.01, true))
	n, err := b.Run(context.Background(), "f1", BackfillConfig{Horizons: []int{1, 5}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("want 2 outcomes, got %d", n)
	}
}

func TestBackfill_PanelSourceError(t *testing.T) {
	r, _, done := newMockRepo(t)
	defer done()
	panels := &fakePanelSource{err: errors.New("source down")}
	b := NewBackfill(r, panels, nil, staticReturns(0, 0, true))
	if _, err := b.Run(context.Background(), "f1", BackfillConfig{}); err == nil {
		t.Error("expected source error")
	}
}
