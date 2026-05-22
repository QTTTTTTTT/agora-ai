package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// DBTX is the minimum surface every repo helper needs. Both *sql.DB and
// *sql.Tx satisfy it, which lets the same code path run inside or outside a
// caller-provided transaction.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// UnitOfWork executes a body inside a single SQL transaction. The body
// receives a *sql.Tx that callers must hand to the *_WithTx repo variants.
//
// Behaviour:
//   - Panics inside body trigger an automatic rollback and re-panic.
//   - Any non-nil error returned from body triggers a rollback.
//   - All other paths attempt Commit and surface its error.
//
// This is the central seam for marketplace purchases (resource transfer +
// agent clone + listing transition + ledger write must all be atomic).
type UnitOfWork interface {
	WithinTx(ctx context.Context, fn func(tx *sql.Tx) error) error
}

type sqlUnitOfWork struct {
	db *sql.DB
}

// NewUnitOfWork wraps an *sql.DB into the UnitOfWork seam.
func NewUnitOfWork(db *sql.DB) UnitOfWork {
	return &sqlUnitOfWork{db: db}
}

// ErrNoTx is returned when callers forget to provide a transaction to
// repository helpers that explicitly require one.
var ErrNoTx = errors.New("repository: transaction is required")

func (u *sqlUnitOfWork) WithinTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if u == nil || u.db == nil {
		return errors.New("repository: unit of work is not initialised")
	}
	if fn == nil {
		return errors.New("repository: unit of work body is nil")
	}
	tx, err := u.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("repository: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// Rollback returns sql.ErrTxDone if the tx was already finalised; we
		// intentionally swallow that case because the caller's primary error
		// is what matters.
		_ = tx.Rollback()
	}()

	defer func() {
		if r := recover(); r != nil {
			// Rollback first, then re-raise so the panic still surfaces.
			_ = tx.Rollback()
			panic(r)
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("repository: commit tx: %w", err)
	}
	committed = true
	return nil
}
