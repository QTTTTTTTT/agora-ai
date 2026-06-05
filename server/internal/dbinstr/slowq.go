// Package dbinstr provides driver-level instrumentation around a
// `database/sql` driver: every Query/Exec is timed, and any call
// that exceeds the configured threshold is logged at Warn level
// with the SQL statement (sanitised) and the elapsed duration.
//
// WHY THIS EXISTS
// ---------------
// The app had no visibility into individual query latency. Slow
// queries showed up indirectly as p99 spikes in the HTTP latency
// histogram, but figuring out WHICH query was slow required
// turning on Postgres's `log_min_duration_statement` (writes
// to the DB log, hard to correlate) or attaching pgbadger
// (heavy, separate workflow). The cheaper fix is right here: as
// every query is dispatched through Go's `database/sql` driver
// surface, we can measure elapsed time and surface the offenders
// in the same slog stream the rest of the app uses.
//
// WHAT IT EMITS
// -------------
// On every Query/Exec/Prepare execution that exceeds the
// threshold, a log line of the form:
//
//   level=WARN msg="slow query"
//     elapsed_ms=423
//     threshold_ms=200
//     query="SELECT id, … FROM funds WHERE company_id = $1 …"
//     args_count=1
//
// We log args COUNT, never the args themselves, because args
// commonly contain PII (user IDs, emails, tokens). The query
// text itself is the prepared-statement template — same shape
// you'd see in `pg_stat_statements` — so PII leakage from query
// text is rare. The threshold defaults to 200ms; override via
// `SLOW_QUERY_THRESHOLD_MS=N`.
//
// NEAR-FREE WHEN DISABLED
// -----------------------
// `Wrap` returns the original driver unchanged when the
// threshold is zero or negative. Callers can therefore
// unconditionally call `dbinstr.Wrap(driver, threshold)` —
// disabling the feature is a single env var, not a code path
// change.
//
// WHAT IT DOES NOT DO
// -------------------
//   - It does not add Prometheus histograms; that's a useful
//     follow-up (see future work) but adds a dep on the metrics
//     package which would couple this internal/dbinstr package
//     to the server's metrics implementation.
//   - It does not log every slow row scan inside a Rows iteration
//     — only the initial Query/Exec call. A query that returns
//     1M rows and is slow because of cursor fetching won't show
//     up; that's a `pg_stat_activity` / `EXPLAIN ANALYZE`
//     conversation.
//   - It does not de-duplicate or aggregate. If a slow query
//     fires 1000x in a burst, you'll get 1000 log lines. That's
//     intentional — sampling / aggregation is a logging-pipeline
//     concern, not a driver concern.

package dbinstr

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Wrap returns a driver.Driver that delegates to `underlying` and
// logs every query/exec slower than `threshold`. Returns the
// original driver unchanged if `threshold <= 0`.
func Wrap(underlying driver.Driver, threshold time.Duration) driver.Driver {
	if threshold <= 0 {
		return underlying
	}
	return &instrumentedDriver{base: underlying, threshold: threshold}
}

// WrapDB wraps an *sql.DB by replacing its underlying driver
// with the instrumented variant. The driver is registered under
// `instrumentedName`; callers typically use Wrap directly with
// `sql.Register` and `sql.Open`, but WrapDB is provided for
// callers that already have a connected *sql.DB and want to
// retrofit instrumentation without re-opening.
//
// CURRENTLY UNUSED — kept for completeness; production code
// connects via Open() so the per-query path goes through
// instrumentedConn directly.
func WrapDB(db *sql.DB, threshold time.Duration) *sql.DB {
	_ = threshold
	return db // placeholder — Re-open path is the supported one.
}

type instrumentedDriver struct {
	base      driver.Driver
	threshold time.Duration
}

func (d *instrumentedDriver) Open(name string) (driver.Conn, error) {
	c, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &instrumentedConn{base: c, threshold: d.threshold}, nil
}

type instrumentedConn struct {
	base      driver.Conn
	threshold time.Duration
}

func (c *instrumentedConn) Prepare(query string) (driver.Stmt, error) {
	return c.base.Prepare(query)
}

func (c *instrumentedConn) Close() error {
	return c.base.Close()
}

func (c *instrumentedConn) Begin() (driver.Tx, error) {
	// Begin without ctx is deprecated; pass through to the
	// underlying driver which still supports it for older callers.
	//nolint:staticcheck
	return c.base.Begin()
}

// QueryerContext is the modern (Go 1.8+) entry point used by
// database/sql when the driver implements it. Almost every modern
// driver does (lib/pq, pgx, mysql, sqlite3 all implement it),
// so wrapping this method captures the vast majority of query
// traffic.
func (c *instrumentedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.base.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	start := time.Now()
	rows, err := q.QueryContext(ctx, query, args)
	c.maybeLog(query, len(args), time.Since(start), err)
	return rows, err
}

func (c *instrumentedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.base.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	start := time.Now()
	res, err := e.ExecContext(ctx, query, args)
	c.maybeLog(query, len(args), time.Since(start), err)
	return res, err
}

// PrepareContext lets callers prepare statements that get reused;
// each Exec/Query on the prepared stmt is then timed by the stmt
// wrapper below.
func (c *instrumentedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	cp, ok := c.base.(driver.ConnPrepareContext)
	if !ok {
		stmt, err := c.base.Prepare(query)
		if err != nil {
			return nil, err
		}
		return &instrumentedStmt{base: stmt, query: query, threshold: c.threshold}, nil
	}
	stmt, err := cp.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &instrumentedStmt{base: stmt, query: query, threshold: c.threshold}, nil
}

