package repository

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func newOverrideRepoMock(t *testing.T) (*FundLLMOverrideRepo, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewFundLLMOverrideRepo(db), mock
}

func TestFundLLMOverrideRow_Specificity(t *testing.T) {
	row := FundLLMOverrideRow{
		AgentID:   uuid.NullUUID{Valid: true, UUID: uuid.New()},
		Role:      sql.NullString{Valid: true, String: "pm"},
		ModelTier: sql.NullString{Valid: true, String: "critical"},
		Label:     sql.NullString{Valid: true, String: "openai-prod"},
	}
	if s := row.Specificity(); s != 8+4+2+1 {
		t.Fatalf("full-specificity score = 15, got %d", s)
	}
	row2 := FundLLMOverrideRow{}
	if s := row2.Specificity(); s != 0 {
		t.Fatalf("all-wildcard score = 0, got %d", s)
	}
	// agent-only > tier-only.
	row3 := FundLLMOverrideRow{
		AgentID: uuid.NullUUID{Valid: true, UUID: uuid.New()},
	}
	row4 := FundLLMOverrideRow{
		ModelTier: sql.NullString{Valid: true, String: "critical"},
	}
	if row3.Specificity() <= row4.Specificity() {
		t.Fatalf("agent should rank higher than tier: %d vs %d",
			row3.Specificity(), row4.Specificity())
	}
}

func TestNewFundLLMOverrideRepo_NilDB(t *testing.T) {
	if NewFundLLMOverrideRepo(nil) != nil {
		t.Fatalf("expected nil repo")
	}
}

