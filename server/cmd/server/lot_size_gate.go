// lot_size_gate.go — production wiring of broker.LotSizeGate (S12.1).
//
// Translates a broker probe into a lotsizegate.Engine call backed by:
//   * internal/instrument for A-share board rules (deterministic);
//   * a DB-backed PositionSource for the fund's current holding
//     (used by the A-share odd-lot residual rule on partial sells);
//   * placeholder HK / crypto resolvers that S12.3 fills in from the
//     instrument_metadata table.
//
// Errors fail-OPEN with a metric tag so a transient DB outage
// doesn't halt trading — the upstream NormalizeBuyQty /
// NormalizeSellQty in wiring_adapters.go are the primary defence;
// this gate is the broker-side terminal safety net.
//
// Trigger story: 2026-06-03 audit revealed:
//   * 301308 buy 1 share (ChiNext minimum 100) — slipped past the
//     PM normaliser because the fallback path stamped the budget as
//     limit price and used quantity=1;
//   * 688195 sell 85 / 688205 sell 62 (STAR step=1, but residual
//     odd-lot rule wasn't enforced because no broker gate existed);
//   * 0.6-share residual holding on 688195 from a corp-action
//     applier bug (S12.2) — broker would have happily accepted a
//     sell of that 0.6 share if the upstream had let it through.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fundai/server/internal/broker"
	"github.com/fundai/server/internal/lotsizegate"
	"github.com/fundai/server/internal/repository"
)

// lotSizeGate implements broker.LotSizeGate.
type lotSizeGate struct {
	engine  *lotsizegate.Engine
	metrics *serverMetrics
	logger  leveledLogger
}

// newLotSizeGate constructs the production wiring. db is required
// (used by the position source for sell residual checks). Returning
// nil when db is nil keeps the simulator's optional-gate contract:
// callers can pass the result straight into broker.WithLotSizeGate
// without an extra nil-check.
//
// hkLots / cryptoSteps / tickRes / overrides are optional; the
// engine ships with safe defaults (HK lot=100, crypto step=1e-6,
// no tick check, no overrides) when they are nil. S12.3/.5/.6
// plug in DB-backed implementations against instrument_metadata.
func newLotSizeGate(db *sql.DB, metrics *serverMetrics, logger leveledLogger,
	hkLots lotsizegate.HKLotResolver, cryptoSteps lotsizegate.CryptoStepResolver,
	tickRes lotsizegate.TickResolver, overrides lotsizegate.OverridesResolver) *lotSizeGate {
	if db == nil {
		return nil
	}
	specs := &lotsizegate.DefaultSpecSource{
		HK:        hkLots,
		Crypto:    cryptoSteps,
		Tick:      tickRes,
		Overrides: overrides,
	}
	positions := &dbHoldingQty{db: db}
	engine := lotsizegate.NewEngine(specs, positions)
	return &lotSizeGate{
		engine:  engine,
		metrics: metrics,
		logger:  logger,
	}
}

// CheckOrder satisfies broker.LotSizeGate.
func (g *lotSizeGate) CheckOrder(ctx context.Context, probe broker.LotSizeProbe) broker.LotSizeVerdict {
	if g == nil || g.engine == nil {
		return broker.LotSizeVerdict{}
	}

	v := g.engine.Check(ctx, lotsizegate.Probe{
		FundID:         probe.FundID,
		InstrumentKey:  probe.InstrumentKey,
		Symbol:         probe.Symbol,
		Market:         probe.Market,
		Exchange:       probe.Exchange,
		AssetClass:     probe.AssetClass,
		InstrumentType: probe.InstrumentType,
		Side:           probe.Side,
		Quantity:       probe.Quantity,
		LimitPrice:     probe.LimitPrice,
		ClientOrderID:  probe.ClientOrderID,
	})

	verdict := broker.LotSizeVerdict{
		Rejected:     v.Rejected,
		RejectReason: v.RejectReason,
		Warnings:     v.Warnings,
		SuggestedQty: v.SuggestedQty,
	}

	switch {
	case v.Rejected && v.AssetClass != "":
		g.recordEvent("reject_" + string(v.AssetClass))
	case v.Rejected:
		g.recordEvent("reject_unknown")
	case len(v.Warnings) > 0 && v.AssetClass == lotsizegate.AssetUnknown:
		// Warning-with-unknown-class is the fail-open signal.
		g.recordEvent("evaluate_failed")
	default:
		g.recordEvent("allow")
	}

	if v.Rejected && g.logger != nil {
		g.logger.Info("lot-size gate reject",
			"fund_id", probe.FundID,
			"symbol", probe.Symbol,
			"side", probe.Side,
			"qty", probe.Quantity,
			"reason", v.RejectReason,
			"suggested_qty", v.SuggestedQty,
		)
	}

	return verdict
}

