// pmpath_gate_chain_test.go — regressions for the four broker-
// side regulatory gates mirrored onto the PM-direct-fill path
// (runtimeTradingEngine.pmPathPreTradeGateChain).
//
// The gate chain re-uses the EXACT same gate implementations
// broker.Simulator was wired with, so the unit-tested behaviour
// of each individual gate (covered by simulator_marketstatus_test.go,
// simulator_lockup_test.go, simulator_borrow_test.go,
// simulator_pricecollar_test.go) is not re-tested here. These tests
// only pin the wiring contract:
//
//   1. all four gates are invoked in the simulator's order
//      (market-status → lockup → borrow → price-collar),
//   2. a reject from ANY gate bubbles a wrapped api.ErrConflict,
//   3. a nil gate is treated as no-op allow (so legacy / smoke
//      builds without WithPMPathGates keep working),
//   4. allowed gates accumulate warnings into the returned slice
//      so the trade row can carry them forward (parity with
//      broker.Simulator's gateWarnings).
//
// Each test uses fake gate impls (deterministic verdicts) so the
// regressions stay close to the chain logic, not the gate
// internals.

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"database/sql"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/broker"
	"github.com/fundai/server/internal/repository"
)

// ---------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------

type fakeMarketStatusGate struct {
	verdict broker.MarketStatusVerdict
	calls   int
}

func (g *fakeMarketStatusGate) CheckOrder(_ context.Context, _ broker.MarketStatusProbe) broker.MarketStatusVerdict {
	g.calls++
	return g.verdict
}

type fakeLockupGate struct {
	verdict broker.LockupVerdict
	calls   int
}

func (g *fakeLockupGate) CheckOrder(_ context.Context, _ broker.LockupProbe) broker.LockupVerdict {
	g.calls++
	return g.verdict
}

type fakeBorrowGate struct {
	verdict broker.BorrowVerdict
	calls   int
}

func (g *fakeBorrowGate) CheckOrder(_ context.Context, _ broker.BorrowProbe) broker.BorrowVerdict {
	g.calls++
	return g.verdict
}

type fakePriceCollarGate struct {
	verdict broker.PriceCollarVerdict
	calls   int
}

func (g *fakePriceCollarGate) CheckOrder(_ context.Context, _ broker.PriceCollarProbe) broker.PriceCollarVerdict {
	g.calls++
	return g.verdict
}

func gateChainAction() repository.PlanAction {
	return repository.PlanAction{
		ID:            "action-1",
		Symbol:        "688205",
		InstrumentKey: "SSE:688205",
		Market:        sql.NullString{String: "a_share", Valid: true},
		Exchange:      sql.NullString{String: "SSE", Valid: true},
		AssetClass:    sql.NullString{String: "equity", Valid: true},
	}
}

func gateChainFund() *repository.Fund {
	return &repository.Fund{ID: "fund-1"}
}

// ---------------------------------------------------------------
// Tests
// ---------------------------------------------------------------

