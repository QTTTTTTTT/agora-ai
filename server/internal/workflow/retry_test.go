package workflow

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"testing"
	"time"
)

func TestIsTransientNil(t *testing.T) {
	if IsTransient(nil) {
		t.Fatal("nil error must not be transient")
	}
}

func TestIsTransientControlFlow(t *testing.T) {
	for _, err := range []error{errAwaitingApproval, errPlanRejected} {
		if IsTransient(err) {
			t.Errorf("control flow error %v must not be retried", err)
		}
	}
}

func TestIsTransientContextCancellation(t *testing.T) {
	if IsTransient(context.Canceled) {
		t.Fatal("context.Canceled must not be retried")
	}
	if IsTransient(context.DeadlineExceeded) {
		t.Fatal("context.DeadlineExceeded must not be retried")
	}
	wrapped := fmt.Errorf("wrapped: %w", context.Canceled)
	if IsTransient(wrapped) {
		t.Fatal("wrapped context.Canceled must not be retried")
	}
}

func TestIsTransientPermanentError(t *testing.T) {
	err := NewPermanent(errors.New("validation: missing field"))
	if IsTransient(err) {
		t.Fatal("PermanentError must not be retried")
	}
	if !strings.Contains(err.Error(), "permanent") {
		t.Fatalf("expected 'permanent' in error string, got %q", err.Error())
	}
	if errors.Unwrap(err).Error() != "validation: missing field" {
		t.Fatalf("Unwrap must return inner error, got %v", errors.Unwrap(err))
	}
}

func TestNewPermanentNilPassThrough(t *testing.T) {
	if NewPermanent(nil) != nil {
		t.Fatal("NewPermanent(nil) must return nil")
	}
}

type fakeNetErr struct{}

func (fakeNetErr) Error() string   { return "fake net error" }
func (fakeNetErr) Timeout() bool   { return false }
func (fakeNetErr) Temporary() bool { return true }

var _ net.Error = fakeNetErr{}

func TestIsTransientNetError(t *testing.T) {
	if !IsTransient(fakeNetErr{}) {
		t.Fatal("net.Error must be transient")
	}
}

