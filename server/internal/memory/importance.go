// Importance scoring: turns raw memory content + context into an
// Importance score in [0, 1] for use by Recall and reflexion.
package memory

import "strings"

// ImportanceSignals describes the externally observable signals that
// influence how important a memory should be considered. All fields are
// optional — pass zero values when a signal is absent.
type ImportanceSignals struct {
	// DailyReturn is the fund's daily return on the date the memory was
	// produced. Larger absolute values raise importance.
	DailyReturn float64
	// HasFailedRiskCheck flags memories tied to a plan that failed risk.
	HasFailedRiskCheck bool
	// LLMRated is an optional self-rating in [0, 1] from a small LLM call
	// asking "how memorable is this on a scale of 0..1?". Pass 0 when not
	// available; the function falls back to heuristics.
	LLMRated float64
	// Tags are the memory's tags. Specific tags carry boosts (see
	// importanceTagBoosts).
	Tags []string
}

// importanceTagBoosts is the table of recognised tag boosts. Tags not in the
// table contribute zero, which means callers can safely tag freely without
// inflating scores.
var importanceTagBoosts = map[string]float64{
	"self_learning": 0.15,
	"risk":          0.20,
	"rejection":     0.20,
	"circuit_breaker": 0.30,
	"event":         0.10,
	"earnings":      0.10,
	"macro":         0.08,
}

// ScoreImportance combines the signals into a deterministic [0,1] score.
// The weights are intentionally simple so the formula is auditable and
// reproducible in tests.
//
//	score = clamp(
//	    0.40 * f(|daily_return|) +
//	    0.30 * tagBoost +
//	    0.20 * llmRated +
//	    0.10 * riskFailureFlag,
//	    0.0, 1.0,
//	)
//
// f(|r|) saturates at |r| ≥ 5% so that an outlier day doesn't drown the
// other signals. Returns 0.5 (the neutral prior) when every signal is
// absent — that prevents brand-new memories from being immediately
// down-ranked into oblivion.
func ScoreImportance(s ImportanceSignals) float64 {
	hasSignal := s.DailyReturn != 0 || s.HasFailedRiskCheck || s.LLMRated > 0 || len(s.Tags) > 0
	if !hasSignal {
		return 0.5
	}
	r := absFloat(s.DailyReturn) / 0.05
	if r > 1 {
		r = 1
	}
	tagBoost := 0.0
	for _, tag := range s.Tags {
		if b, ok := importanceTagBoosts[strings.ToLower(tag)]; ok {
			tagBoost += b
		}
	}
	if tagBoost > 1 {
		tagBoost = 1
	}
	risk := 0.0
	if s.HasFailedRiskCheck {
		risk = 1
	}
	llm := s.LLMRated
	if llm < 0 {
		llm = 0
	}
	if llm > 1 {
		llm = 1
	}
	score := 0.40*r + 0.30*tagBoost + 0.20*llm + 0.10*risk
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}

func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
