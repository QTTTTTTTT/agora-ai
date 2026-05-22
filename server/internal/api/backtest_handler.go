package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// BacktestService is the seam between the HTTP layer and the
// in-memory backtest.JobStore. Implementations live in
// cmd/server/wiring_adapters.go so the api package stays free of
// the backtest package's transitive imports (ohlc / decision /
// repository / etc).
//
// Authorisation is handled by the implementation: every method
// receives userID and is expected to check the fund's owner
// before mutating / reading the per-fund job list.
type BacktestService interface {
	// SubmitBacktest enqueues a new run for the given fund and
	// returns the queued job snapshot. The caller polls
	// GetBacktest periodically for progress + the final result.
	SubmitBacktest(userID string, input SubmitBacktestInput) (*BacktestJob, error)
	// ListBacktests returns the newest-first job ledger for one
	// fund. Returns an empty slice when there's no history yet.
	ListBacktests(userID, fundID string) ([]*BacktestJob, error)
	// GetBacktest returns one job by ID. Returns nil when not
	// found.
	GetBacktest(userID, fundID, jobID string) (*BacktestJob, error)
	// CancelBacktest signals the runner to abort. Returns true
	// when the job was found AND was cancelable (i.e., not
	// already completed/failed). No-op for completed jobs.
	CancelBacktest(userID, fundID, jobID string) (bool, error)
	// CompareBacktests returns a side-by-side projection of two
	// completed jobs in the same fund. Either of the two jobIDs
	// missing or not-yet-complete yields ErrBacktestNotComparable.
	// Caller is expected to pre-filter the UI so only completed
	// jobs are selectable; this guard catches races.
	CompareBacktests(userID, fundID, jobIDA, jobIDB string) (*BacktestComparison, error)
	// SubmitSweep fans out a parameter sweep into N child
	// backtest jobs (one per Cartesian cell of the supplied
	// axes) and returns the sweep header + child snapshots.
	// Validation errors (too many cells, unknown axis,
	// invalid value) surface as ErrSweepInvalid.
	SubmitSweep(userID string, input SubmitSweepInput) (*BacktestSweep, error)
	// ListSweeps returns the newest-first sweep ledger for one
	// fund. Empty slice on no history.
	ListSweeps(userID, fundID string) ([]*BacktestSweep, error)
	// GetSweep returns one sweep + its current child snapshots
	// (live progress from the in-memory store when active,
	// persisted state otherwise). Nil when not found.
	GetSweep(userID, fundID, sweepID string) (*BacktestSweep, error)
	// SweepAxisCatalog reports the set of axis names the
	// caller may vary. The web UI uses this to populate the
	// "axis" picker without hard-coding the list.
	SweepAxisCatalog() []string
}

// SubmitBacktestInput mirrors the public POST body. We keep it
// distinct from backtest.Request so the api package doesn't have
// to import the backtest package — the wiring layer translates
// between them.
type SubmitBacktestInput struct {
	FundID           string                     `json:"fundId"`
	Name             string                     `json:"name,omitempty"`
	Market           string                     `json:"market,omitempty"`
	Symbols          []string                   `json:"symbols"`
	InitialPositions []BacktestInitialPosition  `json:"initialPositions,omitempty"`
	Start            time.Time                  `json:"start"`
	End              time.Time                  `json:"end"`
	InitialCash      float64                    `json:"initialCash"`
	BaseCurrency     string                     `json:"baseCurrency,omitempty"`
	SlippageBps      float64                    `json:"slippageBps,omitempty"`
	CommissionBps    float64                    `json:"commissionBps,omitempty"`
	MaxOrdersPerDay  int                        `json:"maxOrdersPerDay,omitempty"`
	EngineKind       string                     `json:"engineKind,omitempty"`
	// WalkForward, when non-nil, asks the runner to perform a
	// chunked out-of-sample validation: split the window into
	// NumFolds equal test segments, optionally with a leading
	// train window per fold (informational; the platform's
	// decision engines are stateless). The Result echoes a
	// per-fold breakdown for the UI.
	WalkForward *WalkForwardInput `json:"walkForward,omitempty"`
}

