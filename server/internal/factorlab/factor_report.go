package factorlab

import (
	"math"
	"sort"
	"time"
)

// FactorReport is the per-factor cross-sectional analytics block
// the Stage-2 frontend renders. All fields are zero-initialised
// when the input fixture is too short / too sparse.
//
// Field summary:
//   - IC: per-horizon information-coefficient series and headline
//     mean/std/IR/t-stat. Pearson (linear correlation between
//     today's factor scores and the next-h-day return) AND
//     Spearman (same on ranks) are reported because they react
//     differently to outliers and skew.
//   - Layered: 5-quintile cumulative-return table at horizon=5d.
//     L1=lowest score quintile, L5=highest. L5−L1 is the
//     long/short return; the spread is the classical "is this
//     factor's signal monotonic?" check.
//   - LongShort: NAV curve of a daily-rebalanced long-Q5 /
//     short-Q1 portfolio (long-only mode also reports just the
//     long leg).
//   - Qualified: boolean rollup against the canonical thresholds
//     in DefaultQualificationThresholds (US-equity-monthly tier).
type FactorReport struct {
	FactorName string `json:"factorName"`

	StartDate time.Time `json:"startDate"`
	EndDate   time.Time `json:"endDate"`

	UniverseMedianSize int `json:"universeMedianSize"`
	ObservationDays    int `json:"observationDays"`

	IC        map[int]ICStats     `json:"ic"` // horizon (days) → stats
	Layered   *LayeredResult      `json:"layered,omitempty"`
	LongShort *LongShortResult    `json:"longShort,omitempty"`
	Qualified bool                `json:"qualified"`
	QualReport QualificationReport `json:"qualReport"`
}

// ICStats holds the per-horizon information-coefficient summary.
// The series field is exposed so the UI can plot the rolling IC
// (one point per observation day).
type ICStats struct {
	HorizonDays int `json:"horizonDays"`

	// Per-day cross-sectional IC time series. One entry per
	// observation day where the cross-section had >= MinSymbols.
	// Trimmed of NaN/Inf.
	PearsonSeries  []float64 `json:"pearsonSeries"`
	SpearmanSeries []float64 `json:"spearmanSeries"`

	PearsonMean  float64 `json:"pearsonMean"`
	PearsonStd   float64 `json:"pearsonStd"`
	PearsonIR    float64 `json:"pearsonIR"`    // mean / std
	PearsonTStat float64 `json:"pearsonTStat"` // mean / (std/sqrt(N))

	SpearmanMean  float64 `json:"spearmanMean"`
	SpearmanStd   float64 `json:"spearmanStd"`
	SpearmanIR    float64 `json:"spearmanIR"`
	SpearmanTStat float64 `json:"spearmanTStat"`

	PositiveICRatio float64 `json:"positiveICRatio"` // share of days with Spearman IC > 0
}

// LayeredResult is the cumulative-return table for 5 quintiles
// at a fixed horizon (typically 5d). Q5 is highest score, Q1
// lowest. SpreadAnnual is the annualised L5−L1 return.
type LayeredResult struct {
	HorizonDays int `json:"horizonDays"`
	// Mean per-period return per quintile, fraction form
	// (0.012 = +1.2%). Already non-overlapping (we step the
	// observation date by HorizonDays so each forward return
	// only appears once).
	QuintileMeanReturn   [5]float64 `json:"quintileMeanReturn"`
	QuintileAnnualReturn [5]float64 `json:"quintileAnnualReturn"`
	Spread               float64    `json:"spread"`            // Q5 − Q1, per period
	SpreadAnnual         float64    `json:"spreadAnnual"`      // annualised
	SpreadTStat          float64    `json:"spreadTStat"`       // per-period mean / (std/sqrt(N))
	Monotonic            bool       `json:"monotonic"`         // Q1 ≤ Q2 ≤ Q3 ≤ Q4 ≤ Q5 in mean return
	ObservationPeriods   int        `json:"observationPeriods"`
}

