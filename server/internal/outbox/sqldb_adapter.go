// sqldb_adapter.go — concrete *sql.DB → DB interface bridge.
//
// Lives in its own file so the flusher.go body is decoupled from
// the database/sql import (helps test stubbing — a fake DB in a
// future test file doesn't need to satisfy method names that
// require *sql.Tx etc.).

package outbox

import (
	"context"
	"database/sql"
)

// sqlDBAdapter implements DB + the private dbAdapterType from
// flusher.go by delegating Consume to the package-level Consume
// function.
type sqlDBAdapter struct {
	db *sql.DB
}

func newSQLDBAdapter(v interface{}) (DB, bool) {
	d, ok := v.(*sql.DB)
	if !ok || d == nil {
		return nil, false
	}
	return &sqlDBAdapter{db: d}, true
}

func (a *sqlDBAdapter) consume(ctx context.Context, h Handler, limit int) (int, error) {
	return Consume(ctx, a.db, h, limit)
}
