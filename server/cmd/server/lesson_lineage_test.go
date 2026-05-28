package main

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestExtractLessonHypothesisBullish(t *testing.T) {
	got := extractLessonHypothesis("明日开盘前评估 600519 是否具备转 buy 条件以加仓。")
	if got == nil {
		t.Fatal("expected hypothesis, got nil")
	}
	if got.PredictedDirection != 1 {
		t.Errorf("direction: want +1, got %d", got.PredictedDirection)
	}
	if got.Symbol != "600519" {
		t.Errorf("symbol: want 600519, got %q", got.Symbol)
	}
	if got.WindowDays != 1 {
		t.Errorf("window: want 1 (明日), got %d", got.WindowDays)
	}
}

func TestExtractLessonHypothesisBearishWeeklyDefault(t *testing.T) {
	got := extractLessonHypothesis("AAPL 仓位需要在一周内减配以控制集中度风险。")
	if got == nil {
		t.Fatal("expected hypothesis, got nil")
	}
	if got.PredictedDirection != -1 {
		t.Errorf("direction: want -1, got %d", got.PredictedDirection)
	}
	if got.Symbol != "AAPL" {
		t.Errorf("symbol: want AAPL, got %q", got.Symbol)
	}
	if got.WindowDays != 7 {
		t.Errorf("window: want 7, got %d", got.WindowDays)
	}
}

func TestExtractLessonHypothesisIgnoresNeutralOpinion(t *testing.T) {
	if h := extractLessonHypothesis("组合整体应继续保持观望，等待信号更明确再行动。"); h != nil {
		t.Errorf("expected nil hypothesis for neutral text, got %+v", h)
	}
}

func TestExtractLessonHypothesisIgnoresCommonAllCaps(t *testing.T) {
	// "PM 应加仓" — bullish but symbol is "PM" which is a common
	// all-caps word, NOT a real ticker; we should keep direction
	// but drop the symbol.
	got := extractLessonHypothesis("PM 应加仓 5%。")
	if got == nil {
		t.Fatal("expected hypothesis, got nil")
	}
	if got.Symbol != "" {
		t.Errorf("symbol: expected empty (PM is filtered), got %q", got.Symbol)
	}
	if got.PredictedDirection != 1 {
		t.Errorf("direction: want +1, got %d", got.PredictedDirection)
	}
}

