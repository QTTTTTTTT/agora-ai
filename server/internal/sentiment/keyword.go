package sentiment

import (
	"context"
	"math"
	"strings"
)

// KeywordScorer is a deterministic, dependency-free scorer used as
// the fallback when no LLM client is configured. It walks each
// item's title+summary and counts hits against a small bilingual
// lexicon. The score is a tanh-normalised (positives - negatives)
// so the magnitude saturates around -1..+1 even when a long
// article hits many keywords.
//
// The lexicon is small on purpose: this is meant to surface a
// directional signal, not pretend to be a real NLP model. For real
// scoring, run LLMScorer or chain via CompositeScorer.
type KeywordScorer struct {
	// PositiveTerms / NegativeTerms override the built-in
	// lexicon. Both are case-insensitive.
	PositiveTerms []string
	NegativeTerms []string
}

// Score implements Scorer.
func (s *KeywordScorer) Score(_ context.Context, items []Item) ([]Score, error) {
	pos := s.PositiveTerms
	if len(pos) == 0 {
		pos = defaultPositive
	}
	neg := s.NegativeTerms
	if len(neg) == 0 {
		neg = defaultNegative
	}
	out := make([]Score, 0, len(items))
	for _, item := range items {
		text := strings.ToLower(titleSummary(item))
		if text == "" {
			out = append(out, Score{ID: item.ID, Confidence: 0.1})
			continue
		}
		posHits, posTags := countHits(text, pos)
		negHits, negTags := countHits(text, neg)
		net := posHits - negHits
		raw := math.Tanh(float64(net) / 2.0)

		// Confidence: how strongly the lexicon was triggered. We
		// take min(hits, 5)/5 so 5+ hits saturates at 1.0. Zero
		// hits → confidence 0.1 (neutral but low certainty).
		hits := posHits + negHits
		conf := float64(hits) / 5.0
		if conf > 1 {
			conf = 1
		}
		if hits == 0 {
			conf = 0.1
		}

		tags := append([]string{}, posTags...)
		tags = append(tags, negTags...)
		out = append(out, Score{
			ID:         item.ID,
			Score:      clampScore(raw),
			Confidence: clampConfidence(conf),
			Tags:       tags,
		})
	}
	return out, nil
}

// countHits returns (count, terms_found) for terms appearing in text.
func countHits(textLower string, terms []string) (int, []string) {
	count := 0
	found := []string{}
	for _, term := range terms {
		t := strings.ToLower(strings.TrimSpace(term))
		if t == "" {
			continue
		}
		if strings.Contains(textLower, t) {
			count++
			found = append(found, t)
		}
	}
	return count, found
}

// Bilingual lexicons. Keep these small and obvious; the goal is a
// signal not perfect NLP. Operators can override per-fund via
// KeywordScorer.PositiveTerms / NegativeTerms.
var defaultPositive = []string{
	// English
	"beat", "beats", "exceeded", "outperform", "outperforms", "rally",
	"surge", "surges", "soar", "soars", "record high", "strong demand",
	"upgrade", "upgrades", "raised guidance", "raised forecast",
	"buyback", "dividend hike", "bullish",
	// Chinese
	"利好", "上涨", "新高", "买入", "增持", "看多", "回升", "增长", "突破", "盈利",
	"提价", "涨停", "强劲", "超预期", "回购", "分红", "复苏", "增发",
}

var defaultNegative = []string{
	// English
	"miss", "missed", "underperform", "downgrade", "downgrades",
	"sell-off", "selloff", "plunge", "plunges", "tumble", "tumbles",
	"crash", "loss", "losses", "lawsuit", "investigation", "probe",
	"recall", "warning", "halts", "bearish", "default",
	// Chinese
	"利空", "下跌", "暴跌", "亏损", "卖出", "减持", "看空", "下调", "跌停",
	"调查", "处罚", "诉讼", "停牌", "退市", "违约", "破产", "风险",
}
