package backtest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// ErrWalkForwardInvalid is returned when the spec doesn't validate:
// non-positive folds, window too narrow to split, malformed mode.
// The adapter translates it to a 400.
var ErrWalkForwardInvalid = errors.New("walk-forward: invalid spec")

// MaxWalkForwardFolds caps the fold count. Above this the per-
// fold UI becomes unusable AND the OOS validation power flattens
// out: 12 non-overlapping segments of a 5-year window is already
// quarterly granularity.
const MaxWalkForwardFolds = 12

// WalkForwardMode controls how train + test windows slide forward.
//
// "anchored": train always starts at the global Start; test is
//             the next slice. This is the classic "expanding
//             window" mode — useful when older data is still
//             informative for the strategy.
//
// "rolling":  train is a fixed-length window that slides forward
//             one step per fold. Test is the same fixed length
//             that follows. Useful when the market regime changes
//             quickly enough that ancient training data hurts.
//
// In this phase the platform doesn't actually retrain the
// decision engine (LLM prompts are stateless, the fallback engine
// is deterministic), so the "train" window is informational —
// it tells the OOS reader "the strategy hadn't seen this period
// yet." The runner still respects it by not feeding train-window
// bars to the decision engine when running the test fold.
type WalkForwardMode string

const (
	WalkForwardAnchored WalkForwardMode = "anchored"
	WalkForwardRolling  WalkForwardMode = "rolling"
)

// WalkForwardSpec is the input to a chunked OOS validation run.
// Operators set NumFolds + TrainRatio + Mode; the planner derives
// every fold's [trainStart, trainEnd, testStart, testEnd] tuple.
type WalkForwardSpec struct {
	// NumFolds is how many (train, test) pairs to produce.
	// Required, 2 ≤ NumFolds ≤ MaxWalkForwardFolds.
	NumFolds int `json:"numFolds"`
	// TrainRatio is the fraction of each fold spent on the
	// (informational) train window. The remainder is the test
	// window. 0 disables the train phase entirely — every day
	// of the global window is OOS, just chunked into NumFolds
	// equal pieces. Range: 0 ≤ TrainRatio < 1. Defaults to 0.6.
	TrainRatio float64 `json:"trainRatio"`
	// Mode is "anchored" (expanding train window) or "rolling"
	// (sliding fixed-length train window). Defaults to anchored.
	Mode WalkForwardMode `json:"mode,omitempty"`
}

// FoldWindow is one (train, test) pair on the calendar. The
// planner returns N of these; the runner replays each test
// window with its own portfolio and stitches the results.
type FoldWindow struct {
	Index      int       `json:"index"`
	TrainStart time.Time `json:"trainStart"`
	TrainEnd   time.Time `json:"trainEnd"`
	TestStart  time.Time `json:"testStart"`
	TestEnd    time.Time `json:"testEnd"`
}

