package main

import (
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/backtest"
	"github.com/fundai/server/internal/repository"
)

// SubmitSweep with valid 2-axis spec fans out the Cartesian
// product into N in-memory child jobs.
func TestAdapterSubmitSweepFansOutCartesian(t *testing.T) {
	adapter := &backtestServiceAdapter{
		ohlcFetcher: &stubFetcher{},
		userIDByJob: make(map[string]string),
		sweepByJob:  make(map[string]sweepCellRef),
	}
	adapter.store = backtest.NewJobStore(neverRunsEngine{})

	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	in := api.SubmitSweepInput{
		FundID: "fund-1",
		Name:   "trial",
		Base: api.SubmitBacktestInput{
			Symbols: []string{"AAPL"}, Start: start, End: start.AddDate(0, 0, 5),
			InitialCash: 1000, EngineKind: "fallback",
		},
		Axes: []api.SubmitSweepAxisInput{
			{Name: "slippageBps", Values: []string{"3", "5"}},
			{Name: "maxOrdersPerDay", Values: []string{"1", "3"}},
		},
	}
	sweep, err := adapter.SubmitSweep("user-1", in)
	if err != nil {
		t.Fatalf("SubmitSweep: %v", err)
	}
	if sweep == nil || sweep.TotalCells != 4 || len(sweep.Children) != 4 {
		t.Fatalf("expected 4 children, got %+v", sweep)
	}
	// Each cell's request should reflect the axis overrides.
	gotPairs := make(map[string]bool, 4)
	for _, c := range sweep.Children {
		key := c.AxisValues["slippageBps"] + "|" + c.AxisValues["maxOrdersPerDay"]
		gotPairs[key] = true
	}
	for _, want := range []string{"3|1", "3|3", "5|1", "5|3"} {
		if !gotPairs[want] {
			t.Errorf("missing cell %s in %v", want, gotPairs)
		}
	}
	// Each child must be queryable from the in-memory store
	// (no goroutine ran because neverRunsEngine refuses).
	listed := adapter.store.List("fund-1")
	if len(listed) != 4 {
		t.Errorf("expected 4 in-memory jobs, got %d", len(listed))
	}
}

// Submitting a sweep with no axes returns ErrSweepInvalid.
func TestAdapterSubmitSweepEmptyAxesRejected(t *testing.T) {
	adapter := &backtestServiceAdapter{
		ohlcFetcher: &stubFetcher{},
		userIDByJob: make(map[string]string),
		sweepByJob:  make(map[string]sweepCellRef),
	}
	adapter.store = backtest.NewJobStore(neverRunsEngine{})

	_, err := adapter.SubmitSweep("u", api.SubmitSweepInput{
		FundID: "fund-1",
		Base:   api.SubmitBacktestInput{Symbols: []string{"AAPL"}, InitialCash: 1000},
		Axes:   nil,
	})
	if !errors.Is(err, api.ErrSweepInvalid) {
		t.Errorf("expected ErrSweepInvalid, got %v", err)
	}
}

// Submitting a sweep with an unknown axis returns ErrSweepInvalid.
func TestAdapterSubmitSweepUnknownAxisRejected(t *testing.T) {
	adapter := &backtestServiceAdapter{
		ohlcFetcher: &stubFetcher{},
		userIDByJob: make(map[string]string),
		sweepByJob:  make(map[string]sweepCellRef),
	}
	adapter.store = backtest.NewJobStore(neverRunsEngine{})

	_, err := adapter.SubmitSweep("u", api.SubmitSweepInput{
		FundID: "fund-1",
		Base:   api.SubmitBacktestInput{Symbols: []string{"AAPL"}, InitialCash: 1000},
		Axes:   []api.SubmitSweepAxisInput{{Name: "fundId", Values: []string{"a"}}},
	})
	if !errors.Is(err, api.ErrSweepInvalid) {
		t.Errorf("expected ErrSweepInvalid, got %v", err)
	}
}

