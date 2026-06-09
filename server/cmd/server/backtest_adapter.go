package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/backtest"
	"github.com/fundai/server/internal/decision"
	"github.com/fundai/server/internal/llm"
	"github.com/fundai/server/internal/ohlc"
	"github.com/fundai/server/internal/repository"
)

// backtestServiceAdapter wires Phase 2E's in-memory backtest layer
// behind the api.BacktestService façade. Responsibilities:
//
//   1. Authorise per-fund access by reusing repository.FundRepo —
//      the same path the production GetFund / UpdateFund handlers
//      walk, so we don't drift from the platform's existing
//      RBAC story.
//   2. Translate api.SubmitBacktestInput ↔ backtest.Request so the
//      api package can stay free of the backtest/ohlc/decision
//      transitive imports.
//   3. Pick the right DecisionEngine for the requested EngineKind:
//      "fallback" (default) → decision.FallbackEngine (no LLM
//      cost, deterministic); "llm" → decision.LLMDecisionEngine
//      using the platform's shared LLM client; other / unknown →
//      "fallback".
//   4. Run the JobStore. There is exactly one shared JobStore per
//      process — backtest jobs are operator-driven and short, so a
//      single in-memory ledger is enough.
//
// Nil-safety: every method tolerates a nil ohlcFetcher (legacy
// deployments without OHLC); SubmitBacktest returns a friendly
// service-unavailable error, the list endpoint returns an empty
// slice (handled in the handler layer).
type backtestServiceAdapter struct {
	db           *sql.DB
	fundRepo     *repository.FundRepo
	companyRepo  *repository.FundCompanyRepo
	backtestRepo *repository.BacktestRepo
	sweepRepo    *repository.SweepRepo
	ohlcFetcher  ohlc.Fetcher
	llmClient    llm.LLMClient
	store        *backtest.JobStore
	// submitMu serialises Submit calls so the pending* slots are
	// safely paired with the OnSubmit hook that consumes them.
	// Submit completes synchronously w.r.t. OnSubmit (the hook
	// runs inside store.Submit before its goroutine spawns) so
	// holding the lock across Submit isn't a long-lived
	// contention point.
	submitMu       sync.Mutex
	pendingUserID  string
	pendingSweepID string
	// pendingCell carries the axis-name → value map for the
	// current SweepSubmit child. Reset to nil between Submit
	// calls so one-off backtests aren't tagged with stale cells.
	pendingCell    map[string]string

	jobsMu      sync.Mutex
	userIDByJob map[string]string
	// sweepByJob lets persistSubmitted's symmetric persistFinal
	// reach the sweep header without re-querying the DB. Cleared
	// on persistFinal.
	sweepByJob map[string]sweepCellRef
}

// sweepCellRef remembers which sweep a child job belongs to and
// which axis cell it represents. Captured at submit-time, used
// at final-time (currently only for logging — the row already
// carries sweep_id by then).
type sweepCellRef struct {
	sweepID string
	cell    map[string]string
}

// newBacktestServiceAdapter constructs the adapter and starts an
// in-memory JobStore. ohlcFetcher is required for any real run; we
// still build the adapter when nil so the rest of the wiring path
// doesn't have to special-case the missing-provider deployment.
//
// Persistence (Phase 2F) is enabled when db is non-nil. When db is
// nil the adapter still works — just without long-term history.
// Tests that don't care about persistence can keep passing nil.
func newBacktestServiceAdapter(db *sql.DB, ohlcFetcher ohlc.Fetcher, llmClient llm.LLMClient) *backtestServiceAdapter {
	adapter := &backtestServiceAdapter{
		db:          db,
		ohlcFetcher: ohlcFetcher,
		llmClient:   llmClient,
		userIDByJob: make(map[string]string, 16),
		sweepByJob:  make(map[string]sweepCellRef, 16),
	}
	if db != nil {
		adapter.fundRepo = repository.NewFundRepo(db)
		adapter.companyRepo = repository.NewFundCompanyRepo(db)
		adapter.backtestRepo = repository.NewBacktestRepo(db)
		adapter.sweepRepo = repository.NewSweepRepo(db)
	}
	// The engine the JobStore wraps is constructed per-request
	// inside Submit because EngineKind varies per submission. We
	// hand the JobStore a *backtestServiceAdapter as the Engine
	// directly — it dispatches on Request.EngineKind to pick the
	// concrete decision.Engine.
	adapter.store = backtest.NewJobStore(adapter)
	adapter.store.OnSubmit = adapter.persistSubmitted
	adapter.store.OnFinal = adapter.persistFinal
	if adapter.backtestRepo != nil {
		adapter.sweepInterruptedJobs(context.Background())
	}
	return adapter
}