func TestPMPathPreTradeGateChain_AllNilGates_AllowAndNoOp(t *testing.T) {
	// Legacy / smoke wiring: WithPMPathGates never called → all
	// four fields nil. The chain must silently allow so dev /
	// test environments without gate impls keep working,
	// matching broker.Simulator's nil-gate semantics.
	e := &runtimeTradingEngine{metrics: &serverMetrics{}}
	warns, err := e.pmPathPreTradeGateChain(context.Background(), gateChainFund(), gateChainAction(), "buy", 200, 239.35, "k1")
	if err != nil {
		t.Fatalf("nil gates must allow, got: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("nil gates must not produce warnings, got: %v", warns)
	}
}

func TestPMPathPreTradeGateChain_AllAllow_AccumulatesWarnings(t *testing.T) {
	e := &runtimeTradingEngine{
		metrics:          &serverMetrics{},
		marketStatusGate: &fakeMarketStatusGate{verdict: broker.MarketStatusVerdict{Warnings: []string{"stale-quote 45s"}}},
		lockupGate:       &fakeLockupGate{verdict: broker.LockupVerdict{Warnings: []string{"newly-issued lock partial release"}}},
		borrowGate:       &fakeBorrowGate{verdict: broker.BorrowVerdict{Warnings: []string{"locate fee accrued"}}},
		priceCollarGate:  &fakePriceCollarGate{verdict: broker.PriceCollarVerdict{Warnings: []string{"limit within 1% of collar"}}},
	}
	warns, err := e.pmPathPreTradeGateChain(context.Background(), gateChainFund(), gateChainAction(), "buy", 200, 239.35, "k1")
	if err != nil {
		t.Fatalf("all-allow chain must succeed, got: %v", err)
	}
	if len(warns) != 4 {
		t.Fatalf("expected 4 accumulated warnings (one per gate), got %d: %v", len(warns), warns)
	}
}

func TestPMPathPreTradeGateChain_MarketStatusReject_StopsChainEarly(t *testing.T) {
	ms := &fakeMarketStatusGate{verdict: broker.MarketStatusVerdict{Rejected: true, RejectReason: "symbol halted"}}
	lk := &fakeLockupGate{}
	br := &fakeBorrowGate{}
	pc := &fakePriceCollarGate{}
	e := &runtimeTradingEngine{
		metrics:          &serverMetrics{},
		marketStatusGate: ms,
		lockupGate:       lk,
		borrowGate:       br,
		priceCollarGate:  pc,
	}
	_, err := e.pmPathPreTradeGateChain(context.Background(), gateChainFund(), gateChainAction(), "buy", 200, 239.35, "k1")
	if err == nil {
		t.Fatal("expected market-status reject to bubble an error")
	}
	if !errors.Is(err, api.ErrConflict) {
		t.Errorf("error should wrap api.ErrConflict, got: %v", err)
	}
	if !strings.Contains(err.Error(), "halted") {
		t.Errorf("error should include the gate reason, got: %v", err)
	}
	// Later gates must NOT have been called once a gate rejects.
	if ms.calls != 1 {
		t.Errorf("market-status should be called exactly once, got %d", ms.calls)
	}
	if lk.calls != 0 || br.calls != 0 || pc.calls != 0 {
		t.Errorf("downstream gates must not run after a reject; got lockup=%d borrow=%d collar=%d", lk.calls, br.calls, pc.calls)
	}
}

func TestPMPathPreTradeGateChain_LockupRejectsSellOfLockedShares(t *testing.T) {
	lk := &fakeLockupGate{verdict: broker.LockupVerdict{Rejected: true, RejectReason: "shares locked until 2026-06-10"}}
	pc := &fakePriceCollarGate{}
	e := &runtimeTradingEngine{
		metrics:          &serverMetrics{},
		marketStatusGate: &fakeMarketStatusGate{},
		lockupGate:       lk,
		borrowGate:       &fakeBorrowGate{},
		priceCollarGate:  pc,
	}
	_, err := e.pmPathPreTradeGateChain(context.Background(), gateChainFund(), gateChainAction(), "sell", 100, 239.35, "k2")
	if err == nil || !errors.Is(err, api.ErrConflict) {
		t.Fatalf("expected wrapped ErrConflict for lockup reject, got: %v", err)
	}
	if !strings.Contains(err.Error(), "locked") {
		t.Errorf("error should include the lockup reason, got: %v", err)
	}
	if pc.calls != 0 {
		t.Errorf("price-collar must not run after lockup reject, got %d calls", pc.calls)
	}
}

func TestPMPathPreTradeGateChain_BorrowRejectsUnborrowableShort(t *testing.T) {
	br := &fakeBorrowGate{verdict: broker.BorrowVerdict{Rejected: true, RejectReason: "no locate"}}
	pc := &fakePriceCollarGate{}
	e := &runtimeTradingEngine{
		metrics:          &serverMetrics{},
		marketStatusGate: &fakeMarketStatusGate{},
		lockupGate:       &fakeLockupGate{},
		borrowGate:       br,
		priceCollarGate:  pc,
	}
	_, err := e.pmPathPreTradeGateChain(context.Background(), gateChainFund(), gateChainAction(), "sell", 100, 239.35, "k3")
	if err == nil || !errors.Is(err, api.ErrConflict) {
		t.Fatalf("expected wrapped ErrConflict for borrow reject, got: %v", err)
	}
	if !strings.Contains(err.Error(), "no locate") {
		t.Errorf("error should include the borrow reason, got: %v", err)
	}
	if pc.calls != 0 {
		t.Errorf("price-collar must not run after borrow reject, got %d calls", pc.calls)
	}
}

func TestPMPathPreTradeGateChain_PriceCollarRejectsFatFinger(t *testing.T) {
	pc := &fakePriceCollarGate{verdict: broker.PriceCollarVerdict{Rejected: true, RejectReason: "limit 35% above reference 239.35"}}
	e := &runtimeTradingEngine{
		metrics:          &serverMetrics{},
		marketStatusGate: &fakeMarketStatusGate{},
		lockupGate:       &fakeLockupGate{},
		borrowGate:       &fakeBorrowGate{},
		priceCollarGate:  pc,
	}
	_, err := e.pmPathPreTradeGateChain(context.Background(), gateChainFund(), gateChainAction(), "buy", 200, 323.10, "k4")
	if err == nil || !errors.Is(err, api.ErrConflict) {
		t.Fatalf("expected wrapped ErrConflict for price-collar reject, got: %v", err)
	}
	if !strings.Contains(err.Error(), "35%") {
		t.Errorf("error should include the collar reason, got: %v", err)
	}
}

func TestPMPathPreTradeGateChain_OrderingMatchesSimulator(t *testing.T) {
	// All four gates allow. Verify each gate was called exactly
	// once, AND in the right order (market-status first, price-
	// collar last). Order is checked by ascending call indices.
	type counter struct{ n *int }
	ord := 0
	mc, lc, bc, pcc := 0, 0, 0, 0
	ms := &fakeMarketStatusGate{}
	lk := &fakeLockupGate{}
	br := &fakeBorrowGate{}
	pc := &fakePriceCollarGate{}
	e := &runtimeTradingEngine{
		metrics:          &serverMetrics{},
		marketStatusGate: orderingMarketStatusGate{calls: &mc, order: &ord, child: ms},
		lockupGate:       orderingLockupGate{calls: &lc, order: &ord, child: lk},
		borrowGate:       orderingBorrowGate{calls: &bc, order: &ord, child: br},
		priceCollarGate:  orderingPriceCollarGate{calls: &pcc, order: &ord, child: pc},
	}
	_, err := e.pmPathPreTradeGateChain(context.Background(), gateChainFund(), gateChainAction(), "buy", 200, 239.35, "k5")
	if err != nil {
		t.Fatalf("all-allow chain must succeed, got: %v", err)
	}
	if mc != 1 || lc != 2 || bc != 3 || pcc != 4 {
		t.Errorf("expected ordering market=1 lockup=2 borrow=3 collar=4, got %d %d %d %d", mc, lc, bc, pcc)
	}
	_ = counter{}
}

// orderingXxx gates assign the next monotonically-increasing
// index to themselves on each call, so the test can assert the
// chain visited them in the exact simulator order.
type orderingMarketStatusGate struct {
	calls *int
	order *int
	child *fakeMarketStatusGate
}

func (g orderingMarketStatusGate) CheckOrder(ctx context.Context, p broker.MarketStatusProbe) broker.MarketStatusVerdict {
	*g.order++
	*g.calls = *g.order
	return g.child.CheckOrder(ctx, p)
}

type orderingLockupGate struct {
	calls *int
	order *int
	child *fakeLockupGate
}

func (g orderingLockupGate) CheckOrder(ctx context.Context, p broker.LockupProbe) broker.LockupVerdict {
	*g.order++
	*g.calls = *g.order
	return g.child.CheckOrder(ctx, p)
}

type orderingBorrowGate struct {
	calls *int
	order *int
	child *fakeBorrowGate
}

func (g orderingBorrowGate) CheckOrder(ctx context.Context, p broker.BorrowProbe) broker.BorrowVerdict {
	*g.order++
	*g.calls = *g.order
	return g.child.CheckOrder(ctx, p)
}

type orderingPriceCollarGate struct {
	calls *int
	order *int
	child *fakePriceCollarGate
}

func (g orderingPriceCollarGate) CheckOrder(ctx context.Context, p broker.PriceCollarProbe) broker.PriceCollarVerdict {
	*g.order++
	*g.calls = *g.order
	return g.child.CheckOrder(ctx, p)
}

func TestPMPathPreTradeGateChain_InstrumentKeyFallback(t *testing.T) {
	// When action.InstrumentKey is empty, the chain must
	// reconstruct the key from exchange:symbol so downstream
	// gates (which look up holdings / locks by instrument_key)
	// see a non-empty value. We assert by inspecting the probe
	// the market-status gate received.
	var seenKey string
	gate := captureKeyMarketStatusGate{out: &seenKey}
	e := &runtimeTradingEngine{
		metrics:          &serverMetrics{},
		marketStatusGate: gate,
	}
	action := repository.PlanAction{
		ID:         "a",
		Symbol:     "688205",
		Exchange:   sql.NullString{String: "SSE", Valid: true},
		Market:     sql.NullString{String: "a_share", Valid: true},
		AssetClass: sql.NullString{String: "equity", Valid: true},
		// InstrumentKey deliberately empty
	}
	_, err := e.pmPathPreTradeGateChain(context.Background(), gateChainFund(), action, "buy", 200, 239.35, "k6")
	if err != nil {
		t.Fatalf("allow path must succeed, got: %v", err)
	}
	if seenKey == "" {
		t.Errorf("instrument key fallback should produce a non-empty key, got empty")
	}
}

type captureKeyMarketStatusGate struct {
	out *string
}

func (g captureKeyMarketStatusGate) CheckOrder(_ context.Context, p broker.MarketStatusProbe) broker.MarketStatusVerdict {
	*g.out = p.InstrumentKey
	return broker.MarketStatusVerdict{}
}
