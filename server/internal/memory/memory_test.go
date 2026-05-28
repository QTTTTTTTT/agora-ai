package memory

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// ---- Cosine ---------------------------------------------------------------

func TestCosine_Identical(t *testing.T) {
	a := Embedding{1, 2, 3}
	if got := Cosine(a, a); math.Abs(got-1) > 1e-9 {
		t.Errorf("identical: got %v want 1", got)
	}
}

func TestCosine_Orthogonal(t *testing.T) {
	a := Embedding{1, 0}
	b := Embedding{0, 1}
	if got := Cosine(a, b); math.Abs(got) > 1e-9 {
		t.Errorf("orthogonal: got %v want 0", got)
	}
}

func TestCosine_DifferentLengths(t *testing.T) {
	if Cosine(Embedding{1, 2}, Embedding{1, 2, 3}) != 0 {
		t.Error("expected 0 for length mismatch")
	}
	if Cosine(nil, nil) != 0 {
		t.Error("expected 0 for empty")
	}
}

func TestCosine_ZeroVector(t *testing.T) {
	if Cosine(Embedding{0, 0}, Embedding{1, 1}) != 0 {
		t.Error("zero vector should give 0 similarity")
	}
}

// ---- Importance -----------------------------------------------------------

func TestScoreImportance_DefaultWhenNoSignal(t *testing.T) {
	if got := ScoreImportance(ImportanceSignals{}); got != 0.5 {
		t.Errorf("expected neutral 0.5, got %v", got)
	}
}

func TestScoreImportance_DailyReturnSaturates(t *testing.T) {
	// 5% saturates the daily-return component; 10% should give the same
	// contribution from that piece.
	a := ScoreImportance(ImportanceSignals{DailyReturn: 0.05})
	b := ScoreImportance(ImportanceSignals{DailyReturn: 0.10})
	if math.Abs(a-b) > 1e-9 {
		t.Errorf("expected saturation: %v vs %v", a, b)
	}
	// Negative returns count by absolute magnitude.
	c := ScoreImportance(ImportanceSignals{DailyReturn: -0.05})
	if math.Abs(a-c) > 1e-9 {
		t.Errorf("negative return mismatch: %v vs %v", a, c)
	}
}

func TestScoreImportance_TagsBoost(t *testing.T) {
	low := ScoreImportance(ImportanceSignals{Tags: []string{"unrecognised"}})
	high := ScoreImportance(ImportanceSignals{Tags: []string{"circuit_breaker", "risk"}})
	if !(high > low) {
		t.Errorf("expected high > low: high=%v low=%v", high, low)
	}
}

func TestScoreImportance_RiskFlagAddsWeight(t *testing.T) {
	base := ScoreImportance(ImportanceSignals{LLMRated: 0.5})
	withRisk := ScoreImportance(ImportanceSignals{LLMRated: 0.5, HasFailedRiskCheck: true})
	if !(withRisk > base) {
		t.Errorf("risk flag should raise score: %v vs %v", withRisk, base)
	}
}

func TestScoreImportance_Clamps(t *testing.T) {
	got := ScoreImportance(ImportanceSignals{
		DailyReturn:        1.0, // saturates
		HasFailedRiskCheck: true,
		LLMRated:           1.0,
		Tags:               []string{"circuit_breaker", "risk", "event", "earnings"},
	})
	if got < 0 || got > 1 {
		t.Errorf("score out of [0,1]: %v", got)
	}
	if got <= 0.9 {
		t.Errorf("expected near-1 for max signals, got %v", got)
	}
}

// ---- Recall ---------------------------------------------------------------

func TestRecall_PrefersRecent(t *testing.T) {
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	items := []Item{
		{ID: "old", CreatedAt: now.Add(-7 * 24 * time.Hour), Importance: 0.5},
		{ID: "new", CreatedAt: now.Add(-1 * time.Hour), Importance: 0.5},
	}
	got, err := Recall(items, RecallParams{Now: now, TopK: 1})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if got[0].ID != "new" {
		t.Errorf("expected new first, got %s", got[0].ID)
	}
}

