// Package backtest replays the platform's decision pipeline against
// historical OHLC bars so operators can validate strategy variants
// without risking real capital or burning live workflow ticks.
//
// The package is intentionally narrow: it consumes the same
// decision.DecisionEngine the production runtime uses (so what
// you backtest IS what you'd run live) and feeds it a market
// snapshot derived from ohlc.Fetcher. It does NOT call the multi-
// agent debate, news sentiment, or fundamentals providers by
// default — those are LLM-heavy and historical news / Q-by-Q
// fundamentals are sparse upstream. Operators who want the full
// stack can plug in a custom DecisionEngine via the Request.
//
// Output: a per-day NAV curve, a list of simulated trades, and a
// metrics summary (cumulative return, annualised return, Sharpe,
// max drawdown, win rate). The runner reports progress through a
// thread-safe Progress object so async API endpoints can poll.
//
// Phase 2E of the auto-execute + decision refactor.
package backtest

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrCancelled is returned when the caller cancels the run via
// ctx.Cancel() or a Job's Cancel(). Distinct from generic errors so
// the API handler can surface a 200 with status="cancelled" rather
// than a 500.
var ErrCancelled = errors.New("backtest: cancelled")

// ErrEmptyUniverse signals that the Request.Universe + initial
// holdings produced zero tradeable instruments. The runner refuses
// to start in that case so we don't spend N days computing a
// degenerate flatline NAV.
var ErrEmptyUniverse = errors.New("backtest: empty universe")

// ErrInvalidWindow signals Start >= End or a window narrower than
// two trading days.
var ErrInvalidWindow = errors.New("backtest: invalid time window")

// Request is the input to a backtest run. Every field has a
// documented default so callers can construct a minimal Request
// (just Symbols + Start + End) and get a sensible result.
type Request struct {
	// FundID + Name identify the backtest in logs and the JobStore.
	// FundID is required; Name is optional and defaults to a
	// generated tag like "backtest-2026-05-20T12:00:00Z".
	FundID string
	Name   string

	// Market is the canonical lowercase tag (a_share / us_equity /
	// crypto / futures / hk_equity). The runner forwards it to
	// every ohlc.FetchRequest so the provider chain routes
	// correctly.
	Market string

	// Symbols is the candidate universe. The runner derives
	// "initial holdings = empty, available cash = InitialCash" by
	// default — operators can override by populating
	// InitialPositions for "start the backtest with N shares of
	// XYZ at cost C" scenarios.
	Symbols          []string
	InitialPositions []InitialPosition

	// Start / End define the inclusive backtest window in the
	// market's local timezone. We require both to be UTC; the
	// runner does NOT do timezone math — the OHLC bars dictate
	// the daily cadence and the cadence is the same UTC day the
	// bar belongs to.
	Start time.Time
	End   time.Time

	// InitialCash is the starting cash balance (in BaseCurrency).
	// Required to be > 0 unless InitialPositions is set.
	InitialCash float64

	// BaseCurrency is informational; the metrics don't do FX
	// conversion. All InitialPositions cost prices and the
	// resulting NAV are in this currency.
	BaseCurrency string

	// SlippageBps is the per-order slippage applied against you:
	// buys pay (1 + SlippageBps/10000) × close, sells receive
	// (1 - SlippageBps/10000) × close. Default 5 bps.
	SlippageBps float64
	// CommissionBps is the per-order commission applied against you.
	// Default 5 bps (≈ A-share retail brokerage).
	CommissionBps float64

	// MaxOrdersPerDay caps how many decision actions the runner
	// will execute per trading day. The decision engine can
	// propose more; surplus actions are demoted to "watch" with a
	// "max-orders-per-day capped" reasoning. Default 5.
	MaxOrdersPerDay int

	// EngineKind selects which decision.DecisionEngine the runner
	// uses. The wiring layer translates "fallback" / "llm" /
	// "llm-debate" into a concrete engine before invoking
	// Engine.Run. Default "fallback" — LLM-driven runs are an
	// explicit opt-in because they're slow and expensive.
	EngineKind string

	// Now lets tests freeze "today" for deterministic snapshots.
	Now time.Time

	// WalkForward, when non-nil, instructs the WalkForwardRunner
	// to slice [Start, End] into N folds and run each one
	// independently before stitching the results. Plain runs
	// leave this nil. See walkforward.go.
	WalkForward *WalkForwardSpec
}

// InitialPosition is a pre-loaded holding. Used when the operator
// wants to backtest "what would have happened if I had bought 100
// shares of AAPL on Jan 2 and never touched them again" — the
// runner starts with the position and lets the decision engine
// decide whether to hold / reduce / add over time.
type InitialPosition struct {
	Symbol    string
	Quantity  float64
	CostPrice float64
}

