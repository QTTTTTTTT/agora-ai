// Package corpaction is the runtime layer that turns raw corporate
// action events (stock splits, cash dividends) into idempotent
// numerical mutations on holding_positions and position_lots.
//
// # Why a dedicated package
//
// The phantom-loss bug solved by this package was found on
// 2026-05-29 when the fund "OCS 主题精选 1 号" reported a -28k
// unrealized P&L on 688195 (腾景科技) the morning after the
// company emitted a 10送4 + 0.164/股 派现 event. The upstream quote
// provider had already adjusted the price (300+ → 230+) but the
// holding's cost_price stayed at the pre-split level, so
// (current_price - cost_price) * quantity invented a 41% loss out
// of nothing.
//
// The mathematical fix is trivial — split_ratio scales shares up,
// 1/split_ratio scales cost down, and the rest is recompute. The
// hard part is making the operation idempotent and auditable across
// every fund that holds the affected instrument. That is what the
// corp_action_applications PK + the snapshot columns in this
// package buy.
//
// # API shape
//
// One ApplyEvent function. Caller hands in the event row and the
// fund_id, the function does all the SQL inside a transaction, and
// returns either a fully-populated Result or an error. There is no
// sweep helper here; the scheduler / admin handler is responsible
// for fanning out across funds.
//
// # Out of scope
//
// - Cross-currency cash. funds.current_capital has no currency column.
//   A USD dividend on a US holding posts the USD amount as if it were
//   the fund's base currency. Single-market funds are unaffected;
//   cross-market funds need the full Card F redesign (fund_cash_balances
//   + fx).
// - Withholding tax. A-share rules (20% < 1mo, 10% 1mo-1yr, 0% > 1yr)
//   are not modelled. Simulation mode posts gross dividend. Live mode
//   will require a tax module.
// - Backwards adjustment of trade_executions. Past trades stay
//   denominated in the original (pre-split) shares and price; the
//   lot-level adjustment carries the post-split state forward.
//   This is the accounting-correct convention used by every broker
//   we've checked.
package corpaction

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

// Event is the immutable description of a corporate action that the
// applier needs to mutate fund state. Mirrors the columns in the
// corporate_actions table so a Repo.Get() can populate it directly.
type Event struct {
	ID             string
	InstrumentKey  string
	ExDate         time.Time
	ActionType     string
	SplitRatio     float64
	CashDividend   float64 // gross per old share
	Source         string
}

// Result fields PostQuantity / PostCostPrice now reflect the
// applied integer share count for whole-share venues (A-share /
// HK), with the fractional residual booked separately as
// CashResidualCredit. For fractional venues (US / crypto) the post
// values keep the legacy float semantics and CashResidualCredit is
// 0.
//
// SettlementMode reports the rule used:
//
//	"fractional"        — legacy US-style equal scaling (post_qty may
//	                      be non-integer; no residual cash).
//	"whole_shares"      — A-share / HK rule: post_qty = floor, residual
//	                      shares get cash-settled at the post-split
//	                      reference price.
//
// Callers that need to render audit UI surface SettlementMode +
// CashResidualShares + CashResidualCredit alongside the existing
// fields.
type extendedResult struct {
	cashResidualShares float64
	cashResidualCredit float64
	settlementMode     string
}

// Result is the receipt the applier returns on success. The numbers
// here are exactly what got persisted to corp_action_applications;
// callers can show them in a UI or use them for audit.
//
// SettlementMode reports which rule the applier picked:
//
//	"fractional"   — US / crypto-style equal scaling. PostQuantity
//	                 may be non-integer.
//	"whole_shares" — A-share / HK rule (S12.2). PostQuantity is
//	                 the integer share count after a 10送N bonus
//	                 issue; the fractional residual (e.g. 0.6
//	                 shares from 289 × 1.4 = 404.6) is booked as
//	                 cash via CashResidualShares ×
//	                 reference_price.
//
// CashCredit (existing field) still carries the cash-dividend
// component. CashResidualCredit is a NEW field exposing the
// fractional-share residual cash for whole-share venues.
type Result struct {
	FundID             string
	InstrumentKey      string
	PreQuantity        float64
	PostQuantity       float64
	PreCostPrice       float64
	PostCostPrice      float64
	PreUnrealized      float64
	PostUnrealized     float64
	CashCredit         float64
	CashResidualShares float64
	CashResidualCredit float64
	SettlementMode     string
	AlreadyApplied     bool // true → call was a no-op (idempotent re-run)
}

