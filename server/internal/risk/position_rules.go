// Position-sizing rules: single-position cap, total exposure cap, and
// per-sector exposure cap.
package risk

import (
	"context"
	"fmt"
)

// SinglePositionLimit caps the post-trade weight of any one symbol as a
// fraction of TotalAssets.
type SinglePositionLimit struct {
	Max float64 // e.g. 0.30 for 30%
}

func (r SinglePositionLimit) Name() string { return "single_position_limit" }

func (r SinglePositionLimit) Evaluate(_ context.Context, pc PlanContext) ([]Finding, error) {
	if pc.TotalAssets <= 0 {
		return nil, nil
	}
	post := projectedExposurePostTrade(pc)
	var out []Finding
	// Only emit findings for symbols that the trades touch, to avoid
	// spamming on existing in-spec positions.
	seen := map[string]bool{}
	for _, t := range pc.Trades {
		if seen[t.Symbol] {
			continue
		}
		seen[t.Symbol] = true
		ratio := post[t.Symbol] / pc.TotalAssets
		f := Finding{
			Rule:      r.Name(),
			Symbol:    t.Symbol,
			Current:   ratio,
			Threshold: r.Max,
			Message:   fmt.Sprintf("%s projected %s of assets", t.Symbol, fmtPct(ratio)),
		}
		if ratio > r.Max {
			f.Severity = SeverityFail
			f.Message = fmt.Sprintf("%s would be %s (limit %s)", t.Symbol, fmtPct(ratio), fmtPct(r.Max))
			f.Suggestion = fmt.Sprintf("Reduce %s to ≤%s of assets", t.Symbol, fmtPct(r.Max))
		} else {
			f.Severity = SeverityInfo
		}
		out = append(out, f)
	}
	return out, nil
}

// TotalExposureLimit caps the post-trade aggregate gross exposure as a
// fraction of TotalAssets.
type TotalExposureLimit struct {
	Max float64
}

func (r TotalExposureLimit) Name() string { return "total_position_limit" }

func (r TotalExposureLimit) Evaluate(_ context.Context, pc PlanContext) ([]Finding, error) {
	if pc.TotalAssets <= 0 {
		return nil, nil
	}
	exposure := portfolioValue(pc)
	for _, t := range pc.Trades {
		delta := t.Notional()
		if t.Side.IsSell() {
			delta = -delta
		}
		exposure += delta
	}
	ratio := exposure / pc.TotalAssets
	f := Finding{
		Rule:      r.Name(),
		Current:   ratio,
		Threshold: r.Max,
		Message:   fmt.Sprintf("total position %s", fmtPct(ratio)),
	}
	if ratio > r.Max {
		f.Severity = SeverityFail
		f.Message = fmt.Sprintf("total position %s exceeds %s limit", fmtPct(ratio), fmtPct(r.Max))
		f.Suggestion = "Reduce overall position size or increase cash reserves"
	} else {
		f.Severity = SeverityInfo
	}
	return []Finding{f}, nil
}

// SectorExposureLimit warns when any sector's post-trade exposure exceeds the
// configured fraction.
type SectorExposureLimit struct {
	Max      float64
	Severity Severity // optional override (defaults to warn)
}

func (r SectorExposureLimit) Name() string { return "sector_concentration" }

func (r SectorExposureLimit) Evaluate(_ context.Context, pc PlanContext) ([]Finding, error) {
	if pc.TotalAssets <= 0 {
		return nil, nil
	}
	sev := r.Severity
	if sev == "" {
		sev = SeverityWarn
	}
	expo := sectorExposurePostTrade(pc)
	var out []Finding
	for _, sector := range sortedKeys(expo) {
		ratio := expo[sector] / pc.TotalAssets
		f := Finding{
			Rule:      r.Name(),
			Current:   ratio,
			Threshold: r.Max,
			Message:   fmt.Sprintf("sector %s %s", sector, fmtPct(ratio)),
		}
		if ratio > r.Max {
			f.Severity = sev
			f.Message = fmt.Sprintf("sector %s %s exceeds %s", sector, fmtPct(ratio), fmtPct(r.Max))
			f.Suggestion = fmt.Sprintf("Diversify away from %s sector", sector)
		} else {
			f.Severity = SeverityInfo
		}
		out = append(out, f)
	}
	return out, nil
}
