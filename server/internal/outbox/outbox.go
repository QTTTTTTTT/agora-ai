// Package outbox implements the transactional-outbox pattern over
// the outbox_events table (migration 112).
//
// What it solves
//
//   Today the audit / lineage / attribution writers have *Tx
//   variants that piggy-back on the business transaction. That
//   gives us local consistency — if the buyout tx commits, the
//   lineage edge is durable; if it rolls back, neither lands.
//
//   The gap is FAN-OUT TO EXTERNAL SYSTEMS. The moment we want to
//   "publish a lineage event to Kafka / S3 / a public provenance
//   feed", the dual-write problem comes back: do you write the
//   external system before or after the tx? Either order has a
//   failure window that loses data or double-publishes.
//
//   Outbox closes the gap: every fan-out becomes "INSERT into the
//   outbox table inside the same tx as the business row", and a
//   single background flusher reads pending rows, calls the
//   configured handler, and stamps consumed_at on success.
//
// Why we ship the primitive now even though the v1 handler is a
// no-op logger
//
//   Adding outbox enqueues retroactively is easy when there's
//   already a `tx *sql.Tx` available at the call site — the call
//   chain is the same shape we'd need anyway. But discovering
//   later that a writer DOESN'T accept a tx (so we can't add the
//   enqueue) is the kind of refactor that takes months because
//   it threads through 30 call sites. Building the primitive +
//   conventions now means a v1.1 PR can switch on real handlers
//   in a single file.
//
// Semantics summary
//
//   Producer:
//     • Holds a business tx already
//     • Calls Enqueue(ctx, tx, Event{...}) — INSERT happens
//       inside that tx, so commit/rollback decides the row's fate
//
//   Flusher (one process, possibly multiple replicas):
//     • Polls every PollInterval
//     • SELECT … FOR UPDATE SKIP LOCKED LIMIT batchSize — multi-
//       replica safe, never delivers the same row twice in
//       parallel
//     • Invokes the registered handler for each row
//     • Success → consumed_at = now()
//     • Retryable failure → attempts++, last_error stamped, row
//       remains visible to next poll (back-off via attempts)
//     • Hard failure (returned ErrDead) → row marked consumed with
//       last_error="dead" so it stops blocking the queue

package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Event is what a producer enqueues. The Payload is arbitrary JSON
// — convention is "self-describing", i.e. the handler can dispatch
// purely on EventType without needing schema metadata.
type Event struct {
	EventType     string `json:"event_type"`
	AggregateType string `json:"aggregate_type,omitempty"`
	AggregateID   string `json:"aggregate_id,omitempty"`
	Payload       any    `json:"payload"`
}

// ConsumedEvent is the shape passed to the Handler. Includes the
// persisted identifiers (ID, CreatedAt, Attempts) so a handler
// implementation can do idempotency keying on the outbox id.
type ConsumedEvent struct {
	ID            string
	EventType     string
	AggregateType string
	AggregateID   string
	Payload       json.RawMessage
	CreatedAt     time.Time
	Attempts      int
}

// Handler is the interface a downstream system implements. Return
// nil for success → row marked consumed. Return ErrDead to abandon
// the row (logged, marked consumed with last_error="dead"). Any
// other error is treated as transient → attempts++, retry on next
// poll.
//
// CRITICAL: the handler MUST be idempotent. The flusher's SKIP
// LOCKED claim is "at-most-once delivery per concurrent poll" but
// crash recovery can replay an event whose handler completed but
// whose UPDATE consumed_at did not.
type Handler interface {
	Handle(ctx context.Context, ev ConsumedEvent) error
}

// HandlerFunc is the func adapter.
type HandlerFunc func(ctx context.Context, ev ConsumedEvent) error

func (f HandlerFunc) Handle(ctx context.Context, ev ConsumedEvent) error { return f(ctx, ev) }

// ErrDead is the sentinel that tells the flusher "this row is
// poison — don't keep retrying". The row is marked consumed with
// last_error="dead" and the operator can replay it via the admin
// surface (not implemented in v1; a manual UPDATE works).
var ErrDead = errors.New("outbox: dead-letter")

// ---------------------------------------------------------------------------
// Producer side — Enqueue
// ---------------------------------------------------------------------------

