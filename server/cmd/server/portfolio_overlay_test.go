package main

import (
	"database/sql"
	"testing"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/marketdata"
	"github.com/fundai/server/internal/repository"
)

// TestApplyPositionLiveOverlayUsesFreshQuote drives the happy path: a
// position with a stale DB price is overlaid with the live quote and the
// MarketValue / UnrealizedPnL get recomputed off the new price.
func TestApplyPositionLiveOverlayUsesFreshQuote(t *testing.T) {
	asOf := time.Date(2026, 5, 20, 14, 30, 0, 0, time.UTC)
	position := repository.HoldingPosition{
		InstrumentKey: "us|nasdaq|equity|MU",
		Symbol:        "MU",
		Market:        sql.NullString{String: "usstock", Valid: true},
		Quantity:      50,
		CostPrice:     80.0,
		CurrentPrice:  85.0, // stale persisted value
		MarketValue:   85.0 * 50,
		UpdatedAt:     asOf.Add(-1 * time.Hour),
	}
	out := api.Position{
		InstrumentKey: position.InstrumentKey,
		Symbol:        position.Symbol,
		Market:        position.Market.String,
		Quantity:      position.Quantity,
		CostPrice:     position.CostPrice,
		CurrentPrice:  position.CurrentPrice,
		MarketValue:   position.MarketValue,
	}
	quote := &marketdata.QuoteSnapshot{
		Symbol: "MU",
		Price:  100.0,
		AsOf:   asOf,
		Source: "yahoo",
	}
	quotes := map[string]*marketdata.QuoteSnapshot{
		positionInstrumentRef(&position).CacheKey(): quote,
	}

	applyPositionLiveOverlay(&out, &position, quotes)

	if out.CurrentPrice != 100.0 {
		t.Fatalf("CurrentPrice = %v, want 100", out.CurrentPrice)
	}
	if out.MarketValue != 100*50 {
		t.Fatalf("MarketValue = %v, want 5000", out.MarketValue)
	}
	if out.UnrealizedPnL == nil || *out.UnrealizedPnL != (100-80)*50 {
		t.Fatalf("UnrealizedPnL = %v, want 1000", out.UnrealizedPnL)
	}
	if out.PriceAsOf == "" {
		t.Fatalf("PriceAsOf should be populated from the quote")
	}
	if out.PriceSource != "yahoo" {
		t.Fatalf("PriceSource = %q, want yahoo", out.PriceSource)
	}
	if out.IsStale {
		t.Fatalf("IsStale should be false for a fresh quote")
	}
}

// TestApplyPositionLiveOverlayMissingQuoteFallsBack confirms the fallback
// path: when the marketdata service returns no quote for an instrument the
// DB-cached value is preserved and IsStale is set so the UI can render a
// warning badge.
func TestApplyPositionLiveOverlayMissingQuoteFallsBack(t *testing.T) {
	updatedAt := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	position := repository.HoldingPosition{
		InstrumentKey: "us|nyse|equity|TSLA",
		Symbol:        "TSLA",
		Market:        sql.NullString{String: "usstock", Valid: true},
		Quantity:      10,
		CostPrice:     200.0,
		CurrentPrice:  250.0,
		MarketValue:   2500,
		UpdatedAt:     updatedAt,
	}
	out := api.Position{
		InstrumentKey: position.InstrumentKey,
		Symbol:        position.Symbol,
		Market:        position.Market.String,
		Quantity:      position.Quantity,
		CostPrice:     position.CostPrice,
		CurrentPrice:  position.CurrentPrice,
		MarketValue:   position.MarketValue,
	}

	applyPositionLiveOverlay(&out, &position, nil /* no quotes */)

	if out.CurrentPrice != 250.0 {
		t.Fatalf("CurrentPrice should preserve DB value, got %v", out.CurrentPrice)
	}
	if out.MarketValue != 2500 {
		t.Fatalf("MarketValue should preserve DB value, got %v", out.MarketValue)
	}
	if !out.IsStale {
		t.Fatalf("IsStale should be true when no fresh quote is available")
	}
	wantAsOf := updatedAt.UTC().Format(time.RFC3339)
	if out.PriceAsOf != wantAsOf {
		t.Fatalf("PriceAsOf = %q, want %q (fallback to UpdatedAt)", out.PriceAsOf, wantAsOf)
	}
	if out.PriceSource != "" {
		t.Fatalf("PriceSource should be empty when fallback fires, got %q", out.PriceSource)
	}
}

// TestApplyPositionLiveOverlayStaleQuotePropagatesFlag asserts that a quote
// flagged as stale by marketdata is forwarded to the API position so the
// frontend can show a "data is X hours old" badge even when we did manage
// to fetch a price.
func TestApplyPositionLiveOverlayStaleQuotePropagatesFlag(t *testing.T) {
	old := time.Date(2026, 5, 19, 21, 0, 0, 0, time.UTC) // 16h+ before "now"
	position := repository.HoldingPosition{
		InstrumentKey: "us|nasdaq|equity|MU",
		Symbol:        "MU",
		Market:        sql.NullString{String: "usstock", Valid: true},
		Quantity:      30,
		CostPrice:     85.0,
		CurrentPrice:  90.0,
		MarketValue:   2700,
		UpdatedAt:     time.Now().Add(-2 * time.Hour),
	}
	out := api.Position{
		InstrumentKey: position.InstrumentKey,
		Symbol:        position.Symbol,
		Market:        position.Market.String,
		Quantity:      position.Quantity,
		CostPrice:     position.CostPrice,
		CurrentPrice:  position.CurrentPrice,
		MarketValue:   position.MarketValue,
	}
	quote := &marketdata.QuoteSnapshot{
		Symbol:  "MU",
		Price:   91.5,
		AsOf:    old,
		Source:  "yahoo",
		IsStale: true,
	}
	quotes := map[string]*marketdata.QuoteSnapshot{
		positionInstrumentRef(&position).CacheKey(): quote,
	}

	applyPositionLiveOverlay(&out, &position, quotes)

	if !out.IsStale {
		t.Fatalf("IsStale=true on the quote should propagate to the position")
	}
	if out.CurrentPrice != 91.5 {
		t.Fatalf("CurrentPrice should still update on a stale quote, got %v", out.CurrentPrice)
	}
}

// TestPositionInstrumentRefShape sanity-checks the adapter conversion
// preserves the fields required for cache-key matching.
func TestPositionInstrumentRefShape(t *testing.T) {
	position := repository.HoldingPosition{
		InstrumentKey: "us|nasdaq|equity|MU",
		Symbol:        "mu",
		Market:        sql.NullString{String: "usstock", Valid: true},
		Exchange:      sql.NullString{String: "NASDAQ", Valid: true},
		AssetClass:    sql.NullString{String: "equity", Valid: true},
	}
	ref := positionInstrumentRef(&position)
	if ref.NormalizedSymbol() != "MU" {
		t.Fatalf("NormalizedSymbol = %q, want MU", ref.NormalizedSymbol())
	}
	if ref.CacheKey() == "" {
		t.Fatalf("CacheKey should be non-empty")
	}
}