// LongShortResult is the daily long-Q5 / short-Q1 portfolio NAV.
type LongShortResult struct {
	NavCurve     []NavPoint `json:"navCurve"`
	AnnualReturn float64    `json:"annualReturn"`
	AnnualVol    float64    `json:"annualVol"`
	Sharpe       float64    `json:"sharpe"`
	MaxDrawdown  float64    `json:"maxDrawdown"`
}

// QualificationReport details which of the canonical thresholds
// the factor passed. Fields mirror the headline thresholds in
// DefaultQualificationThresholds (US-equity-monthly tier).
type QualificationReport struct {
	HorizonDaysReference int  `json:"horizonDaysReference"`
	PassesIC             bool `json:"passesIC"`
	PassesIR             bool `json:"passesIR"`
	PassesTStat          bool `json:"passesTStat"`
	PassesPositiveRatio  bool `json:"passesPositiveRatio"`
	PassesLongShort      bool `json:"passesLongShort"`
}

// QualificationThresholds is the per-tier benchmark the factor
// must beat to be marked "qualified". Different tiers (A-share
// daily vs US-equity monthly) ship different defaults.
type QualificationThresholds struct {
	ICMeanAbsMin     float64
	IRMin            float64
	TStatMin         float64
	PositiveICRatio  float64
	LongShortSharpe  float64
	HorizonDays      int // reference horizon for the qualification
}

// DefaultQualificationThresholds returns the US-equity monthly
// tier — the right defaults for the Stage-1/Stage-2 SaaS product.
func DefaultQualificationThresholds() QualificationThresholds {
	return QualificationThresholds{
		ICMeanAbsMin:    0.025,
		IRMin:           0.4,
		TStatMin:        2.0,
		PositiveICRatio: 0.53,
		LongShortSharpe: 0.8,
		HorizonDays:     22,
	}
}

// FactorReportConfig controls the report engine.
type FactorReportConfig struct {
	// Horizons is the list of forward-return horizons (in trading
	// days) to compute IC at. Defaults to [5, 22] (weekly +
	// monthly).
	Horizons []int

	// LayeredHorizonDays is the horizon used for the
	// 5-quintile layered table + L5−L1 long/short. Must be one
	// of Horizons. Defaults to the largest horizon (= monthly).
	LayeredHorizonDays int

	// MinSymbolsPerCrossSection is the minimum number of
	// symbols with a non-NaN factor score required to include
	// the day in the IC series. Days below this are skipped.
	// Defaults to 10.
	MinSymbolsPerCrossSection int

	// WarmupDays trims the leading window — same intuition as
	// Simulator.WarmupDays. Defaults to the longest horizon
	// referenced by any default factor (252+21 ≈ momentum).
	WarmupDays int

	// Thresholds drives the Qualified rollup. Defaults to
	// DefaultQualificationThresholds() when zero.
	Thresholds QualificationThresholds
}

func (c *FactorReportConfig) normalise() {
	if len(c.Horizons) == 0 {
		c.Horizons = []int{5, 22}
	}
	sort.Ints(c.Horizons)
	if c.LayeredHorizonDays == 0 {
		c.LayeredHorizonDays = c.Horizons[len(c.Horizons)-1]
	}
	if c.MinSymbolsPerCrossSection <= 0 {
		c.MinSymbolsPerCrossSection = 10
	}
	if c.WarmupDays <= 0 {
		c.WarmupDays = 273
	}
	if (c.Thresholds == QualificationThresholds{}) {
		c.Thresholds = DefaultQualificationThresholds()
	}
}

// RunFactorReport sweeps every observation day in the fixture's
// window, scores the cross-section, and aggregates Pearson/Spearman
// IC plus a 5-quintile layered table. Returns one report per
// factor. nil fixture / empty factors → nil result.
func RunFactorReport(fixture *Fixture, factors []Factor, cfg FactorReportConfig) []*FactorReport {
	if fixture == nil || len(factors) == 0 {
		return nil
	}
	cfg.normalise()
	days := fixture.TradingDays()
	if len(days) < cfg.WarmupDays+10 {
		return nil
	}
	maxHorizon := cfg.Horizons[len(cfg.Horizons)-1]
	if maxHorizon >= len(days)-cfg.WarmupDays {
		return nil
	}

	out := make([]*FactorReport, 0, len(factors))
	for _, factor := range factors {
		rep := newFactorReport(fixture, factor, days, cfg, maxHorizon)
		out = append(out, rep)
	}
	return out
}