// Enqueue inserts an event row inside the caller's transaction.
// The caller is responsible for tx.Commit()/Rollback() — outbox
// has no opinion on the surrounding business logic.
//
// db parameter is unused when tx is non-nil; it's accepted as a
// convenience for the "no business tx; enqueue standalone" case
// where the call ends up doing its own implicit single-statement
// transaction.
func Enqueue(ctx context.Context, tx *sql.Tx, ev Event) error {
	if tx == nil {
		return errors.New("outbox: Enqueue requires a non-nil tx; use EnqueueDB for standalone publishes")
	}
	if strings.TrimSpace(ev.EventType) == "" {
		return errors.New("outbox: EventType required")
	}
	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		return fmt.Errorf("outbox: marshal payload: %w", err)
	}
	const q = `
		INSERT INTO outbox_events (event_type, aggregate_type, aggregate_id, payload)
		VALUES ($1, $2, $3, $4)
	`
	if _, err := tx.ExecContext(ctx, q, ev.EventType, ev.AggregateType, ev.AggregateID, payload); err != nil {
		return fmt.Errorf("outbox: insert event: %w", err)
	}
	return nil
}

// EnqueueDB is the "no business tx" convenience entrypoint. Opens
// its own single-statement transaction. Use Enqueue inside an
// existing tx whenever you can — that's what makes outbox safe.
func EnqueueDB(ctx context.Context, db *sql.DB, ev Event) error {
	if db == nil {
		return errors.New("outbox: db required")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("outbox: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := Enqueue(ctx, tx, ev); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Consumer side — Consume one batch
// ---------------------------------------------------------------------------

// Consume claims up to limit pending rows via FOR UPDATE SKIP
// LOCKED, invokes the handler for each, and stamps consumed_at on
// success. Returns the number of rows attempted (regardless of
// per-row success).
//
// SKIP LOCKED makes this safe to run from multiple flusher
// replicas: each row is exclusively claimed by exactly one tx for
// the duration of its handler call.
func Consume(ctx context.Context, db *sql.DB, h Handler, limit int) (int, error) {
	if db == nil || h == nil {
		return 0, errors.New("outbox: db and handler required")
	}
	if limit <= 0 {
		limit = 32
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("outbox: begin consume tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const q = `
		SELECT id, event_type, aggregate_type, aggregate_id, payload, created_at, attempts
		FROM outbox_events
		WHERE consumed_at IS NULL
		ORDER BY created_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`
	rows, err := tx.QueryContext(ctx, q, limit)
	if err != nil {
		return 0, fmt.Errorf("outbox: select pending: %w", err)
	}
	claimed := make([]ConsumedEvent, 0, limit)
	for rows.Next() {
		var ev ConsumedEvent
		if err := rows.Scan(
			&ev.ID, &ev.EventType, &ev.AggregateType, &ev.AggregateID,
			&ev.Payload, &ev.CreatedAt, &ev.Attempts,
		); err != nil {
			rows.Close()
			return 0, fmt.Errorf("outbox: scan claimed: %w", err)
		}
		claimed = append(claimed, ev)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("outbox: iterate claimed: %w", err)
	}

	for _, ev := range claimed {
		if hErr := h.Handle(ctx, ev); hErr != nil {
			// Mark dead OR retryable.
			markErr := hErr.Error()
			dead := errors.Is(hErr, ErrDead)
			if err := markFailureTx(ctx, tx, ev.ID, markErr, dead); err != nil {
				return len(claimed), err
			}
			continue
		}
		if err := markConsumedTx(ctx, tx, ev.ID); err != nil {
			return len(claimed), err
		}
	}
	if err := tx.Commit(); err != nil {
		return len(claimed), fmt.Errorf("outbox: commit consume tx: %w", err)
	}
	return len(claimed), nil
}

// truncateError keeps the last_error column readable in pg_stat
// views — we don't want a 50KB stack trace permanently bloating
// the queue.
const maxErrorLen = 1024

func markConsumedTx(ctx context.Context, tx *sql.Tx, id string) error {
	const q = `UPDATE outbox_events SET consumed_at = now(), last_error = '' WHERE id = $1`
	_, err := tx.ExecContext(ctx, q, id)
	if err != nil {
		return fmt.Errorf("outbox: mark consumed: %w", err)
	}
	return nil
}

func markFailureTx(ctx context.Context, tx *sql.Tx, id, errMsg string, dead bool) error {
	if len(errMsg) > maxErrorLen {
		errMsg = errMsg[:maxErrorLen]
	}
	if dead {
		const q = `UPDATE outbox_events SET attempts = attempts + 1, last_error = $2, consumed_at = now() WHERE id = $1`
		_, err := tx.ExecContext(ctx, q, id, "dead: "+errMsg)
		if err != nil {
			return fmt.Errorf("outbox: mark dead: %w", err)
		}
		return nil
	}
	const q = `UPDATE outbox_events SET attempts = attempts + 1, last_error = $2 WHERE id = $1`
	if _, err := tx.ExecContext(ctx, q, id, errMsg); err != nil {
		return fmt.Errorf("outbox: mark failure: %w", err)
	}
	return nil
}
