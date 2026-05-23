package newsrecall

// Sprint B #3 contract tests for the newsrecall.Service.
//
// The unit tests cover:
//   - Options.withDefaults pins the production tunings (7d MaxAge,
//     3 hits per symbol, 8 fetch limit, 4 concurrency, 6s timeout)
//     and clamps every degenerate input.
//   - Nil safety: nil service, nil fetcher, empty requests all
//     return nil without panicking.
//   - Dedupe normalises symbol case + market, drops blanks, and
//     deduplicates on (symbol, market) so the wiring layer can
//     pass universe ∪ positions without filtering itself.
//   - filterAndRank drops zero-time / stale / blank-title items
//     and orders surviving items most-recent first; the cap
//     trims to MaxPerSymbol.
//   - BuildCatalysts runs per-symbol fetches and assembles the
//     output deterministically. Per-symbol fetch errors degrade
//     to "no catalysts" rather than aborting the whole call.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fundai/server/internal/marketdata"
)

// fakeFetcher is a private test double. It records every call so
// the dedupe + parallel-fetch tests can assert exactly how many
// times the upstream was hit.
type fakeFetcher struct {
	calls    int64
	bySymbol map[string][]marketdata.NewsItem
	errFor   map[string]error
}

func newFakeFetcher() *fakeFetcher {
	return &fakeFetcher{
		bySymbol: map[string][]marketdata.NewsItem{},
		errFor:   map[string]error{},
	}
}

func (f *fakeFetcher) GetNews(_ context.Context, ref marketdata.InstrumentRef, _ int) ([]marketdata.NewsItem, error) {
	atomic.AddInt64(&f.calls, 1)
	key := ref.Symbol + "|" + ref.Market
	if err, ok := f.errFor[key]; ok {
		return nil, err
	}
	return f.bySymbol[key], nil
}

func TestOptionsWithDefaultsProductionTunings(t *testing.T) {
	got := Options{}.withDefaults()
	want := Options{
		MaxAge:         7 * 24 * time.Hour,
		MaxPerSymbol:   3,
		FetchLimit:     8,
		Concurrency:    4,
		PerCallTimeout: 6 * time.Second,
	}
	if got != want {
		t.Errorf("Options{}.withDefaults() = %+v, want %+v", got, want)
	}
}

func TestOptionsWithDefaultsClampsBounds(t *testing.T) {
	got := Options{
		MaxAge:         365 * 24 * time.Hour,
		MaxPerSymbol:   100,
		FetchLimit:     2,
		Concurrency:    100,
		PerCallTimeout: 5 * time.Minute,
	}.withDefaults()
	if got.MaxAge != 30*24*time.Hour {
		t.Errorf("MaxAge ceiling: got %v, want 30d", got.MaxAge)
	}
	if got.MaxPerSymbol != 10 {
		t.Errorf("MaxPerSymbol ceiling: got %d, want 10", got.MaxPerSymbol)
	}
	// FetchLimit must be >= MaxPerSymbol after clamping; the
	// concrete value here is 10 (= MaxPerSymbol after clamp).
	if got.FetchLimit < got.MaxPerSymbol {
		t.Errorf("FetchLimit %d < MaxPerSymbol %d", got.FetchLimit, got.MaxPerSymbol)
	}
	if got.Concurrency != 16 {
		t.Errorf("Concurrency ceiling: got %d, want 16", got.Concurrency)
	}
	if got.PerCallTimeout != 30*time.Second {
		t.Errorf("PerCallTimeout ceiling: got %v, want 30s", got.PerCallTimeout)
	}
}

func TestBuildCatalystsNilSafe(t *testing.T) {
	var s *Service
	if got := s.BuildCatalysts(context.Background(), []Request{{Symbol: "A"}}, time.Now()); got != nil {
		t.Errorf("nil receiver: got %+v, want nil", got)
	}
	s2 := NewService(nil, Options{})
	if got := s2.BuildCatalysts(context.Background(), []Request{{Symbol: "A"}}, time.Now()); got != nil {
		t.Errorf("nil fetcher: got %+v, want nil", got)
	}
}