func TestRecall_PrefersImportant(t *testing.T) {
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	items := []Item{
		{ID: "boring", CreatedAt: now, Importance: 0.1},
		{ID: "salient", CreatedAt: now, Importance: 0.9},
	}
	got, _ := Recall(items, RecallParams{Now: now, TopK: 1})
	if got[0].ID != "salient" {
		t.Errorf("expected salient first, got %s", got[0].ID)
	}
}

func TestRecall_PrefersSimilar(t *testing.T) {
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	items := []Item{
		{ID: "off", CreatedAt: now, Importance: 0.5, Embedding: Embedding{1, 0}},
		{ID: "match", CreatedAt: now, Importance: 0.5, Embedding: Embedding{0, 1}},
	}
	got, _ := Recall(items, RecallParams{Now: now, Query: Embedding{0, 1}, TopK: 1})
	if got[0].ID != "match" {
		t.Errorf("expected match first, got %s", got[0].ID)
	}
}

func TestRecall_TopKAndMinScore(t *testing.T) {
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	items := []Item{
		{ID: "1", CreatedAt: now, Importance: 0.9},
		{ID: "2", CreatedAt: now, Importance: 0.8},
		{ID: "3", CreatedAt: now.Add(-365 * 24 * time.Hour), Importance: 0.0}, // ancient + 0 imp
	}
	got, _ := Recall(items, RecallParams{Now: now, TopK: 2})
	if len(got) != 2 {
		t.Fatalf("topK: got %d want 2", len(got))
	}
	got, _ = Recall(items, RecallParams{Now: now, MinScore: 0.5})
	for _, s := range got {
		if s.Score < 0.5 {
			t.Errorf("MinScore filter failed: %s -> %v", s.ID, s.Score)
		}
	}
}

func TestRecall_NoCandidates(t *testing.T) {
	if _, err := Recall(nil, RecallParams{}); !errors.Is(err, ErrNoCandidates) {
		t.Errorf("expected ErrNoCandidates, got %v", err)
	}
}

func TestRecall_ZeroWeightsRejected(t *testing.T) {
	items := []Item{{ID: "x", CreatedAt: time.Now()}}
	_, err := Recall(items, RecallParams{Weights: RecallWeights{Recency: 0, Importance: 0, Similarity: 0}})
	// All-zero weights take the default path because RecallWeights{} -> default.
	if err != nil {
		t.Fatalf("default fallback should not error: %v", err)
	}
	// But explicitly negative-balanced weights (sum = 0 with mixed values) is impossible because we don't allow negative weights anyway; verify negative gives the expected sum>0 path or error.
	_, err = Recall(items, RecallParams{Weights: RecallWeights{Recency: -1, Importance: 1, Similarity: 0}})
	if err == nil {
		t.Errorf("expected error for non-positive weight sum")
	}
}

func TestRecency_RefreshesOnAccess(t *testing.T) {
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	items := []Item{
		{ID: "stale-but-touched",
			CreatedAt:      now.Add(-30 * 24 * time.Hour),
			LastAccessedAt: now.Add(-1 * time.Hour),
			Importance:     0.5},
		{ID: "untouched",
			CreatedAt:  now.Add(-7 * 24 * time.Hour),
			Importance: 0.5},
	}
	got, _ := Recall(items, RecallParams{Now: now, TopK: 1})
	if got[0].ID != "stale-but-touched" {
		t.Errorf("expected last-accessed item to win, got %s", got[0].ID)
	}
}

// ---- Reflexion ------------------------------------------------------------

func TestReflect_GroupsByTagAndCallsDistiller(t *testing.T) {
	calls := map[string]int{}
	dist := DistillerFunc(func(_ context.Context, theme string, items []Item) (string, error) {
		calls[theme] = len(items)
		// Substantive Chinese lesson to pass the reflexion
		// quality gate (>=25 runes, no platitude prefix).
		return "对 " + theme + " 类标的的复盘指出，今日收盘前 30 分钟的被动追价显著拉低成交均价。", nil
	})
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	items := []Item{
		{ID: "a", Tags: []string{"earnings", "self_learning"}, Importance: 0.6, CreatedAt: now},
		{ID: "b", Tags: []string{"earnings"}, Importance: 0.5, CreatedAt: now},
		{ID: "c", Tags: []string{"earnings"}, Importance: 0.7, CreatedAt: now},
		{ID: "d", Tags: []string{"macro"}, Importance: 0.4, CreatedAt: now},
		{ID: "e", Tags: []string{"macro"}, Importance: 0.5, CreatedAt: now},
		{ID: "f", Tags: []string{"macro"}, Importance: 0.6, CreatedAt: now},
	}
	got, err := Reflect(context.Background(), items, dist, ReflectParams{
		FundID:       "fund-1",
		Now:          now,
		MinGroupSize: 3,
	})
	if err != nil {
		t.Fatalf("reflect: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 reflections, got %d", len(got))
	}
	if calls["earnings"] != 3 || calls["macro"] != 3 {
		t.Errorf("group sizes wrong: %#v", calls)
	}
	for _, r := range got {
		if r.Layer != "long_term" || r.Kind != "reflection" {
			t.Errorf("wrong layer/kind: %#v", r)
		}
		if r.FundID != "fund-1" {
			t.Errorf("FundID not propagated: %s", r.FundID)
		}
	}
}