// PlanWalkForward translates a global [start, end] window plus a
// WalkForwardSpec into N concrete FoldWindows. Pure function — no
// I/O, no clock, deterministic for stable inputs.
//
// The planner slices on calendar day boundaries; the underlying
// runner still aligns to actual trading days from the OHLC
// provider. So a fold spanning [Jan 2, Jan 5] picks up whatever
// bars the provider returns within that range (3 trading days in
// a normal week, fewer if there's a holiday).
//
// We require at least 2 trading-day-equivalents per fold (4
// calendar days) so degenerate single-bar folds don't compute
// nonsense Sharpe ratios. Returns ErrWalkForwardInvalid otherwise.
func PlanWalkForward(start, end time.Time, spec WalkForwardSpec) ([]FoldWindow, error) {
	if !start.Before(end) {
		return nil, fmt.Errorf("%w: start >= end", ErrWalkForwardInvalid)
	}
	if spec.NumFolds < 2 || spec.NumFolds > MaxWalkForwardFolds {
		return nil, fmt.Errorf("%w: numFolds %d (allowed 2..%d)", ErrWalkForwardInvalid, spec.NumFolds, MaxWalkForwardFolds)
	}
	if spec.TrainRatio < 0 || spec.TrainRatio >= 1 {
		return nil, fmt.Errorf("%w: trainRatio %f (allowed [0,1))", ErrWalkForwardInvalid, spec.TrainRatio)
	}
	mode := normaliseWalkForwardMode(spec.Mode)

	totalDur := end.Sub(start)
	// The window must hold enough days that each fold has at
	// least 4 calendar days for the test side. Anchored sweeps
	// give some folds tiny train sides too, but those are
	// informational so we don't enforce a minimum on train.
	minPerFold := 4 * 24 * time.Hour
	if totalDur < time.Duration(spec.NumFolds)*minPerFold {
		return nil, fmt.Errorf("%w: window %s too short for %d folds (need ≥ %s)",
			ErrWalkForwardInvalid, totalDur, spec.NumFolds, time.Duration(spec.NumFolds)*minPerFold)
	}

	folds := make([]FoldWindow, 0, spec.NumFolds)
	switch mode {
	case WalkForwardAnchored:
		// In anchored mode the test windows tile [start, end]
		// into NumFolds equal chunks. Train is everything from
		// global start up to each test's start — an expanding
		// window. TrainRatio is informational only in this mode
		// (we don't actually retrain a model, and the train
		// window's content is fixed at "everything before").
		stepDur := totalDur / time.Duration(spec.NumFolds)
		for i := 0; i < spec.NumFolds; i++ {
			testStart := start.Add(stepDur * time.Duration(i))
			testEnd := start.Add(stepDur * time.Duration(i+1))
			if i == spec.NumFolds-1 {
				// Snap the last test end exactly to `end` to
				// avoid rounding drift on uneven durations.
				testEnd = end
			}
			folds = append(folds, FoldWindow{
				Index:      i,
				TrainStart: start,
				TrainEnd:   testStart,
				TestStart:  testStart,
				TestEnd:    testEnd,
			})
		}
	case WalkForwardRolling:
		// Rolling: each fold is (trainWindow, testWindow) of
		// the SAME length, stepping forward by stepDur each
		// iteration. We size them so test windows tile the
		// global window and the first fold's train window
		// reaches back to start.
		stepDur := totalDur / time.Duration(spec.NumFolds)
		// trainWindowLen mirrors test length scaled by the
		// (1-ratio)/ratio inversion so that one full fold
		// (train+test) takes `stepDur * 1/(1-trainRatio)` time
		// when trainRatio > 0; when trainRatio == 0 train
		// collapses to zero and the fold is just one test slice.
		ratio := spec.TrainRatio
		if ratio == 0 {
			ratio = 0 // explicit, kept for clarity
		}
		trainLen := time.Duration(0)
		if ratio > 0 {
			trainLen = time.Duration(float64(stepDur) * ratio / (1 - ratio))
		}
		for i := 0; i < spec.NumFolds; i++ {
			testStart := start.Add(stepDur * time.Duration(i))
			testEnd := start.Add(stepDur * time.Duration(i+1))
			if i == spec.NumFolds-1 {
				testEnd = end
			}
			trainEnd := testStart
			trainStart := testStart.Add(-trainLen)
			if trainStart.Before(start) {
				trainStart = start
			}
			folds = append(folds, FoldWindow{
				Index:      i,
				TrainStart: trainStart,
				TrainEnd:   trainEnd,
				TestStart:  testStart,
				TestEnd:    testEnd,
			})
		}
	default:
		return nil, fmt.Errorf("%w: unknown mode %q", ErrWalkForwardInvalid, spec.Mode)
	}
	return folds, nil
}

// normaliseWalkForwardMode collapses empty / unknown values to
// the anchored default so callers don't have to special-case.
func normaliseWalkForwardMode(m WalkForwardMode) WalkForwardMode {
	switch WalkForwardMode(strings.ToLower(strings.TrimSpace(string(m)))) {
	case WalkForwardRolling:
		return WalkForwardRolling
	default:
		return WalkForwardAnchored
	}
}

// FoldRunSummary captures the OOS performance of one test fold.
// Mirrors backtest.Metrics in shape but tagged with the fold's
// window so the UI can render a per-fold table.
type FoldRunSummary struct {
	Index        int       `json:"index"`
	TestStart    time.Time `json:"testStart"`
	TestEnd      time.Time `json:"testEnd"`
	InitialNav   float64   `json:"initialNav"`
	FinalNav     float64   `json:"finalNav"`
	Return       float64   `json:"return"`       // (final-initial)/initial
	Metrics      Metrics   `json:"metrics"`
	TradeCount   int       `json:"tradeCount"`
	Error        string    `json:"error,omitempty"`
}

