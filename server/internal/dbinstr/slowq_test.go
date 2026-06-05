package dbinstr

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// fakeDriver / fakeConn / fakeStmt — minimal in-memory driver
// implementing the interfaces we wrap, so we can drive the
// instrumentation without booting a real Postgres.

type fakeDriver struct{ openErr error }

func (d *fakeDriver) Open(_ string) (driver.Conn, error) {
	if d.openErr != nil {
		return nil, d.openErr
	}
	return &fakeConn{}, nil
}

type fakeConn struct {
	queryDelay time.Duration
	queryErr   error
}

func (c *fakeConn) Prepare(_ string) (driver.Stmt, error) { return &fakeStmt{conn: c}, nil }
func (c *fakeConn) Close() error                          { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)             { return nil, errors.New("not supported") }

func (c *fakeConn) QueryContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	time.Sleep(c.queryDelay)
	if c.queryErr != nil {
		return nil, c.queryErr
	}
	return &fakeRows{}, nil
}

func (c *fakeConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	time.Sleep(c.queryDelay)
	return driver.RowsAffected(0), c.queryErr
}

type fakeStmt struct{ conn *fakeConn }

func (s *fakeStmt) Close() error                                            { return nil }
func (s *fakeStmt) NumInput() int                                           { return 0 }
func (s *fakeStmt) Exec(_ []driver.Value) (driver.Result, error)            { return driver.RowsAffected(0), nil }
func (s *fakeStmt) Query(_ []driver.Value) (driver.Rows, error)             { return &fakeRows{}, nil }

type fakeRows struct{}

func (r *fakeRows) Columns() []string              { return nil }
func (r *fakeRows) Close() error                   { return nil }
func (r *fakeRows) Next(_ []driver.Value) error    { return io.EOF }

func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

func TestWrap_DisabledWhenThresholdZero(t *testing.T) {
	base := &fakeDriver{}
	got := Wrap(base, 0)
	if got != base {
		t.Fatalf("Wrap(threshold=0) should return base driver as-is; got wrapped instance")
	}
	got = Wrap(base, -1*time.Second)
	if got != base {
		t.Fatalf("Wrap(threshold=-1) should return base driver as-is; got wrapped instance")
	}
}

func TestQueryContext_LogsWhenSlow(t *testing.T) {
	base := &fakeDriver{}
	wrapped := Wrap(base, 5*time.Millisecond).(*instrumentedDriver)
	conn, err := wrapped.Open("")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c := conn.(*instrumentedConn)
	c.base.(*fakeConn).queryDelay = 20 * time.Millisecond

	out := captureLog(t, func() {
		_, _ = c.QueryContext(context.Background(), "SELECT 1 FROM accounts WHERE id = $1", []driver.NamedValue{{Name: "", Ordinal: 1, Value: 7}})
	})

	if !strings.Contains(out, "slow query") {
		t.Fatalf("expected slow query log, got: %s", out)
	}
	if !strings.Contains(out, "SELECT 1 FROM accounts") {
		t.Fatalf("expected query text in log, got: %s", out)
	}
	if !strings.Contains(out, "args_count=1") {
		t.Fatalf("expected args_count=1 in log, got: %s", out)
	}
	// args themselves must NOT leak.
	if strings.Contains(out, "Value:7") || strings.Contains(out, "args_value") {
		t.Fatalf("args content leaked into log; got: %s", out)
	}
}

func TestQueryContext_NoLogWhenFast(t *testing.T) {
	base := &fakeDriver{}
	wrapped := Wrap(base, 50*time.Millisecond).(*instrumentedDriver)
	conn, err := wrapped.Open("")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c := conn.(*instrumentedConn)
	c.base.(*fakeConn).queryDelay = 1 * time.Millisecond

	out := captureLog(t, func() {
		_, _ = c.QueryContext(context.Background(), "SELECT 1", nil)
	})
	if strings.Contains(out, "slow query") {
		t.Fatalf("expected no log for fast query, got: %s", out)
	}
}

func TestExecContext_LogsWhenSlow(t *testing.T) {
	base := &fakeDriver{}
	wrapped := Wrap(base, 5*time.Millisecond).(*instrumentedDriver)
	conn, err := wrapped.Open("")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c := conn.(*instrumentedConn)
	c.base.(*fakeConn).queryDelay = 20 * time.Millisecond

	out := captureLog(t, func() {
		_, _ = c.ExecContext(context.Background(), "UPDATE accounts SET kyc='approved' WHERE id=$1", []driver.NamedValue{{Name: "", Ordinal: 1, Value: 7}})
	})
	if !strings.Contains(out, "slow query") || !strings.Contains(out, "UPDATE accounts") {
		t.Fatalf("expected exec slow log, got: %s", out)
	}
}

func TestQueryText_NormalisedAndTruncated(t *testing.T) {
	base := &fakeDriver{}
	wrapped := Wrap(base, 1*time.Millisecond).(*instrumentedDriver)
	conn, err := wrapped.Open("")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c := conn.(*instrumentedConn)
	c.base.(*fakeConn).queryDelay = 5 * time.Millisecond

	long := strings.Repeat("SELECT col_with_a_long_name, ", 80)
	multiline := "SELECT 1\nFROM   accounts\n WHERE id = $1"

	out := captureLog(t, func() {
		_, _ = c.QueryContext(context.Background(), multiline, nil)
	})
	if !strings.Contains(out, "SELECT 1 FROM accounts WHERE id = $1") {
		t.Fatalf("expected whitespace normalisation, got: %s", out)
	}

	out = captureLog(t, func() {
		_, _ = c.QueryContext(context.Background(), long, nil)
	})
	if !strings.Contains(out, "(truncated)") {
		t.Fatalf("expected truncation marker, got: %s", out)
	}
}
