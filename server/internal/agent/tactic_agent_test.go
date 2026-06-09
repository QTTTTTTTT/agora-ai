package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/cnmarketstructure"
)

func TestLoadTacticPersonasIncludesAllFour(t *testing.T) {
	ResetTacticPersonaCache()
	personas, err := LoadTacticPersonas()
	if err != nil {
		t.Fatalf("LoadTacticPersonas err: %v", err)
	}
	expected := []string{"tail_sniper", "first_limit_dip", "dragon_head", "shrink_pullback"}
	for _, key := range expected {
		if _, ok := personas[key]; !ok {
			t.Fatalf("missing persona %q in loaded set", key)
		}
	}
	for _, p := range personas {
		if p.Philosophy == "" {
			t.Fatalf("persona %q missing philosophy", p.Key)
		}
		if len(p.RedLinesRaw) == 0 {
			t.Fatalf("persona %q has no red lines defined", p.Key)
		}
	}
}

func TestTacticAgentOutsideWindowReturnsWait(t *testing.T) {
	personas, err := LoadTacticPersonas()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	persona, ok := personas["tail_sniper"]
	if !ok {
		t.Fatalf("tail_sniper missing")
	}
	loc, _ := time.LoadLocation("Asia/Shanghai")
	mockTime := time.Date(2026, 6, 7, 10, 30, 0, 0, loc) // 10:30 morning, outside 14:30-14:55

	a, err := NewTacticAgent(persona, nil, WithTacticClock(func() time.Time { return mockTime }))
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	rep, err := a.Analyze(context.Background(), TacticInput{Symbol: "600519", AsOf: mockTime})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if rep.Verdict != "WAIT_FOR_WINDOW" {
		t.Fatalf("expected WAIT_FOR_WINDOW outside window, got %q", rep.Verdict)
	}
}

func TestTacticAgentHardRiskBlocks(t *testing.T) {
	personas, _ := LoadTacticPersonas()
	persona := personas["tail_sniper"]
	loc, _ := time.LoadLocation("Asia/Shanghai")
	mockTime := time.Date(2026, 6, 7, 14, 40, 0, 0, loc)

	a, _ := NewTacticAgent(persona, nil, WithTacticClock(func() time.Time { return mockTime }))
	rep, err := a.Analyze(context.Background(), TacticInput{
		Symbol:           "600519",
		AsOf:             mockTime,
		HardRiskFailures: []string{"ST/退市风险警示"},
	})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if rep.Verdict != "SKIP" {
		t.Fatalf("expected SKIP on hard-risk, got %q", rep.Verdict)
	}
	if len(rep.RedLinesHit) == 0 {
		t.Fatalf("expected red_lines_hit to record hard risk")
	}
}

func TestTacticAgentRedLineFiresOnReopen(t *testing.T) {
	personas, _ := LoadTacticPersonas()
	persona := personas["tail_sniper"]
	loc, _ := time.LoadLocation("Asia/Shanghai")
	mockTime := time.Date(2026, 6, 7, 14, 40, 0, 0, loc)

	a, _ := NewTacticAgent(persona, nil, WithTacticClock(func() time.Time { return mockTime }))
	rep, err := a.Analyze(context.Background(), TacticInput{
		Symbol: "600519",
		AsOf:   mockTime,
		Intraday: &cnmarketstructure.IntradaySnapshot{
			Symbol:             "600519",
			DailyGainPct:       4.0,
			TurnoverRatePct:    5.0,
			VolumeRatio:        2.0,
			FloatMarketCapYi:   100,
			DistanceToMA60Pct:  5,
			UpperShadowPct:     1.0,
			LimitUpReopenCount: 1, // triggers "盘中触及涨停又开板 -> 否决"
		},
		Regime: &cnmarketstructure.MarketRegime{LimitUpCount: 50, ShanghaiIndexChangePct: 0.5},
	})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if rep.Verdict != "SKIP" {
		t.Fatalf("expected SKIP after red line, got %q", rep.Verdict)
	}
	if len(rep.RedLinesHit) == 0 {
		t.Fatalf("expected at least one red line hit")
	}
	matched := false
	for _, line := range rep.RedLinesHit {
		if strings.Contains(line, "开板") || strings.Contains(line, "炸板") {
			matched = true
		}
	}
	if !matched {
		t.Fatalf("expected 炸板/开板 red line; got %v", rep.RedLinesHit)
	}
}

