package promotion

import (
	"errors"
	"testing"
)

// Every state in the graph maps to a deterministic set of legal
// next states. Pin them with a table so accidental edits to
// allowedTransitions break the test.
func TestCanTransitionGraph(t *testing.T) {
	cases := []struct {
		from, to Status
		ok       bool
	}{
		// Pending review can be approved or rejected.
		{StatusPendingReview, StatusApproved, true},
		{StatusPendingReview, StatusRejected, true},
		{StatusPendingReview, StatusActive, false},
		{StatusPendingReview, StatusShadow, false},

		// Approved can go to shadow (default), straight to active
		// (shadowDays=0), or be rejected after the fact (rare).
		{StatusApproved, StatusShadow, true},
		{StatusApproved, StatusActive, true},
		{StatusApproved, StatusRejected, true},
		{StatusApproved, StatusPendingReview, false},

		// Shadow can flip to active, be rolled back, or rejected.
		{StatusShadow, StatusActive, true},
		{StatusShadow, StatusRolledBack, true},
		{StatusShadow, StatusRejected, true},
		{StatusShadow, StatusDecayed, false},

		// Active is terminal-ish — only superseded by a new
		// promotion, manually rolled back, or auto-decayed.
		{StatusActive, StatusSuperseded, true},
		{StatusActive, StatusRolledBack, true},
		{StatusActive, StatusDecayed, true},
		{StatusActive, StatusShadow, false},

		// Terminal states have zero outgoing edges.
		{StatusSuperseded, StatusActive, false},
		{StatusRejected, StatusActive, false},
		{StatusRolledBack, StatusActive, false},
		{StatusDecayed, StatusActive, false},

		// Self-transition is forbidden.
		{StatusActive, StatusActive, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"→"+string(tc.to), func(t *testing.T) {
			got := CanTransition(tc.from, tc.to)
			if got != tc.ok {
				t.Errorf("CanTransition(%s,%s) = %v, want %v", tc.from, tc.to, got, tc.ok)
			}
		})
	}
}

// EnsureTransition wraps CanTransition with the
// ErrIllegalTransition sentinel so the API can do errors.Is
// translation without parsing strings.
func TestEnsureTransitionWrapsSentinel(t *testing.T) {
	err := EnsureTransition(StatusActive, StatusShadow)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("expected ErrIllegalTransition, got %v", err)
	}
	if err := EnsureTransition(StatusPendingReview, StatusApproved); err != nil {
		t.Errorf("legal transition errored: %v", err)
	}
}

// IsTerminal / IsLive: pin the small set of states the rest of
// the code branches on.
func TestStatusBuckets(t *testing.T) {
	if !StatusSuperseded.IsTerminal() {
		t.Errorf("superseded should be terminal")
	}
	if StatusShadow.IsTerminal() {
		t.Errorf("shadow should NOT be terminal")
	}
	if !StatusShadow.IsLive() {
		t.Errorf("shadow should be live")
	}
	if !StatusActive.IsLive() {
		t.Errorf("active should be live")
	}
	if StatusPendingReview.IsLive() {
		t.Errorf("pending should not be live")
	}
}

// NextStatusAfterApproval: shadow when ShadowDays > 0, active
// otherwise.
func TestNextStatusAfterApproval(t *testing.T) {
	if got := NextStatusAfterApproval(Promotion{ShadowDays: 7}); got != StatusShadow {
		t.Errorf("got %s, want shadow", got)
	}
	if got := NextStatusAfterApproval(Promotion{ShadowDays: 0}); got != StatusActive {
		t.Errorf("got %s, want active", got)
	}
}

// Validate covers every required-field path.
func TestPromotionValidate(t *testing.T) {
	base := Promotion{
		FundID:     "fund-1",
		BasisJobID: "job-1",
		EngineKind: "llm",
		ProposedBy: "user-1",
		ShadowDays: 7,
		DecayRatio: 0.5,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("base should validate, got %v", err)
	}
	cases := []struct {
		name string
		mut  func(*Promotion)
	}{
		{"no fund", func(p *Promotion) { p.FundID = "" }},
		{"no basis", func(p *Promotion) { p.BasisJobID = "" }},
		{"no engine", func(p *Promotion) { p.EngineKind = "" }},
		{"no proposer", func(p *Promotion) { p.ProposedBy = "" }},
		{"shadow days negative", func(p *Promotion) { p.ShadowDays = -1 }},
		{"shadow days > 90", func(p *Promotion) { p.ShadowDays = 91 }},
		{"decay ratio 0", func(p *Promotion) { p.DecayRatio = 0 }},
		{"decay ratio 1", func(p *Promotion) { p.DecayRatio = 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mut(&p)
			if err := p.Validate(); !errors.Is(err, ErrInvalidPromotion) {
				t.Errorf("want ErrInvalidPromotion, got %v", err)
			}
		})
	}
}

// EffectiveSharpe prefers OOS over in-sample when available.
func TestEffectiveSharpePrefersOOS(t *testing.T) {
	in := 1.2
	oos := 0.7
	b := BaselineMetrics{SharpeRatio: in, OOSSharpe: &oos}
	if got := b.EffectiveSharpe(); got != 0.7 {
		t.Errorf("got %f, want 0.7", got)
	}
	b2 := BaselineMetrics{SharpeRatio: in}
	if got := b2.EffectiveSharpe(); got != 1.2 {
		t.Errorf("got %f, want 1.2", got)
	}
}