// Submitting a sweep with too many cells returns ErrSweepInvalid.
func TestAdapterSubmitSweepOversizedRejected(t *testing.T) {
	adapter := &backtestServiceAdapter{
		ohlcFetcher: &stubFetcher{},
		userIDByJob: make(map[string]string),
		sweepByJob:  make(map[string]sweepCellRef),
	}
	adapter.store = backtest.NewJobStore(neverRunsEngine{})

	_, err := adapter.SubmitSweep("u", api.SubmitSweepInput{
		FundID: "fund-1",
		Base:   api.SubmitBacktestInput{Symbols: []string{"AAPL"}, InitialCash: 1000},
		Axes: []api.SubmitSweepAxisInput{
			{Name: "slippageBps", Values: []string{"1", "2", "3", "4", "5", "6"}},
			{Name: "maxOrdersPerDay", Values: []string{"1", "2", "3", "4", "5"}},
		},
	})
	if !errors.Is(err, api.ErrSweepInvalid) {
		t.Errorf("expected ErrSweepInvalid, got %v", err)
	}
}

// OHLC unconfigured → ErrBacktestUnconfigured.
func TestAdapterSubmitSweepRequiresOHLC(t *testing.T) {
	adapter := &backtestServiceAdapter{
		userIDByJob: make(map[string]string),
		sweepByJob:  make(map[string]sweepCellRef),
	}
	adapter.store = backtest.NewJobStore(neverRunsEngine{})

	_, err := adapter.SubmitSweep("u", api.SubmitSweepInput{
		FundID: "f-1",
		Base:   api.SubmitBacktestInput{Symbols: []string{"AAPL"}, InitialCash: 1000},
		Axes:   []api.SubmitSweepAxisInput{{Name: "slippageBps", Values: []string{"3"}}},
	})
	if !errors.Is(err, api.ErrBacktestUnconfigured) {
		t.Errorf("expected ErrBacktestUnconfigured, got %v", err)
	}
}

// SweepAxisCatalog returns the allow-list.
func TestAdapterSweepAxisCatalog(t *testing.T) {
	adapter := &backtestServiceAdapter{}
	got := adapter.SweepAxisCatalog()
	want := []string{"commissionBps", "engineKind", "initialCash", "maxOrdersPerDay", "slippageBps"}
	if len(got) != len(want) {
		t.Fatalf("got %d axes, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("[%d]: got %s, want %s", i, got[i], w)
		}
	}
}

