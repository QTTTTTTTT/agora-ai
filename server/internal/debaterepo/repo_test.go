package debaterepo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/agent"
)

func newMockRepo(t *testing.T) (*Repo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewRepo(db), mock, func() { _ = db.Close() }
}

func makeValidTranscript(symbol, fundID string, asof time.Time) agent.DebateTranscript {
	return agent.DebateTranscript{
		Symbol:      symbol,
		FundID:      fundID,
		AsOf:        asof,
		GeneratedAt: asof,
		Arguments: []agent.AdvocateArgument{
			{
				AgentID: "bull-1", AgentName: "Bull", Stance: agent.StanceBull,
				Symbol: symbol, Round: 1, Direction: agent.DirectionBullish, Confidence: 70,
				Thesis: "bull r1", SupportPoints: []string{"s1"}, Rebuttals: nil,
				CitedReports: []agent.AnalystCategory{agent.CategoryFundamentals},
				AsOf:         asof, GeneratedAt: asof, LLMModel: "fallback",
			},
			{
				AgentID: "bear-1", AgentName: "Bear", Stance: agent.StanceBear,
				Symbol: symbol, Round: 1, Direction: agent.DirectionBearish, Confidence: 60,
				Thesis: "bear r1", SupportPoints: []string{"r1"}, Rebuttals: nil,
				CitedReports: []agent.AnalystCategory{agent.CategoryNews},
				AsOf:         asof, GeneratedAt: asof, LLMModel: "fallback",
			},
		},
		Verdict: agent.DebateVerdict{
			Direction: agent.DirectionBullish, Confidence: 60, WinnerStance: agent.StanceBull,
			BullConfidence: 70, BearConfidence: 60,
			WinningSummary: "bull r1", LosingSummary: "bear r1",
		},
	}
}

func TestSaveTranscript_HappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	asof := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	tx := makeValidTranscript("AAPL", "f-1", asof)

	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO debate_transcripts").
		WithArgs("f-1", "panel-1", "AAPL", asof, asof,
			"bullish", 60, "bull",
			70, 60, false, "bull r1", "bear r1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("trans-uuid"))
	mock.ExpectPrepare("INSERT INTO debate_arguments")
	for i := 0; i < 2; i++ {
		mock.ExpectExec("INSERT INTO debate_arguments").
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectCommit()

	id, err := r.SaveTranscript(context.Background(), "panel-1", tx)
	if err != nil {
		t.Fatal(err)
	}
	if id != "trans-uuid" {
		t.Errorf("id = %q", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestSaveTranscript_RollsBackOnInsertError(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	asof := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	tx := makeValidTranscript("AAPL", "f-1", asof)

	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO debate_transcripts").WillReturnError(errors.New("db down"))
	mock.ExpectRollback()

	if _, err := r.SaveTranscript(context.Background(), "panel-1", tx); err == nil {
		t.Error("expected error")
	}
}

func TestSaveTranscript_RejectsBadInput(t *testing.T) {
	r, _, done := newMockRepo(t)
	defer done()
	asof := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	if _, err := r.SaveTranscript(context.Background(), "", makeValidTranscript("X", "f", asof)); err == nil {
		t.Error("expected error when panelID empty")
	}
	if _, err := r.SaveTranscript(context.Background(), "p-1", agent.DebateTranscript{}); err == nil {
		t.Error("expected error for invalid transcript")
	}
}

func TestListTranscripts_Filters(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	asof := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM debate_transcripts").
		WithArgs("f-1", "AAPL", 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "panel_id", "symbol", "asof", "generated_at",
			"verdict_direction", "verdict_confidence", "verdict_winner",
			"verdict_bull_confidence", "verdict_bear_confidence",
			"verdict_contested", "verdict_winning_summary", "verdict_losing_summary",
			"created_at",
		}).AddRow("t-1", "f-1", "p-1", "AAPL", asof, asof,
			"bullish", 60, "bull", 70, 60, false, "ws", "ls", asof))
	got, err := r.ListTranscripts(context.Background(), ListTranscriptsParams{
		FundID: "f-1", Symbol: "aapl",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].VerdictDirection != "bullish" {
		t.Errorf("got = %+v", got)
	}
}

func TestGetTranscript_NotFound(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	mock.ExpectQuery("FROM debate_transcripts\\s*WHERE id").
		WithArgs("nope").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "panel_id", "symbol", "asof", "generated_at",
			"verdict_direction", "verdict_confidence", "verdict_winner",
			"verdict_bull_confidence", "verdict_bear_confidence",
			"verdict_contested", "verdict_winning_summary", "verdict_losing_summary",
			"created_at",
		}))
	if _, err := r.GetTranscript(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGetTranscript_WithChildren(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	asof := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM debate_transcripts\\s*WHERE id").
		WithArgs("t-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "panel_id", "symbol", "asof", "generated_at",
			"verdict_direction", "verdict_confidence", "verdict_winner",
			"verdict_bull_confidence", "verdict_bear_confidence",
			"verdict_contested", "verdict_winning_summary", "verdict_losing_summary",
			"created_at",
		}).AddRow("t-1", "f-1", "p-1", "AAPL", asof, asof,
			"bullish", 60, "bull", 70, 60, false, "ws", "ls", asof))
	mock.ExpectQuery("FROM debate_arguments\\s*WHERE transcript_id IN").
		WithArgs("t-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "transcript_id", "fund_id", "agent_id", "agent_name", "stance",
			"symbol", "round_number", "asof", "generated_at",
			"direction", "confidence", "thesis",
			"support_points", "rebuttals", "cited_reports", "llm_model", "created_at",
		}).AddRow("a-1", "t-1", "f-1", "bull-1", "Bull", "bull",
			"AAPL", 1, asof, asof,
			"bullish", 70, "bull thesis",
			[]byte(`["s1"]`), []byte(`[]`), []byte(`["fundamentals"]`), "fallback", asof))
	got, err := r.GetTranscript(context.Background(), "t-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Arguments) != 1 || got.Arguments[0].Thesis != "bull thesis" {
		t.Errorf("got = %+v", got)
	}
}

func TestGetTranscriptByPanel(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	asof := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM debate_transcripts\\s*WHERE panel_id").
		WithArgs("p-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "panel_id", "symbol", "asof", "generated_at",
			"verdict_direction", "verdict_confidence", "verdict_winner",
			"verdict_bull_confidence", "verdict_bear_confidence",
			"verdict_contested", "verdict_winning_summary", "verdict_losing_summary",
			"created_at",
		}).AddRow("t-1", "f-1", "p-1", "AAPL", asof, asof,
			"bullish", 60, "bull", 70, 60, false, "ws", "ls", asof))
	mock.ExpectQuery("FROM debate_arguments").
		WithArgs("t-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "transcript_id", "fund_id", "agent_id", "agent_name", "stance",
			"symbol", "round_number", "asof", "generated_at",
			"direction", "confidence", "thesis",
			"support_points", "rebuttals", "cited_reports", "llm_model", "created_at",
		}))
	got, err := r.GetTranscriptByPanel(context.Background(), "p-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "t-1" {
		t.Errorf("id = %q", got.ID)
	}
}

func TestGetTranscriptByPanel_RejectsEmpty(t *testing.T) {
	r, _, done := newMockRepo(t)
	defer done()
	if _, err := r.GetTranscriptByPanel(context.Background(), "  "); err == nil {
		t.Error("expected error")
	}
}
