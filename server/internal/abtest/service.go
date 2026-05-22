package abtest

import (
	"context"
	"fmt"
	"log"
	"math"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Status and variable-type constants
// ---------------------------------------------------------------------------

type ABTestStatus string

const (
	StatusDraft     ABTestStatus = "draft"
	StatusRunning   ABTestStatus = "running"
	StatusCompleted ABTestStatus = "completed"
	StatusAnalyzed  ABTestStatus = "analyzed"
)

const (
	VarAgentSwap   = "agent_swap"
	VarRiskRule    = "risk_rule"
	VarPMStrategy  = "pm_strategy"
	VarModelChange = "model_change"
	VarSkillChange = "skill_change"
)

// validVariableTypes is the set of recognised test-variable categories.
var validVariableTypes = map[string]bool{
	VarAgentSwap:   true,
	VarRiskRule:    true,
	VarPMStrategy:  true,
	VarModelChange: true,
	VarSkillChange: true,
}

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

// ABTest represents a single A/B experiment that compares a control fund
// against a treatment fund that differs by exactly one variable.
type ABTest struct {
	ID            string
	Name          string
	ControlFundID string
	TreatFundID   string // cloned fund
	Variable      TestVariable
	Status        ABTestStatus // draft → running → completed → analyzed
	StartDate     string       // YYYY-MM-DD
	EndDate       string       // YYYY-MM-DD
	CreatedAt     time.Time
	Results       *ABTestResults
}

// TestVariable describes the single change applied to the treatment fund.
type TestVariable struct {
	Type        string // agent_swap, risk_rule, pm_strategy, model_change, skill_change
	Target      string // which agent/rule is being changed
	OldValue    string // JSON of original config
	NewValue    string // JSON of new config
	Description string
}

// ABTestResults holds the post-experiment analysis.
type ABTestResults struct {
	ControlMetrics   FundMetrics
	TreatmentMetrics FundMetrics
	Divergences      []DivergencePoint
	Summary          string
	Winner           string  // "control", "treatment", "inconclusive"
	Confidence       float64 // 0-1 statistical confidence
}

// FundMetrics captures quantitative performance of a single fund over the
// test window.
type FundMetrics struct {
	TotalReturn  float64
	AnnualReturn float64
	Sharpe       float64
	MaxDrawdown  float64
	Volatility   float64
	WinRate      float64
	TotalTrades  int
	NAVHistory   []NAVPoint
}

// NAVPoint is a single date→NAV observation.
type NAVPoint struct {
	Date string
	NAV  float64
}

// DivergencePoint records a moment where the two funds made different
// decisions and the downstream impact of that divergence.
type DivergencePoint struct {
	Date            string
	ControlAction   string
	TreatmentAction string
	ControlResult   float64
	TreatmentResult float64
	Impact          float64
}

// TradeRecord is a minimal representation of a single trade; the concrete
// definition lives in the provider but we need a local type so that
// interfaces can reference it without import cycles.
type TradeRecord struct {
	Date      string
	Symbol    string
	Side      string // "buy" or "sell"
	Quantity  float64
	Price     float64
	PnL       float64
	Rationale string
}

// ---------------------------------------------------------------------------
// Dependency interfaces
// ---------------------------------------------------------------------------

// FundCloner deep-copies a fund configuration and all associated state so
// that the clone can run independently.
type FundCloner interface {
	CloneFund(ctx context.Context, fundID string, suffix string) (newFundID string, err error)
}

// FundConfigUpdater applies a single variable change to an already-cloned
// fund so that it becomes the treatment group.
type FundConfigUpdater interface {
	ApplyVariable(ctx context.Context, fundID string, variable TestVariable) error
}

// FundRunner drives the daily simulation loop for a single fund over the
// specified date range. It is expected to block until the run finishes or
// the context is cancelled.
type FundRunner interface {
	RunFund(ctx context.Context, fundID string, startDate, endDate string) error
}

// MetricsCollector gathers quantitative performance data for a fund over a
// date range.
type MetricsCollector interface {
	CollectMetrics(ctx context.Context, fundID string, start, end string) (FundMetrics, error)
}

// TradeHistoryProvider returns every trade executed by a fund in the window.
type TradeHistoryProvider interface {
	GetTrades(ctx context.Context, fundID string, start, end string) ([]TradeRecord, error)
}

// LLMAnalyzer uses an LLM to compare two trade histories and produce
// human-readable divergence analysis.
type LLMAnalyzer interface {
	AnalyzeDivergence(ctx context.Context, control, treatment []TradeRecord) ([]DivergencePoint, string, error)
}

// ---------------------------------------------------------------------------
// ABTestService
// ---------------------------------------------------------------------------

// ABTestService manages the full lifecycle of A/B tests: creation, parallel
// execution, metric collection, and post-hoc analysis.
type ABTestService struct {
	mu sync.RWMutex

	tests map[string]*ABTest // keyed by test ID

	cloner   FundCloner
	updater  FundConfigUpdater
	runner   FundRunner
	metrics  MetricsCollector
	trades   TradeHistoryProvider
	analyzer LLMAnalyzer

	// cancels holds context-cancel functions for running tests so that
	// Stop can terminate them early.
	cancels map[string]context.CancelFunc

	nextID int
}

// NewABTestService wires all dependencies and returns a ready-to-use service.
func NewABTestService(
	cloner FundCloner,
	updater FundConfigUpdater,
	runner FundRunner,
	metrics MetricsCollector,
	trades TradeHistoryProvider,
	analyzer LLMAnalyzer,
) *ABTestService {
	return &ABTestService{
		tests:   make(map[string]*ABTest),
		cancels: make(map[string]context.CancelFunc),
		cloner:  cloner,
		updater: updater,
		runner:  runner,
		metrics: metrics,
		trades:  trades,
		analyzer: analyzer,
	}
}

// ---------------------------------------------------------------------------
// Lifecycle: Create
// ---------------------------------------------------------------------------

// CreateTest sets up a new A/B test in draft status. It clones the control
// fund and applies the requested variable change to produce the treatment
// fund. The test is not started until Start is called.
func (s *ABTestService) CreateTest(
	ctx context.Context,
	name string,
	controlFundID string,
	variable TestVariable,
	startDate, endDate string,
) (*ABTest, error) {
	if name == "" {
		return nil, fmt.Errorf("abtest: name must not be empty")
	}
	if controlFundID == "" {
		return nil, fmt.Errorf("abtest: controlFundID must not be empty")
	}
	if !validVariableTypes[variable.Type] {
		return nil, fmt.Errorf("abtest: unknown variable type %q", variable.Type)
	}
	if err := validateDateRange(startDate, endDate); err != nil {
		return nil, fmt.Errorf("abtest: %w", err)
	}

	// Clone the fund for the treatment group.
	suffix := fmt.Sprintf("_abtest_%s", time.Now().Format("20060102150405"))
	treatFundID, err := s.cloner.CloneFund(ctx, controlFundID, suffix)
	if err != nil {
		return nil, fmt.Errorf("abtest: failed to clone fund %s: %w", controlFundID, err)
	}

	// Apply the single variable change.
	if err := s.updater.ApplyVariable(ctx, treatFundID, variable); err != nil {
		return nil, fmt.Errorf("abtest: failed to apply variable to treatment fund %s: %w", treatFundID, err)
	}

	s.mu.Lock()
	s.nextID++
	id := fmt.Sprintf("abt_%d", s.nextID)
	test := &ABTest{
		ID:            id,
		Name:          name,
		ControlFundID: controlFundID,
		TreatFundID:   treatFundID,
		Variable:      variable,
		Status:        StatusDraft,
		StartDate:     startDate,
		EndDate:       endDate,
		CreatedAt:     time.Now(),
	}
	s.tests[id] = test
	s.mu.Unlock()

	log.Printf("[abtest] created test %s (%s): control=%s treatment=%s variable=%s",
		id, name, controlFundID, treatFundID, variable.Type)
	return test, nil
}

// ---------------------------------------------------------------------------
// Lifecycle: Start
// ---------------------------------------------------------------------------

// StartTest transitions a draft test to running and launches both funds in
// parallel. The method returns immediately; execution proceeds in the
// background. When both funds finish the test moves to completed.
func (s *ABTestService) StartTest(ctx context.Context, testID string) error {
	s.mu.Lock()
	test, ok := s.tests[testID]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("abtest: test %s not found", testID)
	}
	if test.Status != StatusDraft {
		s.mu.Unlock()
		return fmt.Errorf("abtest: test %s is in status %s, expected draft", testID, test.Status)
	}
	test.Status = StatusRunning

	runCtx, cancel := context.WithCancel(ctx)
	s.cancels[testID] = cancel
	s.mu.Unlock()

	log.Printf("[abtest] starting test %s: running funds %s and %s from %s to %s",
		testID, test.ControlFundID, test.TreatFundID, test.StartDate, test.EndDate)

	go s.runParallel(runCtx, test)
	return nil
}