func TestReflect_SkipsSmallGroups(t *testing.T) {
	dist := DistillerFunc(func(_ context.Context, _ string, _ []Item) (string, error) {
		return "x", nil
	})
	items := []Item{
		{Tags: []string{"earnings"}, Importance: 1, CreatedAt: time.Now()},
		{Tags: []string{"earnings"}, Importance: 1, CreatedAt: time.Now()},
	}
	_, err := Reflect(context.Background(), items, dist, ReflectParams{MinGroupSize: 3})
	if !errors.Is(err, ErrNothingToReflect) {
		t.Errorf("expected ErrNothingToReflect, got %v", err)
	}
}

func TestReflect_MaxGroupsCap(t *testing.T) {
	dist := DistillerFunc(func(_ context.Context, theme string, _ []Item) (string, error) {
		// Substantive output so the reflexion quality gate doesn't
		// drop it; the test only cares about MaxGroups capping.
		return "对 " + theme + " 主题的复盘提示日内换手与持仓集中度需要联动校验。", nil
	})
	now := time.Now()
	items := []Item{}
	for _, theme := range []string{"a", "b", "c"} {
		for i := 0; i < 3; i++ {
			items = append(items, Item{Tags: []string{theme}, Importance: 0.7, CreatedAt: now})
		}
	}
	got, err := Reflect(context.Background(), items, dist, ReflectParams{
		MinGroupSize: 3,
		MaxGroups:    2,
	})
	if err != nil {
		t.Fatalf("reflect: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected MaxGroups=2 to cap output, got %d", len(got))
	}
}

func TestReflect_MinAvgImportanceFilter(t *testing.T) {
	dist := DistillerFunc(func(_ context.Context, _ string, _ []Item) (string, error) {
		return "x", nil
	})
	now := time.Now()
	items := []Item{
		{Tags: []string{"earnings"}, Importance: 0.1, CreatedAt: now},
		{Tags: []string{"earnings"}, Importance: 0.1, CreatedAt: now},
		{Tags: []string{"earnings"}, Importance: 0.1, CreatedAt: now},
	}
	_, err := Reflect(context.Background(), items, dist, ReflectParams{
		MinGroupSize:     3,
		MinAvgImportance: 0.5,
	})
	if !errors.Is(err, ErrNothingToReflect) {
		t.Errorf("expected nothing to reflect, got %v", err)
	}
}

func TestReflect_RequiresDistiller(t *testing.T) {
	_, err := Reflect(context.Background(), []Item{{}}, nil, ReflectParams{})
	if err == nil {
		t.Error("expected error for nil distiller")
	}
}

func TestReflect_LimitsDistillerInput(t *testing.T) {
	var received []Item
	dist := DistillerFunc(func(_ context.Context, _ string, items []Item) (string, error) {
		received = append([]Item(nil), items...)
		// Substantive lesson to survive the quality gate; this test
		// only cares about the input limiting, not the output.
		return "对 earnings 主题的复盘指出今日盘前研究覆盖度不足，需要补完关键个股的事件日历。", nil
	})
	items := []Item{
		{Tags: []string{"earnings"}, Importance: 0.7, Content: "abcdefghijklmnopqrstuvwxyz"},
		{Tags: []string{"earnings"}, Importance: 0.7, Content: "abcdefghijklmnopqrstuvwxyz"},
		{Tags: []string{"earnings"}, Importance: 0.7, Content: "abcdefghijklmnopqrstuvwxyz"},
	}
	_, err := Reflect(context.Background(), items, dist, ReflectParams{
		MinGroupSize:        3,
		MaxItemsPerGroup:    2,
		MaxItemContentRunes: 10,
	})
	if err != nil {
		t.Fatalf("reflect: %v", err)
	}
	if len(received) != 2 {
		t.Fatalf("expected 2 items sent to distiller, got %d", len(received))
	}
	if !contains(received[0].Content, "[truncated]") {
		t.Fatalf("expected truncated content, got %q", received[0].Content)
	}
}