func (g *lotSizeGate) recordEvent(name string) {
	if g == nil || g.metrics == nil {
		return
	}
	g.metrics.RecordLotSizeEvent(name)
}

// dbHoldingQty implements lotsizegate.PositionSource against
// holding_positions. A missing row → (0, nil), which the engine
// treats as "no position" — the safe default that rejects any sell.
//
// S12.5 — returns LEAST(quantity, COALESCE(available_qty, quantity))
// so the lot-size gate's residual / oversell checks see the post-
// T+1-lock figure. The settlement bookkeeper (PM-side
// SellableQtyToday + execution-time SettlementCycleRule) is the
// primary T+1 enforcer; this gate is the broker-side terminal
// safety net that ensures a sell order whose quantity exceeds
// available_qty is rejected even when the upstream layers missed
// it. Funds that haven't migrated to per-instrument available_qty
// tracking still see legacy behaviour because COALESCE folds the
// NULL down to quantity.
type dbHoldingQty struct {
	db *sql.DB
}

func (s *dbHoldingQty) HoldingQty(ctx context.Context, fundID, instrumentKey string) (float64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	if fundID == "" || instrumentKey == "" {
		return 0, nil
	}
	var qty sql.NullFloat64
	err := s.db.QueryRowContext(ctx,
		`SELECT LEAST(quantity, COALESCE(available_qty, quantity))
		   FROM holding_positions
		  WHERE fund_id = $1 AND instrument_key = $2`,
		fundID, instrumentKey,
	).Scan(&qty)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !qty.Valid {
		return 0, nil
	}
	return qty.Float64, nil
}

// dbHKLotResolver implements lotsizegate.HKLotResolver against
// instrument_metadata (S12.3). A missing row → (0, false), which
// the engine then translates to the safe default lot=100.
//
// The HKEX prefix the lot-size gate sees on a probe is the
// canonical "HKEX:NNNNN" instrument key; we accept both that form
// and a bare "NNNNN" symbol so the resolver is tolerant of probes
// that haven't been normalised by the upstream wiring.
type dbHKLotResolver struct {
	repo *repository.InstrumentMetadataRepo
}

func newHKLotResolver(repo *repository.InstrumentMetadataRepo) *dbHKLotResolver {
	if repo == nil {
		return nil
	}
	return &dbHKLotResolver{repo: repo}
}

func (r *dbHKLotResolver) LotFor(ctx context.Context, symbol string) (int, bool) {
	if r == nil || r.repo == nil || symbol == "" {
		return 0, false
	}
	// Try canonical key first, then a bare-symbol fallback.
	for _, key := range []string{symbol, fmt.Sprintf("HKEX:%s", symbol)} {
		row, err := r.repo.Get(ctx, key)
		if err != nil || row == nil {
			continue
		}
		if row.BoardLot > 0 {
			return int(row.BoardLot), true
		}
	}
	return 0, false
}

// dbCryptoStepResolver implements lotsizegate.CryptoStepResolver
// against instrument_metadata. Returns step + min_notional for the
// pair, or (0, 0, false) when missing so the engine defaults to
// step=1e-6 / no min-notional.
//
// The symbol the lot-size gate sees can be either the canonical
// venue-prefixed key (BINANCE:BTC-USDT) or a bare pair (BTC-USDT);
// we try both for resilience. When multiple venues carry the same
// pair the first hit wins — the admin UI documents that operators
// should keep one canonical row per pair.
type dbCryptoStepResolver struct {
	repo    *repository.InstrumentMetadataRepo
	prefers []string // venue prefixes to try first (in order)
}

func newCryptoStepResolver(repo *repository.InstrumentMetadataRepo, preferredVenues ...string) *dbCryptoStepResolver {
	if repo == nil {
		return nil
	}
	if len(preferredVenues) == 0 {
		preferredVenues = []string{"BINANCE", "COINBASE", "OKEX"}
	}
	return &dbCryptoStepResolver{repo: repo, prefers: preferredVenues}
}

// dbOverridesResolver implements lotsizegate.OverridesResolver
// against instrument_metadata (S12.6). Admin upserts to that table
// flip SupportsFractional / MinNotional / ContractMultiplier on
// individual instruments without code changes — the lot-size gate
// reads the override on the next order.
type dbOverridesResolver struct {
	repo *repository.InstrumentMetadataRepo
}

