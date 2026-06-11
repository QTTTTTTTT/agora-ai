// jwt_rotation_trigger.go — A4 ticker that emits a
// `secret.rotate.jwt` outbox event when the active key has aged
// past JWT_ROTATION_INTERVAL.
//
// The trigger is intentionally minimal: it only DECIDES whether
// to enqueue. The actual rotation work lives in
// jwt_rotation_handler.go so the same outbox retry / dead-letter
// machinery covers both the cron-driven and the manual-INSERT
// rotation paths.
//
// Why a 1-hour tick is enough
//
// Rotation cadence is measured in days / weeks / months. Polling
// every hour means worst-case rotation latency is ~1h after the
// interval elapses, which is well inside any meaningful "key
// should rotate every N days" SLA. The poll itself is one
// SELECT on a small table; the cost is negligible.
//
// Multi-replica safety
//
// On a deployment with N replicas, every replica will hit the
// "should I enqueue?" check at roughly the same time, and they
// can race. We treat the outbox INSERT itself as the
// deduplication boundary: a small in-process flag (lastEnqueuedAt
// + 1/2 interval) prevents the same replica from double-enqueueing
// across consecutive ticks, and the outbox handler is idempotent
// for the rotation case (a duplicate event just demotes the freshly-
// active key in favour of an even-newer one; over-rotation is
// strictly safer than under-rotation). A proper distributed lock
// (e.g. pg_advisory_lock) is overkill for the rotation cadence
// and a follow-up improvement if multi-replica gets very large.

package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/fundai/server/internal/outbox"
	"github.com/fundai/server/internal/secrets"
)

// jwtRotationTriggerOptions configures the cron.
type jwtRotationTriggerOptions struct {
	DB    *sql.DB
	Store secrets.KeyringStore
	// Interval is the rotation cadence. Zero or negative disables
	// the trigger entirely (loops forever sleeping) so deployments
	// that haven't opted into automatic rotation aren't disturbed.
	Interval time.Duration
	// PollEvery controls how often the trigger compares
	// ActiveAge against Interval. Defaults to 1h.
	PollEvery time.Duration
	Logger    *slog.Logger
	// Enqueue is the function used to push the rotation event onto
	// the outbox. Injectable so tests can substitute a fake;
	// production wiring defaults to outbox.EnqueueDB against
	// opts.DB in newJWTRotationTrigger.
	Enqueue func(ctx context.Context, ev outbox.Event) error
}

// jwtRotationTrigger is the runtime object. Run() blocks on a
// ticker until ctx is cancelled.
type jwtRotationTrigger struct {
	opts            jwtRotationTriggerOptions
	mu              sync.Mutex
	lastEnqueuedAt  time.Time
}

func newJWTRotationTrigger(opts jwtRotationTriggerOptions) *jwtRotationTrigger {
	if opts.PollEvery <= 0 {
		opts.PollEvery = time.Hour
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Enqueue == nil {
		// Production default: write straight to the outbox table.
		// Tests inject a fake so they don't need a live DB.
		opts.Enqueue = func(ctx context.Context, ev outbox.Event) error {
			return outbox.EnqueueDB(ctx, opts.DB, ev)
		}
	}
	return &jwtRotationTrigger{opts: opts}
}

// Run loops until ctx is cancelled. Disabled (Interval <= 0)
// reduces to a no-op sleep so the goroutine still exits cleanly
// at shutdown.
func (t *jwtRotationTrigger) Run(ctx context.Context) {
	if t == nil {
		return
	}
	if t.opts.Interval <= 0 {
		// Rotation disabled — sleep until cancelled. Still hold
		// the goroutine so main.go's startup pattern (`go t.Run`)
		// doesn't have to special-case the disabled config.
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(t.opts.PollEvery)
	defer ticker.Stop()
	// Immediate check on boot so a long-stopped instance that
	// comes back online doesn't sit on an aged key for up to
	// PollEvery before noticing.
	t.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.tick(ctx)
		}
	}
}

// tick is one iteration: compare ActiveAge against the cadence,
// enqueue if due. Errors are logged, not propagated — a transient
// DB hiccup must not take the trigger offline.
func (t *jwtRotationTrigger) tick(ctx context.Context) {
	age, err := t.opts.Store.ActiveAge(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// First run on an empty table. The boot-time wiring
			// is expected to seed an initial row from the env
			// keyring; if the table is still empty after that, the
			// platform is in a degraded state we should not paper
			// over by triggering a rotation. Log + skip.
			t.opts.Logger.DebugContext(ctx, "jwt-rotation-trigger: no active key in DB; skipping tick")
			return
		}
		t.opts.Logger.WarnContext(ctx, "jwt-rotation-trigger: ActiveAge failed",
			slog.String("err", err.Error()))
		return
	}
	if age < t.opts.Interval {
		return
	}
	// Self-debounce: a single replica shouldn't enqueue twice
	// within half a rotation interval even if the tick handler
	// re-runs before the outbox handler has processed the first
	// event.
	t.mu.Lock()
	if time.Since(t.lastEnqueuedAt) < t.opts.Interval/2 {
		t.mu.Unlock()
		return
	}
	t.lastEnqueuedAt = time.Now().UTC()
	t.mu.Unlock()

	if err := t.opts.Enqueue(ctx, outbox.Event{
		EventType:     jwtRotationEventType,
		AggregateType: "jwt_keyring",
		AggregateID:   "active",
		Payload: map[string]any{
			"reason":             "scheduled rotation",
			"active_age_seconds": int(age.Seconds()),
			"interval_seconds":   int(t.opts.Interval.Seconds()),
		},
	}); err != nil {
		t.opts.Logger.WarnContext(ctx, "jwt-rotation-trigger: enqueue failed",
			slog.String("err", err.Error()))
		return
	}
	t.opts.Logger.InfoContext(ctx, "jwt-rotation-trigger: enqueued rotation",
		slog.Duration("active_age", age),
		slog.Duration("interval", t.opts.Interval))
}
