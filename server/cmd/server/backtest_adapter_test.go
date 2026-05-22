package main

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/backtest"
	"github.com/fundai/server/internal/decision"
	"github.com/fundai/server/internal/ohlc"
	"github.com/fundai/server/internal/repository"
)

// stubFetcher implements ohlc.Fetcher with a single synthetic bar
// series. Used to exercise the wiring layer without a real
// upstream.
type stubFetcher struct {
	bars map[string][]ohlc.Bar
}

func (s *stubFetcher) Fetch(_ context.Context, req ohlc.FetchRequest) ([]ohlc.Bar, error) {
	if bars, ok := s.bars[req.Symbol]; ok {
		out := make([]ohlc.Bar, len(bars))
		copy(out, bars)
		return out, nil
	}
	return nil, ohlc.ErrNoData
}

// Verify the adapter implements backtest.Engine (so the JobStore
// it embeds can call adapter.Run directly).
func TestBacktestAdapterImplementsEngine(t *testing.T) {
	var _ backtest.Engine = (*backtestServiceAdapter)(nil)
}

// pickDecisionEngine returns FallbackEngine when no LLM client is
// configured, even if "llm" is requested. The trade-off (silently
// degrade vs. fail loudly) is documented; this test pins it down
// so future refactors don't break the contract.
func TestPickDecisionEngineFallsBackWithoutLLM(t *testing.T) {
	adapter := &backtestServiceAdapter{}
	if _, ok := adapter.pickDecisionEngine("llm").(decision.FallbackEngine); !ok {
		t.Errorf("expected FallbackEngine when llmClient nil and kind=llm")
	}
	if _, ok := adapter.pickDecisionEngine("").(decision.FallbackEngine); !ok {
		t.Errorf("expected FallbackEngine when kind empty")
	}
	if _, ok := adapter.pickDecisionEngine("unknown").(decision.FallbackEngine); !ok {
		t.Errorf("expected FallbackEngine for unknown kind")
	}
}

// translateSubmitInput honours field-by-field projection (including
// the UTC normalisation of Start/End and InitialPositions
// pass-through).
func TestTranslateSubmitInputProjectsAllFields(t *testing.T) {
	start := time.Date(2026, 1, 2, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))
	end := start.Add(72 * time.Hour)
	in := api.SubmitBacktestInput{
		FundID:           "fund-1",
		Name:             "trial-run",
		Market:           "us_equity",
		Symbols:          []string{"AAPL", "MSFT"},
		InitialPositions: []api.BacktestInitialPosition{{Symbol: "AAPL", Quantity: 5, CostPrice: 100}},
		Start:            start,
		End:              end,
		InitialCash:      50_000,
		BaseCurrency:     "USD",
		SlippageBps:      10,
		CommissionBps:    8,
		MaxOrdersPerDay:  3,
		EngineKind:       "fallback",
	}
	out := translateSubmitInput(in)
	if out.FundID != "fund-1" || out.Name != "trial-run" {
		t.Errorf("identity fields missing: %+v", out)
	}
	if !out.Start.Equal(start.UTC()) || !out.End.Equal(end.UTC()) {
		t.Errorf("times not normalised to UTC: start=%v end=%v", out.Start, out.End)
	}
	if len(out.Symbols) != 2 || len(out.InitialPositions) != 1 {
		t.Errorf("symbols/positions lost: %+v", out)
	}
	if out.SlippageBps != 10 || out.CommissionBps != 8 || out.MaxOrdersPerDay != 3 {
		t.Errorf("numeric guardrails lost: %+v", out)
	}
}

// End-to-end: adapter runs a small fallback-engine backtest, the
// JobStore reports completion + a non-empty NAV curve. This walks
// the same path the HTTP layer exercises (sans DB).
func TestBacktestAdapterRunCompletesSyntheticJob(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	bars := make([]ohlc.Bar, 10)
	for i := range bars {
		bars[i] = ohlc.Bar{Time: start.AddDate(0, 0, i), Open: 100, High: 100, Low: 100, Close: 100, Volume: 1000}
	}
	adapter := &backtestServiceAdapter{
		ohlcFetcher: &stubFetcher{bars: map[string][]ohlc.Bar{"AAPL": bars}},
	}
	prog := &backtest.Progress{}
	req := backtest.Request{
		FundID:      "fund-1",
		Symbols:     []string{"AAPL"},
		Start:       start,
		End:         start.AddDate(0, 0, 9),
		InitialCash: 1000,
		EngineKind:  "fallback",
	}
	result, err := adapter.Run(context.Background(), req, prog)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.NavCurve) != 10 {
		t.Errorf("expected 10 NAV points, got %d", len(result.NavCurve))
	}
	if snap := prog.Snapshot(); snap.Status != "completed" {
		t.Errorf("progress status = %q, want completed", snap.Status)
	}
}

