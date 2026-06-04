package alphalesson

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/agentreputation"
)

func newMockRepo(t *testing.T) (*Repo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewRepo(db), mock, func() { _ = db.Close() }
}

// TestWriteOptions_Normalize asserts the post-AP2 contract:
// every field except Visibility is defaulted by normalize(),
// because Visibility is deliberately left empty so the
// per-outcome resolver (defaultVisibilityForKind) can pick
// agent_portable for researcher / analyst lessons. Pre-AP2 this
// test asserted normalize().Visibility == "fund", which forced
// every lesson into fund-private regardless of AgentKind.
func TestWriteOptions_Normalize(t *testing.T) {
	o := WriteOptions{}.normalize()
	if o.AlphaThreshold <= 0 {
		t.Errorf("threshold = %v", o.AlphaThreshold)
	}
	if o.Layer == "" {
		t.Error("layer empty")
	}
	// AP2 contract change: empty Visibility is the SIGNAL that
	// the per-outcome resolver should run. normalize() must
	// preserve it. A regression that re-introduced the "fund"
	// default would silently make every researcher lesson
	// fund-private again.
	if o.Visibility != "" {
		t.Errorf("visibility should stay empty after normalize so per-outcome resolver runs, got %q", o.Visibility)
	}
	if o.OriginKind == "" {
		t.Error("origin empty")
	}
}

// TestWriteOptions_NormalizePreservesExplicit asserts that a
// non-empty Visibility passed by the caller survives
// normalize() unchanged — this is the override path for tests
// and operator-driven force-fund-private backfills.
func TestWriteOptions_NormalizePreservesExplicit(t *testing.T) {
	o := WriteOptions{Visibility: "fund"}.normalize()
	if o.Visibility != "fund" {
		t.Errorf("explicit visibility lost: %q", o.Visibility)
	}
	o2 := WriteOptions{Visibility: "agent_portable"}.normalize()
	if o2.Visibility != "agent_portable" {
		t.Errorf("explicit agent_portable lost: %q", o2.Visibility)
	}
}

// TestDefaultVisibilityForKind pins the per-AgentKind mapping.
// The table is the load-bearing decision for AP2's behaviour —
// a regression that flipped pm to agent_portable would leak
// portfolio-construction lessons across funds, and a regression
// that flipped researcher to fund would silently undo the
// whole feature.
func TestDefaultVisibilityForKind(t *testing.T) {
	cases := []struct {
		kind agentreputation.AgentKind
		want string
	}{
		{agentreputation.KindResearcher, "agent_portable"},
		{agentreputation.KindAnalyst, "agent_portable"},
		{agentreputation.KindPM, "fund"},
		{agentreputation.KindAdvocate, "fund"},
		{agentreputation.AgentKind(""), "fund"},
		{agentreputation.AgentKind("bogus"), "fund"},
	}
	for _, tc := range cases {
		if got := defaultVisibilityForKind(tc.kind); got != tc.want {
			t.Errorf("defaultVisibilityForKind(%q) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// TestNullableUUID covers the AP2 helper that gates which
// agent_tag values get propagated into the agent_id FK column.
// Wrong gating here would either (a) push non-UUID tags into a
// UUID column and break the INSERT for the whole batch, or (b)
// silently downgrade legit UUIDs to NULL and exclude them from
// the cross-fund retrieval. Both regressions are worth pinning.
func TestNullableUUID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"whitespace", "   ", false},
		{"tag-style string", "fund_analyst", false},
		{"role-style string", "bull_researcher", false},
		{"truncated uuid", "12345678-1234-1234-1234", false},
		{"canonical uuid", "12345678-1234-1234-1234-1234567890ab", true},
		{"uppercase uuid", "12345678-1234-1234-1234-1234567890AB", true},
		{"uuid with surrounding whitespace", "  12345678-1234-1234-1234-1234567890ab  ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nullableUUID(tc.in)
			if got.Valid != tc.want {
				t.Errorf("nullableUUID(%q).Valid = %v, want %v", tc.in, got.Valid, tc.want)
			}
		})
	}
}

func TestWriteAlphaLessons_NilRepo(t *testing.T) {
	var r *Repo
	if _, err := r.WriteAlphaLessons(context.Background(), nil, WriteOptions{}); err == nil {
		t.Error("expected nil-repo error")
	}
}

func TestWriteAlphaLessons_EmptyOK(t *testing.T) {
	r, _, done := newMockRepo(t)
	defer done()
	if n, err := r.WriteAlphaLessons(context.Background(), nil, WriteOptions{}); err != nil || n != 0 {
		t.Errorf("empty: n=%d err=%v", n, err)
	}
}