// Result is the structured output of a completed backtest. Mirrors
// the on-the-wire JSON shape directly (no Marshaler tricks); the
// HTTP handler returns it verbatim.
type Result struct {
	// FundID / Name / EngineKind echo the Request so consumers
	// don't have to keep state.
	FundID     string
	Name       string
	EngineKind string

	// Window summarises the actual replayed window.
	Start time.Time
	End   time.Time

	// InitialCash + FinalNav let the UI render the headline
	// "started at 1.00M USD, ended at 1.24M USD (+24.1%)" line
	// without recomputing.
	InitialCash float64
	FinalNav    float64

	// NavCurve is the per-day NAV snapshot, sorted ascending.
	// First entry's Nav == InitialCash + Σ(InitialPosition.Cost ×
	// Qty). Last entry's Nav == FinalNav.
	NavCurve []NavPoint

	// Trades is the per-execution log so the UI can render the
	// "what did the strategy do" table. Includes both filled
	// orders and skipped/demoted actions (Status field).
	Trades []TradeEvent

	// Metrics aggregates NavCurve + Trades into the summary the
	// UI shows above the chart.
	Metrics Metrics

	// CompletedAt records when the runner finished. Useful for
	// the job list view ("last run: 2 minutes ago").
	CompletedAt time.Time

	// WalkForward, when non-nil, carries per-fold metadata for
	// runs produced by the WalkForwardRunner. Plain runs leave
	// this nil. NavCurve + Trades + Metrics on the parent
	// Result already aggregate across folds — this struct adds
	// the per-fold breakdown the UI uses for the fold table.
	WalkForward *WalkForwardResult
}

// NavPoint is a single day's portfolio valuation.
type NavPoint struct {
	Date          time.Time
	Nav           float64
	Cash          float64
	Positions     map[string]float64 // symbol → quantity
	PositionValue float64            // Σ(qty × close)
	// DrawdownPct is the running drawdown from peak NAV up to
	// this date, as a fraction (0.12 = -12%). Always ≤ 0 on the
	// downside, 0 at peak.
	DrawdownPct float64
}

// TradeEvent is a single executed (or demoted) action in the
// simulated history.
type TradeEvent struct {
	Date       time.Time
	Symbol     string
	Action     string // "buy" | "sell" | "reduce" | "add" | "hold" | "watch"
	Status     string // "filled" | "skipped" | "no_qty" | "no_cash" | "no_quote" | "capped"
	Quantity   float64
	FillPrice  float64
	Notional   float64
	Reason     string
	Confidence float64
}

// Metrics is the summary block displayed above the NAV chart. All
// returns are fractions (0.18 = +18%).
type Metrics struct {
	CumulativeReturn float64
	AnnualizedReturn float64
	Volatility       float64 // annualised stdev of daily returns
	SharpeRatio      float64 // (mean - rf=0) / stdev, annualised
	MaxDrawdown      float64 // worst peak-to-trough; negative number
	WinRate          float64 // fraction of daily returns > 0
	TradeCount       int
	WinningTradeCount int // trades that closed profitably (round-trip P&L > 0)
	LosingTradeCount  int
}

// Progress tracks runner advancement. The runner updates it after
// each replayed trading day so the API handler can poll for live
// feedback. All fields are guarded by mu.
type Progress struct {
	mu          sync.Mutex
	TotalDays   int
	DoneDays    int
	CurrentDate time.Time
	Status      string // "queued" | "running" | "completed" | "failed" | "cancelled"
	LastError   error
}

// Snapshot returns an immutable copy of the current progress
// suitable for serialisation. The mutex is released before the
// snapshot is returned.
func (p *Progress) Snapshot() ProgressSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ProgressSnapshot{
		TotalDays:   p.TotalDays,
		DoneDays:    p.DoneDays,
		CurrentDate: p.CurrentDate,
		Status:      p.Status,
		LastError:   errString(p.LastError),
	}
}

// ProgressSnapshot is the JSON-friendly view of Progress.
type ProgressSnapshot struct {
	TotalDays   int       `json:"totalDays"`
	DoneDays    int       `json:"doneDays"`
	CurrentDate time.Time `json:"currentDate,omitempty"`
	Status      string    `json:"status"`
	LastError   string    `json:"lastError,omitempty"`
}

// markStatus is a small helper the runner uses to bump status
// without leaking the mutex.
func (p *Progress) markStatus(status string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Status = status
	p.LastError = err
}

// markDayDone records that DoneDays is done.
func (p *Progress) markDayDone(day time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.DoneDays++
	p.CurrentDate = day
}

// markTotal records the planned trading-day count up front so the
// UI can render a determinate progress bar.
func (p *Progress) markTotal(total int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.TotalDays = total
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Engine is the top-level façade. The wiring layer constructs one
// per process (or per request, depending on whether the
// DecisionEngine instance is shared) and the API handler invokes
// Run for synchronous use cases or hands the request to a JobStore
// for the async endpoint.
//
// The interface is intentionally tiny so test doubles are trivial.
type Engine interface {
	// Run executes the backtest synchronously. ctx cancellation is
	// honored at every per-day step; partial results aren't
	// returned (we don't want a half-rendered NAV chart implying
	// completion).
	Run(ctx context.Context, req Request, progress *Progress) (*Result, error)
}
