package futures

import (
	"math"
	"testing"
	"time"
)

func mkContract(sym, root string, exp time.Time, mult float64) Contract {
	return Contract{
		Symbol:        sym,
		Root:          root,
		ExpiresOn:     exp,
		Multiplier:    mult,
		Currency:      "USD",
		MarginInitial: 1000,
		TickSize:      0.01,
		TickValue:     mult * 0.01,
	}
}

func TestContractValidate(t *testing.T) {
	good := mkContract("CL2606", "CL", time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC), 1000)
	if err := good.Validate(); err != nil {
		t.Fatalf("good contract: %v", err)
	}
	bad := good
	bad.Symbol = ""
	if err := bad.Validate(); err == nil {
		t.Fatal("expected blank symbol to be rejected")
	}
	bad = good
	bad.Multiplier = 0
	if err := bad.Validate(); err == nil {
		t.Fatal("expected zero multiplier to be rejected")
	}
}

func TestBuildRollCalendarSortsAndChains(t *testing.T) {
	cs := []Contract{
		mkContract("CL2606", "CL", time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC), 1000),
		mkContract("CL2608", "CL", time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC), 1000),
		mkContract("CL2607", "CL", time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), 1000),
	}
	rolls, err := BuildRollCalendar(cs, RollPolicy{DaysBeforeExpiry: 5})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(rolls) != 2 {
		t.Fatalf("expected 2 roll events, got %d", len(rolls))
	}
	if rolls[0].FromSymbol != "CL2606" || rolls[0].ToSymbol != "CL2607" {
		t.Fatalf("roll 0: %+v", rolls[0])
	}
	if rolls[1].FromSymbol != "CL2607" || rolls[1].ToSymbol != "CL2608" {
		t.Fatalf("roll 1: %+v", rolls[1])
	}
	if rolls[0].RolledOn.After(rolls[1].RolledOn) {
		t.Fatalf("roll dates not sorted: %v before %v", rolls[1].RolledOn, rolls[0].RolledOn)
	}
}

func TestBuildRollCalendarRejectsMixedRoots(t *testing.T) {
	cs := []Contract{
		mkContract("CL2606", "CL", time.Now().AddDate(0, 1, 0), 1000),
		mkContract("GC2606", "GC", time.Now().AddDate(0, 2, 0), 100),
	}
	if _, err := BuildRollCalendar(cs, RollPolicy{}); err == nil {
		t.Fatal("expected mixed roots to be rejected")
	}
}

