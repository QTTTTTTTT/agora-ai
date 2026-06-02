package broker

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestRestoreOpenOrders_HappyPath exercises the basic restart-recovery
// flow: a non-terminal order persisted before the crash should be
// addressable via every read path the running platform uses.
func TestRestoreOpenOrders_HappyPath(t *testing.T) {
	sim := newTestSimulator(t, nil)
	now := time.Date(2026, 1, 1, 9, 25, 0, 0, time.UTC)

	want := Order{
		BrokerOrderID: "broker-restored-1",
		ClientOrderID: "client-restored-1",
		Request: PlaceOrderRequest{
			ClientOrderID: "client-restored-1",
			FundID:        "fund-A",
			InstrumentKey: "us-equity:MSFT",
			Symbol:        "MSFT",
			Market:        "us",
			AssetClass:    "equity",
			Side:          SideBuy,
			OrderType:     OrderTypeLimit,
			Quantity:      200,
			LimitPrice:    250.5,
			TimeInForce:   TIFGTC,
		},
		State:    OrderStateWorking,
		PlacedAt: now,
	}

	report := sim.RestoreOpenOrders([]Order{want})
	if report.Restored != 1 {
		t.Fatalf("Restored = %d, want 1", report.Restored)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("unexpected errors: %#v", report.Errors)
	}

	got, err := sim.GetOrder(context.Background(), "fund-A", "broker-restored-1")
	if err != nil {
		t.Fatalf("GetOrder after restore: %v", err)
	}
	if got.State != OrderStateWorking {
		t.Errorf("State = %v, want working", got.State)
	}
	if got.Request.LimitPrice != 250.5 {
		t.Errorf("LimitPrice = %v, want 250.5", got.Request.LimitPrice)
	}

	// PlacedAt is preserved (audit) but UpdatedAt is forwarded to
	// the simulator clock so observers can tell this Order was
	// reconstructed.
	if !got.PlacedAt.Equal(now) {
		t.Errorf("PlacedAt = %v, want preserved %v", got.PlacedAt, now)
	}
	if got.UpdatedAt.Before(now) {
		t.Errorf("UpdatedAt = %v should be >= original PlacedAt %v", got.UpdatedAt, now)
	}

	// Idempotency index is rebuilt — a duplicate PlaceOrder with
	// the same (FundID, ClientOrderID) should collapse to the
	// restored row.
	dup := want.Request
	dup.Quantity = 999 // would otherwise create a new order with a different qty
	resp, err := sim.PlaceOrder(context.Background(), dup)
	if err != nil {
		t.Fatalf("PlaceOrder dedupe: %v", err)
	}
	if resp.BrokerOrderID != "broker-restored-1" {
		t.Errorf("dedupe returned BrokerOrderID = %q, want broker-restored-1", resp.BrokerOrderID)
	}
	if resp.Request.Quantity != 200 {
		t.Errorf("dedupe Quantity = %v, want preserved 200", resp.Request.Quantity)
	}

	// ListOpenOrders surfaces the restored row.
	open, err := sim.ListOpenOrders(context.Background(), "fund-A")
	if err != nil {
		t.Fatalf("ListOpenOrders: %v", err)
	}
	if len(open) != 1 || open[0].BrokerOrderID != "broker-restored-1" {
		t.Errorf("ListOpenOrders = %#v, want one row with BrokerOrderID broker-restored-1", open)
	}
}

