package exitmanager

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/fundai/server/internal/repository"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func openedAt(daysAgo int) time.Time {
	return time.Date(2026, time.May, 14, 9, 30, 0, 0, time.UTC).Add(-time.Duration(daysAgo) * 24 * time.Hour)
}

func lotFromEntry(id string, entry, qty float64, daysAgo int) *repository.PositionLotRow {
	return &repository.PositionLotRow{
		ID:                id,
		EntryPrice:        entry,
		QuantityOpened:    qty,
		QuantityRemaining: qty,
		OpenedAt:          openedAt(daysAgo),
	}
}

func withHigh(lot *repository.PositionLotRow, high float64) *repository.PositionLotRow {
	lot.HighestPriceSeen = sql.NullFloat64{Float64: high, Valid: true}
	return lot
}

func staticClock(t time.Time) Option {
	return WithClock(func() time.Time { return t })
}

// ---------------------------------------------------------------------------
// Policy decoding + clamping
// ---------------------------------------------------------------------------

func TestPolicyFromFundConfigDecodesNestedShape(t *testing.T) {
	raw := json.RawMessage(`{
		"exitPolicy": {
			"enabled": true,
			"stopLoss":   {"percent": 0.08},
			"takeProfit": {"percent": 0.20},
			"trailing":   {"percent": 0.12},
			"timeStop":   {"maxHoldingDays": 30}
		},
		"market": "us_equity"
	}`)
	p := PolicyFromFundConfig(raw)
	if !p.Enabled {
		t.Fatalf("expected enabled, got %+v", p)
	}
	if p.StopLoss == nil || p.StopLoss.Percent != 0.08 {
		t.Fatalf("stop_loss: got %+v, want 0.08", p.StopLoss)
	}
	if p.TakeProfit == nil || p.TakeProfit.Percent != 0.20 {
		t.Fatalf("take_profit: got %+v, want 0.20", p.TakeProfit)
	}
	if p.Trailing == nil || p.Trailing.Percent != 0.12 {
		t.Fatalf("trailing: got %+v, want 0.12", p.Trailing)
	}
	if p.TimeStop == nil || p.TimeStop.MaxHoldingDays != 30 {
		t.Fatalf("time_stop: got %+v, want 30", p.TimeStop)
	}
}

