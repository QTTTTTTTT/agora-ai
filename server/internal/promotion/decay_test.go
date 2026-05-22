package promotion

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/repository"
)

// buildSnapshot: when actual Sharpe = 0.9 × baseline 1.5 = ratio
// 0.6, and decayRatio threshold = 0.5, decay flag stays FALSE.
func TestBuildSnapshotAboveThreshold(t *testing.T) {
	p := &Promotion{
		ID: "p", DecayRatio: 0.5,
		BaselineMetrics: BaselineMetrics{SharpeRatio: 1.5},
	}
	sharpe := 0.9
	now := time.Now()
	snap := buildSnapshot(p, &LiveMetrics{Sharpe: &sharpe, DataComplete: true}, 30, "id", now)
	if snap.DecayFlag {
		t.Errorf("0.9/1.5 = 0.6 > 0.5 threshold; should NOT flag")
	}
	if snap.SharpeDecayRatio == nil || *snap.SharpeDecayRatio < 0.59 || *snap.SharpeDecayRatio > 0.61 {
		t.Errorf("ratio wrong: %+v", snap.SharpeDecayRatio)
	}
}

// buildSnapshot: when actual Sharpe falls below the threshold,
// decay flag becomes true.
func TestBuildSnapshotBelowThreshold(t *testing.T) {
	p := &Promotion{
		ID: "p", DecayRatio: 0.5,
		BaselineMetrics: BaselineMetrics{SharpeRatio: 1.5},
	}
	sharpe := 0.5 // 0.5/1.5 = 0.33 < 0.5
	now := time.Now()
	snap := buildSnapshot(p, &LiveMetrics{Sharpe: &sharpe, DataComplete: true}, 30, "id", now)
	if !snap.DecayFlag {
		t.Errorf("0.5/1.5 = 0.33 < 0.5 threshold; SHOULD flag")
	}
}

// buildSnapshot: missing live data → no ratio, note set.
func TestBuildSnapshotInsufficientData(t *testing.T) {
	p := &Promotion{ID: "p", DecayRatio: 0.5, BaselineMetrics: BaselineMetrics{SharpeRatio: 1.0}}
	snap := buildSnapshot(p, &LiveMetrics{DataComplete: false}, 30, "id", time.Now())
	if snap.DecayFlag {
		t.Errorf("insufficient data should not flag decay")
	}
	if snap.Notes == "" {
		t.Errorf("expected notes explaining the no-op")
	}
}

// buildSnapshot: negative baseline Sharpe — concept doesn't apply.
func TestBuildSnapshotNegativeBaselineSkipsRatio(t *testing.T) {
	p := &Promotion{ID: "p", DecayRatio: 0.5, BaselineMetrics: BaselineMetrics{SharpeRatio: -0.2}}
	sharpe := 0.5
	snap := buildSnapshot(p, &LiveMetrics{Sharpe: &sharpe, DataComplete: true}, 30, "id", time.Now())
	if snap.SharpeDecayRatio != nil {
		t.Errorf("should not compute ratio with non-positive baseline")
	}
	if snap.DecayFlag {
		t.Errorf("should never flag with non-positive baseline")
	}
}

// Sample: persists the snapshot and (since this is the first
// flagged sample) does NOT yet auto-downgrade.
func TestDecayMonitorSampleNoStreak(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := repository.NewPromotionRepo(db)
	now := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	svc := &Service{Repo: repo, NewID: func() string { return "i" }, Now: func() time.Time { return now }}
	mon := &DecayMonitor{
		Service: svc, Repo: repo,
		LiveLookup: func(context.Context, string, int) (*LiveMetrics, error) {
			sh := 0.4 // below threshold
			return &LiveMetrics{Sharpe: &sh, DataComplete: true}, nil
		},
		NewID:                       func() string { return "h-1" },
		Now:                         func() time.Time { return now },
		WindowDays:                  30,
		MinSnapshotsBeforeDowngrade: 3,
	}
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_health_snapshots`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Streak check: returns only 1 row → no downgrade.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, promotion_id, snapshot_at`)).
		WithArgs("p-1", 3).
		WillReturnRows(sqlmock.NewRows([]string{"id", "promotion_id", "snapshot_at", "window_days", "actual_sharpe", "actual_return", "actual_max_drawdown", "actual_trade_count", "sharpe_decay_ratio", "decay_flag", "notes"}).
			AddRow("h-1", "p-1", now, 30, 0.4, nil, nil, 0, 0.4, true, nil))

	p := &Promotion{
		ID: "p-1", FundID: "fund-1", Status: StatusActive, DecayRatio: 0.5,
		BaselineMetrics: BaselineMetrics{SharpeRatio: 1.5},
	}
	_, err = mon.Sample(context.Background(), p)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// Sample: streak of 3 → auto-downgrade fires.
