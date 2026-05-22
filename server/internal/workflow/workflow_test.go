package workflow

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testEventBus struct{}

func (testEventBus) Publish(ctx context.Context, event WorkflowEvent) error { return nil }

// recordingEventBus captures every WorkflowEvent published by the orchestrator
// so tests can inspect Snapshot population and progression timing.
type recordingEventBus struct {
	mu     sync.Mutex
	events []WorkflowEvent
}

func (b *recordingEventBus) Publish(_ context.Context, evt WorkflowEvent) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, evt)
	return nil
}

func (b *recordingEventBus) collect() []WorkflowEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]WorkflowEvent, len(b.events))
	copy(out, b.events)
	return out
}

type blockingResearcherPool struct {
	started chan struct{}
}

func (p *blockingResearcherPool) MacroBrief(ctx context.Context, fundID string, tradingDate string) (ResearchReport, error) {
	select {
	case <-p.started:
	default:
		close(p.started)
	}
	<-ctx.Done()
	return ResearchReport{}, ctx.Err()
}

func (p *blockingResearcherPool) RunAll(ctx context.Context, fundID string, tradingDate string) ([]ResearchReport, error) {
	return nil, ctx.Err()
}

func (p *blockingResearcherPool) QuantSignals(ctx context.Context, fundID string, tradingDate string) ([]ResearchReport, error) {
	return nil, ctx.Err()
}

func (p *blockingResearcherPool) Roundtable(ctx context.Context, fundID string, reports []ResearchReport, maxRounds int) (*RoundtableResult, error) {
	return nil, ctx.Err()
}

type stubResearcherPool struct {
	macroBrief func(context.Context, string, string) (ResearchReport, error)
	runAll     func(context.Context, string, string) ([]ResearchReport, error)
	quant      func(context.Context, string, string) ([]ResearchReport, error)
	roundtable func(context.Context, string, []ResearchReport, int) (*RoundtableResult, error)
}

func (p stubResearcherPool) MacroBrief(ctx context.Context, fundID string, tradingDate string) (ResearchReport, error) {
	if p.macroBrief == nil {
		return ResearchReport{}, nil
	}
	return p.macroBrief(ctx, fundID, tradingDate)
}

func (p stubResearcherPool) RunAll(ctx context.Context, fundID string, tradingDate string) ([]ResearchReport, error) {
	if p.runAll == nil {
		return nil, nil
	}
	return p.runAll(ctx, fundID, tradingDate)
}

func (p stubResearcherPool) QuantSignals(ctx context.Context, fundID string, tradingDate string) ([]ResearchReport, error) {
	if p.quant == nil {
		return nil, nil
	}
	return p.quant(ctx, fundID, tradingDate)
}

func (p stubResearcherPool) Roundtable(ctx context.Context, fundID string, reports []ResearchReport, maxRounds int) (*RoundtableResult, error) {
	if p.roundtable == nil {
		return nil, nil
	}
	return p.roundtable(ctx, fundID, reports, maxRounds)
}

type noopPM struct{}

func (noopPM) GeneratePlan(ctx context.Context, fundID string, tradingDate string, roundtable *RoundtableResult) (*InvestmentPlanResult, error) {
	return &InvestmentPlanResult{ID: "plan-1", FundID: fundID}, nil
}

func (noopPM) SubmitForExecution(ctx context.Context, planID string) error { return nil }

type noopApproval struct{}

func (noopApproval) RequestApproval(ctx context.Context, plan *InvestmentPlanResult) error {
	return nil
}
func (noopApproval) WaitForDecision(ctx context.Context, planID string) (bool, error) {
	return true, nil
}

type failingApproval struct {
	requestErr error
	waitErr    error
	approved   bool
}

func (a failingApproval) RequestApproval(ctx context.Context, plan *InvestmentPlanResult) error {
	return a.requestErr
}

func (a failingApproval) WaitForDecision(ctx context.Context, planID string) (bool, error) {
	if a.waitErr != nil {
		return false, a.waitErr
	}
	return a.approved, nil
}

type noopTrading struct{}

func (noopTrading) Execute(ctx context.Context, planID string) error                    { return nil }
func (noopTrading) Settle(ctx context.Context, fundID string, tradingDate string) error { return nil }

