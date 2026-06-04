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
