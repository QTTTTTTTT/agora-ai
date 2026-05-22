package risk

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

// HardRiskConfig controls deterministic production execution gates. Zero values
// are replaced with DefaultHardRiskConfig so partially specified fund configs
// remain fail-safe.
type HardRiskConfig struct {
	DailyLossLimit        float64
	MaxSinglePosition     float64
	MaxSectorExposure     float64
	MaxTotalExposure      float64
	MaxOrderPctOfAssets   float64
	MaxOrderAmount        float64
	MaxTradesPerDay       int
	MaxTradesPerSymbolDay int
	// MaxQuoteAge is the maximum staleness allowed for a quote used to
	// price a *risk-increasing* trade (buy/add/short-open). Sells and
	// closes are exempt so the system can still de-risk on a stale tape.
	// Zero means use DefaultHardRiskConfig (15 minutes).
	MaxQuoteAge time.Duration
	// Slippage controls SlippageGuard tolerances. A zero-value config
	// (no entries, zero default) is normalised to DefaultSlippageConfig
	// during HardRiskPolicyFromConfig so partial fund configs remain
	// fail-safe.
	Slippage SlippageConfig
}

// DefaultHardRiskConfig returns conservative production defaults.
func DefaultHardRiskConfig() HardRiskConfig {
	return HardRiskConfig{
		DailyLossLimit:        0.05,
		MaxSinglePosition:     0.30,
		MaxSectorExposure:     0.40,
		MaxTotalExposure:      0.95,
		MaxOrderPctOfAssets:   0.10,
		MaxTradesPerDay:       50,
		MaxTradesPerSymbolDay: 10,
		MaxQuoteAge:           15 * time.Minute,
		Slippage:              DefaultSlippageConfig(),
	}
}

// DefaultHardRiskPolicy is the production execution gate. Unlike advisory
// agent-side checks, every rule in this policy is deterministic and fail-closed:
// any fail finding rejects the proposed order before it reaches execution.
func DefaultHardRiskPolicy() Policy {
	return HardRiskPolicyFromConfig(DefaultHardRiskConfig())
}

// HardRiskPolicyFromConfig builds the execution gate from config while applying
// safe defaults for omitted/invalid values.
func HardRiskPolicyFromConfig(cfg HardRiskConfig) Policy {
	cfg = normalizeHardRiskConfig(cfg)
	return Policy{
		Name: "hard_execution_controls",
		Rules: []Rule{
			DailyLossLimit{MaxLoss: cfg.DailyLossLimit},
			SinglePositionLimit{Max: cfg.MaxSinglePosition},
			SectorExposureLimit{Max: cfg.MaxSectorExposure, Severity: SeverityFail},
			TotalExposureLimit{Max: cfg.MaxTotalExposure},
			MaxOrderNotionalLimit{MaxPctOfAssets: cfg.MaxOrderPctOfAssets, MaxAmount: cfg.MaxOrderAmount},
			TradeFrequencyLimit{MaxTradesPerDay: cfg.MaxTradesPerDay, MaxTradesPerSymbolDay: cfg.MaxTradesPerSymbolDay},
			StaleQuoteGuard{MaxAge: cfg.MaxQuoteAge},
			SlippageGuard{Config: cfg.Slippage},
			SettlementCycleRule{},
		},
	}
}