// sweepInterruptedJobs marks any queued/running rows as failed
// with the "server restart" error so the operator UI shows them
// as terminated. We deliberately don't auto-resume — backtest
// state (portfolio lots, peakNav) lives only in memory.
func (s *backtestServiceAdapter) sweepInterruptedJobs(ctx context.Context) {
	if s.backtestRepo == nil {
		return
	}
	n, err := s.backtestRepo.MarkInterruptedActive(ctx, time.Now().UTC())
	if err != nil {
		slog.Warn("backtest: failed to sweep interrupted jobs", "err", err)
		return
	}
	if n > 0 {
		slog.Info("backtest: swept interrupted jobs on startup", "count", n)
	}
}

// persistSubmitted is the JobStore.OnSubmit hook. It writes the
// initial 'queued' row to Postgres so a process crash doesn't
// erase the audit trail. Returns an error only when persistence
// is required and fails — if backtestRepo is nil (test
// deployments / DB disabled), we silently no-op.
//
// We also stash the userID into userIDByJob keyed by the freshly-
// minted job.ID so the matching OnFinal hook can update the row
// without consulting the API layer.
func (s *backtestServiceAdapter) persistSubmitted(job *backtest.Job) error {
	userID := s.pendingUserID
	sweepID := s.pendingSweepID
	// Defensive copy: cell map is owned by SubmitSweep's
	// iteration loop; reusing the pointer would race the next
	// cell's overwrite.
	var cellCopy map[string]string
	if len(s.pendingCell) > 0 {
		cellCopy = make(map[string]string, len(s.pendingCell))
		for k, v := range s.pendingCell {
			cellCopy[k] = v
		}
	}

	s.jobsMu.Lock()
	if s.userIDByJob == nil {
		s.userIDByJob = make(map[string]string, 4)
	}
	if s.sweepByJob == nil {
		s.sweepByJob = make(map[string]sweepCellRef, 4)
	}
	s.userIDByJob[job.ID] = userID
	if sweepID != "" {
		s.sweepByJob[job.ID] = sweepCellRef{sweepID: sweepID, cell: cellCopy}
	}
	s.jobsMu.Unlock()
	if s.backtestRepo == nil {
		return nil
	}
	reqJSON, err := json.Marshal(job.Request)
	if err != nil {
		return fmt.Errorf("backtest: marshal request: %w", err)
	}
	row := &repository.BacktestJobRow{
		ID:          job.ID,
		FundID:      job.Request.FundID,
		UserID:      userID,
		Name:        job.Request.Name,
		EngineKind:  job.Request.EngineKind,
		Status:      "queued",
		Request:     reqJSON,
		WindowStart: job.Request.Start.UTC(),
		WindowEnd:   job.Request.End.UTC(),
		SubmittedAt: job.SubmittedAt.UTC(),
	}
	if sweepID != "" {
		row.SweepID = sql.NullString{String: sweepID, Valid: true}
		cellJSON, err := json.Marshal(cellCopy)
		if err != nil {
			return fmt.Errorf("backtest: marshal sweep cell: %w", err)
		}
		row.SweepCell = cellJSON
	}
	if err := s.backtestRepo.InsertQueued(context.Background(), row); err != nil {
		// Roll back our in-memory userID/sweep stamps so a
		// re-submit (after the operator inspects the DB)
		// doesn't conflate the two attempts.
		s.jobsMu.Lock()
		delete(s.userIDByJob, job.ID)
		delete(s.sweepByJob, job.ID)
		s.jobsMu.Unlock()
		return err
	}
	return nil
}