// runParallel executes control and treatment funds concurrently, waits for
// both to finish, and marks the test as completed.
func (s *ABTestService) runParallel(ctx context.Context, test *ABTest) {
	var wg sync.WaitGroup
	errs := make([]error, 2)

	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = s.runner.RunFund(ctx, test.ControlFundID, test.StartDate, test.EndDate)
	}()
	go func() {
		defer wg.Done()
		errs[1] = s.runner.RunFund(ctx, test.TreatFundID, test.StartDate, test.EndDate)
	}()
	wg.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.cancels, test.ID)

	if errs[0] != nil || errs[1] != nil {
		log.Printf("[abtest] test %s finished with errors: control=%v treatment=%v",
			test.ID, errs[0], errs[1])
	}
	// Even partial runs move to completed so analysis can inspect whatever
	// data was generated.
	test.Status = StatusCompleted
	log.Printf("[abtest] test %s completed", test.ID)
}

// ---------------------------------------------------------------------------
// Lifecycle: Stop (early termination)
// ---------------------------------------------------------------------------

// StopTest cancels a running test. Both fund simulations are interrupted
// and the test moves to completed so that partial results can still be
// analyzed.
func (s *ABTestService) StopTest(testID string) error {
	s.mu.Lock()
	test, ok := s.tests[testID]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("abtest: test %s not found", testID)
	}
	if test.Status != StatusRunning {
		s.mu.Unlock()
		return fmt.Errorf("abtest: test %s is not running (status=%s)", testID, test.Status)
	}
	cancel, hasCancelFn := s.cancels[testID]
	s.mu.Unlock()

	if hasCancelFn {
		cancel()
	}

	// The runParallel goroutine will observe the cancelled context and
	// transition to completed. Give it a moment then force-set if needed.
	time.Sleep(200 * time.Millisecond)

	s.mu.Lock()
	if test.Status == StatusRunning {
		test.Status = StatusCompleted
		delete(s.cancels, testID)
	}
	s.mu.Unlock()

	log.Printf("[abtest] test %s stopped early", testID)
	return nil
}

