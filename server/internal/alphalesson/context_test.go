package alphalesson

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/agentreputation"
)

func TestContextOptions_Normalize(t *testing.T) {
	o := ContextOptions{}.normalize()
	if o.TopAgents <= 0 || o.BottomAgents <= 0 || o.MaxLessons <= 0 {
		t.Errorf("defaults: %+v", o)
	}
	if o.SectionHeading == "" {
		t.Error("heading empty")
	}
}

func TestSplitTopBottom(t *testing.T) {
	stats := []agentreputation.Stats{
		{AgentID: "a1", DecisionsCount: 10, AvgAlpha: 0.05},
		{AgentID: "a2", DecisionsCount: 12, AvgAlpha: 0.03},
		{AgentID: "a3", DecisionsCount: 8, AvgAlpha: -0.01},
		{AgentID: "a4", DecisionsCount: 6, AvgAlpha: -0.04},
		{AgentID: "thin", DecisionsCount: 1, AvgAlpha: 0.99},
	}
	top, bot := splitTopBottom(stats, 2, 1, 5)
	if len(top) != 2 || top[0].AgentID != "a1" {
		t.Errorf("top = %+v", top)
	}
	if len(bot) != 1 || bot[0].AgentID != "a4" {
		t.Errorf("bot = %+v", bot)
	}
}

func TestSplitTopBottom_NoOverlap(t *testing.T) {
	stats := []agentreputation.Stats{
		{AgentID: "only", DecisionsCount: 10, AvgAlpha: 0.05},
	}
	top, bot := splitTopBottom(stats, 5, 3, 5)
	if len(top) != 1 || len(bot) != 0 {
		t.Errorf("top=%v bot=%v", top, bot)
	}
}

func TestSplitTopBottom_FiltersThin(t *testing.T) {
	stats := []agentreputation.Stats{
		{AgentID: "thin", DecisionsCount: 2, AvgAlpha: 0.99},
	}
	top, bot := splitTopBottom(stats, 5, 3, 5)
	if len(top) != 0 || len(bot) != 0 {
		t.Errorf("thin agents should be filtered: top=%v bot=%v", top, bot)
	}
}

func TestBuildContext_RejectsMissingFundID(t *testing.T) {
	_, err := BuildContext(context.Background(), nil, nil, "", ContextOptions{})
	if err == nil {
		t.Error("expected fundID error")
	}
}

