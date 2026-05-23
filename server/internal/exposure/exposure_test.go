package exposure

// Sprint C #1 contract tests for the exposure.Compute function.
//
// The tests pin:
//   - Options.withDefaults installs the production caps (25%
//     single-name, 50% sector, 60% top-3, 5% cash floor) and
//     clamps degenerate inputs.
//   - Compute is fail-soft (totalAssets <= 0 → empty snapshot).
//   - Positions with non-positive MV are dropped before bucketing.
//   - Sector roll-up uses lower-case keys; blank sector → "unclassified".
//   - Breaches surface deterministic strings the prompt can pin.
//   - Top-3 only kicks in at 3+ positions.
//   - Cash floor breach fires when cash% below the configured floor.
//   - HasSignal correctly distinguishes "render this block" vs "omit".

import (
	"strings"
	"testing"
)

func TestOptionsWithDefaultsProductionCaps(t *testing.T) {
	got := Options{}.withDefaults()
	want := Options{
		SingleNameCap: 0.25,
		SectorCap:     0.50,
		Top3Cap:       0.60,
		CashFloorPct:  0,
	}
	if got != want {
		t.Errorf("Options{}.withDefaults() = %+v, want %+v", got, want)
	}
}

// All four caps clamp at both floors and ceilings.
func TestOptionsWithDefaultsClampsBounds(t *testing.T) {
	got := Options{
		SingleNameCap: 5.0,
		SectorCap:     5.0,
		Top3Cap:       5.0,
		CashFloorPct:  5.0,
	}.withDefaults()
	if got.SingleNameCap != 1.0 {
		t.Errorf("SingleNameCap ceiling: got %v, want 1.0", got.SingleNameCap)
	}
	if got.SectorCap != 1.0 {
		t.Errorf("SectorCap ceiling: got %v, want 1.0", got.SectorCap)
	}
	if got.Top3Cap != 1.0 {
		t.Errorf("Top3Cap ceiling: got %v, want 1.0", got.Top3Cap)
	}
	if got.CashFloorPct != 0.50 {
		t.Errorf("CashFloorPct ceiling: got %v, want 0.5", got.CashFloorPct)
	}

	got = Options{
		SingleNameCap: 0.001,
		SectorCap:     0.001,
		Top3Cap:       0.001,
		CashFloorPct:  -1.0,
	}.withDefaults()
	if got.SingleNameCap != 0.05 {
		t.Errorf("SingleNameCap floor: got %v, want 0.05", got.SingleNameCap)
	}
	if got.SectorCap != 0.10 {
		t.Errorf("SectorCap floor: got %v, want 0.10", got.SectorCap)
	}
	if got.Top3Cap != 0.20 {
		t.Errorf("Top3Cap floor: got %v, want 0.20", got.Top3Cap)
	}
	if got.CashFloorPct != 0 {
		t.Errorf("CashFloorPct floor: got %v, want 0", got.CashFloorPct)
	}
}

// totalAssets <= 0 → empty snapshot (the prompt builder omits it).
func TestComputeZeroNAVReturnsEmpty(t *testing.T) {
	got := Compute(Options{}, 0, 100, []Position{{Symbol: "A", MarketValue: 50}})
	if got.HasSignal() {
		t.Errorf("zero NAV must produce no-signal snapshot, got %+v", got)
	}
}