func newFactorReport(fixture *Fixture, factor Factor, days []time.Time, cfg FactorReportConfig, maxHorizon int) *FactorReport {
	rep := &FactorReport{
		FactorName: factor.Name(),
		IC:         map[int]ICStats{},
	}

	// Pre-allocate horizon → per-day IC slices.
	pearsonByHorizon := map[int][]float64{}
	spearmanByHorizon := map[int][]float64{}
	for _, h := range cfg.Horizons {
		pearsonByHorizon[h] = nil
		spearmanByHorizon[h] = nil
	}

	// Layered: collect per-quintile per-period returns at the
	// LayeredHorizonDays horizon. We step the observation date by
	// LayeredHorizonDays so each forward return only appears once
	// (no overlapping windows ⇒ unbiased standard errors).
	layered := newLayeredAccumulator(cfg.LayeredHorizonDays)

	// LongShort daily NAV: a daily-rebalanced long-Q5 / short-Q1
	// portfolio sized at 1.0 each leg (so total NAV is unitless).
	longShort := newLongShortAccumulator(days, cfg.WarmupDays, maxHorizon)

	universeSizes := make([]int, 0, len(days)-cfg.WarmupDays-maxHorizon)
	obsDays := 0

	for i := cfg.WarmupDays; i < len(days)-maxHorizon; i++ {
		asOf := days[i]
		// Cross-section at asOf.
		section := ScoreCrossSection(fixture, factor, asOf)
		if len(section) < cfg.MinSymbolsPerCrossSection {
			continue
		}
		obsDays++
		universeSizes = append(universeSizes, len(section))

		// For each horizon, compute IC.
		for _, h := range cfg.Horizons {
			if i+h >= len(days) {
				continue
			}
			horizonDay := days[i+h]
			pairs := buildForwardReturnPairs(fixture, section, asOf, horizonDay)
			if len(pairs) < cfg.MinSymbolsPerCrossSection {
				continue
			}
			pe := pearsonCorr(pairs)
			sp := spearmanCorr(pairs)
			if !math.IsNaN(pe) && !math.IsInf(pe, 0) {
				pearsonByHorizon[h] = append(pearsonByHorizon[h], pe)
			}
			if !math.IsNaN(sp) && !math.IsInf(sp, 0) {
				spearmanByHorizon[h] = append(spearmanByHorizon[h], sp)
			}
		}

		// Layered + long/short at the layered horizon. Only step
		// every LayeredHorizonDays so the buckets are non-overlapping.
		if (i-cfg.WarmupDays)%cfg.LayeredHorizonDays == 0 {
			lh := cfg.LayeredHorizonDays
			if i+lh < len(days) {
				horizonDay := days[i+lh]
				layered.observe(fixture, section, asOf, horizonDay)
				longShort.observePeriod(fixture, section, asOf, horizonDay)
			}
		}
	}

	rep.StartDate = days[cfg.WarmupDays]
	rep.EndDate = days[len(days)-1]
	rep.ObservationDays = obsDays
	rep.UniverseMedianSize = medianInt(universeSizes)

	for _, h := range cfg.Horizons {
		stats := computeICStats(h, pearsonByHorizon[h], spearmanByHorizon[h])
		rep.IC[h] = stats
	}
	rep.Layered = layered.finalise()
	rep.LongShort = longShort.finalise()
	rep.QualReport, rep.Qualified = rollupQualification(rep, cfg.Thresholds)
	return rep
}

// ---------------------------------------------------------------------------
// Helpers: forward returns, correlations, ranks
// ---------------------------------------------------------------------------

func buildForwardReturnPairs(fix *Fixture, section []FactorScore, asOf, horizonDay time.Time) [][2]float64 {
	out := make([][2]float64, 0, len(section))
	for _, fs := range section {
		p0, ok0 := fix.CloseAt(fs.Symbol, asOf)
		p1, ok1 := fix.CloseAt(fs.Symbol, horizonDay)
		if !ok0 || !ok1 || p0 <= 0 {
			continue
		}
		fr := p1/p0 - 1.0
		out = append(out, [2]float64{fs.Score, fr})
	}
	return out
}

