package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubBacktestService is an in-memory BacktestService used by the
// handler integration tests. Each method records the args it was
// called with so assertions can check the handler forwarded the
// path-derived fundID + the authenticated userID correctly.
type stubBacktestService struct {
	submitFn      func(userID string, input SubmitBacktestInput) (*BacktestJob, error)
	listFn        func(userID, fundID string) ([]*BacktestJob, error)
	getFn         func(userID, fundID, jobID string) (*BacktestJob, error)
	cancelFn      func(userID, fundID, jobID string) (bool, error)
	compareFn     func(userID, fundID, jobIDA, jobIDB string) (*BacktestComparison, error)
	submitSweepFn func(userID string, input SubmitSweepInput) (*BacktestSweep, error)
	listSweepsFn  func(userID, fundID string) ([]*BacktestSweep, error)
	getSweepFn    func(userID, fundID, sweepID string) (*BacktestSweep, error)
	axisCatalogFn func() []string
	buySymbolsFn  func(userID, fundID string, limit int) ([]BacktestHistoricalBuySymbol, error)
}

func (s stubBacktestService) SubmitBacktest(userID string, input SubmitBacktestInput) (*BacktestJob, error) {
	if s.submitFn != nil {
		return s.submitFn(userID, input)
	}
	return &BacktestJob{ID: "job-1", FundID: input.FundID, Status: "queued"}, nil
}

func (s stubBacktestService) ListBacktests(userID, fundID string) ([]*BacktestJob, error) {
	if s.listFn != nil {
		return s.listFn(userID, fundID)
	}
	return nil, nil
}

func (s stubBacktestService) GetBacktest(userID, fundID, jobID string) (*BacktestJob, error) {
	if s.getFn != nil {
		return s.getFn(userID, fundID, jobID)
	}
	return nil, nil
}

func (s stubBacktestService) CancelBacktest(userID, fundID, jobID string) (bool, error) {
	if s.cancelFn != nil {
		return s.cancelFn(userID, fundID, jobID)
	}
	return true, nil
}

func (s stubBacktestService) CompareBacktests(userID, fundID, a, b string) (*BacktestComparison, error) {
	if s.compareFn != nil {
		return s.compareFn(userID, fundID, a, b)
	}
	return nil, nil
}

func (s stubBacktestService) SubmitSweep(userID string, input SubmitSweepInput) (*BacktestSweep, error) {
	if s.submitSweepFn != nil {
		return s.submitSweepFn(userID, input)
	}
	return nil, nil
}

func (s stubBacktestService) ListSweeps(userID, fundID string) ([]*BacktestSweep, error) {
	if s.listSweepsFn != nil {
		return s.listSweepsFn(userID, fundID)
	}
	return nil, nil
}

func (s stubBacktestService) GetSweep(userID, fundID, sweepID string) (*BacktestSweep, error) {
	if s.getSweepFn != nil {
		return s.getSweepFn(userID, fundID, sweepID)
	}
	return nil, nil
}

func (s stubBacktestService) SweepAxisCatalog() []string {
	if s.axisCatalogFn != nil {
		return s.axisCatalogFn()
	}
	return []string{"slippageBps", "engineKind"}
}

func (s stubBacktestService) ListHistoricalBuySymbols(userID, fundID string, limit int) ([]BacktestHistoricalBuySymbol, error) {
	if s.buySymbolsFn != nil {
		return s.buySymbolsFn(userID, fundID, limit)
	}
	return []BacktestHistoricalBuySymbol{}, nil
}

// newBacktestHandler is the minimal FundHandler wiring needed for
// the backtest tests — everything else gets the empty stub.
func newBacktestHandler(svc BacktestService) *FundHandler {
	return NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	).WithBacktestService(svc)
}

