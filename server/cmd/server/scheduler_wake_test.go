package main

import (
	"testing"
	"time"
)

// TestClampLoopDelayEnvelope locks in the F10.2 contract: the scheduler
// loop never sleeps longer than schedulerMaxIdleDelay regardless of how
// far away the next computed trigger is, and never sleeps less than 0.
func TestClampLoopDelayEnvelope(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"negative becomes zero", -5 * time.Second, 0},
		{"zero stays zero", 0, 0},
		{"small passes through", 30 * time.Second, 30 * time.Second},
		{"exactly cap passes through", schedulerMaxIdleDelay, schedulerMaxIdleDelay},
		{"day clamped to cap", 24 * time.Hour, schedulerMaxIdleDelay},
		{"year clamped to cap", 365 * 24 * time.Hour, schedulerMaxIdleDelay},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clampLoopDelay(tc.in)
			if got != tc.want {
				t.Fatalf("clampLoopDelay(%s)=%s want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestSchedulerMaxIdleDelayConstantIsReasonable guards against accidental
// regression to a multi-hour cap (which would re-introduce the F7 bug
// where new funds sat idle for hours after creation).
func TestSchedulerMaxIdleDelayConstantIsReasonable(t *testing.T) {
	if schedulerMaxIdleDelay > 15*time.Minute {
		t.Fatalf("schedulerMaxIdleDelay=%s is too long; new funds will wait too long to be picked up", schedulerMaxIdleDelay)
	}
	if schedulerMaxIdleDelay < time.Minute {
		t.Fatalf("schedulerMaxIdleDelay=%s is too short; will hammer DB with poll-only ticks", schedulerMaxIdleDelay)
	}
}

// TestWakeBeforeStartIsNoOp verifies F10.1's safety contract: callers of
// WakeScheduler (fund CRUD code paths) don't need to know if the scheduler
// has been started yet. Wake on a fresh, never-started scheduler must not
// panic or leak goroutines.
func TestWakeBeforeStartIsNoOp(t *testing.T) {
	scheduler := newFundWorkflowScheduler(nil)
	scheduler.Wake() // pre-start path

	scheduler.mu.Lock()
	if scheduler.started {
		t.Fatal("Wake should not start the scheduler")
	}
	scheduler.mu.Unlock()
}

// TestWakeOnNilSchedulerIsSafe confirms WakeScheduler is safe to call on
// adapters that have no scheduler wired up (test harnesses, single-shot
// CLI tools that share the fundServiceAdapter).
func TestWakeOnNilSchedulerIsSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("WakeScheduler on nil scheduler panicked: %v", r)
		}
	}()
	var s *workflowServiceAdapter
	s.WakeScheduler()

	adapter := &workflowServiceAdapter{scheduler: nil}
	adapter.WakeScheduler()

	var fs *fundWorkflowScheduler
	fs.Wake()
}

// TestWakeAfterStartDeliversNotification verifies the happy path: a fund
// CRUD-triggered Wake() actually unblocks the loop's wait. We can't easily
// observe the internal loop, but we can verify the wakeCh receives the
// notification (since the channel is buffered=1, the loop's select would
// consume it on next iteration).
func TestWakeAfterStartDeliversNotification(t *testing.T) {
	scheduler := newFundWorkflowScheduler(nil)
	scheduler.mu.Lock()
	scheduler.started = true
	scheduler.wakeCh = make(chan struct{}, 1)
	wakeCh := scheduler.wakeCh
	scheduler.mu.Unlock()

	drain := func() {
		select {
		case <-wakeCh:
		default:
		}
	}
	drain()

	scheduler.Wake()

	select {
	case <-wakeCh:
		// got it
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Wake did not deliver to wakeCh within 100ms")
	}
}

// TestWakeIsIdempotent confirms calling Wake several times back-to-back
// produces a single notification, not a backlog (matches the existing
// private wake() semantics).
func TestWakeIsIdempotent(t *testing.T) {
	scheduler := newFundWorkflowScheduler(nil)
	scheduler.mu.Lock()
	scheduler.started = true
	scheduler.wakeCh = make(chan struct{}, 1)
	wakeCh := scheduler.wakeCh
	scheduler.mu.Unlock()

	for i := 0; i < 10; i++ {
		scheduler.Wake()
	}

	count := 0
loop:
	for {
		select {
		case <-wakeCh:
			count++
		default:
			break loop
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 notification queued, got %d", count)
	}
}