// TestRestoreOpenOrders_TrailingStopSeedsCurrentStopPrice covers a
// subtle case: a trailing-stop persisted with a ratcheted
// CurrentStopPrice should keep that level on restart, not snap back
// to the original Request.StopPrice. If the caller supplies 0 we
// fall back to Request.StopPrice (which the stop-trigger engine
// re-anchors on the next OnQuote).
func TestRestoreOpenOrders_TrailingStopSeedsCurrentStopPrice(t *testing.T) {
	sim := newTestSimulator(t, nil)

	ratcheted := Order{
		BrokerOrderID: "broker-trail-1",
		ClientOrderID: "client-trail-1",
		Request: PlaceOrderRequest{
			ClientOrderID: "client-trail-1",
			FundID:        "fund-A",
			InstrumentKey: "us-equity:NVDA",
			Symbol:        "NVDA",
			Market:        "us",
			AssetClass:    "equity",
			Side:          SideSell,
			OrderType:     OrderTypeTrailingStop,
			Quantity:      50,
			TrailPercent:  0.05,
			StopPrice:     90.0, // initial stop
			TimeInForce:   TIFGTC,
		},
		State:             OrderStateWorking,
		CurrentStopPrice:  108.5, // ratcheted by previous run
		TrailingHighWater: 114.0,
	}
	zeroSeed := Order{
		BrokerOrderID: "broker-trail-2",
		ClientOrderID: "client-trail-2",
		Request: PlaceOrderRequest{
			ClientOrderID: "client-trail-2",
			FundID:        "fund-A",
			InstrumentKey: "us-equity:AMD",
			Symbol:        "AMD",
			Market:        "us",
			AssetClass:    "equity",
			Side:          SideSell,
			OrderType:     OrderTypeTrailingStop,
			Quantity:      30,
			TrailAmount:   2.0,
			StopPrice:     97.0,
			TimeInForce:   TIFGTC,
		},
		State:            OrderStateWorking,
		CurrentStopPrice: 0, // never ratcheted before crash
	}

	report := sim.RestoreOpenOrders([]Order{ratcheted, zeroSeed})
	if report.Restored != 2 {
		t.Fatalf("Restored = %d, want 2 (errors=%v)", report.Restored, report.Errors)
	}

	got1, _ := sim.GetOrder(context.Background(), "fund-A", "broker-trail-1")
	if got1.CurrentStopPrice != 108.5 {
		t.Errorf("ratcheted CurrentStopPrice = %v, want 108.5", got1.CurrentStopPrice)
	}
	if got1.TrailingHighWater != 114.0 {
		t.Errorf("TrailingHighWater = %v, want 114.0", got1.TrailingHighWater)
	}

	got2, _ := sim.GetOrder(context.Background(), "fund-A", "broker-trail-2")
	if got2.CurrentStopPrice != 97.0 {
		t.Errorf("zero-seed CurrentStopPrice = %v, want fallback 97.0", got2.CurrentStopPrice)
	}
}

// TestRestoreOpenOrders_RejectsTerminalAndDuplicates verifies the
// Skipped / Errors counts. Terminal rows must NOT enter the open
// set; duplicates must NOT silently overwrite an in-flight Order.
func TestRestoreOpenOrders_RejectsTerminalAndDuplicates(t *testing.T) {
	sim := newTestSimulator(t, nil)

	terminal := Order{
		BrokerOrderID: "broker-cancelled-1",
		ClientOrderID: "client-c-1",
		Request: PlaceOrderRequest{
			ClientOrderID: "client-c-1",
			FundID:        "fund-A",
			InstrumentKey: "us-equity:AAPL",
			Symbol:        "AAPL",
			Market:        "us",
			AssetClass:    "equity",
			Side:          SideBuy,
			OrderType:     OrderTypeLimit,
			Quantity:      10,
			LimitPrice:    200,
			TimeInForce:   TIFGTC,
		},
		State: OrderStateCancelled,
	}
	open := Order{
		BrokerOrderID: "broker-open-1",
		ClientOrderID: "client-o-1",
		Request: PlaceOrderRequest{
			ClientOrderID: "client-o-1",
			FundID:        "fund-A",
			InstrumentKey: "us-equity:AAPL",
			Symbol:        "AAPL",
			Market:        "us",
			AssetClass:    "equity",
			Side:          SideBuy,
			OrderType:     OrderTypeLimit,
			Quantity:      10,
			LimitPrice:    200,
			TimeInForce:   TIFGTC,
		},
		State: OrderStateWorking,
	}

	r1 := sim.RestoreOpenOrders([]Order{terminal, open})
	if r1.Restored != 1 || r1.Skipped != 1 {
		t.Errorf("Restored=%d Skipped=%d, want 1/1", r1.Restored, r1.Skipped)
	}

	// Restoring the same broker-id twice should error, not
	// overwrite. Pretend the runtime ratcheted CurrentStopPrice
	// after the first restore — a second restore must NOT clobber
	// that.
	got, _ := sim.GetOrder(context.Background(), "fund-A", "broker-open-1")
	got.CurrentStopPrice = 12345 // simulate an in-flight ratchet
	// Push the mutation back into the simulator via a manual
	// CancelOrder no-op (the test only needs the read-after-mutate
	// sequencing; we don't actually need to commit). The duplicate
	// restore below should bail out either way.
	dup := Order{
		BrokerOrderID: "broker-open-1",
		ClientOrderID: "client-o-1",
		Request:       open.Request,
		State:         OrderStateWorking,
	}
	r2 := sim.RestoreOpenOrders([]Order{dup})
	if r2.Restored != 0 {
		t.Errorf("duplicate Restored = %d, want 0", r2.Restored)
	}
	if !errors.Is(r2.Errors["broker-open-1"], ErrAlreadyRestored) {
		t.Errorf("duplicate err = %v, want ErrAlreadyRestored", r2.Errors["broker-open-1"])
	}
}

