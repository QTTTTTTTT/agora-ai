package main

import (
	"fmt"
	"strings"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/factorlab"
)

// factorlabAdapter wires the Stage-2 factor report engine behind
// the api.FactorLabService façade. The adapter is deliberately
// stateless — every request builds the synthetic fixture fresh
// using the request's seed/days overrides, so two operators
// stress-testing the endpoint don't share PRNG state.
//
// Real-CSV fixture loading is out of scope for the MVP: the
// adapter exposes only the synthetic path. When fundamental data
// arrives (Sprint 3-2), a fixture-registry approach replaces
// this in a single edit-point.
type factorlabAdapter struct{}

func newFactorLabAdapter() *factorlabAdapter {
	return &factorlabAdapter{}
}

// RunFactorReport satisfies api.FactorLabService. Builds the
// synthetic fixture, resolves the factor name list, runs the
// report, and projects each result into the api wire shape.
func (a *factorlabAdapter) RunFactorReport(_ string, input api.FactorReportInput) ([]*api.FactorReportView, error) {
	if a == nil {
		return nil, api.ErrFactorLabUnconfigured
	}

	// 1. Resolve the factor set. Empty list = DefaultFactors().
	factors, err := resolveFactors(input.FactorNames)
	if err != nil {
		return nil, err
	}
	if len(factors) == 0 {
		return nil, fmt.Errorf("factorlab: no factors selected and DefaultFactors() returned empty")
	}

	// 2. Resolve the fixture. Only "synthetic" supported in MVP.
	fixtureName := strings.ToLower(strings.TrimSpace(input.FixtureName))
	if fixtureName == "" {
		fixtureName = "synthetic"
	}
	if fixtureName != "synthetic" {
		return nil, fmt.Errorf("factorlab: unknown fixture %q (only 'synthetic' supported)", input.FixtureName)
	}
	seed := input.SeedOverride
	if seed == 0 {
		seed = 42
	}
	days := input.DaysOverride
	if days <= 0 {
		days = 800 // ≈ 3y of business days, enough for both 5d and 22d horizons
	}
	fixture := factorlab.BuildSynthFixture(factorlab.SynthOptions{
		Seed: seed,
		Days: days,
	})

	// 3. Build the report config.
	cfg := factorlab.FactorReportConfig{
		Horizons:                  input.Horizons,
		LayeredHorizonDays:        input.LayeredHorizonDays,
		MinSymbolsPerCrossSection: 3,
		// Default thresholds live inside the factorlab package
		// (US-equity monthly tier).
	}

	// 4. Run + project.
	reports := factorlab.RunFactorReport(fixture, factors, cfg)
	if len(reports) == 0 {
		return nil, fmt.Errorf("factorlab: report engine produced no output (fixture too short?)")
	}

	out := make([]*api.FactorReportView, 0, len(reports))
	for _, r := range reports {
		out = append(out, projectFactorReport(r))
	}
	return out, nil
}

// resolveFactors maps user-supplied factor names to the concrete
// Factor implementations. Empty input returns DefaultFactors().
// Returns an error on the first unknown name so the operator
// gets immediate feedback in the UI.
func resolveFactors(names []string) ([]factorlab.Factor, error) {
	if len(names) == 0 {
		return factorlab.DefaultFactors(), nil
	}
	byName := map[string]factorlab.Factor{}
	for _, f := range factorlab.DefaultFactors() {
		byName[f.Name()] = f
	}
	out := make([]factorlab.Factor, 0, len(names))
	for _, n := range names {
		key := strings.TrimSpace(n)
		if key == "" {
			continue
		}
		f, ok := byName[key]
		if !ok {
			return nil, fmt.Errorf("factorlab: unknown factor name %q", n)
		}
		out = append(out, f)
	}
	return out, nil
}

// RunWalkForwardFactor satisfies api.FactorLabService. Same
// fixture-resolution path as RunFactorReport, but the request
// targets exactly one factor and the result is the per-fold IC
// stability rollup.
func (a *factorlabAdapter) RunWalkForwardFactor(_ string, input api.WalkForwardFactorInput) (*api.WalkForwardFactorResultView, error) {
	if a == nil {
		return nil, api.ErrFactorLabUnconfigured
	}

	factors, err := resolveFactors([]string{input.FactorName})
	if err != nil {
		return nil, err
	}
	if len(factors) != 1 {
		return nil, fmt.Errorf("factorlab: expected exactly 1 factor for walkforward, got %d", len(factors))
	}

	fixtureName := strings.ToLower(strings.TrimSpace(input.FixtureName))
	if fixtureName == "" {
		fixtureName = "synthetic"
	}
	if fixtureName != "synthetic" {
		return nil, fmt.Errorf("factorlab: unknown fixture %q (only 'synthetic' supported)", input.FixtureName)
	}

	seed := input.SeedOverride
	if seed == 0 {
		seed = 42
	}
	days := input.DaysOverride
	if days <= 0 {
		// Walk-forward needs more data than the single-shot report
		// because each fold needs its own warmup window. Default
		// ~6y so 5 folds × ~1y/fold + 273-day warmup all fits.
		days = 1500
	}
	fixture := factorlab.BuildSynthFixture(factorlab.SynthOptions{
		Seed: seed,
		Days: days,
	})

	cfg := factorlab.WalkForwardConfig{
		NumFolds:                  input.NumFolds,
		Horizons:                  input.Horizons,
		Factor:                    factors[0],
		MinSymbolsPerCrossSection: 3,
		Min:                       factorlab.DefaultQualificationThresholds(),
	}
	result, err := factorlab.RunWalkForwardFactor(fixture, cfg)
	if err != nil {
		return nil, err
	}
	return projectWalkForwardFactor(result), nil
}

