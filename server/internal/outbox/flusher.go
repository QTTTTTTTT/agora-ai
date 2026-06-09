// flusher.go — long-running background process that drains the
// outbox table by calling the registered Handler on each pending
// row.
//
// One instance per running cmd/server process is enough. Multiple
// replicas are SAFE thanks to the SELECT … FOR UPDATE SKIP LOCKED
// claim inside Consume; concurrent flushers will simply split the
// pending rows between them.
//
// Lifecycle
//   - main.go calls NewFlusher(db, handler, opts) and Run(ctx)
//   - Run loops on a ticker until ctx is cancelled
//   - Each tick claims up to BatchSize rows and runs them through
//     the handler. Empty queue → returns 0 → next tick sleeps the
//     full PollInterval. Busy queue → keeps draining until the
//     batch comes back empty, then sleeps.
//
// Why "drain to empty" instead of "one batch per tick"
//
//   Bursty publishers (e.g. a backfill that writes 10k rows in a
//   second) would otherwise take 10k/BatchSize ticks to clear,
//   producing artificial latency. The drain loop catches up
//   immediately; the ticker is just the idle-keepalive.

package outbox

import (
	"context"
	"log/slog"
	"time"
)

// FlusherOptions configures the background flusher.
type FlusherOptions struct {
	// PollInterval is how long to sleep when the queue is empty
	// and no work has been done. Defaults to 5s. Too long → stale
	// downstream consumers; too short → wasted DB round-trips.
	PollInterval time.Duration
	// BatchSize caps how many rows a single Consume call claims.
	// Higher = fewer round-trips per drain; lower = better
	// per-row latency under contention. Default 64 — enough to
	// amortise the round-trip without making any single failure
	// retry expensive.
	BatchSize int
	// HandlerTimeout is the per-row deadline passed to the
	// handler. Defaults to 30s. Make sure it's shorter than your
	// statement_timeout so the handler can't block the FOR
	// UPDATE row indefinitely.
	HandlerTimeout time.Duration
	// Logger is used for "drained N events" / "handler failed"
	// breadcrumbs. nil → silent.
	Logger *slog.Logger
}

// Flusher is the runtime object held by main.go.
type Flusher struct {
	db      DB
	handler Handler
	opts    FlusherOptions
}

// DB is the narrow surface Flusher needs. Decoupled from
// *sql.DB so tests can stub it without a real DB.
type DB interface {
	// Pulled to a method-receiver interface so a future "two
	// pools" rollout (read vs write) doesn't require us to refactor
	// the Flusher. For v1, *sql.DB satisfies this via the embedded
	// Consume helper.
}

// NewFlusher constructs a flusher. If opts.PollInterval is zero, a
// 5-second default is applied; BatchSize zero → 64; HandlerTimeout
// zero → 30s. A nil logger is treated as silent.
func NewFlusher(db interface{}, handler Handler, opts FlusherOptions) *Flusher {
	if opts.PollInterval <= 0 {
		opts.PollInterval = 5 * time.Second
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 64
	}
	if opts.HandlerTimeout <= 0 {
		opts.HandlerTimeout = 30 * time.Second
	}
	return &Flusher{db: dbAdapter(db), handler: handler, opts: opts}
}

// Run blocks until ctx is cancelled. Use a context with a
// cancel-on-shutdown wrapping the application's main context so
// the flusher exits cleanly during graceful shutdown.
//
// Errors from Consume are logged but do not stop the loop — a
// transient DB hiccup must not take the flusher offline; the
// next tick will retry.
func (f *Flusher) Run(ctx context.Context) error {
	if f == nil || f.db == nil || f.handler == nil {
		return nil
	}
	t := time.NewTicker(f.opts.PollInterval)
	defer t.Stop()
	for {
		// Drain pass: keep calling Consume until it returns zero
		// or an error. This empties bursts immediately instead of
		// taking N ticks.
		for {
			ctxRun, cancel := context.WithTimeout(ctx, f.opts.HandlerTimeout*time.Duration(f.opts.BatchSize)+10*time.Second)
			n, err := consumeAdapter(ctxRun, f.db, f.handler, f.opts.BatchSize)
			cancel()
			if err != nil {
				if f.opts.Logger != nil {
					f.opts.Logger.Warn("outbox flusher consume failed", slog.String("err", err.Error()))
				}
				break
			}
			if n == 0 {
				break
			}
			if f.opts.Logger != nil {
				f.opts.Logger.Debug("outbox flusher drained batch", slog.Int("count", n))
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// ---------------------------------------------------------------------------
// Adapter — bridge the interface{} DB parameter to *sql.DB
// ---------------------------------------------------------------------------

// dbAdapter wraps the interface{} param so the Flusher can call
// Consume(...) which takes *sql.DB. Keeps the public API loose
// (accept interface{}) while the implementation stays type-safe.
//
// In v1 the only supported type is *sql.DB; passing anything else
// returns a nil adapter, which makes Run a no-op.
type dbAdapterType interface {
	consume(ctx context.Context, h Handler, limit int) (int, error)
}

func dbAdapter(db interface{}) DB {
	if a, ok := newSQLDBAdapter(db); ok {
		return a
	}
	return nil
}

func consumeAdapter(ctx context.Context, db DB, h Handler, limit int) (int, error) {
	if a, ok := db.(dbAdapterType); ok {
		return a.consume(ctx, h, limit)
	}
	return 0, nil
}