// persistFinal is the JobStore.OnFinal hook. Writes the terminal
// status + metrics + NAV curve + trade events in one TX. Errors
// are logged-not-propagated since the in-memory run already
// settled — surfacing here would mislead callers into thinking
// the run itself failed.
func (s *backtestServiceAdapter) persistFinal(job *backtest.Job) {
	if s.backtestRepo == nil {
		return
	}
	s.jobsMu.Lock()
	userID := s.userIDByJob[job.ID]
	delete(s.userIDByJob, job.ID)
	delete(s.sweepByJob, job.ID)
	s.jobsMu.Unlock()

	snap := job.Progress.Snapshot()
	row := &repository.BacktestJobRow{
		ID:          job.ID,
		FundID:      job.Request.FundID,
		UserID:      userID,
		Name:        job.Request.Name,
		EngineKind:  job.Request.EngineKind,
		Status:      snap.Status,
		Error:       toNullString(snap.LastError),
		WindowStart: job.Request.Start.UTC(),
		WindowEnd:   job.Request.End.UTC(),
		SubmittedAt: job.SubmittedAt.UTC(),
		StartedAt:   toNullTime(job.StartedAt),
		CompletedAt: toNullTime(job.CompletedAt),
		TotalDays:   snap.TotalDays,
		DoneDays:    snap.DoneDays,
	}
	var nav []repository.BacktestNavPoint
	var trades []repository.BacktestTradeEvent
	if job.Result != nil {
		row.InitialCash = toNullFloat(job.Result.InitialCash)
		row.FinalNav = toNullFloat(job.Result.FinalNav)
		row.CumulativeReturn = toNullFloat(job.Result.Metrics.CumulativeReturn)
		row.AnnualizedReturn = toNullFloat(job.Result.Metrics.AnnualizedReturn)
		row.Volatility = toNullFloat(job.Result.Metrics.Volatility)
		row.SharpeRatio = toNullFloat(job.Result.Metrics.SharpeRatio)
		row.MaxDrawdown = toNullFloat(job.Result.Metrics.MaxDrawdown)
		row.WinRate = toNullFloat(job.Result.Metrics.WinRate)
		row.TradeCount = job.Result.Metrics.TradeCount
		row.WinningTradeCount = job.Result.Metrics.WinningTradeCount
		row.LosingTradeCount = job.Result.Metrics.LosingTradeCount
		nav = translateNavToRepo(job.Result.NavCurve)
		trades = translateTradesToRepo(job.Result.Trades)
		// Persist the per-fold breakdown for walk-forward runs.
		// Marshalled here rather than in the repo so the repo
		// stays neutral to the structure of the blob.
		if job.Result.WalkForward != nil {
			if blob, err := json.Marshal(translateWalkForwardResult(job.Result.WalkForward)); err == nil {
				row.WalkForward = blob
			} else {
				slog.Warn("backtest: marshal walk_forward failed", "job_id", job.ID, "err", err)
			}
		}
	}
	if err := s.backtestRepo.UpdateFinal(context.Background(), row, nav, trades); err != nil {
		slog.Warn("backtest: persist final state failed", "job_id", job.ID, "err", err)
	}
}

// Run implements backtest.Engine so backtestServiceAdapter can act
// as the JobStore's runner. It builds the per-request engine on
// the fly and delegates to backtest.Runner — or, when the request
// carries a WalkForward sub-spec, to backtest.WalkForwardRunner
// which loops the inner runner over each fold and stitches.
func (s *backtestServiceAdapter) Run(ctx context.Context, req backtest.Request, progress *backtest.Progress) (*backtest.Result, error) {
	if s == nil || s.ohlcFetcher == nil {
		return nil, errors.New("backtest: OHLC provider not configured")
	}
	engine := s.pickDecisionEngine(req.EngineKind)
	inner := &backtest.Runner{
		OHLC:   s.ohlcFetcher,
		Decide: engine,
	}
	if req.WalkForward != nil {
		wf := &backtest.WalkForwardRunner{Inner: inner}
		return wf.Run(ctx, req, progress)
	}
	return inner.Run(ctx, req, progress)
}

// pickDecisionEngine maps EngineKind to a concrete
// decision.DecisionEngine. Unknown / empty kinds collapse to the
// fallback engine.
func (s *backtestServiceAdapter) pickDecisionEngine(kind string) decision.DecisionEngine {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "llm":
		if s.llmClient == nil {
			// Operator asked for LLM but the platform isn't
			// wired with one (test deployments). Fall back to
			// the deterministic engine rather than failing the
			// whole job — surfacing the degradation via the
			// trade log is better UX than a hard error.
			return decision.FallbackEngine{}
		}
		return &decision.LLMDecisionEngine{
			Client:    s.llmClient,
			ModelTier: llm.TierCritical,
			StepName:  "backtest_pm_decision",
		}
	case "llm-debate":
		// Reserved for Phase 2E.2 — for now alias to "llm"; the
		// debate roundtable adds significant per-day cost so we
		// deliberately don't enable it in the backtest path
		// without an explicit follow-up PR that confirms cost
		// guardrails.
		if s.llmClient == nil {
			return decision.FallbackEngine{}
		}
		return &decision.LLMDecisionEngine{
			Client:    s.llmClient,
			ModelTier: llm.TierCritical,
			StepName:  "backtest_pm_decision",
		}
	default:
		return decision.FallbackEngine{}
	}
}