func projectWalkForwardFactor(r *factorlab.WalkForwardFactorResult) *api.WalkForwardFactorResultView {
	if r == nil {
		return nil
	}
	folds := make([]api.FoldICResultView, 0, len(r.Folds))
	for _, f := range r.Folds {
		folds = append(folds, api.FoldICResultView{
			Index:            f.Index,
			StartDate:        f.StartDate,
			EndDate:          f.EndDate,
			ObservationDays:  f.ObservationDays,
			SpearmanMean:     f.SpearmanMean,
			SpearmanIR:       f.SpearmanIR,
			SpearmanTStat:    f.SpearmanTStat,
			PositiveICRatio:  f.PositiveICRatio,
			LongShortSharpe:  f.LongShortSharpe,
			LongShortAnnual:  f.LongShortAnnual,
			LayeredSpreadAnn: f.LayeredSpreadAnn,
			Qualified:        f.Qualified,
			Error:            f.Error,
		})
	}
	return &api.WalkForwardFactorResultView{
		FactorName:         r.FactorName,
		NumFolds:           r.NumFolds,
		Folds:              folds,
		MeanIC22d:          r.MeanIC22d,
		MinIC22d:           r.MinIC22d,
		ICStabilityRatio:   r.ICStabilityRatio,
		AllFoldsQualified:  r.AllFoldsQualified,
		QualifiedFoldCount: r.QualifiedFoldCount,
	}
}

// projectFactorReport copies a factorlab.FactorReport into the
// api wire shape. Pure projection — no I/O, no allocations beyond
// the wire structs themselves.
func projectFactorReport(r *factorlab.FactorReport) *api.FactorReportView {
	if r == nil {
		return nil
	}
	view := &api.FactorReportView{
		FactorName:         r.FactorName,
		StartDate:          r.StartDate,
		EndDate:            r.EndDate,
		UniverseMedianSize: r.UniverseMedianSize,
		ObservationDays:    r.ObservationDays,
		Qualified:          r.Qualified,
		IC:                 map[string]api.ICStatsView{},
		QualReport: api.QualificationReportView{
			HorizonDaysReference: r.QualReport.HorizonDaysReference,
			PassesIC:             r.QualReport.PassesIC,
			PassesIR:             r.QualReport.PassesIR,
			PassesTStat:          r.QualReport.PassesTStat,
			PassesPositiveRatio:  r.QualReport.PassesPositiveRatio,
			PassesLongShort:      r.QualReport.PassesLongShort,
		},
	}
	for h, stats := range r.IC {
		view.IC[fmt.Sprintf("%d", h)] = api.ICStatsView{
			HorizonDays:     stats.HorizonDays,
			PearsonSeries:   stats.PearsonSeries,
			SpearmanSeries:  stats.SpearmanSeries,
			PearsonMean:     stats.PearsonMean,
			PearsonStd:      stats.PearsonStd,
			PearsonIR:       stats.PearsonIR,
			PearsonTStat:    stats.PearsonTStat,
			SpearmanMean:    stats.SpearmanMean,
			SpearmanStd:     stats.SpearmanStd,
			SpearmanIR:      stats.SpearmanIR,
			SpearmanTStat:   stats.SpearmanTStat,
			PositiveICRatio: stats.PositiveICRatio,
		}
	}
	if r.Layered != nil {
		view.Layered = &api.LayeredResultView{
			HorizonDays:          r.Layered.HorizonDays,
			QuintileMeanReturn:   r.Layered.QuintileMeanReturn,
			QuintileAnnualReturn: r.Layered.QuintileAnnualReturn,
			Spread:               r.Layered.Spread,
			SpreadAnnual:         r.Layered.SpreadAnnual,
			SpreadTStat:          r.Layered.SpreadTStat,
			Monotonic:            r.Layered.Monotonic,
			ObservationPeriods:   r.Layered.ObservationPeriods,
		}
	}
	if r.LongShort != nil {
		nav := make([]api.FactorNavPoint, 0, len(r.LongShort.NavCurve))
		for _, p := range r.LongShort.NavCurve {
			nav = append(nav, api.FactorNavPoint{Date: p.Date, Nav: p.Nav})
		}
		view.LongShort = &api.LongShortResultView{
			NavCurve:     nav,
			AnnualReturn: r.LongShort.AnnualReturn,
			AnnualVol:    r.LongShort.AnnualVol,
			Sharpe:       r.LongShort.Sharpe,
			MaxDrawdown:  r.LongShort.MaxDrawdown,
		}
	}
	return view
}
