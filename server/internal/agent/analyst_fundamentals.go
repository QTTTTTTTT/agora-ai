// analyst_fundamentals.go — S8.1 FundamentalsAnalyst.
//
// Reads AnalystInput.Fundamentals (quality z-scores + reported
// PE/PB/ROE/etc.) and writes a structured report keyed on whether
// the symbol's intrinsic value supports the price. This analyst
// intentionally ignores news / sentiment / technicals — the
// fan-out architecture means other analysts cover those.

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// FundamentalsAnalyst implements AnalystAgent for company-level
// financials. ZeroValue is not usable; build with
// NewFundamentalsAnalyst.
type FundamentalsAnalyst struct {
	*analystBase
}

// NewFundamentalsAnalyst constructs a FundamentalsAnalyst. llm
// may be nil — the analyst then falls back to the deterministic
// rule path so unit tests + offline runs still produce a report.
func NewFundamentalsAnalyst(id, name, fundID string, llm LLMClient, opts ...AnalystOption) *FundamentalsAnalyst {
	return &FundamentalsAnalyst{analystBase: newAnalystBase(id, name, fundID, llm, opts...)}
}

// Category returns CategoryFundamentals.
func (a *FundamentalsAnalyst) Category() AnalystCategory { return CategoryFundamentals }

// Analyze produces a fundamentals-focused AnalystReport. The
// scoring uses the quality composite z-score as primary anchor:
//
//	CompositeZ ≥ +0.5  → bullish
//	CompositeZ ≤ -0.5  → bearish
//	otherwise           → neutral
//
// Confidence scales with |CompositeZ|, capped at 100. The LLM
// thesis is consulted on top but the directional anchor is the
// number, not the LLM — this prevents narrative drift.
func (a *FundamentalsAnalyst) Analyze(ctx context.Context, input AnalystInput) (AnalystReport, error) {
	if strings.TrimSpace(input.Symbol) == "" {
		return AnalystReport{}, errors.New("fundamentals: input.Symbol required")
	}
	dir, conf := scoreFundamentalsDirection(input)
	keyFindings, risks := summariseFundamentals(input)

	rep := AnalystReport{
		AgentID:     a.id,
		AgentName:   a.name,
		Category:    CategoryFundamentals,
		Symbol:      input.Symbol,
		AsOf:        input.AsOf,
		GeneratedAt: a.now(),
		Direction:   dir,
		Confidence:  conf,
		Thesis:      fundamentalsFallbackThesis(input, dir, conf),
		KeyFindings: keyFindings,
		Risks:       risks,
		DataPoints:  fundamentalsDataPoints(input),
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
			a.logger.Warn("fundamentals analyst: LLM failed, using fallback",
				"err", err, "symbol", input.Symbol)
		}
	}

	if input.Fundamentals != nil && strings.TrimSpace(input.Fundamentals.FilingsURL) != "" {
		rep.Sources = append(rep.Sources, input.Fundamentals.FilingsURL)
	}
	if err := rep.Validate(); err != nil {
		return AnalystReport{}, err
	}
	return rep, nil
}

// --- internal helpers ------------------------------------------------------

func scoreFundamentalsDirection(input AnalystInput) (Direction, int) {
	if input.Fundamentals == nil || input.Fundamentals.QualityScore == nil {
		// No quality data → neutral with the floor.
		return DirectionNeutral, 20
	}
	z := input.Fundamentals.QualityScore.CompositeZ
	conf := int(absFloat(z) * 50) // 1σ → 50, 2σ → 100
	conf = clampConfidence(conf)
	switch {
	case z >= 0.5:
		return DirectionBullish, conf
	case z <= -0.5:
		return DirectionBearish, conf
	default:
		return DirectionNeutral, conf
	}
}

