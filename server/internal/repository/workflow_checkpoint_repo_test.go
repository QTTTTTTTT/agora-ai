package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newCheckpointRepo(t *testing.T) (*WorkflowCheckpointRepo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewWorkflowCheckpointRepo(db), mock, func() { _ = db.Close() }
}

func TestWorkflowCheckpointRepo_Upsert_HappyPath(t *testing.T) {
	r, mock, done := newCheckpointRepo(t)
	defer done()
	now := time.Now().UTC().Truncate(time.Second)
	cp := &WorkflowCheckpoint{
		RunID:       "run-1",
		FundID:      "fund-1",
		TradingDate: now,
		Step:        "pm_plan",
		Status:      "success",
		Attempts:    1,
		StartedAt:   now,
		EndedAt:     now.Add(2 * time.Second),
		DurationMs:  2000,
		Payload:     json.RawMessage(`{"planId":"plan-99"}`),
	}
	mock.ExpectQuery("INSERT INTO workflow_checkpoints").
		WithArgs(
			cp.RunID, cp.FundID, cp.TradingDate, cp.Step, cp.Status, cp.Attempts,
			cp.StartedAt, cp.EndedAt, cp.DurationMs, sql.NullString{}, cp.Payload,
		).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_id", "fund_id", "trading_date", "step", "status", "attempts",
			"started_at", "ended_at", "duration_ms", "error_text", "payload",
			"created_at", "updated_at",
		}).AddRow(
			"cp-1", cp.RunID, cp.FundID, cp.TradingDate, cp.Step, cp.Status, cp.Attempts,
			cp.StartedAt, cp.EndedAt, cp.DurationMs, sql.NullString{}, cp.Payload,
			now, now,
		))
	out, err := r.Upsert(context.Background(), cp)
	if err != nil {
		t.Fatal(err)
	}
	if out.ID != "cp-1" || out.Step != "pm_plan" {
		t.Errorf("unexpected out: %+v", out)
	}
}

func TestWorkflowCheckpointRepo_Upsert_RejectsBadStatus(t *testing.T) {
	r, _, done := newCheckpointRepo(t)
	defer done()
	_, err := r.Upsert(context.Background(), &WorkflowCheckpoint{
		RunID:  "r1",
		FundID: "f1",
		Step:   "pm_plan",
		Status: "bogus",
	})
	if err == nil {
		t.Fatal("expected error on invalid status")
	}
}

func TestWorkflowCheckpointRepo_Upsert_RejectsMissingIDs(t *testing.T) {
	r, _, done := newCheckpointRepo(t)
	defer done()
	if _, err := r.Upsert(context.Background(), &WorkflowCheckpoint{
		Step: "pm_plan", Status: "success",
	}); err == nil {
		t.Error("expected error on missing run/fund id")
	}
	if _, err := r.Upsert(context.Background(), &WorkflowCheckpoint{
		RunID: "r1", FundID: "f1", Status: "success",
	}); err == nil {
		t.Error("expected error on missing step")
	}
}

func TestWorkflowCheckpointRepo_ListByRun_HappyPath(t *testing.T) {
	r, mock, done := newCheckpointRepo(t)
	defer done()
	now := time.Now().UTC()
	mock.ExpectQuery("FROM workflow_checkpoints").
		WithArgs("run-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_id", "fund_id", "trading_date", "step", "status", "attempts",
			"started_at", "ended_at", "duration_ms", "error_text", "payload",
			"created_at", "updated_at",
		}).AddRow(
			"cp-1", "run-1", "fund-1", now, "macro_brief", "success", 1,
			now, now, int64(100), sql.NullString{}, json.RawMessage(`{}`),
			now, now,
		).AddRow(
			"cp-2", "run-1", "fund-1", now, "pm_plan", "failed", 2,
			now, now, int64(2500), sql.NullString{String: "boom", Valid: true}, json.RawMessage(`{"reason":"llm_timeout"}`),
			now, now,
		))
	out, err := r.ListByRun(context.Background(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 rows, got %d", len(out))
	}
	if out[1].Step != "pm_plan" || out[1].ErrorText != "boom" {
		t.Errorf("unexpected row[1]: %+v", out[1])
	}
}

func TestWorkflowCheckpointRepo_ListByRun_RejectsMissingRunID(t *testing.T) {
	r, _, done := newCheckpointRepo(t)
	defer done()
	if _, err := r.ListByRun(context.Background(), ""); err == nil {
		t.Error("expected error on missing run_id")
	}
}

func TestWorkflowCheckpointRepo_GetLatestFailedOrPaused_HappyPath(t *testing.T) {
	r, mock, done := newCheckpointRepo(t)
	defer done()
	now := time.Now().UTC()
	mock.ExpectQuery("FROM workflow_checkpoints").
		WithArgs("run-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_id", "fund_id", "trading_date", "step", "status", "attempts",
			"started_at", "ended_at", "duration_ms", "error_text", "payload",
			"created_at", "updated_at",
		}).AddRow(
			"cp-2", "run-1", "fund-1", now, "pm_plan", "failed", 2,
			now, now, int64(2500), sql.NullString{String: "boom", Valid: true}, json.RawMessage(`{}`),
			now, now,
		))
	out, err := r.GetLatestFailedOrPaused(context.Background(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if out.Step != "pm_plan" || out.Status != "failed" {
		t.Errorf("unexpected: %+v", out)
	}
}

func TestWorkflowCheckpointRepo_GetLatestFailedOrPaused_NoRowsBubbles(t *testing.T) {
	r, mock, done := newCheckpointRepo(t)
	defer done()
	mock.ExpectQuery("FROM workflow_checkpoints").
		WithArgs("run-empty").
		WillReturnError(sql.ErrNoRows)
	_, err := r.GetLatestFailedOrPaused(context.Background(), "run-empty")
	if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestIsCheckpointStatus(t *testing.T) {
	for _, s := range []string{"success", "failed", "skipped", "pending", "paused"} {
		if !isCheckpointStatus(s) {
			t.Errorf("%s should be valid", s)
		}
	}
	for _, s := range []string{"", "running", "complete", "ok"} {
		if isCheckpointStatus(s) {
			t.Errorf("%s should be invalid", s)
		}
	}
}