func TestFundLLMOverrideRepo_Upsert_RejectsMissingProvider(t *testing.T) {
	repo, mock := newOverrideRepoMock(t)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM platform_llm_providers WHERE provider")).
		WithArgs("openai").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	_, err := repo.Upsert(context.Background(), UpsertParams{
		FundID:   uuid.New(),
		Provider: "openai",
		Enabled:  true,
	})
	if !errors.Is(err, ErrFundLLMOverrideInvalidProvider) {
		t.Fatalf("expected ErrFundLLMOverrideInvalidProvider, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestFundLLMOverrideRepo_Upsert_RejectsMissingProviderLabel(t *testing.T) {
	repo, mock := newOverrideRepoMock(t)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM platform_llm_providers")).
		WithArgs("openai", "missing-label").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	_, err := repo.Upsert(context.Background(), UpsertParams{
		FundID:   uuid.New(),
		Provider: "openai",
		Label:    "missing-label",
		Enabled:  true,
	})
	if !errors.Is(err, ErrFundLLMOverrideInvalidProvider) {
		t.Fatalf("expected provider err, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestFundLLMOverrideRepo_Upsert_CreateHappyPath(t *testing.T) {
	repo, mock := newOverrideRepoMock(t)
	fundID := uuid.New()
	agentID := uuid.New()
	newID := uuid.New()
	now := time.Now()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM platform_llm_providers")).
		WithArgs("openai", "openai-prod").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO fund_llm_overrides")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(newID))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE id = $1")).
		WithArgs(newID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "agent_id", "role", "model_tier",
			"provider", "label", "model_name",
			"enabled", "note",
			"created_at", "updated_at", "created_by", "updated_by",
		}).
			AddRow(newID, fundID, agentID, sql.NullString{Valid: true, String: "pm"},
				sql.NullString{Valid: true, String: "critical"},
				"openai", sql.NullString{Valid: true, String: "openai-prod"},
				sql.NullString{Valid: true, String: "gpt-4o"},
				true, nil,
				now, now, nil, nil))

	row, err := repo.Upsert(context.Background(), UpsertParams{
		FundID:    fundID,
		AgentID:   &agentID,
		Role:      "pm",
		ModelTier: "critical",
		Provider:  "openai",
		Label:     "openai-prod",
		ModelName: "gpt-4o",
		Enabled:   true,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if row.ID != newID {
		t.Fatalf("unexpected id: %v", row.ID)
	}
	if row.Specificity() != 15 {
		t.Fatalf("expected full specificity, got %d", row.Specificity())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestFundLLMOverrideRepo_Upsert_UpdateHappyPath(t *testing.T) {
	repo, mock := newOverrideRepoMock(t)
	fundID := uuid.New()
	rowID := uuid.New()
	now := time.Now()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM platform_llm_providers WHERE provider")).
		WithArgs("claude").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE fund_llm_overrides")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE id = $1")).
		WithArgs(rowID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "agent_id", "role", "model_tier",
			"provider", "label", "model_name",
			"enabled", "note",
			"created_at", "updated_at", "created_by", "updated_by",
		}).AddRow(rowID, fundID, nil, nil, nil,
			"claude", nil, nil, true, nil, now, now, nil, nil))

	row, err := repo.Upsert(context.Background(), UpsertParams{
		ID:       &rowID,
		FundID:   fundID,
		Provider: "claude",
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if row.Provider != "claude" {
		t.Fatalf("got provider %q", row.Provider)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestFundLLMOverrideRepo_Upsert_UpdateNotFound(t *testing.T) {
	repo, mock := newOverrideRepoMock(t)
	fundID := uuid.New()
	missingID := uuid.New()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM platform_llm_providers WHERE provider")).
		WithArgs("openai").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE fund_llm_overrides")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	_, err := repo.Upsert(context.Background(), UpsertParams{
		ID:       &missingID,
		FundID:   fundID,
		Provider: "openai",
		Enabled:  true,
	})
	if !errors.Is(err, ErrFundLLMOverrideNotFound) {
		t.Fatalf("expected not_found, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestFundLLMOverrideRepo_Delete_NotFound(t *testing.T) {
	repo, mock := newOverrideRepoMock(t)
	id := uuid.New()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM fund_llm_overrides")).
		WithArgs(id).
		WillReturnResult(sqlmock.NewResult(0, 0))
	err := repo.Delete(context.Background(), id)
	if !errors.Is(err, ErrFundLLMOverrideNotFound) {
		t.Fatalf("expected not_found, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestFundLLMOverrideRepo_Get_NotFound(t *testing.T) {
	repo, mock := newOverrideRepoMock(t)
	id := uuid.New()
	mock.ExpectQuery(regexp.QuoteMeta("WHERE id = $1")).
		WithArgs(id).
		WillReturnError(sql.ErrNoRows)
	_, err := repo.Get(context.Background(), id)
	if !errors.Is(err, ErrFundLLMOverrideNotFound) {
		t.Fatalf("expected not_found, got %v", err)
	}
}

func TestFundLLMOverrideRepo_ListByFund(t *testing.T) {
	repo, mock := newOverrideRepoMock(t)
	fundID := uuid.New()
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta("WHERE fund_id = $1")).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "agent_id", "role", "model_tier",
			"provider", "label", "model_name",
			"enabled", "note",
			"created_at", "updated_at", "created_by", "updated_by",
		}).
			AddRow(uuid.New(), fundID, nil, nil, nil, "openai", nil, nil, true, nil, now, now, nil, nil).
			AddRow(uuid.New(), fundID, uuid.New(),
				sql.NullString{Valid: true, String: "pm"}, nil,
				"claude", nil, nil, true, nil, now, now, nil, nil))
	rows, err := repo.ListByFund(context.Background(), fundID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
}

func TestFundLLMOverrideRepo_ResolveForRequest_NoMatch(t *testing.T) {
	repo, mock := newOverrideRepoMock(t)
	fundID := uuid.New()
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY")).
		WithArgs(fundID, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)
	row, err := repo.ResolveForRequest(context.Background(), fundID, nil, "", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if row != nil {
		t.Fatalf("expected nil row for no match, got %+v", row)
	}
}

func TestFundLLMOverrideRepo_ResolveForRequest_NilFund(t *testing.T) {
	repo, _ := newOverrideRepoMock(t)
	row, err := repo.ResolveForRequest(context.Background(), uuid.Nil, nil, "", "")
	if err != nil || row != nil {
		t.Fatalf("want nil,nil for empty fund id; got %+v err=%v", row, err)
	}
}

func TestFundLLMOverrideRepo_ResolveForRequest_HappyPath(t *testing.T) {
	repo, mock := newOverrideRepoMock(t)
	fundID := uuid.New()
	agentID := uuid.New()
	hitID := uuid.New()
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY")).
		WithArgs(fundID, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "agent_id", "role", "model_tier",
			"provider", "label", "model_name",
			"enabled", "note",
			"created_at", "updated_at", "created_by", "updated_by",
		}).AddRow(hitID, fundID, agentID,
			sql.NullString{Valid: true, String: "pm"},
			sql.NullString{Valid: true, String: "critical"},
			"openai", sql.NullString{Valid: true, String: "openai-prod"},
			sql.NullString{Valid: true, String: "gpt-4o"},
			true, nil, now, now, nil, nil))
	row, err := repo.ResolveForRequest(context.Background(), fundID, &agentID, "pm", "critical")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if row == nil || row.ID != hitID {
		t.Fatalf("unexpected row: %+v", row)
	}
	if row.Specificity() != 15 {
		t.Fatalf("expected max specificity, got %d", row.Specificity())
	}
}

func TestFundLLMOverrideRepo_ListAllEnabled(t *testing.T) {
	repo, mock := newOverrideRepoMock(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta("WHERE enabled = TRUE")).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "agent_id", "role", "model_tier",
			"provider", "label", "model_name",
			"enabled", "note",
			"created_at", "updated_at", "created_by", "updated_by",
		}).AddRow(uuid.New(), uuid.New(), nil, nil, nil,
			"openai", nil, nil, true, nil, now, now, nil, nil))
	rows, err := repo.ListAllEnabled(context.Background())
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
}
