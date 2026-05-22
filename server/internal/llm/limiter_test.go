package llm

import (
	"errors"
	"testing"
	"time"
)

func TestOwnerLimiterAllowPassesWhenIdle(t *testing.T) {
	l := NewOwnerLimiter(DefaultLimiterConfig())
	if err := l.Allow("owner-A", "openai"); err != nil {
		t.Fatalf("expected first call to be allowed, got %v", err)
	}
	if state := l.State("owner-A", "openai"); state != "closed" {
		t.Fatalf("expected breaker closed, got %s", state)
	}
}

func TestOwnerLimiterEmptyOwnerOrProviderBypasses(t *testing.T) {
	l := NewOwnerLimiter(LimiterConfig{
		BreakerFailureThreshold: 1,
		BreakerOpenDuration:     time.Second,
		BucketCapacity:          1,
		BucketRefillRate:        0.001,
	})
	// First call empty owner — bypassed.
	for i := 0; i < 50; i++ {
		if err := l.Allow("", "openai"); err != nil {
			t.Fatalf("empty owner should bypass, got %v", err)
		}
	}
	for i := 0; i < 50; i++ {
		if err := l.Allow("owner", ""); err != nil {
			t.Fatalf("empty provider should bypass, got %v", err)
		}
	}
}

func TestOwnerLimiterBucketRejectsWhenExhausted(t *testing.T) {
	l := NewOwnerLimiter(LimiterConfig{
		BucketCapacity:   2,
		BucketRefillRate: 0.0001, // effectively no refill in test window
	})
	// Pin time so refill won't help.
	frozen := time.Now()
	l.now = func() time.Time { return frozen }

	if err := l.Allow("o", "p"); err != nil {
		t.Fatalf("first allow: %v", err)
	}
	if err := l.Allow("o", "p"); err != nil {
		t.Fatalf("second allow: %v", err)
	}
	err := l.Allow("o", "p")
	if !IsRateLimited(err) {
		t.Fatalf("expected rate limited, got %v", err)
	}
}

func TestOwnerLimiterBucketIsolatesOwners(t *testing.T) {
	l := NewOwnerLimiter(LimiterConfig{
		BucketCapacity:   1,
		BucketRefillRate: 0.0001,
	})
	frozen := time.Now()
	l.now = func() time.Time { return frozen }

	if err := l.Allow("alice", "openai"); err != nil {
		t.Fatalf("alice first: %v", err)
	}
	// alice is now exhausted, but bob must be unaffected.
	if err := l.Allow("bob", "openai"); err != nil {
		t.Fatalf("bob should still have tokens, got %v", err)
	}
	if err := l.Allow("alice", "openai"); !IsRateLimited(err) {
		t.Fatalf("expected alice rate limited, got %v", err)
	}
	// bob is exhausted now too, but alice's other provider is independent.
	if err := l.Allow("alice", "claude"); err != nil {
		t.Fatalf("alice/claude should be independent, got %v", err)
	}
}

func TestOwnerLimiterBreakerOpensAfterThreshold(t *testing.T) {
	l := NewOwnerLimiter(LimiterConfig{
		BreakerFailureThreshold: 3,
		BreakerOpenDuration:     time.Minute,
		BreakerHalfOpenMaxCalls: 1,
		BucketCapacity:          1000,
		BucketRefillRate:        1000,
	})
	frozen := time.Now()
	l.now = func() time.Time { return frozen }

	// Three failures → open.
	for i := 0; i < 3; i++ {
		if err := l.Allow("o", "p"); err != nil {
			t.Fatalf("allow %d: %v", i, err)
		}
		l.RecordFailure("o", "p")
	}
	if state := l.State("o", "p"); state != "open" {
		t.Fatalf("expected open after 3 failures, got %s", state)
	}
	err := l.Allow("o", "p")
	if !IsCircuitOpen(err) {
		t.Fatalf("expected circuit open, got %v", err)
	}
}

func TestOwnerLimiterBreakerHalfOpenAndRecover(t *testing.T) {
	l := NewOwnerLimiter(LimiterConfig{
		BreakerFailureThreshold: 1,
		BreakerOpenDuration:     time.Minute,
		BreakerHalfOpenMaxCalls: 1,
		BucketCapacity:          100,
		BucketRefillRate:        100,
	})
	now := time.Now()
	l.now = func() time.Time { return now }

	if err := l.Allow("o", "p"); err != nil {
		t.Fatalf("allow: %v", err)
	}
	l.RecordFailure("o", "p")
	if state := l.State("o", "p"); state != "open" {
		t.Fatalf("want open, got %s", state)
	}

	// Advance past openUntil.
	now = now.Add(2 * time.Minute)
	if err := l.Allow("o", "p"); err != nil {
		t.Fatalf("half-open probe should be allowed, got %v", err)
	}
	// A second probe should be blocked while one is in flight.
	if err := l.Allow("o", "p"); !IsCircuitOpen(err) {
		t.Fatalf("expected circuit open during half-open in-flight, got %v", err)
	}
	l.RecordSuccess("o", "p")
	if state := l.State("o", "p"); state != "closed" {
		t.Fatalf("expected closed after success, got %s", state)
	}
	if err := l.Allow("o", "p"); err != nil {
		t.Fatalf("post-recovery allow: %v", err)
	}
}

func TestOwnerLimiterDisabledZeroValueIsTransparent(t *testing.T) {
	l := NewOwnerLimiter(LimiterConfig{}) // every threshold = 0 → all disabled
	for i := 0; i < 100; i++ {
		if err := l.Allow("o", "p"); err != nil {
			t.Fatalf("disabled limiter should always allow, got %v", err)
		}
	}
	// failures must not flip anything when disabled
	for i := 0; i < 100; i++ {
		l.RecordFailure("o", "p")
	}
	if err := l.Allow("o", "p"); err != nil {
		t.Fatalf("disabled limiter must still allow after failures, got %v", err)
	}
}

func TestShouldTripBreaker(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
	}{
		{"rate_limited", true},
		{"server_error", true},
		{"timeout", true},
		{"transport_error", true},
		{"invalid_request", false},
		{"provider_error", false},
		{"empty_choices", false},
	}
	for _, tc := range cases {
		err := &llmRequestError{Reason: tc.reason}
		if got := shouldTripBreaker(err); got != tc.want {
			t.Errorf("reason=%s: got %v, want %v", tc.reason, got, tc.want)
		}
	}
	if shouldTripBreaker(errors.New("plain")) {
		t.Error("plain errors should not trip the breaker")
	}
}

func TestEffectiveOwnerFallback(t *testing.T) {
	r := &ChatRequest{UserID: "u-1"}
	if got := r.EffectiveOwner(); got != "u-1" {
		t.Errorf("expected fallback to UserID, got %s", got)
	}
	r.OwnerID = "owner-2"
	if got := r.EffectiveOwner(); got != "owner-2" {
		t.Errorf("expected OwnerID, got %s", got)
	}
	var nilReq *ChatRequest
	if got := nilReq.EffectiveOwner(); got != "" {
		t.Errorf("nil receiver should return empty, got %s", got)
	}
}
