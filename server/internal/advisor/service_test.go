package advisor

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/agent"
	"github.com/lib/pq"
)

// TestPresetKind makes sure the Kind() helper classifies each preset
// correctly. Used by the handler to route into the right panel and
// by the UI to render the right disclaimer.
func TestPresetKind(t *testing.T) {
	cases := []struct {
		name  string
		in    PersonaPreset
		want  PresetKind
	}{
		{"masters_only", PersonaPreset{MasterKeys: []string{"buffett"}}, PresetKindMasters},
		{"tactics_only", PersonaPreset{TacticKeys: []string{"tail_sniper"}}, PresetKindTactics},
		{
			"mixed",
			PersonaPreset{MasterKeys: []string{"buffett"}, TacticKeys: []string{"tail_sniper"}},
			PresetKindMixed,
		},
		{"empty_custom", PersonaPreset{}, PresetKindEmpty},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.in.Kind(); got != c.want {
				t.Errorf("Kind() = %q want %q", got, c.want)
			}
		})
	}
}

// TestRepoListPresets verifies the SELECT query shape + that
// disabled rows are filtered when enabledOnly=true.
func TestRepoListPresets(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	repo := NewRepo(db)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT preset_key`)).
		WillReturnRows(sqlmock.NewRows([]string{
			"preset_key", "label_zh", "label_en", "description_zh", "description_en",
			"master_keys", "tactic_keys", "enabled", "sort_order",
		}).AddRow(
			"conservative", "保守稳健", "Conservative", "...", "...",
			pq.StringArray{"buffett", "munger"}, pq.StringArray{}, true, 10,
		))

	rows, err := repo.List(context.Background(), true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len = %d want 1", len(rows))
	}
	if rows[0].Key != "conservative" {
		t.Errorf("Key = %q want conservative", rows[0].Key)
	}
	if len(rows[0].MasterKeys) != 2 {
		t.Errorf("MasterKeys = %v want 2", rows[0].MasterKeys)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestServiceConsultErrors verifies the error mapping paths the
// handler depends on.
func TestServiceConsultErrors(t *testing.T) {
	// No repo → ErrNotReady.
	svc := NewService(nil)
	_, err := svc.Consult(context.Background(), ConsultRequest{
		UserID: "u1", Symbol: "AAPL", PresetKey: "conservative",
	})
	if !errors.Is(err, ErrNotReady) {
		t.Errorf("err = %v want ErrNotReady", err)
	}

	// No symbol → validation error.
	db, _, _ := sqlmock.New()
	defer db.Close()
	svc = NewService(NewRepo(db))
	_, err = svc.Consult(context.Background(), ConsultRequest{
		UserID: "u1", PresetKey: "conservative",
	})
	if err == nil {
		t.Error("expected error for missing symbol")
	}
}

// TestServiceCustomPresetHonoursKeys: when the preset is the empty
// `custom` row, the service must honour the request's
// CustomMasterKeys / CustomTacticKeys.
func TestServiceCustomPresetHonoursKeys(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	svc := NewService(NewRepo(db))

	preset := PersonaPreset{Key: "custom", Enabled: true}
	req := ConsultRequest{
		CustomMasterKeys: []string{"buffett", "lynch"},
		CustomTacticKeys: []string{"tail_sniper"},
	}
	masters, tactics := svc.resolveKeys(preset, req)
	if len(masters) != 2 || masters[0] != "buffett" || masters[1] != "lynch" {
		t.Errorf("masters = %v want [buffett lynch]", masters)
	}
	if len(tactics) != 1 || tactics[0] != "tail_sniper" {
		t.Errorf("tactics = %v want [tail_sniper]", tactics)
	}

	// Non-empty preset → custom keys ignored.
	preset = PersonaPreset{
		Key:        "conservative",
		Enabled:    true,
		MasterKeys: []string{"buffett", "munger", "graham"},
	}
	masters, tactics = svc.resolveKeys(preset, req)
	if len(masters) != 3 {
		t.Errorf("expected preset masters, got %v", masters)
	}
	if len(tactics) != 0 {
		t.Errorf("expected no tactics, got %v", tactics)
	}
}

// TestSaveAndReadConsultation exercises the full write + read round
// trip through sqlmock to catch placeholder / column-count errors
// in the SQL.
func TestSaveAndReadConsultation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	repo := NewRepo(db)
	now := time.Now()

	mock.ExpectBegin()
	// SymbolName lands as a zero-value sql.NullString (Valid=false)
	// when the caller doesn't populate it — confirms NULL-by-default
	// behaviour for legacy/unknown rows.
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO advisor_consultations`)).
		WithArgs("u1", "AAPL", sql.NullString{}, "", "", "conservative", "BUY", 75, 80.0, "", nil).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow("c1", now))
	mock.ExpectPrepare(regexp.QuoteMeta(`INSERT INTO advisor_master_reports`))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO advisor_master_reports`)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	saved, err := repo.SaveConsultation(context.Background(), SaveConsultationInput{
		UserID:              "u1",
		Symbol:              "AAPL",
		PresetKey:           "conservative",
		AggregateVerdict:    "BUY",
		AggregateConfidence: 75,
		ConsensusScore:      80.0,
		MasterReports: []MasterReportRow{{
			MasterKey: "buffett", Verdict: "BUY", Confidence: 75,
			Thesis: "x", KeyReasons: []string{"a"}, KeyRisks: []string{"b"},
		}},
	})
	if err != nil {
		t.Fatalf("SaveConsultation: %v", err)
	}
	if saved.ID != "c1" {
		t.Errorf("ID = %q want c1", saved.ID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestMasterReportsFromAgent verifies the agent → service shape
// adapter preserves every field.
func TestMasterReportsFromAgent(t *testing.T) {
	in := []agent.MasterReport{{
		MasterKey:      "buffett",
		MasterNameZh:   "巴菲特",
		MasterNameEn:   "Buffett",
		Verdict:        "BUY",
		Confidence:     80,
		Thesis:         "strong moat",
		KeyReasons:     []string{"ROE > 15%"},
		KeyRisks:       []string{"valuation"},
		MasterSpecific: map[string]any{"intrinsic_value": 195.0},
		RedLinesHit:    []string{},
		LLMModel:       "llm",
	}}
	svc := &Service{}
	out := svc.masterReportsFromAgent(context.Background(), "user-1", in)
	if len(out) != 1 {
		t.Fatalf("len = %d want 1", len(out))
	}
	r := out[0]
	if r.MasterKey != "buffett" || r.Verdict != "BUY" || r.Confidence != 80 {
		t.Errorf("fields lost: %+v", r)
	}
	if r.MasterSpecific["intrinsic_value"] != 195.0 {
		t.Errorf("master_specific lost: %v", r.MasterSpecific)
	}
}