// WalkForwardResult is the aggregated output. Trades + NavCurve
// in the parent Result remain the source of truth for the
// fully-stitched OOS run; this struct adds per-fold context.
type WalkForwardResult struct {
	Spec       WalkForwardSpec  `json:"spec"`
	Mode       WalkForwardMode  `json:"mode"`
	Folds      []FoldRunSummary `json:"folds"`
	OOSReturn  float64          `json:"oosReturn"`  // cumulative return across all folds
	OOSSharpe  float64          `json:"oosSharpe"`  // Sharpe of the stitched daily-return series
	MeanFoldReturn   float64    `json:"meanFoldReturn"`
	WorstFoldReturn  float64    `json:"worstFoldReturn"`
	BestFoldReturn   float64    `json:"bestFoldReturn"`
	// FoldBoundaries indexes the parent Result.NavCurve. Each
	// entry is the NavCurve index where a new fold begins (i.e.
	// the first NAV point of the test segment). Index 0 is
	// always 0. The UI uses this to draw vertical separators on
	// the NAV chart.
	FoldBoundaries []int `json:"foldBoundaries"`
}

// WalkForwardRunner orchestrates the per-fold runs and stitches
// them into one continuous OOS narrative. It wraps an inner
// Runner (the existing single-window machinery) so we don't
// duplicate the daily decision-loop logic.
type WalkForwardRunner struct {
	Inner *Runner
}

// Run satisfies the Engine interface so a WalkForwardRunner can
// drop into the JobStore where a plain Runner used to live.
//
// The portfolio is reset between folds so each fold's return is
// independent — true OOS validation, not "one long backtest with
// reporting checkpoints". The stitched NAV is the multiplicative
// chain of per-fold returns × InitialCash, so the parent Result's
// CumulativeReturn equals OOSReturn.
func (w *WalkForwardRunner) Run(ctx context.Context, req Request, progress *Progress) (*Result, error) {
	if w == nil || w.Inner == nil {
		return nil, errors.New("walkforward: inner runner not configured")
	}
	spec, ok := extractWalkForward(req)
	if !ok {
		// No spec? Delegate to the inner runner unchanged so
		// the JobStore can be wired with a WalkForwardRunner
		// as the default engine without breaking plain runs.
		return w.Inner.Run(ctx, req, progress)
	}
	folds, err := PlanWalkForward(req.Start, req.End, spec)
	if err != nil {
		progress.markStatus("failed", err)
		return nil, err
	}
	// Total days for the progress bar = sum of test-side
	// calendar days across all folds. The inner Runner already
	// reports per-day progress; we just bump TotalDays here so
	// the bar fills correctly across all folds.
	var totalDays int
	for _, f := range folds {
		totalDays += int(f.TestEnd.Sub(f.TestStart).Hours()/24) + 1
	}
	progress.markTotal(totalDays)

	stitchedNav := make([]NavPoint, 0, totalDays)
	stitchedTrades := make([]TradeEvent, 0, 64)
	summaries := make([]FoldRunSummary, 0, len(folds))
	boundaries := make([]int, 0, len(folds))

	// Track running NAV so each fold's normalised return chains
	// multiplicatively into the global OOS curve.
	runningNav := req.InitialCash
	if runningNav <= 0 {
		runningNav = 100_000
	}
	startNav := runningNav

	for _, fold := range folds {
		select {
		case <-ctx.Done():
			progress.markStatus("cancelled", ErrCancelled)
			return nil, ErrCancelled
		default:
		}
		// Per-fold sub-request: same universe + engine + params,
		// just narrower window. Initial cash = current OOS NAV
		// so the per-fold runner sees a portfolio matching the
		// running stitched state.
		subReq := req
		subReq.Start = fold.TestStart
		subReq.End = fold.TestEnd
		subReq.InitialCash = runningNav
		// Drop the embedded walk-forward spec from the sub-
		// request so the inner runner doesn't recurse.
		clearWalkForward(&subReq)

		// Use a sub-progress so per-fold day counts don't trash
		// the parent's TotalDays. The parent bar is bumped
		// manually after each fold completes.
		subProgress := &Progress{}
		boundaries = append(boundaries, len(stitchedNav))

		foldResult, foldErr := w.Inner.Run(ctx, subReq, subProgress)
		if foldErr != nil {
			// Record the fold-level failure but continue —
			// one bad window shouldn't blow up the whole
			// walk-forward. The summary surfaces the error.
			summaries = append(summaries, FoldRunSummary{
				Index:      fold.Index,
				TestStart:  fold.TestStart,
				TestEnd:    fold.TestEnd,
				InitialNav: runningNav,
				FinalNav:   runningNav,
				Error:      foldErr.Error(),
			})
			// Bump progress by the planned span so the bar
			// doesn't stall on a degenerate fold.
			missing := int(fold.TestEnd.Sub(fold.TestStart).Hours()/24) + 1
			for k := 0; k < missing; k++ {
				progress.markDayDone(fold.TestEnd)
			}
			continue
		}
		// Stitch NAV: just concatenate. Because we passed the
		// running NAV as the fold's InitialCash, the per-fold
		// NavCurve already starts from the correct level.
		stitchedNav = append(stitchedNav, foldResult.NavCurve...)
		stitchedTrades = append(stitchedTrades, foldResult.Trades...)
		summaries = append(summaries, FoldRunSummary{
			Index:      fold.Index,
			TestStart:  fold.TestStart,
			TestEnd:    fold.TestEnd,
			InitialNav: runningNav,
			FinalNav:   foldResult.FinalNav,
			Return:     foldReturn(runningNav, foldResult.FinalNav),
			Metrics:    foldResult.Metrics,
			TradeCount: foldResult.Metrics.TradeCount,
		})
		runningNav = foldResult.FinalNav

		// Bump parent progress by the actual days reported in
		// the sub-progress so the bar tracks reality, not
		// calendar approximation.
		subSnap := subProgress.Snapshot()
		for k := 0; k < subSnap.DoneDays; k++ {
			progress.markDayDone(fold.TestEnd)
		}
	}

	// Build the parent Result. CumulativeReturn = OOS chain
	// return; Sharpe / vol / drawdown come from the stitched
	// daily series so they reflect end-to-end OOS behaviour
	// (not the per-fold averages).
	stitchedMetrics := computeMetrics(stitchedNav, stitchedTrades)
	wfResult := &WalkForwardResult{
		Spec:           spec,
		Mode:           normaliseWalkForwardMode(spec.Mode),
		Folds:          summaries,
		OOSReturn:      foldReturn(startNav, runningNav),
		OOSSharpe:      stitchedMetrics.SharpeRatio,
		FoldBoundaries: boundaries,
	}
	wfResult.MeanFoldReturn, wfResult.WorstFoldReturn, wfResult.BestFoldReturn = aggregateFoldReturns(summaries)

	finalResult := &Result{
		FundID:      req.FundID,
		Name:        req.Name,
		EngineKind:  req.EngineKind,
		Start:       req.Start,
		End:         req.End,
		InitialCash: startNav,
		FinalNav:    runningNav,
		NavCurve:    stitchedNav,
		Trades:      stitchedTrades,
		Metrics:     stitchedMetrics,
		CompletedAt: time.Now().UTC(),
		WalkForward: wfResult,
	}
	progress.markStatus("completed", nil)
	return finalResult, nil
}

