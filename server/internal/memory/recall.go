// Recall implements similarity-aware memory retrieval. The scoring formula
// is taken from the Generative Agents paper (Park et al. 2023):
//
//	score = w_recency * recency_decay +
//	        w_importance * importance +
//	        w_similarity * cosine_similarity
//
// Recency decays exponentially with age; the half-life is configurable so
// callers can build "short-term" recall (hours) or "long-term" recall
// (weeks) from the same primitive.
package memory

import (
	"errors"
	"math"
	"sort"
	"time"
)

// RecallWeights configures the relative importance of recency, importance
// and similarity. All weights must be ≥ 0; any combination works as long as
// at least one is positive. Callers usually want non-trivial weights for
// all three.
type RecallWeights struct {
	Recency    float64
	Importance float64
	Similarity float64
}

// DefaultRecallWeights mirrors the Generative Agents defaults (1, 1, 1)
// with a slight emphasis on similarity which we found empirically gives
// PMAgent better thesis context.
var DefaultRecallWeights = RecallWeights{Recency: 1.0, Importance: 1.0, Similarity: 1.5}

// RecallParams configures a recall query.
type RecallParams struct {
	Now            time.Time
	Query          Embedding     // optional; when nil, similarity component is 0
	Weights        RecallWeights // zero-value means use DefaultRecallWeights
	HalfLifeHours  float64       // recency half-life; default 72h (3 days)
	TopK           int           // 0 -> return all
	MinScore       float64       // discard candidates below this score
}

// ErrNoCandidates is returned by Recall when items is empty.
var ErrNoCandidates = errors.New("memory: no candidates")

// Recall ranks items by the weighted recency × importance × similarity
// score and returns the top-K. Items are not mutated (callers should
// separately update access_count / last_accessed_at on the persistence
// layer).
func Recall(items []Item, p RecallParams) ([]ScoredItem, error) {
	if len(items) == 0 {
		return nil, ErrNoCandidates
	}
	w := p.Weights
	if w == (RecallWeights{}) {
		w = DefaultRecallWeights
	}
	half := p.HalfLifeHours
	if half <= 0 {
		half = 72
	}
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}

	wsum := w.Recency + w.Importance + w.Similarity
	if wsum <= 0 {
		return nil, errors.New("memory: weights must sum > 0")
	}

	out := make([]ScoredItem, 0, len(items))
	for _, it := range items {
		rec := recencyDecay(now, it.LastAccessedAt, it.CreatedAt, half)
		imp := clamp01(it.Importance)
		sim := 0.0
		if len(p.Query) > 0 && len(it.Embedding) > 0 {
			sim = Cosine(p.Query, it.Embedding)
		}
		score := (w.Recency*rec + w.Importance*imp + w.Similarity*sim) / wsum
		if score < p.MinScore {
			continue
		}
		out = append(out, ScoredItem{
			Item:       it,
			Recency:    rec,
			Importance: imp,
			Similarity: sim,
			Score:      score,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if p.TopK > 0 && len(out) > p.TopK {
		out = out[:p.TopK]
	}
	return out, nil
}

// recencyDecay returns exp(-ln(2) * age_hours / half) clamped to [0, 1].
// The age is measured from the most recent of LastAccessedAt and CreatedAt
// so that recall hits "refresh" the recency score.
func recencyDecay(now, lastAccess, created time.Time, halfLifeHours float64) float64 {
	ref := created
	if !lastAccess.IsZero() && lastAccess.After(ref) {
		ref = lastAccess
	}
	if ref.IsZero() {
		return 0
	}
	ageH := now.Sub(ref).Hours()
	if ageH < 0 {
		return 1
	}
	return math.Exp(-math.Ln2 * ageH / halfLifeHours)
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