// pearsonCorr is the classic linear correlation coefficient on
// pairs of (x, y). Returns NaN for degenerate input (<2 pairs,
// zero variance on either axis).
func pearsonCorr(pairs [][2]float64) float64 {
	n := len(pairs)
	if n < 2 {
		return math.NaN()
	}
	var sx, sy float64
	for _, p := range pairs {
		sx += p[0]
		sy += p[1]
	}
	mx, my := sx/float64(n), sy/float64(n)
	var cov, vx, vy float64
	for _, p := range pairs {
		dx := p[0] - mx
		dy := p[1] - my
		cov += dx * dy
		vx += dx * dx
		vy += dy * dy
	}
	if vx <= 0 || vy <= 0 {
		return math.NaN()
	}
	return cov / math.Sqrt(vx*vy)
}

// spearmanCorr is Pearson on the ranks of the two series. Ties
// receive the average rank.
func spearmanCorr(pairs [][2]float64) float64 {
	if len(pairs) < 2 {
		return math.NaN()
	}
	xs := make([]float64, len(pairs))
	ys := make([]float64, len(pairs))
	for i, p := range pairs {
		xs[i] = p[0]
		ys[i] = p[1]
	}
	rx := rankWithTies(xs)
	ry := rankWithTies(ys)
	ranked := make([][2]float64, len(pairs))
	for i := range pairs {
		ranked[i] = [2]float64{rx[i], ry[i]}
	}
	return pearsonCorr(ranked)
}

// rankWithTies returns 1-indexed ranks with average-tie handling.
// len(out) == len(in).
func rankWithTies(in []float64) []float64 {
	n := len(in)
	type ix struct {
		val float64
		idx int
	}
	tmp := make([]ix, n)
	for i, v := range in {
		tmp[i] = ix{v, i}
	}
	sort.Slice(tmp, func(i, j int) bool { return tmp[i].val < tmp[j].val })
	out := make([]float64, n)
	i := 0
	for i < n {
		j := i
		// Walk forward while tied.
		for j+1 < n && tmp[j+1].val == tmp[i].val {
			j++
		}
		// Average rank = mean of (i+1) .. (j+1).
		avgRank := float64(i+j+2) / 2.0
		for k := i; k <= j; k++ {
			out[tmp[k].idx] = avgRank
		}
		i = j + 1
	}
	return out
}

// computeICStats reduces per-day IC slices to headline summary
// numbers. Empty inputs return a zero-valued struct.
func computeICStats(horizon int, pearson, spearman []float64) ICStats {
	stats := ICStats{
		HorizonDays:    horizon,
		PearsonSeries:  pearson,
		SpearmanSeries: spearman,
	}
	if len(pearson) > 0 {
		stats.PearsonMean = meanOfF(pearson)
		stats.PearsonStd = stdevOf(pearson)
		if stats.PearsonStd > 0 {
			stats.PearsonIR = stats.PearsonMean / stats.PearsonStd
			stats.PearsonTStat = stats.PearsonMean / (stats.PearsonStd / math.Sqrt(float64(len(pearson))))
		}
	}
	if len(spearman) > 0 {
		stats.SpearmanMean = meanOfF(spearman)
		stats.SpearmanStd = stdevOf(spearman)
		if stats.SpearmanStd > 0 {
			stats.SpearmanIR = stats.SpearmanMean / stats.SpearmanStd
			stats.SpearmanTStat = stats.SpearmanMean / (stats.SpearmanStd / math.Sqrt(float64(len(spearman))))
		}
		pos := 0
		for _, v := range spearman {
			if v > 0 {
				pos++
			}
		}
		stats.PositiveICRatio = float64(pos) / float64(len(spearman))
	}
	return stats
}

// ---------------------------------------------------------------------------
// Layered accumulator
// ---------------------------------------------------------------------------

type layeredAccumulator struct {
	horizon int
	// quintileReturns[q] is a slice of per-period returns for
	// quintile q (0..4 → Q1..Q5).
	quintileReturns [5][]float64
	// spreadReturns is the per-period L5−L1 difference.
	spreadReturns []float64
}

