// Default policy presets that mirror the legacy hard-coded rules in
// agent.RiskAgent so callers can transition to the DSL incrementally.
package risk

// DefaultEquityPolicy returns a policy approximating the legacy
// agent.DefaultRiskConfig defaults: 30% single-position cap, 95% total
// exposure, 40% sector cap, 10% liquidity cap, and A-share lot-size
// alignment (no-op for non-A-share symbols).
func DefaultEquityPolicy() Policy {
	return Policy{
		Name: "default_equity",
		Rules: []Rule{
			SinglePositionLimit{Max: 0.30},
			TotalExposureLimit{Max: 0.95},
			SectorExposureLimit{Max: 0.40},
			LiquidityLimit{Max: 0.10},
			LotSizeRule{},
			SettlementCycleRule{},
		},
	}
}

// QuantPolicy adds VaR, correlation and stress-test rules on top of the
// equity defaults. Callers should populate MarketSnapshot.HistoricalReturns
// (and optionally StressShocks) for these rules to fire.
func QuantPolicy() Policy {
	p := DefaultEquityPolicy()
	p.Name = "default_quant"
	p.Rules = append(p.Rules,
		HistoricalVaRLimit{Confidence: 0.95, Max: 0.05, MinSamples: 60},
		CorrelationLimit{Max: 0.85, MinWeight: 0.05, MinSamples: 60},
		StressTestLimit{Max: 0.10, FailAt: 0.25},
	)
	return p
}
