// handlers.go — bundled outbox handlers.
//
// v1 ships a logging handler (writes a slog entry per event and
// reports success) so the flusher actually drains the queue out of
// the box even when nothing real is configured. When the team
// adds the first external sink (e.g. publish lineage edges to a
// public Kafka topic) it replaces the handler with a chain.
//
// MultiHandler is provided so multiple sinks can be combined
// (e.g. "log AND publish to Kafka") without coupling them.

package outbox

import (
	"context"
	"errors"
	"log/slog"
)

// LoggingHandler returns a Handler that just slogs the event and
// reports success. Useful as the default in a deployment that
// hasn't wired any real sink yet — the flusher still drains the
// queue (preventing unbounded growth) and the events are visible
// in the application log for debugging.
//
// nil logger → uses the default slog logger.
func LoggingHandler(logger *slog.Logger) Handler {
	return HandlerFunc(func(ctx context.Context, ev ConsumedEvent) error {
		l := logger
		if l == nil {
			l = slog.Default()
		}
		l.InfoContext(ctx, "outbox event handled",
			slog.String("id", ev.ID),
			slog.String("event_type", ev.EventType),
			slog.String("aggregate_type", ev.AggregateType),
			slog.String("aggregate_id", ev.AggregateID),
			slog.Int("attempts", ev.Attempts+1),
		)
		return nil
	})
}

// MultiHandler runs every wrapped handler in order. Returns the
// first error encountered; a downstream handler is NOT called
// when an upstream handler fails. Dead-letter takes precedence
// over retryable errors (matches Consume's row-marking logic).
//
// Empty slice or all-nil entries → returns a Handler that always
// succeeds, useful as a placeholder in tests.
func MultiHandler(handlers ...Handler) Handler {
	cleaned := make([]Handler, 0, len(handlers))
	for _, h := range handlers {
		if h != nil {
			cleaned = append(cleaned, h)
		}
	}
	if len(cleaned) == 0 {
		return HandlerFunc(func(_ context.Context, _ ConsumedEvent) error { return nil })
	}
	return HandlerFunc(func(ctx context.Context, ev ConsumedEvent) error {
		for _, h := range cleaned {
			if err := h.Handle(ctx, ev); err != nil {
				return err
			}
		}
		return nil
	})
}

// MustNotFail is a convenience for sinks that should refuse to
// dead-letter. Wraps a handler so the dead-letter sentinel becomes
// a retryable error instead — the row keeps coming back on every
// poll until an operator manually unblocks it. Use sparingly: a
// truly poisoned message will pin a worker forever.
func MustNotFail(h Handler) Handler {
	return HandlerFunc(func(ctx context.Context, ev ConsumedEvent) error {
		err := h.Handle(ctx, ev)
		if err != nil && errors.Is(err, ErrDead) {
			return errors.New("outbox: MustNotFail intercepted dead-letter: " + err.Error())
		}
		return err
	})
}
