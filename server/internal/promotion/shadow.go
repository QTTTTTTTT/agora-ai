package promotion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/repository"
)

// DecisionSnapshot is the compact summary the comparator stores
// per side. Kept as a struct (not just JSONB) so the comparator
// can mechanically derive the agreement flag without parsing
// nested blobs every time.
//
// The intent is "did the two engines agree on the high-level
// trading intent?", not "do they propose identical orders".
// Differences in qty / price are recorded but don't break
// agreement on their own — operators tune the comparator's
// strictness via the Agreement function passed to the
// ShadowComparator.
type DecisionSnapshot struct {
	Action     string  `json:"action"`     // buy / sell / hold
	Symbol     string  `json:"symbol"`
	Quantity   float64 `json:"quantity"`
	Notional   float64 `json:"notional"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
	EngineKind string  `json:"engineKind"`
}

// AgreementFn decides whether the shadow and active decisions
// "agree". Wrapped as a function so the API can tighten /
// loosen the criterion without changing the comparator.
//
// The default implementation (DefaultAgreement) treats two
// decisions as agreeing when (action, symbol) match exactly, and
// the relative quantity difference is within 10%. Action="hold"
// matches another "hold" regardless of symbol.
type AgreementFn func(shadow, active DecisionSnapshot) bool

// DefaultAgreement is the comparator's standard agreement rule.
// Tuned to "high-level intent matches" — operators who need
// stricter price/qty alignment should swap in their own.
func DefaultAgreement(shadow, active DecisionSnapshot) bool {
	if strings.EqualFold(shadow.Action, active.Action) && strings.EqualFold(shadow.Action, "hold") {
		return true
	}
	if !strings.EqualFold(shadow.Action, active.Action) {
		return false
	}
	if !strings.EqualFold(shadow.Symbol, active.Symbol) {
		return false
	}
	// Quantity within ±10% considered the same intent.
	if active.Quantity == 0 {
		return shadow.Quantity == 0
	}
	delta := (shadow.Quantity - active.Quantity) / active.Quantity
	if delta < 0 {
		delta = -delta
	}
	return delta <= 0.10
}

// ShadowComparator persists daily side-by-side comparisons of a
// candidate promotion's decisions vs the production engine's
// decisions. Used during shadow mode to validate that the
// candidate behaves sensibly before letting it drive real money.
type ShadowComparator struct {
	Repo  *repository.PromotionRepo
	NewID IDGen
	Now   Clock
	// Agree controls how the comparator buckets pairs. Defaults
	// to DefaultAgreement when nil.
	Agree AgreementFn
}

// Record persists (or replaces) the per-day comparison row. The
// (promotion_id, trading_date) unique index makes this an
// idempotent upsert.
//
// We accept the trading date explicitly (not derived from Now)
// because the comparator may run async — the trading date is
// authoritative.
func (c *ShadowComparator) Record(
	ctx context.Context,
	promotionID string,
	tradingDate time.Time,
	shadow, active DecisionSnapshot,
) (*ShadowDiff, error) {
	if c == nil || c.Repo == nil || c.NewID == nil || c.Now == nil {
		return nil, errors.New("shadow comparator: not wired")
	}
	if promotionID == "" {
		return nil, errors.New("shadow comparator: promotionID required")
	}
	agree := c.Agree
	if agree == nil {
		agree = DefaultAgreement
	}
	shadowJSON, err := json.Marshal(shadow)
	if err != nil {
		return nil, fmt.Errorf("marshal shadow: %w", err)
	}
	activeJSON, err := json.Marshal(active)
	if err != nil {
		return nil, fmt.Errorf("marshal active: %w", err)
	}
	now := c.Now()
	row := &repository.ShadowDiffRow{
		ID:             c.NewID(),
		PromotionID:    promotionID,
		TradingDate:    tradingDate.UTC().Truncate(24 * time.Hour),
		ShadowDecision: shadowJSON,
		ActiveDecision: activeJSON,
		Agreement:      agree(shadow, active),
		CreatedAt:      now,
	}
	if err := c.Repo.UpsertShadowDiff(ctx, row); err != nil {
		return nil, err
	}
	return rowToShadowDiff(row), nil
}

// ListRecent returns the trailing N rows for a promotion,
// newest first. Used by the detail page.
func (c *ShadowComparator) ListRecent(ctx context.Context, promotionID string, limit int) ([]*ShadowDiff, error) {
	if c == nil || c.Repo == nil {
		return nil, errors.New("shadow comparator: not wired")
	}
	rows, err := c.Repo.ListShadowDiffs(ctx, promotionID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*ShadowDiff, 0, len(rows))
	for _, r := range rows {
		out = append(out, rowToShadowDiff(r))
	}
	return out, nil
}

// AgreementRatio reports the fraction of rows in [from, to] that
// were flagged as agreeing. Used by the API to give the operator
// a single "is this promotion ready?" number.
func (c *ShadowComparator) AgreementRatio(ctx context.Context, promotionID string, limit int) (float64, int, error) {
	rows, err := c.ListRecent(ctx, promotionID, limit)
	if err != nil {
		return 0, 0, err
	}
	if len(rows) == 0 {
		return 0, 0, nil
	}
	agree := 0
	for _, r := range rows {
		if r.Agreement {
			agree++
		}
	}
	return float64(agree) / float64(len(rows)), len(rows), nil
}

func rowToShadowDiff(r *repository.ShadowDiffRow) *ShadowDiff {
	if r == nil {
		return nil
	}
	var shadow, active map[string]any
	if len(r.ShadowDecision) > 0 {
		_ = json.Unmarshal(r.ShadowDecision, &shadow)
	}
	if len(r.ActiveDecision) > 0 {
		_ = json.Unmarshal(r.ActiveDecision, &active)
	}
	return &ShadowDiff{
		ID:             r.ID,
		PromotionID:    r.PromotionID,
		TradingDate:    r.TradingDate,
		ShadowDecision: shadow,
		ActiveDecision: active,
		Agreement:      r.Agreement,
		CreatedAt:      r.CreatedAt,
	}
}