// SubmitBacktest implements api.BacktestService.
func (s *backtestServiceAdapter) SubmitBacktest(userID string, input api.SubmitBacktestInput) (*api.BacktestJob, error) {
	if s.ohlcFetcher == nil {
		return nil, api.ErrBacktestUnconfigured
	}
	if err := s.authorize(userID, input.FundID); err != nil {
		return nil, err
	}
	req := translateSubmitInput(input)
	// Validate the walk-forward sub-spec early so the user gets a
	// 400 instead of a queued job that fails when its goroutine
	// fires. We piggy-back on the planner because it does the
	// authoritative check.
	if req.WalkForward != nil {
		if _, err := backtest.PlanWalkForward(req.Start, req.End, *req.WalkForward); err != nil {
			if errors.Is(err, backtest.ErrWalkForwardInvalid) {
				return nil, fmt.Errorf("%w: %s", api.ErrWalkForwardInvalid, err.Error())
			}
			return nil, err
		}
	}
	// submitMu pairs the pending userID with the synchronous
	// OnSubmit hook that JobStore.Submit fires before any
	// goroutine spawns. Holding the lock through Submit is
	// short-lived (a single DB insert).
	s.submitMu.Lock()
	s.pendingUserID = userID
	job, err := s.store.Submit(context.Background(), req)
	s.pendingUserID = ""
	s.submitMu.Unlock()
	if err != nil {
		return nil, err
	}
	return jobToView(job), nil
}

// ListBacktests implements api.BacktestService. Active jobs come
// from the in-memory store; historical jobs come from the DB. We
// union by ID with in-memory winning so a queued job that was
// also persisted at submit time doesn't show twice.
func (s *backtestServiceAdapter) ListBacktests(userID, fundID string) ([]*api.BacktestJob, error) {
	if err := s.authorize(userID, fundID); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, 32)
	out := make([]*api.BacktestJob, 0, 32)
	for _, j := range s.store.List(fundID) {
		out = append(out, jobToView(j))
		seen[j.ID] = struct{}{}
	}
	if s.backtestRepo != nil {
		rows, err := s.backtestRepo.ListByFund(context.Background(), fundID, 100)
		if err == nil {
			for _, row := range rows {
				if _, dup := seen[row.ID]; dup {
					continue
				}
				out = append(out, rowToView(row, nil, nil))
				seen[row.ID] = struct{}{}
			}
		} else {
			slog.Warn("backtest: list from db failed", "fund_id", fundID, "err", err)
		}
	}
	return out, nil
}

// GetBacktest implements api.BacktestService. Active jobs come
// from the in-memory store; historical jobs come from the DB.
func (s *backtestServiceAdapter) GetBacktest(userID, fundID, jobID string) (*api.BacktestJob, error) {
	if err := s.authorize(userID, fundID); err != nil {
		return nil, err
	}
	if job := s.store.Get(jobID); job != nil && job.Request.FundID == fundID {
		return jobToView(job), nil
	}
	if s.backtestRepo == nil {
		return nil, nil
	}
	full, err := s.backtestRepo.GetWithDetails(context.Background(), jobID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if full.Job.FundID != fundID {
		return nil, nil
	}
	return rowToView(full.Job, full.Nav, full.Trades), nil
}

// CompareBacktests implements api.BacktestService. Reads both
// jobs (via the same DB-or-memory fallthrough used by
// GetBacktest), verifies both completed, and builds the deltas +
// "same window / same universe" flags. Both jobs MUST belong to
// the URL's fundID — the read path enforces this so a malicious
// jobID can't sneak across fund boundaries.
func (s *backtestServiceAdapter) CompareBacktests(userID, fundID, jobIDA, jobIDB string) (*api.BacktestComparison, error) {
	if err := s.authorize(userID, fundID); err != nil {
		return nil, err
	}
	a, err := s.GetBacktest(userID, fundID, jobIDA)
	if err != nil {
		return nil, err
	}
	b, err := s.GetBacktest(userID, fundID, jobIDB)
	if err != nil {
		return nil, err
	}
	if a == nil || b == nil {
		return nil, nil
	}
	if a.Status != "completed" || b.Status != "completed" || a.Result == nil || b.Result == nil {
		return nil, api.ErrBacktestNotComparable
	}
	diff := api.BacktestComparisonDiff{
		CumulativeReturnDelta: b.Result.Metrics.CumulativeReturn - a.Result.Metrics.CumulativeReturn,
		AnnualizedReturnDelta: b.Result.Metrics.AnnualizedReturn - a.Result.Metrics.AnnualizedReturn,
		VolatilityDelta:       b.Result.Metrics.Volatility - a.Result.Metrics.Volatility,
		SharpeDelta:           b.Result.Metrics.SharpeRatio - a.Result.Metrics.SharpeRatio,
		MaxDrawdownDelta:      b.Result.Metrics.MaxDrawdown - a.Result.Metrics.MaxDrawdown,
		WinRateDelta:          b.Result.Metrics.WinRate - a.Result.Metrics.WinRate,
		TradeCountDelta:       b.Result.Metrics.TradeCount - a.Result.Metrics.TradeCount,
		FinalNavDelta:         b.Result.FinalNav - a.Result.FinalNav,
		SameWindow:            sameBacktestWindow(a.Request, b.Request),
		SameUniverse:          sameBacktestUniverse(a.Request, b.Request),
	}
	return &api.BacktestComparison{A: a, B: b, Diff: diff}, nil
}

// sameBacktestWindow checks whether two request echoes cover the
// identical [start, end] window. Nil-safe (defaults to false on
// missing echo).
func sameBacktestWindow(a, b *api.BacktestRequestEcho) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Start.Equal(b.Start) && a.End.Equal(b.End)
}

