package attribution

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/repository"
)

func mkMemory(t *testing.T, title, body string, tags []string, createdAt time.Time) repository.Memory {
	t.Helper()
	return repository.Memory{
		ID:          "mem-" + title,
		FundID:      "fund-test",
		Layer:       MemoryLayer,
		Title:       sql.NullString{String: title, Valid: title != ""},
		Content:     body,
		Tags:        append([]string(nil), tags...),
		Visibility:  "private",
		Sensitivity: "internal",
		OriginKind:  "native",
		CreatedAt:   createdAt,
	}
}

func TestBuildLessonReplayEmptyOnEmptyInput(t *testing.T) {
	got := BuildLessonReplay(nil, time.Now(), LessonReplayOptions{})
	if got.Summary != "" || len(got.Rows) != 0 {
		t.Fatalf("expected zero value, got %+v", got)
	}
}

func TestBuildLessonReplayDropsInsufficientDataByDefault(t *testing.T) {
	now := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	mems := []repository.Memory{
		mkMemory(t, "No closed trades in the last 30 days", "Attribution will populate once the fund has produced its first realized P&L.",
			[]string{"insufficient_data"}, now.Add(-1*time.Hour)),
	}
	got := BuildLessonReplay(mems, now, LessonReplayOptions{})
	if len(got.Rows) != 0 || got.Summary != "" {
		t.Fatalf("expected insufficient_data to be filtered out, got %+v", got)
	}

	// And kept when the flag is on.
	got = BuildLessonReplay(mems, now, LessonReplayOptions{IncludeInsufficientData: true})
	if len(got.Rows) != 1 {
		t.Fatalf("expected 1 row when IncludeInsufficientData set, got %+v", got.Rows)
	}
	if got.Rows[0].Kind != string(LessonInsufficientData) {
		t.Fatalf("expected insufficient_data kind, got %q", got.Rows[0].Kind)
	}
}

func TestBuildLessonReplayDeduplicatesKeepsFreshest(t *testing.T) {
	now := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	stale := mkMemory(t,
		"Sleeve \"trend\" is losing money in regime \"chop\" (12 trades, win-rate 25%, PnL -500)",
		"old body",
		[]string{"loser", "sleeve:trend", "regime:chop"},
		now.Add(-3*24*time.Hour),
	)
	fresh := mkMemory(t,
		"Sleeve \"trend\" is losing money in regime \"chop\" (15 trades, win-rate 22%, PnL -650)",
		"fresh body talks about the latest cohort.",
		[]string{"loser", "sleeve:trend", "regime:chop"},
		now.Add(-1*time.Hour),
	)
	got := BuildLessonReplay([]repository.Memory{stale, fresh}, now, LessonReplayOptions{})
	if len(got.Rows) != 1 {
		t.Fatalf("expected 1 deduped row, got %d: %+v", len(got.Rows), got.Rows)
	}
	if !strings.Contains(got.Rows[0].Body, "fresh body") {
		t.Fatalf("expected fresh body to win dedup, got %q", got.Rows[0].Body)
	}
}

func TestBuildLessonReplaySortsBySeverityThenRecency(t *testing.T) {
	now := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	mems := []repository.Memory{
		mkMemory(t, "winner old", "won.", []string{"winner", "sleeve:trend", "regime:trend_up"}, now.Add(-2*24*time.Hour)),
		mkMemory(t, "critical fresh", "lost.", []string{"loser", "sleeve:mean_reversion", "regime:chop"}, now.Add(-2*time.Hour)),
		mkMemory(t, "critical stale", "lost long ago.", []string{"loser", "sleeve:trend", "regime:chop"}, now.Add(-3*24*time.Hour)),
		mkMemory(t, "winner fresh", "won recently.", []string{"winner", "sleeve:trend", "regime:trend_down"}, now.Add(-1*time.Hour)),
	}
	got := BuildLessonReplay(mems, now, LessonReplayOptions{MaxLessons: 10})
	if len(got.Rows) != 4 {
		t.Fatalf("expected 4 rows, got %d: %+v", len(got.Rows), got.Rows)
	}
	// First two must be critical (loser); freshest critical first.
	if got.Rows[0].Severity != string(SeverityCritical) || !strings.Contains(got.Rows[0].Title, "critical fresh") {
		t.Fatalf("expected critical fresh first, got %+v", got.Rows[0])
	}
	if got.Rows[1].Severity != string(SeverityCritical) || !strings.Contains(got.Rows[1].Title, "critical stale") {
		t.Fatalf("expected critical stale second, got %+v", got.Rows[1])
	}
	// Then winners in recency DESC.
	if got.Rows[2].Severity != string(SeverityInfo) || !strings.Contains(got.Rows[2].Title, "winner fresh") {
		t.Fatalf("expected winner fresh third, got %+v", got.Rows[2])
	}
}

