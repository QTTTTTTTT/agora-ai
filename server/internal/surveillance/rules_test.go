package surveillance

import (
	"testing"
	"time"
)

func tradeAt(id, fund, sym, side string, qty, price float64, t time.Time) TradeSnapshot {
	return TradeSnapshot{
		ID:         id,
		FundID:     fund,
		Symbol:     sym,
		Side:       side,
		Quantity:   qty,
		Price:      price,
		Notional:   qty * price,
		ExecutedAt: t,
		Status:     "filled",
	}
}

// ----- WashTradeRule -----

func TestWashTrade_BuySellBuy_FiresWarning(t *testing.T) {
	r := NewWashTradeRule(DefaultWashTradeOptions)
	t0 := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	snap := []TradeSnapshot{
		tradeAt("a", "f1", "AAPL", "buy", 100, 175, t0),
		tradeAt("b", "f1", "AAPL", "sell", 100, 175, t0.Add(2*time.Minute)),
		tradeAt("c", "f1", "AAPL", "buy", 100, 175, t0.Add(5*time.Minute)),
	}
	out := r.Detect(snap, nil)
	if len(out) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(out), out)
	}
	if out[0].RuleCode != RuleWashTrade || out[0].Severity != SeverityWarning {
		t.Errorf("got %+v", out[0])
	}
	if len(out[0].TradeIDs) != 3 {
		t.Errorf("trade ids = %v", out[0].TradeIDs)
	}
}

func TestWashTrade_OutsideWindow_NoFire(t *testing.T) {
	r := NewWashTradeRule(DefaultWashTradeOptions)
	t0 := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	snap := []TradeSnapshot{
		tradeAt("a", "f1", "AAPL", "buy", 100, 175, t0),
		tradeAt("b", "f1", "AAPL", "sell", 100, 175, t0.Add(5*time.Minute)),
		// Third leg 11 min past first → outside 10m window.
		tradeAt("c", "f1", "AAPL", "buy", 100, 175, t0.Add(11*time.Minute)),
	}
	out := r.Detect(snap, nil)
	if len(out) != 0 {
		t.Errorf("expected no events, got %+v", out)
	}
}

func TestWashTrade_QuantityMismatch_NoFire(t *testing.T) {
	r := NewWashTradeRule(DefaultWashTradeOptions)
	t0 := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	snap := []TradeSnapshot{
		tradeAt("a", "f1", "AAPL", "buy", 100, 175, t0),
		tradeAt("b", "f1", "AAPL", "sell", 50, 175, t0.Add(2*time.Minute)),
		tradeAt("c", "f1", "AAPL", "buy", 100, 175, t0.Add(5*time.Minute)),
	}
	out := r.Detect(snap, nil)
	if len(out) != 0 {
		t.Errorf("qty mismatch should not fire: %+v", out)
	}
}

func TestWashTrade_CrossFundIsolation(t *testing.T) {
	r := NewWashTradeRule(DefaultWashTradeOptions)
	t0 := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	snap := []TradeSnapshot{
		tradeAt("a", "f1", "AAPL", "buy", 100, 175, t0),
		tradeAt("b", "f2", "AAPL", "sell", 100, 175, t0.Add(1*time.Minute)),
		tradeAt("c", "f1", "AAPL", "buy", 100, 175, t0.Add(2*time.Minute)),
	}
	out := r.Detect(snap, nil)
	if len(out) != 0 {
		t.Errorf("cross-fund triplet must not fire: %+v", out)
	}
}

func TestWashTrade_Idempotent(t *testing.T) {
	r := NewWashTradeRule(DefaultWashTradeOptions)
	t0 := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	snap := []TradeSnapshot{
		tradeAt("a", "f1", "AAPL", "buy", 100, 175, t0),
		tradeAt("b", "f1", "AAPL", "sell", 100, 175, t0.Add(1*time.Minute)),
		tradeAt("c", "f1", "AAPL", "buy", 100, 175, t0.Add(2*time.Minute)),
	}
	a := r.Detect(snap, nil)
	b := r.Detect(snap, nil)
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("expected 1 each, got %d / %d", len(a), len(b))
	}
	if a[0].Fingerprint != b[0].Fingerprint {
		t.Errorf("fingerprint not stable: %q vs %q", a[0].Fingerprint, b[0].Fingerprint)
	}
}