// WalkForwardInput is the JSON shape for the sub-spec. Mirrors
// backtest.WalkForwardSpec but lives in the api package so the
// handler doesn't have to import backtest.
type WalkForwardInput struct {
	NumFolds   int     `json:"numFolds"`
	TrainRatio float64 `json:"trainRatio,omitempty"`
	Mode       string  `json:"mode,omitempty"` // "anchored" | "rolling"
}

// BacktestInitialPosition is the JSON shape for a pre-loaded
// holding. Mirrors backtest.InitialPosition.
type BacktestInitialPosition struct {
	Symbol    string  `json:"symbol"`
	Quantity  float64 `json:"quantity"`
	CostPrice float64 `json:"costPrice"`
}

// BacktestJob is the JSON-friendly per-job snapshot returned by
// every endpoint. Result is only populated when Status ==
// "completed"; Error is populated when Status == "failed".
type BacktestJob struct {
	ID          string                `json:"id"`
	FundID      string                `json:"fundId"`
	Name        string                `json:"name"`
	EngineKind  string                `json:"engineKind"`
	Status      string                `json:"status"`
	Progress    BacktestProgressView  `json:"progress"`
	SubmittedAt time.Time             `json:"submittedAt"`
	StartedAt   time.Time             `json:"startedAt,omitempty"`
	CompletedAt time.Time             `json:"completedAt,omitempty"`
	Error       string                `json:"error,omitempty"`
	Result      *BacktestResultView   `json:"result,omitempty"`
	Request     *BacktestRequestEcho  `json:"request,omitempty"`
}

// BacktestProgressView is the polling-friendly fragment.
type BacktestProgressView struct {
	TotalDays   int       `json:"totalDays"`
	DoneDays    int       `json:"doneDays"`
	CurrentDate time.Time `json:"currentDate,omitempty"`
}

// BacktestRequestEcho echoes the original submission so clients
// don't need to maintain their own copy.
type BacktestRequestEcho struct {
	Symbols         []string                  `json:"symbols"`
	Start           time.Time                 `json:"start"`
	End             time.Time                 `json:"end"`
	InitialCash     float64                   `json:"initialCash"`
	BaseCurrency    string                    `json:"baseCurrency,omitempty"`
	SlippageBps     float64                   `json:"slippageBps"`
	CommissionBps   float64                   `json:"commissionBps"`
	MaxOrdersPerDay int                       `json:"maxOrdersPerDay"`
	InitialPositions []BacktestInitialPosition `json:"initialPositions,omitempty"`
	WalkForward     *WalkForwardInput         `json:"walkForward,omitempty"`
}

// BacktestResultView is the final output. Wraps the NAV curve and
// trades in JSON-friendly shapes.
type BacktestResultView struct {
	InitialCash float64                  `json:"initialCash"`
	FinalNav    float64                  `json:"finalNav"`
	NavCurve    []BacktestNavPoint       `json:"navCurve"`
	Trades      []BacktestTradeEvent     `json:"trades"`
	Metrics     BacktestMetricsView      `json:"metrics"`
	CompletedAt time.Time                `json:"completedAt,omitempty"`
	WalkForward *WalkForwardResultView   `json:"walkForward,omitempty"`
}

// WalkForwardResultView is the JSON-friendly per-fold breakdown
// for runs that used the walkForward sub-spec.
type WalkForwardResultView struct {
	Spec            WalkForwardInput        `json:"spec"`
	Mode            string                  `json:"mode"`
	Folds           []WalkForwardFoldView   `json:"folds"`
	OOSReturn       float64                 `json:"oosReturn"`
	OOSSharpe       float64                 `json:"oosSharpe"`
	MeanFoldReturn  float64                 `json:"meanFoldReturn"`
	WorstFoldReturn float64                 `json:"worstFoldReturn"`
	BestFoldReturn  float64                 `json:"bestFoldReturn"`
	// FoldBoundaries indexes into NavCurve — each entry is the
	// first NavCurve index of a new fold. The UI draws vertical
	// separators on the chart at these positions.
	FoldBoundaries []int `json:"foldBoundaries"`
}

