package factorlab

import (
	"fmt"
	"math"
	"time"
)

// WalkForwardConfig drives the rolling N-fold factor stability
// harness. The harness slices the fixture's window into NumFolds
// equal CONTIGUOUS sub-windows (no train/test split — we're not
// fitting, we're MEASURING fold-by-fold) and runs the full IC/IR
// report on each fold independently.
//
// The output is a per-fold IC stability table: did the factor's
// IC sign + magnitude hold up out-of-sample, or did it crater
// after fold 1? This is the canonical "factor decay" check that
// stops you shipping a factor whose alpha lives in 2009-2010.
type WalkForwardConfig struct {
	NumFolds int                // default 5
	Horizons []int              // default {22}
	Factor   Factor             // required
	Min      QualificationCheck // optional gate; nil = report-only

	// MinSymbolsPerCrossSection mirrors FactorReportConfig.
	MinSymbolsPerCrossSection int

	// WarmupDays trims the leading window of EACH fold so the
	// factor has a settling window before scoring. Defaults to
	// 273 (matches Simulator default).
	WarmupDays int
}

// QualificationCheck is the optional per-fold gate. If supplied,
// each fold is also marked Passed against these thresholds; the
// rolled-up result is "all folds passed?" → AllFoldsQualified.
type QualificationCheck = QualificationThresholds

// WalkForwardFactorResult is the harness output. Per-fold rows
// carry the trimmed IC numbers; the rollup carries the
// stability verdict.
type WalkForwardFactorResult struct {
	FactorName string         `json:"factorName"`
	NumFolds   int            `json:"numFolds"`
	Folds      []FoldICResult `json:"folds"`

	// Stability metrics across folds.
	MeanIC22d           float64 `json:"meanIC22d"`           // mean of per-fold Spearman IC at 22d
	MinIC22d            float64 `json:"minIC22d"`            // worst per-fold Spearman IC at 22d
	ICStabilityRatio    float64 `json:"icStabilityRatio"`    // share of folds with IC of same sign as mean
	AllFoldsQualified   bool    `json:"allFoldsQualified"`   // true iff every fold passed Min thresholds
	QualifiedFoldCount  int     `json:"qualifiedFoldCount"`
}

// FoldICResult is one fold's IC summary at the rollup horizon.
// Each fold runs RunFactorReport in miniature; we keep only the
// headline numbers to keep the wire payload small.
type FoldICResult struct {
	Index     int       `json:"index"`
	StartDate time.Time `json:"startDate"`
	EndDate   time.Time `json:"endDate"`

	ObservationDays int     `json:"observationDays"`
	SpearmanMean    float64 `json:"spearmanMean"`
	SpearmanIR      float64 `json:"spearmanIR"`
	SpearmanTStat   float64 `json:"spearmanTStat"`
	PositiveICRatio float64 `json:"positiveICRatio"`

	LongShortSharpe   float64 `json:"longShortSharpe"`
	LongShortAnnual   float64 `json:"longShortAnnual"`
	LayeredSpreadAnn  float64 `json:"layeredSpreadAnnual"`

	Qualified bool `json:"qualified"`
	Error     string `json:"error,omitempty"`
}

// RunWalkForwardFactor slices the fixture's trading-day window
// into NumFolds equal contiguous folds and runs RunFactorReport
// on each fold separately. Returns one rollup result per call.
//
// Sources of error:
//   - nil fixture / nil factor → nil result + error
//   - fixture too short for the requested NumFolds × (warmup +
//     longest-horizon + a few obs days) → returns the partial
//     rollup with whatever folds we DID manage; the empty folds
//     get .Error set and are excluded from the stability stats.
func RunWalkForwardFactor(fixture *Fixture, cfg WalkForwardConfig) (*WalkForwardFactorResult, error) {
	if fixture == nil {
		return nil, fmt.Errorf("walkforward: nil fixture")
	}
	if cfg.Factor == nil {
		return nil, fmt.Errorf("walkforward: nil factor")
	}
	if cfg.NumFolds <= 0 {
		cfg.NumFolds = 5
	}
	if len(cfg.Horizons) == 0 {
		cfg.Horizons = []int{22}
	}
	if cfg.MinSymbolsPerCrossSection <= 0 {
		cfg.MinSymbolsPerCrossSection = 3
	}
	if cfg.WarmupDays <= 0 {
		cfg.WarmupDays = 273
	}
	// Pick the rollup horizon = largest in Horizons; same
	// convention as FactorReportConfig.
	rollupHorizon := cfg.Horizons[0]
	for _, h := range cfg.Horizons {
		if h > rollupHorizon {
			rollupHorizon = h
		}
	}

	days := fixture.TradingDays()
	if len(days) < cfg.WarmupDays+cfg.NumFolds*(rollupHorizon+5) {
		return nil, fmt.Errorf("walkforward: fixture too short (%d days) for %d folds + warmup %d + horizon %d",
			len(days), cfg.NumFolds, cfg.WarmupDays, rollupHorizon)
	}

	rollup := &WalkForwardFactorResult{
		FactorName: cfg.Factor.Name(),
		NumFolds:   cfg.NumFolds,
		Folds:      make([]FoldICResult, 0, cfg.NumFolds),
	}

	// Slice the post-warmup window into NumFolds.
	usableStart := cfg.WarmupDays
	usableEnd := len(days) - rollupHorizon
	foldLen := (usableEnd - usableStart) / cfg.NumFolds
	if foldLen < rollupHorizon+5 {
		return nil, fmt.Errorf("walkforward: fold window (%d days) is shorter than horizon+5 (%d)", foldLen, rollupHorizon+5)
	}

	for i := 0; i < cfg.NumFolds; i++ {
		foldStart := usableStart + i*foldLen
		foldEnd := foldStart + foldLen
		if i == cfg.NumFolds-1 {
			foldEnd = usableEnd // last fold sweeps any remainder
		}
		fold := runOneFold(fixture, cfg, days, foldStart, foldEnd, rollupHorizon, i)
		rollup.Folds = append(rollup.Folds, fold)
	}

	rollup.applyStability(cfg.Min)
	return rollup, nil
}

