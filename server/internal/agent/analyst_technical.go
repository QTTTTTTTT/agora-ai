// analyst_technical.go — S8.1 TechnicalAnalyst.
//
// Reads AnalystInput.Technical (quantsnapshot + signals + spark)
// and produces a chart-pattern / regime-based call. The
// directional anchor is the regime classifier output (TrendUp /
// TrendDown / Range / Chop) combined with the MA cascade /
// RSI / MACD / breakout / volume signals.
//
// Where the FundamentalsAnalyst answers "is this company
// worth X?", the TechnicalAnalyst answers "is this price
// going up or down in the next N bars?".
//
// Well-known Signals keys this analyst hard-votes on (each is a
// float64; missing keys are silently skipped):
//
//   - "ma50_over_ma200"           — +1 / -1 directional vote
//   - "macd_hist"                 — sign drives directional vote
//   - "rsi14"                     — extremes (≥70 / ≤30) vote
//     against / with reversion
//   - "breakout"                  — +1 (close above prior 20-bar
//     high), -1 (close below prior 20-bar low) directional vote
//   - "relative_volume"           — confirmation / dilution
//     post-pass: ≥1.5 amplifies the existing direction; ≤0.5
//     drags conviction without voting
//   - "support_distance_pct"      — informational (findings)
//   - "resistance_distance_pct"   — informational (risks)
//
// indicator.Snapshot.AsAnalystSignals() is the canonical builder
// for this map; wiring layers should prefer it over hand-rolled
// key strings to avoid drift.

package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// TechnicalAnalyst implements AnalystAgent for price-action /
// technical-indicator reads. Construct via NewTechnicalAnalyst.
type TechnicalAnalyst struct {
	*analystBase
}

// NewTechnicalAnalyst constructs a TechnicalAnalyst.
func NewTechnicalAnalyst(id, name, fundID string, llm LLMClient, opts ...AnalystOption) *TechnicalAnalyst {
	return &TechnicalAnalyst{analystBase: newAnalystBase(id, name, fundID, llm, opts...)}
}

// Category returns CategoryTechnical.
func (a *TechnicalAnalyst) Category() AnalystCategory { return CategoryTechnical }

// Analyze produces a technical-focused AnalystReport.
//
// Direction blends:
//   - the regime tag (TrendUp = +1, TrendDown = -1, Range / Chop = 0)
//   - the MA cascade signal ("ma50_over_ma200" > 0 → +1, < 0 → -1)
//   - the MACD histogram sign
//
// Confidence scales with how many signals agree.
func (a *TechnicalAnalyst) Analyze(ctx context.Context, input AnalystInput) (AnalystReport, error) {
	if strings.TrimSpace(input.Symbol) == "" {
		return AnalystReport{}, errors.New("technical: input.Symbol required")
	}
	dir, conf := scoreTechnicalDirection(input)
	findings, risks := summariseTechnical(input)

	rep := AnalystReport{
		AgentID:     a.id,
		AgentName:   a.name,
		Category:    CategoryTechnical,
		Symbol:      input.Symbol,
		AsOf:        input.AsOf,
		GeneratedAt: a.now(),
		Direction:   dir,
		Confidence:  conf,
		Thesis:      technicalFallbackThesis(input, dir, conf),
		KeyFindings: findings,
		Risks:       risks,
		DataPoints:  technicalDataPoints(input),
		LLMModel:    "fallback",
	}

	if a.llm != nil {
		sys := a.buildSystemPrompt()
		user := a.buildUserPrompt(input, dir, conf)
		if parsed, err := a.callLLMForReport(ctx, sys, user); err == nil {
			rep.Direction = mergeDirectionWithRule(dir, normaliseDirection(parsed.Direction))
			if parsed.Confidence > 0 {
				rep.Confidence = clampConfidence(parsed.Confidence)
			}
			if t := strings.TrimSpace(parsed.Thesis); t != "" {
				rep.Thesis = t
			}
			if len(parsed.KeyFindings) > 0 {
				rep.KeyFindings = parsed.KeyFindings
			}
			if len(parsed.Risks) > 0 {
				rep.Risks = parsed.Risks
			}
			rep.LLMModel = "llm"
		} else {
			a.logger.Warn("technical analyst: LLM failed, using fallback",
				"err", err, "symbol", input.Symbol)
		}
	}

	if err := rep.Validate(); err != nil {
		return AnalystReport{}, err
	}
	return rep, nil
}

// --- internal helpers ------------------------------------------------------

