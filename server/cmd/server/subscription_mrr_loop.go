// subscription_mrr_loop.go — B2 hourly refresher for the
// `subscription_mrr_usd` gauge.
//
// Why a background loop and not a per-scrape computation
//
// Prometheus scrapes the /api/metrics endpoint every ~15s. If we
// recomputed MRR on every scrape we'd be running a
// COUNT(*)+GROUP BY against `subscriptions` four times a minute
// across however many replicas Prometheus targets. The metric
// itself is a slow-changing business signal (a new sale per
// hour at most), so a once-an-hour refresh is more than enough
// fidelity for alerting on revenue drops or sign-up spikes.
//
// Methodology
//
// COUNT(*) of active rows per plan_tier, multiply by each plan's
// PriceCentsMonth (CNY cents), convert to USD via the same
// 7.2 CNY/USD factor every other operator-facing log line in
// the codebase uses (daily_picks_loop.estimateUSDFromTokens
// converts a different unit but uses the same constant). The
// gauge intentionally exposes USD only — the operator-facing
// dashboards we care about (Linear "MRR" graph, churn dash) are
// in USD, and exposing CNY-only would force every downstream
// to re-do the conversion.

package main

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/fundai/server/internal/subscription"
)

// subscriptionMRRLoopOptions configures the refresher.
type subscriptionMRRLoopOptions struct {
	// Interval between refreshes. Defaults to 1 hour. Zero or
	// negative disables the loop entirely (the metric stays at 0).
	Interval time.Duration
	// CNYPerUSD is the conversion factor. Defaults to 7.2 to
	// match the rest of the codebase. Settable so tests can
	// pin a predictable value and a future config flag can
	// inject the live FX rate.
	CNYPerUSD float64
	Logger    *slog.Logger
}

type subscriptionMRRLoop struct {
	db      *sql.DB
	metrics *serverMetrics
	opts    subscriptionMRRLoopOptions
}

func newSubscriptionMRRLoop(db *sql.DB, metrics *serverMetrics, opts subscriptionMRRLoopOptions) *subscriptionMRRLoop {
	if db == nil || metrics == nil {
		return nil
	}
	if opts.Interval == 0 {
		opts.Interval = time.Hour
	}
	if opts.CNYPerUSD <= 0 {
		opts.CNYPerUSD = 7.2
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &subscriptionMRRLoop{db: db, metrics: metrics, opts: opts}
}

// Run loops until ctx is cancelled. Refreshes once at boot so the
// gauge is non-zero from the first scrape after startup.
func (l *subscriptionMRRLoop) Run(ctx context.Context) {
	if l == nil {
		return
	}
	if l.opts.Interval <= 0 {
		<-ctx.Done()
		return
	}
	l.refresh(ctx)
	ticker := time.NewTicker(l.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.refresh(ctx)
		}
	}
}

// refresh runs the COUNT(*) GROUP BY plan_tier query, applies
// per-plan pricing, and writes the resulting USD figure into the
// gauge. Errors are logged at warn level (the metric stays at its
// previous value rather than reverting to zero so a transient DB
// hiccup doesn't trigger a false "MRR crashed to zero" alert).
func (l *subscriptionMRRLoop) refresh(ctx context.Context) {
	const q = `
		SELECT plan_tier, COUNT(*)::BIGINT
		  FROM subscriptions
		 WHERE status = 'active'
		   AND end_date > NOW()
		 GROUP BY plan_tier
	`
	rows, err := l.db.QueryContext(ctx, q)
	if err != nil {
		l.opts.Logger.WarnContext(ctx, "subscription_mrr_loop: query failed",
			slog.String("err", err.Error()))
		return
	}
	defer rows.Close()
	totalCNYCents := int64(0)
	for rows.Next() {
		var tier string
		var count int64
		if err := rows.Scan(&tier, &count); err != nil {
			l.opts.Logger.WarnContext(ctx, "subscription_mrr_loop: scan failed",
				slog.String("err", err.Error()))
			return
		}
		plan, ok := subscription.Plans[subscription.PlanTier(tier)]
		if !ok || plan == nil {
			// Unknown tier in the DB. Skip silently — better than
			// crashing the loop on a legacy / test row.
			continue
		}
		totalCNYCents += int64(plan.PriceCentsMonth) * count
	}
	if err := rows.Err(); err != nil {
		l.opts.Logger.WarnContext(ctx, "subscription_mrr_loop: rows.Err",
			slog.String("err", err.Error()))
		return
	}
	usd := float64(totalCNYCents) / 100.0 / l.opts.CNYPerUSD
	l.metrics.SetSubscriptionMRR(usd)
	l.opts.Logger.DebugContext(ctx, "subscription_mrr_loop: refreshed",
		slog.Float64("usd", usd),
		slog.Int64("cny_cents_total", totalCNYCents))
}
