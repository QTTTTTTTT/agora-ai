// mock_provider.go — fixture/mock broker statement source (P1-3).
//
// Why this exists
//
// Until a real broker connection is wired (P0-9 broker_links + a
// future FIX/REST adapter), the platform has no real EOD statement
// to reconcile against. The mock provider closes that gap two ways:
//
//   1. PerfectMirror — fabricates a Statement that EXACTLY matches
//      the InternalSnapshot. Useful as a smoke test: a recon run
//      against a perfect mirror should produce zero breaks. If it
//      doesn't, the engine itself has a bug.
//
//   2. WithDrift — same as PerfectMirror, but applies controlled
//      drifts (one position with a quantity delta, one cash bucket
//      with a balance delta, one trade with a price delta). Useful
//      as a fixture for the admin UI / golden tests / demos.
//
// What this is NOT
//
//   - A live broker adapter. There is no parser for IBKR / Alpaca /
//     etc. yet. Those will land alongside their respective
//     broker_links entries and live in their own internal/brokers/*
//     packages.

package recon

import (
	"fmt"
	"math/rand"
	"time"
)

// MockProviderOptions configures the fabricated drift.
type MockProviderOptions struct {
	// IncludeDrift, when true, perturbs the mirror by:
	//   - adjusting one position's quantity by DriftQuantity
	//   - adjusting one cash currency's balance by DriftCash
	//   - adjusting one trade's price by DriftPrice
	IncludeDrift bool
	// DriftQuantity is the qty delta applied to the FIRST position
	// (selected deterministically by canonicalSymbol order). 0
	// means "no drift on this dimension".
	DriftQuantity float64
	// DriftCash is the balance delta applied to the FIRST cash
	// bucket.
	DriftCash float64
	// DriftPrice is the per-share delta applied to the FIRST trade.
	DriftPrice float64
	// Source labels the resulting Statement. Defaults to SourceMock.
	Source StatementSource
	// Seed makes broker_trade_id generation deterministic in tests.
	Seed int64
}

// DefaultMockOptions is a reasonable starter for a UI demo: $0.50
// cash drift + a 1-share quantity drift = at least one warning-level
// break, which is what the dashboard needs to render the
// 'attention' state.
var DefaultMockOptions = MockProviderOptions{
	IncludeDrift:  true,
	DriftQuantity: 1,
	DriftCash:     0.50,
	DriftPrice:    0.10,
	Source:        SourceMock,
}

// MockProvider fabricates a statement from an internal snapshot.
// Stateless; safe to share.
type MockProvider struct {
	opts MockProviderOptions
}

// NewMockProvider constructs a MockProvider. Pass `MockProviderOptions{}`
// for the perfect-mirror behaviour.
func NewMockProvider(opts MockProviderOptions) *MockProvider {
	if opts.Source == "" {
		opts.Source = SourceMock
	}
	return &MockProvider{opts: opts}
}

// Build generates a Statement from the given snapshot. The
// statement_date is the snapshot's AsOfDate; the broker_trade_id
// for each fabricated trade is deterministic when Seed is set.
func (m *MockProvider) Build(snap *InternalSnapshot) *Statement {
	if snap == nil {
		return nil
	}
	st := &Statement{
		FundID:        snap.FundID,
		StatementDate: snap.AsOfDate,
		Source:        m.opts.Source,
	}

	// Position mirror.
	for i, p := range snap.Positions {
		qty := p.Quantity
		if m.opts.IncludeDrift && i == 0 && m.opts.DriftQuantity != 0 {
			qty += m.opts.DriftQuantity
		}
		st.Positions = append(st.Positions, StatementPosition{
			Symbol:      canonicalSymbol(p.Symbol),
			Quantity:    qty,
			AvgCost:     p.AvgCost,
			MarketValue: 0, // mock doesn't price; engine doesn't need it
			Currency:    canonicalCurrency(p.Currency),
		})
	}

	// Cash mirror.
	for i, c := range snap.Cash {
		bal := c.Balance
		if m.opts.IncludeDrift && i == 0 && m.opts.DriftCash != 0 {
			bal += m.opts.DriftCash
		}
		st.Cash = append(st.Cash, StatementCash{
			Currency: canonicalCurrency(c.Currency),
			Balance:  bal,
		})
	}

	// Trade mirror.
	rng := rand.New(rand.NewSource(m.opts.Seed))
	for i, t := range snap.Trades {
		price := t.Price
		if m.opts.IncludeDrift && i == 0 && m.opts.DriftPrice != 0 {
			price += m.opts.DriftPrice
		}
		brokerID := t.ExternalRef
		if brokerID == "" {
			brokerID = fmt.Sprintf("MOCK-%d-%d", m.opts.Seed, rng.Int63())
		}
		st.Trades = append(st.Trades, StatementTrade{
			BrokerTradeID: brokerID,
			BrokerOrderID: t.ExternalRef,
			Symbol:        canonicalSymbol(t.Symbol),
			Side:          t.Side,
			Quantity:      t.Quantity,
			Price:         price,
			Fee:           t.Fee,
			Currency:      canonicalCurrency(t.Currency),
			ExecutedAt:    t.ExecutedAt,
		})
	}
	return st
}

// IngestParamsFromBuild converts a built Statement into the
// IngestParams the Repo expects. Saves callers a manual conversion
// step.
func IngestParamsFromBuild(st *Statement, ingestedBy string) IngestParams {
	if st == nil {
		return IngestParams{}
	}
	return IngestParams{
		FundID:        st.FundID,
		BrokerLinkID:  st.BrokerLinkID,
		StatementDate: st.StatementDate,
		Source:        st.Source,
		Positions:     st.Positions,
		Cash:          st.Cash,
		Trades:        st.Trades,
		IngestedBy:    ingestedBy,
		RawPayload:    map[string]any{"_synthetic": true, "generated_at": time.Now().UTC().Format(time.RFC3339)},
	}
}