// sameBacktestUniverse compares the two requests' symbol lists as
// sets (order-insensitive, case-insensitive). Used by the diff to
// flag "you're comparing two different universes" runs.
func sameBacktestUniverse(a, b *api.BacktestRequestEcho) bool {
	if a == nil || b == nil {
		return false
	}
	if len(a.Symbols) != len(b.Symbols) {
		return false
	}
	seen := make(map[string]struct{}, len(a.Symbols))
	for _, s := range a.Symbols {
		seen[strings.ToUpper(strings.TrimSpace(s))] = struct{}{}
	}
	for _, s := range b.Symbols {
		if _, ok := seen[strings.ToUpper(strings.TrimSpace(s))]; !ok {
			return false
		}
	}
	return true
}

// CancelBacktest implements api.BacktestService.
func (s *backtestServiceAdapter) CancelBacktest(userID, fundID, jobID string) (bool, error) {
	if err := s.authorize(userID, fundID); err != nil {
		return false, err
	}
	job := s.store.Get(jobID)
	if job == nil || job.Request.FundID != fundID {
		return false, nil
	}
	status := job.Progress.Snapshot().Status
	if status == "completed" || status == "failed" || status == "cancelled" {
		return false, nil
	}
	cancelled := s.store.Cancel(jobID)
	// JobStore's run() goroutine will exit with ErrCancelled and
	// the OnFinal hook will then write status='cancelled' to the
	// DB. We don't double-write here.
	return cancelled, nil
}

// authorize delegates to the platform's shared authorizeFundAccess
// helper — the same RBAC every other fund-scoped endpoint walks.
// We deliberately read the fund (not just check ownership) so
// deleted / archived funds yield NotFound before the per-request
// logic runs.
func (s *backtestServiceAdapter) authorize(userID, fundID string) error {
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("backtest: userID required")
	}
	if strings.TrimSpace(fundID) == "" {
		return fmt.Errorf("backtest: fundID required")
	}
	if s.fundRepo == nil {
		return nil // test paths
	}
	_, err := authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, fundID)
	return err
}

// translateSubmitInput is the SubmitBacktestInput → backtest.Request
// projection. We keep this pure so the wiring layer can unit-test
// it without spinning up a job store.
func translateSubmitInput(in api.SubmitBacktestInput) backtest.Request {
	positions := make([]backtest.InitialPosition, 0, len(in.InitialPositions))
	for _, p := range in.InitialPositions {
		positions = append(positions, backtest.InitialPosition{
			Symbol:    p.Symbol,
			Quantity:  p.Quantity,
			CostPrice: p.CostPrice,
		})
	}
	req := backtest.Request{
		FundID:             in.FundID,
		Name:               in.Name,
		Market:             in.Market,
		Symbols:            in.Symbols,
		InitialPositions:   positions,
		Start:              in.Start.UTC(),
		End:                in.End.UTC(),
		InitialCash:        in.InitialCash,
		BaseCurrency:       in.BaseCurrency,
		SlippageBps:        in.SlippageBps,
		CommissionBps:      in.CommissionBps,
		MaxOrdersPerDay:    in.MaxOrdersPerDay,
		EngineKind:         in.EngineKind,
		BenchmarkSymbol:    in.BenchmarkSymbol,
		RebalanceFrequency: in.RebalanceFrequency,
	}
	if in.WalkForward != nil {
		req.WalkForward = &backtest.WalkForwardSpec{
			NumFolds:   in.WalkForward.NumFolds,
			TrainRatio: in.WalkForward.TrainRatio,
			Mode:       backtest.WalkForwardMode(in.WalkForward.Mode),
		}
	}
	return req
}

