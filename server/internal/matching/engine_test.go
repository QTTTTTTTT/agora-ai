package matching

import (
	"errors"
	"math"
	"testing"
)

func TestNormalizeSide(t *testing.T) {
	cases := map[string]Side{
		"buy":   SideBuy,
		"BUY":   SideBuy,
		" long": SideBuy,
		"sell":  SideSell,
		"Short": SideSell,
		"hold":  "",
		"":      "",
	}
	for in, want := range cases {
		if got := NormalizeSide(in); got != want {
			t.Fatalf("NormalizeSide(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteHasSpreadAndMid(t *testing.T) {
	q := Quote{Last: 100, Bid: 99.5, Ask: 100.5}
	if !q.HasSpread() {
		t.Fatal("expected HasSpread true")
	}
	if math.Abs(q.MidPrice()-100) > 1e-9 {
		t.Fatalf("expected mid 100, got %v", q.MidPrice())
	}
	noSpread := Quote{Last: 50}
	if noSpread.HasSpread() {
		t.Fatal("expected HasSpread false")
	}
	if noSpread.MidPrice() != 50 {
		t.Fatalf("expected fallback to last, got %v", noSpread.MidPrice())
	}
}

func TestDefaultEngineMatchesAtLastWithLegacyFees(t *testing.T) {
	engine := NewDefaultEngine()
	order := Order{InstrumentKey: "AAPL", Side: SideBuy, Quantity: 100, AssetClass: "equity"}
	fill, err := engine.Match(order, Quote{Last: 100})
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if fill.Price != 100 {
		t.Fatalf("expected fill price 100, got %v", fill.Price)
	}
	if fill.Commission != 10 {
		t.Fatalf("expected commission 10 (10bps of 10000), got %v", fill.Commission)
	}
	if fill.StampTax != 0 {
		t.Fatalf("expected no stamp tax on buy, got %v", fill.StampTax)
	}
	if fill.TransferFee != 0.02 {
		t.Fatalf("expected transfer 0.02, got %v", fill.TransferFee)
	}
	if fill.Notional != 10000 {
		t.Fatalf("expected notional 10000, got %v", fill.Notional)
	}
	if fill.SlippageBps != 0 {
		t.Fatalf("expected zero slippage, got %v", fill.SlippageBps)
	}
}

func TestDefaultEngineSellAppliesStampTax(t *testing.T) {
	engine := NewDefaultEngine()
	order := Order{Side: SideSell, Quantity: 100, AssetClass: "equity"}
	fill, err := engine.Match(order, Quote{Last: 50})
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if fill.StampTax != 5 {
		t.Fatalf("expected stamp tax 5 (10bps of 5000), got %v", fill.StampTax)
	}
	if fill.Commission != 5 {
		t.Fatalf("expected commission 5, got %v", fill.Commission)
	}
}

func TestEngineRejectsInvalidOrders(t *testing.T) {
	engine := NewDefaultEngine()
	if _, err := engine.Match(Order{Quantity: 0, Side: SideBuy}, Quote{Last: 1}); !errors.Is(err, ErrInvalidOrder) {
		t.Fatalf("expected ErrInvalidOrder for zero quantity, got %v", err)
	}
	if _, err := engine.Match(Order{Quantity: 1, Side: ""}, Quote{Last: 1}); !errors.Is(err, ErrInvalidOrder) {
		t.Fatalf("expected ErrInvalidOrder for empty side, got %v", err)
	}
	if _, err := engine.Match(Order{Quantity: 1, Side: SideBuy}, Quote{}); !errors.Is(err, ErrNoQuote) {
		t.Fatalf("expected ErrNoQuote for empty quote, got %v", err)
	}
}

func TestEngineRejectsLimitNotMarketable(t *testing.T) {
	engine := NewDefaultEngine()
	buy, err := engine.Match(Order{Side: SideBuy, Quantity: 1, LimitPrice: 99}, Quote{Last: 100})
	if !errors.Is(err, ErrLimitNotMarketable) {
		t.Fatalf("expected ErrLimitNotMarketable for too-low buy limit, got fill=%+v err=%v", buy, err)
	}
	sell, err := engine.Match(Order{Side: SideSell, Quantity: 1, LimitPrice: 101}, Quote{Last: 100})
	if !errors.Is(err, ErrLimitNotMarketable) {
		t.Fatalf("expected ErrLimitNotMarketable for too-high sell limit, got fill=%+v err=%v", sell, err)
	}
}

func TestSpreadCrossSlippageBuyLiftsToAsk(t *testing.T) {
	engine := &MarketableEngine{Slippage: SpreadCrossSlippage{}, Fees: FixedRateEquityFees{}}
	fill, err := engine.Match(Order{Side: SideBuy, Quantity: 100}, Quote{Last: 100, Bid: 99.5, Ask: 100.5})
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if fill.Price != 100.5 {
		t.Fatalf("expected fill price 100.5, got %v", fill.Price)
	}
	// bps = (100.5 - 100)/100 * 10000 = 50
	if fill.SlippageBps != 50 {
		t.Fatalf("expected slippage 50 bps, got %v", fill.SlippageBps)
	}
}

func TestSpreadCrossSlippageSellDropsToBid(t *testing.T) {
	engine := &MarketableEngine{Slippage: SpreadCrossSlippage{}, Fees: FixedRateEquityFees{}}
	fill, err := engine.Match(Order{Side: SideSell, Quantity: 100}, Quote{Last: 100, Bid: 99.5, Ask: 100.5})
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if fill.Price != 99.5 {
		t.Fatalf("expected fill price 99.5, got %v", fill.Price)
	}
	// sell slippage is reported as positive when adverse
	if fill.SlippageBps != 50 {
		t.Fatalf("expected 50 bps adverse slippage, got %v", fill.SlippageBps)
	}
}

func TestSpreadCrossFallsBackWhenSpreadMissing(t *testing.T) {
	slip := SpreadCrossSlippage{}
	if got := slip.FillPrice(Order{Side: SideBuy, Quantity: 1}, Quote{Last: 42}); got != 42 {
		t.Fatalf("expected fallback to last (42), got %v", got)
	}
}

func TestLinearImpactSlippageScalesWithSize(t *testing.T) {
	slip := LinearImpactSlippage{
		Inner:                ZeroSlippage{},
		ImpactCoefficientBps: 10,
		ReferenceNotional:    1_000_000,
	}
	small := slip.FillPrice(Order{Side: SideBuy, Quantity: 100}, Quote{Last: 100})  // 10k notional => bps = 10*sqrt(0.01) = 1
	large := slip.FillPrice(Order{Side: SideBuy, Quantity: 1000}, Quote{Last: 100}) // 100k notional => bps = 10*sqrt(0.1) ~= 3.16
	if small <= 100 || large <= small {
		t.Fatalf("expected impact to scale up: small=%v large=%v", small, large)
	}
	// sells should be reduced symmetrically
	sellSmall := slip.FillPrice(Order{Side: SideSell, Quantity: 100}, Quote{Last: 100})
	if sellSmall >= 100 {
		t.Fatalf("expected sell impact to drop price below 100, got %v", sellSmall)
	}
}

func TestFuturesFeesAppliesNotionalAndPerContract(t *testing.T) {
	fees := FuturesFees{NotionalRateBps: 3, PerContractFee: 0.5}
	commission, stamp, transfer := fees.Fees(Order{Quantity: 2, ContractMultiplier: 10}, 100)
	// notional = 2 * 100 * 10 = 2000; bps fee = 2000 * 3 / 10000 = 0.6; per-contract = 2 * 0.5 = 1
	if math.Abs(commission-1.6) > 1e-9 {
		t.Fatalf("expected commission 1.6, got %v", commission)
	}
	if stamp != 0 || transfer != 0 {
		t.Fatalf("futures should have no stamp/transfer, got %v / %v", stamp, transfer)
	}
}

func TestCryptoFeesUsesTakerRate(t *testing.T) {
	fees := CryptoFees{TakerRateBps: 20}
	commission, stamp, transfer := fees.Fees(Order{Quantity: 0.5}, 30000)
	// notional = 15000; commission = 15000 * 20 / 10000 = 30
	if math.Abs(commission-30) > 1e-9 {
		t.Fatalf("expected commission 30, got %v", commission)
	}
	if stamp != 0 || transfer != 0 {
		t.Fatalf("crypto should have no stamp/transfer, got %v / %v", stamp, transfer)
	}
}

func TestDefaultEngineFuturesNotionalUsesMultiplier(t *testing.T) {
	engine := NewDefaultEngine()
	order := Order{Side: SideBuy, Quantity: 2, ContractMultiplier: 10, AssetClass: "futures"}
	fill, err := engine.Match(order, Quote{Last: 100})
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if fill.Notional != 2000 {
		t.Fatalf("expected notional 2*100*10=2000, got %v", fill.Notional)
	}
}
