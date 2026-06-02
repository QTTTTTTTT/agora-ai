package stress

import (
	"math"
	"strings"
	"time"
)

// Engine applies a Scenario to a list of Holdings and produces a
// Result. Stateless; safe for concurrent use.
type Engine struct {
	Now func() time.Time
}

func NewEngine() *Engine {
	return &Engine{Now: func() time.Time { return time.Now().UTC() }}
}

// Compute applies the scenario's shocks to the holdings and
// returns the full Result.
//
// loadings is consulted only when the scenario contains
// TargetFactor shocks. Callers may pass nil if they know the
// scenario has no factor shocks (handler optimisation).
func (e *Engine) Compute(fundID string, scenario Scenario, holdings []Holding, loadings FactorLoadings) Result {
	now := time.Now().UTC()
	if e != nil && e.Now != nil {
		now = e.Now()
	}

	// Bucket shocks by target type for O(1) lookup per holding.
	exactByKey := map[string]Shock{}
	byMarket := map[string]Shock{}
	byAssetClass := map[string]Shock{}
	factorShocks := []Shock{}
	var wildcard *Shock

	for i := range scenario.Shocks {
		sh := scenario.Shocks[i]
		switch sh.TargetType {
		case TargetInstrument:
			exactByKey[strings.TrimSpace(sh.TargetKey)] = sh
		case TargetMarket:
			byMarket[strings.ToUpper(strings.TrimSpace(sh.TargetKey))] = sh
		case TargetAssetClass:
			byAssetClass[strings.ToLower(strings.TrimSpace(sh.TargetKey))] = sh
		case TargetFactor:
			factorShocks = append(factorShocks, sh)
		case TargetWildcard:
			// Only one wildcard is honoured; later ones overwrite.
			s := sh
			wildcard = &s
		}
	}

	result := Result{
		FundID:       fundID,
		ScenarioID:   scenario.ID,
		GeneratedAt:  now,
		HoldingCount: len(holdings),
		Impacts:      make([]HoldingImpact, 0, len(holdings)),
	}

	for _, h := range holdings {
		impact := HoldingImpact{
			InstrumentKey:     h.InstrumentKey,
			Symbol:            h.Symbol,
			AssetClass:        h.AssetClass,
			MarketValueBefore: h.MarketValue,
			MarketValueAfter:  h.MarketValue,
		}
		applied, shockType, shockKey, matched := chooseShock(h, exactByKey, byMarket, byAssetClass, factorShocks, wildcard, loadings)
		if matched {
			impact.AppliedReturn = applied
			impact.AppliedShockType = string(shockType)
			impact.AppliedShockKey = shockKey
			impact.MarketValueAfter = h.MarketValue * (1 + applied)
			impact.PnL = impact.MarketValueAfter - h.MarketValue
			result.ShockedCount++
		}
		// Gross MV uses |MarketValue| so shorts contribute their
		// absolute notional to NAV. PnL is signed (long down =
		// loss, short down = gain).
		result.NAVBefore += math.Abs(h.MarketValue)
		result.NAVAfter += math.Abs(impact.MarketValueAfter)
		result.PnLTotal += impact.PnL
		result.Impacts = append(result.Impacts, impact)
	}
	if result.NAVBefore > 0 {
		result.PnLPct = result.PnLTotal / result.NAVBefore
	}
	sortImpactsByMagnitude(result.Impacts)
	return result
}

// chooseShock implements the priority ladder:
//   instrument > market > asset_class > factor > wildcard
// For factor it sums loading * shock_value across every factor
// shock that has a loading for the holding.
func chooseShock(
	h Holding,
	exact map[string]Shock,
	byMarket map[string]Shock,
	byAssetClass map[string]Shock,
	factorShocks []Shock,
	wildcard *Shock,
	loadings FactorLoadings,
) (applied float64, t TargetType, key string, matched bool) {
	if sh, ok := exact[h.InstrumentKey]; ok {
		return clampAppliedReturn(sh.Value), sh.TargetType, sh.TargetKey, true
	}
	if h.Market != "" {
		if sh, ok := byMarket[strings.ToUpper(h.Market)]; ok {
			return clampAppliedReturn(sh.Value), sh.TargetType, sh.TargetKey, true
		}
	}
	if h.AssetClass != "" {
		if sh, ok := byAssetClass[strings.ToLower(h.AssetClass)]; ok {
			return clampAppliedReturn(sh.Value), sh.TargetType, sh.TargetKey, true
		}
	}
	if len(factorShocks) > 0 {
		facMap := loadings[h.InstrumentKey]
		if len(facMap) > 0 {
			sum := 0.0
			matchedAny := false
			usedKeys := make([]string, 0, len(factorShocks))
			for _, sh := range factorShocks {
				ld, ok := facMap[sh.TargetKey]
				if !ok {
					continue
				}
				sum += sh.Value * ld
				matchedAny = true
				usedKeys = append(usedKeys, sh.TargetKey)
			}
			if matchedAny {
				return clampAppliedReturn(sum), TargetFactor, strings.Join(usedKeys, "+"), true
			}
		}
	}
	if wildcard != nil {
		return clampAppliedReturn(wildcard.Value), wildcard.TargetType, wildcard.TargetKey, true
	}
	return 0, "", "", false
}