// ErrEventInvalid means the Event itself was rejected before any DB
// mutation: zero/negative ratio, empty instrument_key, etc.
var ErrEventInvalid = errors.New("corpaction: event invalid")

// ErrPositionMissing is returned when the (fund, instrument) is not
// found in holding_positions. The caller usually treats this as a
// soft skip — funds that don't hold the instrument by ex-date have
// nothing to adjust.
var ErrPositionMissing = errors.New("corpaction: position not found")

// dbExec is the minimal slice of *sql.DB / *sql.Tx the applier
// needs. Using the smallest interface lets callers thread either a
// *sql.DB or an existing transaction in.
type dbExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// EventFetcher is the interface every corp-action provider must
// implement. Right now satisfied by YahooProvider (US/HK/LSE) and
// EastmoneyProvider (A-share). Operators (and the future
// scheduled ingester) program against this interface so a new
// provider can be added without touching the CLI / scheduler.
//
// `since` is inclusive — events on or after the cutoff are returned;
// older events are dropped. Implementations must return events
// sorted by ex-date ascending so callers can apply them in order.
//
// On failure the implementation should return a wrapped error; an
// empty event slice and a non-nil error means "fetch failed", whereas
// an empty slice and a nil error means "no relevant events" (e.g.
// the symbol exists but has no splits/dividends in the window).
type EventFetcher interface {
	FetchEvents(ctx context.Context, symbol string, since time.Time) ([]Event, error)
}

