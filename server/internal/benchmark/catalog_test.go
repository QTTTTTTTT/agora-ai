package benchmark

import (
	"math"
	"testing"
	"time"
)

func TestByID_FoundAndCanonicalLowercase(t *testing.T) {
	cases := []struct{ in, wantID string }{
		{"spx", "spx"},
		{"SPX", "spx"},
		{" CSI300 ", "csi300"},
	}
	for _, tc := range cases {
		got, ok := ByID(tc.in)
		if !ok {
			t.Errorf("ByID(%q) not found", tc.in)
			continue
		}
		if got.ID != tc.wantID {
			t.Errorf("ByID(%q).ID = %q, want %q", tc.in, got.ID, tc.wantID)
		}
	}
}

func TestByID_UnknownReturnsFalse(t *testing.T) {
	if got, ok := ByID("definitely-not-a-real-benchmark"); ok {
		t.Errorf("expected !ok; got %+v", got)
	}
}

// TestRecommend_DeterministicAcrossCalls pins the no-randomness
// invariant: two consecutive calls with identical input MUST return
// identical slices. Detects accidental map iteration in the recommender.
func TestRecommend_DeterministicAcrossCalls(t *testing.T) {
	p := FundProfile{Market: "us_equity", Symbols: []string{"NVDA", "AVGO", "AMD"}}
	first := Recommend(p)
	for i := 0; i < 20; i++ {
		got := Recommend(p)
		if !equalSlice(first, got) {
			t.Errorf("Recommend not deterministic: call %d returned %v vs first %v", i, got, first)
		}
	}
}

func TestRecommend_USEquity_TechHeavyTriggersNDX(t *testing.T) {
	got := Recommend(FundProfile{Market: "us_equity", Symbols: []string{"NVDA"}})
	if !contains(got, "spx") {
		t.Errorf("us_equity should always have spx; got %v", got)
	}
	if !contains(got, "ndx") {
		t.Errorf("NVDA holding should pull in ndx; got %v", got)
	}
	if !contains(got, "soxx") {
		t.Errorf("NVDA holding is also semi → soxx; got %v", got)
	}
}

func TestRecommend_USEquity_NoTechHeavySkipsNDX(t *testing.T) {
	got := Recommend(FundProfile{Market: "us_equity", Symbols: []string{"JNJ", "PG", "KO"}})
	if !contains(got, "spx") {
		t.Errorf("expected spx; got %v", got)
	}
	if contains(got, "ndx") {
		t.Errorf("non-tech holdings shouldn't pull ndx; got %v", got)
	}
}

func TestRecommend_AShare_StarMarketTriggersStar50(t *testing.T) {
	got := Recommend(FundProfile{Market: "a_share", Symbols: []string{"688195", "300750"}})
	if got[0] != "csi300" {
		t.Errorf("primary should be csi300; got %v", got)
	}
	if !contains(got, "star50") {
		t.Errorf("688195 holding should pull star50; got %v", got)
	}
	if !contains(got, "csi500") {
		t.Errorf("a_share fallback csi500 missing; got %v", got)
	}
}

func TestRecommend_AShare_SymbolStrippingHandlesPrefix(t *testing.T) {
	// Recommender must strip "SSE:" / ".SS" prefixes before the
	// 688-prefix match. If it doesn't, a fund whose symbols arrive
	// as "SSE:688195" would skip star50.
	got := Recommend(FundProfile{Market: "a_share", Symbols: []string{"SSE:688195"}})
	if !contains(got, "star50") {
		t.Errorf("prefix-stripped symbol should still trigger star50; got %v", got)
	}
}

func TestRecommend_ListsCappedAtFour(t *testing.T) {
	// Hand-craft a fund that pulls every us_equity rule and
	// confirm the recommender doesn't dump the entire catalog.
	got := Recommend(FundProfile{
		Market:  "us_equity",
		Symbols: []string{"NVDA", "AAPL", "MSFT", "AVGO", "AMD", "TSM"},
	})
	if len(got) > 4 {
		t.Errorf("len(got) = %d, want <= 4; got %v", len(got), got)
	}
}

func TestRecommend_UnknownMarketStillRenders(t *testing.T) {
	got := Recommend(FundProfile{Market: "exotic-derivatives"})
	if len(got) == 0 {
		t.Fatal("unknown market must still return at least one fallback")
	}
}