func newLayeredAccumulator(horizon int) *layeredAccumulator {
	return &layeredAccumulator{horizon: horizon}
}

func (l *layeredAccumulator) observe(fix *Fixture, section []FactorScore, asOf, horizonDay time.Time) {
	if len(section) < 5 {
		return
	}
	// Section is already sorted desc by score (Q5 first).
	// Reverse to make Q1=lowest first for cleaner slicing.
	asc := make([]FactorScore, len(section))
	for i, v := range section {
		asc[len(section)-1-i] = v
	}
	// Build forward returns for each quintile.
	bucketSize := len(asc) / 5
	if bucketSize < 1 {
		return
	}
	for q := 0; q < 5; q++ {
		from := q * bucketSize
		to := from + bucketSize
		if q == 4 {
			to = len(asc) // last bucket sweeps any remainder
		}
		bucket := asc[from:to]
		var sum float64
		var ok int
		for _, fs := range bucket {
			p0, ok0 := fix.CloseAt(fs.Symbol, asOf)
			p1, ok1 := fix.CloseAt(fs.Symbol, horizonDay)
			if !ok0 || !ok1 || p0 <= 0 {
				continue
			}
			sum += p1/p0 - 1.0
			ok++
		}
		if ok > 0 {
			l.quintileReturns[q] = append(l.quintileReturns[q], sum/float64(ok))
		}
	}
	// Spread (only if both Q1 and Q5 produced an observation this period).
	if len(l.quintileReturns[0]) > 0 && len(l.quintileReturns[4]) > 0 &&
		len(l.quintileReturns[0]) == len(l.quintileReturns[4]) {
		// The last-appended entries are this period's.
		q1 := l.quintileReturns[0][len(l.quintileReturns[0])-1]
		q5 := l.quintileReturns[4][len(l.quintileReturns[4])-1]
		l.spreadReturns = append(l.spreadReturns, q5-q1)
	}
}

func (l *layeredAccumulator) finalise() *LayeredResult {
	if len(l.spreadReturns) == 0 {
		return nil
	}
	// Annualisation: periods per year = 252 / horizon.
	periodsPerYear := 252.0 / float64(l.horizon)
	result := &LayeredResult{
		HorizonDays:        l.horizon,
		ObservationPeriods: len(l.spreadReturns),
	}
	for q := 0; q < 5; q++ {
		m := meanOfF(l.quintileReturns[q])
		result.QuintileMeanReturn[q] = m
		// Annualised: (1+m)^periodsPerYear − 1, but for small m
		// the linear approximation is fine and avoids weird
		// numerics on negative quintiles.
		result.QuintileAnnualReturn[q] = math.Pow(1+m, periodsPerYear) - 1
	}
	result.Spread = meanOfF(l.spreadReturns)
	result.SpreadAnnual = math.Pow(1+result.Spread, periodsPerYear) - 1
	if std := stdevOf(l.spreadReturns); std > 0 {
		result.SpreadTStat = result.Spread / (std / math.Sqrt(float64(len(l.spreadReturns))))
	}
	mono := true
	for q := 1; q < 5; q++ {
		if result.QuintileMeanReturn[q] < result.QuintileMeanReturn[q-1] {
			mono = false
			break
		}
	}
	result.Monotonic = mono
	return result
}

// ---------------------------------------------------------------------------
// LongShort accumulator
// ---------------------------------------------------------------------------

type longShortAccumulator struct {
	nav            float64
	curve          []NavPoint
	periodReturns  []float64
	lastObs        time.Time
}

func newLongShortAccumulator(days []time.Time, warmup, maxHorizon int) *longShortAccumulator {
	_ = days
	_ = warmup
	_ = maxHorizon
	return &longShortAccumulator{nav: 1.0}
}