func summariseFundamentals(input AnalystInput) (findings, risks []string) {
	if input.Fundamentals == nil {
		findings = append(findings, "no fundamentals data available; analyst sitting out")
		return findings, risks
	}
	q := input.Fundamentals.QualityScore
	if q != nil {
		if q.ProfitabilityZ >= 0.5 {
			findings = append(findings, fmt.Sprintf("profitability z=%.2f (above peers)", q.ProfitabilityZ))
		} else if q.ProfitabilityZ <= -0.5 {
			risks = append(risks, fmt.Sprintf("profitability z=%.2f (below peers)", q.ProfitabilityZ))
		}
		if q.GrowthZ >= 0.5 {
			findings = append(findings, fmt.Sprintf("growth z=%.2f (above peers)", q.GrowthZ))
		} else if q.GrowthZ <= -0.5 {
			risks = append(risks, fmt.Sprintf("growth z=%.2f (below peers)", q.GrowthZ))
		}
		if q.SafetyZ >= 0.5 {
			findings = append(findings, fmt.Sprintf("safety z=%.2f (less leverage than peers)", q.SafetyZ))
		} else if q.SafetyZ <= -0.5 {
			risks = append(risks, fmt.Sprintf("safety z=%.2f (more leverage than peers)", q.SafetyZ))
		}
		if q.Quartile > 0 {
			findings = append(findings, fmt.Sprintf("quality quartile %d/4", q.Quartile))
		}
	}
	for k, v := range input.Fundamentals.Metrics {
		switch strings.ToLower(k) {
		case "pe":
			if v > 0 && v < 12 {
				findings = append(findings, fmt.Sprintf("PE %.1f (cheap)", v))
			} else if v > 35 {
				risks = append(risks, fmt.Sprintf("PE %.1f (rich)", v))
			}
		case "pb":
			if v > 0 && v < 1 {
				findings = append(findings, fmt.Sprintf("PB %.2f (below book)", v))
			} else if v > 8 {
				risks = append(risks, fmt.Sprintf("PB %.2f (extreme premium)", v))
			}
		case "debt_to_equity":
			if v > 2.0 {
				risks = append(risks, fmt.Sprintf("debt/equity %.2f (highly levered)", v))
			}
		case "roe":
			if v >= 0.20 {
				findings = append(findings, fmt.Sprintf("ROE %.0f%%", v*100))
			} else if v < 0 {
				risks = append(risks, fmt.Sprintf("ROE %.0f%% (loss)", v*100))
			}
		}
	}
	if len(findings) == 0 {
		findings = append(findings, "fundamentals roughly in line with peers")
	}
	return findings, risks
}

func fundamentalsDataPoints(input AnalystInput) []DataPoint {
	var dp []DataPoint
	if input.Fundamentals == nil {
		return dp
	}
	if q := input.Fundamentals.QualityScore; q != nil {
		dp = append(dp,
			DataPoint{Name: "quality.composite_z", Value: fmt.Sprintf("%.2f", q.CompositeZ)},
			DataPoint{Name: "quality.profitability_z", Value: fmt.Sprintf("%.2f", q.ProfitabilityZ)},
			DataPoint{Name: "quality.growth_z", Value: fmt.Sprintf("%.2f", q.GrowthZ)},
			DataPoint{Name: "quality.safety_z", Value: fmt.Sprintf("%.2f", q.SafetyZ)},
		)
		if q.Quartile > 0 {
			dp = append(dp, DataPoint{Name: "quality.quartile", Value: fmt.Sprintf("%d", q.Quartile)})
		}
	}
	for k, v := range input.Fundamentals.Metrics {
		dp = append(dp, DataPoint{Name: "fund." + k, Value: fmt.Sprintf("%.4f", v)})
	}
	return dp
}

func fundamentalsFallbackThesis(input AnalystInput, dir Direction, conf int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fundamentals view on %s: %s (confidence %d%%). ", input.Symbol, dir, conf)
	if input.Fundamentals == nil || input.Fundamentals.QualityScore == nil {
		b.WriteString("Quality data unavailable; analyst defers to peers.")
		return b.String()
	}
	q := input.Fundamentals.QualityScore
	fmt.Fprintf(&b, "Composite quality z=%.2f (profitability %.2f, growth %.2f, safety %.2f).",
		q.CompositeZ, q.ProfitabilityZ, q.GrowthZ, q.SafetyZ)
	return b.String()
}

func (a *FundamentalsAnalyst) buildSystemPrompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a fundamentals-focused equity analyst on fund %s. ", a.name, a.fundID)
	b.WriteString("Read the company's reported financials and peer-relative quality z-scores; ")
	b.WriteString("output a one-paragraph thesis grounded in those numbers. Avoid hedge phrases. ")
	b.WriteString("Never cite news headlines or technical signals — those are other analysts' jobs.")
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

func (a *FundamentalsAnalyst) buildUserPrompt(input AnalystInput, dir Direction, conf int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Symbol: %s (%s / %s)\n", input.Symbol, input.Market, input.AssetClass)
	fmt.Fprintf(&b, "Rule-based prior: %s, confidence %d%%\n\n", dir, conf)
	if input.Fundamentals == nil {
		b.WriteString("No fundamentals data attached.\n")
	} else {
		f := input.Fundamentals
		if f.QualityScore != nil {
			q := f.QualityScore
			fmt.Fprintf(&b, "Quality composite z: %.2f (profitability %.2f, growth %.2f, safety %.2f, quartile %d)\n",
				q.CompositeZ, q.ProfitabilityZ, q.GrowthZ, q.SafetyZ, q.Quartile)
		}
		if len(f.Metrics) > 0 {
			b.WriteString("Reported metrics:\n")
			for k, v := range f.Metrics {
				fmt.Fprintf(&b, "  - %s = %.4f\n", k, v)
			}
		}
		if len(f.IndustryPeers) > 0 {
			fmt.Fprintf(&b, "Industry peers: %s\n", strings.Join(f.IndustryPeers, ", "))
		}
		if f.FilingsURL != "" {
			fmt.Fprintf(&b, "Latest filing: %s\n", f.FilingsURL)
		}
	}
	if strings.TrimSpace(input.Notes) != "" {
		fmt.Fprintf(&b, "Operator notes: %s\n", input.Notes)
	}
	return b.String()
}
