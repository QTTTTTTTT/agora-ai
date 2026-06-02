package llm

import "context"

// ModelABHook is the Sprint 10.1 plug-point through which the
// model-level A/B experiment engine influences routing. It
// lives in the llm package (not in internal/modelab) so the
// router can call it without an import cycle.
//
// The hook is consulted ONCE per call, inside ResolveModel,
// strictly after the explicit req.Model short-circuit and
// strictly before the per-user/agent override resolution. If
// the hook returns a non-nil ModelABDecision the router uses
// its Config verbatim — meaning the experiment arm trumps
// user-level model overrides for the scope it matches.
//
// Returning nil signals "no experiment applies to this call"
// and lets the rest of the priority chain proceed unchanged.
//
// Why before the per-user overrides:
//   - An experiment is an OPERATOR-initiated artefact. When an
//     operator says "run claude vs gpt on this fund's PM
//     calls", they explicitly want the test to win over a
//     per-user model preference.
//   - User-level overrides remain in force for ANY call that
//     falls outside the experiment scope, so users keep their
//     personalisation everywhere else.
//
// Why after the explicit req.Model:
//   - When a caller hard-codes req.Model = "gpt-4o-2024-07",
//     it's almost always a smoke-test or a one-off forensic
//     replay. Letting an experiment hijack that breaks the
//     forensic guarantee. Hard-coded model wins.
type ModelABHook func(ctx context.Context, req *ChatRequest) *ModelABDecision

// ModelABDecision is the value type a ModelABHook returns to
// signal "this call was matched to an experiment arm". A nil
// pointer means "no experiment applies" and the router must
// continue down its existing priority chain.
type ModelABDecision struct {
	// Config is the fully-formed ModelConfig the router should
	// use for this call. The hook is responsible for filling in
	// API key, BaseURL and provider-default fields from system
	// keys / defaults — the router treats it as authoritative.
	Config *ModelConfig

	// Metadata: which experiment & arm this call landed in.
	// Used downstream by the dispatcher (S10.2) and by the
	// reporting layer (S10.3) to attribute outcomes.
	ExperimentID   string
	ExperimentName string
	ArmIndex       int
	ArmName        string
	ArmLabel       string // "<provider>/<model>"
	AssignmentID   string
}

// SetModelABHook attaches a hook to the router. Pass nil to
// disable model A/B routing — useful for boot-time before the
// modelab package's DB pool is ready, or for tests that don't
// want experiments to leak between cases.
func (r *ModelRouter) SetModelABHook(hook ModelABHook) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.modelABHook = hook
}

// modelABHookSnapshot returns the current hook under the read
// lock. Returning the function value (not the field) means the
// caller invokes it OUTSIDE the lock, avoiding a re-entrant
// lock when the hook itself calls back into the router (it
// shouldn't, but the contract is clearer this way).
func (r *ModelRouter) modelABHookSnapshot() ModelABHook {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.modelABHook
}
