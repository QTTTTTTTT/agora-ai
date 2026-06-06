// quota_embedder.go — W5-3: wire the W4-23 embedquota.Limiter
// into the production embed call path.
//
// MOTIVATION
// ----------
// W4-23 added embedquota.Limiter with Acquire / RecordUsage but
// nothing in the runtime called it; the limiter sat idle because
// the embed worker (cmd/server/embed_loop.go) and the
// re-embedding queue (internal/memreembed) talked to recall.Embedder
// directly. This decorator closes the loop:
//
//   1. Acquire() before each call. If the limiter says wait, we
//      sleep up to the recommended duration (respecting the call's
//      ctx so a cancelled embed loop doesn't leak goroutines).
//   2. If the daily token quota is exhausted we return the
//      sentinel error without making the upstream call — the
//      embed loop already logs + skips on Embed errors so the
//      worker drains gracefully and resumes after midnight UTC.
//   3. RecordUsage() with the actual character count after a
//      successful call, converted to an OpenAI-style token
//      estimate (~4 chars/token). We deliberately do not parse
//      the upstream usage response: the embed_loop pipeline only
//      retains the vector, not the metadata, and a heuristic
//      conservatively overstates rather than understates so the
//      quota stays realistic.
//
// SCOPE
// -----
//   * Pure decorator: identical Embed signature, identical Model.
//   * No knowledge of memory shape or write-back path — the
//     existing embed_loop / memreembed callers don't need to
//     change. They just receive a decorated embedder.

package recall

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/fundai/server/internal/embedquota"
	"github.com/fundai/server/internal/embedquotaobs"
)

// QuotaEmbedder wraps an inner Embedder with rate limiting and
// daily token-quota awareness. Both fields are required; missing
// fields fall back to a no-op pass-through to keep test setups
// that bypass the limiter readable.
//
// W14-1 — optional Recorder field forwards the same observations
// to the per-fund side-car when set. The recorder is OPTIONAL:
// a nil recorder reduces this back to its original W5-3 behaviour
// so existing tests keep passing without changes.
type QuotaEmbedder struct {
	Inner   Embedder
	Limiter *embedquota.Limiter

	// Recorder is the W13-7 per-fund observability side-car.
	// When set, every successful Acquire / RecordUsage cycle
	// also lands on this recorder, keyed by the fundID extracted
	// via FundIDFromContext. nil keeps the original behaviour.
	Recorder *embedquotaobs.Recorder

	// MaxWaitPerCall caps how long Acquire's recommended wait can
	// block a single call. Defaults to 30 seconds — same horizon
	// as the OpenAI HTTP timeout, so we never burn more time
	// waiting for capacity than the call would have taken.
	MaxWaitPerCall time.Duration
}

// NewQuotaEmbedder is the standard constructor. Either argument
// being nil disables the corresponding behavior:
//   - nil inner → returns nil (caller already handled the
//     "no provider configured" case).
//   - nil limiter → returns inner verbatim, so this is safe to
//     call unconditionally during wiring even when the operator
//     hasn't enabled the quota plumbing yet.
//
// For per-fund observability, use NewQuotaEmbedderWithRecorder.
// This three-argument constructor is kept thin to avoid breaking
// the pre-W14 call sites.
func NewQuotaEmbedder(inner Embedder, limiter *embedquota.Limiter) Embedder {
	return NewQuotaEmbedderWithRecorder(inner, limiter, nil)
}

// NewQuotaEmbedderWithRecorder constructs the W14-1 fund-aware
// variant. recorder may be nil — see Recorder field doc.
func NewQuotaEmbedderWithRecorder(inner Embedder, limiter *embedquota.Limiter, recorder *embedquotaobs.Recorder) Embedder {
	if inner == nil {
		return nil
	}
	if limiter == nil {
		// No limiter → no rate gating, no observation either.
		// Caller is in "embedquota disabled" mode; both the
		// aggregate ledger and the per-fund side-car would be
		// empty for the same call path. Returning the bare
		// inner keeps the old contract.
		return inner
	}
	return &QuotaEmbedder{
		Inner:          inner,
		Limiter:        limiter,
		Recorder:       recorder,
		MaxWaitPerCall: 30 * time.Second,
	}
}

// Model proxies the inner embedder's model id so the
// memories.embedding_model column stays accurate.
func (q *QuotaEmbedder) Model() string {
	if q == nil || q.Inner == nil {
		return ""
	}
	return q.Inner.Model()
}

// Embed acquires capacity from the limiter, optionally sleeps,
// then forwards to the inner embedder. Token usage is recorded
// after the call so the daily ledger matches reality on a
// retry-after-failure path (we don't release the estimate on
// failure because the upstream call may still have charged
// against the quota).
//
// W14-1 — when q.Recorder is set, the same observations land on
// the per-fund side-car too, keyed by FundIDFromContext(ctx).
// A missing fundID degrades to "process-aggregate only"
// silently; the limiter's own histograms keep recording either
// way.
func (q *QuotaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if q == nil || q.Inner == nil {
		return nil, errors.New("recall: quota embedder unconfigured")
	}
	if q.Limiter == nil {
		return q.Inner.Embed(ctx, text)
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return q.Inner.Embed(ctx, text)
	}
	estimated := estimateTokens(trimmed)
	fundID := FundIDFromContext(ctx)

	wait, status, err := q.Limiter.Acquire(estimated)
	if err != nil {
		// Quota exhausted: surface immediately so the embed
		// worker logs and skips instead of busy-looping. Bump
		// the per-fund exhausted counter so dashboards can
		// localise the burn.
		if q.Recorder != nil {
			q.Recorder.RecordExhaust(fundID)
		}
		return nil, err
	}
	if status == embedquota.StatusThrottled && q.Recorder != nil {
		// Per-fund throttle counter is bumped on the wait
		// path; the limiter already emits the aggregate
		// counter via its own atomic. Note: we do this even
		// when wait==0 (rate window happens to be open) since
		// status alone is the discriminator.
		q.Recorder.RecordThrottle(fundID)
	}
	if wait > 0 {
		// Cap the wait so a wedged limiter doesn't hold the
		// embed worker indefinitely. If the cap fires we still
		// proceed with the call — the rate limiter's window will
		// recover on the next slot regardless.
		if q.MaxWaitPerCall > 0 && wait > q.MaxWaitPerCall {
			wait = q.MaxWaitPerCall
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	vec, err := q.Inner.Embed(ctx, text)
	if err != nil {
		// Conservative: still record the estimated usage on
		// failure. Many provider 5xx / 429 errors arrive AFTER
		// the request has been counted upstream, and over-
		// counting on retry beats under-counting silently.
		q.Limiter.RecordUsage(estimated)
		if q.Recorder != nil {
			q.Recorder.RecordCall(fundID, estimated, wait)
		}
		return nil, err
	}
	actual := estimateTokens(trimmed)
	q.Limiter.RecordUsage(actual)
	if q.Recorder != nil {
		q.Recorder.RecordCall(fundID, actual, wait)
	}
	return vec, nil
}

// estimateTokens applies the standard "≈4 chars / token" rule
// of thumb used by OpenAI's tokenizer. Cheap, deterministic,
// and conservative for English / Chinese mixed corpus (Chinese
// characters land closer to 1 token each, which this formula
// over-estimates by ~25% — fine for quota planning).
func estimateTokens(text string) int {
	n := len(text) / 4
	if n < 1 {
		n = 1
	}
	return n
}