func TestPolicyFromFundConfigTreatsMissingOrInvalidAsDisabled(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"null", "null"},
		{"no exit policy", `{"market":"us_equity"}`},
		{"malformed", `{not-json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := PolicyFromFundConfig(json.RawMessage(tc.raw))
			if p.Enabled {
				t.Fatalf("expected disabled, got %+v", p)
			}
			if p.HasAnyRule() {
				t.Fatal("disabled policy must report HasAnyRule=false")
			}
		})
	}
}

func TestEffectivePolicyClampsAndNilsOutInvalidRules(t *testing.T) {
	p := Policy{
		Enabled:    true,
		StopLoss:   &FixedPercent{Percent: 10}, // 10 → 0.10
		TakeProfit: &FixedPercent{Percent: -1}, // invalid → nil
		Trailing:   &TrailingPercent{Percent: 2000}, // > 100% → clipped
		TimeStop:   &TimeWindow{MaxHoldingDays: -3}, // invalid → nil
	}
	eff := p.EffectivePolicy()
	if eff.StopLoss == nil || eff.StopLoss.Percent != 0.10 {
		t.Fatalf("stop_loss should auto-convert 10 → 0.10: got %+v", eff.StopLoss)
	}
	if eff.TakeProfit != nil {
		t.Fatalf("take_profit should drop: got %+v", eff.TakeProfit)
	}
	if eff.Trailing == nil || eff.Trailing.Percent != MaxPercent {
		t.Fatalf("trailing should clip to MaxPercent: got %+v", eff.Trailing)
	}
	if eff.TimeStop != nil {
		t.Fatalf("time_stop should drop: got %+v", eff.TimeStop)
	}
}

func TestEffectivePolicyClipsTimeStopToMaxDays(t *testing.T) {
	p := Policy{
		Enabled:  true,
		TimeStop: &TimeWindow{MaxHoldingDays: 9999},
	}
	eff := p.EffectivePolicy()
	if eff.TimeStop == nil || eff.TimeStop.MaxHoldingDays != MaxHoldingDays {
		t.Fatalf("expected clipped to %d, got %+v", MaxHoldingDays, eff.TimeStop)
	}
}

func TestValidatePolicyDetectsBadInputs(t *testing.T) {
	good := Policy{Enabled: true, StopLoss: &FixedPercent{Percent: 0.10}}
	if err := ValidatePolicy(good); err != nil {
		t.Fatalf("expected good policy to validate, got %v", err)
	}
	bad := Policy{Enabled: true, StopLoss: &FixedPercent{Percent: 0}}
	if err := ValidatePolicy(bad); err == nil {
		t.Fatal("expected validation error for zero percent")
	}
}

// ---------------------------------------------------------------------------
// Stop loss
// ---------------------------------------------------------------------------

func TestStopLossFiresWhenAnyLotIsUnderwater(t *testing.T) {
	svc := NewService()
	policy := Policy{
		Enabled:  true,
		StopLoss: &FixedPercent{Percent: 0.10},
	}
	// Lot1 entry 100 → threshold 90. Lot2 entry 120 → threshold 108.
	// At price 95: lot1 is OK, lot2 is underwater (95 < 108) → fire.
	view := PositionView{
		InstrumentKey: "X",
		Symbol:        "X",
		CurrentPrice:  95,
		OpenLots: []*repository.PositionLotRow{
			lotFromEntry("L1", 100, 10, 5),
			lotFromEntry("L2", 120, 10, 3),
		},
	}
	decs := svc.Evaluate(policy, []PositionView{view})
	if len(decs) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(decs))
	}
	if decs[0].Reason != "stop_loss" {
		t.Fatalf("reason: got %q, want stop_loss", decs[0].Reason)
	}
	if decs[0].LotID != "L2" {
		t.Fatalf("trigger lot: got %q, want L2 (most underwater)", decs[0].LotID)
	}
	if decs[0].Quantity != 20 {
		t.Fatalf("quantity: got %v, want 20 (sum of both lots)", decs[0].Quantity)
	}
}

func TestStopLossDoesNotFireWhenAllLotsAboveThreshold(t *testing.T) {
	svc := NewService()
	policy := Policy{Enabled: true, StopLoss: &FixedPercent{Percent: 0.10}}
	view := PositionView{
		InstrumentKey: "X",
		Symbol:        "X",
		CurrentPrice:  95,
		OpenLots: []*repository.PositionLotRow{
			lotFromEntry("L1", 100, 10, 5), // threshold 90 → 95 > 90 OK
		},
	}
	if decs := svc.Evaluate(policy, []PositionView{view}); len(decs) != 0 {
		t.Fatalf("expected no decision, got %d: %+v", len(decs), decs)
	}
}

// ---------------------------------------------------------------------------
// Take profit
// ---------------------------------------------------------------------------

func TestTakeProfitFiresWhenAnyLotIsAboveThreshold(t *testing.T) {
	svc := NewService()
	policy := Policy{Enabled: true, TakeProfit: &FixedPercent{Percent: 0.20}}
	// Lot1 entry 100 → threshold 120. Lot2 entry 150 → threshold 180.
	// At price 130: lot1 → 130 > 120 fires; lot2 → 130 < 180 no.
	view := PositionView{
		InstrumentKey: "X",
		Symbol:        "X",
		CurrentPrice:  130,
		OpenLots: []*repository.PositionLotRow{
			lotFromEntry("L1", 100, 10, 5),
			lotFromEntry("L2", 150, 10, 3),
		},
	}
	decs := svc.Evaluate(policy, []PositionView{view})
	if len(decs) != 1 || decs[0].Reason != "take_profit" {
		t.Fatalf("expected take_profit, got %+v", decs)
	}
	if decs[0].LotID != "L1" {
		t.Fatalf("trigger lot: got %q, want L1 (most profitable)", decs[0].LotID)
	}
}

// ---------------------------------------------------------------------------
// Trailing
// ---------------------------------------------------------------------------

func TestTrailingFiresOnlyWhenLotPeakedAboveEntry(t *testing.T) {
	svc := NewService()
	policy := Policy{Enabled: true, Trailing: &TrailingPercent{Percent: 0.10}}
	// Lot1 entry 100, peak 120 → threshold 108. Price 105 → fires.
	// Lot2 entry 100, peak 95  → never rose, trailing inactive.
	view := PositionView{
		InstrumentKey: "X",
		Symbol:        "X",
		CurrentPrice:  105,
		OpenLots: []*repository.PositionLotRow{
			withHigh(lotFromEntry("L1", 100, 5, 5), 120),
			withHigh(lotFromEntry("L2", 100, 5, 3), 95),
		},
	}
	decs := svc.Evaluate(policy, []PositionView{view})
	if len(decs) != 1 || decs[0].Reason != "trailing" {
		t.Fatalf("expected trailing, got %+v", decs)
	}
	if decs[0].LotID != "L1" {
		t.Fatalf("trigger lot: got %q, want L1", decs[0].LotID)
	}
}

func TestTrailingDoesNotFireWhenHighIsNil(t *testing.T) {
	svc := NewService()
	policy := Policy{Enabled: true, Trailing: &TrailingPercent{Percent: 0.10}}
	view := PositionView{
		InstrumentKey: "X",
		Symbol:        "X",
		CurrentPrice:  80,
		OpenLots: []*repository.PositionLotRow{
			lotFromEntry("L1", 100, 5, 5), // no high tracked
		},
	}
	if decs := svc.Evaluate(policy, []PositionView{view}); len(decs) != 0 {
		t.Fatalf("expected no decision, got %+v", decs)
	}
}

// ---------------------------------------------------------------------------
// Time stop
// ---------------------------------------------------------------------------

func TestTimeStopFiresOnOldestLotPastWindow(t *testing.T) {
	now := time.Date(2026, time.May, 14, 9, 30, 0, 0, time.UTC)
	svc := NewService(staticClock(now))
	policy := Policy{Enabled: true, TimeStop: &TimeWindow{MaxHoldingDays: 20}}
	// L1 opened 30d ago (over limit), L2 opened 5d ago.
	view := PositionView{
		InstrumentKey: "X",
		Symbol:        "X",
		CurrentPrice:  100,
		OpenLots: []*repository.PositionLotRow{
			lotFromEntry("L1", 100, 5, 30),
			lotFromEntry("L2", 100, 5, 5),
		},
	}
	decs := svc.Evaluate(policy, []PositionView{view})
	if len(decs) != 1 || decs[0].Reason != "time_stop" {
		t.Fatalf("expected time_stop, got %+v", decs)
	}
	if decs[0].LotID != "L1" {
		t.Fatalf("trigger lot: got %q, want L1 (oldest)", decs[0].LotID)
	}
}

// ---------------------------------------------------------------------------
// Priority order
// ---------------------------------------------------------------------------

func TestPriorityStopLossBeatsAll(t *testing.T) {
	now := time.Date(2026, time.May, 14, 9, 30, 0, 0, time.UTC)
	svc := NewService(staticClock(now))
	policy := Policy{
		Enabled:    true,
		StopLoss:   &FixedPercent{Percent: 0.05},
		TakeProfit: &FixedPercent{Percent: 0.10},
		Trailing:   &TrailingPercent{Percent: 0.05},
		TimeStop:   &TimeWindow{MaxHoldingDays: 1},
	}
	// Scenario crafted so all four rules fire on the SAME view:
	//   lot entry 100, peak 130, price 80
	//   stop_loss   (5%): threshold 95  → 80 < 95 ✓
	//   take_profit (10%): threshold 110 → also have peak 130 > 110 historically...
	//     wait, take_profit looks at current price, not peak.
	//     current 80 < 110 → take_profit DOES NOT fire on current 80.
	//   So let me redesign: lot 100 entry, peak 130, current 95.
	//   stop_loss (5%): 95 < 95 false; need 94.99 to fire.
	// Restate: we just need stop_loss to fire when ALSO trailing fires.
	//   entry 100, peak 130, current 80.
	//   stop_loss (5%): 80 < 95 ✓
	//   trailing (5%):   80 < 130*0.95 = 123.5 ✓
	//   take_profit (10%): 80 > 110 false.
	//   time_stop (1d): lot opened 5d ago → 5 > 1 ✓
	// Stop_loss has priority → reason should be "stop_loss".
	view := PositionView{
		InstrumentKey: "X",
		Symbol:        "X",
		CurrentPrice:  80,
		OpenLots: []*repository.PositionLotRow{
			withHigh(lotFromEntry("L1", 100, 5, 5), 130),
		},
	}
	decs := svc.Evaluate(policy, []PositionView{view})
	if len(decs) != 1 || decs[0].Reason != "stop_loss" {
		t.Fatalf("priority: expected stop_loss, got %+v", decs)
	}
}

func TestPriorityTrailingBeatsTakeProfitAndTimeStop(t *testing.T) {
	now := time.Date(2026, time.May, 14, 9, 30, 0, 0, time.UTC)
	svc := NewService(staticClock(now))
	policy := Policy{
		Enabled:    true,
		TakeProfit: &FixedPercent{Percent: 0.10},
		Trailing:   &TrailingPercent{Percent: 0.10},
		TimeStop:   &TimeWindow{MaxHoldingDays: 1},
	}
	// entry 100, peak 150, current 130:
	//   take_profit (10%): 130 > 110 ✓
	//   trailing  (10%):   130 < 150*0.90 = 135 ✓
	//   time_stop (1d):    opened 5d ago ✓
	// Trailing has priority over take_profit.
	view := PositionView{
		InstrumentKey: "X",
		Symbol:        "X",
		CurrentPrice:  130,
		OpenLots: []*repository.PositionLotRow{
			withHigh(lotFromEntry("L1", 100, 5, 5), 150),
		},
	}
	decs := svc.Evaluate(policy, []PositionView{view})
	if len(decs) != 1 || decs[0].Reason != "trailing" {
		t.Fatalf("priority: expected trailing, got %+v", decs)
	}
}

func TestPriorityTakeProfitBeatsTimeStop(t *testing.T) {
	now := time.Date(2026, time.May, 14, 9, 30, 0, 0, time.UTC)
	svc := NewService(staticClock(now))
	policy := Policy{
		Enabled:    true,
		TakeProfit: &FixedPercent{Percent: 0.10},
		TimeStop:   &TimeWindow{MaxHoldingDays: 1},
	}
	// entry 100, current 115, opened 5d ago → both fire; take_profit wins.
	view := PositionView{
		InstrumentKey: "X",
		Symbol:        "X",
		CurrentPrice:  115,
		OpenLots: []*repository.PositionLotRow{
			lotFromEntry("L1", 100, 5, 5),
		},
	}
	decs := svc.Evaluate(policy, []PositionView{view})
	if len(decs) != 1 || decs[0].Reason != "take_profit" {
		t.Fatalf("priority: expected take_profit, got %+v", decs)
	}
}

// ---------------------------------------------------------------------------
// Defensive guards
// ---------------------------------------------------------------------------

func TestEvaluateSkipsViewsWithZeroPrice(t *testing.T) {
	svc := NewService()
	policy := Policy{Enabled: true, StopLoss: &FixedPercent{Percent: 0.05}}
	view := PositionView{
		InstrumentKey: "X",
		Symbol:        "X",
		CurrentPrice:  0, // stale / missing
		OpenLots: []*repository.PositionLotRow{
			lotFromEntry("L1", 100, 5, 5),
		},
	}
	if decs := svc.Evaluate(policy, []PositionView{view}); len(decs) != 0 {
		t.Fatalf("expected no decision on zero price, got %+v", decs)
	}
}

func TestEvaluateSkipsViewsWithoutOpenLots(t *testing.T) {
	svc := NewService()
	policy := Policy{Enabled: true, StopLoss: &FixedPercent{Percent: 0.05}}
	view := PositionView{
		InstrumentKey: "X",
		Symbol:        "X",
		CurrentPrice:  50,
		OpenLots:      nil,
	}
	if decs := svc.Evaluate(policy, []PositionView{view}); len(decs) != 0 {
		t.Fatalf("expected no decision without lots, got %+v", decs)
	}
}

func TestEvaluateSkipsDisabledPolicy(t *testing.T) {
	svc := NewService()
	policy := Policy{Enabled: false, StopLoss: &FixedPercent{Percent: 0.05}}
	view := PositionView{
		InstrumentKey: "X",
		Symbol:        "X",
		CurrentPrice:  50,
		OpenLots: []*repository.PositionLotRow{
			lotFromEntry("L1", 100, 5, 5),
		},
	}
	if decs := svc.Evaluate(policy, []PositionView{view}); len(decs) != 0 {
		t.Fatalf("expected no decision when disabled, got %+v", decs)
	}
}

func TestEvaluateSumsQuantityAcrossAllOpenLots(t *testing.T) {
	svc := NewService()
	policy := Policy{Enabled: true, StopLoss: &FixedPercent{Percent: 0.05}}
	// Two open lots, each 10 remaining. Stop fires → close 20.
	view := PositionView{
		InstrumentKey: "X",
		Symbol:        "X",
		CurrentPrice:  80,
		OpenLots: []*repository.PositionLotRow{
			lotFromEntry("L1", 100, 10, 5),
			lotFromEntry("L2", 90, 10, 3),
		},
	}
	decs := svc.Evaluate(policy, []PositionView{view})
	if len(decs) != 1 || decs[0].Quantity != 20 {
		t.Fatalf("expected qty=20 total, got %+v", decs)
	}
}

func TestEvaluateOrdersResultsByInstrumentKey(t *testing.T) {
	svc := NewService()
	policy := Policy{Enabled: true, StopLoss: &FixedPercent{Percent: 0.05}}
	views := []PositionView{
		{
			InstrumentKey: "ZZZ",
			Symbol:        "ZZZ",
			CurrentPrice:  80,
			OpenLots:      []*repository.PositionLotRow{lotFromEntry("L1", 100, 5, 5)},
		},
		{
			InstrumentKey: "AAA",
			Symbol:        "AAA",
			CurrentPrice:  80,
			OpenLots:      []*repository.PositionLotRow{lotFromEntry("L2", 100, 5, 5)},
		},
	}
	decs := svc.Evaluate(policy, views)
	if len(decs) != 2 {
		t.Fatalf("expected 2 decisions, got %d", len(decs))
	}
	if decs[0].InstrumentKey != "AAA" || decs[1].InstrumentKey != "ZZZ" {
		t.Fatalf("expected alphabetical order, got %+v", decs)
	}
}