func normalizeHardRiskConfig(cfg HardRiskConfig) HardRiskConfig {
	def := DefaultHardRiskConfig()
	if cfg.DailyLossLimit <= 0 || cfg.DailyLossLimit > 0.50 {
		cfg.DailyLossLimit = def.DailyLossLimit
	}
	if cfg.MaxSinglePosition <= 0 || cfg.MaxSinglePosition > 1 {
		cfg.MaxSinglePosition = def.MaxSinglePosition
	}
	if cfg.MaxSectorExposure <= 0 || cfg.MaxSectorExposure > 1 {
		cfg.MaxSectorExposure = def.MaxSectorExposure
	}
	if cfg.MaxTotalExposure <= 0 || cfg.MaxTotalExposure > 1.5 {
		cfg.MaxTotalExposure = def.MaxTotalExposure
	}
	if cfg.MaxOrderPctOfAssets <= 0 || cfg.MaxOrderPctOfAssets > 1 {
		cfg.MaxOrderPctOfAssets = def.MaxOrderPctOfAssets
	}
	if cfg.MaxOrderAmount < 0 {
		cfg.MaxOrderAmount = 0
	}
	if cfg.MaxTradesPerDay <= 0 || cfg.MaxTradesPerDay > 10000 {
		cfg.MaxTradesPerDay = def.MaxTradesPerDay
	}
	if cfg.MaxTradesPerSymbolDay <= 0 || cfg.MaxTradesPerSymbolDay > cfg.MaxTradesPerDay {
		cfg.MaxTradesPerSymbolDay = def.MaxTradesPerSymbolDay
	}
	if cfg.MaxQuoteAge <= 0 {
		cfg.MaxQuoteAge = def.MaxQuoteAge
	} else if cfg.MaxQuoteAge > 24*time.Hour {
		// Anything longer than a day is almost certainly a misconfig.
		// Clamp to a defensive ceiling rather than failing closed (which
		// would block all trading for a stale-but-fixable input).
		cfg.MaxQuoteAge = 24 * time.Hour
	}
	cfg.Slippage = normalizeSlippageConfig(cfg.Slippage)
	return cfg
}

// normalizeSlippageConfig fills in DefaultSlippageConfig values for any
// entirely empty SlippageConfig, and clamps individual tolerances to a
// sane upper bound (50%) to avoid disabling the guard accidentally.
func normalizeSlippageConfig(cfg SlippageConfig) SlippageConfig {
	empty := cfg.DefaultTolerance <= 0 &&
		len(cfg.ToleranceByBoard) == 0 &&
		len(cfg.ToleranceByMarket) == 0
	if empty {
		return DefaultSlippageConfig()
	}
	const ceiling = 0.5
	if cfg.DefaultTolerance > ceiling {
		cfg.DefaultTolerance = ceiling
	}
	if cfg.DefaultTolerance < 0 {
		cfg.DefaultTolerance = 0
	}
	for k, v := range cfg.ToleranceByBoard {
		if v < 0 {
			delete(cfg.ToleranceByBoard, k)
			continue
		}
		if v > ceiling {
			cfg.ToleranceByBoard[k] = ceiling
		}
	}
	for k, v := range cfg.ToleranceByMarket {
		if v < 0 {
			delete(cfg.ToleranceByMarket, k)
			continue
		}
		if v > ceiling {
			cfg.ToleranceByMarket[k] = ceiling
		}
	}
	return cfg
}

// DailyLossLimit blocks new risk-taking once the fund's latest daily return is
// below the configured loss threshold. Sells/reductions are allowed so the
// system can still de-risk after the breaker has tripped.
type DailyLossLimit struct {
	MaxLoss float64 // positive fraction, e.g. 0.05 means stop buys after -5%
}

func (r DailyLossLimit) Name() string { return "hard_daily_loss_limit" }

func (r DailyLossLimit) Evaluate(_ context.Context, pc PlanContext) ([]Finding, error) {
	limit := math.Abs(r.MaxLoss)
	if limit <= 0 || pc.DailyReturn > -limit || !hasRiskIncreasingTrade(pc.Trades) {
		return nil, nil
	}
	return []Finding{{
		Rule:       r.Name(),
		Severity:   SeverityFail,
		Current:    pc.DailyReturn,
		Threshold:  -limit,
		Message:    fmt.Sprintf("daily return %s breaches hard loss limit -%s", fmtPct(pc.DailyReturn), fmtPct(limit)),
		Suggestion: "Block new buy/add orders until the next trading day or manual risk reset",
	}}, nil
}