// jobToView produces the JSON-friendly view of a backtest.Job for
// the api response. The handler can also pass in nil safely.
//
// W14-4 — uses Job.Snapshot() to read the runner-mutable fields
// (StartedAt / CompletedAt / Result / Err) under Job.mu.
// Reading those fields directly from this goroutine (the API
// handler) races with the runner goroutine that writes them.
func jobToView(job *backtest.Job) *api.BacktestJob {
	if job == nil {
		return nil
	}
	snap := job.Snapshot()
	view := &api.BacktestJob{
		ID:         snap.ID,
		FundID:     snap.Request.FundID,
		Name:       snap.Request.Name,
		EngineKind: snap.Request.EngineKind,
		Status:     snap.Progress.Status,
		Progress: api.BacktestProgressView{
			TotalDays:   snap.Progress.TotalDays,
			DoneDays:    snap.Progress.DoneDays,
			CurrentDate: snap.Progress.CurrentDate,
		},
		SubmittedAt: snap.SubmittedAt,
		StartedAt:   snap.StartedAt,
		CompletedAt: snap.CompletedAt,
		Request:     translateRequestEcho(snap.Request),
	}
	if snap.Err != nil {
		view.Error = snap.Err.Error()
	}
	if snap.Result != nil {
		view.Result = translateResultView(snap.Result)
	}
	return view
}

// translateRequestEcho is the backtest.Request → BacktestRequestEcho
// shape projection.
func translateRequestEcho(req backtest.Request) *api.BacktestRequestEcho {
	positions := make([]api.BacktestInitialPosition, 0, len(req.InitialPositions))
	for _, p := range req.InitialPositions {
		positions = append(positions, api.BacktestInitialPosition{
			Symbol:    p.Symbol,
			Quantity:  p.Quantity,
			CostPrice: p.CostPrice,
		})
	}
	echo := &api.BacktestRequestEcho{
		Symbols:            req.Symbols,
		Start:              req.Start,
		End:                req.End,
		InitialCash:        req.InitialCash,
		BaseCurrency:       req.BaseCurrency,
		SlippageBps:        req.SlippageBps,
		CommissionBps:      req.CommissionBps,
		MaxOrdersPerDay:    req.MaxOrdersPerDay,
		InitialPositions:   positions,
		BenchmarkSymbol:    req.BenchmarkSymbol,
		RebalanceFrequency: req.RebalanceFrequency,
	}
	if req.WalkForward != nil {
		echo.WalkForward = &api.WalkForwardInput{
			NumFolds:   req.WalkForward.NumFolds,
			TrainRatio: req.WalkForward.TrainRatio,
			Mode:       string(req.WalkForward.Mode),
		}
	}
	return echo
}

// translateResultView is the backtest.Result → BacktestResultView
// projection. Caps NavCurve / Trades length so a tab-friendly JSON
// response stays under ~100 KB even on multi-year runs.
func translateResultView(r *backtest.Result) *api.BacktestResultView {
	curve := make([]api.BacktestNavPoint, 0, len(r.NavCurve))
	for _, p := range r.NavCurve {
		curve = append(curve, api.BacktestNavPoint{
			Date:          p.Date,
			Nav:           p.Nav,
			Cash:          p.Cash,
			PositionValue: p.PositionValue,
			DrawdownPct:   p.DrawdownPct,
			Positions:     p.Positions,
		})
	}
	trades := make([]api.BacktestTradeEvent, 0, len(r.Trades))
	for _, t := range r.Trades {
		trades = append(trades, api.BacktestTradeEvent{
			Date:       t.Date,
			Symbol:     t.Symbol,
			Action:     t.Action,
			Status:     t.Status,
			Quantity:   t.Quantity,
			FillPrice:  t.FillPrice,
			Notional:   t.Notional,
			Reason:     t.Reason,
			Confidence: t.Confidence,
		})
	}
	bench := make([]api.BacktestBenchmarkPoint, 0, len(r.BenchmarkCurve))
	for _, p := range r.BenchmarkCurve {
		bench = append(bench, api.BacktestBenchmarkPoint{
			Date:  p.Date,
			Close: p.Close,
			Nav:   p.Nav,
			Pct:   p.Pct,
		})
	}
	return &api.BacktestResultView{
		InitialCash: r.InitialCash,
		FinalNav:    r.FinalNav,
		NavCurve:    curve,
		Trades:      trades,
		Metrics: api.BacktestMetricsView{
			CumulativeReturn:          r.Metrics.CumulativeReturn,
			AnnualizedReturn:          r.Metrics.AnnualizedReturn,
			Volatility:                r.Metrics.Volatility,
			SharpeRatio:               r.Metrics.SharpeRatio,
			MaxDrawdown:               r.Metrics.MaxDrawdown,
			WinRate:                   r.Metrics.WinRate,
			TradeCount:                r.Metrics.TradeCount,
			WinningTradeCount:         r.Metrics.WinningTradeCount,
			LosingTradeCount:          r.Metrics.LosingTradeCount,
			BenchmarkCumulativeReturn: r.Metrics.BenchmarkCumulativeReturn,
			ExcessReturn:              r.Metrics.ExcessReturn,
			ExcessMaxDrawdown:         r.Metrics.ExcessMaxDrawdown,
			Alpha:                     r.Metrics.Alpha,
			Beta:                      r.Metrics.Beta,
			TrackingError:             r.Metrics.TrackingError,
			InformationRatio:          r.Metrics.InformationRatio,
		},
		CompletedAt:     r.CompletedAt,
		WalkForward:     translateWalkForwardResult(r.WalkForward),
		BenchmarkSymbol: r.BenchmarkSymbol,
		BenchmarkCurve:  bench,
	}
}

