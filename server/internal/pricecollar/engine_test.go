package pricecollar

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fixedSource returns the same quote (or error / nil) for every
// probe. Tests construct one per case.
type fixedSource struct {
	quote *ReferenceQuote
	err   error
}

func (s fixedSource) GetReferenceQuote(_ context.Context, _ Probe) (*ReferenceQuote, error) {
	return s.quote, s.err
}

// fixedNow returns a deterministic clock function locked to t.
func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

// ----- ResolveThresholdBps -----

func TestResolveThresholdBps_MarketOverrideWins(t *testing.T) {
	opts := EngineOptions{
		OverrideThresholdBpsByMarket: map[string]int{"a_share": 500},
	}
	bps := ResolveThresholdBps(opts, Probe{Market: "a_share", Symbol: "600519", AssetClass: "equity"})
	if bps != 500 {
		t.Fatalf("override should win, got %d", bps)
	}
}

func TestResolveThresholdBps_AShareWideBoardSymbols(t *testing.T) {
	cases := []struct {
		name    string
		symbol  string
		wantBps int
	}{
		{"chinext 300", "300750", aShareWideBoardThresholdBps},
		{"chinext 301", "301308", aShareWideBoardThresholdBps},
		{"star 688", "688981", aShareWideBoardThresholdBps},
		{"star 689", "689009", aShareWideBoardThresholdBps},
		{"bse 8", "832000", aShareWideBoardThresholdBps},
		{"main 600", "600519", aShareMainBoardThresholdBps},
		{"main 000", "000001", aShareMainBoardThresholdBps},
		{"main 002", "002230", aShareMainBoardThresholdBps},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bps := ResolveThresholdBps(EngineOptions{}, Probe{
				Market:     "a_share",
				Symbol:     tc.symbol,
				AssetClass: "equity",
			})
			if bps != tc.wantBps {
				t.Fatalf("symbol=%s want %d, got %d", tc.symbol, tc.wantBps, bps)
			}
		})
	}
}

func TestResolveThresholdBps_AssetClassDefaults(t *testing.T) {
	cases := []struct {
		assetClass string
		want       int
	}{
		{"equity", 1500},
		{"futures", 2000},
		{"crypto", 3000},
		{"bond", 3000},
		// Unknown asset class falls back to the equity-flavored
		// safety net rather than rejecting outright.
		{"mystery", fallbackThresholdBps},
	}
	for _, tc := range cases {
		t.Run(tc.assetClass, func(t *testing.T) {
			bps := ResolveThresholdBps(EngineOptions{}, Probe{
				Market:     "us_equity",
				Symbol:     "AAPL",
				AssetClass: tc.assetClass,
			})
			if bps != tc.want {
				t.Fatalf("assetClass=%s want %d, got %d", tc.assetClass, tc.want, bps)
			}
		})
	}
}

// ----- Check decision paths -----

func TestEngineCheck_NilEngine(t *testing.T) {
	var e *Engine
	if _, err := e.Check(context.Background(), Probe{InstrumentKey: "k"}); err == nil {
		t.Fatal("nil engine must return error")
	}
}

func TestEngineCheck_InvalidProbe(t *testing.T) {
	e, _ := NewEngine(NoOpReferenceSource{}, EngineOptions{})
	_, err := e.Check(context.Background(), Probe{IntendedPrice: 10})
	if err == nil || !errors.Is(err, ErrInvalidProbe) {
		t.Fatalf("want ErrInvalidProbe, got %v", err)
	}
}