func TestWriteAlphaLessons_HappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	asof := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	out := agentreputation.Outcome{
		ID: "o1", FundID: "f1", AgentID: "fund_analyst", AgentName: "F",
		AgentKind: agentreputation.KindAnalyst, Category: "fundamentals",
		Symbol: "AAPL", AsOf: asof,
		Direction: agentreputation.DirBullish, Confidence: 65,
		RealisedReturn: 0.04, BenchmarkReturn: 0.01, Alpha: 0.03,
		HorizonDays: 5,
	}
	mock.ExpectBegin()
	mock.ExpectPrepare("INSERT INTO memories")
	mock.ExpectQuery("FROM memories WHERE source_outcome_id").
		WithArgs("o1").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("INSERT INTO memories").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	n, err := r.WriteAlphaLessons(context.Background(), []agentreputation.Outcome{out}, WriteOptions{})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 1 {
		t.Errorf("want 1, got %d", n)
	}
}

func TestWriteAlphaLessons_BelowThresholdSkipped(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	out := agentreputation.Outcome{
		ID: "o1", FundID: "f1", AgentID: "a", AgentKind: agentreputation.KindAnalyst,
		Direction: agentreputation.DirBullish, Symbol: "AAPL", Confidence: 50,
		Alpha: 0.001, AsOf: time.Now(),
	}
	mock.ExpectBegin()
	mock.ExpectPrepare("INSERT INTO memories")
	mock.ExpectCommit()
	n, err := r.WriteAlphaLessons(context.Background(), []agentreputation.Outcome{out}, WriteOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 lessons written for sub-threshold alpha, got %d", n)
	}
}

func TestWriteAlphaLessons_DedupSkipsExisting(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	out := agentreputation.Outcome{
		ID: "o-dupe", FundID: "f1", AgentID: "a", AgentKind: agentreputation.KindAnalyst,
		Direction: agentreputation.DirBullish, Symbol: "AAPL", Confidence: 60,
		Alpha: 0.03, AsOf: time.Now(),
	}
	mock.ExpectBegin()
	mock.ExpectPrepare("INSERT INTO memories")
	mock.ExpectQuery("FROM memories WHERE source_outcome_id").
		WithArgs("o-dupe").
		WillReturnRows(sqlmock.NewRows([]string{"?"}).AddRow(1))
	mock.ExpectCommit()
	n, err := r.WriteAlphaLessons(context.Background(), []agentreputation.Outcome{out}, WriteOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 0 {
		t.Errorf("expected dedupe skip, got %d", n)
	}
}

// TestWriteAlphaLessons_VisibilityPerKindMatrix is the
// end-to-end pin on AP2: it verifies that the INSERT actually
// reaches Postgres with visibility='agent_portable' for a
// researcher outcome, visibility='fund' for a PM outcome, and
// that an explicit opts.Visibility override wins regardless of
// the AgentKind. Sub-test isolation means each row sees a fresh
// sqlmock so the arg-position pin is unambiguous.
//
// This test is the load-bearing assertion that the schema work
// from AP1 actually starts producing 'agent_portable' rows in
// production. Without it a refactor that broke the per-outcome
// resolver dispatch would compile and pass every other test in
// the package but silently regress to fund-private writes.
func TestWriteAlphaLessons_VisibilityPerKindMatrix(t *testing.T) {
	canonicalUUID := "11111111-2222-3333-4444-555555555555"
	cases := []struct {
		name           string
		kind           agentreputation.AgentKind
		explicit       string
		wantVisibility string
		wantAgentIDSet bool
	}{
		{
			name:           "researcher default → agent_portable",
			kind:           agentreputation.KindResearcher,
			wantVisibility: "agent_portable",
			wantAgentIDSet: true,
		},
		{
			name:           "analyst default → agent_portable",
			kind:           agentreputation.KindAnalyst,
			wantVisibility: "agent_portable",
			wantAgentIDSet: true,
		},
		{
			name:           "pm default → fund",
			kind:           agentreputation.KindPM,
			wantVisibility: "fund",
			wantAgentIDSet: true,
		},
		{
			name:           "advocate default → fund",
			kind:           agentreputation.KindAdvocate,
			wantVisibility: "fund",
			wantAgentIDSet: true,
		},
		{
			name:           "explicit fund overrides researcher kind",
			kind:           agentreputation.KindResearcher,
			explicit:       "fund",
			wantVisibility: "fund",
			wantAgentIDSet: true,
		},
		{
			name:           "explicit agent_portable overrides pm kind",
			kind:           agentreputation.KindPM,
			explicit:       "agent_portable",
			wantVisibility: "agent_portable",
			wantAgentIDSet: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, mock, done := newMockRepo(t)
			defer done()

			out := agentreputation.Outcome{
				ID:        "o-" + tc.name,
				FundID:    "f1",
				AgentID:   canonicalUUID,
				AgentName: "X",
				AgentKind: tc.kind,
				Symbol:    "AAPL",
				AsOf:      time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC),
				Direction: agentreputation.DirBullish,
				Alpha:     0.03,
			}

			var agentIDArg interface{}
			if tc.wantAgentIDSet {
				agentIDArg = sql.NullString{String: canonicalUUID, Valid: true}
			} else {
				agentIDArg = sql.NullString{}
			}

			mock.ExpectBegin()
			mock.ExpectPrepare("INSERT INTO memories")
			mock.ExpectQuery("FROM memories WHERE source_outcome_id").
				WithArgs("o-" + tc.name).
				WillReturnError(sql.ErrNoRows)
			// Pin the load-bearing args: fund_id at $1,
			// agent_id at $2 (new in AP2), layer at $3,
			// visibility at $4. The remaining args are
			// dynamic (title / content / time) so we use
			// sqlmock.AnyArg for them.
			mock.ExpectExec("INSERT INTO memories").
				WithArgs(
					"f1",              // $1 fund_id
					agentIDArg,        // $2 agent_id (AP2 addition)
					"long_term",       // $3 layer
					tc.wantVisibility, // $4 visibility (the assertion)
					"internal",        // $5 sensitivity
					"alpha_lesson",    // $6 origin_kind
					sqlmock.AnyArg(),  // $7 title
					sqlmock.AnyArg(),  // $8 content
					sqlmock.AnyArg(),  // $9 trading_date
					sqlmock.AnyArg(),  // $10 tags
					sqlmock.AnyArg(),  // $11 agent_tag
					sqlmock.AnyArg(),  // $12 alpha_vs_benchmark
					sqlmock.AnyArg(),  // $13 source_outcome_id
				).
				WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectCommit()

			n, err := r.WriteAlphaLessons(
				context.Background(),
				[]agentreputation.Outcome{out},
				WriteOptions{Visibility: tc.explicit},
			)
			if err != nil {
				t.Fatalf("write: %v", err)
			}
			if n != 1 {
				t.Errorf("want 1 lesson written, got %d", n)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("mock expectations: %v", err)
			}
		})
	}
}

