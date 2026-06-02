// Order replay / restart recovery wiring (P1-5).
//
// Glue between repository.TradeExecution rows persisted in the DB
// and the broker.Simulator's in-memory book. Called once at process
// start, after the simulator and trade repo are constructed but
// before any HTTP / scheduler goroutine is allowed to accept new
// orders.
//
// Why a separate file
//
//   - The broker package must NOT import repository. Restoring open
//     orders is fundamentally a wiring concern: it sits at the seam
//     between the persistence layer and the in-memory venue. Putting
//     the projection here keeps the simulator package free of
//     persistence types.
//
//   - The boot-time call site is also the place to decide WHAT to
//     replay. Today we replay every non-terminal trade across every
//     fund. When a real broker adapter (long-port, IBKR) is wired,
//     the same shape will instead reconcile against the broker's
//     own order list and only replay the diff.

package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fundai/server/internal/broker"
	"github.com/fundai/server/internal/repository"
)

// replayOpenOrders queries every non-terminal trade across all funds
// and seeds them into the simulator. Returns the broker-level
// RestoreReport so callers can log counts / surface them via /healthz
// in a follow-up.
//
// Errors at the repo level are returned as-is: a failed query at
// boot is fatal — we'd rather refuse to start than start with an
// inconsistent in-memory book. Per-row projection failures are
// recorded in the report and logged, but do not abort the boot;
// losing one ill-formed legacy row is preferable to refusing to
// serve every other fund.
func replayOpenOrders(ctx context.Context, sim *broker.Simulator, repo *repository.TradeRepo, log *slog.Logger) (broker.RestoreReport, error) {
	if sim == nil || repo == nil {
		return broker.RestoreReport{}, nil
	}
	if log == nil {
		log = slog.Default()
	}

	rows, err := repo.ListOpenAcrossFunds(ctx, 0)
	if err != nil {
		return broker.RestoreReport{}, fmt.Errorf("replayOpenOrders: list open: %w", err)
	}
	if len(rows) == 0 {
		log.Info("order replay: no open orders to restore")
		return broker.RestoreReport{}, nil
	}

	orders := make([]broker.Order, 0, len(rows))
	skipped := 0
	for i := range rows {
		o, ok := tradeRowToBrokerOrder(&rows[i])
		if !ok {
			skipped++
			log.Warn("order replay: skipping row that cannot be projected",
				"trade_id", rows[i].ID, "fund_id", rows[i].FundID, "status", rows[i].Status,
				"order_type", rows[i].OrderType)
			continue
		}
		orders = append(orders, o)
	}

	report := sim.RestoreOpenOrders(orders)
	log.Info("order replay: complete",
		"restored", report.Restored,
		"skipped_in_db", skipped,
		"skipped_in_sim", report.Skipped,
		"errors", len(report.Errors))
	for k, v := range report.Errors {
		log.Warn("order replay: row error", "broker_order_id", k, "err", v.Error())
	}
	return report, nil
}