// Empty / blank-only requests short-circuit before any fetch.
func TestBuildCatalystsEmptyRequestsShortCircuits(t *testing.T) {
	f := newFakeFetcher()
	s := NewService(f, Options{})
	if got := s.BuildCatalysts(context.Background(), []Request{{Symbol: " "}, {Symbol: ""}}, time.Now()); got != nil {
		t.Errorf("empty requests: got %+v, want nil", got)
	}
	if f.calls != 0 {
		t.Errorf("fetcher called %d times for empty requests; want 0", f.calls)
	}
}

// Dedupe on (Symbol, Market). Two requests differing only by case /
// whitespace must produce a single upstream call.
func TestDedupeRequestsNormalisesAndDedupes(t *testing.T) {
	got := dedupeRequests([]Request{
		{Symbol: " aapl ", Market: "US_EQUITY"},
		{Symbol: "AAPL", Market: "us_equity"}, // dup
		{Symbol: "NVDA"},
		{Symbol: ""}, // dropped
		{Symbol: "  "},
		{Symbol: "AAPL", Market: "hk_stock"}, // same symbol, different market — kept
	})
	if len(got) != 3 {
		t.Fatalf("expected 3 unique (symbol, market) pairs, got %d (%+v)", len(got), got)
	}
	if got[0].Symbol != "AAPL" || got[0].Market != "us_equity" {
		t.Errorf("first row = %+v, want {AAPL, us_equity}", got[0])
	}
	if got[1].Symbol != "NVDA" {
		t.Errorf("second row symbol = %q, want NVDA", got[1].Symbol)
	}
	if got[2].Symbol != "AAPL" || got[2].Market != "hk_stock" {
		t.Errorf("third row = %+v, want {AAPL, hk_stock}", got[2])
	}
}

// filterAndRank drops stale + zero-time + blank-title items and
// sorts the survivors most-recent first, then caps.
func TestFilterAndRankSortsTrimsAndDrops(t *testing.T) {
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-48 * time.Hour)

	items := []marketdata.NewsItem{
		{Title: "fresh-a", PublishedAt: now.Add(-1 * time.Hour)},  // 1h old → kept
		{Title: "fresh-b", PublishedAt: now.Add(-30 * time.Hour)}, // 30h → kept
		{Title: "stale", PublishedAt: now.Add(-72 * time.Hour)},   // 72h → dropped (past cutoff)
		{Title: "no-time", PublishedAt: time.Time{}},              // zero time → dropped
		{Title: "  ", PublishedAt: now.Add(-2 * time.Hour)},       // blank title → dropped
		{Title: "fresh-c", PublishedAt: now.Add(-5 * time.Hour)},  // 5h → kept
	}
	got := filterAndRank(items, cutoff, now, 3)
	if len(got) != 3 {
		t.Fatalf("expected 3 hits after filter+cap, got %d (%+v)", len(got), got)
	}
	if got[0].Title != "fresh-a" {
		t.Errorf("got[0].Title = %q, want fresh-a (newest)", got[0].Title)
	}
	if got[1].Title != "fresh-c" {
		t.Errorf("got[1].Title = %q, want fresh-c", got[1].Title)
	}
	if got[2].Title != "fresh-b" {
		t.Errorf("got[2].Title = %q, want fresh-b", got[2].Title)
	}
	if got[0].HoursOld < 0.5 || got[0].HoursOld > 1.5 {
		t.Errorf("fresh-a HoursOld = %v, want ≈1", got[0].HoursOld)
	}
}

// firstNonEmpty test pins the language fallback order
// English → ambiguous → Chinese so an English-first prompt prefers
// the English variant when one is present.
func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", "x"); got != "x" {
		t.Errorf("first non-empty = %q, want x", got)
	}
	if got := firstNonEmpty(); got != "" {
		t.Errorf("empty variadic = %q, want empty", got)
	}
	if got := firstNonEmpty("en", "zh"); got != "en" {
		t.Errorf("en should win: got %q", got)
	}
}