// Happy path: 3 positions, 1 sector, no breaches.
func TestComputeBasicConcentration(t *testing.T) {
	got := Compute(
		Options{SingleNameCap: 0.40, SectorCap: 0.80, Top3Cap: 0.90, CashFloorPct: 0.05},
		1000,
		200,
		[]Position{
			{Symbol: "AAPL", Sector: "tech", MarketValue: 300},
			{Symbol: "MSFT", Sector: "tech", MarketValue: 250},
			{Symbol: "NVDA", Sector: "tech", MarketValue: 250},
		},
	)
	if got.TotalAssets != 1000 {
		t.Errorf("TotalAssets = %v, want 1000", got.TotalAssets)
	}
	if got.AvailableCash != 200 {
		t.Errorf("AvailableCash = %v, want 200", got.AvailableCash)
	}
	if got.PositionCount != 3 {
		t.Errorf("PositionCount = %d, want 3", got.PositionCount)
	}
	if got.CashPct != 0.2 {
		t.Errorf("CashPct = %v, want 0.2", got.CashPct)
	}
	if len(got.SingleName) != 3 {
		t.Fatalf("SingleName: want 3 rows, got %d (%+v)", len(got.SingleName), got.SingleName)
	}
	if got.SingleName[0].Symbol != "AAPL" {
		t.Errorf("SingleName[0] = %q, want AAPL (heaviest first)", got.SingleName[0].Symbol)
	}
	if got.SingleName[0].Weight != 0.30 {
		t.Errorf("SingleName[0].Weight = %v, want 0.30", got.SingleName[0].Weight)
	}
	if len(got.SectorWeights) != 1 || got.SectorWeights[0].Sector != "tech" {
		t.Errorf("SectorWeights = %+v, want single tech row", got.SectorWeights)
	}
	if got.SectorWeights[0].Weight != 0.80 {
		t.Errorf("tech sector Weight = %v, want 0.80", got.SectorWeights[0].Weight)
	}
	if got.Top3Weight != 0.80 {
		t.Errorf("Top3Weight = %v, want 0.80", got.Top3Weight)
	}
	if len(got.Breaches) != 0 {
		t.Errorf("expected no breaches, got %+v", got.Breaches)
	}
}