// tradeRowToBrokerOrder projects a persisted trade row onto a
// broker.Order suitable for RestoreOpenOrders. Returns ok=false when
// the row is missing fields that the simulator's read paths require
// (broker_order_id, fund_id, side, order_type, quantity).
//
// The mapping is intentionally lossy in one direction only: we
// don't reconstruct fees, fills, or slippage because Restore should
// not re-emit those. We DO reconstruct CurrentStopPrice from the
// stored stop_price column so the trigger engine has a sane starting
// point; trailing high/low water marks are rebuilt by the engine on
// the next tick (we don't persist them today — see the "future
// work" note below).
//
// Future work: persist TrailingHighWater / TrailingLowWater so a
// long-running trailing stop ratcheted to a favourable level
// survives a crash without falling back to "now". For now we accept
// a one-tick re-anchor on restart; the comment in
// simulator_restore.go captures the trade-off.
func tradeRowToBrokerOrder(t *repository.TradeExecution) (broker.Order, bool) {
	if t == nil {
		return broker.Order{}, false
	}
	brokerID := strings.TrimSpace(t.BrokerOrderID.String)
	if !t.BrokerOrderID.Valid || brokerID == "" {
		// The cancel/replace + stop-trigger paths key off
		// broker_order_id. A row without one was never booked
		// against the simulator (e.g. a synthetic settlement
		// trade); skip it cleanly.
		return broker.Order{}, false
	}
	if strings.TrimSpace(t.FundID) == "" || strings.TrimSpace(t.Side) == "" || strings.TrimSpace(t.OrderType) == "" {
		return broker.Order{}, false
	}
	if t.Quantity <= 0 {
		return broker.Order{}, false
	}

	state := mapTradeStatusToOrderState(t.Status)
	if state == "" || broker.OrderState(state).IsTerminal() {
		return broker.Order{}, false
	}

	clientID := ""
	if t.ClientIdempotencyKey.Valid {
		clientID = t.ClientIdempotencyKey.String
	}

	req := broker.PlaceOrderRequest{
		ClientOrderID: clientID,
		FundID:        t.FundID,
		InstrumentKey: t.InstrumentKey,
		Symbol:        t.Symbol,
		Market:        nullStringOr(t.Market, ""),
		AssetClass:    nullStringOr(t.AssetClass, ""),
		Side:          broker.Side(t.Side),
		OrderType:     broker.OrderType(t.OrderType),
		Quantity:      t.Quantity,
		LimitPrice:    nullFloat64Or(t.Price, 0),
		StopPrice:     nullFloat64Or(t.StopPrice, 0),
		TrailAmount:   nullFloat64Or(t.TrailAmount, 0),
		TrailPercent:  nullFloat64Or(t.TrailPercent, 0),
		DisplayQty:    nullFloat64Or(t.DisplayQty, 0),
		TimeInForce:   broker.TimeInForce(nullStringOr(t.TimeInForce, "")),
		ReduceOnly:    nullBoolOr(t.ReduceOnly, false),
		PositionSide:  broker.PositionSide(nullStringOr(t.PositionSide, "")),
	}
	if !req.OrderType.IsValid() {
		return broker.Order{}, false
	}
	if req.TimeInForce == "" {
		req.TimeInForce = broker.TIFDay
	}

	order := broker.Order{
		BrokerOrderID:  brokerID,
		ClientOrderID:  clientID,
		Request:        req,
		State:          broker.OrderState(state),
		FilledQuantity: t.FilledQty,
		AvgFillPrice:   nullFloat64Or(t.FilledPrice, 0),
		PlacedAt:       t.CreatedAt,
		UpdatedAt:      t.CreatedAt,
		Fees: broker.Fees{
			Commission:  t.FeeCommission,
			StampTax:    t.FeeStampTax,
			TransferFee: t.FeeTransfer,
		},
	}
	if order.PlacedAt.IsZero() {
		order.PlacedAt = time.Now()
	}
	if req.OrderType.IsStopType() {
		order.CurrentStopPrice = nullFloat64Or(t.StopPrice, 0)
	}
	return order, true
}

// mapTradeStatusToOrderState bridges the trade_executions.status
// vocabulary ('pending','working','triggered','partial','filled',
// ...) onto the broker.OrderState enum. Returns "" for an unknown
// status — the caller skips those rows so we don't accidentally
// surface them as open.
func mapTradeStatusToOrderState(status string) broker.OrderState {
	switch strings.TrimSpace(strings.ToLower(status)) {
	case "pending", "submitted":
		return broker.OrderStatePending
	case "working":
		return broker.OrderStateWorking
	case "triggered":
		return broker.OrderStateTriggered
	case "partial", "partial_filled", "partially_filled":
		return broker.OrderStatePartial
	case "filled":
		return broker.OrderStateFilled
	case "cancelled":
		return broker.OrderStateCancelled
	case "rejected", "failed":
		return broker.OrderStateRejected
	case "expired":
		return broker.OrderStateExpired
	}
	return ""
}

// nullStringOr returns the string value when Valid, otherwise the
// fallback. Used by the trade-row → broker.Order projection where
// many columns are nullable for legacy rows.
func nullStringOr(v sql.NullString, fallback string) string {
	if v.Valid {
		return v.String
	}
	return fallback
}

// nullFloat64Or returns the float64 value when Valid, otherwise the
// fallback. Pairs with nullStringOr for the trade-row projection.
func nullFloat64Or(v sql.NullFloat64, fallback float64) float64 {
	if v.Valid {
		return v.Float64
	}
	return fallback
}

// nullBoolOr returns the bool value when Valid, otherwise the
// fallback.
func nullBoolOr(v sql.NullBool, fallback bool) bool {
	if v.Valid {
		return v.Bool
	}
	return fallback
}
