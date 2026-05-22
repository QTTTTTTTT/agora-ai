package matching

// ZeroSlippage fills the order at the last printed price. This reproduces the
// current runtime behaviour and is the safe default until a real slippage
// model is calibrated for each venue.
type ZeroSlippage struct{}

// FillPrice implements SlippageModel.
func (ZeroSlippage) FillPrice(_ Order, quote Quote) float64 {
	if quote.Last > 0 {
		return quote.Last
	}
	return quote.MidPrice()
}

// SpreadCrossSlippage fills buy orders at the ask and sell orders at the bid.
// When bid/ask are unavailable it falls back to the configured fallback
// model, which defaults to ZeroSlippage.
type SpreadCrossSlippage struct {
	Fallback SlippageModel
}

// FillPrice implements SlippageModel.
func (s SpreadCrossSlippage) FillPrice(order Order, quote Quote) float64 {
	if quote.HasSpread() {
		switch order.Side {
		case SideBuy:
			return quote.Ask
		case SideSell:
			return quote.Bid
		}
	}
	fallback := s.Fallback
	if fallback == nil {
		fallback = ZeroSlippage{}
	}
	return fallback.FillPrice(order, quote)
}

// LinearImpactSlippage adds a square-root-style impact term on top of an
// inner slippage model. Notional is in quote-currency units; the impact is
// expressed in basis points and applied symmetrically (buys pay more, sells
// receive less).
//
//   adverseBps = ImpactCoefficientBps * sqrt(notional / ReferenceNotional)
//
// This is intentionally a simple model; calibration belongs in PR-12 forward
// test work. The default coefficients (5 bps at $1M notional) are inert
// enough to be a placeholder, but tunable.
type LinearImpactSlippage struct {
	Inner                SlippageModel
	ImpactCoefficientBps float64
	ReferenceNotional    float64
}

// FillPrice implements SlippageModel.
func (s LinearImpactSlippage) FillPrice(order Order, quote Quote) float64 {
	inner := s.Inner
	if inner == nil {
		inner = ZeroSlippage{}
	}
	base := inner.FillPrice(order, quote)
	if base <= 0 {
		return base
	}
	if s.ImpactCoefficientBps <= 0 || s.ReferenceNotional <= 0 {
		return base
	}
	multiplier := order.ContractMultiplier
	if multiplier <= 0 {
		multiplier = 1
	}
	notional := order.Quantity * base * multiplier
	if notional <= 0 {
		return base
	}
	ratio := notional / s.ReferenceNotional
	if ratio <= 0 {
		return base
	}
	bps := s.ImpactCoefficientBps * sqrt(ratio)
	adjustment := base * bps / 10000
	switch order.Side {
	case SideBuy:
		return base + adjustment
	case SideSell:
		return base - adjustment
	}
	return base
}

// sqrt is a tiny helper to avoid importing math at the top level of this
// file (keeps the surface obvious for review). math.Sqrt would also be
// fine; using it here keeps the dependency local.
func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	// Newton-Raphson converges quickly for the values we deal with
	// (positive notionals well below 1e18). Three iterations are plenty
	// for the precision we need (slippage is bps-level).
	z := x
	for i := 0; i < 8; i++ {
		if z == 0 {
			break
		}
		z = z - (z*z-x)/(2*z)
	}
	return z
}