func runOneFold(fix *Fixture, cfg WalkForwardConfig, days []time.Time, foldStart, foldEnd, rollupHorizon, index int) FoldICResult {
	out := FoldICResult{
		Index:     index,
		StartDate: days[foldStart],
		EndDate:   days[foldEnd-1],
	}
	// Build a sub-window fixture by trimming each history's bars
	// to [days[foldStart - warmup], days[foldEnd]]. The
	// per-fold RunFactorReport then sees a window with its own
	// warmup baked in.
	warmStart := foldStart - cfg.WarmupDays
	if warmStart < 0 {
		warmStart = 0
	}
	subFixture := sliceFixture(fix, days[warmStart], days[foldEnd-1])
	if subFixture == nil || len(subFixture.Histories) == 0 {
		out.Error = "sub-fixture empty after slicing"
		return out
	}

	reps := RunFactorReport(subFixture, []Factor{cfg.Factor}, FactorReportConfig{
		Horizons:                  cfg.Horizons,
		LayeredHorizonDays:        rollupHorizon,
		MinSymbolsPerCrossSection: cfg.MinSymbolsPerCrossSection,
		WarmupDays:                cfg.WarmupDays,
		Thresholds:                cfg.Min,
	})
	if len(reps) == 0 {
		out.Error = "report engine returned no result (fold too short?)"
		return out
	}
	r := reps[0]
	out.ObservationDays = r.ObservationDays

	stats, ok := r.IC[rollupHorizon]
	if !ok {
		out.Error = fmt.Sprintf("no IC at rollup horizon %d", rollupHorizon)
		return out
	}
	out.SpearmanMean = stats.SpearmanMean
	out.SpearmanIR = stats.SpearmanIR
	out.SpearmanTStat = stats.SpearmanTStat
	out.PositiveICRatio = stats.PositiveICRatio

	if r.LongShort != nil {
		out.LongShortSharpe = r.LongShort.Sharpe
		out.LongShortAnnual = r.LongShort.AnnualReturn
	}
	if r.Layered != nil {
		out.LayeredSpreadAnn = r.Layered.SpreadAnnual
	}
	out.Qualified = r.Qualified
	return out
}

func (w *WalkForwardFactorResult) applyStability(thr QualificationThresholds) {
	if w == nil || len(w.Folds) == 0 {
		return
	}
	var sumIC, minIC float64
	var validFolds, sameSignFolds int
	var qualifiedFolds int
	first := true
	for _, f := range w.Folds {
		if f.Error != "" {
			continue
		}
		validFolds++
		sumIC += f.SpearmanMean
		if first {
			minIC = f.SpearmanMean
			first = false
		} else if f.SpearmanMean < minIC {
			minIC = f.SpearmanMean
		}
		if f.Qualified {
			qualifiedFolds++
		}
	}
	if validFolds == 0 {
		return
	}
	w.MeanIC22d = sumIC / float64(validFolds)
	w.MinIC22d = minIC
	w.QualifiedFoldCount = qualifiedFolds
	w.AllFoldsQualified = qualifiedFolds == validFolds && (thr != QualificationThresholds{})

	meanSign := 1.0
	if w.MeanIC22d < 0 {
		meanSign = -1.0
	}
	for _, f := range w.Folds {
		if f.Error != "" {
			continue
		}
		sig := 1.0
		if f.SpearmanMean < 0 {
			sig = -1.0
		}
		if sig == meanSign {
			sameSignFolds++
		}
	}
	w.ICStabilityRatio = float64(sameSignFolds) / float64(validFolds)
	if math.IsNaN(w.ICStabilityRatio) {
		w.ICStabilityRatio = 0
	}
}

// sliceFixture returns a NEW fixture whose histories are
// restricted to the [from, to] inclusive date window. The
// underlying SymbolHistory.Bars slices are sub-slices (no copy)
// so the operation is cheap.
func sliceFixture(src *Fixture, from, to time.Time) *Fixture {
	if src == nil {
		return nil
	}
	fromN := normaliseDate(from)
	toN := normaliseDate(to)
	out := &Fixture{
		Start: fromN,
		End:   toN,
	}
	for _, h := range src.Histories {
		filtered := SymbolHistory{
			Symbol: h.Symbol,
			Market: h.Market,
		}
		for _, b := range h.Bars {
			d := normaliseDate(b.Date)
			if d.Before(fromN) || d.After(toN) {
				continue
			}
			filtered.Bars = append(filtered.Bars, b)
		}
		if len(filtered.Bars) > 0 {
			out.Histories = append(out.Histories, filtered)
		}
	}
	if src.Benchmark != nil {
		bh := SymbolHistory{Symbol: src.Benchmark.Symbol, Market: src.Benchmark.Market}
		for _, b := range src.Benchmark.Bars {
			d := normaliseDate(b.Date)
			if d.Before(fromN) || d.After(toN) {
				continue
			}
			bh.Bars = append(bh.Bars, b)
		}
		if len(bh.Bars) > 0 {
			out.Benchmark = &bh
		}
	}
	return out
}