func TestWashTrade_BelowMinNotional_NoFire(t *testing.T) {
	r := NewWashTradeRule(WashTradeOptions{
		Window:         10 * time.Minute,
		QuantityRelTol: 0.05,
		MinNotional:    50_000,
	})
	t0 := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	snap := []TradeSnapshot{
		tradeAt("a", "f1", "AAPL", "buy", 1, 100, t0),
		tradeAt("b", "f1", "AAPL", "sell", 1, 100, t0.Add(1*time.Minute)),
		tradeAt("c", "f1", "AAPL", "buy", 1, 100, t0.Add(2*time.Minute)),
	}
	if got := r.Detect(snap, nil); len(got) != 0 {
		t.Errorf("min notional must filter penny pattern: %+v", got)
	}
}

// ----- MarkingCloseRule -----

func newMarketContext(close time.Time) *MarketContext {
	return &MarketContext{
		SessionClose:     close,
		AvgDailyNotional: map[string]float64{"AAPL": 1_000_000},
		RecentVWAP:       map[string]float64{"AAPL": 175},
	}
}

func TestMarkingClose_BigSize_NearClose_Fires(t *testing.T) {
	r := NewMarkingCloseRule(DefaultMarkingCloseOptions)
	close := time.Date(2026, 6, 1, 20, 0, 0, 0, time.UTC) // 4PM ET
	ctx := newMarketContext(close)
	snap := []TradeSnapshot{
		// 5 minutes before close, $100k → 10% of avg daily.
		tradeAt("a", "f1", "AAPL", "buy", 1000, 100, close.Add(-5*time.Minute)),
	}
	// match the snap helper math: notional was set wrong above
	snap[0].Notional = 100_000
	out := r.Detect(snap, ctx)
	if len(out) != 1 {
		t.Fatalf("expected 1 event, got %+v", out)
	}
	if out[0].Severity != SeverityWarning && out[0].Severity != SeverityCritical {
		t.Errorf("severity = %s", out[0].Severity)
	}
}

func TestMarkingClose_NoSessionClose_NoFire(t *testing.T) {
	r := NewMarkingCloseRule(DefaultMarkingCloseOptions)
	snap := []TradeSnapshot{
		tradeAt("a", "f1", "AAPL", "buy", 1000, 100, time.Now()),
	}
	out := r.Detect(snap, &MarketContext{})
	if len(out) != 0 {
		t.Errorf("nil session close must produce no events: %+v", out)
	}
}

func TestMarkingClose_OutsideWindow_NoFire(t *testing.T) {
	r := NewMarkingCloseRule(DefaultMarkingCloseOptions)
	close := time.Date(2026, 6, 1, 20, 0, 0, 0, time.UTC)
	ctx := newMarketContext(close)
	snap := []TradeSnapshot{
		// 30 minutes before close — outside 15m window.
		tradeAt("a", "f1", "AAPL", "buy", 1000, 100, close.Add(-30*time.Minute)),
	}
	out := r.Detect(snap, ctx)
	if len(out) != 0 {
		t.Errorf("expected no events, got %+v", out)
	}
}

func TestMarkingClose_VwapDeviation_Fires(t *testing.T) {
	r := NewMarkingCloseRule(DefaultMarkingCloseOptions)
	close := time.Date(2026, 6, 1, 20, 0, 0, 0, time.UTC)
	ctx := newMarketContext(close)
	// Tiny size but +1% off vwap — flag on price half.
	snap := []TradeSnapshot{
		tradeAt("a", "f1", "AAPL", "buy", 5, 176.75, close.Add(-2*time.Minute)),
	}
	out := r.Detect(snap, ctx)
	if len(out) != 1 {
		t.Fatalf("expected 1 event, got %+v", out)
	}
}

func TestMarkingClose_RequireBoth_OneFlagOnly_NoFire(t *testing.T) {
	r := NewMarkingCloseRule(MarkingCloseOptions{
		CloseWindow:            15 * time.Minute,
		SizeRatioThreshold:     0.05,
		VWAPDeviationThreshold: 0.005,
		RequireBoth:            true,
	})
	close := time.Date(2026, 6, 1, 20, 0, 0, 0, time.UTC)
	ctx := newMarketContext(close)
	snap := []TradeSnapshot{
		tradeAt("a", "f1", "AAPL", "buy", 5, 176.75, close.Add(-2*time.Minute)),
	}
	if got := r.Detect(snap, ctx); len(got) != 0 {
		t.Errorf("RequireBoth must filter single-flag: %+v", got)
	}
}

// ----- SelfTradePairRule -----