func (a *longShortAccumulator) observePeriod(fix *Fixture, section []FactorScore, asOf, horizonDay time.Time) {
	if len(section) < 5 {
		return
	}
	// Section is sorted desc by score. Q5 = top 20%, Q1 = bottom 20%.
	cut := len(section) / 5
	if cut < 1 {
		cut = 1
	}
	q5 := section[:cut]
	q1 := section[len(section)-cut:]
	q5Ret := bucketReturn(fix, q5, asOf, horizonDay)
	q1Ret := bucketReturn(fix, q1, asOf, horizonDay)
	periodReturn := q5Ret - q1Ret
	a.periodReturns = append(a.periodReturns, periodReturn)
	a.nav *= 1 + periodReturn
	a.curve = append(a.curve, NavPoint{Date: horizonDay, Nav: a.nav})
	a.lastObs = horizonDay
}

func bucketReturn(fix *Fixture, bucket []FactorScore, asOf, horizonDay time.Time) float64 {
	var sum float64
	var n int
	for _, fs := range bucket {
		p0, ok0 := fix.CloseAt(fs.Symbol, asOf)
		p1, ok1 := fix.CloseAt(fs.Symbol, horizonDay)
		if !ok0 || !ok1 || p0 <= 0 {
			continue
		}
		sum += p1/p0 - 1.0
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

func (a *longShortAccumulator) finalise() *LongShortResult {
	if len(a.periodReturns) == 0 {
		return nil
	}
	// Periods per year guess: derive from the spacing of curve
	// entries. With monthly horizon → ~12 periods/year, weekly →
	// ~52, daily → 252. Compute from the actual elapsed span.
	periodsPerYear := 12.0
	if len(a.curve) >= 2 {
		span := a.curve[len(a.curve)-1].Date.Sub(a.curve[0].Date).Hours() / 24.0 / 365.0
		if span > 0 {
			periodsPerYear = float64(len(a.curve)-1) / span
		}
	}
	mean := meanOfF(a.periodReturns)
	std := stdevOf(a.periodReturns)
	result := &LongShortResult{
		NavCurve:     append([]NavPoint(nil), a.curve...),
		AnnualReturn: math.Pow(1+mean, periodsPerYear) - 1,
	}
	result.AnnualVol = std * math.Sqrt(periodsPerYear)
	if result.AnnualVol > 0 {
		result.Sharpe = result.AnnualReturn / result.AnnualVol
	}
	result.MaxDrawdown = maxDrawdown(a.curve)
	return result
}

// ---------------------------------------------------------------------------
// Qualification rollup
// ---------------------------------------------------------------------------

func rollupQualification(rep *FactorReport, thr QualificationThresholds) (QualificationReport, bool) {
	report := QualificationReport{HorizonDaysReference: thr.HorizonDays}
	stats, ok := rep.IC[thr.HorizonDays]
	if !ok {
		return report, false
	}
	report.PassesIC = math.Abs(stats.SpearmanMean) >= thr.ICMeanAbsMin
	report.PassesIR = math.Abs(stats.SpearmanIR) >= thr.IRMin
	report.PassesTStat = math.Abs(stats.SpearmanTStat) >= thr.TStatMin
	report.PassesPositiveRatio = stats.PositiveICRatio >= thr.PositiveICRatio || (1.0-stats.PositiveICRatio) >= thr.PositiveICRatio
	if rep.LongShort != nil {
		report.PassesLongShort = math.Abs(rep.LongShort.Sharpe) >= thr.LongShortSharpe
	}
	passed := report.PassesIC && report.PassesIR && report.PassesTStat && report.PassesPositiveRatio && report.PassesLongShort
	return report, passed
}

// ---------------------------------------------------------------------------
// tiny math helpers (factorlab-internal; existing metrics.go has
// the simulator-specific stdev — keep separate so we don't break
// the Result.applyMetrics fast path).
// ---------------------------------------------------------------------------

func meanOfF(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func stdevOf(v []float64) float64 {
	if len(v) < 2 {
		return 0
	}
	m := meanOfF(v)
	var ss float64
	for _, x := range v {
		d := x - m
		ss += d * d
	}
	return math.Sqrt(ss / float64(len(v)-1))
}

func medianInt(v []int) int {
	if len(v) == 0 {
		return 0
	}
	cp := append([]int(nil), v...)
	sort.Ints(cp)
	return cp[len(cp)/2]
}
