package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// stubLLM returns a fixed reply unless err is set.
type stubLLM struct {
	reply string
	err   error
	gotSys, gotUser string
}

func (s *stubLLM) Complete(_ context.Context, sys, user string) (string, error) {
	s.gotSys, s.gotUser = sys, user
	if s.err != nil {
		return "", s.err
	}
	return s.reply, nil
}

func TestResearcherAgent_ProduceBrief_Bullish(t *testing.T) {
	llm := &stubLLM{reply: "AAPL is set up for a leg higher into earnings."}
	r := NewResearcherAgent("a1", "Alice", "fund-1", llm,
		WithResearcherFocus(FocusStock),
		WithResearcherClock(func() time.Time { return time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC) }),
	)
	mc := MarketContext{
		Symbol:      "AAPL",
		Market:      "NASDAQ",
		AssetClass:  "equity",
		PriceLast:   200,
		PriceChange: 0.03,
		Volume:      2_000_000,
		AvgVolume:   500_000,
		Signals:     map[string]float64{"momentum": 0.6, "rsi": 0.7},
		Headlines:   []string{"Q1 beat expectations"},
	}
	b, err := r.ProduceBrief(context.Background(), mc)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	if b.Direction != DirectionBullish {
		t.Errorf("direction: got %s want bullish", b.Direction)
	}
	if b.Confidence < 20 || b.Confidence > 100 {
		t.Errorf("confidence out of band: %d", b.Confidence)
	}
	if b.Thesis != llm.reply {
		t.Errorf("thesis: got %q want %q", b.Thesis, llm.reply)
	}
	if b.PriceTarget <= 200 {
		t.Errorf("bullish price target should be > last price, got %v", b.PriceTarget)
	}
	if b.HorizonDays != 20 {
		t.Errorf("default stock horizon: got %d want 20", b.HorizonDays)
	}
	if !containsAny(b.Catalysts, "Q1 beat expectations") {
		t.Errorf("expected headline carried as catalyst, got %v", b.Catalysts)
	}
	if !containsAny(b.Catalysts, "volume spike") {
		t.Errorf("expected volume spike catalyst, got %v", b.Catalysts)
	}
	if !b.GeneratedAt.Equal(time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("clock not honoured: %v", b.GeneratedAt)
	}
}

func TestResearcherAgent_ProduceBrief_BearishFromSignals(t *testing.T) {
	r := NewResearcherAgent("a1", "Alice", "fund-1", nil)
	mc := MarketContext{
		Symbol:      "BAD",
		PriceLast:   50,
		PriceChange: -0.02,
		Signals:     map[string]float64{"trend": -0.7, "rsi": -0.6},
	}
	b, err := r.ProduceBrief(context.Background(), mc)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	if b.Direction != DirectionBearish {
		t.Errorf("direction: got %s want bearish", b.Direction)
	}
	if b.PriceTarget >= 50 {
		t.Errorf("bearish price target should be < last price, got %v", b.PriceTarget)
	}
	// At least one risk from negative signals
	if !containsAny(b.Risks, "weak") {
		t.Errorf("expected weak-signal risk, got %v", b.Risks)
	}
}

func TestResearcherAgent_ProduceBrief_NeutralWhenFlat(t *testing.T) {
	r := NewResearcherAgent("a1", "Alice", "fund-1", nil)
	mc := MarketContext{Symbol: "FLAT", PriceLast: 100, PriceChange: 0.0001}
	b, err := r.ProduceBrief(context.Background(), mc)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	if b.Direction != DirectionNeutral {
		t.Errorf("expected neutral, got %s", b.Direction)
	}
	if b.PriceTarget != 0 {
		t.Errorf("neutral should have no price target, got %v", b.PriceTarget)
	}
}

func TestResearcherAgent_FallsBackWhenLLMFails(t *testing.T) {
	llm := &stubLLM{err: errors.New("rate limit")}
	r := NewResearcherAgent("a1", "Alice", "fund-1", llm)
	b, err := r.ProduceBrief(context.Background(), MarketContext{
		Symbol:      "NVDA",
		PriceLast:   500,
		PriceChange: 0.04,
		Headlines:   []string{"AI capex up"},
	})
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	if !strings.Contains(b.Thesis, "NVDA") {
		t.Errorf("fallback thesis should mention symbol, got %q", b.Thesis)
	}
	if !strings.Contains(b.Thesis, "AI capex up") {
		t.Errorf("fallback should include headline, got %q", b.Thesis)
	}
}

