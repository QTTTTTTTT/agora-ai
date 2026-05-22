package sentiment

import (
	"context"
	"errors"
	"log/slog"
)

// CompositeScorer chains a primary scorer to a fallback so the
// wiring layer is one-liner: hand it the LLM scorer + a keyword
// fallback, and any LLM blip degrades gracefully instead of leaving
// the debate without sentiment signal.
//
// Behavior:
//   - Primary returns a non-error result → return it.
//   - Primary errors → log a warning and try Fallback.
//   - Fallback errors (or both nil) → return the last error.
type CompositeScorer struct {
	Primary  Scorer
	Fallback Scorer
}

// Score implements Scorer.
func (c *CompositeScorer) Score(ctx context.Context, items []Item) ([]Score, error) {
	if c == nil {
		return nil, errors.New("composite scorer is nil")
	}
	if c.Primary != nil {
		scores, err := c.Primary.Score(ctx, items)
		if err == nil {
			return scores, nil
		}
		slog.Warn("primary sentiment scorer failed; trying fallback",
			"err", err,
			"items", len(items),
		)
	}
	if c.Fallback != nil {
		return c.Fallback.Score(ctx, items)
	}
	return nil, errors.New("no scorer configured")
}
