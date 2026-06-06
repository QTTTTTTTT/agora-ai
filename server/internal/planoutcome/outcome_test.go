package planoutcome

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMarshalRoundTrip(t *testing.T) {
	want := Outcome{
		WindowKind:    WindowFixed5d,
		WindowEndedAt: time.Date(2026, 6, 5, 16, 0, 0, 0, time.UTC),
		RealizedPnL:   12345.67,
		VsBenchmark:   0.0125,
		Alpha:         0.082,
		WinRate:       0.6,
		SampleCount:   5,
		ComputedAt:    time.Date(2026, 6, 5, 16, 5, 0, 0, time.UTC),
		ComputedBy:    "fixed_window_resolver",
	}

	payload, err := Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if len(payload) == 0 {
		t.Fatalf("expected non-empty payload")
	}

	got, err := Unmarshal(payload)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.WindowKind != want.WindowKind {
		t.Errorf("WindowKind: got %q, want %q", got.WindowKind, want.WindowKind)
	}
	if !got.WindowEndedAt.Equal(want.WindowEndedAt) {
		t.Errorf("WindowEndedAt: got %v, want %v", got.WindowEndedAt, want.WindowEndedAt)
	}
	if got.RealizedPnL != want.RealizedPnL {
		t.Errorf("RealizedPnL: got %v, want %v", got.RealizedPnL, want.RealizedPnL)
	}
	if got.SampleCount != want.SampleCount {
		t.Errorf("SampleCount: got %d, want %d", got.SampleCount, want.SampleCount)
	}
}

func TestMarshalZeroIsNil(t *testing.T) {
	payload, err := Marshal(Outcome{})
	if err != nil {
		t.Fatalf("Marshal(zero): %v", err)
	}
	if payload != nil {
		t.Errorf("expected nil for zero Outcome, got %q", string(payload))
	}
}

// TestMarshalRejectsMissingWindowKind — non-zero Outcome without
// a WindowKind is a programming error: the resolver must always
// declare which window flavour produced the snapshot. Catch
// this at the encoder so a bad write never reaches Postgres.
func TestMarshalRejectsMissingWindowKind(t *testing.T) {
	_, err := Marshal(Outcome{RealizedPnL: 1, ComputedAt: time.Now()})
	if err == nil {
		t.Fatalf("expected error for missing WindowKind")
	}
	if !strings.Contains(err.Error(), "windowKind") {
		t.Errorf("expected error to mention windowKind, got %q", err.Error())
	}
}

func TestUnmarshalEmpty(t *testing.T) {
	out, err := Unmarshal(nil)
	if err != nil {
		t.Fatalf("Unmarshal(nil): %v", err)
	}
	if !out.IsZero() {
		t.Errorf("expected zero Outcome, got %+v", out)
	}
	out2, err := Unmarshal([]byte{})
	if err != nil {
		t.Fatalf("Unmarshal([]): %v", err)
	}
	if !out2.IsZero() {
		t.Errorf("expected zero Outcome, got %+v", out2)
	}
}

// fakeResolver lets tests script the resolver behaviour
// per-plan: returns the matching record or "not yet ready".
type fakeResolver struct {
	byPlan map[string]struct {
		out   Outcome
		ready bool
		err   error
	}
}

func (f *fakeResolver) ResolveForPlan(_ context.Context, planID string) (Outcome, bool, error) {
	rec, ok := f.byPlan[planID]
	if !ok {
		return Outcome{}, false, nil
	}
	return rec.out, rec.ready, rec.err
}

type fakeLister struct {
	ids []string
	err error
}

func (f *fakeLister) ListPendingOutcomePlans(_ context.Context, _ time.Time, _ int) ([]string, error) {
	return f.ids, f.err
}

type fakeWriter struct {
	calls []string
	err   error
}

func (f *fakeWriter) SetPlanOutcome(_ context.Context, planID string, _ []byte) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, planID)
	return nil
}

func TestSweeperTickResolvesAndPersists(t *testing.T) {
	now := time.Now()
	resolver := &fakeResolver{byPlan: map[string]struct {
		out   Outcome
		ready bool
		err   error
	}{
		"p-ready": {
			out:   Outcome{WindowKind: WindowFixed5d, WindowEndedAt: now, ComputedAt: now, RealizedPnL: 100},
			ready: true,
		},
		"p-pending": {
			ready: false,
		},
		"p-error": {
			err: errors.New("boom"),
		},
	}}
	lister := &fakeLister{ids: []string{"p-ready", "p-pending", "p-error"}}
	writer := &fakeWriter{}

	s := NewSweeper(lister, writer, resolver, SweeperConfig{})
	stats, err := s.Tick(context.Background(), now)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if stats.Scanned != 3 {
		t.Errorf("Scanned: got %d, want 3", stats.Scanned)
	}
	if stats.Resolved != 1 {
		t.Errorf("Resolved: got %d, want 1", stats.Resolved)
	}
	if stats.Pending != 1 {
		t.Errorf("Pending: got %d, want 1", stats.Pending)
	}
	if stats.Errors != 1 {
		t.Errorf("Errors: got %d, want 1", stats.Errors)
	}
	if stats.WroteRows != 1 {
		t.Errorf("WroteRows: got %d, want 1", stats.WroteRows)
	}
	if len(writer.calls) != 1 || writer.calls[0] != "p-ready" {
		t.Errorf("expected one SetPlanOutcome for p-ready, got %v", writer.calls)
	}
}

func TestSweeperTickNilSafe(t *testing.T) {
	var s *Sweeper
	stats, err := s.Tick(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("nil Sweeper Tick: %v", err)
	}
	if stats.Scanned != 0 {
		t.Errorf("nil Sweeper should produce zero stats, got %+v", stats)
	}

	noopSweeper := &Sweeper{}
	stats, err = noopSweeper.Tick(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("empty Sweeper Tick: %v", err)
	}
	if stats.Scanned != 0 {
		t.Errorf("empty Sweeper should produce zero stats, got %+v", stats)
	}
}

func TestSweeperTickRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	resolver := &fakeResolver{byPlan: map[string]struct {
		out   Outcome
		ready bool
		err   error
	}{
		// All plans are pending so we keep iterating; cancel
		// half-way through the loop.
		"p1": {ready: false},
		"p2": {ready: false},
		"p3": {ready: false},
	}}
	lister := &fakeLister{ids: []string{"p1", "p2", "p3"}}
	writer := &fakeWriter{}
	s := NewSweeper(lister, writer, resolver, SweeperConfig{})
	cancel()
	_, err := s.Tick(ctx, time.Now())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestNoopResolverAlwaysDeclines(t *testing.T) {
	r := NoopResolver{}
	out, ready, err := r.ResolveForPlan(context.Background(), "any")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ready {
		t.Errorf("expected NoopResolver to always say not-ready")
	}
	if !out.IsZero() {
		t.Errorf("expected zero Outcome, got %+v", out)
	}
}

func TestWindowKindIsValid(t *testing.T) {
	if !WindowFixed5d.IsValid() {
		t.Error("expected WindowFixed5d to be valid")
	}
	if WindowKind("").IsValid() {
		t.Error("expected empty WindowKind to be invalid")
	}
	if !WindowKind("custom_resolver_v2").IsValid() {
		t.Error("expected non-empty unknown WindowKind to be valid (forward compat)")
	}
}
