// fund_context.go — W14-1 ctx propagation for embedquotaobs.
//
// MOTIVATION
// ----------
// embedquotaobs.Recorder (W13-7) wants a fundID on every
// observation so per-fund histograms and counters can answer
// "is fund X being throttled?". The Embedder.Embed signature is
// `Embed(ctx, text)` — no place to add a parameter without a
// large API ripple. Threading via ctx.WithValue is the standard
// Go idiom for this kind of cross-cutting attribution.
//
// DESIGN
// ------
// The fund ID is OPTIONAL: a missing value silently downgrades
// to "process-aggregate only" observation. This keeps the
// Embedder usable from call paths that don't know which fund
// (cold-start backfill that processes legacy memories without
// fund_id, anonymous batches) without forcing those paths to
// invent a sentinel.
//
// Public surface kept tight:
//
//   - WithFundID seals the value into a derived ctx.
//   - FundIDFromContext is the only reader.
//
// We deliberately export neither the key type nor the literal
// string — callers can ONLY interact via these two functions,
// preventing accidental key collisions and keeping the seam
// auditable.

package recall

import "context"

// fundIDKey is the ctx key. Unexported, value-typed empty
// struct — the standard Go pattern for ctx keys.
type fundIDKey struct{}

// WithFundID returns a derived context that carries fundID for
// downstream Embed observability. Empty fundID returns the
// parent unchanged (avoids planting a "" sentinel that would
// fall through and pollute per-fund metrics).
func WithFundID(ctx context.Context, fundID string) context.Context {
	if ctx == nil || fundID == "" {
		return ctx
	}
	return context.WithValue(ctx, fundIDKey{}, fundID)
}

// FundIDFromContext extracts the fundID seeded by WithFundID,
// or "" when no fund was attached. The empty-string return is
// safe to pass to embedquotaobs.Recorder.RecordCall, which
// silently drops empty IDs.
func FundIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, ok := ctx.Value(fundIDKey{}).(string)
	if !ok {
		return ""
	}
	return v
}