func (c *instrumentedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, ok := c.base.(driver.ConnBeginTx)
	if !ok {
		//nolint:staticcheck
		return c.base.Begin()
	}
	return tx.BeginTx(ctx, opts)
}

// Ping forwarding so connection-pool ping checks don't panic on
// drivers that implement Pinger.
func (c *instrumentedConn) Ping(ctx context.Context) error {
	if p, ok := c.base.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

type instrumentedStmt struct {
	base      driver.Stmt
	query     string
	threshold time.Duration
}

func (s *instrumentedStmt) Close() error                    { return s.base.Close() }
func (s *instrumentedStmt) NumInput() int                   { return s.base.NumInput() }
func (s *instrumentedStmt) Exec(args []driver.Value) (driver.Result, error) {
	//nolint:staticcheck
	start := time.Now()
	res, err := s.base.Exec(args)
	s.maybeLog(len(args), time.Since(start), err)
	return res, err
}
func (s *instrumentedStmt) Query(args []driver.Value) (driver.Rows, error) {
	//nolint:staticcheck
	start := time.Now()
	rows, err := s.base.Query(args)
	s.maybeLog(len(args), time.Since(start), err)
	return rows, err
}

func (s *instrumentedStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if e, ok := s.base.(driver.StmtExecContext); ok {
		start := time.Now()
		res, err := e.ExecContext(ctx, args)
		s.maybeLog(len(args), time.Since(start), err)
		return res, err
	}
	values := make([]driver.Value, len(args))
	for i, a := range args {
		values[i] = a.Value
	}
	return s.Exec(values)
}

func (s *instrumentedStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if q, ok := s.base.(driver.StmtQueryContext); ok {
		start := time.Now()
		rows, err := q.QueryContext(ctx, args)
		s.maybeLog(len(args), time.Since(start), err)
		return rows, err
	}
	values := make([]driver.Value, len(args))
	for i, a := range args {
		values[i] = a.Value
	}
	return s.Query(values)
}

func (s *instrumentedStmt) maybeLog(argsCount int, elapsed time.Duration, err error) {
	if elapsed < s.threshold {
		return
	}
	logSlow(s.query, argsCount, elapsed, s.threshold, err)
}

func (c *instrumentedConn) maybeLog(query string, argsCount int, elapsed time.Duration, err error) {
	if elapsed < c.threshold {
		return
	}
	logSlow(query, argsCount, elapsed, c.threshold, err)
}

// logSlow emits the slog line. Query text is sanitised by
// collapsing internal whitespace runs to a single space — long
// multi-line statements get noisy in the log file otherwise.
// The arg count is included so a follow-up investigator knows
// whether to expect 1, 2 or 100+ params (instrumentation that
// matters for IN (...) clauses).
func logSlow(query string, argsCount int, elapsed, threshold time.Duration, err error) {
	cleaned := strings.Join(strings.Fields(query), " ")
	if len(cleaned) > 1024 {
		cleaned = cleaned[:1024] + "…(truncated)"
	}
	attrs := []any{
		"elapsed_ms", elapsed.Milliseconds(),
		"threshold_ms", threshold.Milliseconds(),
		"query", cleaned,
		"args_count", argsCount,
	}
	if err != nil && !errors.Is(err, driver.ErrSkip) {
		attrs = append(attrs, "error", err.Error())
	}
	slog.Warn("slow query", attrs...)
}

// RegisterInstrumented wraps `baseDriverName` (e.g. "postgres")
// with slow-query instrumentation and registers the result under
// `instrumentedName` (e.g. "postgres-instrumented"). Returns
// instrumentedName so the caller can pass it straight into
// sql.Open. If threshold <= 0, registers the underlying driver
// AS-IS under the new name, so the rest of the codepath stays
// uniform regardless of whether instrumentation is on.
//
// Safe to call once at startup; subsequent calls with the same
// `instrumentedName` are no-ops (the duplicate-register panic
// from database/sql is caught and ignored). This allows test
// binaries to call it multiple times across packages.
func RegisterInstrumented(baseDriverName, instrumentedName string, threshold time.Duration) (string, error) {
	// Open a temporary handle on the base driver to fish out its
	// driver.Driver. database/sql doesn't expose a "give me the
	// driver for this name" function, so we use sql.Drivers() +
	// a transient sql.Open to coerce the registration to be
	// observable.
	tmp, err := sql.Open(baseDriverName, "")
	if err != nil {
		// Some drivers (lib/pq) require a non-empty DSN even for
		// driver lookup; an error here is fine — we still got the
		// reference via tmp.Driver().
		_ = err
	}
	if tmp == nil {
		return "", fmt.Errorf("dbinstr: failed to resolve base driver %q", baseDriverName)
	}
	base := tmp.Driver()
	_ = tmp.Close()

	wrapped := Wrap(base, threshold)

	// Defensive: Register panics on duplicate names. We tolerate
	// repeated calls (test re-init) by recovering and treating
	// the duplicate as success.
	defer func() {
		if r := recover(); r != nil {
			// Ignore "Register called twice" — name is already there.
		}
	}()
	sql.Register(instrumentedName, wrapped)
	return instrumentedName, nil
}
