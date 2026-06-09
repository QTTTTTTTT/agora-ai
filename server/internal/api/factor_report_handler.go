package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// FactorLabService is the seam between the HTTP layer and the
// internal/factorlab package. Implementations live in
// cmd/server/factorlab_adapter.go so this package stays free of
// the factorlab → ohlc → repository import chain.
//
// Stage 2 MVP: the only operation is "run a report against the
// synthetic fixture" — that's enough to demonstrate IC/IR/分层
// to operators without a fundamental-data pipeline. Real-CSV
// fixtures arrive in Stage 2 follow-up; production market-wide
// scans land in Stage 3 with the walk-forward harness.
type FactorLabService interface {
	// RunFactorReport runs RunFactorReport against the requested
	// fixture and returns one FactorReportView per requested
	// factor. The synthetic fixture is the default; the request
	// can opt into a frozen CSV fixture by name when the deploy
	// has shipped one.
	RunFactorReport(userID string, input FactorReportInput) ([]*FactorReportView, error)

	// RunWalkForwardFactor slices the fixture into N folds and
	// runs the IC report on each fold independently. Returns the
	// per-fold stability table.
	RunWalkForwardFactor(userID string, input WalkForwardFactorInput) (*WalkForwardFactorResultView, error)
}

// WalkForwardFactorInput is the POST body for
// /api/factorlab/walkforward.
type WalkForwardFactorInput struct {
	// FactorName must be one of the registered factor names. Required.
	FactorName string `json:"factorName"`

	// NumFolds defaults to 5.
	NumFolds int `json:"numFolds,omitempty"`

	// Horizons defaults to [22].
	Horizons []int `json:"horizons,omitempty"`

	// FixtureName, SeedOverride, DaysOverride are the same
	// synthetic-fixture knobs as FactorReportInput.
	FixtureName  string `json:"fixtureName,omitempty"`
	SeedOverride int64  `json:"seedOverride,omitempty"`
	DaysOverride int    `json:"daysOverride,omitempty"`
}

// WalkForwardFactorResultView is the wire shape for the
// walkforward rollup. Mirrors factorlab.WalkForwardFactorResult.
type WalkForwardFactorResultView struct {
	FactorName         string             `json:"factorName"`
	NumFolds           int                `json:"numFolds"`
	Folds              []FoldICResultView `json:"folds"`
	MeanIC22d          float64            `json:"meanIC22d"`
	MinIC22d           float64            `json:"minIC22d"`
	ICStabilityRatio   float64            `json:"icStabilityRatio"`
	AllFoldsQualified  bool               `json:"allFoldsQualified"`
	QualifiedFoldCount int                `json:"qualifiedFoldCount"`
}

// FoldICResultView is one fold's IC summary.
type FoldICResultView struct {
	Index            int       `json:"index"`
	StartDate        time.Time `json:"startDate"`
	EndDate          time.Time `json:"endDate"`
	ObservationDays  int       `json:"observationDays"`
	SpearmanMean     float64   `json:"spearmanMean"`
	SpearmanIR       float64   `json:"spearmanIR"`
	SpearmanTStat    float64   `json:"spearmanTStat"`
	PositiveICRatio  float64   `json:"positiveICRatio"`
	LongShortSharpe  float64   `json:"longShortSharpe"`
	LongShortAnnual  float64   `json:"longShortAnnual"`
	LayeredSpreadAnn float64   `json:"layeredSpreadAnnual"`
	Qualified        bool      `json:"qualified"`
	Error            string    `json:"error,omitempty"`
}

// FactorReportInput is the POST body for /api/factorlab/reports.
type FactorReportInput struct {
	// FactorNames selects which factors to score. Empty = all
	// MVP factors (DefaultFactors()).
	FactorNames []string `json:"factorNames,omitempty"`

	// FixtureName picks which fixture to score. Currently only
	// "synthetic" (default) is supported; future values will
	// reference frozen CSV bundles by short-name.
	FixtureName string `json:"fixtureName,omitempty"`

	// Horizons is the list of forward-return horizons (in trading
	// days) to compute IC at. Empty = [5, 22] (weekly + monthly).
	Horizons []int `json:"horizons,omitempty"`

	// LayeredHorizonDays is the horizon used for the 5-quintile
	// table + long/short. Must be one of Horizons. 0 = largest
	// horizon.
	LayeredHorizonDays int `json:"layeredHorizonDays,omitempty"`

	// SeedOverride lets the caller pin the synthetic fixture's
	// PRNG seed for reproducibility. 0 = adapter default.
	SeedOverride int64 `json:"seedOverride,omitempty"`

	// DaysOverride lets the caller request a longer/shorter
	// synthetic fixture. 0 = adapter default (~3y).
	DaysOverride int `json:"daysOverride,omitempty"`
}

// FactorReportView is the JSON-friendly projection of one
// factorlab.FactorReport. Field names mirror the Go struct so the
// adapter is a single-pass copy.
type FactorReportView struct {
	FactorName string `json:"factorName"`

	StartDate          time.Time `json:"startDate"`
	EndDate            time.Time `json:"endDate"`
	UniverseMedianSize int       `json:"universeMedianSize"`
	ObservationDays    int       `json:"observationDays"`

	// IC, keyed by horizon-in-days. JSON object keys are
	// strings — the adapter does the int→string conversion.
	IC map[string]ICStatsView `json:"ic"`

	Layered   *LayeredResultView   `json:"layered,omitempty"`
	LongShort *LongShortResultView `json:"longShort,omitempty"`

	Qualified  bool                    `json:"qualified"`
	QualReport QualificationReportView `json:"qualReport"`
}