// MaxOrderNotionalLimit caps each order by percent of NAV and/or an absolute
// amount. If both caps are set, the stricter positive cap wins.
//
// Sell-side exemption: the NAV-percent cap is a concentration guard for *new*
// risk-taking (buy/add). Applying it symmetrically to sells creates a
// pathological loop where a position that grew past the cap percentage —
// the exact situation the operator wants to trim — can never be closed,
// because the clear-out order's notional is by definition above
// MaxPctOfAssets * NAV. We therefore exempt sell/reduce orders whose
// quantity is within the held position (i.e. real de-risking, not short
// selling). Short-sell prevention is handled by the downstream
// AvailableQty < quantity check in the trading engine.
type MaxOrderNotionalLimit struct {
	MaxPctOfAssets float64
	MaxAmount      float64
}

func (r MaxOrderNotionalLimit) Name() string { return "hard_max_order_notional" }

func (r MaxOrderNotionalLimit) Evaluate(_ context.Context, pc PlanContext) ([]Finding, error) {
	var capAmount float64
	if r.MaxPctOfAssets > 0 && pc.TotalAssets > 0 {
		capAmount = r.MaxPctOfAssets * pc.TotalAssets
	}
	if r.MaxAmount > 0 && (capAmount <= 0 || r.MaxAmount < capAmount) {
		capAmount = r.MaxAmount
	}
	if capAmount <= 0 {
		return nil, nil
	}

	posQty := make(map[string]float64, len(pc.Positions))
	for _, p := range pc.Positions {
		posQty[p.Symbol] = p.Quantity
	}

	var out []Finding
	for _, t := range pc.Trades {
		notional := t.Notional()
		f := Finding{
			Rule:      r.Name(),
			Symbol:    t.Symbol,
			Current:   notional,
			Threshold: capAmount,
			Message:   fmt.Sprintf("%s order notional %.2f within hard cap %.2f", t.Symbol, notional, capAmount),
			Severity:  SeverityInfo,
		}
		if notional > capAmount+1e-9 {
			// Sell-side exemption: a reduce/sell whose quantity stays
			// inside the held position is real de-risking, not new
			// concentration. The notional cap is a buy-side guard; we
			// log it as info ("waived") instead of failing the order.
			if t.Side.IsSell() && posQty[t.Symbol] > 0 && t.Quantity <= posQty[t.Symbol]+1e-9 {
				f.Severity = SeverityInfo
				f.Message = fmt.Sprintf(
					"%s sell notional %.2f exceeds hard cap %.2f but stays within held quantity %.0f — waived as position-reducing",
					t.Symbol, notional, capAmount, posQty[t.Symbol],
				)
			} else {
				f.Severity = SeverityFail
				f.Message = fmt.Sprintf("%s order notional %.2f exceeds hard cap %.2f", t.Symbol, notional, capAmount)
				f.Suggestion = fmt.Sprintf("Split or reduce %s order to <= %.2f", t.Symbol, capAmount)
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// TradeFrequencyLimit caps the total number of trades per day and per symbol.
type TradeFrequencyLimit struct {
	MaxTradesPerDay       int
	MaxTradesPerSymbolDay int
}

func (r TradeFrequencyLimit) Name() string { return "hard_trade_frequency" }

func (r TradeFrequencyLimit) Evaluate(_ context.Context, pc PlanContext) ([]Finding, error) {
	dayCount := countActiveTrades(pc.TradesToday)
	symbolCounts := map[string]int{}
	for _, t := range pc.TradesToday {
		if !tradeCountsTowardFrequency(t.Status) {
			continue
		}
		symbolCounts[strings.TrimSpace(t.Symbol)]++
	}
	for _, t := range pc.Trades {
		dayCount++
		symbolCounts[strings.TrimSpace(t.Symbol)]++
	}

	var out []Finding
	if r.MaxTradesPerDay > 0 {
		f := Finding{
			Rule:      r.Name(),
			Current:   float64(dayCount),
			Threshold: float64(r.MaxTradesPerDay),
			Message:   fmt.Sprintf("daily trade count %d/%d", dayCount, r.MaxTradesPerDay),
			Severity:  SeverityInfo,
		}
		if dayCount > r.MaxTradesPerDay {
			f.Severity = SeverityFail
			f.Message = fmt.Sprintf("daily trade count %d exceeds hard cap %d", dayCount, r.MaxTradesPerDay)
			f.Suggestion = "Stop submitting new orders for the rest of the trading day"
		}
		out = append(out, f)
	}

	if r.MaxTradesPerSymbolDay > 0 {
		for _, symbol := range sortedIntKeys(symbolCounts) {
			count := symbolCounts[symbol]
			f := Finding{
				Rule:      r.Name(),
				Symbol:    symbol,
				Current:   float64(count),
				Threshold: float64(r.MaxTradesPerSymbolDay),
				Message:   fmt.Sprintf("%s daily trade count %d/%d", symbol, count, r.MaxTradesPerSymbolDay),
				Severity:  SeverityInfo,
			}
			if count > r.MaxTradesPerSymbolDay {
				f.Severity = SeverityFail
				f.Message = fmt.Sprintf("%s daily trade count %d exceeds hard cap %d", symbol, count, r.MaxTradesPerSymbolDay)
				f.Suggestion = fmt.Sprintf("Stop trading %s for the rest of the trading day", symbol)
			}
			out = append(out, f)
		}
	}

	return out, nil
}

func hasRiskIncreasingTrade(trades []ProposedTrade) bool {
	for _, t := range trades {
		if !t.Side.IsSell() {
			return true
		}
	}
	return false
}

func countActiveTrades(trades []ExecutedTrade) int {
	total := 0
	for _, t := range trades {
		if tradeCountsTowardFrequency(t.Status) {
			total++
		}
	}
	return total
}

func tradeCountsTowardFrequency(status string) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	return s == "" || s == "filled" || s == "partial" || s == "pending" || s == "submitted"
}

func sortedIntKeys(m map[string]int) []string {
	floats := make(map[string]float64, len(m))
	for k, v := range m {
		floats[k] = float64(v)
	}
	return sortedKeys(floats)
}

// StaleQuoteGuard blocks risk-increasing orders (buys/adds, short opens)
// priced off an outdated quote. The rule fires when *either* signal is set:
//   - QuoteIsStale was already flagged by the market-data layer (which knows
//     the operator-tuned threshold), or
//   - QuoteAsOf is older than the local MaxAge fallback.
//
// Sells, reductions, and closes are exempt: we still want to let the system
// de-risk when the tape goes cold (the alternative — being stuck long in a
// halted market — is strictly worse).
type StaleQuoteGuard struct {
	MaxAge time.Duration
	// nowFn is overridable for tests. Production paths leave it nil and
	// fall back to time.Now().UTC().
	nowFn func() time.Time
}

func (r StaleQuoteGuard) Name() string { return "hard_stale_quote_guard" }

func (r StaleQuoteGuard) Evaluate(_ context.Context, pc PlanContext) ([]Finding, error) {
	now := time.Now().UTC()
	if r.nowFn != nil {
		now = r.nowFn()
	}
	var out []Finding
	for _, trade := range pc.Trades {
		if trade.Side.IsSell() {
			continue
		}
		age := time.Duration(0)
		if !trade.QuoteAsOf.IsZero() {
			age = now.Sub(trade.QuoteAsOf)
		}
		stale := trade.QuoteIsStale
		if r.MaxAge > 0 && age > r.MaxAge {
			stale = true
		}
		if !stale {
			continue
		}
		f := Finding{
			Rule:      r.Name(),
			Symbol:    trade.Symbol,
			Severity:  SeverityFail,
			Current:   age.Seconds(),
			Threshold: r.MaxAge.Seconds(),
		}
		if !trade.QuoteAsOf.IsZero() {
			f.Message = fmt.Sprintf("%s quote outdated (age: %s); refusing risk-increasing order", trade.Symbol, age.Round(time.Second))
		} else {
			f.Message = fmt.Sprintf("%s quote flagged stale by market data layer; refresh before retry", trade.Symbol)
		}
		f.Suggestion = fmt.Sprintf("Refresh the live quote for %s and re-run the plan before executing.", trade.Symbol)
		out = append(out, f)
	}
	return out, nil
}