// ---------------------------------------------------------------------------
// Lifecycle: Analyze
// ---------------------------------------------------------------------------

// AnalyzeTest collects metrics for both funds, asks the LLM to detect
// divergences, runs a statistical significance check, and stores the
// results. The test transitions to analyzed.
func (s *ABTestService) AnalyzeTest(ctx context.Context, testID string) (*ABTestResults, error) {
	s.mu.RLock()
	test, ok := s.tests[testID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("abtest: test %s not found", testID)
	}
	if test.Status != StatusCompleted {
		return nil, fmt.Errorf("abtest: test %s must be completed before analysis (status=%s)", testID, test.Status)
	}

	log.Printf("[abtest] analyzing test %s", testID)

	// --- Collect metrics in parallel -----------------------------------------
	type metricsResult struct {
		fm  FundMetrics
		err error
	}
	controlCh := make(chan metricsResult, 1)
	treatCh := make(chan metricsResult, 1)

	go func() {
		fm, err := s.metrics.CollectMetrics(ctx, test.ControlFundID, test.StartDate, test.EndDate)
		controlCh <- metricsResult{fm, err}
	}()
	go func() {
		fm, err := s.metrics.CollectMetrics(ctx, test.TreatFundID, test.StartDate, test.EndDate)
		treatCh <- metricsResult{fm, err}
	}()

	cr := <-controlCh
	tr := <-treatCh
	if cr.err != nil {
		return nil, fmt.Errorf("abtest: failed to collect control metrics: %w", cr.err)
	}
	if tr.err != nil {
		return nil, fmt.Errorf("abtest: failed to collect treatment metrics: %w", tr.err)
	}

	// --- Collect trade histories in parallel ---------------------------------
	type tradesResult struct {
		trades []TradeRecord
		err    error
	}
	ctCh := make(chan tradesResult, 1)
	ttCh := make(chan tradesResult, 1)

	go func() {
		t, err := s.trades.GetTrades(ctx, test.ControlFundID, test.StartDate, test.EndDate)
		ctCh <- tradesResult{t, err}
	}()
	go func() {
		t, err := s.trades.GetTrades(ctx, test.TreatFundID, test.StartDate, test.EndDate)
		ttCh <- tradesResult{t, err}
	}()

	ctRes := <-ctCh
	ttRes := <-ttCh
	if ctRes.err != nil {
		return nil, fmt.Errorf("abtest: failed to get control trades: %w", ctRes.err)
	}
	if ttRes.err != nil {
		return nil, fmt.Errorf("abtest: failed to get treatment trades: %w", ttRes.err)
	}

	// --- LLM divergence analysis --------------------------------------------
	divergences, summary, err := s.analyzer.AnalyzeDivergence(ctx, ctRes.trades, ttRes.trades)
	if err != nil {
		return nil, fmt.Errorf("abtest: LLM divergence analysis failed: %w", err)
	}

	// --- Statistical significance -------------------------------------------
	confidence := computeConfidence(cr.fm, tr.fm)

	// --- Determine winner ---------------------------------------------------
	winner := determineWinner(cr.fm, tr.fm, confidence)

	results := &ABTestResults{
		ControlMetrics:   cr.fm,
		TreatmentMetrics: tr.fm,
		Divergences:      divergences,
		Summary:          summary,
		Winner:           winner,
		Confidence:       confidence,
	}

	s.mu.Lock()
	test.Results = results
	test.Status = StatusAnalyzed
	s.mu.Unlock()

	log.Printf("[abtest] test %s analyzed: winner=%s confidence=%.2f", testID, winner, confidence)
	return results, nil
}

