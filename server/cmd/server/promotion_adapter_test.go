package main

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/promotion"
	"github.com/fundai/server/internal/repository"
)

// newPromotionAdapterTest builds an adapter against a sqlmock DB +
// bypasses the authorize() check (which requires a real fund) by
// installing a nil-safe fundRepo override pattern: we leave
// fundRepo as the real repo and let sqlmock satisfy the
// authorization queries.
func newPromotionAdapterTest(t *testing.T) (*promotionServiceAdapter, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	a := newPromotionServiceAdapter(db,
		repository.NewBacktestRepo(db),
		func(context.Context, string, int) (*promotion.LiveMetrics, error) { return nil, nil },
	)
	// Override authorize via short-circuiting fundRepo + companyRepo
	// with nil so authorize() returns nil immediately. The adapter
	// tolerates this when fundRepo IS nil; we just set it.
	a.fundRepo = nil
	a.companyRepo = nil
	return a, mock, func() { db.Close() }
}

// Propose: rejects when basis backtest isn't completed.
func TestAdapterProposeRejectsNonCompletedBasis(t *testing.T) {
	a, mock, cleanup := newPromotionAdapterTest(t)
	defer cleanup()

	// lookupBacktest queries via BacktestRepo.GetJob.
	mock.ExpectQuery(`FROM backtest_jobs`).
		WithArgs("job-1").
		WillReturnRows(sqlmock.NewRows(backtestCols()).
			AddRow("job-1", "fund-1", "u-1", "name", "llm", "running",
				[]byte(`{}`), nil, time.Now(), time.Now(),
				nil, nil, nil, nil, nil, nil, nil, nil, 0, 0, 0, 0, 0,
				time.Now(), nil, nil, nil, nil, nil))

	_, err := a.Propose("user-1", api.ProposeInput{
		FundID: "fund-1", BasisJobID: "job-1",
	})
	if !errors.Is(err, api.ErrPromotionBasisIneligible) {
		t.Errorf("want ErrPromotionBasisIneligible, got %v", err)
	}
}

// Propose: happy path with completed basis writes the row +
// audit log.
func TestAdapterProposeHappy(t *testing.T) {
	a, mock, cleanup := newPromotionAdapterTest(t)
	defer cleanup()

	mock.ExpectQuery(`FROM backtest_jobs`).
		WithArgs("job-1").
		WillReturnRows(sqlmock.NewRows(backtestCols()).
			AddRow("job-1", "fund-1", "u-1", "name", "llm", "completed",
				[]byte(`{}`), nil, time.Now(), time.Now(),
				100000.0, 110000.0, 0.10, 0.20, 0.15, 1.2, 0.05, 0.55, 25, 14, 11,
				100, 100, time.Now(), nil, nil, nil, nil, nil))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO strategy_promotions`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_events`)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	p, err := a.Propose("user-1", api.ProposeInput{
		FundID: "fund-1", BasisJobID: "job-1", Notes: "looks good",
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if p.EngineKind != "llm" {
		t.Errorf("engineKind = %s, want llm", p.EngineKind)
	}
	if p.BaselineMetrics.SharpeRatio != 1.2 {
		t.Errorf("sharpe = %f, want 1.2", p.BaselineMetrics.SharpeRatio)
	}
	if p.Status != "pending_review" {
		t.Errorf("status = %s, want pending_review", p.Status)
	}
}

// Approve: dual-control rejects when approver == proposer.
func TestAdapterApproveRejectsSelfApproval(t *testing.T) {
	a, mock, cleanup := newPromotionAdapterTest(t)
	defer cleanup()
	now := time.Now().UTC()

	// ensureBelongs Get
	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("p-1").
		WillReturnRows(adapterPromotionRow("p-1", "fund-1", "u-1", "pending_review", now))
	// ensureNotProposer Get — separate call so a second expectation
	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("p-1").
		WillReturnRows(adapterPromotionRow("p-1", "fund-1", "u-1", "pending_review", now))

	_, err := a.Approve("u-1", "fund-1", "p-1")
	if !errors.Is(err, api.ErrPromotionDualControl) {
		t.Errorf("want ErrPromotionDualControl, got %v", err)
	}
}

// Approve: forwards to the service when proposer != approver.
func TestAdapterApproveDualControlPass(t *testing.T) {
	a, mock, cleanup := newPromotionAdapterTest(t)
	defer cleanup()
	now := time.Now().UTC()

	// ensureBelongs + ensureNotProposer each Get.
	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("p-1").
		WillReturnRows(adapterPromotionRow("p-1", "fund-1", "proposer", "pending_review", now))
	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("p-1").
		WillReturnRows(adapterPromotionRow("p-1", "fund-1", "proposer", "pending_review", now))
	// svc.Approve: Get → UpdateStatus → InsertEvent (approved) → InsertEvent (shadow_started/activated) → Get
	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("p-1").
		WillReturnRows(adapterPromotionRow("p-1", "fund-1", "proposer", "pending_review", now))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE strategy_promotions SET`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_events`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_events`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("p-1").
		WillReturnRows(adapterPromotionRow("p-1", "fund-1", "proposer", "shadow", now))

	p, err := a.Approve("manager", "fund-1", "p-1")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if p.Status != "shadow" {
		t.Errorf("status = %s, want shadow", p.Status)
	}
}

// Cross-fund Get returns NotFound — defense against ID guessing.
func TestAdapterGetRejectsCrossFundLookup(t *testing.T) {
	a, mock, cleanup := newPromotionAdapterTest(t)
	defer cleanup()
	now := time.Now().UTC()

	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("p-1").
		WillReturnRows(adapterPromotionRow("p-1", "fund-OTHER", "u-1", "active", now))

	_, err := a.Get("u", "fund-1", "p-1")
	if !errors.Is(err, api.ErrPromotionNotFound) {
		t.Errorf("want ErrPromotionNotFound, got %v", err)
	}
}

// --- helpers ---

// backtestCols mirrors backtestJobSelect's column order. Adding a
// column to the schema means updating this in lockstep with the
// repo + the rest of the test fixtures.
func backtestCols() []string {
	return []string{
		"id", "fund_id", "user_id", "name", "engine_kind", "status",
		"request", "error", "window_start", "window_end",
		"initial_cash", "final_nav", "cumulative_return", "annualized_return",
		"volatility", "sharpe_ratio", "max_drawdown", "win_rate",
		"trade_count", "winning_trade_count", "losing_trade_count",
		"total_days", "done_days",
		"submitted_at", "started_at", "completed_at",
		"sweep_id", "sweep_cell", "walk_forward",
	}
}

func adapterPromotionRow(id, fundID, proposer, status string, now time.Time) *sqlmock.Rows {
	cols := []string{
		"id", "fund_id", "proposed_by", "basis_job_id", "engine_kind",
		"engine_params", "baseline_metrics", "status", "shadow_days", "decay_ratio",
		"approved_by", "approved_at",
		"rejected_by", "rejected_at", "rejected_reason",
		"shadow_started_at", "shadow_completed_at",
		"activated_at", "deactivated_at", "deactivated_reason",
		"notes", "created_at", "updated_at",
	}
	return sqlmock.NewRows(cols).
		AddRow(id, fundID, proposer, "job-1", "llm",
			[]byte(`{}`), []byte(`{}`), status, 7, 0.5,
			nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, now, now)
}