func newOverridesResolver(repo *repository.InstrumentMetadataRepo) *dbOverridesResolver {
	if repo == nil {
		return nil
	}
	return &dbOverridesResolver{repo: repo}
}

func (r *dbOverridesResolver) OverridesFor(ctx context.Context, instrumentKey, symbol string) (lotsizegate.Overrides, bool) {
	if r == nil || r.repo == nil {
		return lotsizegate.Overrides{}, false
	}
	candidates := []string{}
	if instrumentKey != "" {
		candidates = append(candidates, instrumentKey)
	}
	if symbol != "" && symbol != instrumentKey {
		candidates = append(candidates, symbol)
	}
	for _, key := range candidates {
		row, err := r.repo.Get(ctx, key)
		if err != nil || row == nil {
			continue
		}
		return lotsizegate.Overrides{
			SupportsFractional: row.SupportsFractional,
			MinNotional:        row.MinNotional,
			ContractMultiplier: row.ContractMultiplier,
		}, true
	}
	return lotsizegate.Overrides{}, false
}

// dbTickResolver implements lotsizegate.TickResolver against
// instrument_metadata (S12.5). Parses the tick_rules JSONB into the
// engine's []TickRule shape; a malformed JSON blob falls back to
// the scalar tick_size with a logged warning.
type dbTickResolver struct {
	repo   *repository.InstrumentMetadataRepo
	logger leveledLogger
}

func newTickResolver(repo *repository.InstrumentMetadataRepo, logger leveledLogger) *dbTickResolver {
	if repo == nil {
		return nil
	}
	return &dbTickResolver{repo: repo, logger: logger}
}

// rawTickRule mirrors the JSON shape stored in tick_rules. Kept
// local to the wiring file so the lotsizegate package doesn't pick
// up an encoding/json dependency.
type rawTickRule struct {
	MaxPrice float64 `json:"max_price"`
	Tick     float64 `json:"tick"`
}

func (r *dbTickResolver) TickFor(ctx context.Context, instrumentKey, symbol string) (float64, []lotsizegate.TickRule, bool) {
	if r == nil || r.repo == nil {
		return 0, nil, false
	}
	candidates := []string{}
	if instrumentKey != "" {
		candidates = append(candidates, instrumentKey)
	}
	if symbol != "" && symbol != instrumentKey {
		// Bare-symbol fallback (useful for tests / legacy probes).
		candidates = append(candidates, symbol)
	}
	for _, key := range candidates {
		row, err := r.repo.Get(ctx, key)
		if err != nil || row == nil {
			continue
		}
		var rules []lotsizegate.TickRule
		if len(row.TickRulesJSON) > 0 && string(row.TickRulesJSON) != "[]" {
			var raw []rawTickRule
			if jerr := json.Unmarshal(row.TickRulesJSON, &raw); jerr != nil {
				if r.logger != nil {
					r.logger.Warn("lot-size: malformed tick_rules JSON; falling back to scalar tick_size",
						"instrument_key", row.InstrumentKey,
						"err", jerr,
					)
				}
			} else {
				rules = make([]lotsizegate.TickRule, 0, len(raw))
				for _, x := range raw {
					if x.Tick > 0 && x.MaxPrice > 0 {
						rules = append(rules, lotsizegate.TickRule{
							MaxPrice: x.MaxPrice,
							Tick:     x.Tick,
						})
					}
				}
			}
		}
		if row.TickSize > 0 || len(rules) > 0 {
			return row.TickSize, rules, true
		}
	}
	return 0, nil, false
}

func (r *dbCryptoStepResolver) StepFor(ctx context.Context, symbol string) (step, minNotional float64, ok bool) {
	if r == nil || r.repo == nil || symbol == "" {
		return 0, 0, false
	}
	// Symbol as-is first (already canonical), then prefixed forms.
	candidates := make([]string, 0, len(r.prefers)+1)
	candidates = append(candidates, symbol)
	for _, venue := range r.prefers {
		candidates = append(candidates, fmt.Sprintf("%s:%s", venue, symbol))
	}
	for _, key := range candidates {
		row, err := r.repo.Get(ctx, key)
		if err != nil || row == nil {
			continue
		}
		if row.StepSize > 0 {
			return row.StepSize, row.MinNotional, true
		}
	}
	return 0, 0, false
}
