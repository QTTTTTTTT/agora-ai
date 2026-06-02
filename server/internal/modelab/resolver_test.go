package modelab

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/llm"
)

func openMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock open: %v", err)
	}
	return db, mock
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func stubArms() []ArmConfig {
	return []ArmConfig{
		{Name: "control", Provider: llm.ProviderOpenAI, ModelName: "gpt-4o", ModelTier: llm.TierCritical},
		{Name: "treat", Provider: llm.ProviderClaude, ModelName: "claude-opus", ModelTier: llm.TierCritical},
	}
}

func experimentRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "name", "description", "scope", "scope_target",
		"step_filter", "arms", "traffic_split",
		"status", "start_at", "end_at",
		"max_total_tokens", "tokens_used", "created_by",
		"created_at", "updated_at",
	})
}

func TestResolver_NoMatch_DegradesGracefully(t *testing.T) {
	db, mock := openMock(t)
	defer db.Close()
	mock.ExpectQuery(`FROM model_ab_experiments\s+WHERE status = ANY`).
		WillReturnRows(experimentRows())

	r := NewResolver(NewRepo(db))
	r.Logger = discardLogger()
	d := r.Resolve(context.Background(), "f1", "ag-1", "pm", "pm_decision", "run-1")
	if d.InExperiment {
		t.Fatalf("expected no experiment match, got %+v", d)
	}
}

func TestResolver_HappyPath_PicksArmAndPersistsAssignment(t *testing.T) {
	db, mock := openMock(t)
	defer db.Close()
	armsJSON, _ := MarshalArms(stubArms())

	rows := experimentRows().AddRow(
		"00000000-0000-0000-0000-000000000001",
		"claude vs gpt", "",
		string(ScopeAgentRole), "pm",
		stringArray(),
		armsJSON,
		float64Array(0.5, 0.5),
		string(StatusRunning),
		time.Now().Add(-1*time.Hour), time.Time{},
		int64(0), int64(0), "",
		time.Now().Add(-1*time.Hour), time.Now().Add(-1*time.Hour),
	)
	mock.ExpectQuery(`FROM model_ab_experiments\s+WHERE status = ANY`).WillReturnRows(rows)

	mock.ExpectQuery(`INSERT INTO model_ab_assignments`).
		WithArgs(
			"00000000-0000-0000-0000-000000000001",
			"run-1", "pm_decision", "agent-7", sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnRows(sqlmock.NewRows([]string{"id", "arm_index", "arm_name", "assigned_at"}).
			AddRow("00000000-0000-0000-0000-000000000099", 0, "control", time.Now()))

	r := NewResolver(NewRepo(db))
	r.Logger = discardLogger()
	d := r.Resolve(context.Background(), "fund-1", "agent-7", "pm", "pm_decision", "run-1")
	if !d.InExperiment {
		t.Fatalf("expected to be in experiment, got %+v", d)
	}
	if d.Experiment == nil || d.Experiment.Name != "claude vs gpt" {
		t.Fatalf("experiment not loaded: %+v", d.Experiment)
	}
	if d.Assignment == nil || d.Assignment.ID == "" {
		t.Fatalf("assignment not persisted: %+v", d.Assignment)
	}
	if d.Arm.ModelName == "" {
		t.Fatalf("arm config not selected: %+v", d.Arm)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

func TestResolver_BudgetExhausted_SkipsExperiment(t *testing.T) {
	db, mock := openMock(t)
	defer db.Close()
	armsJSON, _ := MarshalArms(stubArms())

	rows := experimentRows().AddRow(
		"00000000-0000-0000-0000-000000000001",
		"exhausted", "",
		string(ScopeGlobal), "",
		stringArray(),
		armsJSON,
		float64Array(0.5, 0.5),
		string(StatusRunning),
		time.Time{}, time.Time{},
		int64(1000), int64(1500), "",
		time.Now(), time.Now(),
	)
	mock.ExpectQuery(`FROM model_ab_experiments\s+WHERE status = ANY`).WillReturnRows(rows)

	r := NewResolver(NewRepo(db))
	r.Logger = discardLogger()
	d := r.Resolve(context.Background(), "fund-1", "agent-7", "pm", "pm_decision", "run-1")
	if d.InExperiment {
		t.Fatalf("budget exhausted experiment should be skipped, got %+v", d)
	}
}

func TestResolver_Invalidate_ForcesRefresh(t *testing.T) {
	db, mock := openMock(t)
	defer db.Close()
	mock.ExpectQuery(`FROM model_ab_experiments\s+WHERE status = ANY`).WillReturnRows(experimentRows())
	mock.ExpectQuery(`FROM model_ab_experiments\s+WHERE status = ANY`).WillReturnRows(experimentRows())

	r := NewResolver(NewRepo(db))
	r.RefreshInterval = 1 * time.Hour
	r.Logger = discardLogger()
	_ = r.Resolve(context.Background(), "f1", "a1", "pm", "step", "run-1")
	r.Invalidate()
	_ = r.Resolve(context.Background(), "f1", "a1", "pm", "step", "run-1")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("Invalidate did not force a second query: %v", err)
	}
}

func TestResolver_NilRepoIsNoOp(t *testing.T) {
	r := NewResolver(nil)
	d := r.Resolve(context.Background(), "f1", "a1", "pm", "step", "run-1")
	if d.InExperiment {
		t.Fatalf("nil repo must produce no-match decision")
	}
}

// --- pq.Array literal helpers for sqlmock --------------------------------

func stringArray(vs ...string) string {
	if len(vs) == 0 {
		return "{}"
	}
	out := "{"
	for i, v := range vs {
		if i > 0 {
			out += ","
		}
		out += "\"" + v + "\""
	}
	out += "}"
	return out
}

func float64Array(vs ...float64) string {
	if len(vs) == 0 {
		return "{}"
	}
	out := "{"
	for i, v := range vs {
		if i > 0 {
			out += ","
		}
		out += strconv.FormatFloat(v, 'f', -1, 64)
	}
	out += "}"
	return out
}
