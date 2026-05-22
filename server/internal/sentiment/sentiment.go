// Package sentiment scores news items along a -1..+1 axis so the
// debate's Bull / Bear agents and the LLM PM can reason over
// concrete "the news flow today is bullish on Tech (+0.42), bearish
// on Energy (-0.31)" signals instead of staring at raw headlines.
//
// Two scorers ship:
//
//   - LLMScorer:     hands a batch of headlines to the configured
//                    LLM, parses a structured JSON response. This is
//                    the production path.
//   - KeywordScorer: pure-Go fallback that runs when no LLM client
//                    is configured (offline / cost-constrained
//                    deployments). It uses a small Chinese+English
//                    positive/negative lexicon.
//
// CompositeScorer chains scorers — LLM first, falling back to
// keyword on error — so wiring stays a one-liner.
//
// Scores follow a simple contract:
//
//	-1.0 .. -0.6  strongly bearish
//	-0.6 .. -0.2  bearish
//	-0.2 .. +0.2  neutral
//	+0.2 .. +0.6  bullish
//	+0.6 .. +1.0  strongly bullish
//
// Magnitude expresses confidence in the direction, not just polarity.
package sentiment

import (
	"context"
	"strings"
	"time"
)

// Item is the input to a scorer. The package intentionally does NOT
// depend on marketdata.NewsItem so the wiring layer can adapt
// arbitrary news shapes; the marketdata adapter is one helper
// function in news_adapter.go.
type Item struct {
	ID          string
	Title       string
	Summary     string
	Source      string
	URL         string
	Language    string
	PublishedAt time.Time
	Symbols     []string
}

// Score is a single news item's verdict. Reason is the LLM's short
// explanation (empty for KeywordScorer). Tags surfaces hits from
// the lexicon for debugging / display.
type Score struct {
	ID         string
	Score      float64
	Confidence float64
	Reason     string
	Tags       []string
}

// AggregateScore is the per-symbol or per-market roll-up. Average
// is the mean of Item.Score weighted by Confidence; Polarity is
// "bullish" / "bearish" / "neutral" derived from Average.
type AggregateScore struct {
	Scope    string
	Count    int
	Average  float64
	Polarity string
	Reasons  []string
}

// Scorer is the abstract per-source scorer.
type Scorer interface {
	Score(ctx context.Context, items []Item) ([]Score, error)
}

// Aggregator groups scored items by scope (symbol / market / sector).
type Aggregator interface {
	Aggregate(scores []Score, items []Item, scope func(Item) string) []AggregateScore
}

// polarityLabel maps a score into the canonical English label that
// the LLM PM prompt expects.
func polarityLabel(score float64) string {
	switch {
	case score >= 0.6:
		return "strongly bullish"
	case score >= 0.2:
		return "bullish"
	case score <= -0.6:
		return "strongly bearish"
	case score <= -0.2:
		return "bearish"
	default:
		return "neutral"
	}
}

// clampScore enforces the -1..+1 contract — LLM outputs occasionally
// wander outside the range and we don't want a single outlier to
// destabilize a downstream weighted mean.
func clampScore(v float64) float64 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

// clampConfidence enforces 0..1.
func clampConfidence(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// titleSummary concatenates the visible fields the scorer cares
// about. We prefer Title + Summary; fall back to either alone when
// one is missing.
func titleSummary(item Item) string {
	title := strings.TrimSpace(item.Title)
	summary := strings.TrimSpace(item.Summary)
	switch {
	case title != "" && summary != "":
		return title + ". " + summary
	case title != "":
		return title
	case summary != "":
		return summary
	default:
		return ""
	}
}