// ---------------------------------------------------------------------------
// Query helpers
// ---------------------------------------------------------------------------

// GetTest returns a snapshot of a test by ID.
func (s *ABTestService) GetTest(testID string) (*ABTest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tests[testID]
	if !ok {
		return nil, fmt.Errorf("abtest: test %s not found", testID)
	}
	return t, nil
}

// ListTests returns all tests, optionally filtered by status. Pass an empty
// status to list everything.
func (s *ABTestService) ListTests(status ABTestStatus) []*ABTest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*ABTest, 0, len(s.tests))
	for _, t := range s.tests {
		if status == "" || t.Status == status {
			out = append(out, t)
		}
	}
	return out
}

// DeleteTest removes a test that is not currently running.
func (s *ABTestService) DeleteTest(testID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tests[testID]
	if !ok {
		return fmt.Errorf("abtest: test %s not found", testID)
	}
	if t.Status == StatusRunning {
		return fmt.Errorf("abtest: cannot delete running test %s; stop it first", testID)
	}
	delete(s.tests, testID)
	log.Printf("[abtest] deleted test %s", testID)
	return nil
}

// ---------------------------------------------------------------------------
// Comparison report
// ---------------------------------------------------------------------------

// CompareResults produces a human-readable comparison string for a test that
// has been analyzed.
func (s *ABTestService) CompareResults(testID string) (string, error) {
	s.mu.RLock()
	test, ok := s.tests[testID]
	s.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("abtest: test %s not found", testID)
	}
	if test.Status != StatusAnalyzed || test.Results == nil {
		return "", fmt.Errorf("abtest: test %s has not been analyzed yet", testID)
	}

	r := test.Results
	report := fmt.Sprintf(
		"A/B Test Report: %s\n"+
			"============================================================\n"+
			"Variable changed : %s (%s)\n"+
			"  Target         : %s\n"+
			"  Description    : %s\n"+
			"Period           : %s → %s\n"+
			"\n"+
			"                    Control        Treatment\n"+
			"------------------------------------------------------------\n"+
			"Total Return      %+8.2f%%       %+8.2f%%\n"+
			"Annual Return     %+8.2f%%       %+8.2f%%\n"+
			"Sharpe Ratio      %8.3f        %8.3f\n"+
			"Max Drawdown      %8.2f%%       %8.2f%%\n"+
			"Volatility        %8.2f%%       %8.2f%%\n"+
			"Win Rate          %8.2f%%       %8.2f%%\n"+
			"Total Trades      %8d        %8d\n"+
			"\n"+
			"Winner            : %s (confidence %.1f%%)\n"+
			"Divergence Points : %d\n"+
			"\n"+
			"Summary:\n%s\n",
		test.Name,
		test.Variable.Type, test.Variable.NewValue,
		test.Variable.Target,
		test.Variable.Description,
		test.StartDate, test.EndDate,
		r.ControlMetrics.TotalReturn*100, r.TreatmentMetrics.TotalReturn*100,
		r.ControlMetrics.AnnualReturn*100, r.TreatmentMetrics.AnnualReturn*100,
		r.ControlMetrics.Sharpe, r.TreatmentMetrics.Sharpe,
		r.ControlMetrics.MaxDrawdown*100, r.TreatmentMetrics.MaxDrawdown*100,
		r.ControlMetrics.Volatility*100, r.TreatmentMetrics.Volatility*100,
		r.ControlMetrics.WinRate*100, r.TreatmentMetrics.WinRate*100,
		r.ControlMetrics.TotalTrades, r.TreatmentMetrics.TotalTrades,
		r.Winner, r.Confidence*100,
		len(r.Divergences),
		r.Summary,
	)
	return report, nil
}