func TestTacticAgentRegimeBlocksOnLowLimitUpCount(t *testing.T) {
	personas, _ := LoadTacticPersonas()
	persona := personas["tail_sniper"]
	loc, _ := time.LoadLocation("Asia/Shanghai")
	mockTime := time.Date(2026, 6, 7, 14, 40, 0, 0, loc)

	a, _ := NewTacticAgent(persona, nil, WithTacticClock(func() time.Time { return mockTime }))
	rep, _ := a.Analyze(context.Background(), TacticInput{
		Symbol:   "600519",
		AsOf:     mockTime,
		Intraday: &cnmarketstructure.IntradaySnapshot{Symbol: "600519", DailyGainPct: 3, TurnoverRatePct: 5, VolumeRatio: 2, FloatMarketCapYi: 100},
		Regime:   &cnmarketstructure.MarketRegime{LimitUpCount: 5, ShanghaiIndexChangePct: 0.2},
	})
	if rep.Verdict != "SKIP" || rep.MarketRegimePass {
		t.Fatalf("expected SKIP on regime block, got verdict=%q pass=%v", rep.Verdict, rep.MarketRegimePass)
	}
}

func TestTacticAgentPassesCleanSetup(t *testing.T) {
	personas, _ := LoadTacticPersonas()
	persona := personas["tail_sniper"]
	loc, _ := time.LoadLocation("Asia/Shanghai")
	mockTime := time.Date(2026, 6, 7, 14, 40, 0, 0, loc)

	a, _ := NewTacticAgent(persona, nil, WithTacticClock(func() time.Time { return mockTime }))
	rep, err := a.Analyze(context.Background(), TacticInput{
		Symbol:    "600519",
		AsOf:      mockTime,
		PriceLast: 100.0,
		Intraday: &cnmarketstructure.IntradaySnapshot{
			Symbol:              "600519",
			DailyGainPct:        3.5,
			TurnoverRatePct:     5.5,
			VolumeRatio:         2.0,
			FloatMarketCapYi:    150,
			DistanceToMA60Pct:   3,
			UpperShadowPct:      0.5,
			LimitUpReopenCount:  0,
			SectorName:          "白酒",
			NorthboundNetInflow: 500_000_000,
		},
		Regime:  &cnmarketstructure.MarketRegime{LimitUpCount: 50, ShanghaiIndexChangePct: 0.4},
		Sectors: []cnmarketstructure.SectorStrength{{SectorName: "白酒", ChangePct: 3.2, RankToday: 1}},
	})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if rep.Verdict != "BUY_TAIL" {
		t.Fatalf("expected BUY_TAIL, got %q (reasons=%v)", rep.Verdict, rep.KeyReasons)
	}
	if rep.EntryPriceLow == nil || rep.StopLossPrice == nil {
		t.Fatalf("expected entry / stop prices populated")
	}
	if *rep.StopLossPrice >= 100.0 {
		t.Fatalf("expected stop loss below price, got %.4f", *rep.StopLossPrice)
	}
	if !rep.MarketRegimePass {
		t.Fatalf("expected MarketRegimePass true")
	}
}

func TestAggregateTacticReports(t *testing.T) {
	reports := []TacticReport{
		{TacticKey: "tail_sniper", Verdict: "BUY_TAIL", Confidence: 70},
		{TacticKey: "first_limit_dip", Verdict: "BUY_DIP", Confidence: 60},
		{TacticKey: "dragon_head", Verdict: "SKIP", Confidence: 40},
		{TacticKey: "shrink_pullback", Verdict: "WAIT_FOR_CONFIRMATION", Confidence: 30},
	}
	view := AggregateTacticReports(reports)
	if view.BuyCount != 2 || view.SkipCount != 1 || view.WaitCount != 1 {
		t.Fatalf("expected 2 buy / 1 skip / 1 wait, got %+v", view)
	}
	if !strings.HasPrefix(view.Verdict, "BUY") && view.Verdict != "MIXED" {
		t.Fatalf("expected BUY* or MIXED, got %q", view.Verdict)
	}
}
