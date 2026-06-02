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