type noopMemory struct{}

func (noopMemory) ConsolidateDaily(ctx context.Context, fundID string, state WorkflowState) error {
	return nil
}

func TestRunFullRejectsDuplicateExecution(t *testing.T) {
	pool := &blockingResearcherPool{started: make(chan struct{})}
	orchestrator := NewDailyOrchestrator(
		"fund-1",
		testEventBus{},
		pool,
		noopPM{},
		noopApproval{},
		noopTrading{},
		noopMemory{},
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := orchestrator.RunFull(ctx, "2026-05-10")
		done <- err
	}()

	select {
	case <-pool.started:
	case <-time.After(time.Second):
		t.Fatal("first workflow run did not start")
	}

	_, err := orchestrator.RunFull(context.Background(), "2026-05-10")
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("expected duplicate-run error, got %v", err)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("first workflow run did not exit after cancel")
	}
}

func TestRunFullFailsWhenRoundtableFails(t *testing.T) {
	orchestrator := NewDailyOrchestrator(
		"fund-1",
		testEventBus{},
		stubResearcherPool{
			macroBrief: func(context.Context, string, string) (ResearchReport, error) {
				return ResearchReport{AgentID: "macro-1"}, nil
			},
			roundtable: func(context.Context, string, []ResearchReport, int) (*RoundtableResult, error) {
				return nil, errors.New("roundtable down")
			},
		},
		noopPM{},
		noopApproval{},
		noopTrading{},
		noopMemory{},
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)

	state, err := orchestrator.RunFull(context.Background(), "2026-05-10")
	if err == nil || !strings.Contains(err.Error(), "roundtable failed") {
		t.Fatalf("expected roundtable failure, got %v", err)
	}
	if state == nil {
		t.Fatal("expected workflow state snapshot")
	}
	if state.Status != RunStatusFailed {
		t.Fatalf("expected failed status, got %s", state.Status)
	}
	if result, ok := findStepResult(state, StepRoundtable); !ok || result.Status != "failed" {
		t.Fatalf("expected failed roundtable step result, got %#v", result)
	}
}

func TestRunFullFailsWhenUserApprovalRequestFails(t *testing.T) {
	orchestrator := NewDailyOrchestrator(
		"fund-1",
		testEventBus{},
		stubResearcherPool{
			macroBrief: func(context.Context, string, string) (ResearchReport, error) {
				return ResearchReport{AgentID: "macro-1"}, nil
			},
			runAll: func(context.Context, string, string) ([]ResearchReport, error) {
				return []ResearchReport{{AgentID: "r1"}}, nil
			},
			quant: func(context.Context, string, string) ([]ResearchReport, error) {
				return []ResearchReport{{AgentID: "q1"}}, nil
			},
			roundtable: func(context.Context, string, []ResearchReport, int) (*RoundtableResult, error) {
				return &RoundtableResult{ID: "rt-1", Rounds: 1}, nil
			},
		},
		noopPM{},
		failingApproval{requestErr: errors.New("approval service down")},
		noopTrading{},
		noopMemory{},
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)

	state, err := orchestrator.RunFull(context.Background(), "2026-05-10")
	if err == nil || !strings.Contains(err.Error(), "user approval failed") {
		t.Fatalf("expected fatal approval failure, got %v", err)
	}
	if state == nil {
		t.Fatal("expected workflow state snapshot")
	}
	if state.Status != RunStatusFailed {
		t.Fatalf("expected failed status, got %s", state.Status)
	}

	approval, ok := findStepResult(state, StepUserApproval)
	if !ok || approval.Status != "failed" {
		t.Fatalf("expected failed user approval step, got %#v", approval)
	}
	if _, ok := findStepResult(state, StepTradeExecution); ok {
		t.Fatalf("expected trade execution to not run after fatal approval failure")
	}
}