type ICStatsView struct {
	HorizonDays int `json:"horizonDays"`

	PearsonSeries  []float64 `json:"pearsonSeries"`
	SpearmanSeries []float64 `json:"spearmanSeries"`

	PearsonMean  float64 `json:"pearsonMean"`
	PearsonStd   float64 `json:"pearsonStd"`
	PearsonIR    float64 `json:"pearsonIR"`
	PearsonTStat float64 `json:"pearsonTStat"`

	SpearmanMean  float64 `json:"spearmanMean"`
	SpearmanStd   float64 `json:"spearmanStd"`
	SpearmanIR    float64 `json:"spearmanIR"`
	SpearmanTStat float64 `json:"spearmanTStat"`

	PositiveICRatio float64 `json:"positiveICRatio"`
}

type LayeredResultView struct {
	HorizonDays          int        `json:"horizonDays"`
	QuintileMeanReturn   [5]float64 `json:"quintileMeanReturn"`
	QuintileAnnualReturn [5]float64 `json:"quintileAnnualReturn"`
	Spread               float64    `json:"spread"`
	SpreadAnnual         float64    `json:"spreadAnnual"`
	SpreadTStat          float64    `json:"spreadTStat"`
	Monotonic            bool       `json:"monotonic"`
	ObservationPeriods   int        `json:"observationPeriods"`
}

type LongShortResultView struct {
	NavCurve     []FactorNavPoint `json:"navCurve"`
	AnnualReturn float64          `json:"annualReturn"`
	AnnualVol    float64          `json:"annualVol"`
	Sharpe       float64          `json:"sharpe"`
	MaxDrawdown  float64          `json:"maxDrawdown"`
}

type FactorNavPoint struct {
	Date time.Time `json:"date"`
	Nav  float64   `json:"nav"`
}

type QualificationReportView struct {
	HorizonDaysReference int  `json:"horizonDaysReference"`
	PassesIC             bool `json:"passesIC"`
	PassesIR             bool `json:"passesIR"`
	PassesTStat          bool `json:"passesTStat"`
	PassesPositiveRatio  bool `json:"passesPositiveRatio"`
	PassesLongShort      bool `json:"passesLongShort"`
}

// ErrFactorLabUnconfigured is returned when the FactorLabService
// wasn't wired (legacy deployments). The handler translates it
// to a 503.
var ErrFactorLabUnconfigured = errors.New("factorlab service not configured")

// RunFactorReport handles POST /api/factorlab/reports. Body:
// FactorReportInput. Auth: any logged-in user (factor reports are
// market-wide research, not per-fund — but we still require auth
// so the synthetic fixture compute cost can't be DoSed publicly).
//
// Returns 200 with []FactorReportView on success.
//
// Status codes:
//   - 503 when the service isn't wired in this deployment.
//   - 400 on bad input (unknown factor name, bad horizons).
//   - 401 when no session cookie / bearer token present.
func (h *FundHandler) RunFactorReport(w http.ResponseWriter, r *http.Request) {
	if h.factorlab == nil {
		writeError(w, http.StatusServiceUnavailable, "factorlab service not configured", "")
		return
	}
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}

	var input FactorReportInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil && !errors.Is(err, http.ErrBodyNotAllowed) {
		// Empty body is allowed (use all defaults); only bad JSON
		// is a hard fail.
		if !strings.Contains(err.Error(), "EOF") {
			writeError(w, http.StatusBadRequest, "invalid JSON body", err.Error())
			return
		}
	}

	reports, err := h.factorlab.RunFactorReport(userID, input)
	if err != nil {
		if errors.Is(err, ErrFactorLabUnconfigured) {
			writeError(w, http.StatusServiceUnavailable, err.Error(), "")
			return
		}
		writeError(w, http.StatusBadRequest, "factorlab run failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, reports)
}

// RunWalkForwardFactor handles POST /api/factorlab/walkforward.
// Same auth/error pattern as RunFactorReport.
func (h *FundHandler) RunWalkForwardFactor(w http.ResponseWriter, r *http.Request) {
	if h.factorlab == nil {
		writeError(w, http.StatusServiceUnavailable, "factorlab service not configured", "")
		return
	}
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}

	var input WalkForwardFactorInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil && !errors.Is(err, http.ErrBodyNotAllowed) {
		if !strings.Contains(err.Error(), "EOF") {
			writeError(w, http.StatusBadRequest, "invalid JSON body", err.Error())
			return
		}
	}
	if strings.TrimSpace(input.FactorName) == "" {
		writeError(w, http.StatusBadRequest, "factorName is required", "")
		return
	}

	result, err := h.factorlab.RunWalkForwardFactor(userID, input)
	if err != nil {
		if errors.Is(err, ErrFactorLabUnconfigured) {
			writeError(w, http.StatusServiceUnavailable, err.Error(), "")
			return
		}
		writeError(w, http.StatusBadRequest, "walkforward run failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}
