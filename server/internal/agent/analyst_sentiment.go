// analyst_sentiment.go — S8.1 SentimentAnalyst.
//
// Reads AnalystInput.Sentiment (pre-scored items + per-source
// breakdown) and produces a crowd-mood call. The directional
// anchor is the aggregate average; the LLM thesis is consulted
// for narrative but cannot override the sign of the numbers.

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SentimentAnalyst implements AnalystAgent for crowd / news
// mood. Construct via NewSentimentAnalyst.
type SentimentAnalyst struct {
	*analystBase
}

// NewSentimentAnalyst constructs a SentimentAnalyst.
func NewSentimentAnalyst(id, name, fundID string, llm LLMClient, opts ...AnalystOption) *SentimentAnalyst {
	return &SentimentAnalyst{analystBase: newAnalystBase(id, name, fundID, llm, opts...)}
}

// Category returns CategorySentiment.
func (a *SentimentAnalyst) Category() AnalystCategory { return CategorySentiment }

// Analyze produces a sentiment-focused AnalystReport. The
// aggregate average drives the direction:
//
//	avg ≥ +0.2 → bullish
//	avg ≤ -0.2 → bearish
//	otherwise   → neutral
//
// Confidence scales with |average| and is dampened when the
// item count is small (< 5) — crowd mood with few data points
// is unreliable.
func (a *SentimentAnalyst) Analyze(ctx context.Context, input AnalystInput) (AnalystReport, error) {
	if strings.TrimSpace(input.Symbol) == "" {
		return AnalystReport{}, errors.New("sentiment: input.Symbol required")
	}
	dir, conf := scoreSentimentDirection(input)
	findings, risks := summariseSentiment(input)

	rep := AnalystReport{
		AgentID:     a.id,
		AgentName:   a.name,
		Category:    CategorySentiment,
		Symbol:      input.Symbol,
		AsOf:        input.AsOf,
		GeneratedAt: a.now(),
		Direction:   dir,
		Confidence:  conf,
		Thesis:      sentimentFallbackThesis(input, dir, conf),
		KeyFindings: findings,
		Risks:       risks,
		DataPoints:  sentimentDataPoints(input),
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
			a.logger.Warn("sentiment analyst: LLM failed, using fallback",
				"err", err, "symbol", input.Symbol)
		}
	}

	if input.Sentiment != nil {
		for _, item := range input.Sentiment.RecentItems {
			if item.URL != "" {
				rep.Sources = append(rep.Sources, item.URL)
				if len(rep.Sources) >= 5 {
					break
				}
			}
		}
	}
	if err := rep.Validate(); err != nil {
		return AnalystReport{}, err
	}
	return rep, nil
}

// --- internal helpers ------------------------------------------------------

func scoreSentimentDirection(input AnalystInput) (Direction, int) {
	if input.Sentiment == nil || input.Sentiment.Aggregate.Count == 0 {
		return DirectionNeutral, 20
	}
	avg := input.Sentiment.Aggregate.Average
	conf := int(absFloat(avg) * 100) // |avg|=1 → 100
	if input.Sentiment.Aggregate.Count < 5 {
		// Few items → halve the conviction.
		conf /= 2
	}
	conf = clampConfidence(conf)
	switch {
	case avg >= 0.2:
		return DirectionBullish, conf
	case avg <= -0.2:
		return DirectionBearish, conf
	default:
		return DirectionNeutral, conf
	}
}