func TestSelfTradePair_TightWindowFires(t *testing.T) {
	r := NewSelfTradePairRule(DefaultSelfTradePairOptions)
	t0 := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	snap := []TradeSnapshot{
		tradeAt("a", "f1", "AAPL", "buy", 100, 175.50, t0),
		tradeAt("b", "f1", "AAPL", "sell", 100, 175.50, t0.Add(2*time.Second)),
	}
	out := r.Detect(snap, nil)
	if len(out) != 1 {
		t.Fatalf("expected 1 event, got %+v", out)
	}
	if out[0].Severity != SeverityCritical {
		t.Errorf("severity = %s, want critical", out[0].Severity)
	}
}

func TestSelfTradePair_OutsideWindow_NoFire(t *testing.T) {
	r := NewSelfTradePairRule(DefaultSelfTradePairOptions)
	t0 := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	snap := []TradeSnapshot{
		tradeAt("a", "f1", "AAPL", "buy", 100, 175.50, t0),
		tradeAt("b", "f1", "AAPL", "sell", 100, 175.50, t0.Add(1*time.Minute)),
	}
	out := r.Detect(snap, nil)
	if len(out) != 0 {
		t.Errorf("expected no events, got %+v", out)
	}
}

func TestSelfTradePair_DifferentPrice_NoFire(t *testing.T) {
	r := NewSelfTradePairRule(DefaultSelfTradePairOptions)
	t0 := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	snap := []TradeSnapshot{
		tradeAt("a", "f1", "AAPL", "buy", 100, 175.50, t0),
		tradeAt("b", "f1", "AAPL", "sell", 100, 175.55, t0.Add(2*time.Second)),
	}
	if got := r.Detect(snap, nil); len(got) != 0 {
		t.Errorf("price mismatch should not fire: %+v", got)
	}
}

func TestSelfTradePair_SameSide_NoFire(t *testing.T) {
	r := NewSelfTradePairRule(DefaultSelfTradePairOptions)
	t0 := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	snap := []TradeSnapshot{
		tradeAt("a", "f1", "AAPL", "buy", 100, 175.50, t0),
		tradeAt("b", "f1", "AAPL", "buy", 100, 175.50, t0.Add(1*time.Second)),
	}
	if got := r.Detect(snap, nil); len(got) != 0 {
		t.Errorf("same side must not fire: %+v", got)
	}
}

// ----- Engine dedup -----

func TestEngine_Dedup_SelfTradeBeatsWash(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	// Construct a 2-leg pair that satisfies self-trade. Wash
	// rule needs 3 legs so it won't fire here, but the test
	// guarantees the engine emits the self-trade event.
	snap := []TradeSnapshot{
		tradeAt("a", "f1", "AAPL", "buy", 100, 175, t0),
		tradeAt("b", "f1", "AAPL", "sell", 100, 175, t0.Add(2*time.Second)),
	}
	e := NewEngine(DefaultRules()...)
	res := e.Run(snap, &MarketContext{})
	if len(res.Events) != 1 || res.Events[0].RuleCode != RuleSelfTradePair {
		t.Errorf("got %+v", res.Events)
	}
}

func TestEngine_Empty_NoEvents(t *testing.T) {
	e := NewEngine(DefaultRules()...)
	res := e.Run(nil, &MarketContext{})
	if len(res.Events) != 0 {
		t.Errorf("expected empty: %+v", res.Events)
	}
}

func TestEngine_FilterCancelled(t *testing.T) {
	e := NewEngine(NewSelfTradePairRule(DefaultSelfTradePairOptions))
	t0 := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	snap := []TradeSnapshot{
		{ID: "a", FundID: "f1", Symbol: "AAPL", Side: "buy", Quantity: 100, Price: 175, ExecutedAt: t0, Status: "cancelled"},
		{ID: "b", FundID: "f1", Symbol: "AAPL", Side: "sell", Quantity: 100, Price: 175, ExecutedAt: t0.Add(time.Second), Status: "cancelled"},
	}
	res := e.Run(snap, nil)
	if len(res.Events) != 0 {
		t.Errorf("cancelled trades must be ignored: %+v", res.Events)
	}
}

func TestFingerprint_StableAcrossOrder(t *testing.T) {
	a := fingerprintFor("f1", RuleWashTrade, []string{"a", "b", "c"})
	b := fingerprintFor("f1", RuleWashTrade, []string{"c", "a", "b"})
	if a != b {
		t.Errorf("fingerprint not order-stable: %q vs %q", a, b)
	}
}
