package workflow

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"time"
)

// RetryPolicy describes how a single step's fn() should be retried when
// it returns a transient error.
//
// Conservative defaults are critical. A buggy classifier that calls a
// trade-execution step "transient" can re-submit the same order N times.
// The orchestrator's safety net:
//   - Retry is OPT-IN per step (default policy = MaxAttempts:1, no retry)
//   - Transient classification has a deny-list for known-non-retryable
//     errors (errAwaitingApproval, errPlanRejected, context cancellation)
//   - Each retry emits a step_retried event so observability shows the
//     fan-out (not hidden under a "step took 30s" silent retry)
type RetryPolicy struct {
	// MaxAttempts is the total number of fn() calls including the first.
	// Must be >= 1. A value of 1 disables retry entirely.
	MaxAttempts int

	// BaseDelay is the initial sleep between attempts.
	BaseDelay time.Duration

	// MaxDelay caps the per-attempt sleep regardless of multiplier
	// growth. Prevents a 5-attempt policy from sleeping for 30 minutes.
	MaxDelay time.Duration

	// Multiplier scales the delay between successive attempts (1.0 =
	// constant delay, 2.0 = exponential doubling, etc).
	Multiplier float64

	// Jitter, if > 0, adds a random fraction of the computed delay to
	// each sleep. Range [0, 1]. Recommended 0.2 to avoid thundering
	// herd on cohort retries.
	Jitter float64
}

// noRetryPolicy returns the explicit "do not retry" policy. Used as the
// safe default for any step not in the retryable allow-list.
func noRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 1}
}

// readOnlyLLMPolicy is appropriate for LLM-only steps that have no DB
// side effects beyond what the orchestrator records itself (macro brief,
// research summaries, post-trade narrative). Three attempts with modest
// backoff captures transient OpenAI / Anthropic 5xx + network hiccups
// without burning budget on persistent failures.
func readOnlyLLMPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: 3,
		BaseDelay:   500 * time.Millisecond,
		MaxDelay:    5 * time.Second,
		Multiplier:  2.0,
		Jitter:      0.2,
	}
}

// defaultStepRetryPolicy returns the per-step retry policy. Steps not
// listed get noRetryPolicy(). This is the single source of truth for
// "which steps are safe to retry"; new steps default to safe-no-retry
// and must be explicitly added here after auditing idempotency.
//
//	Step                | Retries | Rationale
//	--------------------|---------|----------------------------------------
//	macro_brief         | 3       | LLM-only summary, safe to repeat
//	research_parallel   | 3       | LLM-only, fan-out tolerated
//	quant_signals       | 3       | Market-data read + LLM, idempotent
//	roundtable          | 1       | Multi-agent discussion writes memories
//	                              | per-turn — re-running would duplicate
//	pm_plan             | 1       | Writes investment_plans row; needs
//	                              | dedupe before retry-safe
//	risk_review         | 1       | Writes risk_review JSON onto plan —
//	                              | naive re-run overwrites differently
//	user_approval       | 1       | Pauses; not applicable
//	trade_execution     | 1       | Submits orders — DOUBLE-SUBMIT RISK
//	                              | until broker client provides idempotency
//	settlement          | 1       | Mutates wallet / NAV; needs dedupe
//	daily_review        | 3       | LLM narrative + memory write, safe
func defaultStepRetryPolicy(step WorkflowStep) RetryPolicy {
	switch step {
	case StepMacroBrief, StepResearchParallel, StepQuantSignals, StepDailyReview:
		return readOnlyLLMPolicy()
	default:
		return noRetryPolicy()
	}
}

// IsTransient returns true when err is a candidate for retry. Returns
// false for:
//   - nil (caller should not retry success)
//   - context cancellation / deadline (already exhausted)
//   - workflow-specific control flow errors (awaiting approval, rejected)
//   - explicit Permanent errors (see PermanentError below)
//
// Everything else defaults to transient, on the principle that LLM and
// network errors are far more common than business-logic failures, and a
// false-positive retry on a deterministic bug just burns a small amount
// of budget while a false-negative refuses-to-retry on a flaky API call
// fails the entire workflow.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errAwaitingApproval) || errors.Is(err, errPlanRejected) {
		return false
	}
	if errors.Is(err, ErrLLMBudgetExceeded) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var permanent *PermanentError
	if errors.As(err, &permanent) {
		return false
	}
	// Network errors are explicitly transient. Use both net.Error
	// interface and string heuristics because some libraries wrap
	// errors in ways that hide the net.Error type assertion.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	// Common transient signatures across LLM providers / DBs / HTTP
	// stacks. Conservative — only add a substring here if a permanent
	// failure can never produce that message.
	transientHints := []string{
		"connection reset",
		"connection refused",
		"no such host",
		"timeout",
		"i/o timeout",
		"deadline",
		"temporary failure",
		"503 service unavailable",
		"502 bad gateway",
		"504 gateway timeout",
		"429 too many requests",
		"rate limit",
		"throttle",
		"eof",
	}
	for _, hint := range transientHints {
		if strings.Contains(msg, hint) {
			return true
		}
	}
	return false
}

// PermanentError wraps an underlying error and forces IsTransient to
// return false. Use it for business-logic / validation failures where a
// retry has zero chance of changing the outcome:
//
//	return workflow.NewPermanent(fmt.Errorf("missing required field"))
type PermanentError struct {
	Err error
}

func (e *PermanentError) Error() string {
	if e == nil || e.Err == nil {
		return "permanent error"
	}
	return "permanent: " + e.Err.Error()
}

func (e *PermanentError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// NewPermanent constructs a PermanentError. Returns nil if err is nil so
// callers can wrap unconditionally.
func NewPermanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// computeBackoff returns the sleep before attempt N (1-indexed). Pure
// function for unit-testing the backoff envelope. Caller passes a *rand.Rand
// so tests can seed deterministically.
func computeBackoff(policy RetryPolicy, attempt int, rng *rand.Rand) time.Duration {
	if attempt <= 1 || policy.BaseDelay <= 0 {
		return 0
	}
	multiplier := policy.Multiplier
	if multiplier <= 0 {
		multiplier = 1.0
	}
	// attempt is 1-indexed and we sleep BEFORE attempt N (N >= 2). So
	// attempt 2 should use BaseDelay (no multiplier yet), attempt 3 uses
	// BaseDelay * multiplier, attempt 4 BaseDelay * multiplier^2, etc.
	delay := float64(policy.BaseDelay)
	for i := 2; i < attempt; i++ {
		delay *= multiplier
	}
	if policy.MaxDelay > 0 && delay > float64(policy.MaxDelay) {
		delay = float64(policy.MaxDelay)
	}
	if policy.Jitter > 0 && rng != nil {
		jitter := policy.Jitter
		if jitter > 1.0 {
			jitter = 1.0
		}
		delay += delay * jitter * rng.Float64()
	}
	return time.Duration(delay)
}

// describeRetry produces the log line for a retry attempt. Kept as a
// helper so tests can assert on a stable format.
func describeRetry(step WorkflowStep, attempt, maxAttempts int, err error, delay time.Duration) string {
	return fmt.Sprintf("step %s attempt %d/%d failed (transient), retrying in %s: %v",
		step.String(), attempt, maxAttempts, delay, err)
}