func summariseSentiment(input AnalystInput) (findings, risks []string) {
	if input.Sentiment == nil || input.Sentiment.Aggregate.Count == 0 {
		findings = append(findings, "no sentiment items in window; analyst sitting out")
		return findings, risks
	}
	agg := input.Sentiment.Aggregate
	findings = append(findings, fmt.Sprintf("aggregate mood %s (avg %.2f over %d items)",
		agg.Polarity, agg.Average, agg.Count))

	if len(input.Sentiment.SourceBreakdown) > 0 {
		// Flag single-source bias: if one source dominates >70%
		// of items, the crowd mood is really one outlet's mood.
		total := 0
		var topSrc string
		var topCount int
		for src, n := range input.Sentiment.SourceBreakdown {
			total += n
			if n > topCount {
				topSrc, topCount = src, n
			}
		}
		if total > 0 && float64(topCount)/float64(total) > 0.7 {
			risks = append(risks,
				fmt.Sprintf("source bias: %d/%d items from %s only",
					topCount, total, topSrc))
		}
	}

	// Surface the single strongest item per side as a finding.
	var topPos, topNeg *SentimentItemLite
	for i := range input.Sentiment.RecentItems {
		it := &input.Sentiment.RecentItems[i]
		if topPos == nil || it.Score > topPos.Score {
			topPos = it
		}
		if topNeg == nil || it.Score < topNeg.Score {
			topNeg = it
		}
	}
	if topPos != nil && topPos.Score > 0.4 {
		findings = append(findings,
			fmt.Sprintf("strongest bull: %q (%.2f, %s)",
				truncateRunes(topPos.Title, 80), topPos.Score, topPos.Source))
	}
	if topNeg != nil && topNeg.Score < -0.4 {
		risks = append(risks,
			fmt.Sprintf("strongest bear: %q (%.2f, %s)",
				truncateRunes(topNeg.Title, 80), topNeg.Score, topNeg.Source))
	}
	return findings, risks
}

func sentimentDataPoints(input AnalystInput) []DataPoint {
	var dp []DataPoint
	if input.Sentiment == nil {
		return dp
	}
	agg := input.Sentiment.Aggregate
	if agg.Count > 0 {
		dp = append(dp,
			DataPoint{Name: "sentiment.avg", Value: fmt.Sprintf("%.3f", agg.Average)},
			DataPoint{Name: "sentiment.count", Value: fmt.Sprintf("%d", agg.Count)},
			DataPoint{Name: "sentiment.polarity", Value: agg.Polarity},
		)
	}
	for src, n := range input.Sentiment.SourceBreakdown {
		dp = append(dp, DataPoint{Name: "sentiment.src." + src, Value: fmt.Sprintf("%d", n)})
	}
	return dp
}

func sentimentFallbackThesis(input AnalystInput, dir Direction, conf int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Sentiment read on %s: %s (confidence %d%%). ", input.Symbol, dir, conf)
	if input.Sentiment == nil || input.Sentiment.Aggregate.Count == 0 {
		b.WriteString("No social / news items in the window.")
		return b.String()
	}
	agg := input.Sentiment.Aggregate
	fmt.Fprintf(&b, "Crowd is %s on average (%.2f over %d items).",
		agg.Polarity, agg.Average, agg.Count)
	return b.String()
}

func (a *SentimentAnalyst) buildSystemPrompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a sentiment-focused analyst on fund %s. ", a.name, a.fundID)
	b.WriteString("Read the aggregate crowd mood + the strongest items, and call the directional bias. ")
	b.WriteString("Flag source bias when one outlet dominates. Avoid commentary on financials or technicals.")
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

func (a *SentimentAnalyst) buildUserPrompt(input AnalystInput, dir Direction, conf int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Symbol: %s\n", input.Symbol)
	fmt.Fprintf(&b, "Rule-based prior: %s, confidence %d%%\n\n", dir, conf)
	if input.Sentiment == nil || input.Sentiment.Aggregate.Count == 0 {
		b.WriteString("No sentiment items in window.\n")
		return b.String()
	}
	agg := input.Sentiment.Aggregate
	fmt.Fprintf(&b, "Aggregate: %s (avg %.2f over %d items)\n", agg.Polarity, agg.Average, agg.Count)
	if len(input.Sentiment.SourceBreakdown) > 0 {
		b.WriteString("Source breakdown:\n")
		for src, n := range input.Sentiment.SourceBreakdown {
			fmt.Fprintf(&b, "  - %s: %d\n", src, n)
		}
	}
	if len(input.Sentiment.RecentItems) > 0 {
		b.WriteString("Recent items (most-recent first):\n")
		for _, it := range input.Sentiment.RecentItems {
			fmt.Fprintf(&b, "  - [%.2f] %s (%s)\n", it.Score, truncateRunes(it.Title, 100), it.Source)
		}
	}
	if strings.TrimSpace(input.Notes) != "" {
		fmt.Fprintf(&b, "Operator notes: %s\n", input.Notes)
	}
	return b.String()
}
