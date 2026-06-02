package alphalesson

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/agentreputation"
)

func newMockRepo(t *testing.T) (*Repo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewRepo(db), mock, func() { _ = db.Close() }
}

func TestWriteOptions_Normalize(t *testing.T) {
	o := WriteOptions{}.normalize()
	if o.AlphaThreshold <= 0 {
		t.Errorf("threshold = %v", o.AlphaThreshold)
	}
	if o.Layer == "" {
		t.Error("layer empty")
	}
	if o.Visibility == "" {
		t.Error("visibility empty")
	}
	if o.OriginKind == "" {
		t.Error("origin empty")
	}
}

func TestWriteAlphaLessons_NilRepo(t *testing.T) {
	var r *Repo
	if _, err := r.WriteAlphaLessons(context.Background(), nil, WriteOptions{}); err == nil {
		t.Error("expected nil-repo error")
	}
}

func TestWriteAlphaLessons_EmptyOK(t *testing.T) {
	r, _, done := newMockRepo(t)
	defer done()
	if n, err := r.WriteAlphaLessons(context.Background(), nil, WriteOptions{}); err != nil || n != 0 {
		t.Errorf("empty: n=%d err=%v", n, err)
	}
}

func TestWriteAlphaLessons_HappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	asof := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	out := agentreputation.Outcome{
		ID: "o1", FundID: "f1", AgentID: "fund_analyst", AgentName: "F",
		AgentKind: agentreputation.KindAnalyst, Category: "fundamentals",
		Symbol: "AAPL", AsOf: asof,
		Direction: agentreputation.DirBullish, Confidence: 65,
		RealisedReturn: 0.04, BenchmarkReturn: 0.01, Alpha: 0.03,
		HorizonDays: 5,
	}
	mock.ExpectBegin()
	mock.ExpectPrepare("INSERT INTO memories")
	mock.ExpectQuery("FROM memories WHERE source_outcome_id").
		WithArgs("o1").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("INSERT INTO memories").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	n, err := r.WriteAlphaLessons(context.Background(), []agentreputation.Outcome{out}, WriteOptions{})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 1 {
		t.Errorf("want 1, got %d", n)
	}
}

func TestWriteAlphaLessons_BelowThresholdSkipped(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	out := agentreputation.Outcome{
		ID: "o1", FundID: "f1", AgentID: "a", AgentKind: agentreputation.KindAnalyst,
		Direction: agentreputation.DirBullish, Symbol: "AAPL", Confidence: 50,
		Alpha: 0.001, AsOf: time.Now(),
	}
	mock.ExpectBegin()
	mock.ExpectPrepare("INSERT INTO memories")
	mock.ExpectCommit()
	n, err := r.WriteAlphaLessons(context.Background(), []agentreputation.Outcome{out}, WriteOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 lessons written for sub-threshold alpha, got %d", n)
	}
}

func TestWriteAlphaLessons_DedupSkipsExisting(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	out := agentreputation.Outcome{
		ID: "o-dupe", FundID: "f1", AgentID: "a", AgentKind: agentreputation.KindAnalyst,
		Direction: agentreputation.DirBullish, Symbol: "AAPL", Confidence: 60,
		Alpha: 0.03, AsOf: time.Now(),
	}
	mock.ExpectBegin()
	mock.ExpectPrepare("INSERT INTO memories")
	mock.ExpectQuery("FROM memories WHERE source_outcome_id").
		WithArgs("o-dupe").
		WillReturnRows(sqlmock.NewRows([]string{"?"}).AddRow(1))
	mock.ExpectCommit()
	n, err := r.WriteAlphaLessons(context.Background(), []agentreputation.Outcome{out}, WriteOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 0 {
		t.Errorf("expected dedupe skip, got %d", n)
	}
}

func TestListLessons_HappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	now := time.Now()
	mock.ExpectQuery("FROM memories").
		WithArgs("f1", 50).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "agent_tag", "content", "title",
			"alpha_vs_benchmark", "source_outcome_id", "trading_date", "created_at",
		}).AddRow("l1", "f1", "fund_analyst", "lesson body", "lesson title",
			0.025, "o1", now, now))
	out, err := r.ListLessons(context.Background(), ListLessonsParams{FundID: "f1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].AgentTag != "fund_analyst" {
		t.Errorf("got %+v", out)
	}
}

func TestListLessons_RejectsMissingFundID(t *testing.T) {
	r, _, done := newMockRepo(t)
	defer done()
	if _, err := r.ListLessons(context.Background(), ListLessonsParams{}); err == nil {
		t.Error("expected fundID error")
	}
}

func TestFormatLessonTitle(t *testing.T) {
	out := agentreputation.Outcome{
		AgentName: "Bull", Direction: agentreputation.DirBullish, Symbol: "AAPL",
		HorizonDays: 5, Alpha: 0.02,
	}
	got := formatLessonTitle(out)
	if got == "" {
		t.Error("empty title")
	}
}

func TestLessonTags(t *testing.T) {
	out := agentreputation.Outcome{
		AgentID: "fund_analyst", AgentKind: agentreputation.KindAnalyst,
		Category: "fundamentals", Symbol: "aapl", Alpha: 0.02,
	}
	got := lessonTags(out)
	gotSet := map[string]bool{}
	for _, t := range got {
		gotSet[t] = true
	}
	for _, want := range []string{"alpha_lesson", "analyst", "fund_analyst", "AAPL", "positive_alpha", "fundamentals"} {
		if !gotSet[want] {
			t.Errorf("missing tag %q in %v", want, got)
		}
	}
}
