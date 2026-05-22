package backtest

import (
	"errors"
	"testing"
)

// Happy path: 2 axes, 2x3 grid → 6 cells, row-major.
func TestExpandSweepCartesian2Axes(t *testing.T) {
	spec := SweepSpec{
		Base: Request{FundID: "fund-1", InitialCash: 100_000, EngineKind: "fallback"},
		Axes: []SweepAxis{
			{Name: "slippageBps", Values: []string{"3", "5"}},
			{Name: "maxOrdersPerDay", Values: []string{"1", "3", "5"}},
		},
	}
	cells, err := ExpandSweep(spec)
	if err != nil {
		t.Fatalf("ExpandSweep: %v", err)
	}
	if len(cells) != 6 {
		t.Fatalf("want 6 cells, got %d", len(cells))
	}
	// Row-major: axis[0]=3 with axis[1]=1,3,5 ; then axis[0]=5 with 1,3,5.
	want := [][2]string{{"3", "1"}, {"3", "3"}, {"3", "5"}, {"5", "1"}, {"5", "3"}, {"5", "5"}}
	for i, w := range want {
		if cells[i].AxisValues["slippageBps"] != w[0] || cells[i].AxisValues["maxOrdersPerDay"] != w[1] {
			t.Errorf("cell %d: got %+v, want slip=%s maxOrd=%s", i, cells[i].AxisValues, w[0], w[1])
		}
	}
	// Spot-check that Request fields were actually overridden.
	if cells[0].Request.SlippageBps != 3 || cells[0].Request.MaxOrdersPerDay != 1 {
		t.Errorf("cell 0 not overridden: %+v", cells[0].Request)
	}
	if cells[5].Request.SlippageBps != 5 || cells[5].Request.MaxOrdersPerDay != 5 {
		t.Errorf("cell 5 not overridden: %+v", cells[5].Request)
	}
}

// Single axis → N cells linearly.
func TestExpandSweepSingleAxis(t *testing.T) {
	spec := SweepSpec{
		Base: Request{FundID: "f", InitialCash: 100_000, EngineKind: "fallback"},
		Axes: []SweepAxis{
			{Name: "engineKind", Values: []string{"fallback", "llm", "llm-debate"}},
		},
	}
	cells, err := ExpandSweep(spec)
	if err != nil {
		t.Fatalf("ExpandSweep: %v", err)
	}
	if len(cells) != 3 {
		t.Fatalf("want 3 cells, got %d", len(cells))
	}
	if cells[0].Request.EngineKind != "fallback" || cells[2].Request.EngineKind != "llm-debate" {
		t.Errorf("engineKind not propagated: %+v", cells)
	}
}

// Symbols slice must be cloned per cell (no shared backing).
func TestExpandSweepDoesNotShareSymbolsSlice(t *testing.T) {
	spec := SweepSpec{
		Base: Request{FundID: "f", InitialCash: 100_000, Symbols: []string{"AAPL", "MSFT"}},
		Axes: []SweepAxis{
			{Name: "slippageBps", Values: []string{"3", "5"}},
		},
	}
	cells, err := ExpandSweep(spec)
	if err != nil {
		t.Fatalf("ExpandSweep: %v", err)
	}
	cells[0].Request.Symbols[0] = "MUTATED"
	if cells[1].Request.Symbols[0] == "MUTATED" {
		t.Error("Symbols slice is shared across cells")
	}
}

// Zero-axis spec → ErrSweepEmpty.
func TestExpandSweepEmptyAxes(t *testing.T) {
	_, err := ExpandSweep(SweepSpec{Base: Request{}, Axes: nil})
	if !errors.Is(err, ErrSweepEmpty) {
		t.Errorf("want ErrSweepEmpty, got %v", err)
	}
}

// Axis with empty values list → ErrSweepEmpty.
func TestExpandSweepEmptyValues(t *testing.T) {
	_, err := ExpandSweep(SweepSpec{
		Base: Request{},
		Axes: []SweepAxis{{Name: "slippageBps", Values: nil}},
	})
	if !errors.Is(err, ErrSweepEmpty) {
		t.Errorf("want ErrSweepEmpty, got %v", err)
	}
}

// Too many axes → ErrSweepTooLarge.
func TestExpandSweepTooManyAxes(t *testing.T) {
	_, err := ExpandSweep(SweepSpec{
		Base: Request{},
		Axes: []SweepAxis{
			{Name: "slippageBps", Values: []string{"3"}},
			{Name: "commissionBps", Values: []string{"5"}},
			{Name: "maxOrdersPerDay", Values: []string{"1"}},
		},
	})
	if !errors.Is(err, ErrSweepTooLarge) {
		t.Errorf("want ErrSweepTooLarge, got %v", err)
	}
}

// Too many cells (6x5=30 > 25) → ErrSweepTooLarge.
func TestExpandSweepTooManyCells(t *testing.T) {
	_, err := ExpandSweep(SweepSpec{
		Base: Request{},
		Axes: []SweepAxis{
			{Name: "slippageBps", Values: []string{"1", "2", "3", "4", "5", "6"}},
			{Name: "maxOrdersPerDay", Values: []string{"1", "2", "3", "4", "5"}},
		},
	})
	if !errors.Is(err, ErrSweepTooLarge) {
		t.Errorf("want ErrSweepTooLarge, got %v", err)
	}
}

// Unknown axis name → ErrSweepAxisUnknown.
func TestExpandSweepUnknownAxis(t *testing.T) {
	_, err := ExpandSweep(SweepSpec{
		Base: Request{},
		Axes: []SweepAxis{{Name: "fundId", Values: []string{"a", "b"}}},
	})
	if !errors.Is(err, ErrSweepAxisUnknown) {
		t.Errorf("want ErrSweepAxisUnknown, got %v", err)
	}
}

// Unparseable value → ErrSweepAxisValue.
func TestExpandSweepInvalidValue(t *testing.T) {
	_, err := ExpandSweep(SweepSpec{
		Base: Request{},
		Axes: []SweepAxis{{Name: "slippageBps", Values: []string{"not-a-number"}}},
	})
	if !errors.Is(err, ErrSweepAxisValue) {
		t.Errorf("want ErrSweepAxisValue, got %v", err)
	}
}

// engineKind with disallowed value → ErrSweepAxisValue.
func TestExpandSweepInvalidEngineKind(t *testing.T) {
	_, err := ExpandSweep(SweepSpec{
		Base: Request{},
		Axes: []SweepAxis{{Name: "engineKind", Values: []string{"gpt5"}}},
	})
	if !errors.Is(err, ErrSweepAxisValue) {
		t.Errorf("want ErrSweepAxisValue, got %v", err)
	}
}

func TestSortedAllowedSweepAxes(t *testing.T) {
	got := SortedAllowedSweepAxes()
	want := []string{"commissionBps", "engineKind", "initialCash", "maxOrdersPerDay", "slippageBps"}
	if len(got) != len(want) {
		t.Fatalf("len: got %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("[%d]: got %q, want %q", i, got[i], w)
		}
	}
}