// TestWriteAlphaLessons_NonUUIDAgentTagDowngradesAgentID
// pins the nullableUUID escape hatch: a string-tag-style
// AgentID (no UUID shape) must NOT abort the INSERT and must
// land with agent_id=NULL while preserving the agent_tag
// for downstream attribution. The lesson is still written
// (visibility='agent_portable' for a researcher kind) but the
// AP3 read path won't propagate it cross-fund because the
// join key is agent_id, not the loose tag.
func TestWriteAlphaLessons_NonUUIDAgentTagDowngradesAgentID(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()

	out := agentreputation.Outcome{
		ID:        "o-tag",
		FundID:    "f1",
		AgentID:   "fund_analyst", // non-UUID
		AgentKind: agentreputation.KindResearcher,
		Symbol:    "AAPL",
		AsOf:      time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC),
		Direction: agentreputation.DirBullish,
		Alpha:     0.03,
	}

	mock.ExpectBegin()
	mock.ExpectPrepare("INSERT INTO memories")
	mock.ExpectQuery("FROM memories WHERE source_outcome_id").
		WithArgs("o-tag").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("INSERT INTO memories").
		WithArgs(
			"f1",
			sql.NullString{}, // agent_id NULL because tag is non-UUID
			"long_term",
			"agent_portable", // researcher kind default
			"internal",
			"alpha_lesson",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sql.NullString{String: "fund_analyst", Valid: true}, // agent_tag preserved
			sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if _, err := r.WriteAlphaLessons(
		context.Background(),
		[]agentreputation.Outcome{out},
		WriteOptions{},
	); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

func TestListLessons_HappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	now := time.Now()
	mock.ExpectQuery("FROM memories").
		WithArgs("f1", 50).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "agent_tag", "content", "title",
			"alpha_vs_benchmark", "source_outcome_id", "trading_date", "created_at",
		}).AddRow("l1", "f1", "fund_analyst", "lesson body", "lesson title",
			0.025, "o1", now, now))
	out, err := r.ListLessons(context.Background(), ListLessonsParams{FundID: "f1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].AgentTag != "fund_analyst" {
		t.Errorf("got %+v", out)
	}
}

func TestListLessons_RejectsMissingFundID(t *testing.T) {
	r, _, done := newMockRepo(t)
	defer done()
	if _, err := r.ListLessons(context.Background(), ListLessonsParams{}); err == nil {
		t.Error("expected fundID error")
	}
}

func TestFormatLessonTitle(t *testing.T) {
	out := agentreputation.Outcome{
		AgentName: "Bull", Direction: agentreputation.DirBullish, Symbol: "AAPL",
		HorizonDays: 5, Alpha: 0.02,
	}
	got := formatLessonTitle(out)
	if got == "" {
		t.Error("empty title")
	}
}

func TestLessonTags(t *testing.T) {
	out := agentreputation.Outcome{
		AgentID: "fund_analyst", AgentKind: agentreputation.KindAnalyst,
		Category: "fundamentals", Symbol: "aapl", Alpha: 0.02,
	}
	got := lessonTags(out)
	gotSet := map[string]bool{}
	for _, t := range got {
		gotSet[t] = true
	}
	for _, want := range []string{"alpha_lesson", "analyst", "fund_analyst", "AAPL", "positive_alpha", "fundamentals"} {
		if !gotSet[want] {
			t.Errorf("missing tag %q in %v", want, got)
		}
	}
}
