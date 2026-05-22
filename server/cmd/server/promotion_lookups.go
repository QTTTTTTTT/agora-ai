package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/fundai/server/internal/promotion"
)

// buildLiveMetricsLookup wires the DecayMonitor's
// promotion.LiveMetricsLookup against the platform's nav_snapshots
// table. The lookup reads the trailing-N-day window of daily NAV
// rows and derives Sharpe / Return / MaxDrawdown from the
// daily_return column.
//
// The shape mirrors backtest.computeMetrics so a decay flag fires
// when "this strategy's live behaviour looks meaningfully worse
// than the basis backtest measured the same way".
//
// When db is nil (legacy / smoke deployments without persistence)
// the closure returns nil-with-nil-error and the monitor records a
// "insufficient data" snapshot — never a false decay signal.
func buildLiveMetricsLookup(db *sql.DB) promotion.LiveMetricsLookup {
	if db == nil {
		return func(context.Context, string, int) (*promotion.LiveMetrics, error) {
			return nil, nil
		}
	}
	return func(ctx context.Context, fundID string, windowDays int) (*promotion.LiveMetrics, error) {
		if windowDays <= 0 {
			windowDays = 30
		}
		from := time.Now().UTC().AddDate(0, 0, -windowDays)
		rows, err := db.QueryContext(ctx, `
SELECT trading_date, nav, daily_return
  FROM nav_snapshots
 WHERE fund_id = $1
   AND trading_date >= $2
 ORDER BY trading_date ASC`, fundID, from)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		var (
			dates      []time.Time
			navs       []float64
			dailyRets  []float64
		)
		for rows.Next() {
			var d time.Time
			var nav, ret float64
			if err := rows.Scan(&d, &nav, &ret); err != nil {
				return nil, err
			}
			dates = append(dates, d)
			navs = append(navs, nav)
			dailyRets = append(dailyRets, ret)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(navs) < 2 {
			// Not enough samples to compute meaningful metrics.
			return &promotion.LiveMetrics{
				WindowFrom:   from,
				WindowTo:     time.Now().UTC(),
				DataComplete: false,
			}, nil
		}

		ret := navs[len(navs)-1]/navs[0] - 1
		sharpe := annualisedSharpe(dailyRets)
		dd := maxDrawdown(navs)

		// We don't have a "trade_count" projection on
		// nav_snapshots — count distinct trading days with
		// non-zero daily_return as a coarse proxy. Better than
		// nothing for the UI; the persistence layer can plug in
		// a more accurate count later.
		tradeCount := 0
		for _, r := range dailyRets {
			if r != 0 {
				tradeCount++
			}
		}

		return &promotion.LiveMetrics{
			Sharpe:       &sharpe,
			Return:       &ret,
			MaxDrawdown:  &dd,
			TradeCount:   tradeCount,
			WindowFrom:   dates[0],
			WindowTo:     dates[len(dates)-1],
			DataComplete: true,
		}, nil
	}
}

// annualisedSharpe returns the daily-return Sharpe ratio scaled to
// an annual horizon (√252). Matches the convention used inside
// backtest.computeMetrics so live vs backtest numbers are
// comparable. Returns 0 when the sample is degenerate.
func annualisedSharpe(daily []float64) float64 {
	if len(daily) < 2 {
		return 0
	}
	var sum float64
	for _, v := range daily {
		sum += v
	}
	mean := sum / float64(len(daily))
	var variance float64
	for _, v := range daily {
		d := v - mean
		variance += d * d
	}
	variance /= float64(len(daily) - 1)
	if variance <= 0 {
		return 0
	}
	stddev := math.Sqrt(variance)
	return (mean / stddev) * math.Sqrt(252)
}

// maxDrawdown returns the largest peak-to-trough decline in nav,
// expressed as a positive fraction (0.10 = 10% drawdown).
func maxDrawdown(nav []float64) float64 {
	if len(nav) == 0 {
		return 0
	}
	peak := nav[0]
	worst := 0.0
	for _, v := range nav {
		if v > peak {
			peak = v
		}
		if peak > 0 {
			dd := (peak - v) / peak
			if dd > worst {
				worst = dd
			}
		}
	}
	return worst
}

// buildDefaultEngineLookup returns a closure that reads the fund's
// `config` JSONB column and extracts a default engine kind /
// param bag. Used by the Resolver when no active promotion exists
// for a fund.
//
// The fund's config has historically held an "engineKind" string
// (Phase 2A) and an optional "engineParams" object. Anything else
// we ignore.
func buildDefaultEngineLookup(db *sql.DB) promotion.DefaultEngineLookup {
	if db == nil {
		return func(context.Context, string) (promotion.EngineSelection, error) {
			return promotion.EngineSelection{Source: "default"}, nil
		}
	}
	return func(ctx context.Context, fundID string) (promotion.EngineSelection, error) {
		row := db.QueryRowContext(ctx, `SELECT config FROM funds WHERE id = $1`, fundID)
		var blob []byte
		if err := row.Scan(&blob); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return promotion.EngineSelection{Source: "default-fund-missing"}, nil
			}
			return promotion.EngineSelection{}, err
		}
		var cfg struct {
			EngineKind   string         `json:"engineKind"`
			EngineParams map[string]any `json:"engineParams"`
		}
		if len(blob) > 0 {
			_ = json.Unmarshal(blob, &cfg)
		}
		if cfg.EngineKind == "" {
			cfg.EngineKind = "fallback"
		}
		return promotion.EngineSelection{
			EngineKind:   cfg.EngineKind,
			EngineParams: promotion.EngineParams(cfg.EngineParams),
			Source:       "default",
		}, nil
	}
}