func TestBuildContext_EmptyWhenNothing(t *testing.T) {
	got, err := BuildContext(context.Background(), nil, nil, "f1", ContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestBuildContext_RendersLeaderboardAndLessons(t *testing.T) {
	statsDB, statsMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer statsDB.Close()
	statsRepo := agentreputation.NewRepo(statsDB)

	now := time.Now()
	statsMock.ExpectQuery("FROM agent_reputation_stats").
		WillReturnRows(sqlmock.NewRows([]string{
			"fund_id", "agent_id", "agent_name", "agent_kind", "category",
			"decisions_count", "hits_count", "misses_count",
			"avg_alpha", "sum_alpha", "avg_confidence",
			"last_decision_at", "updated_at",
		}).AddRow("f1", "fund_analyst", "Fundamentals", "analyst", "fundamentals",
			int64(10), int64(7), int64(3), 0.02, 0.20, 65.0, now, now).
			AddRow("f1", "news_analyst", "News", "analyst", "news",
				int64(8), int64(2), int64(6), -0.02, -0.16, 50.0, now, now))

	lessonDB, lessonMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer lessonDB.Close()
	lessonRepo := NewRepo(lessonDB)
	lessonMock.ExpectQuery("FROM memories").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "agent_id", "visibility", "sensitivity", "agent_tag", "content", "title",
			"alpha_vs_benchmark", "source_outcome_id", "trading_date", "created_at",
		}).AddRow("l1", "f1", nil, "fund", "internal", "fund_analyst", "lesson body",
			sql.NullString{String: "lesson title", Valid: true},
			0.015, "o1", now, now))

	got, err := BuildContext(context.Background(), statsRepo, lessonRepo, "f1", ContextOptions{
		TopAgents: 3, BottomAgents: 3, MaxLessons: 5, MinDecisions: 5,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(got, "Agent Track Record") {
		t.Errorf("missing heading: %s", got)
	}
	if !strings.Contains(got, "Top by avg α") {
		t.Errorf("missing top section: %s", got)
	}
	if !strings.Contains(got, "Recent alpha-tagged lessons") {
		t.Errorf("missing lessons section: %s", got)
	}
	if !strings.Contains(got, "Fundamentals") {
		t.Errorf("missing top agent label: %s", got)
	}
}

// TestBuildContext_InheritedLabelOnCrossFundLesson is AP8 —
// the load-bearing assertion that the LLM prompt actually
// surfaces the inheritance signal. Without this label the
// PM (and the LLM reading the markdown) has no way to tell
// "this NVDA lesson was learned at YOUR fund last quarter"
// apart from "this NVDA lesson was inherited from the same
// researcher's prior fund and may or may not apply here".
//
// The test wires:
//   - a TeamProvider that returns one team UUID + a regime
//   - a sqlmock that returns an agent_portable lesson whose
//     fund_id differs from the querying fund (so the read
//     path marks it InheritedFromOtherFund=true)
//
// And asserts that:
//   - the rendered markdown contains the inherited label
//     for the cross-fund lesson
//   - a co-resident same-fund lesson does NOT carry the label
func TestBuildContext_InheritedLabelOnCrossFundLesson(t *testing.T) {
	uuidA := "11111111-1111-1111-1111-111111111111"
	statsDB, statsMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer statsDB.Close()
	statsRepo := agentreputation.NewRepo(statsDB)
	statsMock.ExpectQuery("FROM agent_reputation_stats").
		WillReturnRows(sqlmock.NewRows([]string{
			"fund_id", "agent_id", "agent_name", "agent_kind", "category",
			"decisions_count", "hits_count", "misses_count",
			"avg_alpha", "sum_alpha", "avg_confidence",
			"last_decision_at", "updated_at",
		}))

	now := time.Now()
	lessonDB, lessonMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer lessonDB.Close()
	lessonRepo := NewRepo(lessonDB)

	// Two lessons:
	//   l-native   : fund_id=fund-A (matches querying fund) → not inherited
	//   l-inherited: fund_id=fund-B (differs) + agent_portable → inherited
	lessonMock.ExpectQuery("FROM memories").
		WithArgs("fund-A", sqlmock.AnyArg(), sqlmock.AnyArg(), 8).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "agent_id", "visibility", "sensitivity", "agent_tag",
			"content", "title", "alpha_vs_benchmark",
			"source_outcome_id", "trading_date", "created_at",
		}).AddRow("l-native", "fund-A", uuidA, "agent_portable", "internal", "tag",
			"native body", "native title", 0.02, "o1", now, now).
			AddRow("l-inherited", "fund-B", uuidA, "agent_portable", "internal", "tag",
				"inherited body", "inherited title", 0.03, "o2", now, now))

	teamCalls := 0
	teamProvider := func(_ context.Context, fundID string) ([]string, string, bool) {
		teamCalls++
		if fundID != "fund-A" {
			t.Errorf("TeamProvider called with %q, want fund-A", fundID)
		}
		return []string{uuidA}, "trend_up", false
	}

	got, err := BuildContext(context.Background(), statsRepo, lessonRepo, "fund-A", ContextOptions{
		TopAgents: 3, BottomAgents: 3, MaxLessons: 8, MinDecisions: 5,
		TeamProvider: teamProvider,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if teamCalls != 1 {
		t.Errorf("TeamProvider called %d times, want 1", teamCalls)
	}

	// Build a per-line index so we can assert that the
	// inherited marker lands on EXACTLY the inherited lesson.
	// A naïve substring check would pass even if the renderer
	// accidentally tagged BOTH lessons as inherited (or
	// neither).
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "native title") {
			if strings.Contains(line, "[inherited]") {
				t.Errorf("native lesson should NOT carry inherited label: %q", line)
			}
		}
		if strings.Contains(line, "inherited title") {
			if !strings.Contains(line, "[inherited]") {
				t.Errorf("cross-fund lesson should carry inherited label: %q", line)
			}
		}
	}
	if !strings.Contains(got, "[inherited]") {
		t.Errorf("rendered markdown missing inherited label entirely: %s", got)
	}
}