// extractWalkForward / clearWalkForward isolate the embedded
// sub-spec access. The spec lives on the Request struct as an
// optional pointer (see types.go); we keep the lookups in one
// place so adding a sibling sub-spec doesn't have to touch every
// caller.
func extractWalkForward(req Request) (WalkForwardSpec, bool) {
	if req.WalkForward == nil {
		return WalkForwardSpec{}, false
	}
	return *req.WalkForward, true
}

func clearWalkForward(req *Request) {
	req.WalkForward = nil
}

// foldReturn computes (final-initial)/initial, guarding against
// a zero initial (which would NaN out).
func foldReturn(initial, final float64) float64 {
	if initial == 0 {
		return 0
	}
	return (final - initial) / initial
}

// aggregateFoldReturns reports mean / worst / best returns
// across the summary list. NaN-safe (folds that failed have
// Return == 0).
func aggregateFoldReturns(folds []FoldRunSummary) (mean, worst, best float64) {
	if len(folds) == 0 {
		return 0, 0, 0
	}
	sum := 0.0
	worst = math.Inf(1)
	best = math.Inf(-1)
	for _, f := range folds {
		sum += f.Return
		if f.Return < worst {
			worst = f.Return
		}
		if f.Return > best {
			best = f.Return
		}
	}
	if math.IsInf(worst, 0) {
		worst = 0
	}
	if math.IsInf(best, 0) {
		best = 0
	}
	return sum / float64(len(folds)), worst, best
}
