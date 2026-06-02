package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fundai/server/internal/fx"
)

// fakeFXProvider is a deterministic stand-in for the loop tests.
// Each Fetch returns the configured rate or, when the pair is in
// errorPairs, the configured error. Concurrency-safe.
type fakeFXProvider struct {
	mu          sync.Mutex
	calls       int
	rates       map[string]float64
	errorPairs  map[string]error
	calledWith  []string
}

func newFakeFXProvider(rates map[string]float64, errorPairs map[string]error) *fakeFXProvider {
	return &fakeFXProvider{rates: rates, errorPairs: errorPairs}
}

func (p *fakeFXProvider) Name() string { return "yahoo" }

func (p *fakeFXProvider) Fetch(ctx context.Context, base, quote string) (*fx.Rate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	pair := fx.FormatPair(base, quote)
	p.calledWith = append(p.calledWith, pair)
	if err, ok := p.errorPairs[pair]; ok {
		return nil, err
	}
	r, ok := p.rates[pair]
	if !ok {
		return nil, fx.ErrRateUnavailable
	}
	return &fx.Rate{
		Base:   base,
		Quote:  quote,
		Rate:   r,
		RateAt: time.Now().UTC(),
		Source: p.Name(),
	}, nil
}

// stubLogger satisfies leveledLogger without spamming test output.
type stubLogger struct {
	infos []string
	warns []string
	mu    sync.Mutex
}

func (l *stubLogger) Info(msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, fmt.Sprintf("%s %v", msg, kv))
}
func (l *stubLogger) Warn(msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, fmt.Sprintf("%s %v", msg, kv))
}

func newFXLoopEnv(t *testing.T, p *fakeFXProvider) (*fxLoop, sqlmock.Sqlmock, *serverMetrics, *stubLogger, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	repo := fx.NewRepo(db)
	mets := newServerMetrics()
	log := &stubLogger{}
	loop := newFXLoop(repo, p, mets, log, fxLoopOptions{
		Interval:     time.Hour,
		FetchTimeout: 100 * time.Millisecond,
	})
	return loop, mock, mets, log, func() { _ = db.Close() }
}

func TestFXLoop_RunOnce_HappyPath(t *testing.T) {
	provider := newFakeFXProvider(map[string]float64{
		"USD/CNY": 7.18,
		"USD/HKD": 7.80,
		"USD/EUR": 0.93,
		"USD/JPY": 149.5,
		"USD/GBP": 0.79,
		"USD/SGD": 1.35,
	}, nil)
	loop, mock, mets, _, cleanup := newFXLoopEnv(t, provider)
	defer cleanup()
	for i := 0; i < 6; i++ {
		mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO fx_rates")).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(fmt.Sprintf("rate-%d", i)))
	}
	if err := loop.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce err = %v", err)
	}
	if provider.calls != 6 {
		t.Errorf("calls = %d, want 6", provider.calls)
	}
	if mets.fxEvents["fetch_ok"] != 6 {
		t.Errorf("fetch_ok = %d, want 6", mets.fxEvents["fetch_ok"])
	}
}

func TestFXLoop_RunOnce_HandlesProviderErrorPerPair(t *testing.T) {
	provider := newFakeFXProvider(map[string]float64{
		"USD/CNY": 7.18,
	}, map[string]error{
		"USD/HKD": fmt.Errorf("yahoo: %w", fx.ErrRateUnavailable),
	})
	loop, mock, mets, _, cleanup := newFXLoopEnv(t, provider)
	defer cleanup()
	loop.opts.Pairs = []fxPair{
		{"USD", "CNY"},
		{"USD", "HKD"},
	}
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO fx_rates")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("rate-1"))

	if err := loop.runOnce(context.Background()); err == nil {
		t.Error("expected non-nil firstErr")
	}
	if mets.fxEvents["fetch_ok"] != 1 {
		t.Errorf("fetch_ok = %d, want 1", mets.fxEvents["fetch_ok"])
	}
	if mets.fxEvents["fetch_error"] != 1 {
		t.Errorf("fetch_error = %d, want 1", mets.fxEvents["fetch_error"])
	}
}

func TestFXLoop_RunOnce_SilentForUnsupportedPair(t *testing.T) {
	provider := newFakeFXProvider(nil, map[string]error{
		"USD/CNY": fmt.Errorf("yahoo: %w", fx.ErrUnsupportedPair),
	})
	loop, _, mets, _, cleanup := newFXLoopEnv(t, provider)
	defer cleanup()
	loop.opts.Pairs = []fxPair{{"USD", "CNY"}}
	if err := loop.runOnce(context.Background()); err != nil {
		t.Errorf("err = %v, want nil for unsupported pair", err)
	}
	if mets.fxEvents["fetch_error"] != 1 {
		t.Errorf("fetch_error = %d, want 1", mets.fxEvents["fetch_error"])
	}
	if mets.fxEvents["fetch_ok"] != 0 {
		t.Errorf("fetch_ok = %d, want 0", mets.fxEvents["fetch_ok"])
	}
}

func TestFXLoop_NextDelay_RespectsInterval(t *testing.T) {
	loop := newFXLoop(nil, nil, nil, nil, fxLoopOptions{Interval: time.Hour, JitterPct: 0.0})
	d := loop.nextDelay()
	if d != time.Hour {
		t.Errorf("delay = %v, want exactly 1h with 0 jitter", d)
	}
}

func TestFXLoop_NextDelay_JitterStaysBounded(t *testing.T) {
	loop := newFXLoop(nil, nil, nil, nil, fxLoopOptions{Interval: time.Hour, JitterPct: 0.10})
	d := loop.nextDelay()
	if d < 54*time.Minute || d > 66*time.Minute {
		t.Errorf("delay = %v, expected within ±10%% of 1h", d)
	}
}

func TestFXLoop_Run_StopsOnCancel(t *testing.T) {
	provider := newFakeFXProvider(map[string]float64{
		"USD/CNY": 7.18,
		"USD/HKD": 7.80,
		"USD/EUR": 0.93,
		"USD/JPY": 149.5,
		"USD/GBP": 0.79,
		"USD/SGD": 1.35,
	}, nil)
	loop, mock, _, _, cleanup := newFXLoopEnv(t, provider)
	defer cleanup()
	for i := 0; i < 6; i++ {
		mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO fx_rates")).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(fmt.Sprintf("rate-%d", i)))
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		loop.Run(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("loop did not stop on cancel")
	}
}

func TestFXLoop_NilProvider_NoOp(t *testing.T) {
	loop := newFXLoop(nil, nil, nil, nil, fxLoopOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Should return immediately, not panic.
	loop.Run(ctx)
}

// Sanity — ensure errors.Is correctly unwraps the loop's wrappers.
func TestFXLoop_ErrorMatchesIs(t *testing.T) {
	wrapped := fmt.Errorf("yahoo: %w", fx.ErrRateUnavailable)
	if !errors.Is(wrapped, fx.ErrRateUnavailable) {
		t.Error("ErrRateUnavailable should match")
	}
}