// Adapter without ohlcFetcher → Run returns an error.
func TestBacktestAdapterRunRequiresOHLC(t *testing.T) {
	adapter := &backtestServiceAdapter{}
	_, err := adapter.Run(context.Background(), backtest.Request{}, nil)
	if err == nil {
		t.Errorf("expected error when ohlcFetcher nil")
	}
}

// -------------------- Phase 2F persistence integration --------------------

// newPersistentAdapter wires a real backtestServiceAdapter on top
// of a sqlmock-backed DB. Returns the adapter, the mock, and a
// teardown helper. authorisation is shorted out via a nil
// fundRepo (the adapter's authorize short-circuits when nil).
func newPersistentAdapter(t *testing.T, fetcher ohlc.Fetcher) (*backtestServiceAdapter, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	// Startup sweep runs in newBacktestServiceAdapter — pre-arm it.
	mock.ExpectExec(`UPDATE backtest_jobs\s+SET status = 'failed'`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	adapter := &backtestServiceAdapter{
		db:           db,
		backtestRepo: repository.NewBacktestRepo(db),
		sweepRepo:    repository.NewSweepRepo(db),
		ohlcFetcher:  fetcher,
		userIDByJob:  make(map[string]string, 4),
		sweepByJob:   make(map[string]sweepCellRef, 4),
	}
	adapter.store = backtest.NewJobStore(adapter)
	adapter.store.OnSubmit = adapter.persistSubmitted
	adapter.store.OnFinal = adapter.persistFinal
	adapter.sweepInterruptedJobs(context.Background())
	cleanup := func() { db.Close() }
	return adapter, mock, cleanup
}

// PersistSubmitted writes the queued row.
func TestAdapterPersistSubmittedInsertsQueuedRow(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	bars := buildSyntheticBars(start, []float64{100, 101, 102})
	fetcher := &stubFetcher{bars: map[string][]ohlc.Bar{"AAPL": bars}}
	adapter, mock, cleanup := newPersistentAdapter(t, fetcher)
	defer cleanup()

	// Expect the INSERT from persistSubmitted + UPDATE/DELETE/INSERT
	// trio from persistFinal at completion.
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO backtest_jobs`)).
		WithArgs(sqlmock.AnyArg(), "fund-1", "user-1", "", "fallback", "queued", sqlmock.AnyArg(),
			start, start.AddDate(0, 0, 2), sqlmock.AnyArg(), 0, 0,
			sql.NullString{}, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE backtest_jobs SET`)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM backtest_nav_points`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM backtest_trade_events`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO backtest_nav_points`)).WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO backtest_trade_events`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	resp, err := adapter.SubmitBacktest("user-1", api.SubmitBacktestInput{
		FundID: "fund-1", Symbols: []string{"AAPL"},
		Start: start, End: start.AddDate(0, 0, 2),
		InitialCash: 1000, EngineKind: "fallback",
	})
	if err != nil {
		t.Fatalf("SubmitBacktest: %v", err)
	}
	if resp == nil || resp.ID == "" {
		t.Fatalf("no job returned: %+v", resp)
	}
	// Wait for the goroutine to complete and persistFinal to fire.
	if status := waitJobStatus(adapter.store, resp.ID, "completed", time.Second); status != "completed" {
		t.Fatalf("job did not complete: status=%s", status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Walk-forward end-to-end: Submit with a WalkForward sub-spec
// dispatches to the WalkForwardRunner, completes all folds, and
// the persisted row carries the per-fold blob.
func TestAdapterSubmitWalkForwardEndToEnd(t *testing.T) {
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC) // Monday
	// 60 calendar days → ~42 trading days, plenty for 3 folds.
	end := start.Add(60 * 24 * time.Hour)
	bars := buildSyntheticBars(start, repeatFloat(100, 60))
	fetcher := &stubFetcher{bars: map[string][]ohlc.Bar{"AAPL": bars}}
	adapter, mock, cleanup := newPersistentAdapter(t, fetcher)
	defer cleanup()

	// Submit insert + final update/delete/insert trio. We don't
	// assert the exact argument list because walk-forward jobs
	// drag a longer set through (including the WalkForward
	// blob), but we DO require that the final update succeeds
	// so the OnFinal hook completes cleanly.
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO backtest_jobs`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE backtest_jobs SET`)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM backtest_nav_points`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM backtest_trade_events`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO backtest_nav_points`)).WillReturnResult(sqlmock.NewResult(0, 1))
	// Walk-forward over flat synthetic prices emits a stream of
	// "buy qty rounds to zero" skipped trades — the runner still
	// records them so the trade INSERT fires. Use AnyArg matchers
	// so we don't have to enumerate every fold's worth of trades.
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO backtest_trade_events`)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	resp, err := adapter.SubmitBacktest("user-1", api.SubmitBacktestInput{
		FundID: "fund-1", Symbols: []string{"AAPL"},
		Start: start, End: end,
		InitialCash: 1000, EngineKind: "fallback",
		WalkForward: &api.WalkForwardInput{NumFolds: 3, TrainRatio: 0, Mode: "anchored"},
	})
	if err != nil {
		t.Fatalf("SubmitBacktest: %v", err)
	}
	if status := waitJobStatus(adapter.store, resp.ID, "completed", 3*time.Second); status != "completed" {
		t.Fatalf("job did not complete: status=%s", status)
	}
	// Pull the in-memory job and inspect the per-fold breakdown.
	job := adapter.store.Get(resp.ID)
	if job == nil || job.Result == nil {
		t.Fatal("no result")
	}
	if job.Result.WalkForward == nil {
		t.Fatal("expected WalkForward result")
	}
	if got := len(job.Result.WalkForward.Folds); got != 3 {
		t.Errorf("got %d folds, want 3", got)
	}
}