// TestEmittedEventsCarryStateSnapshot verifies that every WorkflowEvent
// published by the orchestrator carries a non-nil Snapshot reflecting the
// state at the moment the event fired. Persistence subscribers (such as the
// workflow_runs writer) depend on this contract to flush progress to the DB
// even when RunFull is blocked inside WaitForDecision.
func TestEmittedEventsCarryStateSnapshot(t *testing.T) {
	bus := &recordingEventBus{}
	orchestrator := NewDailyOrchestrator(
		"fund-snapshot",
		bus,
		stubResearcherPool{
			macroBrief: func(context.Context, string, string) (ResearchReport, error) {
				return ResearchReport{AgentID: "macro-1"}, nil
			},
			runAll: func(context.Context, string, string) ([]ResearchReport, error) {
				return []ResearchReport{{AgentID: "r1"}}, nil
			},
			quant: func(context.Context, string, string) ([]ResearchReport, error) {
				return []ResearchReport{{AgentID: "q1"}}, nil
			},
			roundtable: func(context.Context, string, []ResearchReport, int) (*RoundtableResult, error) {
				return &RoundtableResult{ID: "rt-1", Rounds: 1}, nil
			},
		},
		noopPM{},
		failingApproval{waitErr: context.DeadlineExceeded},
		noopTrading{},
		noopMemory{},
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)

	_, err := orchestrator.RunFull(context.Background(), "2026-05-10")
	if err != nil {
		t.Fatalf("expected pause-on-timeout run to succeed, got %v", err)
	}

	events := bus.collect()
	if len(events) == 0 {
		t.Fatal("expected at least one event published, got 0")
	}

	for i, evt := range events {
		if evt.Snapshot == nil {
			t.Fatalf("event[%d] type=%s missing Snapshot", i, evt.Type)
		}
		if strings.TrimSpace(evt.Snapshot.RunID) == "" {
			t.Fatalf("event[%d] type=%s snapshot has empty RunID", i, evt.Type)
		}
		if evt.Snapshot.FundID != "fund-snapshot" {
			t.Fatalf("event[%d] type=%s snapshot fundID=%q want fund-snapshot", i, evt.Type, evt.Snapshot.FundID)
		}
	}

	var sawAwaiting bool
	for _, evt := range events {
		if evt.Type != "awaiting_user" {
			continue
		}
		sawAwaiting = true
		if evt.Snapshot.Status != RunStatusPaused {
			t.Fatalf("awaiting_user snapshot status=%s want paused", evt.Snapshot.Status)
		}
		if evt.Snapshot.CurrentStep != StepUserApproval {
			t.Fatalf("awaiting_user snapshot step=%s want user_approval", evt.Snapshot.CurrentStep)
		}
	}
	if !sawAwaiting {
		t.Fatal("expected awaiting_user event in event stream")
	}

	for i, evt := range events[1:] {
		prev := events[i].Snapshot
		curr := evt.Snapshot
		if curr.StartedAt.Before(prev.StartedAt) {
			t.Fatalf("snapshot StartedAt regressed between events %d and %d", i, i+1)
		}
	}
}

func TestRunFullLeavesWorkflowPausedWhenApprovalWaitContextEnds(t *testing.T) {
	orchestrator := NewDailyOrchestrator(
		"fund-1",
		testEventBus{},
		stubResearcherPool{
			macroBrief: func(context.Context, string, string) (ResearchReport, error) {
				return ResearchReport{AgentID: "macro-1"}, nil
			},
			runAll: func(context.Context, string, string) ([]ResearchReport, error) {
				return []ResearchReport{{AgentID: "r1"}}, nil
			},
			quant: func(context.Context, string, string) ([]ResearchReport, error) {
				return []ResearchReport{{AgentID: "q1"}}, nil
			},
			roundtable: func(context.Context, string, []ResearchReport, int) (*RoundtableResult, error) {
				return &RoundtableResult{ID: "rt-1", Rounds: 1}, nil
			},
		},
		noopPM{},
		failingApproval{waitErr: context.DeadlineExceeded},
		noopTrading{},
		noopMemory{},
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)

	state, err := orchestrator.RunFull(context.Background(), "2026-05-10")
	if err != nil {
		t.Fatalf("expected approval wait timeout to leave workflow paused, got %v", err)
	}
	if state == nil {
		t.Fatal("expected workflow state snapshot")
	}
	if state.Status != RunStatusPaused {
		t.Fatalf("expected paused status, got %s", state.Status)
	}
	approval, ok := findStepResult(state, StepUserApproval)
	if !ok || approval.Status != "pending" {
		t.Fatalf("expected pending user approval step, got %#v", approval)
	}
	if _, ok := findStepResult(state, StepTradeExecution); ok {
		t.Fatalf("expected trade execution to not run before approval resume")
	}
}

