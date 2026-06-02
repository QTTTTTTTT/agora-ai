package modelab

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/llm"
)

func TestReporter_ExtractField(t *testing.T) {
	r := NewReporter(nil)

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain stance", `{"stance":"bullish"}`, "bullish"},
		{"trimmed", `{"stance":"  buy  "}`, "buy"},
		{"missing", `{"verdict":"buy"}`, ""},
		{"non-string", `{"stance":42}`, ""},
		{"non-json", `not json`, ""},
		{"empty", ``, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := r.ExtractField(json.RawMessage(c.in))
			if got != c.want {
				t.Fatalf("ExtractField(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestReporter_Compute_HappyPath(t *testing.T) {
	db, mock := openMock(t)
	defer db.Close()

	expID := "00000000-0000-0000-0000-000000000010"
	armsJSON, _ := MarshalArms([]ArmConfig{
		{Name: "ctrl", Provider: llm.ProviderOpenAI, ModelName: "gpt-4o", ModelTier: llm.TierCritical},
		{Name: "treat", Provider: llm.ProviderClaude, ModelName: "claude-opus", ModelTier: llm.TierCritical},
	})

	// 1) GetExperiment.
	mock.ExpectQuery(`FROM model_ab_experiments\s+WHERE id =`).
		WithArgs(expID).
		WillReturnRows(experimentRows().AddRow(
			expID, "ab",
			"", string(ScopeGlobal), "",
			stringArray(),
			armsJSON,
			float64Array(0.5, 0.5),
			string(StatusRunning),
			time.Now().Add(-2*time.Hour), time.Time{},
			int64(0), int64(0), "",
			time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour),
		))

	// 2) listAssignmentsInWindow — 3 calls on arm 0, 2 on arm 1.
	assignRows := sqlmock.NewRows([]string{
		"id", "experiment_id", "run_id", "step", "agent_id", "fund_id", "arm_index", "arm_name", "assigned_at",
	}).
		AddRow("a1", expID, "r1", "pm_decision", "ag", "f", 0, "ctrl", time.Now()).
		AddRow("a2", expID, "r2", "pm_decision", "ag", "f", 0, "ctrl", time.Now()).
		AddRow("a3", expID, "r3", "pm_decision", "ag", "f", 0, "ctrl", time.Now()).
		AddRow("a4", expID, "r4", "pm_decision", "ag", "f", 1, "treat", time.Now()).
		AddRow("a5", expID, "r5", "pm_decision", "ag", "f", 1, "treat", time.Now())
	mock.ExpectQuery(`FROM model_ab_assignments\s+WHERE experiment_id`).
		WithArgs(expID).
		WillReturnRows(assignRows)

	// 3) listShadowsInWindow — 2 shadow rows on arm 1 (one ok, one error)
	parsedOK := []byte(`{"stance":"bullish"}`)
	shadowRows := sqlmock.NewRows([]string{
		"id", "experiment_id", "assignment_id", "run_id", "step",
		"agent_id", "fund_id",
		"arm_index", "arm_name", "arm_model",
		"raw_output", "parsed_output", "parse_error",
		"input_tokens", "output_tokens",
		"latency_ms", "cost_micro",
		"error_text", "finished_at",
	}).
		AddRow("s1", expID, "a4", "r4", "pm_decision",
			"ag", "f",
			1, "treat", "claude/claude-opus",
			`{"stance":"bullish"}`, parsedOK, "",
			10, 20,
			1000, 5_000,
			"", time.Now()).
		AddRow("s2", expID, "a5", "r5", "pm_decision",
			"ag", "f",
			1, "treat", "claude/claude-opus",
			"", nil, "",
			0, 0,
			0, 0,
			"timeout", time.Now())
	mock.ExpectQuery(`FROM model_ab_shadow_responses\s+WHERE experiment_id`).
		WithArgs(expID).
		WillReturnRows(shadowRows)

	r := NewReporter(NewRepo(db))
	rep, err := r.Compute(context.Background(), expID, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(rep.Arms) != 2 {
		t.Fatalf("expected 2 arms, got %d", len(rep.Arms))
	}
	if rep.Arms[0].PrimaryCount != 3 {
		t.Errorf("arm 0 PrimaryCount = %d, want 3", rep.Arms[0].PrimaryCount)
	}
	if rep.Arms[1].PrimaryCount != 2 {
		t.Errorf("arm 1 PrimaryCount = %d, want 2", rep.Arms[1].PrimaryCount)
	}
	if rep.Arms[1].ShadowCount != 2 {
		t.Errorf("arm 1 ShadowCount = %d, want 2", rep.Arms[1].ShadowCount)
	}
	if rep.Arms[1].ErrorCount != 1 {
		t.Errorf("arm 1 ErrorCount = %d, want 1", rep.Arms[1].ErrorCount)
	}
	if rep.Arms[1].AvgLatencyMs != 1000 {
		t.Errorf("arm 1 AvgLatencyMs = %d, want 1000", rep.Arms[1].AvgLatencyMs)
	}
	if rep.Arms[1].TotalOutTok != 20 {
		t.Errorf("arm 1 TotalOutTok = %d, want 20", rep.Arms[1].TotalOutTok)
	}
	if rep.Arms[1].TotalCostMicr != 5_000 {
		t.Errorf("arm 1 TotalCostMicr = %d, want 5000", rep.Arms[1].TotalCostMicr)
	}
	if rep.Experiment.ID != expID {
		t.Errorf("experiment ID = %q, want %q", rep.Experiment.ID, expID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}