func TestEngineCheck_MarketOrderAlwaysAllow(t *testing.T) {
	// Even a hostile reference source would be irrelevant — the
	// engine must short-circuit before calling it. We use a source
	// that PANICs to assert it's never invoked.
	e, _ := NewEngine(panicSource{}, EngineOptions{})
	res, err := e.Check(context.Background(), Probe{
		InstrumentKey: "k", Symbol: "AAPL", IntendedPrice: 0, Side: "buy", Quantity: 100,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Decision != DecisionAllow {
		t.Fatalf("market order must allow, got %s", res.Decision)
	}
	if len(res.Events) != 0 {
		t.Fatalf("market order must emit no events, got %+v", res.Events)
	}
}

func TestEngineCheck_AllowWithinCollar(t *testing.T) {
	now := time.Date(2026, 6, 3, 1, 30, 0, 0, time.UTC)
	src := fixedSource{quote: &ReferenceQuote{
		InstrumentKey: "SZSE:301308", Symbol: "301308",
		Price: 500, AsOf: now.Add(-1 * time.Minute),
	}}
	e, _ := NewEngine(src, EngineOptions{Now: fixedNow(now)})

	// 500 * (1 + 5%) = 525 → well inside the 21% ChiNext collar.
	res, err := e.Check(context.Background(), Probe{
		InstrumentKey: "SZSE:301308", Symbol: "301308", Market: "a_share",
		AssetClass: "equity", Side: "buy", Quantity: 100,
		IntendedPrice: 525,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Decision != DecisionAllow {
		t.Fatalf("want Allow, got %s; events=%+v", res.Decision, res.Events)
	}
	if res.AppliedThresholdBps != aShareWideBoardThresholdBps {
		t.Fatalf("ChiNext should resolve to wide board, got bps=%d", res.AppliedThresholdBps)
	}
}

func TestEngineCheck_RejectAt96226CNYRegression(t *testing.T) {
	// This is the literal scenario from 2026-06-02: 301308 limit
	// price 96,226.42 vs true mid ~500. The collar must reject.
	now := time.Date(2026, 6, 3, 1, 30, 0, 0, time.UTC)
	src := fixedSource{quote: &ReferenceQuote{
		InstrumentKey: "SZSE:301308", Symbol: "301308",
		Price: 500, AsOf: now.Add(-30 * time.Second),
	}}
	e, _ := NewEngine(src, EngineOptions{Now: fixedNow(now)})

	res, err := e.Check(context.Background(), Probe{
		InstrumentKey: "SZSE:301308", Symbol: "301308", Market: "a_share",
		AssetClass: "equity", Side: "buy", Quantity: 1,
		IntendedPrice: 96226.4188,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Decision != DecisionReject {
		t.Fatalf("the 96,226 vs 500 case MUST reject (this is the bug we're guarding against); got %s, events=%+v", res.Decision, res.Events)
	}
	if len(res.Events) != 1 || res.Events[0].RuleCode != RulePriceCollar {
		t.Fatalf("want exactly one price_collar event, got %+v", res.Events)
	}
	// Summary should mention both prices so an on-call operator can
	// understand the reject without opening the metadata blob.
	for _, needle := range []string{"96226", "500", "fat-finger"} {
		if !strings.Contains(res.Events[0].Summary, needle) {
			t.Fatalf("reject summary missing %q: %s", needle, res.Events[0].Summary)
		}
	}
}

func TestEngineCheck_RejectJustAboveThreshold(t *testing.T) {
	// US equity collar is 1,500 bps = 15%. Reference 100; intended
	// 116 → 16% deviation → reject. Intended 114 → 14% → allow.
	now := time.Date(2026, 6, 3, 14, 0, 0, 0, time.UTC)
	src := fixedSource{quote: &ReferenceQuote{
		Symbol: "AAPL", Price: 100, AsOf: now.Add(-1 * time.Minute),
	}}
	e, _ := NewEngine(src, EngineOptions{Now: fixedNow(now)})

	rejectRes, _ := e.Check(context.Background(), Probe{
		InstrumentKey: "us:AAPL", Symbol: "AAPL", Market: "us_equity",
		AssetClass: "equity", Side: "buy", Quantity: 1,
		IntendedPrice: 116,
	})
	if rejectRes.Decision != DecisionReject {
		t.Fatalf("116 vs 100 (16%%) must reject under 15%% collar, got %s", rejectRes.Decision)
	}

	allowRes, _ := e.Check(context.Background(), Probe{
		InstrumentKey: "us:AAPL", Symbol: "AAPL", Market: "us_equity",
		AssetClass: "equity", Side: "buy", Quantity: 1,
		IntendedPrice: 114,
	})
	if allowRes.Decision != DecisionAllow {
		t.Fatalf("114 vs 100 (14%%) must allow under 15%% collar, got %s", allowRes.Decision)
	}
}

func TestEngineCheck_NoReferenceDefaultsToWarn(t *testing.T) {
	now := time.Date(2026, 6, 3, 14, 0, 0, 0, time.UTC)
	e, _ := NewEngine(NoOpReferenceSource{}, EngineOptions{Now: fixedNow(now)})

	res, err := e.Check(context.Background(), Probe{
		InstrumentKey: "x", Symbol: "X", IntendedPrice: 10, Side: "buy",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Decision != DecisionWarn {
		t.Fatalf("no-reference default must be Warn, got %s", res.Decision)
	}
	if len(res.Events) != 1 || res.Events[0].RuleCode != RuleNoReference {
		t.Fatalf("want one no_reference event, got %+v", res.Events)
	}
}

func TestEngineCheck_NoReferenceStrictRejects(t *testing.T) {
	now := time.Date(2026, 6, 3, 14, 0, 0, 0, time.UTC)
	e, _ := NewEngine(NoOpReferenceSource{}, EngineOptions{
		Now:                 fixedNow(now),
		NoReferenceDecision: DecisionReject,
	})

	res, _ := e.Check(context.Background(), Probe{
		InstrumentKey: "x", Symbol: "X", IntendedPrice: 10, Side: "buy",
	})
	if res.Decision != DecisionReject {
		t.Fatalf("strict mode must reject no-reference orders, got %s", res.Decision)
	}
}

func TestEngineCheck_StaleReferenceDowngrades(t *testing.T) {
	now := time.Date(2026, 6, 3, 14, 0, 0, 0, time.UTC)
	// Reference quote is 30 min old; max age 10 min → stale.
	src := fixedSource{quote: &ReferenceQuote{
		Symbol: "AAPL", Price: 100, AsOf: now.Add(-30 * time.Minute),
	}}
	e, _ := NewEngine(src, EngineOptions{
		Now:                 fixedNow(now),
		MaxReferenceAge:     10 * time.Minute,
		NoReferenceDecision: DecisionWarn,
	})

	res, _ := e.Check(context.Background(), Probe{
		InstrumentKey: "x", Symbol: "AAPL", IntendedPrice: 10, Side: "buy", AssetClass: "equity",
	})
	if res.Decision != DecisionWarn {
		t.Fatalf("stale reference must warn, got %s", res.Decision)
	}
	if len(res.Events) != 1 {
		t.Fatalf("want 1 event, got %+v", res.Events)
	}
	if res.Events[0].RuleCode != RuleNoReference {
		t.Fatalf("want no_reference rule for stale path, got %s", res.Events[0].RuleCode)
	}
	if r, ok := res.Events[0].Metadata["reason"].(string); !ok || r != "stale" {
		t.Fatalf("want reason=stale, got %+v", res.Events[0].Metadata)
	}
}

func TestEngineCheck_SourceErrorFailsOpen(t *testing.T) {
	now := time.Date(2026, 6, 3, 14, 0, 0, 0, time.UTC)
	src := fixedSource{err: errors.New("db down")}
	e, _ := NewEngine(src, EngineOptions{Now: fixedNow(now)})

	res, err := e.Check(context.Background(), Probe{
		InstrumentKey: "x", Symbol: "X", IntendedPrice: 10, Side: "buy",
	})
	if err != nil {
		t.Fatalf("source error must be swallowed (fail-open), got %v", err)
	}
	if res.Decision != DecisionWarn {
		t.Fatalf("source error must warn (fail-open), got %s", res.Decision)
	}
	if len(res.Events) != 1 || res.Events[0].RuleCode != RuleNoReference {
		t.Fatalf("want no_reference warn on source error, got %+v", res.Events)
	}
}

func TestNewEngine_NilSourceReturnsError(t *testing.T) {
	if _, err := NewEngine(nil, EngineOptions{}); err == nil {
		t.Fatal("NewEngine(nil) must fail loudly")
	}
}

// panicSource panics on every call. Used to prove the engine
// short-circuits before invoking the source on market orders.
type panicSource struct{}

func (panicSource) GetReferenceQuote(_ context.Context, _ Probe) (*ReferenceQuote, error) {
	panic("source should not be called")
}