func TestReflect_ContinuesOnDistillerError(t *testing.T) {
	calls := 0
	dist := DistillerFunc(func(_ context.Context, theme string, _ []Item) (string, error) {
		calls++
		if theme == "earnings" {
			return "", errors.New("boom")
		}
		// Return a substantive Chinese lesson so the new quality
		// gate (>= 25 runes, no platitude prefix) accepts it; the
		// test's intent is "errors in one group don't kill others",
		// not "tiny strings still get through".
		return "对 " + theme + " 主题的复盘显示日内换手与持仓集中度需要联动监控。", nil
	})
	now := time.Now()
	items := []Item{
		{Tags: []string{"earnings"}, Importance: 0.7, CreatedAt: now},
		{Tags: []string{"earnings"}, Importance: 0.7, CreatedAt: now},
		{Tags: []string{"earnings"}, Importance: 0.7, CreatedAt: now},
		{Tags: []string{"macro"}, Importance: 0.7, CreatedAt: now},
		{Tags: []string{"macro"}, Importance: 0.7, CreatedAt: now},
		{Tags: []string{"macro"}, Importance: 0.7, CreatedAt: now},
	}
	got, err := Reflect(context.Background(), items, dist, ReflectParams{MinGroupSize: 3})
	if err != nil {
		t.Fatalf("reflect: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 distiller calls, got %d", calls)
	}
	if len(got) != 1 || !contains(got[0].Title, "macro") {
		t.Errorf("expected the surviving macro reflection, got %#v", got)
	}
}

// TestReflect_DropsLowQualityLessons covers the quality gate added to
// stop the OCS Selection regression where the distiller emitted
// "To maximize the utility of research" — a sub-40-rune motivational
// fragment with no operational content — and that single useless
// lesson then propagated into every agent's skill_config as a
// "needs approval" entry. The gate has two arms (length floor +
// platitude prefix); test both, plus a substantive lesson that must
// still pass.
func TestReflect_DropsLowQualityLessons(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		out     string
		wantOut bool
	}{
		{"short fragment", "Avoid it.", false},
		{"platitude english", "To maximize the utility of research, the team should iterate.", false},
		{"platitude chinese", "为了让研究价值最大化，团队应当持续优化输入质量。", false},
		{"meaty english", "Avoid chasing the third consecutive green chip bar in pre-market trading on momentum spikes.", true},
		{"meaty chinese", "688205 在融资客大幅净买入后两个交易日出现回吐，未来需结合资金流确认而非单一新闻。", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dist := DistillerFunc(func(_ context.Context, _ string, _ []Item) (string, error) {
				return tc.out, nil
			})
			items := []Item{
				{Tags: []string{"chip"}, Importance: 0.7, CreatedAt: time.Now()},
				{Tags: []string{"chip"}, Importance: 0.7, CreatedAt: time.Now()},
				{Tags: []string{"chip"}, Importance: 0.7, CreatedAt: time.Now()},
			}
			got, err := Reflect(context.Background(), items, dist, ReflectParams{MinGroupSize: 3})
			if tc.wantOut {
				if err != nil {
					t.Fatalf("reflect: %v", err)
				}
				if len(got) != 1 {
					t.Fatalf("expected 1 reflection, got %d", len(got))
				}
				return
			}
			// Low-quality outputs collapse to ErrNothingToReflect
			// because that was the only group.
			if !errors.Is(err, ErrNothingToReflect) {
				t.Fatalf("expected ErrNothingToReflect for %q, got %v (out=%v)", tc.out, err, got)
			}
			if len(got) != 0 {
				t.Errorf("expected no reflections to be persisted, got %d", len(got))
			}
		})
	}
}

// helpers
func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})())
}