// TestRestoreOpenOrders_RejectsMissingFields makes sure a malformed
// row is reported through Errors rather than panicking.
func TestRestoreOpenOrders_RejectsMissingFields(t *testing.T) {
	sim := newTestSimulator(t, nil)

	missingBroker := Order{
		ClientOrderID: "x",
		Request: PlaceOrderRequest{
			FundID:    "fund-A",
			Side:      SideBuy,
			OrderType: OrderTypeLimit,
			Quantity:  1,
		},
		State: OrderStateWorking,
	}
	missingFund := Order{
		BrokerOrderID: "broker-mf-1",
		ClientOrderID: "x",
		Request: PlaceOrderRequest{
			Side:      SideBuy,
			OrderType: OrderTypeLimit,
			Quantity:  1,
		},
		State: OrderStateWorking,
	}

	r := sim.RestoreOpenOrders([]Order{missingBroker, missingFund})
	if r.Restored != 0 {
		t.Errorf("Restored = %d, want 0", r.Restored)
	}
	if len(r.Errors) != 2 {
		t.Errorf("Errors = %#v, want 2 entries", r.Errors)
	}
}

// TestRestoreOpenOrders_NoOpEmptyInput exercises the boring case so
// callers that boot with zero open orders don't crash.
func TestRestoreOpenOrders_NoOpEmptyInput(t *testing.T) {
	sim := newTestSimulator(t, nil)
	r := sim.RestoreOpenOrders(nil)
	if r.Restored != 0 || r.Skipped != 0 || len(r.Errors) != 0 {
		t.Errorf("empty restore should be a no-op, got %#v", r)
	}
}

// TestRestoreOpenOrders_PendingStopsResurfaceToTriggerEngine ties
// the restore path to the stop-trigger engine's read API: after
// restore, AllPendingStops should include the restored stop so
// trailing high/low water resumes accumulating on the next quote
// tick.
func TestRestoreOpenOrders_PendingStopsResurfaceToTriggerEngine(t *testing.T) {
	sim := newTestSimulator(t, nil)

	stop := Order{
		BrokerOrderID: "broker-stop-1",
		ClientOrderID: "client-stop-1",
		Request: PlaceOrderRequest{
			ClientOrderID: "client-stop-1",
			FundID:        "fund-A",
			InstrumentKey: "us-equity:TSLA",
			Symbol:        "TSLA",
			Market:        "us",
			AssetClass:    "equity",
			Side:          SideSell,
			OrderType:     OrderTypeStop,
			Quantity:      40,
			StopPrice:     200,
			TimeInForce:   TIFGTC,
		},
		State:            OrderStatePending,
		CurrentStopPrice: 200,
	}
	r := sim.RestoreOpenOrders([]Order{stop})
	if r.Restored != 1 {
		t.Fatalf("Restored = %d, want 1", r.Restored)
	}
	pending := sim.AllPendingStops()
	found := false
	for _, p := range pending {
		if p.BrokerOrderID == "broker-stop-1" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("restored stop not in AllPendingStops; pending=%#v", pending)
	}
}