// Submit accepts a valid body, returns 201 with the new job.
func TestSubmitBacktestHappyPath(t *testing.T) {
	var captured SubmitBacktestInput
	svc := stubBacktestService{
		submitFn: func(userID string, input SubmitBacktestInput) (*BacktestJob, error) {
			captured = input
			if userID != "user-1" {
				t.Errorf("user id = %q, want user-1", userID)
			}
			return &BacktestJob{ID: "job-42", FundID: input.FundID, Status: "queued"}, nil
		},
	}
	mux := http.NewServeMux()
	newBacktestHandler(svc).RegisterRoutes(mux)

	body := SubmitBacktestInput{
		Symbols:     []string{"AAPL"},
		Start:       time.Now().AddDate(0, -6, 0),
		End:         time.Now(),
		InitialCash: 100_000,
		EngineKind:  "fallback",
	}
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-7/backtests", bytes.NewReader(buf))
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var job BacktestJob
	if err := json.Unmarshal(rr.Body.Bytes(), &job); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if job.ID != "job-42" || job.FundID != "fund-7" {
		t.Errorf("returned job mismatch: %+v", job)
	}
	if captured.FundID != "fund-7" {
		t.Errorf("handler should overwrite body fundId with URL value, got %q", captured.FundID)
	}
}

// Body fundId mismatch is silently overridden by the URL fundId so
// a client typo can't cross-submit.
func TestSubmitBacktestOverridesBodyFundID(t *testing.T) {
	captured := ""
	svc := stubBacktestService{
		submitFn: func(_ string, input SubmitBacktestInput) (*BacktestJob, error) {
			captured = input.FundID
			return &BacktestJob{ID: "x", FundID: input.FundID, Status: "queued"}, nil
		},
	}
	mux := http.NewServeMux()
	newBacktestHandler(svc).RegisterRoutes(mux)

	body := SubmitBacktestInput{
		FundID:      "tampered-id",
		Symbols:     []string{"AAPL"},
		Start:       time.Now().AddDate(0, -1, 0),
		End:         time.Now(),
		InitialCash: 100,
	}
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-real/backtests", bytes.NewReader(buf))
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if captured != "fund-real" {
		t.Errorf("expected URL fundId to win, got %q", captured)
	}
}

