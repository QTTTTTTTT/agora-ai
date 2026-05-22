package sentiment

import (
	"fmt"
	"sort"
	"strings"
)

// AggregateBySymbol rolls scores up per symbol. Items contributing
// to a symbol's bucket are: every Item that lists the symbol in
// Symbols, plus the global "MARKET" bucket which captures every
// item.
//
// Score weighting: scores are weighted by Confidence (so a
// high-confidence -0.8 outweighs ten weak +0.1's), then averaged.
// Reasons capture the top-3 most extreme |score| items so the LLM
// PM has concrete catalysts.
func AggregateBySymbol(scores []Score, items []Item) []AggregateScore {
	byScore := make(map[string]Score, len(scores))
	for _, s := range scores {
		byScore[s.ID] = s
	}
	buckets := make(map[string]*aggregatorState)
	for _, item := range items {
		score, ok := byScore[item.ID]
		if !ok {
			continue
		}
		// Always contribute to the MARKET bucket.
		mark := bucketState(buckets, "MARKET")
		mark.add(score, item)
		for _, sym := range item.Symbols {
			sym = strings.TrimSpace(sym)
			if sym == "" {
				continue
			}
			b := bucketState(buckets, strings.ToUpper(sym))
			b.add(score, item)
		}
	}
	out := make([]AggregateScore, 0, len(buckets))
	for scope, st := range buckets {
		out = append(out, AggregateScore{
			Scope:    scope,
			Count:    st.count,
			Average:  st.average(),
			Polarity: polarityLabel(st.average()),
			Reasons:  st.topReasons(3),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Scope == "MARKET" {
			return true
		}
		if out[j].Scope == "MARKET" {
			return false
		}
		// Sort symbol buckets by absolute polarity descending so
		// the most "loud" symbols come first.
		ai := out[i].Average
		if ai < 0 {
			ai = -ai
		}
		aj := out[j].Average
		if aj < 0 {
			aj = -aj
		}
		if ai != aj {
			return ai > aj
		}
		return out[i].Scope < out[j].Scope
	})
	return out
}

// aggregatorState tracks a running weighted mean + the most-extreme
// reasons. We don't need numerical stability tricks here — N is
// bounded by the news count (typically <100).
type aggregatorState struct {
	count       int
	weightSum   float64
	weightedSum float64
	picks       []reasonPick
}

type reasonPick struct {
	abs    float64
	reason string
}

func (s *aggregatorState) add(score Score, item Item) {
	s.count++
	w := score.Confidence
	if w <= 0 {
		w = 0.1
	}
	s.weightSum += w
	s.weightedSum += w * score.Score
	if score.Reason == "" {
		return
	}
	abs := score.Score
	if abs < 0 {
		abs = -abs
	}
	headline := strings.TrimSpace(item.Title)
	if headline == "" {
		headline = strings.TrimSpace(item.Summary)
	}
	if headline == "" {
		headline = "(untitled)"
	}
	s.picks = append(s.picks, reasonPick{
		abs:    abs,
		reason: fmt.Sprintf("%s [%+.2f] — %s", headline, score.Score, score.Reason),
	})
}

func (s *aggregatorState) average() float64 {
	if s.weightSum == 0 {
		return 0
	}
	return s.weightedSum / s.weightSum
}

func (s *aggregatorState) topReasons(n int) []string {
	if len(s.picks) == 0 {
		return nil
	}
	sort.SliceStable(s.picks, func(i, j int) bool {
		return s.picks[i].abs > s.picks[j].abs
	})
	if n > len(s.picks) {
		n = len(s.picks)
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, s.picks[i].reason)
	}
	return out
}

func bucketState(m map[string]*aggregatorState, key string) *aggregatorState {
	if v, ok := m[key]; ok {
		return v
	}
	v := &aggregatorState{}
	m[key] = v
	return v
}

// FormatForPrompt produces a compact multi-line block for the LLM
// PM and the debate Bull/Bear agents. Layout:
//
//   News sentiment (12 items): market neutral (avg +0.04).
//   - AAPL bullish (+0.42, 5 items)
//     · "Apple beats Q2 earnings on Services growth" [+0.65] — Strong beat with raised guidance
//     · "iPhone 16 demand strong in Asia" [+0.40] — Re-acceleration thesis
//   - TSLA bearish (-0.31, 3 items)
//     · "Tesla recalls 2.4M vehicles over Autopilot" [-0.55] — Regulatory overhang
func FormatForPrompt(aggregates []AggregateScore, totalItems int) string {
	if len(aggregates) == 0 || totalItems == 0 {
		return ""
	}
	var sb strings.Builder
	market := findMarket(aggregates)
	if market != nil {
		sb.WriteString(fmt.Sprintf("News sentiment (%d items): market %s (avg %+.2f).",
			totalItems, market.Polarity, market.Average))
	} else {
		sb.WriteString(fmt.Sprintf("News sentiment (%d items):", totalItems))
	}
	wroteAny := false
	for _, agg := range aggregates {
		if agg.Scope == "MARKET" {
			continue
		}
		if !wroteAny {
			sb.WriteString("\n")
			wroteAny = true
		}
		sb.WriteString(fmt.Sprintf("\n- %s %s (%+.2f, %d items)",
			agg.Scope, agg.Polarity, agg.Average, agg.Count))
		for _, reason := range agg.Reasons {
			sb.WriteString("\n  · ")
			sb.WriteString(reason)
		}
	}
	return sb.String()
}

func findMarket(aggs []AggregateScore) *AggregateScore {
	for i := range aggs {
		if aggs[i].Scope == "MARKET" {
			return &aggs[i]
		}
	}
	return nil
}