func TestBuildLessonReplayLookbackDropsAncientLessons(t *testing.T) {
	now := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	mems := []repository.Memory{
		mkMemory(t, "fresh loser", "body fresh", []string{"loser", "sleeve:trend", "regime:chop"}, now.Add(-2*24*time.Hour)),
		mkMemory(t, "ancient loser", "body ancient", []string{"loser", "sleeve:mean_reversion", "regime:trend_up"}, now.Add(-60*24*time.Hour)),
	}
	got := BuildLessonReplay(mems, now, LessonReplayOptions{LookbackDays: 14})
	if len(got.Rows) != 1 {
		t.Fatalf("expected ancient to drop, got %d rows: %+v", len(got.Rows), got.Rows)
	}
	if !strings.Contains(got.Rows[0].Title, "fresh") {
		t.Fatalf("expected fresh loser to survive, got %+v", got.Rows[0])
	}
}

func TestBuildLessonReplayCapsAtMaxLessons(t *testing.T) {
	now := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	mems := []repository.Memory{}
	for i := 0; i < 8; i++ {
		mems = append(mems, mkMemory(t,
			"loser " + string(rune('a'+i)),
			"body",
			[]string{"loser", "sleeve:s" + string(rune('a'+i)), "regime:trend_up"},
			now.Add(-time.Duration(i+1)*time.Hour),
		))
	}
	got := BuildLessonReplay(mems, now, LessonReplayOptions{MaxLessons: 3})
	if len(got.Rows) != 3 {
		t.Fatalf("expected cap at 3, got %d", len(got.Rows))
	}
	// Freshest should win — i=0 has CreatedAt = now-1h, the
	// freshest of the bunch.
	if !strings.Contains(got.Rows[0].Title, "loser a") {
		t.Fatalf("expected freshest first, got %+v", got.Rows[0])
	}
}

func TestBuildLessonReplayTruncatesBodyAtSentence(t *testing.T) {
	now := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	longBody := strings.Repeat("Sentence one. ", 30) + "Sentence two. Sentence three."
	mems := []repository.Memory{
		mkMemory(t, "loser", longBody, []string{"loser", "sleeve:trend", "regime:chop"}, now.Add(-1*time.Hour)),
	}
	got := BuildLessonReplay(mems, now, LessonReplayOptions{MaxBodyChars: 60})
	if len(got.Rows) != 1 {
		t.Fatalf("expected 1 row, got %+v", got)
	}
	body := got.Rows[0].Body
	if len(body) > 60 {
		t.Fatalf("body must respect MaxBodyChars=60, got len=%d: %q", len(body), body)
	}
	if !strings.HasSuffix(body, ".") && !strings.HasSuffix(body, "…") {
		t.Fatalf("expected truncated body to end at sentence boundary, got %q", body)
	}
}

func TestBuildLessonReplaySummaryMentionsSeverityAndSleeve(t *testing.T) {
	now := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	mems := []repository.Memory{
		mkMemory(t, "Sleeve \"trend\" is losing money in regime \"chop\"",
			"Across 12 closed lots ... consider pausing.",
			[]string{"loser", "sleeve:trend", "regime:chop"},
			now.Add(-1*time.Hour),
		),
	}
	got := BuildLessonReplay(mems, now, LessonReplayOptions{})
	if got.Summary == "" {
		t.Fatalf("expected non-empty summary")
	}
	for _, want := range []string{"CRITICAL", "trend", "chop", "consider pausing"} {
		if !strings.Contains(got.Summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, got.Summary)
		}
	}
	if !strings.Contains(got.Window, "days") {
		t.Fatalf("expected window label, got %q", got.Window)
	}
}

func TestBuildLessonReplayUntaggedLegacyRowRecoveredByTitleHeuristic(t *testing.T) {
	now := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	// Legacy memory: no "loser" tag but title carries the
	// canonical phrasing. The heuristic in memoryToReplayRow
	// classifies it as a loser so the LLM still sees it.
	mems := []repository.Memory{
		mkMemory(t,
			"Sleeve \"trend\" is losing money in regime \"chop\" (12 trades)",
			"legacy body that pre-dates the tag schema.",
			[]string{},
			now.Add(-1*time.Hour),
		),
	}
	got := BuildLessonReplay(mems, now, LessonReplayOptions{})
	if len(got.Rows) != 1 {
		t.Fatalf("expected legacy heuristic to recover the row, got %+v", got.Rows)
	}
	if got.Rows[0].Severity != string(SeverityCritical) {
		t.Fatalf("expected critical severity for legacy loser, got %q", got.Rows[0].Severity)
	}
}
