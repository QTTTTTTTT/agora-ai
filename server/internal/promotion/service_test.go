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

// serviceFixture wires a fresh PromotionRepo + sqlmock + a
// deterministic Service. Returned closures cover setup / teardown
// for every test in this file.
func serviceFixture(t *testing.T) (*Service, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	repo := repository.NewPromotionRepo(db)
	counter := 0
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	svc := &Service{
		Repo: repo,
		NewID: func() string {
			counter++
			return "id-" + intStr(counter)
		},
		Now:               func() time.Time { return now },
		DefaultShadowDays: 7,
		DefaultDecayRatio: 0.5,
	}
	return svc, mock, func() { db.Close() }
}

func intStr(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return "x" // good enough for low-count tests
}

// Propose happy path: basis backtest passes all eligibility
// checks, the service writes the row + audit log and returns the
// pending promotion.
func TestServiceProposeHappy(t *testing.T) {
	svc, mock, cleanup := serviceFixture(t)
	defer cleanup()

	svc.LookupBacktest = func(_ context.Context, jobID string) (*BacktestBasis, error) {
		if jobID != "job-1" {
			t.Fatalf("unexpected jobID %s", jobID)
		}
		return &BacktestBasis{
			JobID: jobID, FundID: "fund-1", Status: "completed",
			EngineKind:       "llm",
			CumulativeReturn: 0.18, SharpeRatio: 1.2,
		}, nil
	}

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO strategy_promotions`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_events`)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	p, err := svc.Propose(context.Background(), ProposeInput{
		FundID: "fund-1", ProposedBy: "user-1", BasisJobID: "job-1",
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if p.Status != StatusPendingReview {
		t.Errorf("status = %s, want pending_review", p.Status)
	}
	if p.EngineKind != "llm" {
		t.Errorf("engineKind = %s, want llm", p.EngineKind)
	}
	if p.BaselineMetrics.SharpeRatio != 1.2 {
		t.Errorf("baseline sharpe = %f, want 1.2", p.BaselineMetrics.SharpeRatio)
	}
	if p.ShadowDays != 7 {
		t.Errorf("shadowDays = %d, want 7 (default)", p.ShadowDays)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// Eligibility rejections: backtest missing / wrong fund / non-
// completed status / walk-forward required but absent.
func TestServiceProposeRejectsIneligibleBasis(t *testing.T) {
	cases := []struct {
		name   string
		lookup BacktestLookup
		strict bool
	}{
		{"missing", func(context.Context, string) (*BacktestBasis, error) { return nil, nil }, false},
		{"wrong fund", func(context.Context, string) (*BacktestBasis, error) {
			return &BacktestBasis{FundID: "other", Status: "completed", EngineKind: "llm"}, nil
		}, false},
		{"non-completed", func(context.Context, string) (*BacktestBasis, error) {
			return &BacktestBasis{FundID: "fund-1", Status: "running", EngineKind: "llm"}, nil
		}, false},
		{"walk-forward required but absent", func(context.Context, string) (*BacktestBasis, error) {
			return &BacktestBasis{FundID: "fund-1", Status: "completed", EngineKind: "llm", HasWalkForward: false}, nil
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, cleanup := serviceFixture(t)
			defer cleanup()
			svc.LookupBacktest = tc.lookup
			svc.RequireWalkForward = tc.strict
			_, err := svc.Propose(context.Background(), ProposeInput{
				FundID: "fund-1", ProposedBy: "user-1", BasisJobID: "job-1",
			})
			if !errors.Is(err, ErrBasisNotEligible) {
				t.Errorf("want ErrBasisNotEligible, got %v", err)
			}
		})
	}
}

// Approve with default ShadowDays > 0 transitions pending →
// shadow (NOT straight to active).
func TestServiceApproveDefaultsToShadow(t *testing.T) {
	svc, mock, cleanup := serviceFixture(t)
	defer cleanup()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)

	// Get → returns pending row.
	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("p-1").
		WillReturnRows(promotionMockRow("p-1", "fund-1", "pending_review", 7, now))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE strategy_promotions SET`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_events`)).
		WillReturnResult(sqlmock.NewResult(0, 1)) // approved event
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_events`)).
		WillReturnResult(sqlmock.NewResult(0, 1)) // shadow_started event
	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("p-1").
		WillReturnRows(promotionMockRow("p-1", "fund-1", "shadow", 7, now))

	got, err := svc.Approve(context.Background(), "p-1", "manager-1")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if got.Status != StatusShadow {
		t.Errorf("status = %s, want shadow", got.Status)
	}
}

// Approve with ShadowDays==0 jumps straight to active and triggers
// the prior-active supersede flow (which is a no-op here because
// there is no prior active).
func TestServiceApproveZeroShadowJumpsToActive(t *testing.T) {
	svc, mock, cleanup := serviceFixture(t)
	defer cleanup()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("p-1").
		WillReturnRows(promotionMockRow("p-1", "fund-1", "pending_review", 0, now))
	// supersedePriorActive: lookup returns no prior.
	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows(promotionCols()))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE strategy_promotions SET`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_events`)).
		WillReturnResult(sqlmock.NewResult(0, 1)) // approved
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_events`)).
		WillReturnResult(sqlmock.NewResult(0, 1)) // activated
	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("p-1").
		WillReturnRows(promotionMockRow("p-1", "fund-1", "active", 0, now))

	got, err := svc.Approve(context.Background(), "p-1", "manager-1")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if got.Status != StatusActive {
		t.Errorf("status = %s, want active", got.Status)
	}
}

