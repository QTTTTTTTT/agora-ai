package main

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	"github.com/fundai/server/internal/ohlc"
	"github.com/fundai/server/internal/regime"
	"github.com/fundai/server/internal/repository"
)

// ---------------------------------------------------------------------------
// Stub fetcher: returns deterministic regime-shaped bars per symbol
// ---------------------------------------------------------------------------

type regimeStubFetcher struct {
	bySymbol map[string][]ohlc.Bar
}

func (s *regimeStubFetcher) Fetch(_ context.Context, req ohlc.FetchRequest) ([]ohlc.Bar, error) {
	if bars, ok := s.bySymbol[req.Symbol]; ok {
		return bars, nil
	}
	return nil, ohlc.ErrNoData
}

// makeRegimeBars rebuilds the same trend / chop fixtures used by
// the regime package's own unit tests so the wiring exercises a
// real classifier output without copying generator code.
func makeRegimeUptrend() []ohlc.Bar  { return regimeBars("trend_up") }
func makeRegimeDowntrend() []ohlc.Bar { return regimeBars("trend_down") }
func makeRegimeRange() []ohlc.Bar    { return regimeBars("range") }

func regimeBars(kind string) []ohlc.Bar {
	start := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	n := 260
	base := 100.0
	bars := make([]ohlc.Bar, n)
	for i := 0; i < n; i++ {
		var c float64
		switch kind {
		case "trend_up":
			c = base*(1+0.5*float64(i)/float64(n-1)) + 0.3*math.Sin(float64(i)/5)
		case "trend_down":
			c = base*(1-0.5*float64(i)/float64(n-1)) + 0.3*math.Sin(float64(i)/5)
		default: // range
			c = base + base*0.005*math.Sin(float64(i)/4)
		}
		bars[i] = ohlc.Bar{
			Time: start.Add(time.Duration(i) * 24 * time.Hour),
			Open: c, High: c * 1.005, Low: c * 0.995, Close: c, Volume: 1e6,
		}
	}
	return bars
}

// ---------------------------------------------------------------------------
// stampRegimeTags
// ---------------------------------------------------------------------------

func TestStampRegimeTagsAppliesClassifierResultToActions(t *testing.T) {
	fetcher := &regimeStubFetcher{
		bySymbol: map[string][]ohlc.Bar{
			"NVDA": makeRegimeUptrend(),
			"GME":  makeRegimeDowntrend(),
			"AAPL": makeRegimeRange(),
		},
	}
	agent := &runtimePMAgent{
		regimeService: regime.NewService(fetcher),
	}
	actions := []repository.PlanAction{
		{Symbol: "NVDA", Market: sql.NullString{String: "us_equity", Valid: true}},
		{Symbol: "GME", Market: sql.NullString{String: "us_equity", Valid: true}},
		{Symbol: "AAPL", Market: sql.NullString{String: "us_equity", Valid: true}},
	}
	agent.stampRegimeTags(context.Background(), actions)

	cases := map[string]string{"NVDA": "trend_up", "GME": "trend_down", "AAPL": "range"}
	for i, a := range actions {
		want := cases[a.Symbol]
		if !a.RegimeTag.Valid || a.RegimeTag.String != want {
			t.Fatalf("action[%d] %s: regime got %+v, want %q", i, a.Symbol, a.RegimeTag, want)
		}
	}
}

func TestStampRegimeTagsPreservesExistingTag(t *testing.T) {
	fetcher := &regimeStubFetcher{
		bySymbol: map[string][]ohlc.Bar{"NVDA": makeRegimeUptrend()},
	}
	agent := &runtimePMAgent{
		regimeService: regime.NewService(fetcher),
	}
	actions := []repository.PlanAction{
		{
			Symbol:    "NVDA",
			Market:    sql.NullString{String: "us_equity", Valid: true},
			RegimeTag: sql.NullString{String: "chop", Valid: true}, // pre-set by some sleeve
		},
	}
	agent.stampRegimeTags(context.Background(), actions)
	if actions[0].RegimeTag.String != "chop" {
		t.Fatalf("expected pre-set tag to be preserved, got %q", actions[0].RegimeTag.String)
	}
}

func TestStampRegimeTagsLeavesUnknownActionsAlone(t *testing.T) {
	// Stub returns no data → classifier yields Unknown → tag stays NULL.
	fetcher := &regimeStubFetcher{bySymbol: map[string][]ohlc.Bar{}}
	agent := &runtimePMAgent{
		regimeService: regime.NewService(fetcher),
	}
	actions := []repository.PlanAction{
		{Symbol: "RIVN", Market: sql.NullString{String: "us_equity", Valid: true}},
	}
	agent.stampRegimeTags(context.Background(), actions)
	if actions[0].RegimeTag.Valid {
		t.Fatalf("expected NULL tag on unknown, got %+v", actions[0].RegimeTag)
	}
}

func TestStampRegimeTagsNoopWhenServiceMissing(t *testing.T) {
	agent := &runtimePMAgent{regimeService: nil}
	actions := []repository.PlanAction{
		{Symbol: "NVDA", Market: sql.NullString{String: "us_equity", Valid: true}},
	}
	agent.stampRegimeTags(context.Background(), actions)
	if actions[0].RegimeTag.Valid {
		t.Fatal("no-op path must not touch the action")
	}
}

func TestStampRegimeTagsBatchesDuplicatesEfficiently(t *testing.T) {
	// Use a stub that counts per-symbol fetches so we can confirm
	// the dedupe path. 3 NVDA actions should produce ONE fetch.
	counts := map[string]int{}
	fetcher := &countingFetcher{
		inner: &regimeStubFetcher{
			bySymbol: map[string][]ohlc.Bar{"NVDA": makeRegimeUptrend()},
		},
		counts: counts,
	}
	agent := &runtimePMAgent{regimeService: regime.NewService(fetcher)}

	actions := []repository.PlanAction{
		{Symbol: "NVDA", Market: sql.NullString{String: "us_equity", Valid: true}},
		{Symbol: "NVDA", Market: sql.NullString{String: "us_equity", Valid: true}},
		{Symbol: "NVDA", Market: sql.NullString{String: "us_equity", Valid: true}},
	}
	agent.stampRegimeTags(context.Background(), actions)

	if counts["NVDA"] != 1 {
		t.Fatalf("expected 1 fetch for duplicated NVDA, got %d", counts["NVDA"])
	}
	for i, a := range actions {
		if !a.RegimeTag.Valid || a.RegimeTag.String != "trend_up" {
			t.Fatalf("action[%d]: regime got %+v, want trend_up", i, a.RegimeTag)
		}
	}
}

type countingFetcher struct {
	inner  ohlc.Fetcher
	counts map[string]int
}

func (f *countingFetcher) Fetch(ctx context.Context, req ohlc.FetchRequest) ([]ohlc.Bar, error) {
	f.counts[req.Symbol]++
	return f.inner.Fetch(ctx, req)
}
