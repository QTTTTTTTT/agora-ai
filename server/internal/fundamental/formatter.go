package fundamental

import (
	"fmt"
	"strings"
)

// FormatForPrompt produces a single-line summary the debate Bull /
// Bear agents can embed in their FundamentalReports prompt block.
// Returns "" when nothing useful is set so callers can skip an
// empty bullet.
//
// Examples:
//   "AAPL: PE 28.3 / fwd 24.1, PB 47.2, ROE 156%, rev growth +8%,
//    op margin 31%, mkt cap 2.85T USD"
//   "600519: PE 28.1, PB 8.2, ROE 32%, rev growth +12%, mkt cap
//    2.1T CNY"
//
// Numbers are formatted to two significant decimals for ratios, one
// decimal for percentages, and condensed (B/T suffix) for market
// cap. The currency tag is preserved at the tail when present.
func (m *Metrics) FormatForPrompt() string {
	if m == nil {
		return ""
	}
	parts := []string{}
	if m.PE != 0 {
		if m.ForwardPE != 0 && m.ForwardPE != m.PE {
			parts = append(parts, fmt.Sprintf("PE %.1f/fwd %.1f", m.PE, m.ForwardPE))
		} else {
			parts = append(parts, fmt.Sprintf("PE %.1f", m.PE))
		}
	} else if m.ForwardPE != 0 {
		parts = append(parts, fmt.Sprintf("fwdPE %.1f", m.ForwardPE))
	}
	if m.PB != 0 {
		parts = append(parts, fmt.Sprintf("PB %.1f", m.PB))
	}
	if m.ReturnOnEquity != 0 {
		parts = append(parts, fmt.Sprintf("ROE %.1f%%", m.ReturnOnEquity*100))
	}
	if m.ProfitMargin != 0 {
		parts = append(parts, fmt.Sprintf("net margin %.1f%%", m.ProfitMargin*100))
	}
	if m.OperatingMargin != 0 {
		parts = append(parts, fmt.Sprintf("op margin %.1f%%", m.OperatingMargin*100))
	}
	if m.RevenueGrowth != 0 {
		parts = append(parts, fmt.Sprintf("rev growth %+.1f%%", m.RevenueGrowth*100))
	}
	if m.EarningsGrowth != 0 {
		parts = append(parts, fmt.Sprintf("eps growth %+.1f%%", m.EarningsGrowth*100))
	}
	if m.DividendYield != 0 {
		parts = append(parts, fmt.Sprintf("dividend %.2f%%", m.DividendYield*100))
	}
	if m.DebtToEquity != 0 {
		parts = append(parts, fmt.Sprintf("D/E %.2f", m.DebtToEquity))
	}
	if m.Beta != 0 {
		parts = append(parts, fmt.Sprintf("beta %.2f", m.Beta))
	}
	if m.MarketCap != 0 {
		parts = append(parts, "mkt cap "+formatMarketCap(m.MarketCap, m.Currency))
	}
	if len(parts) == 0 {
		return ""
	}
	symbol := strings.ToUpper(strings.TrimSpace(m.Symbol))
	if symbol == "" {
		return strings.Join(parts, ", ")
	}
	return symbol + ": " + strings.Join(parts, ", ")
}

// formatMarketCap renders a raw market cap number using human-
// readable suffixes (K / M / B / T) so the prompt doesn't waste
// tokens on "2848234500000". Trailing zeros after the decimal are
// trimmed so 1.00T → 1T and 2.85T stays 2.85T.
func formatMarketCap(value float64, currency string) string {
	abs := value
	if abs < 0 {
		abs = -abs
	}
	var unit string
	var scaled float64
	switch {
	case abs >= 1e12:
		unit = "T"
		scaled = value / 1e12
	case abs >= 1e9:
		unit = "B"
		scaled = value / 1e9
	case abs >= 1e6:
		unit = "M"
		scaled = value / 1e6
	case abs >= 1e3:
		unit = "K"
		scaled = value / 1e3
	default:
		unit = ""
		scaled = value
	}
	formatted := fmt.Sprintf("%.2f", scaled)
	formatted = strings.TrimRight(formatted, "0")
	formatted = strings.TrimRight(formatted, ".")
	out := formatted + unit
	if currency = strings.TrimSpace(currency); currency != "" {
		out += " " + strings.ToUpper(currency)
	}
	return out
}
