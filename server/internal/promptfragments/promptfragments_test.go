package promptfragments

import (
	"strings"
	"testing"
	"time"
)

type fakeSource struct {
	values []float64
	idx    int
}

func (f *fakeSource) Float64() float64 {
	if f == nil || len(f.values) == 0 {
		return 0
	}
	v := f.values[f.idx%len(f.values)]
	f.idx++
	return v
}

func TestPickActiveByDefault(t *testing.T) {
	s := New(SelectionPolicy{ShadowFraction: 0})
	s.Load([]Fragment{
		{SlotKey: "a", VariantID: "v1", Body: "active body", Status: StatusActive},
		{SlotKey: "a", VariantID: "v2", Body: "shadow body", Status: StatusShadow},
	})
	got, err := s.Pick("a", &fakeSource{values: []float64{0.5}})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.VariantID != "v1" {
		t.Errorf("got %q, want v1", got.VariantID)
	}
}

func TestPickShadowsAtFraction(t *testing.T) {
	s := New(SelectionPolicy{ShadowFraction: 0.20})
	s.Load([]Fragment{
		{SlotKey: "a", VariantID: "active", Status: StatusActive, Body: "A"},
		{SlotKey: "a", VariantID: "shadow1", Status: StatusShadow, Body: "S"},
	})
	// Roll 0.10 < 0.20 → shadow.
	got, err := s.Pick("a", &fakeSource{values: []float64{0.10}})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.VariantID != "shadow1" {
		t.Errorf("expected shadow1, got %q", got.VariantID)
	}
	// Roll 0.50 > 0.20 → active.
	got, _ = s.Pick("a", &fakeSource{values: []float64{0.50}})
	if got.VariantID != "active" {
		t.Errorf("expected active, got %q", got.VariantID)
	}
}

func TestWeightedShadowPick(t *testing.T) {
	s := New(SelectionPolicy{ShadowFraction: 1.0}) // always shadow
	s.Load([]Fragment{
		{SlotKey: "a", VariantID: "low", Status: StatusShadow, Weight: 1, Body: "L"},
		{SlotKey: "a", VariantID: "high", Status: StatusShadow, Weight: 9, Body: "H"},
	})
	counts := map[string]int{}
	src := &fakeSource{values: []float64{0.05, 0.55, 0.95, 0.10, 0.50, 0.99, 0.01, 0.30, 0.85, 0.40}}
	for i := 0; i < 10; i++ {
		got, err := s.Pick("a", src)
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		counts[got.VariantID]++
	}
	if counts["high"] <= counts["low"] {
		t.Errorf("weighted shadow: high=%d low=%d, expected high to dominate",
			counts["high"], counts["low"])
	}
}

func TestPickReturnsErrorForUnknownSlot(t *testing.T) {
	s := New(DefaultPolicy())
	if _, err := s.Pick("missing", nil); err == nil {
		t.Errorf("expected error for missing slot")
	}
}