func TestResearcherAgent_NoSymbolError(t *testing.T) {
	r := NewResearcherAgent("a1", "Alice", "fund-1", nil)
	if _, err := r.ProduceBrief(context.Background(), MarketContext{}); err == nil {
		t.Fatal("expected error for missing symbol")
	}
}

func TestResearcherAgent_ProduceBriefs_SkipsBadSymbols(t *testing.T) {
	r := NewResearcherAgent("a1", "Alice", "fund-1", nil)
	got := r.ProduceBriefs(context.Background(), []MarketContext{
		{Symbol: "GOOD1", PriceLast: 10, PriceChange: 0.02},
		{Symbol: ""}, // skipped
		{Symbol: "GOOD2", PriceLast: 20, PriceChange: -0.02},
	})
	if len(got) != 2 {
		t.Fatalf("expected 2 briefs, got %d", len(got))
	}
	if got[0].Symbol != "GOOD1" || got[1].Symbol != "GOOD2" {
		t.Errorf("unexpected order/symbols: %#v", got)
	}
}

func TestResearchBriefValidate(t *testing.T) {
	bad := ResearchBrief{Symbol: "X", Direction: "side", Confidence: 50}
	if err := bad.Validate(); err == nil {
		t.Error("expected invalid direction")
	}
	bad2 := ResearchBrief{Symbol: "X", Direction: DirectionBullish, Confidence: 150}
	if err := bad2.Validate(); err == nil {
		t.Error("expected invalid confidence")
	}
	bad3 := ResearchBrief{Symbol: "", Direction: DirectionBullish, Confidence: 50}
	if err := bad3.Validate(); err == nil {
		t.Error("expected invalid symbol")
	}
	good := ResearchBrief{Symbol: "X", Direction: DirectionBullish, Confidence: 50, Focus: FocusStock}
	if err := good.Validate(); err != nil {
		t.Errorf("expected valid, got %v", err)
	}
}

func TestResearchBriefToOpinion(t *testing.T) {
	b := ResearchBrief{
		AgentID:     "a1",
		AgentName:   "Alice",
		Focus:       FocusFundamental,
		Symbol:      "X",
		Direction:   DirectionBullish,
		Thesis:      "good biz",
		Catalysts:   []string{"earnings"},
		Risks:       []string{"valuation"},
		PriceTarget: 100,
		HorizonDays: 60,
		DataPoints:  []DataPoint{{Name: "pe", Value: "12"}},
	}
	op := b.ToOpinion()
	if op.AgentID != "a1" || op.Symbol != "X" || op.Direction != "bullish" {
		t.Errorf("opinion fields wrong: %#v", op)
	}
	if op.Focus != "fundamental" {
		t.Errorf("focus: %s", op.Focus)
	}
	if op.DataPoints["pe"] != "12" {
		t.Errorf("expected pe data point, got %#v", op.DataPoints)
	}
	if op.DataPoints["catalysts"] != "earnings" {
		t.Errorf("expected catalysts joined, got %#v", op.DataPoints["catalysts"])
	}
	if op.DataPoints["price_target"].(float64) != 100 {
		t.Errorf("expected price_target=100, got %#v", op.DataPoints["price_target"])
	}
}

func TestResearcherAgent_LifecycleAndAccessors(t *testing.T) {
	r := NewResearcherAgent("a1", "Alice", "fund-1", nil,
		WithResearcherFocus(FocusMacro),
		WithResearcherPersona("hawkish on inflation"),
	)
	if r.ID() != "a1" || r.Name() != "Alice" || r.Focus() != FocusMacro {
		t.Errorf("accessor mismatch")
	}
	r.Start()
	r.Start() // idempotent
	r.Stop()
	r.Stop() // idempotent
}

func TestResearcherAgent_PersonaAppearsInPrompt(t *testing.T) {
	llm := &stubLLM{reply: "ok"}
	r := NewResearcherAgent("a1", "Alice", "fund-1", llm,
		WithResearcherPersona("a deeply skeptical contrarian"),
	)
	_, err := r.ProduceBrief(context.Background(), MarketContext{Symbol: "X", PriceLast: 1, PriceChange: 0.05})
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	if !strings.Contains(llm.gotSys, "deeply skeptical contrarian") {
		t.Errorf("persona missing from system prompt: %q", llm.gotSys)
	}
}

func TestDefaultHorizonForFocus(t *testing.T) {
	cases := map[ResearchFocus]int{
		FocusMacro:       90,
		FocusFundamental: 60,
		FocusQuant:       5,
		FocusStock:       20,
		"unknown":        20,
	}
	for f, want := range cases {
		if got := defaultHorizonForFocus(f); got != want {
			t.Errorf("focus=%s: got %d want %d", f, got, want)
		}
	}
}

func containsAny(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