// TestBuildContext_InheritedLabelCustomisable pins that an
// operator-supplied InheritedLabel overrides the default.
// Useful for locales (CN PM prompts) and for tooling that
// wants a richer label like " [from agent's prior fund]".
func TestBuildContext_InheritedLabelCustomisable(t *testing.T) {
	uuidA := "11111111-1111-1111-1111-111111111111"
	lessonDB, lessonMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer lessonDB.Close()
	lessonRepo := NewRepo(lessonDB)
	now := time.Now()

	lessonMock.ExpectQuery("FROM memories").
		WithArgs("fund-A", sqlmock.AnyArg(), 8).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "agent_id", "visibility", "sensitivity", "agent_tag",
			"content", "title", "alpha_vs_benchmark",
			"source_outcome_id", "trading_date", "created_at",
		}).AddRow("l-inherited", "fund-B", uuidA, "agent_portable", "internal", "tag",
			"cross body", "cross title", 0.03, "o2", now, now))

	got, err := BuildContext(context.Background(), nil, lessonRepo, "fund-A", ContextOptions{
		MaxLessons: 8,
		// Custom label including a localised phrase so the
		// rendered prompt reads naturally in CN PM seats.
		InheritedLabel: " [继承自其他基金]",
		TeamProvider: func(_ context.Context, _ string) ([]string, string, bool) {
			return []string{uuidA}, "", false
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(got, "[继承自其他基金]") {
		t.Errorf("custom inherited label not honoured: %s", got)
	}
	if strings.Contains(got, "[inherited]") {
		t.Errorf("default label leaked through despite custom override: %s", got)
	}
}

// TestBuildContext_NoTeamProviderStaysLegacy guards the
// backwards-compat invariant: a caller that doesn't supply a
// TeamProvider gets the EXACT same SQL as pre-AP8 — no
// TeamAgentIDs, no regime, no opt-out arg. A regression that
// "helpfully" defaulted TeamProvider to fund-team-from-DB
// would change every existing prompt without operator consent.
func TestBuildContext_NoTeamProviderStaysLegacy(t *testing.T) {
	lessonDB, lessonMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer lessonDB.Close()
	lessonRepo := NewRepo(lessonDB)

	// Legacy shape: only $1=fundID and $2=limit. The
	// cross-fund branch is OFF so no pq.Array arg appears.
	lessonMock.ExpectQuery("FROM memories").
		WithArgs("fund-A", 8).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "agent_id", "visibility", "sensitivity", "agent_tag",
			"content", "title", "alpha_vs_benchmark",
			"source_outcome_id", "trading_date", "created_at",
		}))

	if _, err := BuildContext(context.Background(), nil, lessonRepo, "fund-A", ContextOptions{
		MaxLessons: 8,
	}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := lessonMock.ExpectationsWereMet(); err != nil {
		t.Errorf("legacy SQL shape regressed: %v", err)
	}
}

func TestOneLine(t *testing.T) {
	if got := oneLine("a\nb"); got != "a" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("x", 300)
	got := oneLine(long)
	if len([]rune(got)) > 165 {
		t.Errorf("truncation too lax: %d runes", len([]rune(got)))
	}
}
