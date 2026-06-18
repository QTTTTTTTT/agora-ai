package llm

import "context"

// limiterBypassKey is the ctx key used to opt out of the OwnerLimiter
// and CallBudgetLimiter checks. It is intentionally an unexported
// struct type so external packages cannot forge a key with the same
// shape.
type limiterBypassKey struct{}

// WithLimiterBypass marks ctx so that OwnerLimiter and
// CallBudgetLimiter checks are skipped inside Chat.
//
// Use ONLY at trusted scheduler entry points (e.g. the daily_picks
// loop) where the workload is a known batch and the per-owner /
// per-step rate caps designed for interactive HTTP traffic do not
// apply. Real-time HTTP handlers (advisor consult, roundtable, etc.)
// MUST NOT call this — doing so disables the rate-limit safety net
// for every downstream LLM call on that request.
//
// Note: the dollar-budget gate and per-fund quota gate are NOT
// affected by this flag; cost guardrails still apply.
func WithLimiterBypass(ctx context.Context) context.Context {
	if ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, limiterBypassKey{}, true)
}

// limiterBypassed returns true when ctx carries a WithLimiterBypass
// marker.
func limiterBypassed(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(limiterBypassKey{}).(bool)
	return v
}