// translateWalkForwardResult flattens the per-fold breakdown into
// the JSON view. nil-safe — runs without the spec return nil.
func translateWalkForwardResult(wf *backtest.WalkForwardResult) *api.WalkForwardResultView {
	if wf == nil {
		return nil
	}
	folds := make([]api.WalkForwardFoldView, 0, len(wf.Folds))
	for _, f := range wf.Folds {
		folds = append(folds, api.WalkForwardFoldView{
			Index:      f.Index,
			TestStart:  f.TestStart,
			TestEnd:    f.TestEnd,
			InitialNav: f.InitialNav,
			FinalNav:   f.FinalNav,
			Return:     f.Return,
			Metrics: api.BacktestMetricsView{
				CumulativeReturn:  f.Metrics.CumulativeReturn,
				AnnualizedReturn:  f.Metrics.AnnualizedReturn,
				Volatility:        f.Metrics.Volatility,
				SharpeRatio:       f.Metrics.SharpeRatio,
				MaxDrawdown:       f.Metrics.MaxDrawdown,
				WinRate:           f.Metrics.WinRate,
				TradeCount:        f.Metrics.TradeCount,
				WinningTradeCount: f.Metrics.WinningTradeCount,
				LosingTradeCount:  f.Metrics.LosingTradeCount,
			},
			TradeCount: f.TradeCount,
			Error:      f.Error,
		})
	}
	return &api.WalkForwardResultView{
		Spec: api.WalkForwardInput{
			NumFolds:   wf.Spec.NumFolds,
			TrainRatio: wf.Spec.TrainRatio,
			Mode:       string(wf.Spec.Mode),
		},
		Mode:            string(wf.Mode),
		Folds:           folds,
		OOSReturn:       wf.OOSReturn,
		OOSSharpe:       wf.OOSSharpe,
		MeanFoldReturn:  wf.MeanFoldReturn,
		WorstFoldReturn: wf.WorstFoldReturn,
		BestFoldReturn:  wf.BestFoldReturn,
		FoldBoundaries:  wf.FoldBoundaries,
	}
}

// -------------------- persistence helpers --------------------

func toNullString(s string) sql.NullString {
	if strings.TrimSpace(s) == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func toNullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t.UTC(), Valid: true}
}

// toNullFloat returns NULL for NaN/Inf so a degenerate metric
// (e.g., Sharpe with zero stdev) doesn't break the DB write.
func toNullFloat(v float64) sql.NullFloat64 {
	if v != v || v > 1.79e308 || v < -1.79e308 {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: v, Valid: true}
}

// translateNavToRepo flattens an in-memory NAV curve into the
// repository's row shape. positions map becomes JSON; missing
// values default to '{}'.
func translateNavToRepo(curve []backtest.NavPoint) []repository.BacktestNavPoint {
	if len(curve) == 0 {
		return nil
	}
	out := make([]repository.BacktestNavPoint, len(curve))
	for i, p := range curve {
		var positions json.RawMessage
		if len(p.Positions) > 0 {
			if b, err := json.Marshal(p.Positions); err == nil {
				positions = b
			}
		}
		out[i] = repository.BacktestNavPoint{
			Seq:           i,
			Date:          p.Date.UTC(),
			Nav:           p.Nav,
			Cash:          p.Cash,
			PositionValue: p.PositionValue,
			DrawdownPct:   p.DrawdownPct,
			Positions:     positions,
		}
	}
	return out
}

func translateTradesToRepo(events []backtest.TradeEvent) []repository.BacktestTradeEvent {
	if len(events) == 0 {
		return nil
	}
	out := make([]repository.BacktestTradeEvent, len(events))
	for i, t := range events {
		out[i] = repository.BacktestTradeEvent{
			Seq:        i,
			Date:       t.Date.UTC(),
			Symbol:     t.Symbol,
			Action:     t.Action,
			Status:     t.Status,
			Quantity:   t.Quantity,
			FillPrice:  t.FillPrice,
			Notional:   t.Notional,
			Reason:     toNullString(t.Reason),
			Confidence: toNullFloat(t.Confidence),
		}
	}
	return out
}