// ---------------------------------------------------------------------------
// Statistical helpers
// ---------------------------------------------------------------------------

// computeConfidence estimates statistical significance via a simplified
// Welch's t-test on daily returns derived from the NAV histories. If there
// are too few data points we fall back to a heuristic based on return
// difference relative to combined volatility.
func computeConfidence(control, treatment FundMetrics) float64 {
	cReturns := dailyReturns(control.NAVHistory)
	tReturns := dailyReturns(treatment.NAVHistory)

	if len(cReturns) < 5 || len(tReturns) < 5 {
		return heuristicConfidence(control, treatment)
	}

	cMean, cVar := meanVariance(cReturns)
	tMean, tVar := meanVariance(tReturns)

	nC := float64(len(cReturns))
	nT := float64(len(tReturns))

	// Welch's t-statistic
	denominator := math.Sqrt(cVar/nC + tVar/nT)
	if denominator == 0 {
		return 0
	}
	tStat := math.Abs(cMean-tMean) / denominator

	// Welch-Satterthwaite degrees of freedom
	num := math.Pow(cVar/nC+tVar/nT, 2)
	denom := math.Pow(cVar/nC, 2)/(nC-1) + math.Pow(tVar/nT, 2)/(nT-1)
	if denom == 0 {
		return 0
	}
	df := num / denom

	// Approximate two-tailed p-value using the t-distribution CDF.
	p := approxTwoTailedP(tStat, df)
	confidence := 1 - p
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}
	return confidence
}

// heuristicConfidence provides a rough confidence when we lack enough data
// points for a proper t-test.
func heuristicConfidence(control, treatment FundMetrics) float64 {
	diff := math.Abs(control.TotalReturn - treatment.TotalReturn)
	combinedVol := (control.Volatility + treatment.Volatility) / 2
	if combinedVol == 0 {
		if diff > 0 {
			return 0.6 // some difference but unknown distribution
		}
		return 0
	}
	ratio := diff / combinedVol
	// Sigmoid-ish mapping: ratio of 1 ≈ 73%, 2 ≈ 88%, 3 ≈ 95%
	conf := 1 - 1/(1+ratio)
	if conf > 0.99 {
		conf = 0.99
	}
	return conf
}

// determineWinner picks the better fund or declares inconclusive if
// confidence is below a threshold.
func determineWinner(control, treatment FundMetrics, confidence float64) string {
	const minConfidence = 0.60

	if confidence < minConfidence {
		return "inconclusive"
	}

	// Composite score: heavily weight Sharpe, then total return, penalise
	// drawdown.
	cScore := compositeScore(control)
	tScore := compositeScore(treatment)

	if tScore > cScore {
		return "treatment"
	}
	if cScore > tScore {
		return "control"
	}
	return "inconclusive"
}

// compositeScore reduces a FundMetrics to a single comparable number.
func compositeScore(m FundMetrics) float64 {
	return 0.4*m.Sharpe + 0.3*m.TotalReturn + 0.2*(1+m.MaxDrawdown) + 0.1*m.WinRate
}