// Single-name breach: AAPL = 40% > 25% cap.
func TestComputeFlagsSingleNameBreach(t *testing.T) {
	got := Compute(
		Options{}, // default 25% single-name cap
		1000,
		200,
		[]Position{
			{Symbol: "AAPL", Sector: "tech", MarketValue: 400},
			{Symbol: "MSFT", Sector: "tech", MarketValue: 200},
		},
	)
	if !got.SingleName[0].Breach {
		t.Errorf("AAPL 40%% > 25%% must breach: %+v", got.SingleName[0])
	}
	if len(got.Breaches) == 0 {
		t.Fatal("expected at least one breach string")
	}
	matched := false
	for _, b := range got.Breaches {
		if strings.Contains(b, "single-name=AAPL") {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("expected AAPL breach line, got %+v", got.Breaches)
	}
}

// Sector breach: total tech = 60% > 50% cap.
func TestComputeFlagsSectorBreach(t *testing.T) {
	got := Compute(
		Options{}, // default 50% sector cap
		1000,
		400,
		[]Position{
			{Symbol: "AAPL", Sector: "tech", MarketValue: 200},
			{Symbol: "MSFT", Sector: "tech", MarketValue: 200},
			{Symbol: "NVDA", Sector: "tech", MarketValue: 200},
		},
	)
	if got.SectorWeights[0].Sector != "tech" || !got.SectorWeights[0].Breach {
		t.Errorf("tech 60%% > 50%% must breach: %+v", got.SectorWeights)
	}
	if !containsSubstring(got.Breaches, "sector=tech") {
		t.Errorf("expected sector=tech breach, got %+v", got.Breaches)
	}
}

// Top-3 breach: 3 positions of 25% each = 75% > 60% top-3 cap.
func TestComputeFlagsTop3Breach(t *testing.T) {
	got := Compute(
		Options{}, // default 60% top-3 cap
		1000,
		250,
		[]Position{
			{Symbol: "AAPL", Sector: "tech", MarketValue: 250},
			{Symbol: "MSFT", Sector: "finance", MarketValue: 250},
			{Symbol: "JPM", Sector: "finance", MarketValue: 250},
			{Symbol: "OTHER", Sector: "energy", MarketValue: 0}, // dropped
		},
	)
	if got.Top3Weight != 0.75 {
		t.Errorf("Top3Weight = %v, want 0.75", got.Top3Weight)
	}
	if !containsSubstring(got.Breaches, "top-3=cluster") {
		t.Errorf("expected top-3 breach, got %+v", got.Breaches)
	}
}

// Cash floor breach: 2% cash < 5% floor.
func TestComputeFlagsCashFloorBreach(t *testing.T) {
	got := Compute(
		Options{CashFloorPct: 0.05},
		1000,
		20,
		[]Position{
			{Symbol: "AAPL", Sector: "tech", MarketValue: 500},
		},
	)
	if !containsSubstring(got.Breaches, "cash=") {
		t.Errorf("expected cash breach, got %+v", got.Breaches)
	}
}

// Cash floor of 0 means "don't enforce" — no breach even at 0%
// cash.
func TestComputeNoCashFloorWhenFloorZero(t *testing.T) {
	got := Compute(
		Options{}, // default CashFloorPct = 0 → no enforcement
		1000,
		0,
		[]Position{
			{Symbol: "AAPL", Sector: "tech", MarketValue: 500},
		},
	)
	for _, b := range got.Breaches {
		if strings.HasPrefix(b, "BREACH: cash=") {
			t.Errorf("default CashFloorPct=0 should not breach, got %q", b)
		}
	}
}

// Top-3 only fires with ≥ 3 positions.
func TestComputeTop3RequiresThreePositions(t *testing.T) {
	got := Compute(Options{}, 1000, 0, []Position{
		{Symbol: "AAPL", Sector: "tech", MarketValue: 800},
		{Symbol: "MSFT", Sector: "tech", MarketValue: 100},
	})
	if got.Top3Weight != 0 {
		t.Errorf("Top3Weight on 2-position book = %v, want 0", got.Top3Weight)
	}
}

// Blank-sector positions land in "unclassified".
func TestComputeUnclassifiedSector(t *testing.T) {
	got := Compute(Options{}, 1000, 0, []Position{
		{Symbol: "XYZ", Sector: "", MarketValue: 500},
		{Symbol: "ABC", Sector: "   ", MarketValue: 500},
	})
	if len(got.SectorWeights) != 1 || got.SectorWeights[0].Sector != "unclassified" {
		t.Errorf("blank sectors should collapse: %+v", got.SectorWeights)
	}
}

// Duplicate symbols accumulate their MV (a single instrument
// represented twice in the slice shouldn't bypass concentration
// math).
func TestComputeDeduplicatesSymbols(t *testing.T) {
	got := Compute(Options{}, 1000, 0, []Position{
		{Symbol: "AAPL", Sector: "tech", MarketValue: 200},
		{Symbol: "aapl", Sector: "tech", MarketValue: 200},
	})
	if got.PositionCount != 1 {
		t.Errorf("PositionCount = %d, want 1 (dedup)", got.PositionCount)
	}
	if got.SingleName[0].Weight != 0.40 {
		t.Errorf("dedup weight = %v, want 0.40", got.SingleName[0].Weight)
	}
}

// HasSignal: empty position list + cash > 0 is still worth rendering
// (it conveys "you're 100% cash" to the PM), but a zero-NAV input
// returns false.
func TestHasSignalSemantics(t *testing.T) {
	if (Snapshot{}.HasSignal()) {
		t.Error("empty Snapshot should not signal")
	}
	if !(Snapshot{TotalAssets: 1, CashPct: 1.0}).HasSignal() {
		t.Error("100% cash Snapshot should signal")
	}
	if !(Snapshot{TotalAssets: 1, PositionCount: 1}).HasSignal() {
		t.Error("1-position Snapshot should signal")
	}
}

// Breach output is sorted alphabetically so the prompt is
// deterministic across runs.
func TestComputeBreachesAreSorted(t *testing.T) {
	got := Compute(
		Options{SingleNameCap: 0.20, SectorCap: 0.30, Top3Cap: 0.40, CashFloorPct: 0.10},
		1000,
		20,
		[]Position{
			{Symbol: "AAPL", Sector: "tech", MarketValue: 400},
			{Symbol: "JPM", Sector: "finance", MarketValue: 350},
			{Symbol: "XOM", Sector: "energy", MarketValue: 230},
		},
	)
	if len(got.Breaches) < 2 {
		t.Fatalf("expected multiple breaches, got %+v", got.Breaches)
	}
	for i := 1; i < len(got.Breaches); i++ {
		if got.Breaches[i-1] > got.Breaches[i] {
			t.Errorf("breaches not sorted at index %d: %q then %q", i, got.Breaches[i-1], got.Breaches[i])
		}
	}
}

// BreachKinds classifies each breach string into the
// Prometheus-safe enum used by Sprint D #1's
// fundai_decision_exposure_breaches_total counter.
func TestBreachKindsCoversEveryClass(t *testing.T) {
	// One of each: single-name, sector, top-3, cash floor.
	got := Compute(
		Options{SingleNameCap: 0.20, SectorCap: 0.30, Top3Cap: 0.40, CashFloorPct: 0.10},
		1000,
		20,
		[]Position{
			{Symbol: "AAPL", Sector: "tech", MarketValue: 400},
			{Symbol: "JPM", Sector: "finance", MarketValue: 350},
			{Symbol: "XOM", Sector: "energy", MarketValue: 230},
		},
	)
	kinds := got.BreachKinds()
	if len(kinds) != len(got.Breaches) {
		t.Fatalf("BreachKinds len=%d want %d (one per breach)", len(kinds), len(got.Breaches))
	}
	// Each kind in the result should be one of the canonical enum values.
	allowed := map[string]bool{
		BreachKindSingleName: true,
		BreachKindSector:     true,
		BreachKindTop3:       true,
		BreachKindCashFloor:  true,
		"other":              true,
	}
	for i, k := range kinds {
		if !allowed[k] {
			t.Errorf("BreachKinds[%d]=%q not in canonical enum %+v", i, k, got.Breaches[i])
		}
	}
	// Every kind classification must round-trip through the
	// canonical enum (no surprise "other" for known prefixes).
	for i, k := range kinds {
		if k == "other" {
			t.Errorf("breach %q classified as 'other' — classifier missing a case", got.Breaches[i])
		}
	}
}

// Empty snapshot returns nil kinds (no allocations) so the
// metrics caller can append directly without nil checks.
func TestBreachKindsEmptyIsNil(t *testing.T) {
	snap := Compute(Options{}, 1000, 1000, nil)
	if kinds := snap.BreachKinds(); kinds != nil {
		t.Errorf("empty snapshot BreachKinds should be nil, got %+v", kinds)
	}
}

// classifyBreach is the lookup helper exposed for callers that
// already have a breach string. Verify the discriminator matching
// is strict so a malformed entry doesn't accidentally map onto
// the wrong kind. The cases below mirror formatBreach /
// formatCashBreach output formats verbatim.
func TestClassifyBreachStrictPrefix(t *testing.T) {
	cases := map[string]string{
		"BREACH: single-name=AAPL weight=32.0% > cap=25.0%": BreachKindSingleName,
		"BREACH: sector=tech weight=55.0% > cap=50.0%":      BreachKindSector,
		"BREACH: top-3=cluster weight=70.0% > cap=60.0%":    BreachKindTop3,
		"BREACH: cash=2.5% < floor=5.0% (consider releasing a position before any new buy)": BreachKindCashFloor,
		"BREACH: unknown=XYZ weight=1% > cap=0%":                                            "other",
		"single-name AAPL 32.0% > 25.0%":                                                    "other",
		"":                                                                                  "other",
	}
	for input, want := range cases {
		if got := classifyBreach(input); got != want {
			t.Errorf("classifyBreach(%q) = %q, want %q", input, got, want)
		}
	}
}

// round4 scrubs NaN / Inf / negatives so the prompt fields never
// carry junk.
func TestRound4(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{0.123456, 0.1235},
		{1.999999, 2.0},
		{-1.5, 0},
	}
	for _, c := range cases {
		got := round4(c.in)
		if got != c.want {
			t.Errorf("round4(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func containsSubstring(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