// Walk-forward spec invalid → SubmitBacktest returns
// api.ErrWalkForwardInvalid synchronously, before any DB write.
func TestAdapterSubmitWalkForwardInvalidRejects(t *testing.T) {
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * 24 * time.Hour)
	bars := buildSyntheticBars(start, repeatFloat(100, 5))
	fetcher := &stubFetcher{bars: map[string][]ohlc.Bar{"AAPL": bars}}
	adapter, _, cleanup := newPersistentAdapter(t, fetcher)
	defer cleanup()
	// No mock.ExpectExec — we expect the call to fail before
	// touching the DB.

	_, err := adapter.SubmitBacktest("user-1", api.SubmitBacktestInput{
		FundID: "fund-1", Symbols: []string{"AAPL"},
		Start: start, End: end,
		InitialCash: 1000, EngineKind: "fallback",
		// fold=1 violates the 2..12 range.
		WalkForward: &api.WalkForwardInput{NumFolds: 1, Mode: "anchored"},
	})
	if !errors.Is(err, api.ErrWalkForwardInvalid) {
		t.Errorf("want api.ErrWalkForwardInvalid, got %v", err)
	}
}

// repeatFloat is a tiny helper for synthetic bar fixtures.
func repeatFloat(v float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// If the OnSubmit hook's INSERT fails, the whole Submit call
// returns an error and no goroutine starts (so OnFinal never
// fires either).
func TestAdapterPersistSubmittedFailureAbortsSubmit(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	bars := buildSyntheticBars(start, []float64{100, 101, 102})
	fetcher := &stubFetcher{bars: map[string][]ohlc.Bar{"AAPL": bars}}
	adapter, mock, cleanup := newPersistentAdapter(t, fetcher)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO backtest_jobs`)).
		WillReturnError(sql.ErrConnDone)

	_, err := adapter.SubmitBacktest("user-1", api.SubmitBacktestInput{
		FundID: "fund-1", Symbols: []string{"AAPL"},
		Start: start, End: start.AddDate(0, 0, 2),
		InitialCash: 1000,
	})
	if err == nil {
		t.Fatalf("expected error when persistSubmitted fails")
	}
	if got := len(adapter.store.List("fund-1")); got != 0 {
		t.Errorf("expected store empty after failed submit, got %d", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ListBacktests unions in-memory jobs with DB rows and dedupes by
// ID. Active job (in-memory) wins over its DB shadow.
func TestAdapterListBacktestsUnionsInMemoryAndDB(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	bars := buildSyntheticBars(start, []float64{100, 101, 102})
	fetcher := &stubFetcher{bars: map[string][]ohlc.Bar{"AAPL": bars}}
	adapter, mock, cleanup := newPersistentAdapter(t, fetcher)
	defer cleanup()

	// Force the engine to block so the submitted job stays
	// "running" while we exercise List. We swap in a blocking
	// engine + rebuild the JobStore.
	hold := make(chan struct{})
	adapter.store = backtest.NewJobStore(holdingEngine{block: hold})
	adapter.store.OnSubmit = adapter.persistSubmitted
	adapter.store.OnFinal = adapter.persistFinal

	// Submit one in-flight job (INSERT only, no UPDATE because
	// the engine is blocked).
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO backtest_jobs`)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	resp, err := adapter.SubmitBacktest("user-1", api.SubmitBacktestInput{
		FundID: "fund-1", Symbols: []string{"AAPL"},
		Start: start, End: start.AddDate(0, 0, 2), InitialCash: 1000,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	cols := []string{
		"id", "fund_id", "user_id", "name", "engine_kind", "status", "request", "error",
		"window_start", "window_end",
		"initial_cash", "final_nav", "cumulative_return", "annualized_return",
		"volatility", "sharpe_ratio", "max_drawdown", "win_rate",
		"trade_count", "winning_trade_count", "losing_trade_count",
		"total_days", "done_days",
		"submitted_at", "started_at", "completed_at",
		"sweep_id", "sweep_cell", "walk_forward",
	}
	now := time.Now().Add(-time.Hour)
	// DB returns BOTH the in-flight job (so we can verify dedup
	// works) and an unrelated completed historical one.
	rows := sqlmock.NewRows(cols).
		AddRow(resp.ID, "fund-1", "user-1", "", "fallback", "queued", []byte(`{}`), nil,
			start, start, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{},
			sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{},
			0, 0, 0, 0, 0, now, sql.NullTime{}, sql.NullTime{},
			sql.NullString{}, []byte(`null`), []byte(`null`)).
		AddRow("db-only-1", "fund-1", "user-1", "old run", "fallback", "completed", []byte(`{}`), nil,
			start, start, sql.NullFloat64{Float64: 1000, Valid: true}, sql.NullFloat64{Float64: 1100, Valid: true},
			sql.NullFloat64{Float64: 0.1, Valid: true}, sql.NullFloat64{},
			sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{},
			0, 0, 0, 0, 0, now.Add(-time.Hour), sql.NullTime{}, sql.NullTime{Time: now, Valid: true},
			sql.NullString{}, []byte(`null`), []byte(`null`))
	mock.ExpectQuery(`SELECT id, fund_id`).
		WithArgs("fund-1", 100).
		WillReturnRows(rows)

	out, err := adapter.ListBacktests("user-x", "fund-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("expected 2 dedup'd jobs, got %d", len(out))
	}
	// In-memory active job wins: status remains "running" / "queued"
	// (not the DB's stale "queued" — but in this stub they coincide
	// because the engine just received the ctx).
	var inMem *api.BacktestJob
	var dbOnly *api.BacktestJob
	for _, j := range out {
		if j.ID == resp.ID {
			inMem = j
		}
		if j.ID == "db-only-1" {
			dbOnly = j
		}
	}
	if inMem == nil {
		t.Errorf("in-mem job dropped from list")
	}
	if dbOnly == nil || dbOnly.Status != "completed" {
		t.Errorf("db-only historical job missing or wrong status: %+v", dbOnly)
	}

	// Arm the OnFinal write chain triggered when we unblock the
	// engine, then close the channel and wait for the runner to
	// settle so cleanup doesn't race against the persistFinal
	// goroutine.
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE backtest_jobs SET`)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM backtest_nav_points`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM backtest_trade_events`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	close(hold)
	if status := waitJobStatus(adapter.store, resp.ID, "failed", 2*time.Second); status != "failed" {
		t.Errorf("expected job to settle to failed after release, got %q", status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// holdingEngine blocks Run() until its channel is closed. Used to
// keep a job in the in-memory store while List/Get are exercised.
type holdingEngine struct {
	block chan struct{}
}

func (h holdingEngine) Run(ctx context.Context, _ backtest.Request, _ *backtest.Progress) (*backtest.Result, error) {
	select {
	case <-h.block:
		return nil, errors.New("engine blocked then released")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Get falls through to the DB when the in-memory store doesn't
// have the job.
func TestAdapterGetBacktestFallsBackToDB(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	adapter, mock, cleanup := newPersistentAdapter(t, nil)
	defer cleanup()

	cols := []string{
		"id", "fund_id", "user_id", "name", "engine_kind", "status", "request", "error",
		"window_start", "window_end",
		"initial_cash", "final_nav", "cumulative_return", "annualized_return",
		"volatility", "sharpe_ratio", "max_drawdown", "win_rate",
		"trade_count", "winning_trade_count", "losing_trade_count",
		"total_days", "done_days",
		"submitted_at", "started_at", "completed_at",
		"sweep_id", "sweep_cell", "walk_forward",
	}
	mock.ExpectQuery(`SELECT id, fund_id`).
		WithArgs("job-old").
		WillReturnRows(sqlmock.NewRows(cols).AddRow(
			"job-old", "fund-1", "user-1", "old", "fallback", "completed", []byte(`{}`), nil,
			start, start.AddDate(0, 0, 5), sql.NullFloat64{Float64: 1000, Valid: true}, sql.NullFloat64{Float64: 1234, Valid: true},
			sql.NullFloat64{Float64: 0.234, Valid: true}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{},
			5, 3, 2, 5, 5, start, sql.NullTime{Time: start, Valid: true}, sql.NullTime{Time: start.AddDate(0, 0, 5), Valid: true},
			sql.NullString{}, []byte(`null`), []byte(`null`),
		))
	mock.ExpectQuery(`SELECT seq, date, nav, cash`).WithArgs("job-old").
		WillReturnRows(sqlmock.NewRows([]string{"seq", "date", "nav", "cash", "position_value", "drawdown_pct", "positions"}))
	mock.ExpectQuery(`SELECT seq, date, symbol, action`).WithArgs("job-old").
		WillReturnRows(sqlmock.NewRows([]string{"seq", "date", "symbol", "action", "status", "quantity", "fill_price", "notional", "reason", "confidence"}))

	job, err := adapter.GetBacktest("user-x", "fund-1", "job-old")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job == nil || job.ID != "job-old" || job.Status != "completed" {
		t.Errorf("unexpected job from DB fallback: %+v", job)
	}
	if job.Result == nil || job.Result.FinalNav != 1234 {
		t.Errorf("result projection wrong: %+v", job.Result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Get returns nil when the DB row belongs to a different fund —
// the caller's URL fundId is the authoritative scope.
func TestAdapterGetBacktestRejectsCrossFundLookup(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	adapter, mock, cleanup := newPersistentAdapter(t, nil)
	defer cleanup()
	cols := []string{
		"id", "fund_id", "user_id", "name", "engine_kind", "status", "request", "error",
		"window_start", "window_end",
		"initial_cash", "final_nav", "cumulative_return", "annualized_return",
		"volatility", "sharpe_ratio", "max_drawdown", "win_rate",
		"trade_count", "winning_trade_count", "losing_trade_count",
		"total_days", "done_days",
		"submitted_at", "started_at", "completed_at",
		"sweep_id", "sweep_cell", "walk_forward",
	}
	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("job-y").
		WillReturnRows(sqlmock.NewRows(cols).AddRow(
			"job-y", "fund-OTHER", "user-1", "x", "fallback", "completed", []byte(`{}`), nil,
			start, start, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{},
			sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{},
			0, 0, 0, 0, 0, start, sql.NullTime{}, sql.NullTime{},
			sql.NullString{}, []byte(`null`), []byte(`null`)))
	mock.ExpectQuery(`SELECT seq, date, nav, cash`).WithArgs("job-y").
		WillReturnRows(sqlmock.NewRows([]string{"seq", "date", "nav", "cash", "position_value", "drawdown_pct", "positions"}))
	mock.ExpectQuery(`SELECT seq, date, symbol, action`).WithArgs("job-y").
		WillReturnRows(sqlmock.NewRows([]string{"seq", "date", "symbol", "action", "status", "quantity", "fill_price", "notional", "reason", "confidence"}))

	job, err := adapter.GetBacktest("user-x", "fund-1", "job-y")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job != nil {
		t.Errorf("expected nil for cross-fund lookup, got %+v", job)
	}
}

// rowToView translates a persisted row back into the API shape.
func TestRowToViewProjectsAllFields(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	row := repository.BacktestJobRow{
		ID:               "job-1",
		FundID:           "fund-1",
		Name:             "trial",
		EngineKind:       "fallback",
		Status:           "completed",
		Request:          []byte(`{"symbols":["AAPL"],"initialCash":1000,"start":"2026-01-02T00:00:00Z","end":"2026-01-07T00:00:00Z","slippageBps":5,"commissionBps":5,"maxOrdersPerDay":5}`),
		InitialCash:      sql.NullFloat64{Float64: 1000, Valid: true},
		FinalNav:         sql.NullFloat64{Float64: 1100, Valid: true},
		CumulativeReturn: sql.NullFloat64{Float64: 0.1, Valid: true},
		TradeCount:       2,
		SubmittedAt:      start,
		StartedAt:        sql.NullTime{Time: start, Valid: true},
		CompletedAt:      sql.NullTime{Time: start.AddDate(0, 0, 5), Valid: true},
	}
	view := rowToView(row, []repository.BacktestNavPoint{
		{Seq: 0, Date: start, Nav: 1000, Cash: 1000},
		{Seq: 1, Date: start.AddDate(0, 0, 1), Nav: 1100, Cash: 950, PositionValue: 150, Positions: []byte(`{"AAPL":1}`)},
	}, []repository.BacktestTradeEvent{
		{Seq: 0, Date: start, Symbol: "AAPL", Action: "buy", Status: "filled", Quantity: 1, FillPrice: 150, Notional: 150},
	})
	if view.Status != "completed" || view.Result == nil {
		t.Fatalf("view incomplete: %+v", view)
	}
	if view.Result.FinalNav != 1100 || len(view.Result.NavCurve) != 2 || len(view.Result.Trades) != 1 {
		t.Errorf("result projection wrong: %+v", view.Result)
	}
	if view.Request == nil || len(view.Request.Symbols) != 1 {
		t.Errorf("request echo not decoded: %+v", view.Request)
	}
	if view.Result.NavCurve[1].Positions["AAPL"] != 1 {
		t.Errorf("positions map not decoded")
	}
}

// Sweep is invoked at adapter construction and clears any
// queued/running rows.
func TestAdapterSweepInterruptedJobsOnConstruct(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectExec(`UPDATE backtest_jobs\s+SET status = 'failed'`).
		WillReturnResult(sqlmock.NewResult(0, 2))

	_ = newBacktestServiceAdapter(db, nil, nil)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// -------------------- helpers --------------------

// neverRunsEngine is an Engine that should never be invoked
// because the test injects fake jobs directly into the store.
type neverRunsEngine struct{}

func (neverRunsEngine) Run(_ context.Context, _ backtest.Request, _ *backtest.Progress) (*backtest.Result, error) {
	return nil, errors.New("never runs in this test")
}

// buildSyntheticBars creates a flat OHLC series for tests that
// just need bars to flow through the runner without driving any
// real signal. (Mirrors the helper in backtest_test.go but lives
// here because the test files are in different packages.)
func buildSyntheticBars(start time.Time, closes []float64) []ohlc.Bar {
	bars := make([]ohlc.Bar, len(closes))
	for i, c := range closes {
		bars[i] = ohlc.Bar{
			Time: start.AddDate(0, 0, i), Open: c, High: c, Low: c, Close: c, Volume: 1000,
		}
	}
	return bars
}

func waitJobStatus(store *backtest.JobStore, id, want string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job := store.Get(id)
		if job != nil && job.Progress.Snapshot().Status == want {
			return want
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job := store.Get(id); job != nil {
		return job.Progress.Snapshot().Status
	}
	return ""
}

// Adapter.CompareBacktests: both jobs in-memory + completed →
// returns deltas. Uses real in-memory store with completed Jobs
// to exercise the full path including GetBacktest lookups.
func TestAdapterCompareBacktestsInMemoryHappy(t *testing.T) {
	adapter := &backtestServiceAdapter{
		ohlcFetcher: &stubFetcher{},
		userIDByJob: make(map[string]string),
	}
	adapter.store = backtest.NewJobStore(neverRunsEngine{})

	now := time.Now().UTC()
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	jobA := makeCompletedJob("job-a", "fund-1", start, end, []string{"AAPL"}, 1000, 1100, 0.1, 0.5, now)
	jobB := makeCompletedJob("job-b", "fund-1", start, end, []string{"AAPL"}, 1000, 1250, 0.25, 1.2, now)
	injectJobsIntoStore(adapter.store, jobA, jobB)

	cmp, err := adapter.CompareBacktests("user-x", "fund-1", "job-a", "job-b")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if cmp == nil || cmp.A == nil || cmp.B == nil {
		t.Fatal("nil comparison")
	}
	// B is +0.15 cumulative, +0.7 sharpe, +150 finalNav.
	if d := cmp.Diff.CumulativeReturnDelta; d < 0.149 || d > 0.151 {
		t.Errorf("cumulativeDelta = %f, want ~0.15", d)
	}
	if d := cmp.Diff.SharpeDelta; d < 0.69 || d > 0.71 {
		t.Errorf("sharpeDelta = %f, want ~0.7", d)
	}
	if d := cmp.Diff.FinalNavDelta; d < 149 || d > 151 {
		t.Errorf("finalNavDelta = %f, want ~150", d)
	}
	if !cmp.Diff.SameWindow || !cmp.Diff.SameUniverse {
		t.Errorf("expected SameWindow + SameUniverse, got %+v", cmp.Diff)
	}
}

// Different windows → SameWindow=false.
func TestAdapterCompareFlagsDifferentWindowAndUniverse(t *testing.T) {
	adapter := &backtestServiceAdapter{
		ohlcFetcher: &stubFetcher{},
		userIDByJob: make(map[string]string),
	}
	adapter.store = backtest.NewJobStore(neverRunsEngine{})
	now := time.Now().UTC()
	startA := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	endA := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	startB := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC) // different year
	endB := time.Date(2025, 6, 30, 0, 0, 0, 0, time.UTC)
	jobA := makeCompletedJob("job-a", "fund-1", startA, endA, []string{"AAPL"}, 1000, 1100, 0.1, 0.5, now)
	jobB := makeCompletedJob("job-b", "fund-1", startB, endB, []string{"MSFT", "GOOG"}, 1000, 1100, 0.1, 0.5, now)
	injectJobsIntoStore(adapter.store, jobA, jobB)

	cmp, err := adapter.CompareBacktests("u", "fund-1", "job-a", "job-b")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if cmp.Diff.SameWindow {
		t.Errorf("SameWindow should be false")
	}
	if cmp.Diff.SameUniverse {
		t.Errorf("SameUniverse should be false")
	}
}

// Universe set-equality is order-/case-insensitive.
func TestAdapterCompareUniverseSetEquality(t *testing.T) {
	adapter := &backtestServiceAdapter{
		ohlcFetcher: &stubFetcher{},
		userIDByJob: make(map[string]string),
	}
	adapter.store = backtest.NewJobStore(neverRunsEngine{})
	now := time.Now().UTC()
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	jobA := makeCompletedJob("job-a", "fund-1", start, end, []string{"AAPL", "msft"}, 1000, 1100, 0, 0, now)
	jobB := makeCompletedJob("job-b", "fund-1", start, end, []string{"MSFT", "aapl"}, 1000, 1100, 0, 0, now)
	injectJobsIntoStore(adapter.store, jobA, jobB)

	cmp, err := adapter.CompareBacktests("u", "fund-1", "job-a", "job-b")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !cmp.Diff.SameUniverse {
		t.Errorf("set-equal universes should compare SameUniverse=true")
	}
}

// One job still running → ErrBacktestNotComparable.
func TestAdapterCompareRejectsActiveJob(t *testing.T) {
	adapter := &backtestServiceAdapter{
		ohlcFetcher: &stubFetcher{},
		userIDByJob: make(map[string]string),
	}
	adapter.store = backtest.NewJobStore(neverRunsEngine{})
	now := time.Now().UTC()
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	completed := makeCompletedJob("job-a", "fund-1", start, end, []string{"AAPL"}, 1000, 1100, 0.1, 0.5, now)
	active := &backtest.Job{
		ID:          "job-b",
		Request:     backtest.Request{FundID: "fund-1", Symbols: []string{"AAPL"}, Start: start, End: end},
		SubmittedAt: now,
		Progress:    &backtest.Progress{Status: "running"},
	}
	injectJobsIntoStore(adapter.store, completed, active)

	_, err := adapter.CompareBacktests("u", "fund-1", "job-a", "job-b")
	if !errors.Is(err, api.ErrBacktestNotComparable) {
		t.Errorf("expected ErrBacktestNotComparable, got %v", err)
	}
}

// Cross-fund attempt → nil (handler maps to 404).
func TestAdapterCompareRejectsCrossFund(t *testing.T) {
	adapter := &backtestServiceAdapter{
		ohlcFetcher: &stubFetcher{},
		userIDByJob: make(map[string]string),
	}
	adapter.store = backtest.NewJobStore(neverRunsEngine{})
	now := time.Now().UTC()
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	jobA := makeCompletedJob("job-a", "fund-1", start, end, []string{"AAPL"}, 1000, 1100, 0.1, 0.5, now)
	jobB := makeCompletedJob("job-b", "fund-OTHER", start, end, []string{"AAPL"}, 1000, 1100, 0.1, 0.5, now)
	injectJobsIntoStore(adapter.store, jobA, jobB)

	cmp, err := adapter.CompareBacktests("u", "fund-1", "job-a", "job-b")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if cmp != nil {
		t.Errorf("expected nil for cross-fund compare, got %+v", cmp)
	}
}

// makeCompletedJob constructs a synthetic completed Job suitable
// for the in-memory test path. The Result has minimal fields
// because the compare path only consumes Metrics + FinalNav.
func makeCompletedJob(id, fundID string, start, end time.Time, symbols []string, initial, final, cumret, sharpe float64, now time.Time) *backtest.Job {
	return &backtest.Job{
		ID: id,
		Request: backtest.Request{
			FundID: fundID, Symbols: symbols, Start: start, End: end,
			InitialCash: initial, EngineKind: "fallback", BaseCurrency: "USD",
			SlippageBps: 5, CommissionBps: 5, MaxOrdersPerDay: 5,
		},
		SubmittedAt: now,
		StartedAt:   now,
		CompletedAt: now.Add(time.Minute),
		Progress:    &backtest.Progress{Status: "completed", TotalDays: 100, DoneDays: 100},
		Result: &backtest.Result{
			FundID: fundID, EngineKind: "fallback",
			Start: start, End: end,
			InitialCash: initial, FinalNav: final,
			Metrics: backtest.Metrics{
				CumulativeReturn: cumret,
				SharpeRatio:      sharpe,
			},
			CompletedAt: now.Add(time.Minute),
		},
	}
}

// injectJobsIntoStore peeks into the JobStore internals via
// Submit/Cancel acrobatics — but the cleanest path is to mutate
// the underlying map directly. The store doesn't expose that
// publicly so we use reflection to keep the test focused on the
// compare logic rather than the submit machinery.
func injectJobsIntoStore(store *backtest.JobStore, jobs ...*backtest.Job) {
	for _, j := range jobs {
		store.AddJobForTest(j)
	}
}

// jobToView populates Status / Request / Result fields from a
// completed Job.
func TestJobToViewProjectsResult(t *testing.T) {
	job := &backtest.Job{
		ID:          "j1",
		Request:     backtest.Request{FundID: "fund-1", Name: "trial", EngineKind: "fallback", Symbols: []string{"AAPL"}},
		SubmittedAt: time.Now(),
		Progress:    &backtest.Progress{Status: "completed", TotalDays: 5, DoneDays: 5},
		Result: &backtest.Result{
			InitialCash: 1000, FinalNav: 1100,
			NavCurve: []backtest.NavPoint{{Date: time.Now(), Nav: 1100, Cash: 100, PositionValue: 1000}},
			Trades:   []backtest.TradeEvent{{Symbol: "AAPL", Action: "buy", Status: "filled"}},
			Metrics:  backtest.Metrics{CumulativeReturn: 0.1, TradeCount: 1},
		},
	}
	view := jobToView(job)
	if view.ID != "j1" || view.Status != "completed" {
		t.Errorf("view header wrong: %+v", view)
	}
	if view.Result == nil || view.Result.FinalNav != 1100 || len(view.Result.NavCurve) != 1 || len(view.Result.Trades) != 1 {
		t.Errorf("result projection wrong: %+v", view.Result)
	}
	if view.Request == nil || len(view.Request.Symbols) != 1 {
		t.Errorf("request echo wrong: %+v", view.Request)
	}
}