// Happy path with 3 symbols, 1 stale row, 1 too-old item: assert
// the structured result preserves Symbol order from the input, drops
// the symbol that has no in-window hits, and sorts hits inside each
// SymbolCatalysts most-recent first.
func TestBuildCatalystsHappyPath(t *testing.T) {
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	f := newFakeFetcher()
	f.bySymbol["AAPL|us_equity"] = []marketdata.NewsItem{
		{Title: "AAPL fresh", Source: "reuters", PublishedAt: now.Add(-2 * time.Hour)},
		{Title: "AAPL older", Source: "wsj", PublishedAt: now.Add(-30 * time.Hour)},
	}
	f.bySymbol["NVDA|us_equity"] = []marketdata.NewsItem{
		{Title: "NVDA stale", Source: "bloomberg", PublishedAt: now.Add(-30 * 24 * time.Hour)}, // dropped
	}
	f.bySymbol["TSLA|us_equity"] = []marketdata.NewsItem{
		{Title: "TSLA breaking", Source: "ft", PublishedAt: now.Add(-1 * time.Hour)},
	}

	s := NewService(f, Options{})
	got := s.BuildCatalysts(context.Background(), []Request{
		{Symbol: "AAPL", Market: "us_equity"},
		{Symbol: "NVDA", Market: "us_equity"},
		{Symbol: "TSLA", Market: "us_equity"},
	}, now)

	if len(got) != 2 {
		t.Fatalf("expected 2 SymbolCatalysts (NVDA dropped), got %d (%+v)", len(got), got)
	}
	if got[0].Symbol != "AAPL" || got[1].Symbol != "TSLA" {
		t.Errorf("symbol order: got [%s, %s], want [AAPL, TSLA]", got[0].Symbol, got[1].Symbol)
	}
	// AAPL has both items inside the 7d window.
	if len(got[0].Hits) != 2 {
		t.Fatalf("AAPL hit count = %d, want 2", len(got[0].Hits))
	}
	if got[0].Hits[0].Title != "AAPL fresh" {
		t.Errorf("AAPL most-recent should be 'AAPL fresh', got %q", got[0].Hits[0].Title)
	}
	if got[1].Hits[0].Title != "TSLA breaking" {
		t.Errorf("TSLA hit title = %q, want TSLA breaking", got[1].Hits[0].Title)
	}
}

// Per-symbol upstream errors are downgraded to "no catalysts" for
// that symbol — the rest of the universe still surfaces.
func TestBuildCatalystsTolerantOfPerSymbolErrors(t *testing.T) {
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	f := newFakeFetcher()
	f.bySymbol["AAPL|us_equity"] = []marketdata.NewsItem{
		{Title: "AAPL ok", Source: "reuters", PublishedAt: now.Add(-1 * time.Hour)},
	}
	f.errFor["NVDA|us_equity"] = errors.New("rate limited")

	s := NewService(f, Options{})
	got := s.BuildCatalysts(context.Background(), []Request{
		{Symbol: "AAPL", Market: "us_equity"},
		{Symbol: "NVDA", Market: "us_equity"},
	}, now)
	if len(got) != 1 || got[0].Symbol != "AAPL" {
		t.Errorf("expected just AAPL, got %+v", got)
	}
}

// Cap respect: 4 in-window items + MaxPerSymbol=2 → only 2
// surface, and they're the two newest.
func TestBuildCatalystsRespectsMaxPerSymbolCap(t *testing.T) {
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	f := newFakeFetcher()
	f.bySymbol["AAPL|us_equity"] = []marketdata.NewsItem{
		{Title: "old", PublishedAt: now.Add(-5 * time.Hour)},
		{Title: "older", PublishedAt: now.Add(-20 * time.Hour)},
		{Title: "newest", PublishedAt: now.Add(-1 * time.Hour)},
		{Title: "second-newest", PublishedAt: now.Add(-2 * time.Hour)},
	}

	s := NewService(f, Options{MaxPerSymbol: 2})
	got := s.BuildCatalysts(context.Background(), []Request{
		{Symbol: "AAPL", Market: "us_equity"},
	}, now)
	if len(got) != 1 {
		t.Fatalf("expected 1 SymbolCatalysts, got %d", len(got))
	}
	if len(got[0].Hits) != 2 {
		t.Fatalf("expected 2 hits after cap, got %d (%+v)", len(got[0].Hits), got[0].Hits)
	}
	if got[0].Hits[0].Title != "newest" || got[0].Hits[1].Title != "second-newest" {
		t.Errorf("cap surface order: got [%s, %s], want [newest, second-newest]",
			got[0].Hits[0].Title, got[0].Hits[1].Title)
	}
}

// Options accessor on nil receiver.
func TestOptionsAccessorNilSafe(t *testing.T) {
	var s *Service
	if got := s.Options(); got.MaxAge != 7*24*time.Hour {
		t.Errorf("nil Options(): MaxAge = %v, want 7d", got.MaxAge)
	}
}