// WalkForwardFoldView is one fold's row in the per-fold table.
type WalkForwardFoldView struct {
	Index      int                 `json:"index"`
	TestStart  time.Time           `json:"testStart"`
	TestEnd    time.Time           `json:"testEnd"`
	InitialNav float64             `json:"initialNav"`
	FinalNav   float64             `json:"finalNav"`
	Return     float64             `json:"return"`
	Metrics    BacktestMetricsView `json:"metrics"`
	TradeCount int                 `json:"tradeCount"`
	Error      string              `json:"error,omitempty"`
}

type BacktestNavPoint struct {
	Date          time.Time          `json:"date"`
	Nav           float64            `json:"nav"`
	Cash          float64            `json:"cash"`
	PositionValue float64            `json:"positionValue"`
	DrawdownPct   float64            `json:"drawdownPct"`
	Positions     map[string]float64 `json:"positions,omitempty"`
}

type BacktestTradeEvent struct {
	Date       time.Time `json:"date"`
	Symbol     string    `json:"symbol"`
	Action     string    `json:"action"`
	Status     string    `json:"status"`
	Quantity   float64   `json:"quantity,omitempty"`
	FillPrice  float64   `json:"fillPrice,omitempty"`
	Notional   float64   `json:"notional,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Confidence float64   `json:"confidence,omitempty"`
}

type BacktestMetricsView struct {
	CumulativeReturn  float64 `json:"cumulativeReturn"`
	AnnualizedReturn  float64 `json:"annualizedReturn"`
	Volatility        float64 `json:"volatility"`
	SharpeRatio       float64 `json:"sharpeRatio"`
	MaxDrawdown       float64 `json:"maxDrawdown"`
	WinRate           float64 `json:"winRate"`
	TradeCount        int     `json:"tradeCount"`
	WinningTradeCount int     `json:"winningTradeCount"`
	LosingTradeCount  int     `json:"losingTradeCount"`
}

// ErrBacktestUnconfigured is returned when the BacktestService
// wasn't wired (legacy deployments without OHLC). The handler
// translates it to a 503.
var ErrBacktestUnconfigured = errors.New("backtest service not configured")

// ErrSweepInvalid wraps a validation failure from the underlying
// backtest.ExpandSweep call (too many cells, unknown axis,
// invalid value). The handler translates it to a 400.
var ErrSweepInvalid = errors.New("sweep request invalid")

// ErrWalkForwardInvalid wraps validation failures from the
// backtest.PlanWalkForward call (bad fold count, window too short
// for the requested folds, malformed ratio). The handler returns
// 400.
var ErrWalkForwardInvalid = errors.New("walkForward sub-spec invalid")

// SubmitSweepInput is the JSON body for POST /sweeps. Base is
// the template request (same shape as a single Submit); Axes is
// the list of dimensions to vary. The wire format is intentionally
// dumb so the UI can serialise it from a 3-field form.
type SubmitSweepInput struct {
	FundID string                   `json:"fundId"`
	Name   string                   `json:"name,omitempty"`
	Base   SubmitBacktestInput      `json:"base"`
	Axes   []SubmitSweepAxisInput   `json:"axes"`
}

// SubmitSweepAxisInput is one varying dimension. Values are kept
// as strings on the wire so the caller doesn't need to know the
// underlying scalar type per axis (float for slippage, int for
// maxOrders, enum for engineKind).
type SubmitSweepAxisInput struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

// BacktestSweep is the JSON-friendly sweep view. Children is
// either: (a) snapshots of the live in-memory jobs when the sweep
// is mid-flight, or (b) the historical persisted projections after
// completion. The status field aggregates children's statuses
// (see deriveSweepStatus in the adapter).
type BacktestSweep struct {
	ID         string                  `json:"id"`
	FundID     string                  `json:"fundId"`
	Name       string                  `json:"name"`
	Status     string                  `json:"status"`
	TotalCells int                     `json:"totalCells"`
	DoneCells  int                     `json:"doneCells"`
	CreatedAt  time.Time               `json:"createdAt"`
	Base       *BacktestRequestEcho    `json:"base,omitempty"`
	Axes       []SubmitSweepAxisInput  `json:"axes"`
	Children   []*BacktestSweepChild   `json:"children,omitempty"`
}

// BacktestSweepChild is a thin reference to one cell's job +
// the axis-name → value map that identifies which cell it is.
// The full BacktestJob view is embedded so the UI can render a
// grid cell without a second lookup.
type BacktestSweepChild struct {
	Job        *BacktestJob      `json:"job"`
	AxisValues map[string]string `json:"axisValues"`
}

// ErrBacktestNotComparable is returned when either of the two
// jobIDs in a CompareBacktests call hasn't completed (or doesn't
// exist). The handler translates it to a 409 so the web UI can
// surface a friendly "wait until both runs finish" toast.
var ErrBacktestNotComparable = errors.New("backtest not comparable: both jobs must be completed")

// BacktestComparison is the JSON payload for the compare endpoint.
// Two job views + a small derived diff block so the UI doesn't
// have to recompute the delta on every render.
type BacktestComparison struct {
	A     *BacktestJob          `json:"a"`
	B     *BacktestJob          `json:"b"`
	Diff  BacktestComparisonDiff `json:"diff"`
}

// BacktestComparisonDiff holds B - A for every comparable metric.
// All deltas are returned as raw differences (not %-of-A) so the
// UI can render either "+0.12 Sharpe" or "+12% Sharpe vs A" as
// appropriate.
type BacktestComparisonDiff struct {
	CumulativeReturnDelta float64 `json:"cumulativeReturnDelta"`
	AnnualizedReturnDelta float64 `json:"annualizedReturnDelta"`
	VolatilityDelta       float64 `json:"volatilityDelta"`
	SharpeDelta           float64 `json:"sharpeDelta"`
	MaxDrawdownDelta      float64 `json:"maxDrawdownDelta"`
	WinRateDelta          float64 `json:"winRateDelta"`
	TradeCountDelta       int     `json:"tradeCountDelta"`
	FinalNavDelta         float64 `json:"finalNavDelta"`
	// SameWindow reports whether the two runs cover the exact
	// same [start, end] window. When false the deltas are still
	// computed but the UI surfaces a warning that the comparison
	// isn't apples-to-apples.
	SameWindow bool `json:"sameWindow"`
	// SameUniverse reports whether the two runs ran against the
	// exact same symbol list (set-equality, order-insensitive).
	SameUniverse bool `json:"sameUniverse"`
}

// SubmitBacktest handles POST /api/funds/{fundId}/backtests.
//
// Body: SubmitBacktestInput. The handler injects fundId from the
// URL so a body mismatch is treated as a 400.
//
// Response: 201 Created with the queued job snapshot.
func (h *FundHandler) SubmitBacktest(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	if h.backtests == nil {
		writeError(w, http.StatusServiceUnavailable, "backtest service unavailable", ErrBacktestUnconfigured.Error())
		return
	}
	var input SubmitBacktestInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	// The URL is the authoritative fundId; we override the body
	// so a client typo can't cross-submit to a different fund.
	input.FundID = fundID
	if len(input.Symbols) == 0 && len(input.InitialPositions) == 0 {
		writeError(w, http.StatusBadRequest, "invalid request", "symbols or initialPositions required")
		return
	}
	if input.Start.IsZero() || input.End.IsZero() || !input.End.After(input.Start) {
		writeError(w, http.StatusBadRequest, "invalid request", "start/end required, end must be after start")
		return
	}
	if input.InitialCash <= 0 && len(input.InitialPositions) == 0 {
		writeError(w, http.StatusBadRequest, "invalid request", "initialCash > 0 required when initialPositions empty")
		return
	}
	job, err := h.backtests.SubmitBacktest(userID, input)
	if err != nil {
		if errors.Is(err, ErrWalkForwardInvalid) {
			writeError(w, http.StatusBadRequest, "invalid walkForward", err.Error())
			return
		}
		handleServiceError(w, err, "backtest")
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

// ListBacktests handles GET /api/funds/{fundId}/backtests.
func (h *FundHandler) ListBacktests(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	if h.backtests == nil {
		// Empty list is the friendly response for unconfigured
		// deployments: the web UI gets to render the form
		// without an error toast.
		writeJSON(w, http.StatusOK, []*BacktestJob{})
		return
	}
	jobs, err := h.backtests.ListBacktests(userID, fundID)
	if err != nil {
		handleServiceError(w, err, "backtest")
		return
	}
	if jobs == nil {
		jobs = []*BacktestJob{}
	}
	writeJSON(w, http.StatusOK, jobs)
}

// GetBacktest handles GET /api/funds/{fundId}/backtests/{jobId}.
func (h *FundHandler) GetBacktest(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	jobID := pathValue(r, "jobId")
	if !requireNonEmpty(w, fundID, "fundId") || !requireNonEmpty(w, jobID, "jobId") {
		return
	}
	if h.backtests == nil {
		writeError(w, http.StatusServiceUnavailable, "backtest service unavailable", ErrBacktestUnconfigured.Error())
		return
	}
	job, err := h.backtests.GetBacktest(userID, fundID, jobID)
	if err != nil {
		handleServiceError(w, err, "backtest")
		return
	}
	if job == nil {
		writeError(w, http.StatusNotFound, "backtest not found", "")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// CancelBacktest handles POST /api/funds/{fundId}/backtests/{jobId}/cancel.
func (h *FundHandler) CancelBacktest(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	jobID := pathValue(r, "jobId")
	if !requireNonEmpty(w, fundID, "fundId") || !requireNonEmpty(w, jobID, "jobId") {
		return
	}
	if h.backtests == nil {
		writeError(w, http.StatusServiceUnavailable, "backtest service unavailable", ErrBacktestUnconfigured.Error())
		return
	}
	cancelled, err := h.backtests.CancelBacktest(userID, fundID, jobID)
	if err != nil {
		handleServiceError(w, err, "backtest")
		return
	}
	if !cancelled {
		writeError(w, http.StatusConflict, "backtest already terminated or unknown", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
}

// CompareBacktests handles GET /api/funds/{fundId}/backtests/compare?a=X&b=Y.
// Both jobIDs are required and must reference completed jobs in
// the URL's fundId. The handler returns 409 when either job
// isn't ready for comparison (via ErrBacktestNotComparable).
func (h *FundHandler) CompareBacktests(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	if h.backtests == nil {
		writeError(w, http.StatusServiceUnavailable, "backtest service unavailable", ErrBacktestUnconfigured.Error())
		return
	}
	a := strings.TrimSpace(r.URL.Query().Get("a"))
	b := strings.TrimSpace(r.URL.Query().Get("b"))
	if a == "" || b == "" {
		writeError(w, http.StatusBadRequest, "invalid request", "query params a and b required")
		return
	}
	if a == b {
		writeError(w, http.StatusBadRequest, "invalid request", "a and b must reference different jobs")
		return
	}
	cmp, err := h.backtests.CompareBacktests(userID, fundID, a, b)
	if err != nil {
		if errors.Is(err, ErrBacktestNotComparable) {
			writeError(w, http.StatusConflict, "backtest not comparable", err.Error())
			return
		}
		handleServiceError(w, err, "backtest")
		return
	}
	if cmp == nil {
		writeError(w, http.StatusNotFound, "backtest not found", "")
		return
	}
	writeJSON(w, http.StatusOK, cmp)
}

// SubmitSweep handles POST /api/funds/{fundId}/backtests/sweeps.
// Body shape is SubmitSweepInput. Returns the queued sweep + all
// child jobs in 'queued' state. Validation errors (too many
// cells, unknown axis, malformed value) return 400.
func (h *FundHandler) SubmitSweep(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	if h.backtests == nil {
		writeError(w, http.StatusServiceUnavailable, "backtest service unavailable", ErrBacktestUnconfigured.Error())
		return
	}
	var input SubmitSweepInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request", err.Error())
		return
	}
	input.FundID = fundID
	if len(input.Base.Symbols) == 0 {
		writeError(w, http.StatusBadRequest, "invalid request", "base.symbols required")
		return
	}
	if len(input.Axes) == 0 {
		writeError(w, http.StatusBadRequest, "invalid request", "axes required (use POST /backtests for one-off runs)")
		return
	}
	sweep, err := h.backtests.SubmitSweep(userID, input)
	if err != nil {
		if errors.Is(err, ErrSweepInvalid) {
			writeError(w, http.StatusBadRequest, "invalid sweep", err.Error())
			return
		}
		handleServiceError(w, err, "sweep")
		return
	}
	writeJSON(w, http.StatusAccepted, sweep)
}

// ListSweeps handles GET /api/funds/{fundId}/backtests/sweeps.
func (h *FundHandler) ListSweeps(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	if h.backtests == nil {
		writeError(w, http.StatusServiceUnavailable, "backtest service unavailable", ErrBacktestUnconfigured.Error())
		return
	}
	out, err := h.backtests.ListSweeps(userID, fundID)
	if err != nil {
		handleServiceError(w, err, "sweep")
		return
	}
	if out == nil {
		out = []*BacktestSweep{}
	}
	writeJSON(w, http.StatusOK, out)
}

// GetSweep handles GET /api/funds/{fundId}/backtests/sweeps/{sweepId}.
func (h *FundHandler) GetSweep(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	sweepID := pathValue(r, "sweepId")
	if !requireNonEmpty(w, fundID, "fundId") || !requireNonEmpty(w, sweepID, "sweepId") {
		return
	}
	if h.backtests == nil {
		writeError(w, http.StatusServiceUnavailable, "backtest service unavailable", ErrBacktestUnconfigured.Error())
		return
	}
	sweep, err := h.backtests.GetSweep(userID, fundID, sweepID)
	if err != nil {
		handleServiceError(w, err, "sweep")
		return
	}
	if sweep == nil {
		writeError(w, http.StatusNotFound, "sweep not found", "")
		return
	}
	writeJSON(w, http.StatusOK, sweep)
}

// SweepAxisCatalog handles GET /api/backtests/sweeps/axes. It's
// fund-independent — the same set of axes is valid for every
// fund. We expose this endpoint so the web UI can populate its
// "axis" dropdown without hard-coding the allow-list.
func (h *FundHandler) SweepAxisCatalog(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAuthenticatedUserID(w, r); !ok {
		return
	}
	if h.backtests == nil {
		writeError(w, http.StatusServiceUnavailable, "backtest service unavailable", ErrBacktestUnconfigured.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"axes": h.backtests.SweepAxisCatalog()})
}

// normaliseEngineKind keeps the supported strings centralised so
// the wiring layer doesn't drift from the handler validation.
// Empty / unknown values collapse to "fallback".
func normaliseEngineKind(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "llm":
		return "llm"
	case "llm-debate", "llm_debate", "debate":
		return "llm-debate"
	case "fallback", "":
		return "fallback"
	default:
		return "fallback"
	}
}