func TestRunFullMarksRejectedWhenUserRejectsPlan(t *testing.T) {
	orchestrator := NewDailyOrchestrator(
		"fund-1",
		testEventBus{},
		stubResearcherPool{
			macroBrief: func(context.Context, string, string) (ResearchReport, error) {
				return ResearchReport{AgentID: "macro-1"}, nil
			},
			runAll: func(context.Context, string, string) ([]ResearchReport, error) {
				return []ResearchReport{{AgentID: "r1"}}, nil
			},
			quant: func(context.Context, string, string) ([]ResearchReport, error) {
				return []ResearchReport{{AgentID: "q1"}}, nil
			},
			roundtable: func(context.Context, string, []ResearchReport, int) (*RoundtableResult, error) {
				return &RoundtableResult{ID: "rt-1", Rounds: 1}, nil
			},
		},
		noopPM{},
		failingApproval{approved: false},
		noopTrading{},
		noopMemory{},
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)

	state, err := orchestrator.RunFull(context.Background(), "2026-05-10")
	if err != nil {
		t.Fatalf("expected user rejection to be non-fatal, got %v", err)
	}
	if state == nil {
		t.Fatal("expected workflow state snapshot")
	}
	if state.Status != RunStatusRejected {
		t.Fatalf("expected rejected status, got %s", state.Status)
	}

	approval, ok := findStepResult(state, StepUserApproval)
	if !ok || approval.Status != "failed" {
		t.Fatalf("expected failed user approval step, got %#v", approval)
	}
	trade, ok := findStepResult(state, StepTradeExecution)
	if !ok || trade.Status != "skipped" {
		t.Fatalf("expected skipped trade execution step, got %#v", trade)
	}
	settlement, ok := findStepResult(state, StepSettlement)
	if !ok || settlement.Status != "skipped" {
		t.Fatalf("expected skipped settlement step, got %#v", settlement)
	}
	review, ok := findStepResult(state, StepDailyReview)
	if !ok || review.Status != "skipped" {
		t.Fatalf("expected skipped daily review step, got %#v", review)
	}
}

func TestRunFullHonorsConfiguredIntervals(t *testing.T) {
	schedule := DefaultSchedule(nil)
	schedule.MacroBriefTime = "invalid"
	schedule.ResearchParallelTime = "invalid"
	schedule.QuantSignalsTime = "invalid"
	schedule.RoundtableTime = "invalid"
	schedule.PMPlanTime = "invalid"
	schedule.RiskReviewTime = "invalid"
	schedule.UserApprovalTime = "invalid"
	schedule.TradeExecutionTime = "invalid"
	schedule.SettlementTime = "invalid"
	schedule.DailyReviewTime = "invalid"
	schedule.ResearcherInterval = 25 * time.Millisecond
	schedule.PMInterval = 25 * time.Millisecond
	schedule.RiskInterval = 25 * time.Millisecond
	schedule.TraderInterval = 25 * time.Millisecond

	orchestrator := NewDailyOrchestrator(
		"fund-1",
		testEventBus{},
		stubResearcherPool{
			macroBrief: func(context.Context, string, string) (ResearchReport, error) {
				return ResearchReport{AgentID: "macro-1"}, nil
			},
			runAll: func(context.Context, string, string) ([]ResearchReport, error) {
				return []ResearchReport{{AgentID: "r1"}}, nil
			},
			quant: func(context.Context, string, string) ([]ResearchReport, error) {
				return []ResearchReport{{AgentID: "q1"}}, nil
			},
			roundtable: func(context.Context, string, []ResearchReport, int) (*RoundtableResult, error) {
				return &RoundtableResult{ID: "rt-1", Rounds: 1}, nil
			},
		},
		noopPM{},
		noopApproval{},
		noopTrading{},
		noopMemory{},
		WithSchedule(schedule),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)

	start := time.Now()
	state, err := orchestrator.RunFull(context.Background(), "2026-05-10")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected run to succeed, got %v", err)
	}
	if state == nil {
		t.Fatal("expected workflow state snapshot")
	}
	if elapsed < 110*time.Millisecond {
		t.Fatalf("expected configured intervals to delay execution, got elapsed %v", elapsed)
	}
	if state.Status != RunStatusCompleted {
		t.Fatalf("expected completed status, got %s", state.Status)
	}
	if result, ok := findStepResult(state, StepTradeExecution); !ok || result.Status != "success" {
		t.Fatalf("expected successful trade execution step, got %#v", result)
	}
}

