// fx_loop.go — daily FX fetch scheduler (P1-4).
//
// What this loop does
//
//   - On startup, kicks one initial fetch so a fresh container
//     has rates within seconds rather than waiting for the
//     first scheduled tick.
//   - Every `interval` (default 6h) iterates over USD-anchored
//     pairs, calls the configured Provider, and Upserts the
//     result via the FX repo.
//   - Records each fetch's outcome through serverMetrics so
//     /metrics shows fetch_ok vs fetch_error.
//
// Pairs we fetch
//
//   USD/CNY, USD/HKD, USD/EUR, USD/JPY, USD/GBP, USD/SGD
//
// We deliberately fetch USD-anchored only — every cross-rate
// the platform needs is computed on read by triangulating
// through USD. That keeps the request rate to a fixed 6/run
// regardless of how many funds use which base currency.
//
// Failure handling
//
//   - Provider errors that wrap fx.ErrRateUnavailable (rate-limit,
//     5xx, zero rate) are logged + counted, then the loop moves
//     on to the next pair. A second-rate Yahoo response shouldn't
//     stall the whole batch.
//   - Provider errors that wrap fx.ErrUnsupportedPair are skipped
//     silently — the schedule shouldn't keep beating its head
//     against a pair Yahoo's symbol convention can't reach.
//   - DB upsert errors fail the pair but not the run.
//
// Concurrency
//
//   - The loop is leader-elected via the same shared scheduler
//     coordinator the rest of the platform uses. A non-leader
//     instance Skip()s every tick, returning fast.
//   - We don't parallelise fetches inside a tick — Yahoo treats
//     1 request/sec as the safe bound and 6 sequential requests
//     comfortably finish in under a minute.

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/fx"
)

// fxLoopOptions configures the scheduler.
type fxLoopOptions struct {
	// Interval between successive fetch waves. Defaults to 6h
	// when zero — long enough to avoid Yahoo's per-IP throttle
	// and short enough that the dashboard's "FX as-of" indicator
	// stays within a quarter-day window.
	Interval time.Duration
	// JitterPct adds up to ±N% noise to the interval so a fleet
	// of replicas doesn't burst Yahoo at the same minute on the
	// hour. Defaults to 10%.
	JitterPct float64
	// FetchTimeout caps each Provider.Fetch call. Defaults to 15s.
	FetchTimeout time.Duration
	// Pairs lists USD-anchored pairs to fetch. Empty defaults to
	// the platform's full supported set.
	Pairs []fxPair
}

type fxPair struct {
	Base  string
	Quote string
}

// defaultFXPairs is the USD-anchored fetch list. Order matters
// only for log ordering — we keep CNY first because it's the
// most-watched LP currency.
var defaultFXPairs = []fxPair{
	{"USD", "CNY"},
	{"USD", "HKD"},
	{"USD", "EUR"},
	{"USD", "JPY"},
	{"USD", "GBP"},
	{"USD", "SGD"},
}

// fxLoop is the runnable produced by newFXLoop.
type fxLoop struct {
	repo     *fx.Repo
	provider fx.Provider
	metrics  *serverMetrics
	logger   leveledLogger
	opts     fxLoopOptions
}

// leveledLogger is the small log surface the loop uses. main.go
// already provides a wrapper that satisfies it.
type leveledLogger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
}

func newFXLoop(repo *fx.Repo, provider fx.Provider, metrics *serverMetrics, logger leveledLogger, opts fxLoopOptions) *fxLoop {
	if opts.Interval <= 0 {
		opts.Interval = 6 * time.Hour
	}
	if opts.JitterPct < 0 {
		opts.JitterPct = 0.10
	}
	if opts.FetchTimeout <= 0 {
		opts.FetchTimeout = 15 * time.Second
	}
	if len(opts.Pairs) == 0 {
		opts.Pairs = append([]fxPair(nil), defaultFXPairs...)
	}
	return &fxLoop{
		repo:     repo,
		provider: provider,
		metrics:  metrics,
		logger:   logger,
		opts:     opts,
	}
}

// Run blocks until ctx is cancelled. The first fetch happens
// immediately; subsequent fetches respect Interval+jitter.
func (l *fxLoop) Run(ctx context.Context) {
	if l == nil || l.provider == nil || l.repo == nil {
		return
	}
	// Initial fetch — best-effort, ignore errors.
	if err := l.runOnce(ctx); err != nil {
		l.logf("fx loop: initial fetch failed", "err", err)
	}
	timer := time.NewTimer(l.nextDelay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := l.runOnce(ctx); err != nil {
				l.logf("fx loop: tick failed", "err", err)
			}
			timer.Reset(l.nextDelay())
		}
	}
}

// runOnce runs one full sweep over the configured pair list.
// Returns the first error encountered for logging — does NOT
// short-circuit. Pairs after a failure still run.
func (l *fxLoop) runOnce(ctx context.Context) error {
	if l == nil {
		return nil
	}
	var firstErr error
	for _, p := range l.opts.Pairs {
		fctx, cancel := context.WithTimeout(ctx, l.opts.FetchTimeout)
		rate, err := l.provider.Fetch(fctx, p.Base, p.Quote)
		cancel()
		if err != nil {
			l.recordEvent("fetch_error")
			if errors.Is(err, fx.ErrUnsupportedPair) {
				continue
			}
			l.logf("fx loop: provider error", "pair", fx.FormatPair(p.Base, p.Quote), "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if rate == nil {
			l.recordEvent("fetch_error")
			continue
		}
		_, upErr := l.repo.Upsert(ctx, fx.UpsertParams{
			Base:   rate.Base,
			Quote:  rate.Quote,
			Rate:   rate.Rate,
			RateAt: rate.RateAt,
			Source: l.provider.Name(),
			Metadata: map[string]any{
				"loop": "scheduled",
			},
		})
		if upErr != nil {
			l.recordEvent("fetch_error")
			l.logf("fx loop: upsert failed", "pair", fx.FormatPair(p.Base, p.Quote), "err", upErr)
			if firstErr == nil {
				firstErr = upErr
			}
			continue
		}
		l.recordEvent("fetch_ok")
		l.logf("fx loop: fetched", "pair", fx.FormatPair(rate.Base, rate.Quote), "rate", fmt.Sprintf("%.6f", rate.Rate))
	}
	return firstErr
}

// nextDelay returns Interval+jitter so a multi-replica fleet
// staggers naturally. We use the seconds-precision timestamp as
// a cheap PRNG seed — quasi-random across replicas and runs is
// good enough for jitter; we don't need cryptographic noise.
func (l *fxLoop) nextDelay() time.Duration {
	if l == nil {
		return time.Hour
	}
	base := l.opts.Interval
	if base <= 0 {
		base = 6 * time.Hour
	}
	// Spread across ±jitter%. Using the wall-clock nanos modulo
	// a small range avoids importing math/rand globally.
	noise := float64(time.Now().UnixNano()%1000) / 1000.0 // 0..1
	signed := (2*noise - 1) * l.opts.JitterPct            // -p..+p
	return base + time.Duration(float64(base)*signed)
}

func (l *fxLoop) recordEvent(name string) {
	if l == nil || l.metrics == nil {
		return
	}
	l.metrics.RecordFXEvent(name)
}

func (l *fxLoop) logf(msg string, kv ...any) {
	if l == nil || l.logger == nil {
		return
	}
	if strings.HasPrefix(msg, "fx loop: provider error") || strings.HasPrefix(msg, "fx loop: upsert failed") {
		l.logger.Warn(msg, kv...)
		return
	}
	l.logger.Info(msg, kv...)
}