// Activate from shadow → active triggers the supersede flow when
// a prior active exists.
func TestServiceActivateSupersedesPrior(t *testing.T) {
	svc, mock, cleanup := serviceFixture(t)
	defer cleanup()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	// 1) Get the new promotion (currently shadow).
	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("p-new").
		WillReturnRows(promotionMockRow("p-new", "fund-1", "shadow", 7, now))
	// 2) supersedePriorActive: lookup returns p-old, update it.
	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("fund-1").
		WillReturnRows(promotionMockRow("p-old", "fund-1", "active", 7, now))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE strategy_promotions SET`)).
		WillReturnResult(sqlmock.NewResult(0, 1)) // p-old → superseded
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_events`)).
		WillReturnResult(sqlmock.NewResult(0, 1)) // superseded event
	// 3) Update new → active.
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE strategy_promotions SET`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_events`)).
		WillReturnResult(sqlmock.NewResult(0, 1)) // shadow_finished
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_events`)).
		WillReturnResult(sqlmock.NewResult(0, 1)) // activated
	// 4) Re-Get post-update.
	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("p-new").
		WillReturnRows(promotionMockRow("p-new", "fund-1", "active", 7, now))

	got, err := svc.Activate(context.Background(), "p-new", "manager-1")
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if got.Status != StatusActive {
		t.Errorf("status = %s, want active", got.Status)
	}
}

// Deactivate respects the target status: rolled_back / decayed /
// superseded are allowed; anything else is rejected.
func TestServiceDeactivateRejectsBogusTarget(t *testing.T) {
	svc, _, cleanup := serviceFixture(t)
	defer cleanup()
	_, err := svc.Deactivate(context.Background(), "p-1", StatusPendingReview, "u", "")
	if !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("want ErrIllegalTransition, got %v", err)
	}
}

// ResolveActive surfaces nil-with-nil-err when no active exists.
func TestServiceResolveActiveNone(t *testing.T) {
	svc, mock, cleanup := serviceFixture(t)
	defer cleanup()
	mock.ExpectQuery(`SELECT id, fund_id`).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows(promotionCols()))

	p, err := svc.ResolveActive(context.Background(), "fund-1")
	if err != nil || p != nil {
		t.Errorf("expected nil/nil, got %+v err=%v", p, err)
	}
}

// --- helpers ---

func promotionCols() []string {
	return []string{
		"id", "fund_id", "proposed_by", "basis_job_id", "engine_kind",
		"engine_params", "baseline_metrics", "status", "shadow_days", "decay_ratio",
		"approved_by", "approved_at",
		"rejected_by", "rejected_at", "rejected_reason",
		"shadow_started_at", "shadow_completed_at",
		"activated_at", "deactivated_at", "deactivated_reason",
		"notes", "created_at", "updated_at",
	}
}

func promotionMockRow(id, fundID, status string, shadowDays int, now time.Time) *sqlmock.Rows {
	return sqlmock.NewRows(promotionCols()).
		AddRow(id, fundID, "u-1", "job-1", "llm",
			[]byte(`{}`), []byte(`{}`), status, shadowDays, 0.5,
			nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, now, now)
}