// deriveSweepStatus unit table.
func TestDeriveSweepStatus(t *testing.T) {
	cases := []struct {
		name     string
		statuses []string
		want     string
	}{
		{"all queued", []string{"queued", "queued"}, "queued"},
		{"any running", []string{"queued", "running", "completed"}, "running"},
		{"all completed", []string{"completed", "completed"}, "completed"},
		{"mix completed + failed", []string{"completed", "failed"}, "completed"},
		{"all failed", []string{"failed", "failed"}, "failed"},
		{"all cancelled", []string{"cancelled", "cancelled"}, "failed"},
		{"empty", nil, "queued"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			children := make([]*api.BacktestSweepChild, 0, len(tc.statuses))
			for _, st := range tc.statuses {
				children = append(children, &api.BacktestSweepChild{
					Job: &api.BacktestJob{Status: st},
				})
			}
			if got := deriveSweepStatus(children); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// countSweepDone counts terminal children only.
func TestCountSweepDone(t *testing.T) {
	children := []*api.BacktestSweepChild{
		{Job: &api.BacktestJob{Status: "queued"}},
		{Job: &api.BacktestJob{Status: "running"}},
		{Job: &api.BacktestJob{Status: "completed"}},
		{Job: &api.BacktestJob{Status: "failed"}},
		{Job: &api.BacktestJob{Status: "cancelled"}},
	}
	if got := countSweepDone(children); got != 3 {
		t.Errorf("got %d, want 3", got)
	}
}

// SubmitSweep persists the header and tags each child with
// sweep_id + sweep_cell. We piggyback on the persistent
// adapter helper from backtest_adapter_test.go.
func TestAdapterSubmitSweepPersistsHeaderAndChildren(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	adapter, mock, cleanup := newPersistentAdapter(t, &stubFetcher{})
	defer cleanup()
	// Swap in a blocking engine so children stay "running"
	// and never trigger persistFinal during the test window.
	// We intentionally do NOT close the channel — the goroutines
	// stay parked until process exit, but Go's test runner reaps
	// them. Closing would unblock + race with the DB cleanup.
	hold := make(chan struct{})
	adapter.store = backtest.NewJobStore(holdingEngine{block: hold})
	adapter.store.OnSubmit = adapter.persistSubmitted
	adapter.store.OnFinal = adapter.persistFinal

	// Expectations: 1 sweep header insert, then 2 child job
	// inserts (one per cell of a 1-axis × 2-values spec).
	mock.ExpectExec(`INSERT INTO backtest_sweeps`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO backtest_jobs`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO backtest_jobs`).WillReturnResult(sqlmock.NewResult(0, 1))

	sweep, err := adapter.SubmitSweep("user-1", api.SubmitSweepInput{
		FundID: "fund-1",
		Base: api.SubmitBacktestInput{
			Symbols: []string{"AAPL"}, Start: start, End: start.AddDate(0, 0, 2),
			InitialCash: 1000,
		},
		Axes: []api.SubmitSweepAxisInput{
			{Name: "slippageBps", Values: []string{"3", "5"}},
		},
	})
	if err != nil {
		t.Fatalf("SubmitSweep: %v", err)
	}
	if sweep == nil || sweep.TotalCells != 2 || len(sweep.Children) != 2 {
		t.Fatalf("unexpected sweep: %+v", sweep)
	}
	// Confirm the adapter recorded the sweep tag for both child IDs.
	adapter.jobsMu.Lock()
	defer adapter.jobsMu.Unlock()
	for _, child := range sweep.Children {
		ref, ok := adapter.sweepByJob[child.Job.ID]
		if !ok {
			t.Errorf("sweepByJob missing entry for child %s", child.Job.ID)
			continue
		}
		if ref.sweepID != sweep.ID {
			t.Errorf("child %s tagged with sweep %s, want %s", child.Job.ID, ref.sweepID, sweep.ID)
		}
		if ref.cell["slippageBps"] == "" {
			t.Errorf("child %s missing slippageBps cell value", child.Job.ID)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// echoFromSubmitInput round-trip — Symbols + InitialPositions
// must be defensively copied (no shared backing).
func TestEchoFromSubmitInputCopiesSlices(t *testing.T) {
	in := api.SubmitBacktestInput{
		Symbols: []string{"AAPL", "MSFT"},
		InitialPositions: []api.BacktestInitialPosition{
			{Symbol: "GOOG", Quantity: 10, CostPrice: 100},
		},
	}
	echo := echoFromSubmitInput(in)
	echo.Symbols[0] = "MUTATED"
	if in.Symbols[0] == "MUTATED" {
		t.Error("Symbols slice is shared")
	}
	echo.InitialPositions[0].Symbol = "MUTATED"
	if in.InitialPositions[0].Symbol == "MUTATED" {
		t.Error("InitialPositions slice is shared")
	}
}

// liveOrRowStatus returns DB status when no live job exists.
func TestLiveOrRowStatusFallsBackToRow(t *testing.T) {
	store := backtest.NewJobStore(neverRunsEngine{})
	row := repository.BacktestJobRow{ID: "job-x", Status: "completed"}
	if got := liveOrRowStatus(row, store); got != "completed" {
		t.Errorf("got %s, want completed", got)
	}
}

// liveOrRowStatus prefers the in-memory job status when one exists.
func TestLiveOrRowStatusPrefersLive(t *testing.T) {
	store := backtest.NewJobStore(neverRunsEngine{})
	live := &backtest.Job{
		ID:       "job-x",
		Request:  backtest.Request{FundID: "fund-1"},
		Progress: &backtest.Progress{Status: "running"},
	}
	store.AddJobForTest(live)
	row := repository.BacktestJobRow{ID: "job-x", Status: "queued"}
	if got := liveOrRowStatus(row, store); got != "running" {
		t.Errorf("got %s, want running", got)
	}
}