// ---------------------------------------------------------------------------
// Math utilities
// ---------------------------------------------------------------------------

// dailyReturns converts an NAV history into a series of daily log-returns.
func dailyReturns(nav []NAVPoint) []float64 {
	if len(nav) < 2 {
		return nil
	}
	ret := make([]float64, 0, len(nav)-1)
	for i := 1; i < len(nav); i++ {
		if nav[i-1].NAV <= 0 {
			continue
		}
		ret = append(ret, math.Log(nav[i].NAV/nav[i-1].NAV))
	}
	return ret
}

// meanVariance returns the sample mean and sample variance.
func meanVariance(xs []float64) (float64, float64) {
	n := float64(len(xs))
	if n == 0 {
		return 0, 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / n
	if n < 2 {
		return mean, 0
	}
	var ss float64
	for _, x := range xs {
		d := x - mean
		ss += d * d
	}
	return mean, ss / (n - 1)
}

// approxTwoTailedP approximates the two-tailed p-value for a t-statistic
// using the regularised incomplete beta function identity:
//   p = I_{df/(df+t^2)}(df/2, 1/2)
// We approximate the regularised incomplete beta with a continued-fraction
// expansion (Lentz's method) which is accurate to ~1e-8 for typical inputs.
func approxTwoTailedP(t float64, df float64) float64 {
	if df <= 0 {
		return 1
	}
	x := df / (df + t*t)
	a := df / 2
	b := 0.5
	p := regIncBeta(x, a, b)
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	return p
}

// regIncBeta computes the regularised incomplete beta function I_x(a,b)
// using the continued-fraction representation (Lentz's algorithm).
func regIncBeta(x, a, b float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}

	// Use the symmetry relation when x > (a+1)/(a+b+2) for better
	// convergence.
	if x > (a+1)/(a+b+2) {
		return 1 - regIncBeta(1-x, b, a)
	}

	const maxIter = 200
	const epsilon = 1e-10

	// Front factor: x^a * (1-x)^b / (a * Beta(a,b))
	lnFront := a*math.Log(x) + b*math.Log(1-x) -
		math.Log(a) - lnBeta(a, b)
	front := math.Exp(lnFront)

	// Evaluate continued fraction using modified Lentz's method.
	f := 1.0
	c := 1.0
	d := 1 - (a+b)*x/(a+1)
	if math.Abs(d) < epsilon {
		d = epsilon
	}
	d = 1 / d
	f = d

	for i := 1; i <= maxIter; i++ {
		m := float64(i)

		// Even step numerator
		num := m * (b - m) * x / ((a + 2*m - 1) * (a + 2*m))
		d = 1 + num*d
		if math.Abs(d) < epsilon {
			d = epsilon
		}
		c = 1 + num/c
		if math.Abs(c) < epsilon {
			c = epsilon
		}
		d = 1 / d
		f *= d * c

		// Odd step numerator
		num = -((a + m) * (a + b + m) * x) / ((a + 2*m) * (a + 2*m + 1))
		d = 1 + num*d
		if math.Abs(d) < epsilon {
			d = epsilon
		}
		c = 1 + num/c
		if math.Abs(c) < epsilon {
			c = epsilon
		}
		d = 1 / d
		delta := d * c
		f *= delta

		if math.Abs(delta-1) < epsilon {
			break
		}
	}

	return front * f
}

// lnBeta returns ln(Beta(a,b)) = lnGamma(a) + lnGamma(b) - lnGamma(a+b).
func lnBeta(a, b float64) float64 {
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	lab, _ := math.Lgamma(a + b)
	return la + lb - lab
}

// ---------------------------------------------------------------------------
// Date validation
// ---------------------------------------------------------------------------

const dateFmt = "2006-01-02"

func validateDateRange(start, end string) error {
	s, err := time.Parse(dateFmt, start)
	if err != nil {
		return fmt.Errorf("invalid start date %q: %w", start, err)
	}
	e, err := time.Parse(dateFmt, end)
	if err != nil {
		return fmt.Errorf("invalid end date %q: %w", end, err)
	}
	if !e.After(s) {
		return fmt.Errorf("end date %s must be after start date %s", end, start)
	}
	return nil
}
