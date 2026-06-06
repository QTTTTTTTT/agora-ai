package eventwindow

import (
	"testing"

	"github.com/fundai/server/internal/planoutcome"
)

func TestResolveEarningsTags(t *testing.T) {
	cases := [][]string{
		{"earnings_beat"},
		{"PEAD"},
		{"guidance_raise"},
		{"earnings_miss", "value"},
	}
	for _, tags := range cases {
		got := Resolve(tags, DefaultPolicy())
		if got != planoutcome.WindowNextEarnings {
			t.Errorf("tags %v: got %v, want next_earnings", tags, got)
		}
	}
}

func TestResolveNewsTags(t *testing.T) {
	got := Resolve([]string{"news_catalyst"}, DefaultPolicy())
	if got != planoutcome.WindowNextNews {
		t.Errorf("got %v, want next_news", got)
	}
}

func TestResolveLongHorizonTags(t *testing.T) {
	cases := []struct {
		tags []string
		want planoutcome.WindowKind
	}{
		{[]string{"value"}, planoutcome.WindowFixed20d},
		{[]string{"quality"}, planoutcome.WindowFixed20d},
		{[]string{"macro"}, planoutcome.WindowFixed20d},
		{[]string{"fundamentals"}, planoutcome.WindowFixed10d},
		{[]string{"sector_rotation"}, planoutcome.WindowFixed10d},
	}
	for _, c := range cases {
		got := Resolve(c.tags, DefaultPolicy())
		if got != c.want {
			t.Errorf("tags %v: got %v, want %v", c.tags, got, c.want)
		}
	}
}

func TestResolveShortHorizonTags(t *testing.T) {
	cases := []string{"momentum", "breakout", "mean_reversion"}
	for _, tag := range cases {
		got := Resolve([]string{tag}, DefaultPolicy())
		if got != planoutcome.WindowFixed5d {
			t.Errorf("tag %q: got %v, want fixed_5d", tag, got)
		}
	}
}

func TestResolveOverridesWin(t *testing.T) {
	p := DefaultPolicy()
	p.Overrides = map[string]planoutcome.WindowKind{
		"earnings_beat": planoutcome.WindowFixed10d,
	}
	got := Resolve([]string{"earnings_beat"}, p)
	if got != planoutcome.WindowFixed10d {
		t.Errorf("override should win: got %v, want fixed_10d", got)
	}
}

func TestResolveCaseInsensitive(t *testing.T) {
	got := Resolve([]string{"  EARNINGS_BEAT  "}, DefaultPolicy())
	if got != planoutcome.WindowNextEarnings {
		t.Errorf("got %v, want next_earnings", got)
	}
}

func TestResolveEmptyTagsUsesDefault(t *testing.T) {
	if got := Resolve(nil, DefaultPolicy()); got != planoutcome.WindowFixed5d {
		t.Errorf("nil tags: got %v, want fixed_5d", got)
	}
	p := Policy{Default: planoutcome.WindowFixed20d}
	if got := Resolve(nil, p); got != planoutcome.WindowFixed20d {
		t.Errorf("custom default: got %v, want fixed_20d", got)
	}
}

func TestResolveHeuristicFallback(t *testing.T) {
	// Unknown tag with substring match — heuristic should
	// pick up "earnings_special_event" → next_earnings.
	got := Resolve([]string{"earnings_special_event"}, DefaultPolicy())
	if got != planoutcome.WindowNextEarnings {
		t.Errorf("got %v, want next_earnings (heuristic)", got)
	}
	got = Resolve([]string{"breaking_news_about_company"}, DefaultPolicy())
	if got != planoutcome.WindowNextNews {
		t.Errorf("got %v, want next_news (heuristic)", got)
	}
}

func TestResolveFirstMatchWins(t *testing.T) {
	// A bull-and-value plan: bull is short horizon, value is
	// long. First match wins for determinism.
	got := Resolve([]string{"momentum", "value"}, DefaultPolicy())
	if got != planoutcome.WindowFixed5d {
		t.Errorf("first-match: got %v, want fixed_5d", got)
	}
	got = Resolve([]string{"value", "momentum"}, DefaultPolicy())
	if got != planoutcome.WindowFixed20d {
		t.Errorf("first-match reversed: got %v, want fixed_20d", got)
	}
}

func TestResolveDaysForFixedWindows(t *testing.T) {
	cases := []struct {
		kind planoutcome.WindowKind
		want int
	}{
		{planoutcome.WindowFixed5d, 5},
		{planoutcome.WindowFixed10d, 10},
		{planoutcome.WindowFixed20d, 20},
		{planoutcome.WindowNextEarnings, 0},
		{planoutcome.WindowManual, 0},
	}
	for _, c := range cases {
		if got := ResolveDays(c.kind); got != c.want {
			t.Errorf("ResolveDays(%v): got %d, want %d", c.kind, got, c.want)
		}
	}
}

func TestResolveBlankTagsSkipped(t *testing.T) {
	got := Resolve([]string{"", "  ", "earnings_beat"}, DefaultPolicy())
	if got != planoutcome.WindowNextEarnings {
		t.Errorf("blank tags should not block matching: got %v", got)
	}
}