func scoreTechnicalDirection(input AnalystInput) (Direction, int) {
	if input.Technical == nil {
		return DirectionNeutral, 20
	}
	t := input.Technical
	votes := 0
	totalVotes := 0

	// Regime vote.
	switch strings.ToLower(strings.TrimSpace(t.Snapshot.Regime)) {
	case "trendup", "trend_up":
		votes++
		totalVotes++
	case "trenddown", "trend_down":
		votes--
		totalVotes++
	case "range", "chop":
		totalVotes++ // neutral vote but still a "data point" for conviction
	}

	// MA cascade vote.
	if v, ok := t.Signals["ma50_over_ma200"]; ok {
		totalVotes++
		if v > 0 {
			votes++
		} else if v < 0 {
			votes--
		}
	}

	// MACD histogram vote.
	if v, ok := t.Signals["macd_hist"]; ok {
		totalVotes++
		if v > 0 {
			votes++
		} else if v < 0 {
			votes--
		}
	}

	// RSI vote (only counts the extremes).
	if v, ok := t.Signals["rsi14"]; ok {
		totalVotes++
		switch {
		case v >= 70:
			votes-- // overbought
		case v <= 30:
			votes++ // oversold
		}
	}

	// Breakout vote: +1 = close above the prior 20-bar high;
	// -1 = close below the prior 20-bar low. Range-bound bars
	// pass through 0 and are dropped from the tally entirely
	// (no totalVotes++) — being inside the range isn't a
	// directional fact, just the absence of one.
	if v, ok := t.Signals["breakout"]; ok {
		switch {
		case v > 0:
			totalVotes++
			votes++
		case v < 0:
			totalVotes++
			votes--
		}
	}

	// Volume confirmation / dilution post-pass. Volume is not
	// directional on its own — a surge confirms the direction the
	// other signals already found, while thin volume drags
	// conviction down regardless of which way price moved. We
	// therefore apply RV ONLY after the directional votes are in.
	if v, ok := t.Signals["relative_volume"]; ok && v > 0 {
		switch {
		case v >= 1.5 && votes > 0:
			votes++
			totalVotes++
		case v >= 1.5 && votes < 0:
			votes--
			totalVotes++
		case v <= 0.5:
			// Low-participation tape: count it as "data we have"
			// but cast no directional vote, so |votes|/totalVotes
			// → conviction shrinks.
			totalVotes++
		}
	}

	if totalVotes == 0 {
		return DirectionNeutral, 20
	}
	conf := int(float64(absInt(votes)) / float64(totalVotes) * 100)
	conf = clampConfidence(conf)
	switch {
	case votes > 0:
		return DirectionBullish, conf
	case votes < 0:
		return DirectionBearish, conf
	default:
		return DirectionNeutral, conf
	}
}

func summariseTechnical(input AnalystInput) (findings, risks []string) {
	if input.Technical == nil {
		findings = append(findings, "no quant snapshot attached; analyst sitting out")
		return findings, risks
	}
	t := input.Technical
	if t.Snapshot.Regime != "" {
		findings = append(findings, fmt.Sprintf("regime: %s", t.Snapshot.Regime))
	}
	if t.Snapshot.ATRPct > 0 {
		entry := fmt.Sprintf("ATR%% %.2f (%.2f price units)", t.Snapshot.ATRPct, t.Snapshot.ATR14)
		findings = append(findings, entry)
		if t.Snapshot.ATRPct > 5 {
			risks = append(risks, fmt.Sprintf("elevated vol: ATR%% %.2f > 5", t.Snapshot.ATRPct))
		}
	}
	if t.Snapshot.PositionSizeCeilingPct > 0 {
		findings = append(findings,
			fmt.Sprintf("vol-budget ceiling: %.1f%% of NAV", t.Snapshot.PositionSizeCeilingPct))
	}
	// Render top signals deterministically (sorted by name) so
	// tests can pin the order.
	if len(t.Signals) > 0 {
		keys := make([]string, 0, len(t.Signals))
		for k := range t.Signals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := t.Signals[k]
			switch k {
			case "rsi14":
				switch {
				case v >= 70:
					risks = append(risks, fmt.Sprintf("RSI14 %.0f (overbought)", v))
				case v <= 30:
					findings = append(findings, fmt.Sprintf("RSI14 %.0f (oversold)", v))
				}
			case "macd_hist":
				if v > 0 {
					findings = append(findings, fmt.Sprintf("MACD hist +%.3f", v))
				} else if v < 0 {
					risks = append(risks, fmt.Sprintf("MACD hist %.3f", v))
				}
			case "ma50_over_ma200":
				if v > 0 {
					findings = append(findings, "MA50 > MA200 (golden cross territory)")
				} else if v < 0 {
					risks = append(risks, "MA50 < MA200 (death cross territory)")
				}
			case "breakout":
				if v > 0 {
					findings = append(findings, "breakout above prior 20-bar high")
				} else if v < 0 {
					risks = append(risks, "breakdown below prior 20-bar low")
				}
			case "relative_volume":
				switch {
				case v >= 1.5:
					findings = append(findings, fmt.Sprintf("volume %.2fx 20d avg (surge confirms move)", v))
				case v <= 0.5 && v > 0:
					risks = append(risks, fmt.Sprintf("volume %.2fx 20d avg (low participation)", v))
				}
			case "support_distance_pct":
				// Within ~1.5% of support and not yet broken
				// down → mean-revert long territory.
				if v > 0 && v <= 0.015 {
					findings = append(findings, fmt.Sprintf(
						"price within %.2f%% of 20-bar support", v*100))
				}
			case "resistance_distance_pct":
				// Within ~1.5% of resistance from below →
				// rejection risk.
				if v > 0 && v <= 0.015 {
					risks = append(risks, fmt.Sprintf(
						"price within %.2f%% of 20-bar resistance", v*100))
				}
			}
		}
	}
	if len(findings) == 0 && len(risks) == 0 {
		findings = append(findings, "no clear technical signal in current bar")
	}
	return findings, risks
}

