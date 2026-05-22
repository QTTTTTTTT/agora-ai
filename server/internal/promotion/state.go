package promotion

import "fmt"

// allowedTransitions encodes the legal state graph. Centralising
// it here lets the service layer ask one question ("can I move X
// to Y?") instead of scattering switch statements across every
// endpoint.
//
// Read as: from → list of legal next states.
var allowedTransitions = map[Status][]Status{
	StatusPendingReview: {StatusApproved, StatusRejected},
	// Approved can either jump straight into shadow (default
	// when shadowDays > 0) or skip to active when shadowDays==0.
	StatusApproved: {StatusShadow, StatusActive, StatusRejected},
	StatusShadow:   {StatusActive, StatusRolledBack, StatusRejected},
	StatusActive:   {StatusSuperseded, StatusRolledBack, StatusDecayed},
	// Terminal states never transition further. Leaving these
	// out of the map = no outgoing edges.
}

// CanTransition reports whether `to` is a legal next state from
// `from`. Terminal states return false for every target.
func CanTransition(from, to Status) bool {
	allowed, ok := allowedTransitions[from]
	if !ok {
		return false
	}
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

// EnsureTransition is CanTransition's error-returning sibling.
// The service layer calls this before every state change so the
// failure mode is a clean error rather than a half-mutated row.
func EnsureTransition(from, to Status) error {
	if from == to {
		return fmt.Errorf("%w: already %s", ErrIllegalTransition, from)
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: cannot move %s → %s", ErrIllegalTransition, from, to)
	}
	return nil
}

// NextStatusAfterApproval returns the natural target after an
// approval: shadow when ShadowDays > 0, active otherwise. We
// codify this here so both the API and the scheduler agree on
// the post-approval default.
func NextStatusAfterApproval(p Promotion) Status {
	if p.ShadowDays > 0 {
		return StatusShadow
	}
	return StatusActive
}
