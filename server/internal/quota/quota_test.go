package quota

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newServiceFromMock(t *testing.T) (*Service, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	frozen := time.Date(2026, time.May, 18, 12, 0, 0, 0, time.UTC)
	s := NewService(db)
	s.now = func() time.Time { return frozen }
	return s, mock, db
}

// TestNilSafe verifies the service can be invoked with nil receivers
// and no-op DBs without panicking. Workflow code paths that ALWAYS
// call Check* would otherwise be brittle in CI / test environments
// that don't wire a quota service.
func TestNilSafe(t *testing.T) {
	var s *Service
	if err := s.CheckActiveAgents(context.Background(), "fund-1", 1); err != nil {
		t.Errorf("nil receiver should be no-op, got %v", err)
	}
	if err := s.CheckConcurrentWorkflows(context.Background(), "fund-1"); err != nil {
		t.Errorf("nil receiver should be no-op, got %v", err)
	}
	if err := s.CheckLLMTokens(context.Background(), "fund-1", 100); err != nil {
		t.Errorf("nil receiver should be no-op, got %v", err)
	}
	if err := s.RecordLLMTokens(context.Background(), "fund-1", 10, 5); err != nil {
		t.Errorf("nil receiver should be no-op, got %v", err)
	}
}

// TestEffectiveQuotaMergesDefaultsAndOverride proves the layering
// contract: per-fund row's non-null fields override platform defaults,
// null fields fall through. This is the core invariant — a regression
// here would silently apply the wrong cap.
func TestEffectiveQuotaMergesDefaultsAndOverride(t *testing.T) {
	s, mock, db := newServiceFromMock(t)
	defer db.Close()

	defaultUpdated := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	overrideUpdated := time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(fund_id::text, ''), max_active_agents")).
		WillReturnRows(sqlmock.NewRows([]string{"fund_id", "max_active_agents", "max_concurrent_workflows", "daily_llm_token_limit", "monthly_llm_token_limit", "notes", "updated_at"}).
			AddRow("", sql.NullInt64{Int64: 5, Valid: true}, sql.NullInt64{Int64: 3, Valid: true}, sql.NullInt64{Int64: 100000, Valid: true}, sql.NullInt64{}, "platform default", defaultUpdated))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(fund_id::text, ''), max_active_agents")).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"fund_id", "max_active_agents", "max_concurrent_workflows", "daily_llm_token_limit", "monthly_llm_token_limit", "notes", "updated_at"}).
			AddRow("fund-1", sql.NullInt64{Int64: 10, Valid: true}, sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{Int64: 1000000, Valid: true}, "premium", overrideUpdated))

	q, err := s.EffectiveQuota(context.Background(), "fund-1")
	if err != nil {
		t.Fatalf("effective: %v", err)
	}
	if q.MaxActiveAgents.Int64 != 10 {
		t.Errorf("agent cap should come from override: got %d", q.MaxActiveAgents.Int64)
	}
	if q.MaxConcurrentWorkflows.Int64 != 3 {
		t.Errorf("workflow cap should fall through to default: got %d", q.MaxConcurrentWorkflows.Int64)
	}
	if q.DailyLLMTokenLimit.Int64 != 100000 {
		t.Errorf("daily token cap should fall through to default: got %d", q.DailyLLMTokenLimit.Int64)
	}
	if q.MonthlyLLMTokenLimit.Int64 != 1000000 {
		t.Errorf("monthly token cap should come from override: got %d", q.MonthlyLLMTokenLimit.Int64)
	}
	if q.Notes != "premium" {
		t.Errorf("notes should prefer override: got %s", q.Notes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestCheckActiveAgentsRejects exercises the rejection path. The
// returned error MUST be ErrQuotaExceeded-compatible so callers can
// switch on it cleanly.
func TestCheckActiveAgentsRejects(t *testing.T) {
	s, mock, db := newServiceFromMock(t)
	defer db.Close()

	// EffectiveQuota: default + fund row, fund row caps at 5.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(fund_id::text, ''), max_active_agents")).
		WillReturnRows(sqlmock.NewRows([]string{"fund_id", "max_active_agents", "max_concurrent_workflows", "daily_llm_token_limit", "monthly_llm_token_limit", "notes", "updated_at"})) // empty: no default row
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(fund_id::text, ''), max_active_agents")).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"fund_id", "max_active_agents", "max_concurrent_workflows", "daily_llm_token_limit", "monthly_llm_token_limit", "notes", "updated_at"}).
			AddRow("fund-1", sql.NullInt64{Int64: 5, Valid: true}, sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{}, "", time.Now()))

	// Active count = 5; proposing +1 should breach.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM agents")).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(5))

	err := s.CheckActiveAgents(context.Background(), "fund-1", 1)
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected ErrQuotaExceeded, got %v", err)
	}
	var ee *ExceededError
	if !errors.As(err, &ee) || ee.Resource != ResourceActiveAgents {
		t.Errorf("expected typed ExceededError with active_agents resource, got %T %v", err, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestCheckLLMTokensRespectsBothWindows confirms daily caps fire
// before monthly caps when both are configured — daily is the cheaper
// check (smaller SUM scan) so it should short-circuit first.
func TestCheckLLMTokensRespectsBothWindows(t *testing.T) {
	s, mock, db := newServiceFromMock(t)
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(fund_id::text, ''), max_active_agents")).
		WillReturnRows(sqlmock.NewRows([]string{"fund_id", "max_active_agents", "max_concurrent_workflows", "daily_llm_token_limit", "monthly_llm_token_limit", "notes", "updated_at"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(fund_id::text, ''), max_active_agents")).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"fund_id", "max_active_agents", "max_concurrent_workflows", "daily_llm_token_limit", "monthly_llm_token_limit", "notes", "updated_at"}).
			AddRow("fund-1", sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{Int64: 1000, Valid: true}, sql.NullInt64{Int64: 10000, Valid: true}, "", time.Now()))

	// Daily usage = 900; requested = 200 → 1100 > 1000, reject.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT SUM(total_tokens)")).
		WithArgs("fund-1", "2026-05-18", "2026-05-18").
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(900))

	err := s.CheckLLMTokens(context.Background(), "fund-1", 200)
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected ErrQuotaExceeded, got %v", err)
	}
	var ee *ExceededError
	if !errors.As(err, &ee) || ee.Resource != ResourceLLMTokensDaily {
		t.Errorf("expected daily quota breach, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestCheckLLMTokensSkipsWhenNoLimits short-circuits when neither
// daily nor monthly caps are configured. Critical for performance:
// LLM client calls Check on every request, and most funds in the
// platform-default tier will have no token cap.
func TestCheckLLMTokensSkipsWhenNoLimits(t *testing.T) {
	s, mock, db := newServiceFromMock(t)
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(fund_id::text, ''), max_active_agents")).
		WillReturnRows(sqlmock.NewRows([]string{"fund_id", "max_active_agents", "max_concurrent_workflows", "daily_llm_token_limit", "monthly_llm_token_limit", "notes", "updated_at"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(fund_id::text, ''), max_active_agents")).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"fund_id", "max_active_agents", "max_concurrent_workflows", "daily_llm_token_limit", "monthly_llm_token_limit", "notes", "updated_at"}).
			AddRow("fund-1", sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{}, "", time.Now()))

	if err := s.CheckLLMTokens(context.Background(), "fund-1", 1000); err != nil {
		t.Fatalf("expected no error when caps absent, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestRecordLLMTokensUpsertsDailyRow verifies the SQL shape so a
// regression that drops the ON CONFLICT clause would surface a
// PK violation in production. We also assert the running totals
// add (don't replace) the existing row.
func TestRecordLLMTokensUpsertsDailyRow(t *testing.T) {
	s, mock, db := newServiceFromMock(t)
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO fund_llm_token_usage")).
		WithArgs("fund-1", "2026-05-18", int64(120), int64(80), int64(200)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.RecordLLMTokens(context.Background(), "fund-1", 120, 80); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestRecordLLMTokensRejectsNegative is the input-validation guard.
// A negative count from a buggy provider integration would corrupt
// the rolling sum.
func TestRecordLLMTokensRejectsNegative(t *testing.T) {
	s, _, db := newServiceFromMock(t)
	defer db.Close()
	if err := s.RecordLLMTokens(context.Background(), "fund-1", -1, 0); err == nil {
		t.Fatal("expected error on negative tokens")
	}
}

// TestCheckConcurrentWorkflowsRejects mirrors the agent case but for
// workflow_runs. Schedulers and admin manual-triggers both depend on
// this; a regression would let a runaway scheduler queue dozens of
// workflows for the same fund.
func TestCheckConcurrentWorkflowsRejects(t *testing.T) {
	s, mock, db := newServiceFromMock(t)
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(fund_id::text, ''), max_active_agents")).
		WillReturnRows(sqlmock.NewRows([]string{"fund_id", "max_active_agents", "max_concurrent_workflows", "daily_llm_token_limit", "monthly_llm_token_limit", "notes", "updated_at"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(fund_id::text, ''), max_active_agents")).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"fund_id", "max_active_agents", "max_concurrent_workflows", "daily_llm_token_limit", "monthly_llm_token_limit", "notes", "updated_at"}).
			AddRow("fund-1", sql.NullInt64{}, sql.NullInt64{Int64: 2, Valid: true}, sql.NullInt64{}, sql.NullInt64{}, "", time.Now()))

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM workflow_runs")).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(2))

	err := s.CheckConcurrentWorkflows(context.Background(), "fund-1")
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected ErrQuotaExceeded, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