func TestBuildRollCalendarSingleContractIsEmpty(t *testing.T) {
	cs := []Contract{mkContract("CL2606", "CL", time.Now().AddDate(0, 1, 0), 1000)}
	rolls, err := BuildRollCalendar(cs, RollPolicy{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(rolls) != 0 {
		t.Fatalf("single contract should produce no rolls, got %d", len(rolls))
	}
}

func TestActiveContractPicksUpcoming(t *testing.T) {
	exp1 := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	exp2 := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	exp3 := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	cs := []Contract{
		mkContract("CL2606", "CL", exp1, 1000),
		mkContract("CL2607", "CL", exp2, 1000),
		mkContract("CL2608", "CL", exp3, 1000),
	}
	policy := RollPolicy{DaysBeforeExpiry: 5}

	// Well before the first roll: front month active.
	wellBefore := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if got, ok := ActiveContract(cs, policy, wellBefore); !ok || got.Symbol != "CL2606" {
		t.Fatalf("before first roll: expected CL2606, got %q ok=%v", got.Symbol, ok)
	}
	// Inside the roll-out window of CL2606 (15 June 2026, exp - 5d): should jump to next.
	rollWindow := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	if got, ok := ActiveContract(cs, policy, rollWindow); !ok || got.Symbol != "CL2607" {
		t.Fatalf("during first roll window: expected CL2607, got %q", got.Symbol)
	}
}

type stubFetcher struct{ prices map[string]map[string]float64 }

func (s stubFetcher) Close(symbol string, day time.Time) (float64, bool) {
	key := day.Format("2006-01-02")
	if m, ok := s.prices[symbol]; ok {
		if p, ok := m[key]; ok {
			return p, true
		}
	}
	return 0, false
}

func TestBuildContinuousSeriesStitchesAcrossRoll(t *testing.T) {
	exp1 := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	exp2 := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	cs := []Contract{
		mkContract("CL2606", "CL", exp1, 1000),
		mkContract("CL2607", "CL", exp2, 1000),
	}
	prices := map[string]map[string]float64{
		"CL2606": {
			"2026-06-13": 70.0,
			"2026-06-14": 71.0,
		},
		"CL2607": {
			"2026-06-15": 72.5,
			"2026-06-16": 72.8,
		},
	}
	from := time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	pts := BuildContinuousSeries(cs, RollPolicy{DaysBeforeExpiry: 5}, stubFetcher{prices}, from, to)
	if len(pts) != 4 {
		t.Fatalf("expected 4 points, got %d", len(pts))
	}
	if pts[0].Symbol != "CL2606" {
		t.Fatalf("first day should be CL2606, got %s", pts[0].Symbol)
	}
	if pts[2].Symbol != "CL2607" {
		t.Fatalf("post-roll day should be CL2607, got %s", pts[2].Symbol)
	}
}

func TestMarkToMarketComputesPnL(t *testing.T) {
	c := mkContract("CL2606", "CL", time.Now().AddDate(0, 1, 0), 1000)
	contracts := map[string]Contract{"CL2606": c}
	marks := map[string]float64{"CL2606": 72.0}
	positions := []MarkPosition{
		{Symbol: "CL2606", Qty: 2, EntryMark: 70.0},
		{Symbol: "CL2606", Qty: -1, EntryMark: 71.0},
	}
	snaps := MarkToMarket(positions, contracts, marks)
	if len(snaps) != 2 {
		t.Fatalf("expected 2 snaps, got %d", len(snaps))
	}
	wantLong := 2 * 1000 * (72.0 - 70.0)
	if math.Abs(snaps[0].UnrealizedPnL-wantLong) > 1e-6 {
		t.Fatalf("long PnL mismatch: want %v got %v", wantLong, snaps[0].UnrealizedPnL)
	}
	wantShort := -1 * 1000 * (72.0 - 71.0)
	if math.Abs(snaps[1].UnrealizedPnL-wantShort) > 1e-6 {
		t.Fatalf("short PnL mismatch: want %v got %v", wantShort, snaps[1].UnrealizedPnL)
	}
}

func TestMarkToMarketSilentOnMissingData(t *testing.T) {
	positions := []MarkPosition{{Symbol: "MISSING", Qty: 1, EntryMark: 100}}
	snaps := MarkToMarket(positions, nil, nil)
	if len(snaps) != 1 {
		t.Fatalf("expected pass-through snap, got %d", len(snaps))
	}
	if snaps[0].UnrealizedPnL != 0 || snaps[0].CurrentMark != 0 {
		t.Fatalf("expected zeros for missing data: %+v", snaps[0])
	}
}

func TestComputeVariationMarginLongAndShort(t *testing.T) {
	c := mkContract("CL2606", "CL", time.Now().AddDate(0, 1, 0), 1000)
	contracts := map[string]Contract{"CL2606": c}
	y := map[string]float64{"CL2606": 70.0}
	td := map[string]float64{"CL2606": 71.0}
	positions := []MarkPosition{
		{Symbol: "CL2606", Qty: 2, EntryMark: 70.0},
		{Symbol: "CL2606", Qty: -3, EntryMark: 70.0},
	}
	vm := ComputeVariationMargin(positions, contracts, y, td)
	if len(vm) != 2 {
		t.Fatalf("expected 2 VM rows, got %d", len(vm))
	}
	if math.Abs(vm[0].CashDelta-(2*1000*(71-70))) > 1e-6 {
		t.Fatalf("long VM mismatch: %+v", vm[0])
	}
	if math.Abs(vm[1].CashDelta-(-3*1000*(71-70))) > 1e-6 {
		t.Fatalf("short VM mismatch: %+v", vm[1])
	}
}

func TestComputeVariationMarginZeroWhenMissing(t *testing.T) {
	c := mkContract("CL2606", "CL", time.Now().AddDate(0, 1, 0), 1000)
	contracts := map[string]Contract{"CL2606": c}
	y := map[string]float64{"CL2606": 70.0}
	td := map[string]float64{} // missing today mark
	positions := []MarkPosition{{Symbol: "CL2606", Qty: 1, EntryMark: 70.0}}
	vm := ComputeVariationMargin(positions, contracts, y, td)
	if len(vm) != 1 || vm[0].CashDelta != 0 {
		t.Fatalf("expected zero VM, got %+v", vm)
	}
}

func TestAccrueFundingLongPaysShortReceives(t *testing.T) {
	c := mkContract("BTCPERP", "BTC", time.Now().AddDate(0, 1, 0), 1)
	contracts := map[string]Contract{"BTCPERP": c}
	marks := map[string]float64{"BTCPERP": 65000.0}
	rates := map[string]float64{"BTCPERP": 0.0001} // 1bp/day
	positions := []MarkPosition{
		{Symbol: "BTCPERP", Qty: 2, EntryMark: 64000},
		{Symbol: "BTCPERP", Qty: -1, EntryMark: 64000},
	}
	f := AccrueFunding(positions, contracts, marks, rates)
	if len(f) != 2 {
		t.Fatalf("want 2 funding rows, got %d", len(f))
	}
	// Long pays: cash delta negative.
	if f[0].CashDelta >= 0 {
		t.Fatalf("long funding should be negative: %+v", f[0])
	}
	// Short receives: positive.
	if f[1].CashDelta <= 0 {
		t.Fatalf("short funding should be positive: %+v", f[1])
	}
}

func TestAccrueFundingSkipsZeroRates(t *testing.T) {
	c := mkContract("FOO", "F", time.Now().AddDate(0, 1, 0), 100)
	contracts := map[string]Contract{"FOO": c}
	marks := map[string]float64{"FOO": 10}
	rates := map[string]float64{} // empty
	positions := []MarkPosition{{Symbol: "FOO", Qty: 1, EntryMark: 10}}
	f := AccrueFunding(positions, contracts, marks, rates)
	if len(f) != 0 {
		t.Fatalf("expected no funding rows when rates empty, got %d", len(f))
	}
}