func TestRunFullForceImmediateSkipsScheduleWaits(t *testing.T) {
	schedule := DefaultSchedule(nil)
	schedule.MacroBriefTime = "23:59"
	schedule.ResearchParallelTime = "23:59"
	schedule.QuantSignalsTime = "23:59"
	schedule.RoundtableTime = "23:59"
	schedule.PMPlanTime = "23:59"
	schedule.RiskReviewTime = "23:59"
	schedule.UserApprovalTime = "23:59"
	schedule.TradeExecutionTime = "23:59"
	schedule.SettlementTime = "23:59"
	schedule.DailyReviewTime = "23:59"
	schedule.ForceImmediate = true

	orchestrator := NewDailyOrchestrator(
		"fund-1",
		testEventBus{},
		stubResearcherPool{
			macroBrief: func(context.Context, string, string) (ResearchReport, error) { return ResearchReport{AgentID: "macro-1"}, nil },
			runAll: func(context.Context, string, string) ([]ResearchReport, error) { return []ResearchReport{{AgentID: "r1"}}, nil },
			quant: func(context.Context, string, string) ([]ResearchReport, error) { return []ResearchReport{{AgentID: "q1"}}, nil },
			roundtable: func(context.Context, string, []ResearchReport, int) (*RoundtableResult, error) { return &RoundtableResult{ID: "rt-1", Rounds: 1}, nil },
		},
		noopPM{},
		noopApproval{},
		noopTrading{},
		noopMemory{},
		WithSchedule(schedule),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)

	start := time.Now()
	state, err := orchestrator.RunFull(context.Background(), time.Now().UTC().Format("2006-01-02"))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected run to succeed, got %v", err)
	}
	if state == nil || state.Status != RunStatusCompleted {
		t.Fatalf("expected completed state, got %#v", state)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("expected force immediate execution, got elapsed %v", elapsed)
	}
	if result, ok := findStepResult(state, StepMacroBrief); !ok || result.Status != "success" {
		t.Fatalf("expected macro brief to run immediately, got %#v", result)
	}
}

func TestSupportsManualTrigger(t *testing.T) {
	if !SupportsManualTrigger(StepMacroBrief) {
		t.Fatal("expected macro brief to support manual trigger")
	}
	if SupportsManualTrigger(StepRoundtable) {
		t.Fatal("expected roundtable to reject manual trigger")
	}
	steps := SupportedManualTriggerStepNames()
	if len(steps) == 0 || steps[0] != "macro_brief" {
		t.Fatalf("unexpected manual trigger steps: %#v", steps)
	}
}