func TestDecayMonitorSampleStreakTriggersDowngrade(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	repo := repository.NewPromotionRepo(db)
	now := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	svc := &Service{Repo: repo, NewID: func() string { return "audit-id" }, Now: func() time.Time { return now }}
	downgraded := ""
	mon := &DecayMonitor{
		Service: svc, Repo: repo,
		LiveLookup: func(context.Context, string, int) (*LiveMetrics, error) {
			sh := 0.4
			return &LiveMetrics{Sharpe: &sh, DataComplete: true}, nil
		},
		NewID:                       func() string { return "h-1" },
		Now:                         func() time.Time { return now },
		MinSnapshotsBeforeDowngrade: 3,
		OnDowngrade: func(_ context.Context, fundID, _ string) { downgraded = fundID },
	}
	// 1) Insert today's snapshot.
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_health_snapshots`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// 2) Streak query: 3 flagged samples.
	rows := sqlmock.NewRows([]string{"id", "promotion_id", "snapshot_at", "window_days", "actual_sharpe", "actual_return", "actual_max_drawdown", "actual_trade_count", "sharpe_decay_ratio", "decay_flag", "notes"}).
		AddRow("h-1", "p-1", now, 30, 0.4, nil, nil, 0, 0.27, true, nil).
		AddRow("h-2", "p-1", now.Add(-24*time.Hour), 30, 0.3, nil, nil, 0, 0.20, true, nil).
		AddRow("h-3", "p-1", now.Add(-48*time.Hour), 30, 0.35, nil, nil, 0, 0.23, true, nil)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, promotion_id, snapshot_at`)).
		WithArgs("p-1", 3).
		WillReturnRows(rows)
	// 3) Service.Deactivate(target=decayed) — Get → UPDATE → audit insert → Get
	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("p-1").
		WillReturnRows(sqlmock.NewRows(promotionCols()).
			AddRow("p-1", "fund-1", "u-1", "job-1", "llm",
				[]byte(`{}`), []byte(`{}`), "active", 7, 0.5,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, now, now))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE strategy_promotions SET`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_events`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("p-1").
		WillReturnRows(sqlmock.NewRows(promotionCols()).
			AddRow("p-1", "fund-1", "u-1", "job-1", "llm",
				[]byte(`{}`), []byte(`{}`), "decayed", 7, 0.5,
				nil, nil, nil, nil, nil, nil, nil, nil, now, "decay", nil, now, now))

	p := &Promotion{
		ID: "p-1", FundID: "fund-1", Status: StatusActive, DecayRatio: 0.5,
		BaselineMetrics: BaselineMetrics{SharpeRatio: 1.5},
	}
	if _, err := mon.Sample(context.Background(), p); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if downgraded != "fund-1" {
		t.Errorf("OnDowngrade not called; got %q", downgraded)
	}
}

// Sample rejects non-live promotions — the scheduler should
// never call it for, say, a superseded row.
func TestDecayMonitorSampleRejectsNonLive(t *testing.T) {
	mon := &DecayMonitor{
		Service:    &Service{},
		Repo:       &repository.PromotionRepo{},
		LiveLookup: func(context.Context, string, int) (*LiveMetrics, error) { return nil, nil },
		NewID:      func() string { return "" },
		Now:        time.Now,
	}
	p := &Promotion{ID: "p", Status: StatusSuperseded}
	if _, err := mon.Sample(context.Background(), p); err == nil {
		t.Errorf("expected error for non-live promotion")
	}
}

// SampleAll: live lookup failure for one promotion shouldn't
// stop the loop — the error is captured and returned, but the
// other promotions still get processed.
func TestDecayMonitorSampleAllContinuesOnError(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	repo := repository.NewPromotionRepo(db)
	now := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	svc := &Service{Repo: repo, NewID: func() string { return "i" }, Now: func() time.Time { return now }}

	// ListLive returns 2 rows.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id`)).
		WillReturnRows(sqlmock.NewRows(promotionCols()).
			AddRow("p-1", "fund-1", "u-1", "job-1", "llm",
				[]byte(`{}`), []byte(`{}`), "active", 7, 0.5,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, now, now).
			AddRow("p-2", "fund-2", "u-1", "job-2", "llm",
				[]byte(`{}`), []byte(`{}`), "shadow", 7, 0.5,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, now, now))
	// p-1 lookup fails; p-2 succeeds and snapshot persists +
	// streak query (no streak).
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_health_snapshots`)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	call := 0
	mon := &DecayMonitor{
		Service: svc, Repo: repo,
		NewID: func() string { return "h" },
		Now:   func() time.Time { return now },
		LiveLookup: func(_ context.Context, fundID string, _ int) (*LiveMetrics, error) {
			call++
			if fundID == "fund-1" {
				return nil, errors.New("market data down")
			}
			sh := 1.2 // healthy
			return &LiveMetrics{Sharpe: &sh, DataComplete: true}, nil
		},
		MinSnapshotsBeforeDowngrade: 3,
	}
	got, err := mon.SampleAll(context.Background())
	if err == nil {
		t.Errorf("expected first-error to be returned")
	}
	if call != 2 {
		t.Errorf("expected loop to call LiveLookup twice, got %d", call)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 successful snapshot, got %d", len(got))
	}
}