func technicalDataPoints(input AnalystInput) []DataPoint {
	var dp []DataPoint
	if input.Technical == nil {
		return dp
	}
	t := input.Technical
	if t.Snapshot.Regime != "" {
		dp = append(dp, DataPoint{Name: "tech.regime", Value: t.Snapshot.Regime})
	}
	if t.Snapshot.ATR14 > 0 {
		dp = append(dp, DataPoint{Name: "tech.atr14", Value: fmt.Sprintf("%.4f", t.Snapshot.ATR14)})
	}
	if t.Snapshot.ATRPct > 0 {
		dp = append(dp, DataPoint{Name: "tech.atr_pct", Value: fmt.Sprintf("%.2f", t.Snapshot.ATRPct)})
	}
	if t.Snapshot.PositionSizeCeilingPct > 0 {
		dp = append(dp, DataPoint{
			Name:  "tech.size_ceiling_pct",
			Value: fmt.Sprintf("%.2f", t.Snapshot.PositionSizeCeilingPct),
		})
	}
	keys := make([]string, 0, len(t.Signals))
	for k := range t.Signals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		dp = append(dp, DataPoint{Name: "tech.sig." + k, Value: fmt.Sprintf("%.4f", t.Signals[k])})
	}
	return dp
}

func technicalFallbackThesis(input AnalystInput, dir Direction, conf int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Technical read on %s: %s (confidence %d%%). ", input.Symbol, dir, conf)
	if input.Technical == nil {
		b.WriteString("No quant snapshot available.")
		return b.String()
	}
	t := input.Technical
	if t.Snapshot.Regime != "" {
		fmt.Fprintf(&b, "Regime is %s. ", t.Snapshot.Regime)
	}
	if t.Snapshot.ATRPct > 0 {
		fmt.Fprintf(&b, "Realised vol (ATR14 / close) is %.2f%%.", t.Snapshot.ATRPct)
	}
	return b.String()
}

func (a *TechnicalAnalyst) buildSystemPrompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a technical analyst on fund %s. ", a.name, a.fundID)
	b.WriteString("Read the quant snapshot (regime + ATR + size ceiling) and the pre-computed signals ")
	b.WriteString("(RSI / MACD / MA cascade). Call the directional bias for the next ~5–20 bars. ")
	b.WriteString("Ignore fundamentals and news — those are other analysts' jobs.")
	if a.persona != "" {
		fmt.Fprintf(&b, " Persona: %s.", a.persona)
	}
	b.WriteString("\n\nReturn ONLY a JSON object with this exact shape, no markdown:")
	b.WriteString(`
{
  "direction": "bullish" | "bearish" | "neutral",
  "confidence": <int 0-100>,
  "thesis": "<one-paragraph>",
  "key_findings": ["<bullet>", ...],
  "risks": ["<bullet>", ...]
}`)
	return b.String()
}

func (a *TechnicalAnalyst) buildUserPrompt(input AnalystInput, dir Direction, conf int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Symbol: %s\n", input.Symbol)
	fmt.Fprintf(&b, "Rule-based prior: %s, confidence %d%%\n\n", dir, conf)
	if input.Technical == nil {
		b.WriteString("No quant snapshot attached.\n")
		return b.String()
	}
	t := input.Technical
	if t.Snapshot.Regime != "" {
		fmt.Fprintf(&b, "Regime: %s (close %.4f, ATR14 %.4f / %.2f%%, vol-budget ceiling %.1f%% NAV)\n",
			t.Snapshot.Regime, t.Snapshot.Close, t.Snapshot.ATR14, t.Snapshot.ATRPct,
			t.Snapshot.PositionSizeCeilingPct)
	}
	if len(t.Signals) > 0 {
		b.WriteString("Signals:\n")
		keys := make([]string, 0, len(t.Signals))
		for k := range t.Signals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "  - %s = %.4f\n", k, t.Signals[k])
		}
	}
	if len(t.PriceHistorySpark) > 0 {
		b.WriteString("Last closes (oldest → newest):\n  ")
		for i, p := range t.PriceHistorySpark {
			if i > 0 {
				b.WriteString(" ")
			}
			fmt.Fprintf(&b, "%.2f", p)
		}
		b.WriteString("\n")
	}
	if strings.TrimSpace(input.Notes) != "" {
		fmt.Fprintf(&b, "Operator notes: %s\n", input.Notes)
	}
	return b.String()
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