func TestNormalize_FirstPointIsExactly100(t *testing.T) {
	dates := []time.Time{
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
	}
	values := []float64{4500.0, 4545.0, 4477.5}
	pts, err := Normalize(dates, values)
	if err != nil {
		t.Fatalf("Normalize err = %v", err)
	}
	if pts[0].Value != 100.0 {
		t.Errorf("first = %v, want 100.0", pts[0].Value)
	}
	// 4545 / 4500 * 100 = 101.0
	if math.Abs(pts[1].Value-101.0) > 1e-9 {
		t.Errorf("pts[1] = %v, want 101.0", pts[1].Value)
	}
	// 4477.5 / 4500 * 100 = 99.5
	if math.Abs(pts[2].Value-99.5) > 1e-9 {
		t.Errorf("pts[2] = %v, want 99.5", pts[2].Value)
	}
}

func TestNormalize_SortsUnsortedInput(t *testing.T) {
	dates := []time.Time{
		time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}
	values := []float64{4477.5, 4500.0, 4545.0}
	pts, err := Normalize(dates, values)
	if err != nil {
		t.Fatalf("Normalize err = %v", err)
	}
	for i := 1; i < len(pts); i++ {
		if pts[i].Date.Before(pts[i-1].Date) {
			t.Errorf("sort broken at i=%d: %v before %v", i, pts[i].Date, pts[i-1].Date)
		}
	}
	// First point (after sort) should anchor at 100, regardless
	// of what order the caller provided.
	if pts[0].Value != 100.0 {
		t.Errorf("first after sort = %v, want 100.0", pts[0].Value)
	}
}

func TestNormalize_RejectsNegativeFirst(t *testing.T) {
	dates := []time.Time{
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if _, err := Normalize(dates, []float64{-1.0}); err == nil {
		t.Error("expected ErrEmptySeries for non-positive first")
	}
	if _, err := Normalize(dates, []float64{0.0}); err == nil {
		t.Error("expected ErrEmptySeries for zero first")
	}
}

func TestNormalize_TruncatesDateToMidnightUTC(t *testing.T) {
	cstNoon, _ := time.Parse(time.RFC3339, "2026-05-29T12:00:00+08:00")
	pts, err := Normalize([]time.Time{cstNoon}, []float64{100.0})
	if err != nil {
		t.Fatalf("Normalize err = %v", err)
	}
	want := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
	if !pts[0].Date.Equal(want) {
		t.Errorf("date = %v, want %v", pts[0].Date, want)
	}
}

func TestAlphaSpread_OnlySharedDates(t *testing.T) {
	d := func(day int) time.Time {
		return time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC)
	}
	fund := []Point{
		{Date: d(1), Value: 100.0},
		{Date: d(2), Value: 102.0},
		{Date: d(3), Value: 104.0}, // benchmark missing this date
		{Date: d(4), Value: 105.0},
	}
	bench := []Point{
		{Date: d(1), Value: 100.0},
		{Date: d(2), Value: 101.0},
		{Date: d(4), Value: 103.0},
	}
	got, err := AlphaSpread(fund, bench)
	if err != nil {
		t.Fatalf("AlphaSpread err = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3 (only shared days)", len(got))
	}
	// d1: 100-100=0; d2: 102-101=1; d4: 105-103=2
	want := []float64{0.0, 1.0, 2.0}
	for i, w := range want {
		if math.Abs(got[i].Value-w) > 1e-9 {
			t.Errorf("got[%d].Value = %v, want %v", i, got[i].Value, w)
		}
	}
	// d3 must NOT appear in output.
	for _, p := range got {
		if p.Date.Equal(d(3)) {
			t.Errorf("d3 should be dropped (no benchmark point), got %+v", p)
		}
	}
}

func TestAlphaSpread_RejectsEmpty(t *testing.T) {
	if _, err := AlphaSpread(nil, []Point{{Date: time.Now()}}); err == nil {
		t.Error("expected error on empty fund")
	}
	if _, err := AlphaSpread([]Point{{Date: time.Now()}}, nil); err == nil {
		t.Error("expected error on empty bench")
	}
}

func TestAllIDs_StableOrder(t *testing.T) {
	a := AllIDs()
	b := AllIDs()
	if !equalSlice(a, b) {
		t.Errorf("AllIDs returned different order across calls: %v vs %v", a, b)
	}
	if len(a) != len(Catalog) {
		t.Errorf("AllIDs len = %d, want %d", len(a), len(Catalog))
	}
}

// Test helpers — kept private to the test file.

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(slice []string, v string) bool {
	for _, s := range slice {
		if s == v {
			return true
		}
	}
	return false
}