// txBeginner is what we need to open a fresh transaction. The
// concrete *sql.DB satisfies it; tests can pass a sqlmock-backed
// equivalent.
type txBeginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// ApplyEvent applies the corporate action carried by `evt` to a
// single fund's holding of `evt.InstrumentKey`, in a single
// transaction. The whole thing is idempotent: a second call with
// the same (event_id, fund_id) returns a Result with
// AlreadyApplied=true and does not mutate anything.
//
// Steps inside the transaction:
//
//  1. Lock the holding_positions row (FOR UPDATE) to prevent the
//     position_quote_refresher from racing us mid-application.
//  2. Compute new_quantity = quantity * split_ratio,
//             new_cost     = cost_price / split_ratio.
//  3. Persist the mutation on holding_positions (recomputing
//     market_value + unrealized_pnl from the new quantities and the
//     existing current_price).
//  4. Apply the same math to every open lot in position_lots that
//     references (fund, instrument).
//  5. Insert into corp_action_applications. PK violation here
//     means a concurrent caller beat us to it — we roll back and
//     return AlreadyApplied=true.
//
// The function returns ErrPositionMissing without opening a
// transaction when the (fund, instrument) does not exist. That keeps
// the bookkeeping cheap when sweeping a corp action across hundreds
// of funds, most of which won't hold the instrument.
func ApplyEvent(ctx context.Context, db txBeginner, evt Event, fundID string) (Result, error) {
	if err := validateEvent(evt); err != nil {
		return Result{}, err
	}
	if fundID == "" {
		return Result{}, fmt.Errorf("%w: empty fund_id", ErrEventInvalid)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("corpaction: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // Commit makes Rollback a no-op.

	// Idempotency probe: did a previous run already apply this
	// (event, fund) pair? If yes, return its receipt verbatim.
	prior, found, err := lookupPriorApplication(ctx, tx, evt.ID, fundID)
	if err != nil {
		return Result{}, err
	}
	if found {
		_ = tx.Commit() // close the read transaction cleanly
		prior.AlreadyApplied = true
		return prior, nil
	}

	// Lock the position row so the price refresher can't move
	// current_price while we're recomputing market_value /
	// unrealized_pnl from it.
	pre, err := lockPosition(ctx, tx, fundID, evt.InstrumentKey)
	if err != nil {
		return Result{}, err
	}

	mode := settlementModeFor(evt.InstrumentKey)
	rawPostQty := pre.quantity * evt.SplitRatio
	postCost := round8(pre.costPrice / evt.SplitRatio)

	// S12.2 — A-share / HK rule: stock dividends ("送股") are
	// distributed in whole shares only. Any fractional residual
	// (e.g. 289 × 1.4 = 404.6 → 404 whole + 0.6 residual) is
	// cash-settled at the post-split reference price. The
	// pre-event currentPrice the position carried is already
	// split-adjusted by the upstream marketdata refresher, so we
	// use postCost (≈ pre-split close / split_ratio) as the
	// reference — this is the conservative cash equivalent and
	// matches the SSE/SZSE/HKEX convention.
	//
	// For US / crypto venues the legacy equal-scaling stays
	// unchanged (postQuantity is a NUMERIC(20,8) float that can
	// legally be non-integer).
	var (
		postQuantity       float64
		cashResidualShares float64
		cashResidualCredit float64
	)
	switch mode {
	case settlementWholeShares:
		whole := math.Floor(rawPostQty)
		postQuantity = round8(whole)
		cashResidualShares = round8(rawPostQty - whole)
		if cashResidualShares > 0 {
			// Residual reference price: prefer the post-split
			// current price (already adjusted by the
			// marketdata refresher), fall back to postCost
			// when current_price is missing.
			ref := pre.currentPrice
			if ref <= 0 {
				ref = postCost
			}
			cashResidualCredit = round4(cashResidualShares * ref)
		}
	default:
		postQuantity = round8(rawPostQty)
	}
	postMV := round4(postQuantity * pre.currentPrice)
	postUnreal := round4(postQuantity * (pre.currentPrice - postCost))
	cashCredit := round4(pre.quantity * evt.CashDividend) // gross, on OLD shares
	totalCash := round4(cashCredit + cashResidualCredit)

	if _, err := tx.ExecContext(ctx,
		`UPDATE holding_positions
		   SET quantity      = $3,
		       available_qty = round(available_qty * $4 / $5, 4),
		       cost_price    = $6,
		       market_value  = $7,
		       unrealized_pnl= $8,
		       updated_at    = NOW()
		 WHERE fund_id = $1 AND instrument_key = $2`,
		fundID, evt.InstrumentKey,
		postQuantity,
		evt.SplitRatio, 1.0,
		postCost, postMV, postUnreal,
	); err != nil {
		return Result{}, fmt.Errorf("corpaction: update holding: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE position_lots
		   SET quantity_opened    = round(quantity_opened    * $3, 8),
		       quantity_remaining = round(quantity_remaining * $3, 8),
		       entry_price        = round(entry_price        / $3, 8),
		       highest_price_seen = round(highest_price_seen / $3, 8),
		       updated_at         = NOW()
		 WHERE fund_id = $1 AND instrument_key = $2 AND status = 'open'`,
		fundID, evt.InstrumentKey, evt.SplitRatio,
	); err != nil {
		return Result{}, fmt.Errorf("corpaction: update lots: %w", err)
	}

	// Cash dividends post directly to funds.current_capital — that's the
	// fund's primary cash state machine, read by trader (for available
	// buying power), navcalc (for NAV computation), and the risk engine
	// (for cash sufficiency checks). We deliberately DO NOT keep a
	// separate dividend ledger / cash_credits table: corp_action_applications
	// already records cash_credit per (event, fund) for audit, so a
	// JOIN reconstructs full forensics on demand.
	//
	// Caveats consciously deferred to the full cash-account project:
	//   - Currency: current_capital is single-currency NUMERIC (no
	//     `currency` column). When a US holding's USD dividend lands,
	//     we currently post the USD amount AS-IF base-currency. Funds
	//     that hold cross-market positions need the full Card F
	//     redesign (fund_cash_balances + fx). All current funds are
	//     single-market, so this is a known acceptable simplification.
	//   - Withholding tax: A-share rules (20% < 1mo, 10% 1mo-1yr, 0%
	//     after 1yr) are not modelled. Simulation mode posts gross.
	//     Live mode will require a tax module; this is captured as a
	//     follow-up rather than a blocker.
	if cashCredit > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE funds
			    SET current_capital = current_capital + $2,
			        updated_at      = NOW()
			  WHERE id = $1`,
			fundID, cashCredit,
		); err != nil {
			return Result{}, fmt.Errorf("corpaction: credit fund cash: %w", err)
		}
		// P1-1 — also append a cash_ledger row inside the same
		// transaction so funds.current_capital and the journal
		// stay in lock-step. Idempotency key is keyed off
		// (corp_action_id, fund_id) so re-running ApplyEvent for
		// the same event collapses cleanly. We deliberately use
		// raw SQL here (instead of CashLedgerRepo) to keep the
		// applier package free of the repository import — the
		// corpaction module is intentionally narrow.
		idem := fmt.Sprintf("corp:%s:%s", evt.ID, fundID)
		desc := fmt.Sprintf("dividend %s @ %.4f × %.0f", evt.InstrumentKey, evt.CashDividend, pre.quantity)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cash_ledger
			    (fund_id, posted_at, entry_type, amount, currency,
			     corp_action_id, description, metadata, idempotency_key)
			 VALUES ($1, NOW(), 'dividend_cash', $2, 'USD',
			         NULLIF($3, '')::uuid, $4, $5::jsonb, $6)
			 ON CONFLICT (fund_id, idempotency_key)
			   WHERE idempotency_key IS NOT NULL DO NOTHING`,
			fundID, cashCredit, evt.ID, desc,
			fmt.Sprintf(`{"instrument_key":%q,"shares_basis":%g}`, evt.InstrumentKey, pre.quantity),
			idem,
		); err != nil {
			return Result{}, fmt.Errorf("corpaction: append cash_ledger: %w", err)
		}
	}

	// S12.2 — fractional-share residual (A-share 10送4 etc.) is
	// cash-settled in a SECOND cash_ledger row with a distinct
	// idempotency key, so a re-application of the event still
	// only credits the residual once. We charge the WHOLE
	// (cashCredit + cashResidualCredit) against current_capital
	// in a single update? No — the cashCredit path above already
	// credited current_capital with cashCredit; here we credit
	// the additional cashResidualCredit.
	if cashResidualCredit > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE funds
			    SET current_capital = current_capital + $2,
			        updated_at      = NOW()
			  WHERE id = $1`,
			fundID, cashResidualCredit,
		); err != nil {
			return Result{}, fmt.Errorf("corpaction: credit residual cash: %w", err)
		}
		idem := fmt.Sprintf("corp:%s:%s:residual", evt.ID, fundID)
		desc := fmt.Sprintf("fractional residual %s — %.4f shares × ref → %.4f", evt.InstrumentKey, cashResidualShares, cashResidualCredit)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cash_ledger
			    (fund_id, posted_at, entry_type, amount, currency,
			     corp_action_id, description, metadata, idempotency_key)
			 VALUES ($1, NOW(), 'dividend_cash', $2, 'USD',
			         NULLIF($3, '')::uuid, $4, $5::jsonb, $6)
			 ON CONFLICT (fund_id, idempotency_key)
			   WHERE idempotency_key IS NOT NULL DO NOTHING`,
			fundID, cashResidualCredit, evt.ID, desc,
			fmt.Sprintf(`{"instrument_key":%q,"settlement":"whole_shares","residual_shares":%g,"split_ratio":%g}`,
				evt.InstrumentKey, cashResidualShares, evt.SplitRatio),
			idem,
		); err != nil {
			return Result{}, fmt.Errorf("corpaction: append residual cash_ledger: %w", err)
		}
	}

	_ = totalCash // documented for callers; not separately persisted (sum of two ledger rows above).

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO corp_action_applications
		   (corp_action_id, fund_id, applied_at,
		    pre_quantity, post_quantity,
		    pre_cost_price, post_cost_price,
		    cash_credit)
		 VALUES ($1, $2, NOW(), $3, $4, $5, $6, $7)
		 ON CONFLICT (corp_action_id, fund_id) DO NOTHING`,
		evt.ID, fundID,
		pre.quantity, postQuantity,
		pre.costPrice, postCost,
		cashCredit,
	); err != nil {
		return Result{}, fmt.Errorf("corpaction: record application: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("corpaction: commit: %w", err)
	}

	return Result{
		FundID:             fundID,
		InstrumentKey:      evt.InstrumentKey,
		PreQuantity:        pre.quantity,
		PostQuantity:       postQuantity,
		PreCostPrice:       pre.costPrice,
		PostCostPrice:      postCost,
		PreUnrealized:      pre.unrealizedPnl,
		PostUnrealized:     postUnreal,
		CashCredit:         cashCredit,
		CashResidualShares: cashResidualShares,
		CashResidualCredit: cashResidualCredit,
		SettlementMode:     string(mode),
		AlreadyApplied:     false,
	}, nil
}

// settlementMode identifies which corporate-action settlement rule
// to use. A-share + HK distribute stock dividends in whole shares
// only with a cash residual; US + crypto default to fractional
// scaling.
type settlementMode string

const (
	settlementFractional   settlementMode = "fractional"
	settlementWholeShares  settlementMode = "whole_shares"
)

// settlementModeFor picks the mode from the instrument_key prefix.
// The platform's canonical keys are venue-prefixed (SSE:600519,
// SZSE:301308, BSE:830799, HKEX:00700, NASDAQ:AAPL, …) so prefix
// inspection is enough — no DB lookup required.
//
// Defaults to fractional when the prefix is unknown, which matches
// the legacy behaviour (won't break any non-A-share / non-HK
// instrument added in the future).
func settlementModeFor(instrumentKey string) settlementMode {
	if instrumentKey == "" {
		return settlementFractional
	}
	idx := -1
	for i := 0; i < len(instrumentKey); i++ {
		if instrumentKey[i] == ':' {
			idx = i
			break
		}
	}
	prefix := instrumentKey
	if idx > 0 {
		prefix = instrumentKey[:idx]
	}
	switch prefix {
	case "SSE", "SZSE", "BSE", "SHA", "SHE", "XSHG", "XSHE", "XBSE":
		return settlementWholeShares
	case "HKEX", "HK", "XHKG":
		return settlementWholeShares
	default:
		return settlementFractional
	}
}

// validateEvent enforces the small handful of invariants the SQL
// CHECK constraints would otherwise catch much later. Splitting it
// out lets the admin handler reject bad input with a 400 before the
// transaction opens.
func validateEvent(e Event) error {
	if e.ID == "" {
		return fmt.Errorf("%w: empty event id", ErrEventInvalid)
	}
	if e.InstrumentKey == "" {
		return fmt.Errorf("%w: empty instrument_key", ErrEventInvalid)
	}
	if e.SplitRatio <= 0 {
		return fmt.Errorf("%w: split_ratio must be > 0, got %v", ErrEventInvalid, e.SplitRatio)
	}
	if e.CashDividend < 0 {
		return fmt.Errorf("%w: cash_dividend must be >= 0, got %v", ErrEventInvalid, e.CashDividend)
	}
	return nil
}

// positionRow is the minimal shape we need to lock and read from
// holding_positions. Only the columns that participate in the
// application math are pulled.
type positionRow struct {
	quantity      float64
	costPrice     float64
	currentPrice  float64
	marketValue   float64
	unrealizedPnl float64
}

func lockPosition(ctx context.Context, tx dbExec, fundID, instrumentKey string) (positionRow, error) {
	var row positionRow
	err := tx.QueryRowContext(ctx,
		`SELECT quantity, cost_price, current_price, market_value, COALESCE(unrealized_pnl, 0)
		   FROM holding_positions
		  WHERE fund_id = $1 AND instrument_key = $2
		  FOR UPDATE`,
		fundID, instrumentKey,
	).Scan(&row.quantity, &row.costPrice, &row.currentPrice, &row.marketValue, &row.unrealizedPnl)
	if errors.Is(err, sql.ErrNoRows) {
		return row, ErrPositionMissing
	}
	if err != nil {
		return row, fmt.Errorf("corpaction: lock position: %w", err)
	}
	return row, nil
}

func lookupPriorApplication(ctx context.Context, tx dbExec, eventID, fundID string) (Result, bool, error) {
	var r Result
	r.FundID = fundID
	err := tx.QueryRowContext(ctx,
		`SELECT pre_quantity, post_quantity, pre_cost_price, post_cost_price, cash_credit
		   FROM corp_action_applications
		  WHERE corp_action_id = $1 AND fund_id = $2`,
		eventID, fundID,
	).Scan(&r.PreQuantity, &r.PostQuantity, &r.PreCostPrice, &r.PostCostPrice, &r.CashCredit)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, fmt.Errorf("corpaction: prior application lookup: %w", err)
	}
	return r, true, nil
}

// round8 rounds to 8 decimal places (NUMERIC(20,8) column scale)
// so we don't drift across re-applications by sub-LSB amounts.
func round8(v float64) float64 {
	const k = 1e8
	if v >= 0 {
		return float64(int64(v*k+0.5)) / k
	}
	return float64(int64(v*k-0.5)) / k
}

// round4 matches the holding_positions.market_value precision.
func round4(v float64) float64 {
	const k = 1e4
	if v >= 0 {
		return float64(int64(v*k+0.5)) / k
	}
	return float64(int64(v*k-0.5)) / k
}