func TestResumeApprovedPlanContinuesPostApprovalSteps(t *testing.T) {
	orchestrator := NewDailyOrchestrator(
		"fund-1",
		testEventBus{},
		stubResearcherPool{},
		noopPM{},
		noopApproval{},
		noopTrading{},
		noopMemory{},
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	orchestrator.RestoreState(WorkflowState{
		RunID:       "run-1",
		FundID:      "fund-1",
		TradingDate: "2026-05-10",
		Status:      RunStatusPaused,
		CurrentStep: StepUserApproval,
		StepResults: []StepResult{{Step: StepUserApproval, Status: "pending", StartedAt: time.Now(), EndedAt: time.Now()}},
		PlanID:      "plan-1",
		StartedAt:   time.Now(),
	})

	state, err := orchestrator.ResumeApprovedPlan(context.Background(), "2026-05-10", "plan-1")
	if err != nil {
		t.Fatalf("resume approved plan: %v", err)
	}
	if state == nil {
		t.Fatal("expected workflow state snapshot")
	}
	if state.Status != RunStatusCompleted {
		t.Fatalf("expected completed status after resume, got %s", state.Status)
	}
	approval, ok := findStepResult(state, StepUserApproval)
	if !ok || approval.Status != "success" {
		t.Fatalf("expected successful user approval after resume, got %#v", approval)
	}
	if got := countStepResults(state, StepUserApproval); got != 1 {
		t.Fatalf("expected exactly one user approval step result after resume, got %d", got)
	}
	trade, ok := findStepResult(state, StepTradeExecution)
	if !ok || trade.Status != "success" {
		t.Fatalf("expected successful trade execution after resume, got %#v", trade)
	}
	settlement, ok := findStepResult(state, StepSettlement)
	if !ok || settlement.Status != "success" {
		t.Fatalf("expected successful settlement after resume, got %#v", settlement)
	}
	review, ok := findStepResult(state, StepDailyReview)
	if !ok || review.Status != "success" {
		t.Fatalf("expected successful daily review after resume, got %#v", review)
	}
}

func TestWaitForIntervalRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForInterval(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
}

func TestStepTimeoutValues(t *testing.T) {
	if got := stepTimeout(StepMacroBrief); got != 2*time.Minute {
		t.Fatalf("expected macro brief timeout 2m, got %v", got)
	}
	if got := stepTimeout(StepRoundtable); got != 5*time.Minute {
		t.Fatalf("expected roundtable timeout 5m, got %v", got)
	}
	if got := stepTimeout(StepUserApproval); got != 0 {
		t.Fatalf("expected user approval timeout 0, got %v", got)
	}
}

func TestRoundtableConfigConcurrencyDefaults(t *testing.T) {
	if got := (RoundtableConfig{}).maxConcurrency(); got != defaultMaxConcurrency {
		t.Fatalf("expected default max concurrency %d, got %d", defaultMaxConcurrency, got)
	}
	if got := (RoundtableConfig{MaxConcurrency: 2}).maxConcurrency(); got != 2 {
		t.Fatalf("expected configured max concurrency 2, got %d", got)
	}
}

func TestCollectOpinionsFailsWhenAllResearchersFail(t *testing.T) {
	engine, err := NewRoundtableEngine([]ResearcherAgent{
		failingResearcher{id: "r1"},
	}, noopSummarizer{}, RoundtableConfig{MaxConcurrency: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	_, err = engine.collectOpinions(context.Background(), "rt-1", 1, []string{"AAPL"}, nil)
	if err == nil {
		t.Fatal("expected collectOpinions to fail when all researchers fail")
	}
}

func TestCollectOpinionsRespectsMaxConcurrency(t *testing.T) {
	probe := &concurrencyProbe{}
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	engine, err := NewRoundtableEngine([]ResearcherAgent{
		countingResearcher{id: "r1", probe: probe, entered: entered, release: release},
		countingResearcher{id: "r2", probe: probe, entered: entered, release: release},
	}, noopSummarizer{}, RoundtableConfig{MaxConcurrency: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	go func() {
		<-entered
		<-entered
		close(release)
	}()

	opinions, err := engine.collectOpinions(context.Background(), "rt-1", 1, []string{"AAPL", "MSFT"}, nil)
	if err != nil {
		t.Fatalf("collect opinions: %v", err)
	}
	if got := len(opinions); got != 4 {
		t.Fatalf("expected 4 opinions, got %d", got)
	}
	if got := atomic.LoadInt32(&probe.max); got > 2 {
		t.Fatalf("expected max concurrency <= 2, got %d", got)
	}
}

func TestExecuteRoundMarksTimeout(t *testing.T) {
	engine, err := NewRoundtableEngine([]ResearcherAgent{
		blockingAgentResearcher{id: "r1"},
	}, noopSummarizer{}, RoundtableConfig{MaxConcurrency: 1, RoundTimeout: 10 * time.Millisecond}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	rt := &Roundtable{ID: "rt-1"}
	_, err = engine.executeRound(context.Background(), rt, 1, []string{"AAPL"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	if rt.Status != "timeout" {
		t.Fatalf("expected timeout status, got %q", rt.Status)
	}
}

type failingResearcher struct{ id string }

func (r failingResearcher) AgentID() string   { return r.id }
func (r failingResearcher) AgentName() string { return r.id }
func (r failingResearcher) Focus() string     { return "stock" }
func (r failingResearcher) ProduceOpinion(ctx context.Context, topic string, previousRound *RoundtableRound) (ResearcherOpinion, error) {
	return ResearcherOpinion{}, errors.New("boom")
}

type blockingAgentResearcher struct{ id string }

func (r blockingAgentResearcher) AgentID() string   { return r.id }
func (r blockingAgentResearcher) AgentName() string { return r.id }
func (r blockingAgentResearcher) Focus() string     { return "stock" }
func (r blockingAgentResearcher) ProduceOpinion(ctx context.Context, topic string, previousRound *RoundtableRound) (ResearcherOpinion, error) {
	<-ctx.Done()
	return ResearcherOpinion{}, ctx.Err()
}

type concurrencyProbe struct {
	current int32
	max     int32
}

type countingResearcher struct {
	id      string
	probe   *concurrencyProbe
	entered chan<- struct{}
	release <-chan struct{}
}

func (r countingResearcher) AgentID() string   { return r.id }
func (r countingResearcher) AgentName() string { return r.id }
func (r countingResearcher) Focus() string     { return "stock" }
func (r countingResearcher) ProduceOpinion(ctx context.Context, topic string, previousRound *RoundtableRound) (ResearcherOpinion, error) {
	current := atomic.AddInt32(&r.probe.current, 1)
	defer atomic.AddInt32(&r.probe.current, -1)
	for {
		max := atomic.LoadInt32(&r.probe.max)
		if current <= max {
			break
		}
		if atomic.CompareAndSwapInt32(&r.probe.max, max, current) {
			break
		}
	}
	if r.entered != nil {
		r.entered <- struct{}{}
	}
	if r.release != nil {
		select {
		case <-ctx.Done():
			return ResearcherOpinion{}, ctx.Err()
		case <-r.release:
		}
	}
	return ResearcherOpinion{AgentID: r.id, AgentName: r.id, Focus: r.Focus(), Symbol: topic, Direction: "bullish", Confidence: 75}, nil
}

type noopSummarizer struct{}

func (noopSummarizer) SummarizeRound(ctx context.Context, round RoundtableRound) (string, []string, error) {
	return "", nil, nil
}
func (noopSummarizer) ExtractConsensus(ctx context.Context, allRounds []RoundtableRound) ([]ConsensusItem, error) {
	return nil, nil
}

func BenchmarkRoundtableCollectOpinions(b *testing.B) {
	engine, err := NewRoundtableEngine([]ResearcherAgent{
		benchmarkResearcher{id: "r1"},
		benchmarkResearcher{id: "r2"},
		benchmarkResearcher{id: "r3"},
	}, noopSummarizer{}, RoundtableConfig{MaxConcurrency: 3}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		b.Fatalf("create engine: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		opinions, err := engine.collectOpinions(context.Background(), "rt-bench", 1, []string{"AAPL", "MSFT", "TSLA"}, nil)
		if err != nil {
			b.Fatalf("collect opinions: %v", err)
		}
		if len(opinions) == 0 {
			b.Fatal("expected opinions")
		}
	}
}

type benchmarkResearcher struct{ id string }

func (r benchmarkResearcher) AgentID() string   { return r.id }
func (r benchmarkResearcher) AgentName() string { return r.id }
func (r benchmarkResearcher) Focus() string     { return "stock" }
func (r benchmarkResearcher) ProduceOpinion(ctx context.Context, topic string, previousRound *RoundtableRound) (ResearcherOpinion, error) {
	return ResearcherOpinion{AgentID: r.id, AgentName: r.id, Focus: r.Focus(), Symbol: topic, Direction: "bullish", Confidence: 80}, nil
}

func findStepResult(state *WorkflowState, step WorkflowStep) (StepResult, bool) {
	for _, result := range state.StepResults {
		if result.Step == step {
			return result, true
		}
	}
	return StepResult{}, false
}

func countStepResults(state *WorkflowState, step WorkflowStep) int {
	count := 0
	for _, result := range state.StepResults {
		if result.Step == step {
			count++
		}
	}
	return count
}