// Missing required fields → 400.
func TestSubmitBacktestValidatesRequiredFields(t *testing.T) {
	mux := http.NewServeMux()
	newBacktestHandler(stubBacktestService{}).RegisterRoutes(mux)
	cases := []struct {
		name string
		body SubmitBacktestInput
	}{
		{"empty symbols", SubmitBacktestInput{Start: time.Now().AddDate(0, -1, 0), End: time.Now(), InitialCash: 100}},
		{"inverted window", SubmitBacktestInput{Symbols: []string{"AAPL"}, Start: time.Now(), End: time.Now().AddDate(0, -1, 0), InitialCash: 100}},
		{"no initial cash", SubmitBacktestInput{Symbols: []string{"AAPL"}, Start: time.Now().AddDate(0, -1, 0), End: time.Now()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, _ := json.Marshal(tc.body)
			req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-1/backtests", bytes.NewReader(buf))
			req = authRequest(req, "user-1")
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

// Unwired service → 503.
func TestSubmitBacktestServiceUnavailable(t *testing.T) {
	mux := http.NewServeMux()
	newBacktestHandler(nil).RegisterRoutes(mux)

	body := SubmitBacktestInput{Symbols: []string{"AAPL"}, Start: time.Now().AddDate(0, -1, 0), End: time.Now(), InitialCash: 100}
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-1/backtests", bytes.NewReader(buf))
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

// List endpoint returns empty array on unconfigured service.
func TestListBacktestsUnconfiguredReturnsEmpty(t *testing.T) {
	mux := http.NewServeMux()
	newBacktestHandler(nil).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/backtests", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var jobs []BacktestJob
	if err := json.Unmarshal(rr.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("expected empty list, got %d", len(jobs))
	}
}

// Get unknown job → 404.
func TestGetBacktestNotFound(t *testing.T) {
	svc := stubBacktestService{
		getFn: func(_, _, _ string) (*BacktestJob, error) { return nil, nil },
	}
	mux := http.NewServeMux()
	newBacktestHandler(svc).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/backtests/job-missing", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

// Cancel terminated job → 409.
func TestCancelBacktestTerminatedReturnsConflict(t *testing.T) {
	svc := stubBacktestService{
		cancelFn: func(_, _, _ string) (bool, error) { return false, nil },
	}
	mux := http.NewServeMux()
	newBacktestHandler(svc).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-1/backtests/job-1/cancel", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rr.Code)
	}
}

// Cancel running job → 200.
func TestCancelBacktestSuccess(t *testing.T) {
	called := false
	svc := stubBacktestService{
		cancelFn: func(userID, fundID, jobID string) (bool, error) {
			called = true
			if userID != "user-9" || fundID != "fund-7" || jobID != "job-3" {
				t.Errorf("args: %s %s %s", userID, fundID, jobID)
			}
			return true, nil
		},
	}
	mux := http.NewServeMux()
	newBacktestHandler(svc).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-7/backtests/job-3/cancel", nil)
	req = authRequest(req, "user-9")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if !called {
		t.Error("cancelFn never invoked")
	}
}

// Service error → 5xx (via handleServiceError).
func TestSubmitBacktestServiceError(t *testing.T) {
	svc := stubBacktestService{
		submitFn: func(_ string, _ SubmitBacktestInput) (*BacktestJob, error) {
			return nil, errors.New("upstream down")
		},
	}
	mux := http.NewServeMux()
	newBacktestHandler(svc).RegisterRoutes(mux)
	body := SubmitBacktestInput{Symbols: []string{"AAPL"}, Start: time.Now().AddDate(0, -1, 0), End: time.Now(), InitialCash: 100}
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-1/backtests", bytes.NewReader(buf))
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code < 500 {
		t.Errorf("status = %d, want 5xx (server error from upstream)", rr.Code)
	}
}

// Walk-forward spec invalid: service returns ErrWalkForwardInvalid,
// the handler must translate it to 400 (not 500).
func TestSubmitBacktestWalkForwardInvalidReturns400(t *testing.T) {
	svc := stubBacktestService{
		submitFn: func(_ string, _ SubmitBacktestInput) (*BacktestJob, error) {
			return nil, fmt.Errorf("%w: numFolds 1 (allowed 2..12)", ErrWalkForwardInvalid)
		},
	}
	mux := http.NewServeMux()
	newBacktestHandler(svc).RegisterRoutes(mux)
	body := SubmitBacktestInput{
		Symbols: []string{"AAPL"}, Start: time.Now().AddDate(0, -1, 0), End: time.Now(), InitialCash: 100,
		WalkForward: &WalkForwardInput{NumFolds: 1, Mode: "anchored"},
	}
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-1/backtests", bytes.NewReader(buf))
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// Walk-forward spec valid: handler forwards the sub-spec on to the
// service unmodified.
func TestSubmitBacktestForwardsWalkForwardSubSpec(t *testing.T) {
	var captured SubmitBacktestInput
	svc := stubBacktestService{
		submitFn: func(_ string, input SubmitBacktestInput) (*BacktestJob, error) {
			captured = input
			return &BacktestJob{ID: "wf-1", FundID: input.FundID, Status: "queued"}, nil
		},
	}
	mux := http.NewServeMux()
	newBacktestHandler(svc).RegisterRoutes(mux)
	body := SubmitBacktestInput{
		Symbols: []string{"AAPL"}, Start: time.Now().AddDate(0, -1, 0), End: time.Now(), InitialCash: 1000,
		WalkForward: &WalkForwardInput{NumFolds: 4, TrainRatio: 0.5, Mode: "rolling"},
	}
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-1/backtests", bytes.NewReader(buf))
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if captured.WalkForward == nil {
		t.Fatal("walkForward not propagated to service")
	}
	if captured.WalkForward.NumFolds != 4 || captured.WalkForward.Mode != "rolling" {
		t.Errorf("walkForward = %+v", captured.WalkForward)
	}
}

// Compare happy path — 2 completed jobs.
func TestCompareBacktestsHappyPath(t *testing.T) {
	svc := stubBacktestService{
		compareFn: func(userID, fundID, a, b string) (*BacktestComparison, error) {
			if a != "job-a" || b != "job-b" || fundID != "fund-1" {
				t.Errorf("args: %s %s %s", fundID, a, b)
			}
			return &BacktestComparison{
				A: &BacktestJob{ID: a, Status: "completed"},
				B: &BacktestJob{ID: b, Status: "completed"},
				Diff: BacktestComparisonDiff{
					SharpeDelta: 0.4, SameWindow: true, SameUniverse: true,
				},
			}, nil
		},
	}
	mux := http.NewServeMux()
	newBacktestHandler(svc).RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/backtests/compare?a=job-a&b=job-b", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var cmp BacktestComparison
	if err := json.Unmarshal(rr.Body.Bytes(), &cmp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cmp.A == nil || cmp.B == nil || cmp.Diff.SharpeDelta != 0.4 {
		t.Errorf("unexpected payload: %+v", cmp)
	}
	if !cmp.Diff.SameWindow || !cmp.Diff.SameUniverse {
		t.Errorf("expected SameWindow + SameUniverse to be true")
	}
}

// Missing a/b → 400.
func TestCompareBacktestsRequiresBothJobIDs(t *testing.T) {
	mux := http.NewServeMux()
	newBacktestHandler(stubBacktestService{}).RegisterRoutes(mux)

	cases := []string{
		"/api/funds/fund-1/backtests/compare",
		"/api/funds/fund-1/backtests/compare?a=x",
		"/api/funds/fund-1/backtests/compare?b=y",
		"/api/funds/fund-1/backtests/compare?a=&b=",
	}
	for _, path := range cases {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = authRequest(req, "user-1")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("path=%s status=%d, want 400", path, rr.Code)
		}
	}
}

// Same a == b → 400 (self-compare is meaningless).
func TestCompareBacktestsRejectsSelfCompare(t *testing.T) {
	mux := http.NewServeMux()
	newBacktestHandler(stubBacktestService{}).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/backtests/compare?a=same&b=same", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

// Not-comparable (one of the jobs not finished) → 409.
func TestCompareBacktestsNotComparable409(t *testing.T) {
	svc := stubBacktestService{
		compareFn: func(_, _, _, _ string) (*BacktestComparison, error) {
			return nil, ErrBacktestNotComparable
		},
	}
	mux := http.NewServeMux()
	newBacktestHandler(svc).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/backtests/compare?a=x&b=y", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("status=%d, want 409", rr.Code)
	}
}

// Missing one of the jobs (compareFn returns nil) → 404.
func TestCompareBacktestsMissingJob404(t *testing.T) {
	svc := stubBacktestService{
		compareFn: func(_, _, _, _ string) (*BacktestComparison, error) { return nil, nil },
	}
	mux := http.NewServeMux()
	newBacktestHandler(svc).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/backtests/compare?a=x&b=y", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", rr.Code)
	}
}

// Service unconfigured → 503.
func TestCompareBacktestsServiceUnavailable(t *testing.T) {
	mux := http.NewServeMux()
	newBacktestHandler(nil).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/backtests/compare?a=x&b=y", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", rr.Code)
	}
}

// Submit sweep happy path → 202.
func TestSubmitSweepHappy202(t *testing.T) {
	svc := stubBacktestService{
		submitSweepFn: func(_ string, in SubmitSweepInput) (*BacktestSweep, error) {
			if in.FundID != "fund-1" || len(in.Axes) != 1 {
				t.Errorf("input not propagated: %+v", in)
			}
			return &BacktestSweep{
				ID:         "sweep-x",
				FundID:     in.FundID,
				TotalCells: 2,
				Status:     "queued",
				Children: []*BacktestSweepChild{
					{Job: &BacktestJob{ID: "c1", Status: "queued"}, AxisValues: map[string]string{"slippageBps": "3"}},
					{Job: &BacktestJob{ID: "c2", Status: "queued"}, AxisValues: map[string]string{"slippageBps": "5"}},
				},
			}, nil
		},
	}
	mux := http.NewServeMux()
	newBacktestHandler(svc).RegisterRoutes(mux)
	body := `{"base":{"symbols":["AAPL"],"initialCash":1000},"axes":[{"name":"slippageBps","values":["3","5"]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-1/backtests/sweeps", strings.NewReader(body))
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var resp BacktestSweep
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ID != "sweep-x" || len(resp.Children) != 2 {
		t.Errorf("response wrong: %+v", resp)
	}
}

// Submit sweep with missing axes → 400.
func TestSubmitSweepMissingAxes400(t *testing.T) {
	mux := http.NewServeMux()
	newBacktestHandler(stubBacktestService{}).RegisterRoutes(mux)
	body := `{"base":{"symbols":["AAPL"],"initialCash":1000}}`
	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-1/backtests/sweeps", strings.NewReader(body))
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// Submit sweep with empty base.symbols → 400.
func TestSubmitSweepMissingSymbols400(t *testing.T) {
	mux := http.NewServeMux()
	newBacktestHandler(stubBacktestService{}).RegisterRoutes(mux)
	body := `{"base":{"initialCash":1000},"axes":[{"name":"slippageBps","values":["3"]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-1/backtests/sweeps", strings.NewReader(body))
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// Submit sweep with invalid axis name → 400 (service returns ErrSweepInvalid).
func TestSubmitSweepInvalidAxis400(t *testing.T) {
	svc := stubBacktestService{
		submitSweepFn: func(_ string, _ SubmitSweepInput) (*BacktestSweep, error) {
			return nil, ErrSweepInvalid
		},
	}
	mux := http.NewServeMux()
	newBacktestHandler(svc).RegisterRoutes(mux)
	body := `{"base":{"symbols":["AAPL"],"initialCash":1000},"axes":[{"name":"fundId","values":["x"]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-1/backtests/sweeps", strings.NewReader(body))
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// Submit sweep when service is nil → 503.
func TestSubmitSweep503(t *testing.T) {
	mux := http.NewServeMux()
	newBacktestHandler(nil).RegisterRoutes(mux)
	body := `{"base":{"symbols":["AAPL"]},"axes":[{"name":"slippageBps","values":["3"]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-1/backtests/sweeps", strings.NewReader(body))
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

// List sweeps returns the service's slice, empty when nil.
func TestListSweepsReturnsEmptyArray(t *testing.T) {
	mux := http.NewServeMux()
	newBacktestHandler(stubBacktestService{}).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/backtests/sweeps", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Errorf("body = %q, want []", rr.Body.String())
	}
}

// Get sweep returns the service's payload.
func TestGetSweepHappy(t *testing.T) {
	svc := stubBacktestService{
		getSweepFn: func(_ string, fundID, sweepID string) (*BacktestSweep, error) {
			if fundID != "fund-1" || sweepID != "sw-1" {
				t.Errorf("args: %s %s", fundID, sweepID)
			}
			return &BacktestSweep{ID: "sw-1", Status: "completed", TotalCells: 4, DoneCells: 4}, nil
		},
	}
	mux := http.NewServeMux()
	newBacktestHandler(svc).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/backtests/sweeps/sw-1", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
}

// Get sweep when not found → 404.
func TestGetSweepNotFound(t *testing.T) {
	svc := stubBacktestService{
		getSweepFn: func(_, _, _ string) (*BacktestSweep, error) { return nil, nil },
	}
	mux := http.NewServeMux()
	newBacktestHandler(svc).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/backtests/sweeps/missing", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

// Sweep axis catalog returns the allow-list.
func TestSweepAxisCatalog(t *testing.T) {
	svc := stubBacktestService{
		axisCatalogFn: func() []string {
			return []string{"slippageBps", "engineKind"}
		},
	}
	mux := http.NewServeMux()
	newBacktestHandler(svc).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/backtests/sweeps/axes", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "slippageBps") {
		t.Errorf("body missing axes: %s", rr.Body.String())
	}
}

func TestListHistoricalBuySymbols(t *testing.T) {
	svc := stubBacktestService{
		buySymbolsFn: func(userID, fundID string, limit int) ([]BacktestHistoricalBuySymbol, error) {
			if userID != "user-1" || fundID != "fund-1" {
				t.Fatalf("unexpected args userID=%q fundID=%q", userID, fundID)
			}
			if limit != 25 {
				t.Fatalf("limit = %d want 25", limit)
			}
			return []BacktestHistoricalBuySymbol{{Symbol: "AAPL", Market: "us_equity", BuyCount: 2}}, nil
		},
	}
	mux := http.NewServeMux()
	newBacktestHandler(svc).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/backtests/historical-buy-symbols?limit=25", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "AAPL") {
		t.Errorf("body missing symbol: %s", rr.Body.String())
	}
}

// normaliseEngineKind tests
func TestNormaliseEngineKind(t *testing.T) {
	cases := map[string]string{
		"":           "fallback",
		"fallback":   "fallback",
		"LLM":        "llm",
		" Debate ":   "llm-debate",
		"llm-debate": "llm-debate",
		"llm_debate": "llm-debate",
		"bogus":      "fallback",
	}
	for in, want := range cases {
		if got := normaliseEngineKind(in); got != want {
			t.Errorf("normaliseEngineKind(%q) = %q, want %q", in, got, want)
		}
	}
}