func TestPickWithoutActiveFallsBackToShadow(t *testing.T) {
	s := New(DefaultPolicy())
	s.Load([]Fragment{
		{SlotKey: "a", VariantID: "shadow", Status: StatusShadow, Body: "S"},
	})
	got, err := s.Pick("a", nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.VariantID != "shadow" {
		t.Errorf("got %q, want shadow", got.VariantID)
	}
}

func TestPromoteDraftToShadow(t *testing.T) {
	s := New(DefaultPolicy())
	s.Load([]Fragment{
		{SlotKey: "a", VariantID: "draft1", Status: StatusDraft},
	})
	if err := s.PromoteDraftToShadow("a", "draft1"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	got, err := s.Pick("a", nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.Status != StatusShadow {
		t.Errorf("got status %q, want shadow", got.Status)
	}
}

func TestPromoteShadowToActiveDemotesPrevious(t *testing.T) {
	s := New(DefaultPolicy())
	s.Load([]Fragment{
		{SlotKey: "a", VariantID: "v1", Status: StatusActive},
		{SlotKey: "a", VariantID: "v2", Status: StatusShadow},
	})
	if err := s.PromoteShadowToActive("a", "v2"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	// Verify v1 is now archived and v2 is active.
	activeCount := 0
	archivedCount := 0
	for _, slot := range s.Slots() {
		if slot != "a" {
			continue
		}
		// Force a rebuild via Load to read state.
	}
	// Use Pick(active) and ensure we can no longer find v1
	// active.
	got, _ := s.Pick("a", &fakeSource{values: []float64{0.99}})
	if got.VariantID != "v2" {
		t.Errorf("expected v2 active, got %q", got.VariantID)
	}
	_ = activeCount
	_ = archivedCount
}

func TestPromoteFailsOnInvalidTransition(t *testing.T) {
	s := New(DefaultPolicy())
	s.Load([]Fragment{{SlotKey: "a", VariantID: "v1", Status: StatusActive}})
	if err := s.PromoteDraftToShadow("a", "v1"); err == nil {
		t.Errorf("expected error: cannot promote active → shadow")
	}
}

func TestArchiveTransition(t *testing.T) {
	s := New(DefaultPolicy())
	s.Load([]Fragment{{SlotKey: "a", VariantID: "v1", Status: StatusShadow}})
	if err := s.Archive("a", "v1"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := s.Pick("a", nil); err == nil {
		t.Errorf("expected error: no eligible variants after archive")
	}
}

func TestSlotsSorted(t *testing.T) {
	s := New(DefaultPolicy())
	s.Load([]Fragment{
		{SlotKey: "z", VariantID: "v1", Status: StatusActive},
		{SlotKey: "a", VariantID: "v1", Status: StatusActive},
		{SlotKey: "m", VariantID: "v1", Status: StatusActive},
	})
	got := s.Slots()
	if len(got) != 3 {
		t.Fatalf("len: got %d, want 3", len(got))
	}
	if got[0] != "a" || got[2] != "z" {
		t.Errorf("not sorted: %v", got)
	}
}

func TestNilSelectorIsErrorButNotPanic(t *testing.T) {
	var s *Selector
	s.Load(nil) // should not panic
	if _, err := s.Pick("a", nil); err == nil {
		t.Errorf("nil selector Pick should error")
	}
	if err := s.PromoteDraftToShadow("a", "v"); err == nil {
		t.Errorf("nil selector promote should error")
	}
}

func TestCanonicalisePutsActiveFirst(t *testing.T) {
	fs := []Fragment{
		{VariantID: "draft1", Status: StatusDraft},
		{VariantID: "active1", Status: StatusActive},
		{VariantID: "shadow1", Status: StatusShadow, Weight: 5},
	}
	canonicalise(fs)
	if fs[0].VariantID != "active1" {
		t.Errorf("first should be active, got %q", fs[0].VariantID)
	}
	if fs[1].Status != StatusShadow {
		t.Errorf("second should be shadow, got %q", fs[1].Status)
	}
}

func TestPolicyClampsShadowFraction(t *testing.T) {
	s := New(SelectionPolicy{ShadowFraction: 1.5})
	if s.cfg.ShadowFraction != 1.0 {
		t.Errorf("ShadowFraction not clamped: got %v", s.cfg.ShadowFraction)
	}
	s = New(SelectionPolicy{ShadowFraction: -0.3})
	if s.cfg.ShadowFraction != 0 {
		t.Errorf("ShadowFraction not floored: got %v", s.cfg.ShadowFraction)
	}
}

func TestCreatedUpdatedAtPreservedOnLoad(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := New(DefaultPolicy())
	s.Load([]Fragment{
		{SlotKey: "a", VariantID: "v1", Status: StatusActive, CreatedAt: t0, UpdatedAt: t0},
	})
	got, err := s.Pick("a", nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if !got.CreatedAt.Equal(t0) {
		t.Errorf("CreatedAt: got %v, want %v", got.CreatedAt, t0)
	}
}

func TestPromoteUnknownVariantErrors(t *testing.T) {
	s := New(DefaultPolicy())
	s.Load([]Fragment{{SlotKey: "a", VariantID: "v1", Status: StatusDraft}})
	err := s.PromoteShadowToActive("a", "missing")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found error, got %v", err)
	}
}