// rowToView projects a persisted job (and optionally its nav +
// trades) into the api.BacktestJob shape the handler returns.
// nav/trades may be nil; in that case only the metrics summary
// is populated.
func rowToView(row repository.BacktestJobRow, nav []repository.BacktestNavPoint, trades []repository.BacktestTradeEvent) *api.BacktestJob {
	view := &api.BacktestJob{
		ID:         row.ID,
		FundID:     row.FundID,
		Name:       row.Name,
		EngineKind: row.EngineKind,
		Status:     row.Status,
		Progress: api.BacktestProgressView{
			TotalDays: row.TotalDays,
			DoneDays:  row.DoneDays,
		},
		SubmittedAt: row.SubmittedAt,
	}
	if row.StartedAt.Valid {
		view.StartedAt = row.StartedAt.Time
	}
	if row.CompletedAt.Valid {
		view.CompletedAt = row.CompletedAt.Time
	}
	if row.Error.Valid {
		view.Error = row.Error.String
	}
	if len(row.Request) > 0 {
		var echo api.BacktestRequestEcho
		// Best-effort decode: if the JSON drifts (schema change
		// across versions) we still want to render the job
		// metadata; the echo just stays empty.
		if err := json.Unmarshal(row.Request, &echo); err == nil {
			view.Request = &echo
		}
	}
	if row.FinalNav.Valid || len(nav) > 0 || len(trades) > 0 {
		view.Result = &api.BacktestResultView{
			InitialCash: row.InitialCash.Float64,
			FinalNav:    row.FinalNav.Float64,
			Metrics: api.BacktestMetricsView{
				CumulativeReturn:  row.CumulativeReturn.Float64,
				AnnualizedReturn:  row.AnnualizedReturn.Float64,
				Volatility:        row.Volatility.Float64,
				SharpeRatio:       row.SharpeRatio.Float64,
				MaxDrawdown:       row.MaxDrawdown.Float64,
				WinRate:           row.WinRate.Float64,
				TradeCount:        row.TradeCount,
				WinningTradeCount: row.WinningTradeCount,
				LosingTradeCount:  row.LosingTradeCount,
			},
			CompletedAt: view.CompletedAt,
		}
		if len(row.WalkForward) > 0 {
			var wf api.WalkForwardResultView
			if err := json.Unmarshal(row.WalkForward, &wf); err == nil {
				view.Result.WalkForward = &wf
			}
		}
		for _, n := range nav {
			var positions map[string]float64
			if len(n.Positions) > 0 {
				_ = json.Unmarshal(n.Positions, &positions)
			}
			view.Result.NavCurve = append(view.Result.NavCurve, api.BacktestNavPoint{
				Date:          n.Date,
				Nav:           n.Nav,
				Cash:          n.Cash,
				PositionValue: n.PositionValue,
				DrawdownPct:   n.DrawdownPct,
				Positions:     positions,
			})
		}
		for _, t := range trades {
			view.Result.Trades = append(view.Result.Trades, api.BacktestTradeEvent{
				Date:       t.Date,
				Symbol:     t.Symbol,
				Action:     t.Action,
				Status:     t.Status,
				Quantity:   t.Quantity,
				FillPrice:  t.FillPrice,
				Notional:   t.Notional,
				Reason:     t.Reason.String,
				Confidence: t.Confidence.Float64,
			})
		}
	}
	return view
}

// backtestEnvOverride lets ops force-disable the endpoint via env
// (`BACKTEST_DISABLED=1`). Useful for hardened deployments where
// historical replays are unwanted compute.
func backtestEnvOverride() bool {
	if v := os.Getenv("BACKTEST_DISABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
		return true
	}
	return false
}

// buildBacktestService is the single seam main.go uses to wire the
// Phase 2E backtest endpoint. It pulls the same OHLC fetcher chain
// the live workflow uses (so backtests see the same data live runs
// see) and the same shared LLM client (when ops elect the "llm"
// engine kind).
//
// Returns nil — which the api layer interprets as "endpoints
// return 503 / empty list" — when BACKTEST_DISABLED=1 is set OR
// the OHLC chain is empty. The rest of the platform is unaffected.
func buildBacktestService(db *sql.DB, runtime *llmRuntime) api.BacktestService {
	if backtestEnvOverride() {
		return nil
	}
	fetcher := buildOHLCFetcherFromEnv()
	if fetcher == nil {
		return nil
	}
	var client llm.LLMClient
	if runtime != nil {
		client = runtime.LLMClient()
	}
	return newBacktestServiceAdapter(db, fetcher, client)
}
