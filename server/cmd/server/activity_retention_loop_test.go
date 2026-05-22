package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/repository"
)

// TestActivityRetentionLoopHonoursFundConfigDays asserts the cron's
// happy path: each fund's retentionDays produces a distinct cutoff,
// and the DELETE call goes through the WorkflowActivityRepo for every
// active fund found by ListActive.
//
// We don't simulate the leader lease here — the loop tolerates a nil
// checker and behaves as the permanent leader, which is exactly what
// the test wants.
func TestActivityRetentionLoopHonoursFundConfigDays(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// Two active funds with different retentionDays.
	now := time.Now().UTC()
	fundFastCfg := mustEncodeFundConfig(t, map[string]any{"activityRetentionDays": 2})
	fundSlowCfg := mustEncodeFundConfig(t, map[string]any{"activityRetentionDays": 10})

	rows := sqlmock.NewRows([]string{
		"id", "company_id", "name", "description", "trading_mode",
		"initial_capital", "current_capital", "total_assets", "nav",
		"status", "config", "created_at", "updated_at",
	}).
		AddRow("fund-fast", "co-1", "Fast", sql.NullString{String: "", Valid: false}, "paper", 1000.0, 1000.0, 1000.0, 1.0, "active", fundFastCfg, now, now).
		AddRow("fund-slow", "co-1", "Slow", sql.NullString{String: "", Valid: false}, "paper", 1000.0, 1000.0, 1000.0, 1.0, "active", fundSlowCfg, now, now)

	mock.ExpectQuery(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at`).
		WillReturnRows(rows)

	// The cutoff must be roughly now - 2d for the fast fund and
	// now - 10d for the slow fund. We can't pin it to the exact
	// microsecond, so we use a custom matcher that accepts any
	// timestamp inside the right per-fund window.
	mock.ExpectExec(`DELETE FROM workflow_activity_events WHERE fund_id = \$1 AND event_at < \$2`).
		WithArgs("fund-fast", aroundArg{ref: now.AddDate(0, 0, -2), tol: time.Minute}).
		WillReturnResult(sqlmock.NewResult(0, 3))

	mock.ExpectExec(`DELETE FROM workflow_activity_events WHERE fund_id = \$1 AND event_at < \$2`).
		WithArgs("fund-slow", aroundArg{ref: now.AddDate(0, 0, -10), tol: time.Minute}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	loop := newActivityRetentionLoop(repository.NewFundRepo(db), repository.NewWorkflowActivityRepo(db))
	loop.runOnce()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestActivityRetentionLoopUsesDefaultWhenConfigMissing closes the
// gap for legacy funds whose JSON doesn't pin a retention setting:
// the cron must still sweep them, using DefaultActivityRetentionDays.
func TestActivityRetentionLoopUsesDefaultWhenConfigMissing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "company_id", "name", "description", "trading_mode",
			"initial_capital", "current_capital", "total_assets", "nav",
			"status", "config", "created_at", "updated_at",
		}).AddRow("fund-legacy", "co-1", "Legacy", sql.NullString{}, "paper", 1000.0, 1000.0, 1000.0, 1.0, "active",
			[]byte("{}"), now, now))

	mock.ExpectExec(`DELETE FROM workflow_activity_events WHERE fund_id = \$1 AND event_at < \$2`).
		WithArgs("fund-legacy", aroundArg{ref: now.AddDate(0, 0, -DefaultActivityRetentionDays), tol: time.Minute}).
		WillReturnResult(sqlmock.NewResult(0, 0))

	loop := newActivityRetentionLoop(repository.NewFundRepo(db), repository.NewWorkflowActivityRepo(db))
	loop.runOnce()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// aroundArg matches a time.Time argument when it falls within ±tol of
// ref. Needed because the loop computes "now - Nd" at call time and
// sqlmock's default equality wants an exact match. Implements the
// sqlmock.Argument interface (Match(driver.Value) bool).
type aroundArg struct {
	ref time.Time
	tol time.Duration
}

func (a aroundArg) Match(v driver.Value) bool {
	asTime, ok := v.(time.Time)
	if !ok {
		return false
	}
	diff := asTime.Sub(a.ref)
	if diff < 0 {
		diff = -diff
	}
	return diff <= a.tol
}

func mustEncodeFundConfig(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode fund config: %v", err)
	}
	return raw
}

// ensure runOnce's signature stays accessible to the test file.
var _ = (*activityRetentionLoop)(nil)
var _ = context.Background
