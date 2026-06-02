// Package stress applies named stress scenarios to a fund's
// current holdings and produces a projected P&L.
//
// Scenarios are stored in stress_scenarios and are a list of
// Shock specs. Each Shock has:
//
//   * TargetType: how to match holdings —
//     - "instrument"   exact instrument_key match (most specific)
//     - "market"       match HoldingPosition.Market
//     - "asset_class"  match HoldingPosition.AssetClass
//     - "factor"       lookup InstrumentFactorLoading and apply
//                      Value * loading per matched holding
//     - "wildcard"     applies to every holding ("*" sentinel)
//
//   * TargetKey: the key to compare against. For factor shocks
//     it's a factor name from internal/factorexposure (e.g.
//     "momentum"); for wildcard it's typically "*".
//
//   * Value: a signed decimal fraction. -0.20 means "-20%
//     return" for the holding. For factor shocks, the per-holding
//     applied shock is Value * loading, capped by the engine if
//     |applied| > 1 to avoid pathologically large notional
//     swings.
//
// Matching priority (engine evaluates highest-specificity first):
//   instrument > market > asset_class > factor > wildcard
//
// A single holding picks the most-specific match and applies that
// one Shock — except factor shocks, which compound additively
// across multiple factor specs (loading_size * shock_size +
// loading_value * shock_value + ...). This mirrors the textbook
// "factor model" P&L attribution.
//
// Why a single package and not "stresstest" — we already have
// stoptrigger / surveillance / factorexposure / varisk all named
// after the domain concept, so "stress" sticks.
package stress

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Category groups scenarios on the admin UI. The set is small and
// the CHECK constraint in migration 069 enforces it.
type Category string

const (
	CategoryHistorical   Category = "historical"
	CategoryHypothetical Category = "hypothetical"
	CategoryRegulatory   Category = "regulatory"
)

var AllCategories = []Category{CategoryHistorical, CategoryHypothetical, CategoryRegulatory}

func (c Category) IsValid() bool {
	switch c {
	case CategoryHistorical, CategoryHypothetical, CategoryRegulatory:
		return true
	}
	return false
}

// TargetType enumerates the supported shock-target buckets.
type TargetType string

const (
	TargetInstrument TargetType = "instrument"
	TargetMarket     TargetType = "market"
	TargetAssetClass TargetType = "asset_class"
	TargetFactor     TargetType = "factor"
	TargetWildcard   TargetType = "wildcard"
)

func (t TargetType) IsValid() bool {
	switch t {
	case TargetInstrument, TargetMarket, TargetAssetClass, TargetFactor, TargetWildcard:
		return true
	}
	return false
}

// Specificity gives each target type a rank so the engine can
// pick the most-specific match for a holding. Higher = more
// specific.
func (t TargetType) Specificity() int {
	switch t {
	case TargetInstrument:
		return 4
	case TargetMarket:
		return 3
	case TargetAssetClass:
		return 2
	case TargetFactor:
		return 1
	case TargetWildcard:
		return 0
	}
	return -1
}

// Shock is one element of a scenario's shock list.
type Shock struct {
	TargetType TargetType `json:"target_type"`
	TargetKey  string     `json:"target_key"`
	Value      float64    `json:"value"`
}

// Validate rejects malformed shocks. Used by both the repo on
// write and the engine on read so we never compute with
// nonsense.
func (s Shock) Validate() error {
	if !s.TargetType.IsValid() {
		return fmt.Errorf("stress: invalid target_type %q", s.TargetType)
	}
	if s.TargetType != TargetWildcard && strings.TrimSpace(s.TargetKey) == "" {
		return fmt.Errorf("stress: target_key required for target_type=%s", s.TargetType)
	}
	if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
		return fmt.Errorf("stress: value must be finite")
	}
	// Sanity clamp. -10 (i.e. -1000%) is already far past anything
	// historically observable; +10 likewise. Refuse beyond that.
	if math.Abs(s.Value) > 10 {
		return fmt.Errorf("stress: |value| %f exceeds 10x cap", s.Value)
	}
	return nil
}

// Scenario is the full scenario definition stored in stress_scenarios.
type Scenario struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Category    Category  `json:"category"`
	Description string    `json:"description"`
	Shocks      []Shock   `json:"shocks"`
	CreatedBy   string    `json:"created_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Validate enforces invariants needed for safe persistence.
func (s Scenario) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("stress: scenario name required")
	}
	if !s.Category.IsValid() {
		return fmt.Errorf("stress: invalid category %q", s.Category)
	}
	for i, sh := range s.Shocks {
		if err := sh.Validate(); err != nil {
			return fmt.Errorf("stress: shocks[%d]: %w", i, err)
		}
	}
	return nil
}

// Holding is the engine's minimum view of a position. The
// per-fund handler converts repository.HoldingPosition into this
// shape (shorts get a negative MarketValue).
type Holding struct {
	InstrumentKey string
	Symbol        string
	AssetClass    string
	Market        string
	MarketValue   float64
}

// FactorLoadings carries the per-instrument, per-factor loading
// values used by TargetFactor shocks. The handler queries
// internal/factorexposure for these and passes the map in.
//
// Key = instrument_key. Value = map of factor name → loading.
type FactorLoadings map[string]map[string]float64

// HoldingImpact is one row of the per-holding stress result.
//
// AppliedShock is nil when no shock matched (the holding had a
// pnl of 0 because nothing touched it).
type HoldingImpact struct {
	InstrumentKey      string  `json:"instrument_key"`
	Symbol             string  `json:"symbol"`
	AssetClass         string  `json:"asset_class,omitempty"`
	MarketValueBefore  float64 `json:"market_value_before"`
	MarketValueAfter   float64 `json:"market_value_after"`
	PnL                float64 `json:"pnl"`
	AppliedReturn      float64 `json:"applied_return"`
	AppliedShockType   string  `json:"applied_shock_type,omitempty"`
	AppliedShockKey    string  `json:"applied_shock_key,omitempty"`
}

// Result is the engine's output for one (fund, scenario)
// computation.
type Result struct {
	FundID        string
	ScenarioID    string
	GeneratedAt   time.Time
	NAVBefore     float64
	NAVAfter      float64
	PnLTotal      float64
	PnLPct        float64
	HoldingCount  int
	ShockedCount  int
	Impacts       []HoldingImpact
}

// sortImpactsByMagnitude sorts impacts by |PnL| descending so the
// UI drill-down naturally surfaces the biggest contributors first.
func sortImpactsByMagnitude(impacts []HoldingImpact) {
	sort.SliceStable(impacts, func(i, j int) bool {
		return math.Abs(impacts[i].PnL) > math.Abs(impacts[j].PnL)
	})
}

// clampAppliedReturn caps an applied factor return at ±100% so a
// pathological loading × shock combination can't wipe out 5x the
// position value. This is a guard for engine sanity; the shock
// validator already refuses |value| > 10 at the source.
func clampAppliedReturn(r float64) float64 {
	if r > 1 {
		return 1
	}
	if r < -1 {
		return -1
	}
	return r
}