func TestRecordLessonLineagePersists(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tradingDate := time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)
	memID := "11111111-1111-1111-1111-111111111111"
	fundID := "22222222-2222-2222-2222-222222222222"
	agentID := sql.NullString{String: "33333333-3333-3333-3333-333333333333", Valid: true}

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO lesson_pnl_lineage`)).
		WithArgs(memID, fundID, agentID, sql.NullString{String: "688205", Valid: true},
			"明日 688205 加仓至 6%。", 1, 1, tradingDate.AddDate(0, 0, 1)).
		WillReturnResult(sqlmock.NewResult(1, 1))

	recordLessonLineage(context.Background(), db, memID, fundID, agentID,
		[]string{"明日 688205 加仓至 6%。", "组合整体保持观望以等待信号。"}, // 第二条 neutral，会被跳过
		tradingDate,
	)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestRecordLessonLineageNoOpWhenEmptyInputs(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// 没有 expectation；任何调用都会让 mock 报错。
	recordLessonLineage(context.Background(), db, "", "fund", sql.NullString{},
		[]string{"任何 lesson"}, time.Now())
	recordLessonLineage(context.Background(), nil, "mem", "fund", sql.NullString{},
		[]string{"任何 lesson"}, time.Now())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestClassifyVerdict(t *testing.T) {
	cases := []struct {
		score float64
		want  string
	}{
		{1.0, "validated"},
		{0.6, "validated"},
		{0.2, "weak_validated"},
		{0.05, "neutral"},
		{-0.05, "neutral"},
		{-0.2, "weak_refuted"},
		{-0.7, "refuted"},
	}
	for _, c := range cases {
		if got := classifyVerdict(c.score); got != c.want {
			t.Errorf("classifyVerdict(%.2f) = %q, want %q", c.score, got, c.want)
		}
	}
}

func TestLessonScoringLoopScoreOneValidates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	l := &lessonScoringLoop{db: db}

	fundID := "11111111-1111-1111-1111-111111111111"
	due := dueLessonLineage{
		ID:                 42,
		FundID:             fundID,
		PredictedDirection: 1,
		WindowOpen:         time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC),
		WindowClose:        time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC),
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT total_assets")).
		WithArgs(fundID, due.WindowOpen).
		WillReturnRows(sqlmock.NewRows([]string{"total_assets"}).AddRow(1000.0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT total_assets")).
		WithArgs(fundID, due.WindowClose).
		WillReturnRows(sqlmock.NewRows([]string{"total_assets"}).AddRow(1040.0))
	// retPct = 4%, scaled = 3 (cap), signed = +3 → score = 1.0 → validated
	mock.ExpectExec(regexp.QuoteMeta("UPDATE lesson_pnl_lineage")).
		WithArgs(
			sqlmock.AnyArg(), // observed_at NOW()
			sqlmock.AnyArg(), // pnl ≈ 40
			sqlmock.AnyArg(), // retPct ≈ 4
			1.0,              // score = 1.0 (clamped at +3 / 3)
			"validated",      // verdict
			int64(42),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := l.scoreOne(context.Background(), due); err != nil {
		t.Fatalf("scoreOne error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestLessonScoringLoopScoreOneSkipsWhenNavMissing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	l := &lessonScoringLoop{db: db}

	due := dueLessonLineage{
		ID:                 7,
		FundID:             "fund",
		PredictedDirection: -1,
		WindowOpen:         time.Now().UTC().AddDate(0, 0, -7),
		WindowClose:        time.Now().UTC(),
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT total_assets")).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT total_assets")).
		WillReturnRows(sqlmock.NewRows([]string{"total_assets"}).AddRow(1000.0))

	// 应该静默返回 nil（不写 UPDATE，让下一轮再试）。
	if err := l.scoreOne(context.Background(), due); err != nil {
		t.Fatalf("scoreOne error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestLessonHitRateAggregatesFraction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	memoryID := "11111111-1111-1111-1111-111111111111"
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FILTER")).
		WithArgs(memoryID).
		WillReturnRows(sqlmock.NewRows([]string{"hits", "total"}).AddRow(3, 5))

	rate, total, err := lessonHitRate(context.Background(), db, memoryID)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if total != 5 {
		t.Errorf("total: want 5, got %d", total)
	}
	if rate != 0.6 {
		t.Errorf("rate: want 0.6, got %.3f", rate)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestLessonHitRateZeroWhenNoSamples(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	memoryID := "11111111-1111-1111-1111-111111111111"
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FILTER")).
		WithArgs(memoryID).
		WillReturnRows(sqlmock.NewRows([]string{"hits", "total"}).AddRow(0, 0))

	rate, total, err := lessonHitRate(context.Background(), db, memoryID)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if total != 0 || rate != 0 {
		t.Errorf("want zero, got rate=%.3f total=%d", rate, total)
	}
}

func TestDecayRecentLessonsDropsOldLowWeight(t *testing.T) {
	now := time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)
	lessons := []string{"recent", "stale", "ancient"}
	timestamps := []string{
		now.Format(time.RFC3339),                       // age=0 days, weight=1
		now.AddDate(0, 0, -45).Format(time.RFC3339),    // age=45 days → 0.5^(45/60) ≈ 0.59
		now.AddDate(0, 0, -240).Format(time.RFC3339),   // age=240 days → 0.5^(240/60) = 0.0625 < 0.1
	}
	survivors, stamps := decayRecentLessons(lessons, timestamps, now, 60, 0.1)
	if len(survivors) != 2 {
		t.Fatalf("survivors: want 2, got %d (%v)", len(survivors), survivors)
	}
	if survivors[0] != "recent" || survivors[1] != "stale" {
		t.Errorf("ordering: got %v", survivors)
	}
	if len(stamps) != 2 {
		t.Errorf("stamps len: want 2, got %d", len(stamps))
	}
}

func TestDecayRecentLessonsLegacyTimestamps(t *testing.T) {
	now := time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)
	// 没有 timestamps（legacy config） — 应该全部 surviving 并打上 now stamp。
	lessons := []string{"a", "b"}
	survivors, stamps := decayRecentLessons(lessons, nil, now, 60, 0.1)
	if len(survivors) != 2 {
		t.Errorf("survivors: want 2, got %d", len(survivors))
	}
	for _, s := range stamps {
		if s != now.Format(time.RFC3339) {
			t.Errorf("stamp: want now, got %q", s)
		}
	}
}

func TestMemoryArchiveLoopArchiveBatchMovesAndCounts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	l := &memoryArchiveLoop{db: db, ageDays: defaultMemoryArchiveAgeDays, batch: defaultMemoryArchiveBatch}

	cutoff := time.Now().UTC().AddDate(0, 0, -90)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("WITH due AS")).
		WithArgs(cutoff, 50).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(7))
	mock.ExpectCommit()

	moved, err := l.archiveBatch(context.Background(), cutoff, 50)
	if err != nil {
		t.Fatalf("archiveBatch error: %v", err)
	}
	if moved != 7 {
		t.Errorf("moved: want 7, got %d", moved)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestMemoryArchiveLoopArchiveBatchZeroWhenNothingDue(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	l := &memoryArchiveLoop{db: db, ageDays: 90, batch: 100}

	cutoff := time.Now().UTC().AddDate(0, 0, -90)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("WITH due AS")).
		WithArgs(cutoff, 100).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectCommit()

	moved, err := l.archiveBatch(context.Background(), cutoff, 100)
	if err != nil {
		t.Fatalf("archiveBatch error: %v", err)
	}
	if moved != 0 {
		t.Errorf("moved: want 0, got %d", moved)
	}
}

func TestContainsStringFold(t *testing.T) {
	hay := []string{"Hello", "World"}
	if !containsStringFold(hay, "hello") {
		t.Errorf("expected case-insensitive match")
	}
	if !containsStringFold(hay, "WORLD") {
		t.Errorf("expected case-insensitive match")
	}
	if containsStringFold(hay, "foo") {
		t.Errorf("expected no match")
	}
}