func TestIsTransientHintMatching(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"connection reset by peer", true},
		{"connection refused", true},
		{"dial tcp: lookup foo: no such host", true},
		{"i/o timeout", true},
		{"context deadline exceeded retrieving foo", true},
		{"OpenAI returned 503 service unavailable", true},
		{"upstream 502 bad gateway", true},
		{"429 too many requests", true},
		{"rate limit exceeded", true},
		{"throttle: please slow down", true},
		{"unexpected EOF", true},
		// Non-transient — business / validation errors.
		{"invalid plan: missing risk score", false},
		{"fund does not exist", false},
		{"401 unauthorized", false},
		{"403 forbidden", false},
		{"400 bad request", false},
		{"plan already approved", false},
	}
	for _, tc := range cases {
		got := IsTransient(errors.New(tc.msg))
		if got != tc.want {
			t.Errorf("IsTransient(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

func TestComputeBackoffNoBaseDelayIsZero(t *testing.T) {
	policy := RetryPolicy{MaxAttempts: 3, BaseDelay: 0, Multiplier: 2}
	for attempt := 1; attempt <= 3; attempt++ {
		if got := computeBackoff(policy, attempt, nil); got != 0 {
			t.Errorf("attempt %d: got %v, want 0", attempt, got)
		}
	}
}

func TestComputeBackoffFirstAttemptIsZero(t *testing.T) {
	policy := RetryPolicy{MaxAttempts: 3, BaseDelay: 100 * time.Millisecond, Multiplier: 2}
	if got := computeBackoff(policy, 1, nil); got != 0 {
		t.Errorf("attempt 1 must be 0 delay, got %v", got)
	}
}

func TestComputeBackoffExponential(t *testing.T) {
	policy := RetryPolicy{
		MaxAttempts: 5,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    10 * time.Second,
		Multiplier:  2,
		Jitter:      0,
	}
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
	}
	for i, w := range want {
		got := computeBackoff(policy, i+2, nil)
		if got != w {
			t.Errorf("attempt %d: got %v, want %v", i+2, got, w)
		}
	}
}

func TestComputeBackoffMaxDelayCap(t *testing.T) {
	policy := RetryPolicy{
		MaxAttempts: 10,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    500 * time.Millisecond,
		Multiplier:  2,
	}
	for attempt := 5; attempt <= 10; attempt++ {
		got := computeBackoff(policy, attempt, nil)
		if got > 500*time.Millisecond {
			t.Errorf("attempt %d: got %v exceeds MaxDelay 500ms", attempt, got)
		}
	}
}

func TestComputeBackoffJitterEnvelope(t *testing.T) {
	policy := RetryPolicy{
		MaxAttempts: 3,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    0,
		Multiplier:  1,
		Jitter:      0.5,
	}
	rng := rand.New(rand.NewSource(42))
	min, max := 100*time.Millisecond, 150*time.Millisecond
	for i := 0; i < 100; i++ {
		got := computeBackoff(policy, 2, rng)
		if got < min || got > max {
			t.Errorf("iter %d: jitter result %v outside [%v, %v]", i, got, min, max)
		}
	}
}

func TestDefaultStepRetryPolicy(t *testing.T) {
	// Allow-listed steps — must be retried.
	for _, step := range []WorkflowStep{StepMacroBrief, StepResearchParallel, StepQuantSignals, StepDailyReview} {
		p := defaultStepRetryPolicy(step)
		if p.MaxAttempts < 2 {
			t.Errorf("step %v expected MaxAttempts >= 2, got %d", step, p.MaxAttempts)
		}
	}
	// Idempotency-risk steps — must NOT retry.
	for _, step := range []WorkflowStep{StepRoundtable, StepPMPlan, StepRiskReview, StepUserApproval, StepTradeExecution, StepSettlement} {
		p := defaultStepRetryPolicy(step)
		if p.MaxAttempts != 1 {
			t.Errorf("step %v MUST stay at 1 attempt (idempotency risk), got %d", step, p.MaxAttempts)
		}
	}
}

// TestRunWithRetrySuccessFirstAttempt proves no sleep + 1 attempt on
// happy path — the most common case, must add zero latency.
func TestRunWithRetrySuccessFirstAttempt(t *testing.T) {
	o := newOrchestratorForRetryTest()
	calls := 0
	start := time.Now()
	err, attempts := o.runWithRetry(context.Background(), StepMacroBrief, func(ctx context.Context) error {
		calls++
		return nil
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 1 {
		t.Errorf("expected 1 attempt, got %d", attempts)
	}
	if calls != 1 {
		t.Errorf("expected fn called once, got %d", calls)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("happy path should be ~instant, took %v", elapsed)
	}
}

// TestRunWithRetryTransientThenSuccess simulates a flaky LLM API: fails
// twice with a 503, succeeds on the third attempt. The exact behaviour
// the retry feature was built to handle.
func TestRunWithRetryTransientThenSuccess(t *testing.T) {
	o := newOrchestratorForRetryTest()
	calls := 0
	err, attempts := o.runWithRetry(context.Background(), StepMacroBrief, func(ctx context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("503 service unavailable")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success on retry, got %v", err)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
	if calls != 3 {
		t.Errorf("expected 3 fn calls, got %d", calls)
	}
}

// TestRunWithRetryPermanentNoRetry proves a non-transient error returns
// immediately without burning the remaining budget — critical for
// validation errors that would never succeed on retry.
func TestRunWithRetryPermanentNoRetry(t *testing.T) {
	o := newOrchestratorForRetryTest()
	calls := 0
	err, attempts := o.runWithRetry(context.Background(), StepMacroBrief, func(ctx context.Context) error {
		calls++
		return NewPermanent(errors.New("invalid plan"))
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if attempts != 1 {
		t.Errorf("expected 1 attempt for permanent error, got %d", attempts)
	}
	if calls != 1 {
		t.Errorf("expected fn called once for permanent error, got %d", calls)
	}
}

// TestRunWithRetryAllAttemptsFail returns the last error after exhausting
// the budget. Verifies caller sees the actual failure, not a wrapped one.
func TestRunWithRetryAllAttemptsFail(t *testing.T) {
	o := newOrchestratorForRetryTest()
	calls := 0
	want := errors.New("connection reset")
	err, attempts := o.runWithRetry(context.Background(), StepMacroBrief, func(ctx context.Context) error {
		calls++
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("expected last err to be %v, got %v", want, err)
	}
	if attempts != 3 {
		t.Errorf("MacroBrief policy is 3 attempts, got %d", attempts)
	}
	if calls != 3 {
		t.Errorf("expected 3 fn calls, got %d", calls)
	}
}

// TestRunWithRetryNoRetryStepStaysAtOneAttempt is the trade-execution
// safety test: even on a transient-looking error, MaxAttempts=1 steps
// must never call fn twice.
func TestRunWithRetryNoRetryStepStaysAtOneAttempt(t *testing.T) {
	o := newOrchestratorForRetryTest()
	calls := 0
	_, attempts := o.runWithRetry(context.Background(), StepTradeExecution, func(ctx context.Context) error {
		calls++
		return errors.New("503 service unavailable")
	})
	if attempts != 1 {
		t.Fatalf("StepTradeExecution must stay at 1 attempt, got %d (double-submit risk!)", attempts)
	}
	if calls != 1 {
		t.Fatalf("StepTradeExecution must call fn exactly once, got %d", calls)
	}
}

// TestRunWithRetryDoesNotRetryBudgetExceeded is the F14 safety test:
// a budget-exhausted error must short-circuit retries. Otherwise we'd
// burn 3x the call-count limiter quota on a known-blocked owner.
func TestRunWithRetryDoesNotRetryBudgetExceeded(t *testing.T) {
	o := newOrchestratorForRetryTest()
	calls := 0
	_, attempts := o.runWithRetry(context.Background(), StepMacroBrief, func(ctx context.Context) error {
		calls++
		return ErrLLMBudgetExceeded
	})
	if attempts != 1 {
		t.Fatalf("budget exceeded must NOT retry, got %d attempts", attempts)
	}
	if calls != 1 {
		t.Fatalf("budget exceeded must call fn exactly once, got %d", calls)
	}
}

// TestIsTransientBudgetExceeded confirms the retry classifier rejects
// budget exhaustion at the sentinel level too (defense in depth — the
// retry loop already short-circuits, but if a future change moves the
// classifier elsewhere, this guards against regressions).
func TestIsTransientBudgetExceeded(t *testing.T) {
	if IsTransient(ErrLLMBudgetExceeded) {
		t.Fatal("ErrLLMBudgetExceeded must NOT be transient")
	}
}

// TestRunWithRetryRespectsContextCancellation proves a cancelled context
// short-circuits remaining attempts. Important so a shutdown signal
// doesn't get held up by a 10s exponential sleep.
func TestRunWithRetryRespectsContextCancellation(t *testing.T) {
	o := newOrchestratorForRetryTest()
	ctx, cancel := context.WithCancel(context.Background())

	calls := 0
	go func() {
		// Cancel after the first call so the retry sleep is interrupted.
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, attempts := o.runWithRetry(ctx, StepMacroBrief, func(ctx context.Context) error {
		calls++
		return errors.New("connection refused")
	})

	if calls < 1 || calls > 2 {
		t.Errorf("expected 1-2 fn calls before cancellation, got %d", calls)
	}
	if attempts > 2 {
		t.Errorf("expected <=2 attempts before cancellation, got %d", attempts)
	}
}

// newOrchestratorForRetryTest creates a minimal orchestrator suitable
// for retry-only unit tests. Avoids the heavyweight construction in
// daily_test.go because retry has no dependencies on agents / bus.
func newOrchestratorForRetryTest() *DailyOrchestrator {
	o := NewDailyOrchestrator(
		"fund-test",
		&nopEventBus{},
		nil, // researchers
		nil, // pm
		nil, // approval
		nil, // trading
		nil, // memory
	)
	// Tighten the backoff so tests run fast. Inject a deterministic
	// RNG so jitter is reproducible.
	o.retryRNG = rand.New(rand.NewSource(1))
	// We can't directly patch defaultStepRetryPolicy from outside the
	// package — instead each test uses a step that already has a fast
	// policy (MaxAttempts: 3, BaseDelay: 500ms) and verifies behaviour
	// in terms of attempts, not wall-clock. The 500ms base is fine
	// for a test suite that runs each retry test in ~1.5s worst case.
	o.state = newWorkflowState("fund-test", "2026-05-18")
	return o
}

type nopEventBus struct{}

func (nopEventBus) Publish(context.Context, WorkflowEvent) error { return nil }
