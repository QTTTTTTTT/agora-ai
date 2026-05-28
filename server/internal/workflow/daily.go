package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Domain value types (mirrors domain package — kept local to avoid imports)
// ---------------------------------------------------------------------------

type AgentRole string

const (
	RolePM         AgentRole = "pm"
	RoleResearcher AgentRole = "researcher"
	RoleTrader     AgentRole = "trader"
	RoleRisk       AgentRole = "risk"
)

type ResearchFocus string

const (
	FocusStock       ResearchFocus = "stock"
	FocusFundamental ResearchFocus = "fundamental"
	FocusMacro       ResearchFocus = "macro"
)

type PlanWorkflowStatus string

const (
	PlanStatusDraft       PlanWorkflowStatus = "draft"
	PlanStatusRiskReview  PlanWorkflowStatus = "risk_review"
	PlanStatusPendingUser PlanWorkflowStatus = "pending_user"
	PlanStatusApproved    PlanWorkflowStatus = "approved"
	PlanStatusRejected    PlanWorkflowStatus = "rejected"
	PlanStatusExecuting   PlanWorkflowStatus = "executing"
	PlanStatusCompleted   PlanWorkflowStatus = "completed"
	// Sprint 3 / L2: 部分成交但有 action 失败。比"completed"更
	// 诚实（不掩盖失败），又比"rejected"更准确（毕竟有 fill 发生）。
	PlanStatusMixed PlanWorkflowStatus = "mixed"
)

// ---------------------------------------------------------------------------
// Workflow step definitions
// ---------------------------------------------------------------------------

// WorkflowStep enumerates each discrete phase in the daily workflow.
type WorkflowStep int

const (
	StepMacroBrief       WorkflowStep = iota // 09:00
	StepResearchParallel                     // 09:30
	StepQuantSignals                         // 10:00
	StepRoundtable                           // 10:30
	StepPMPlan                               // 11:00
	StepRiskReview                           // 11:10
	StepUserApproval                         // 11:30
	StepTradeExecution                       // after user approval
	StepSettlement                           // 15:00
	StepDailyReview                          // 15:30
)

var stepNames = map[WorkflowStep]string{
	StepMacroBrief:       "macro_brief",
	StepResearchParallel: "research_parallel",
	StepQuantSignals:     "quant_signals",
	StepRoundtable:       "roundtable",
	StepPMPlan:           "pm_plan",
	StepRiskReview:       "risk_review",
	StepUserApproval:     "user_approval",
	StepTradeExecution:   "trade_execution",
	StepSettlement:       "settlement",
	StepDailyReview:      "daily_review",
}

var manualTriggerSteps = []WorkflowStep{
	StepMacroBrief,
	StepResearchParallel,
	StepQuantSignals,
	StepSettlement,
	StepDailyReview,
}

func (s WorkflowStep) String() string {
	if n, ok := stepNames[s]; ok {
		return n
	}
	return fmt.Sprintf("unknown_step(%d)", int(s))
}

func SupportsManualTrigger(step WorkflowStep) bool {
	for _, candidate := range manualTriggerSteps {
		if candidate == step {
			return true
		}
	}
	return false
}

func SupportedManualTriggerStepNames() []string {
	result := make([]string, 0, len(manualTriggerSteps))
	for _, step := range manualTriggerSteps {
		result = append(result, step.String())
	}
	return result
}

// ---------------------------------------------------------------------------
// WorkflowState — tracks live progress of a single daily run
// ---------------------------------------------------------------------------

// RunStatus describes the overall status of a daily workflow run.
type RunStatus string

const (
	RunStatusPending   RunStatus = "pending"
	RunStatusRunning   RunStatus = "running"
	RunStatusPaused    RunStatus = "paused"
	RunStatusCompleted RunStatus = "completed"
	RunStatusCancelled RunStatus = "cancelled"
	RunStatusFailed    RunStatus = "failed"
	RunStatusRejected  RunStatus = "rejected"
)

var (
	errPlanRejected     = errors.New("plan rejected")
	errAwaitingApproval = errors.New("awaiting user approval")

	// ErrLLMBudgetExceeded is a workflow-layer sentinel surfaced when a
	// step's LLM calls trip the dollar-budget gate (F14). The LLM
	// runtime wraps subscription.ErrLLMBudgetExceeded to also satisfy
	// this sentinel so the orchestrator can pause the run with an
	// explicit reason without importing the subscription package.
	ErrLLMBudgetExceeded = errors.New("workflow: llm dollar budget exceeded")
)

// StepResult captures the outcome of an individual step.
type StepResult struct {
	Step      WorkflowStep
	Status    string // "success", "skipped", "failed"
	Error     error
	StartedAt time.Time
	EndedAt   time.Time
}

// WorkflowState holds the mutable state for one daily run.
type WorkflowState struct {
	mu *sync.RWMutex

	RunID       string
	FundID      string
	TradingDate string // YYYY-MM-DD
	Status      RunStatus
	CurrentStep WorkflowStep
	StepResults []StepResult
	PlanID      string // set after PM generates a plan
	StartedAt   time.Time
	EndedAt     time.Time
}

func newWorkflowState(fundID, tradingDate string) *WorkflowState {
	return &WorkflowState{
		mu:          &sync.RWMutex{},
		RunID:       fmt.Sprintf("run_%s_%s_%d", fundID, tradingDate, time.Now().UnixMilli()),
		FundID:      fundID,
		TradingDate: tradingDate,
		Status:      RunStatusPending,
		StepResults: make([]StepResult, 0, 10),
		StartedAt:   time.Now(),
	}
}

func (ws *WorkflowState) recordStep(sr StepResult) {
	mu := ws.lock()
	mu.Lock()
	defer mu.Unlock()
	for i := range ws.StepResults {
		if ws.StepResults[i].Step == sr.Step {
			ws.StepResults[i] = sr
			return
		}
	}
	ws.StepResults = append(ws.StepResults, sr)
}

func (ws *WorkflowState) setStatus(s RunStatus) {
	mu := ws.lock()
	mu.Lock()
	defer mu.Unlock()
	ws.Status = s
}

func (ws *WorkflowState) setCurrent(step WorkflowStep) {
	mu := ws.lock()
	mu.Lock()
	defer mu.Unlock()
	ws.CurrentStep = step
}

// Snapshot returns a read-only copy safe for external consumption.
func (ws *WorkflowState) Snapshot() WorkflowState {
	mu := ws.lock()
	mu.RLock()
	defer mu.RUnlock()
	cp := *ws
	cp.mu = &sync.RWMutex{}
	cp.StepResults = make([]StepResult, len(ws.StepResults))
	copy(cp.StepResults, ws.StepResults)
	return cp
}

func (ws *WorkflowState) lock() *sync.RWMutex {
	if ws.mu == nil {
		ws.mu = &sync.RWMutex{}
	}
	return ws.mu
}

// ---------------------------------------------------------------------------
// Event types emitted through the EventBus
// ---------------------------------------------------------------------------

// WorkflowEvent is published at every meaningful transition.
type WorkflowEvent struct {
	Type        string // e.g. "step_started", "step_completed", "run_completed"
	RunID       string
	FundID      string
	Step        WorkflowStep
	TradingDate string
	Payload     interface{} // step-specific data
	Timestamp   time.Time
	Error       error
	// Snapshot is a deep copy of the orchestrator state at the moment the
	// event was emitted. Subscribers that need to persist progress (e.g. the
	// workflow_runs table) can read it directly without racing against the
	// live state. May be nil if the orchestrator does not yet have a state
	// (extremely rare; tests construct events without one).
	Snapshot *WorkflowState
}

// ---------------------------------------------------------------------------
// Dependency interfaces (satisfied by other packages at wire time)
// ---------------------------------------------------------------------------

// EventBus publishes workflow lifecycle events to subscribers.
type EventBus interface {
	Publish(ctx context.Context, event WorkflowEvent) error
}

// ResearchReport is the output produced by a single researcher run.
type ResearchReport struct {
	AgentID string
	Focus   ResearchFocus
	Content string
}

// ResearcherPool manages the set of researcher agents for a fund.
type ResearcherPool interface {
	// MacroBrief asks the macro researcher to produce a morning brief.
	MacroBrief(ctx context.Context, fundID string, tradingDate string) (ResearchReport, error)
	// RunAll kicks off every researcher in parallel and collects reports.
	RunAll(ctx context.Context, fundID string, tradingDate string) ([]ResearchReport, error)
	// QuantSignals asks stock-focused researchers for technical signals.
	QuantSignals(ctx context.Context, fundID string, tradingDate string) ([]ResearchReport, error)
	// Roundtable orchestrates a multi-round debate among researchers.
	// maxRounds caps the number of back-and-forth rounds.
	Roundtable(ctx context.Context, fundID string, reports []ResearchReport, maxRounds int) (*RoundtableResult, error)
}

// RoundtableResult mirrors the domain Roundtable but stays local.
type RoundtableResult struct {
	ID        string
	Rounds    int
	Consensus []string // simplified consensus items
	// Phase 2B (debate roundtable) optional fields. Empty when the
	// legacy text-concat path produced this result, populated when
	// the multi-agent bull/bear/quant debate ran.
	OverallStance string
	BullCase      string
	BearCase      string
	QuantCase     string
	Symbols       []RoundtableSymbolVerdict
	// Phase 2D enrichment. Empty when the corresponding fetcher
	// isn't wired or the provider returned no data. The PMAgent
	// forwards these into DecisionInput so the LLM PM sees the
	// same fundamentals / sector flow / news sentiment that drove
	// the debate. Preserving them in RoundtableResult avoids
	// double-fetching across the debate → PM hand-off.
	FundamentalSummary string
	SectorRotation     string
	NewsSentiment      string
	// Sprint 1 / S2 — macro briefing carry-through. The macro
	// researcher (StepMacroBrief, day's first step) returns a
	// ResearchReport.Content the orchestrator forwards into the
	// roundtable / PM pipeline. Before this field was added it was
	// dropped on the floor: DecisionInput had a MacroBriefing slot
	// (and the PM system prompt expected it) but the wiring layer
	// never populated it. The orchestrator now copies the macro
	// report into this field after StepMacroBrief succeeds; the
	// PMAgent forwards it into DecisionInput.MacroBriefing.
	//
	// Empty when the macro step failed or no macro researcher exists
	// on the fund. The downstream decision prompt's "macroBriefing
	// is optional" rule already covers that case.
	MacroBriefing string
}

// RoundtableSymbolVerdict carries the per-symbol majority verdict
// the Phase 2B debate produced. Verdict is one of
// "bull"/"bear"/"neutral"; DissentVotes counts agents that voted
// against Verdict (0..2). Bull/Bear/QuantCase are the keyPoint
// rationales each role attached to this symbol — the LLMDecisionEngine
// (Phase 2A) consumes them so the PM can see both sides.
type RoundtableSymbolVerdict struct {
	Symbol       string
	Verdict      string
	BullCase     string
	BearCase     string
	QuantCase    string
	DissentVotes int
}

// InvestmentPlanResult mirrors the domain InvestmentPlan.
type InvestmentPlanResult struct {
	ID           string
	FundID       string
	Status       PlanWorkflowStatus
	RoundtableID string
}

// PMAgent generates an investment plan from roundtable consensus.
type PMAgent interface {
	GeneratePlan(ctx context.Context, fundID string, tradingDate string, roundtable *RoundtableResult) (*InvestmentPlanResult, error)
	SubmitForExecution(ctx context.Context, planID string) error
}

// RiskAgent reviews a plan and may flag violations.
type RiskAgent interface {
	ReviewPlan(ctx context.Context, plan *InvestmentPlanResult) (approved bool, remarks string, err error)
}

// UserApprovalGateway handles the async user-approval handshake.
type UserApprovalGateway interface {
	// RequestApproval puts the plan into pending_user state and notifies.
	RequestApproval(ctx context.Context, plan *InvestmentPlanResult) error
	// WaitForDecision blocks until the user approves/rejects or the ctx is
	// cancelled. Returns true when approved.
	WaitForDecision(ctx context.Context, planID string) (approved bool, err error)
}

// TradingEngine executes approved plans in the simulation.
type TradingEngine interface {
	Execute(ctx context.Context, planID string) error
	// Settle runs end-of-day T+1 settlement and NAV calculation.
	Settle(ctx context.Context, fundID string, tradingDate string) error
}

// PlanLifecycleNotifier is an OPTIONAL hook the orchestrator fires on
// terminal plan transitions (completed / mixed / failed) so a push
// fan-out (FCM) layer can notify the fund's owners. Implementations
// must be non-blocking — the orchestrator runs the hook synchronously
// at the end of the trading-execution step and can't afford a 10s
// FCM call delaying NAV settlement.
//
// Sprint 4 / android-core. Soft-typed (any orchestrator without an
// injected notifier just no-ops) — preserves test stubs.
type PlanLifecycleNotifier interface {
	NotifyPlanLifecycle(ctx context.Context, fundID, planID string, status PlanWorkflowStatus)
}

// PartialFillReporter is an OPTIONAL interface a TradingEngine may
// implement to surface partial-fill outcomes. When implemented and the
// last Execute call had at least one successful and at least one failed
// plan_action, ReportPartialFill returns true; the orchestrator then
// stamps PlanStatusMixed instead of PlanStatusCompleted.
//
// Decoupled into its own interface so:
//   - Existing test stubs (noopTrading) keep compiling without changes.
//   - Engines that don't track per-action status can opt out by simply
//     not implementing this method.
type PartialFillReporter interface {
	ReportPartialFill(ctx context.Context, planID string) (partial bool, err error)
}

// MemorySystem persists daily learnings and performance data.
type MemorySystem interface {
	ConsolidateDaily(ctx context.Context, fundID string, state WorkflowState) error
}

// ---------------------------------------------------------------------------
// Schedule configuration
// ---------------------------------------------------------------------------

// ScheduleConfig defines the wall-clock times for each step.
// Times are expressed as hour:minute in the fund's local timezone.
type ScheduleConfig struct {
	MacroBriefTime       string        // "09:00"
	ResearchParallelTime string        // "09:30"
	QuantSignalsTime     string        // "10:00"
	RoundtableTime       string        // "10:30"
	PMPlanTime           string        // "11:00"
	RiskReviewTime       string        // "11:10"
	UserApprovalTime     string        // "11:30"
	TradeExecutionTime   string        // "11:45"
	SettlementTime       string        // "15:00"
	DailyReviewTime      string        // "15:30"
	ResearcherInterval   time.Duration // cadence before researcher-owned steps during an active run
	PMInterval           time.Duration // cadence before PM planning during an active run
	RiskInterval         time.Duration // cadence before risk review during an active run
	TraderInterval       time.Duration // cadence before trade execution during an active run
	MaxRoundtableRounds  int           // default 3
	Location             *time.Location
	ForceImmediate       bool
}

// DefaultSchedule returns the standard PRD §4.2.1 schedule in the given tz.
func DefaultSchedule(loc *time.Location) ScheduleConfig {
	if loc == nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	return ScheduleConfig{
		MacroBriefTime:       "09:00",
		ResearchParallelTime: "09:30",
		QuantSignalsTime:     "10:00",
		RoundtableTime:       "10:30",
		PMPlanTime:           "11:00",
		RiskReviewTime:       "11:10",
		UserApprovalTime:     "11:30",
		TradeExecutionTime:   "11:45",
		SettlementTime:       "15:00",
		DailyReviewTime:      "15:30",
		MaxRoundtableRounds:  3,
		Location:             loc,
	}
}

func normalizeScheduleConfig(s ScheduleConfig) ScheduleConfig {
	base := DefaultSchedule(s.Location)
	if s.MacroBriefTime != "" {
		base.MacroBriefTime = s.MacroBriefTime
	}
	if s.ResearchParallelTime != "" {
		base.ResearchParallelTime = s.ResearchParallelTime
	}
	if s.QuantSignalsTime != "" {
		base.QuantSignalsTime = s.QuantSignalsTime
	}
	if s.RoundtableTime != "" {
		base.RoundtableTime = s.RoundtableTime
	}
	if s.PMPlanTime != "" {
		base.PMPlanTime = s.PMPlanTime
	}
	if s.RiskReviewTime != "" {
		base.RiskReviewTime = s.RiskReviewTime
	}
	if s.UserApprovalTime != "" {
		base.UserApprovalTime = s.UserApprovalTime
	}
	if s.TradeExecutionTime != "" {
		base.TradeExecutionTime = s.TradeExecutionTime
	}
	if s.SettlementTime != "" {
		base.SettlementTime = s.SettlementTime
	}
	if s.DailyReviewTime != "" {
		base.DailyReviewTime = s.DailyReviewTime
	}
	if s.MaxRoundtableRounds > 0 {
		base.MaxRoundtableRounds = s.MaxRoundtableRounds
	}
	base.ResearcherInterval = s.ResearcherInterval
	base.PMInterval = s.PMInterval
	base.RiskInterval = s.RiskInterval
	base.TraderInterval = s.TraderInterval
	base.ForceImmediate = s.ForceImmediate
	return base
}

// ---------------------------------------------------------------------------
// DailyOrchestrator
// ---------------------------------------------------------------------------

// DailyOrchestrator drives the full daily workflow for a single fund.
type DailyOrchestrator struct {
	fundID   string
	schedule ScheduleConfig
	logger   *slog.Logger

	// Dependencies — all required unless noted.
	eventBus     EventBus
	researchers  ResearcherPool
	pm           PMAgent
	risk         RiskAgent // optional — nil means skip risk review
	approval     UserApprovalGateway
	trading      TradingEngine
	memory       MemorySystem
	planNotifier PlanLifecycleNotifier // optional — push fan-out hook

	// Internal bookkeeping.
	mu       sync.Mutex
	state    *WorkflowState
	cancelFn context.CancelFunc // for stopping a running workflow
	running  bool

	// retryRNG is consumed only by runWithRetry's jitter computation.
	// Per-orchestrator instance so concurrent runs don't contend on the
	// global rand source, and so tests can inject a seeded RNG via
	// WithRetryRNG for deterministic backoff assertions.
	retryRNG *rand.Rand
}

// OrchestratorOption applies optional configuration.
type OrchestratorOption func(*DailyOrchestrator)

// WithRiskAgent attaches an optional risk agent.
func WithRiskAgent(r RiskAgent) OrchestratorOption {
	return func(o *DailyOrchestrator) { o.risk = r }
}

// WithLogger overrides the default logger.
func WithLogger(l *slog.Logger) OrchestratorOption {
	return func(o *DailyOrchestrator) { o.logger = l }
}

// WithSchedule overrides the default schedule.
func WithSchedule(s ScheduleConfig) OrchestratorOption {
	return func(o *DailyOrchestrator) { o.schedule = normalizeScheduleConfig(s) }
}

// WithRetryRNG injects a deterministic random source for the retry
// backoff jitter. Production callers leave this nil and get the default
// time-seeded RNG initialized lazily.
func WithRetryRNG(r *rand.Rand) OrchestratorOption {
	return func(o *DailyOrchestrator) { o.retryRNG = r }
}

// WithPlanLifecycleNotifier wires the optional push fan-out hook
// (Sprint 4 / android-core). Implementations should be non-blocking;
// the orchestrator calls them on the trading-execution-completion path.
func WithPlanLifecycleNotifier(n PlanLifecycleNotifier) OrchestratorOption {
	return func(o *DailyOrchestrator) { o.planNotifier = n }
}

// notifyPlan fires the optional PlanLifecycleNotifier in a defensive
// guarded way: panics from the hook are swallowed (with a log) so a
// broken push provider can never tank a trading workflow run.
func (o *DailyOrchestrator) notifyPlan(ctx context.Context, plan *InvestmentPlanResult) {
	if o == nil || o.planNotifier == nil || plan == nil {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			if o.logger != nil {
				o.logger.Warn("plan lifecycle notifier panicked",
					"plan_id", plan.ID, "fund_id", plan.FundID, "panic", rec)
			}
		}
	}()
	o.planNotifier.NotifyPlanLifecycle(ctx, plan.FundID, plan.ID, plan.Status)
}

// NewDailyOrchestrator creates an orchestrator for the given fund.
func NewDailyOrchestrator(
	fundID string,
	bus EventBus,
	researchers ResearcherPool,
	pm PMAgent,
	approval UserApprovalGateway,
	trading TradingEngine,
	memory MemorySystem,
	opts ...OrchestratorOption,
) *DailyOrchestrator {
	o := &DailyOrchestrator{
		fundID:      fundID,
		schedule:    DefaultSchedule(nil),
		logger:      slog.Default(),
		eventBus:    bus,
		researchers: researchers,
		pm:          pm,
		approval:    approval,
		trading:     trading,
		memory:      memory,
	}
	for _, opt := range opts {
		opt(o)
	}
	if o.retryRNG == nil {
		// Lazy-init the retry RNG. Time-seeded is fine since the jitter
		// only protects against thundering-herd, not security.
		o.retryRNG = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return o
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// RunFull executes the complete daily workflow synchronously.
// It blocks until all steps complete, the context is cancelled, or a fatal
// error occurs. Non-fatal step failures are logged and skipped.
func (o *DailyOrchestrator) RunFull(ctx context.Context, tradingDate string) (*WorkflowState, error) {
	ctx, cancel := context.WithCancel(ctx)

	o.mu.Lock()
	if o.running {
		o.mu.Unlock()
		cancel()
		return stateSnapshot(o.state), fmt.Errorf("workflow already running for fund %s", o.fundID)
	}
	o.running = true
	o.cancelFn = cancel
	o.state = newWorkflowState(o.fundID, tradingDate)
	schedule := o.schedule
	o.mu.Unlock()

	defer func() {
		cancel()
		o.mu.Lock()
		o.running = false
		o.cancelFn = nil
		o.mu.Unlock()
	}()

	o.state.setStatus(RunStatusRunning)
	o.emit(ctx, "run_started", StepMacroBrief, nil, nil)
	o.logger.Info("daily workflow started", "run_id", o.state.RunID, "fund", o.fundID, "date", tradingDate)

	// Accumulate research artefacts across steps.
	var (
		allReports []ResearchReport
		roundtable *RoundtableResult
		plan       *InvestmentPlanResult
	)

	// Step 1 — 09:00 Macro brief
	if err := waitForScheduledOrInterval(ctx, tradingDate, schedule.Location, schedule.MacroBriefTime, schedule.ResearcherInterval, schedule.ForceImmediate); err != nil {
		return stateSnapshot(o.state), err
	}
	var macroReportContent string
	macroBrief, err := o.runStep(ctx, StepMacroBrief, func(ctx context.Context) error {
		report, e := o.researchers.MacroBrief(ctx, o.fundID, tradingDate)
		if e != nil {
			return e
		}
		allReports = append(allReports, report)
		// Sprint 1 / S2: preserve the raw macro brief so we can
		// stitch it onto the RoundtableResult after the roundtable
		// step. Without this hand-off the LLMDecisionEngine never
		// sees the macro analysis even though the prompt has a
		// dedicated slot for it.
		macroReportContent = report.Content
		return nil
	})
	_ = macroBrief

	// Step 2 — 09:30 Parallel research
	if err := waitForScheduledOrInterval(ctx, tradingDate, schedule.Location, schedule.ResearchParallelTime, schedule.ResearcherInterval, schedule.ForceImmediate); err != nil {
		return stateSnapshot(o.state), err
	}
	_, err = o.runStep(ctx, StepResearchParallel, func(ctx context.Context) error {
		reports, e := o.researchers.RunAll(ctx, o.fundID, tradingDate)
		if e != nil {
			return e
		}
		allReports = append(allReports, reports...)
		return nil
	})

	// Step 3 — 10:00 Quant / technical signals
	if err := waitForScheduledOrInterval(ctx, tradingDate, schedule.Location, schedule.QuantSignalsTime, schedule.ResearcherInterval, schedule.ForceImmediate); err != nil {
		return stateSnapshot(o.state), err
	}
	_, err = o.runStep(ctx, StepQuantSignals, func(ctx context.Context) error {
		reports, e := o.researchers.QuantSignals(ctx, o.fundID, tradingDate)
		if e != nil {
			return e
		}
		allReports = append(allReports, reports...)
		return nil
	})

	// Step 4 — 10:30 Roundtable (max N rounds)
	if err := waitForScheduledOrInterval(ctx, tradingDate, schedule.Location, schedule.RoundtableTime, 0, schedule.ForceImmediate); err != nil {
		return stateSnapshot(o.state), err
	}
	_, err = o.runStep(ctx, StepRoundtable, func(ctx context.Context) error {
		maxRounds := o.schedule.MaxRoundtableRounds
		if maxRounds <= 0 {
			maxRounds = 3
		}
		rt, e := o.researchers.Roundtable(ctx, o.fundID, allReports, maxRounds)
		if e != nil {
			return e
		}
		// Sprint 1 / S2: stitch the macro brief onto the roundtable
		// result so the downstream PMAgent.buildDecisionInput can
		// populate DecisionInput.MacroBriefing. The roundtable engine
		// itself doesn't own the macro report — the orchestrator does
		// — so we do the join here rather than threading another
		// argument through the researcher pool interface.
		if rt != nil && strings.TrimSpace(rt.MacroBriefing) == "" {
			rt.MacroBriefing = macroReportContent
		}
		roundtable = rt
		return nil
	})

	// If roundtable failed we cannot generate a plan — mark run failed.
	if err != nil && roundtable == nil {
		o.state.setStatus(RunStatusFailed)
		o.state.EndedAt = time.Now()
		o.emit(ctx, "run_failed", StepRoundtable, nil, fmt.Errorf("roundtable failed, cannot produce plan: %w", err))
		return stateSnapshot(o.state), fmt.Errorf("roundtable failed: %w", err)
	}

	// Step 5 — 11:00 PM generates investment plan
	if err := waitForScheduledOrInterval(ctx, tradingDate, schedule.Location, schedule.PMPlanTime, schedule.PMInterval, schedule.ForceImmediate); err != nil {
		return stateSnapshot(o.state), err
	}
	_, err = o.runStep(ctx, StepPMPlan, func(ctx context.Context) error {
		p, e := o.pm.GeneratePlan(ctx, o.fundID, tradingDate, roundtable)
		if e != nil {
			return e
		}
		plan = p
		o.state.mu.Lock()
		o.state.PlanID = p.ID
		o.state.mu.Unlock()
		return nil
	})
	if err != nil && plan == nil {
		o.state.setStatus(RunStatusFailed)
		o.state.EndedAt = time.Now()
		o.emit(ctx, "run_failed", StepPMPlan, nil, fmt.Errorf("PM plan generation failed: %w", err))
		return stateSnapshot(o.state), fmt.Errorf("PM plan failed: %w", err)
	}

	// Step 6 — 11:10 Risk review (optional)
	planRejected := false
	if o.risk != nil {
		if err := waitForScheduledOrInterval(ctx, tradingDate, schedule.Location, schedule.RiskReviewTime, schedule.RiskInterval, schedule.ForceImmediate); err != nil {
			return stateSnapshot(o.state), err
		}
		_, err = o.runStep(ctx, StepRiskReview, func(ctx context.Context) error {
			approved, remarks, e := o.risk.ReviewPlan(ctx, plan)
			if e != nil {
				return e
			}
			if !approved {
				plan.Status = PlanStatusRejected
				return fmt.Errorf("%w: risk agent rejected plan: %s", errPlanRejected, remarks)
			}
			plan.Status = PlanStatusRiskReview
			return nil
		})
		if err != nil {
			if errors.Is(err, errPlanRejected) {
				planRejected = true
				o.logger.Warn("risk review rejected plan", "err", err, "run_id", o.state.RunID)
			} else {
				o.state.setStatus(RunStatusFailed)
				o.state.EndedAt = time.Now()
				o.emit(ctx, "run_failed", StepRiskReview, nil, fmt.Errorf("risk review failed: %w", err))
				return stateSnapshot(o.state), fmt.Errorf("risk review failed: %w", err)
			}
		}
	} else {
		o.recordSkip(StepRiskReview)
	}

	if !planRejected && plan != nil && plan.Status == PlanStatusRejected {
		planRejected = true
	}

	// Step 7 — 11:30 User approval
	if !planRejected {
		if err := waitForScheduledOrInterval(ctx, tradingDate, schedule.Location, schedule.UserApprovalTime, 0, schedule.ForceImmediate); err != nil {
			return stateSnapshot(o.state), err
		}
		_, err = o.runStep(ctx, StepUserApproval, func(ctx context.Context) error {
			if e := o.approval.RequestApproval(ctx, plan); e != nil {
				return fmt.Errorf("failed to request user approval: %w", e)
			}
			plan.Status = PlanStatusPendingUser
			o.state.setStatus(RunStatusPaused)
			o.emit(ctx, "awaiting_user", StepUserApproval, plan, nil)

			approved, e := o.approval.WaitForDecision(ctx, plan.ID)
			if e != nil {
				if errors.Is(e, context.DeadlineExceeded) || errors.Is(e, context.Canceled) {
					return fmt.Errorf("%w: %v", errAwaitingApproval, e)
				}
				return fmt.Errorf("user approval wait failed: %w", e)
			}
			if !approved {
				plan.Status = PlanStatusRejected
				return fmt.Errorf("%w: user rejected the plan", errPlanRejected)
			}
			plan.Status = PlanStatusApproved
			o.state.setStatus(RunStatusRunning)
			return nil
		})
		if err != nil {
			if errors.Is(err, errPlanRejected) {
				planRejected = true
				o.logger.Warn("user approval rejected plan", "err", err, "run_id", o.state.RunID)
			} else if errors.Is(err, errAwaitingApproval) {
				o.logger.Info("workflow paused awaiting user approval", "run_id", o.state.RunID, "plan_id", plan.ID)
				return stateSnapshot(o.state), nil
			} else {
				o.state.setStatus(RunStatusFailed)
				o.state.EndedAt = time.Now()
				o.emit(ctx, "run_failed", StepUserApproval, nil, fmt.Errorf("user approval failed: %w", err))
				return stateSnapshot(o.state), fmt.Errorf("user approval failed: %w", err)
			}
		}
	} else {
		o.recordSkip(StepUserApproval)
	}

	// Step 8 — Trade execution (only when approved)
	if !planRejected && plan != nil {
		if err := waitForScheduledOrInterval(ctx, tradingDate, schedule.Location, schedule.TradeExecutionTime, schedule.TraderInterval, schedule.ForceImmediate); err != nil {
			return stateSnapshot(o.state), err
		}
		_, err = o.runStep(ctx, StepTradeExecution, func(ctx context.Context) error {
			plan.Status = PlanStatusExecuting
			if e := o.pm.SubmitForExecution(ctx, plan.ID); e != nil {
				return fmt.Errorf("PM submit for execution failed: %w", e)
			}
			if e := o.trading.Execute(ctx, plan.ID); e != nil {
				return fmt.Errorf("trade execution failed: %w", e)
			}
			// Sprint 3 / L2: 当 trader 实现了 PartialFillReporter
			// 且本轮有 partial fail 时，状态降级到 mixed 而不是
			// completed。Reporter 实现可空 — 不实现 == 老行为，
			// 单元测试与 noopTrading 不受影响。
			if reporter, ok := o.trading.(PartialFillReporter); ok {
				partial, rerr := reporter.ReportPartialFill(ctx, plan.ID)
				if rerr != nil {
					o.logger.Warn("partial-fill reporter failed; defaulting to completed",
						"plan_id", plan.ID, "err", rerr)
				} else if partial {
					plan.Status = PlanStatusMixed
					o.notifyPlan(ctx, plan)
					return nil
				}
			}
			plan.Status = PlanStatusCompleted
			o.notifyPlan(ctx, plan)
			return nil
		})
		if err != nil {
			o.logger.Error("trade execution step failed", "err", err, "run_id", o.state.RunID)
		}
	} else {
		o.recordSkip(StepTradeExecution)
	}

	if planRejected {
		o.recordSkip(StepSettlement)
		o.recordSkip(StepDailyReview)
		o.state.setStatus(RunStatusRejected)
		o.state.EndedAt = time.Now()
		o.emit(ctx, "run_rejected", StepUserApproval, plan, nil)
		o.logger.Warn("daily workflow rejected", "run_id", o.state.RunID, "fund", o.fundID, "date", tradingDate)
		return stateSnapshot(o.state), nil
	}

	// Step 9 — 15:00 Daily settlement
	if err := waitForScheduledOrInterval(ctx, tradingDate, schedule.Location, schedule.SettlementTime, 0, schedule.ForceImmediate); err != nil {
		return stateSnapshot(o.state), err
	}
	_, _ = o.runStep(ctx, StepSettlement, func(ctx context.Context) error {
		return o.trading.Settle(ctx, o.fundID, tradingDate)
	})

	// Step 10 — 15:30 Daily review / memory consolidation
	if err := waitForScheduledOrInterval(ctx, tradingDate, schedule.Location, schedule.DailyReviewTime, 0, schedule.ForceImmediate); err != nil {
		return stateSnapshot(o.state), err
	}
	_, _ = o.runStep(ctx, StepDailyReview, func(ctx context.Context) error {
		return o.memory.ConsolidateDaily(ctx, o.fundID, o.state.Snapshot())
	})

	// Finalise.
	o.state.setStatus(RunStatusCompleted)
	o.state.EndedAt = time.Now()
	o.emit(ctx, "run_completed", StepDailyReview, nil, nil)
	o.logger.Info("daily workflow completed", "run_id", o.state.RunID, "fund", o.fundID, "date", tradingDate)

	return stateSnapshot(o.state), nil
}

// TriggerStep lets callers manually re-run or force a single step outside
// the normal schedule. The orchestrator must already have a state (i.e. RunFull
// was started or a previous manual trigger created one).
func (o *DailyOrchestrator) TriggerStep(ctx context.Context, step WorkflowStep, tradingDate string) (*StepResult, error) {
	o.mu.Lock()
	if o.state == nil {
		o.state = newWorkflowState(o.fundID, tradingDate)
	}
	o.mu.Unlock()

	sr, err := o.runStep(ctx, step, o.stepFuncFor(step, tradingDate))
	return &sr, err
}

// Cancel aborts a running workflow gracefully.
func (o *DailyOrchestrator) Cancel() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.cancelFn != nil {
		o.cancelFn()
	}
	if o.state != nil {
		o.state.setStatus(RunStatusCancelled)
		o.state.EndedAt = time.Now()
	}
}

// State returns a snapshot of the current workflow state, or nil.
func (o *DailyOrchestrator) State() *WorkflowState {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.state == nil {
		return nil
	}
	snap := o.state.Snapshot()
	return &snap
}

// RestoreState hydrates the orchestrator from a previously persisted snapshot.
func (o *DailyOrchestrator) RestoreState(snapshot WorkflowState) {
	o.mu.Lock()
	defer o.mu.Unlock()
	restored := snapshot
	restored.mu = &sync.RWMutex{}
	restored.StepResults = append([]StepResult(nil), snapshot.StepResults...)
	o.state = &restored
}

func (o *DailyOrchestrator) SetForceImmediate(force bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.schedule.ForceImmediate = force
}

// ResumeApprovedPlan continues a paused workflow from trade execution onward.
func (o *DailyOrchestrator) ResumeApprovedPlan(ctx context.Context, tradingDate, planID string) (*WorkflowState, error) {
	if strings.TrimSpace(planID) == "" {
		return stateSnapshot(o.state), fmt.Errorf("approved plan id is required")
	}
	ctx, cancel := context.WithCancel(ctx)

	o.mu.Lock()
	if o.running {
		o.mu.Unlock()
		cancel()
		return stateSnapshot(o.state), fmt.Errorf("workflow already running for fund %s", o.fundID)
	}
	if o.state == nil || strings.TrimSpace(o.state.TradingDate) != strings.TrimSpace(tradingDate) {
		o.state = newWorkflowState(o.fundID, tradingDate)
	}
	o.running = true
	o.cancelFn = cancel
	o.state.PlanID = strings.TrimSpace(planID)
	schedule := o.schedule
	o.mu.Unlock()

	defer func() {
		cancel()
		o.mu.Lock()
		o.running = false
		o.cancelFn = nil
		o.mu.Unlock()
	}()

	now := time.Now()
	o.state.setStatus(RunStatusRunning)
	o.state.recordStep(StepResult{Step: StepUserApproval, Status: "success", StartedAt: now, EndedAt: now})
	o.emit(ctx, "run_resumed", StepTradeExecution, nil, nil)
	o.logger.Info("daily workflow resumed", "run_id", o.state.RunID, "fund", o.fundID, "date", tradingDate, "plan_id", planID)

	plan := &InvestmentPlanResult{ID: strings.TrimSpace(planID), FundID: o.fundID, Status: PlanStatusApproved}

	if err := waitForScheduledOrInterval(ctx, tradingDate, schedule.Location, schedule.TradeExecutionTime, schedule.TraderInterval, schedule.ForceImmediate); err != nil {
		return stateSnapshot(o.state), err
	}
	if _, err := o.runStep(ctx, StepTradeExecution, func(ctx context.Context) error {
		plan.Status = PlanStatusExecuting
		if e := o.pm.SubmitForExecution(ctx, plan.ID); e != nil {
			return fmt.Errorf("PM submit for execution failed: %w", e)
		}
		if e := o.trading.Execute(ctx, plan.ID); e != nil {
			return fmt.Errorf("trade execution failed: %w", e)
		}
		plan.Status = PlanStatusCompleted
		return nil
	}); err != nil {
		o.logger.Error("trade execution step failed", "err", err, "run_id", o.state.RunID)
	}

	if err := waitForScheduledOrInterval(ctx, tradingDate, schedule.Location, schedule.SettlementTime, 0, schedule.ForceImmediate); err != nil {
		return stateSnapshot(o.state), err
	}
	_, _ = o.runStep(ctx, StepSettlement, func(ctx context.Context) error {
		return o.trading.Settle(ctx, o.fundID, tradingDate)
	})

	if err := waitForScheduledOrInterval(ctx, tradingDate, schedule.Location, schedule.DailyReviewTime, 0, schedule.ForceImmediate); err != nil {
		return stateSnapshot(o.state), err
	}
	_, _ = o.runStep(ctx, StepDailyReview, func(ctx context.Context) error {
		return o.memory.ConsolidateDaily(ctx, o.fundID, o.state.Snapshot())
	})

	o.state.setStatus(RunStatusCompleted)
	o.state.EndedAt = time.Now()
	o.emit(ctx, "run_completed", StepDailyReview, nil, nil)
	o.logger.Info("daily workflow completed", "run_id", o.state.RunID, "fund", o.fundID, "date", tradingDate)

	return stateSnapshot(o.state), nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// runStep wraps a step function with timing, event emission, and error
// handling. Non-fatal errors are recorded but do not stop the workflow.
func (o *DailyOrchestrator) runStep(ctx context.Context, step WorkflowStep, fn func(context.Context) error) (StepResult, error) {
	if err := ctx.Err(); err != nil {
		sr := StepResult{Step: step, Status: "skipped", Error: err, StartedAt: time.Now(), EndedAt: time.Now()}
		o.state.recordStep(sr)
		return sr, err
	}

	o.state.setCurrent(step)
	o.emit(ctx, "step_started", step, nil, nil)
	o.logger.Info("step started", "step", step.String(), "run_id", o.state.RunID)

	stepCtx := ctx
	cancel := func() {}
	if timeout := stepTimeout(step); timeout > 0 {
		stepCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	start := time.Now()
	// F13: retry transient failures per the step's policy. Only steps
	// in the allow-list (read-only LLM steps today) have MaxAttempts > 1.
	// Trade execution + settlement deliberately stay at 1 to avoid
	// double-submit until the underlying clients carry idempotency keys.
	err, attempts := o.runWithRetry(stepCtx, step, fn)
	if err == nil && stepCtx.Err() != nil {
		err = stepCtx.Err()
	}
	end := time.Now()

	sr := StepResult{
		Step:      step,
		StartedAt: start,
		EndedAt:   end,
	}

	if err != nil {
		switch {
		case errors.Is(err, errAwaitingApproval):
			sr.Status = "pending"
			sr.Error = nil
			o.logger.Info("step awaiting approval", "step", step.String(), "run_id", o.state.RunID, "elapsed", end.Sub(start), "attempts", attempts)
			o.emit(ctx, "step_paused", step, nil, nil)
		case errors.Is(err, ErrLLMBudgetExceeded):
			// F14: budget-exceeded pauses the run rather than failing
			// it. User can bump the budget and re-trigger; without the
			// pause, the next scheduler tick would burn N retries on
			// the same budget-exhausted owner.
			sr.Status = "paused"
			sr.Error = err
			o.state.setStatus(RunStatusPaused)
			o.logger.Warn("step paused: llm dollar budget exceeded",
				"step", step.String(),
				"run_id", o.state.RunID,
				"err", err,
			)
			o.emit(ctx, "step_paused", step, map[string]any{"reason": "llm_budget_exceeded", "err": err.Error()}, nil)
		default:
			sr.Status = "failed"
			sr.Error = err
			o.logger.Error("step failed", "step", step.String(), "err", err, "run_id", o.state.RunID, "elapsed", end.Sub(start), "attempts", attempts)
			o.emit(ctx, "step_failed", step, nil, err)
		}
	} else {
		sr.Status = "success"
		o.logger.Info("step completed", "step", step.String(), "run_id", o.state.RunID, "elapsed", end.Sub(start), "attempts", attempts)
		o.emit(ctx, "step_completed", step, nil, nil)
	}

	o.state.recordStep(sr)
	return sr, err
}

// runWithRetry executes fn under the step's RetryPolicy, returning the
// final error and the total number of attempts made. Attempts >= 1
// always (we count the first call). Transient failures sleep between
// attempts using the policy's backoff envelope. Non-transient failures
// return immediately so business-logic bugs aren't masked by retries.
func (o *DailyOrchestrator) runWithRetry(ctx context.Context, step WorkflowStep, fn func(context.Context) error) (error, int) {
	policy := defaultStepRetryPolicy(step)
	if policy.MaxAttempts < 1 {
		policy.MaxAttempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err, attempt - 1
		}
		err := fn(ctx)
		if err == nil {
			return nil, attempt
		}
		lastErr = err
		// Last attempt — no point sleeping, just return.
		if attempt == policy.MaxAttempts {
			break
		}
		if !IsTransient(err) {
			return err, attempt
		}
		delay := computeBackoff(policy, attempt+1, o.retryRNG)
		o.logger.Warn("retrying transient failure",
			"step", step.String(),
			"attempt", attempt,
			"max_attempts", policy.MaxAttempts,
			"delay", delay.String(),
			"err", err,
		)
		// Emit step_retried so SSE / activity stream surfaces the
		// fan-out instead of hiding it under one long-running step.
		o.emit(ctx, "step_retried", step, map[string]any{
			"attempt":      attempt,
			"max_attempts": policy.MaxAttempts,
			"delay_ms":     delay.Milliseconds(),
			"err":          err.Error(),
		}, nil)
		if delay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err(), attempt
			case <-time.After(delay):
			}
		}
	}
	return lastErr, policy.MaxAttempts
}

// recordSkip adds a "skipped" result without running anything.
func (o *DailyOrchestrator) recordSkip(step WorkflowStep) {
	now := time.Now()
	sr := StepResult{Step: step, Status: "skipped", StartedAt: now, EndedAt: now}
	o.state.recordStep(sr)
	o.logger.Info("step skipped", "step", step.String(), "run_id", o.state.RunID)
	o.emit(context.Background(), "step_skipped", step, nil, nil)
}

// emit publishes an event; failures are logged but never propagated.
func (o *DailyOrchestrator) emit(ctx context.Context, eventType string, step WorkflowStep, payload interface{}, err error) {
	evt := WorkflowEvent{
		Type:        eventType,
		RunID:       o.state.RunID,
		FundID:      o.fundID,
		Step:        step,
		TradingDate: o.state.TradingDate,
		Payload:     payload,
		Timestamp:   time.Now(),
		Error:       err,
		Snapshot:    stateSnapshot(o.state),
	}
	if pubErr := o.eventBus.Publish(ctx, evt); pubErr != nil {
		o.logger.Warn("failed to publish event", "type", eventType, "err", pubErr)
	}
}

// stepFuncFor returns the executable closure for a given step, used by
// TriggerStep for manual invocations.
func (o *DailyOrchestrator) stepFuncFor(step WorkflowStep, tradingDate string) func(context.Context) error {
	switch step {
	case StepMacroBrief:
		return func(ctx context.Context) error {
			_, err := o.researchers.MacroBrief(ctx, o.fundID, tradingDate)
			return err
		}
	case StepResearchParallel:
		return func(ctx context.Context) error {
			_, err := o.researchers.RunAll(ctx, o.fundID, tradingDate)
			return err
		}
	case StepQuantSignals:
		return func(ctx context.Context) error {
			_, err := o.researchers.QuantSignals(ctx, o.fundID, tradingDate)
			return err
		}
	case StepSettlement:
		return func(ctx context.Context) error {
			return o.trading.Settle(ctx, o.fundID, tradingDate)
		}
	case StepDailyReview:
		return func(ctx context.Context) error {
			return o.memory.ConsolidateDaily(ctx, o.fundID, o.state.Snapshot())
		}
	default:
		return func(ctx context.Context) error {
			return fmt.Errorf("manual trigger not supported for step %s", step)
		}
	}
}

// stateSnapshot is a nil-safe helper.
func stateSnapshot(ws *WorkflowState) *WorkflowState {
	if ws == nil {
		return nil
	}
	snap := ws.Snapshot()
	return &snap
}

func scheduleTimeForTradingDate(tradingDate string, loc *time.Location, clock string) (time.Time, bool) {
	if loc == nil {
		loc = time.UTC
	}
	trimmedClock := strings.TrimSpace(clock)
	if strings.TrimSpace(tradingDate) == "" || trimmedClock == "" {
		return time.Time{}, false
	}
	parsed, err := time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(tradingDate)+" "+trimmedClock, loc)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

func waitForScheduledOrInterval(ctx context.Context, tradingDate string, loc *time.Location, clock string, interval time.Duration, forceImmediate bool) error {
	if forceImmediate {
		return nil
	}
	if scheduledAt, ok := scheduleTimeForTradingDate(tradingDate, loc, clock); ok {
		delay := time.Until(scheduledAt)
		if delay <= 0 {
			return nil
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	return waitForInterval(ctx, interval)
}

func waitForInterval(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return nil
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func stepTimeout(step WorkflowStep) time.Duration {
	switch step {
	case StepMacroBrief, StepQuantSignals:
		return 2 * time.Minute
	case StepResearchParallel, StepRoundtable, StepPMPlan, StepRiskReview:
		return 5 * time.Minute
	case StepUserApproval:
		return 0
	case StepTradeExecution, StepSettlement, StepDailyReview:
		return 3 * time.Minute
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// CronScheduler — thin wrapper for automatic daily triggering
// ---------------------------------------------------------------------------

// CronScheduler runs the daily workflow automatically at the configured start
// time. It is intentionally simple; production deployments may replace it with
// a proper cron library.
type CronScheduler struct {
	orchestrator *DailyOrchestrator
	schedule     ScheduleConfig
	logger       *slog.Logger
	stopCh       chan struct{}
	wg           sync.WaitGroup
}

// NewCronScheduler creates a scheduler bound to an orchestrator.
func NewCronScheduler(o *DailyOrchestrator) *CronScheduler {
	return &CronScheduler{
		orchestrator: o,
		schedule:     o.schedule,
		logger:       o.logger,
		stopCh:       make(chan struct{}),
	}
}

// Start begins the scheduling loop in a background goroutine.
func (cs *CronScheduler) Start() {
	cs.wg.Add(1)
	go cs.loop()
}

// Stop terminates the scheduling loop and waits for it to exit.
func (cs *CronScheduler) Stop() {
	close(cs.stopCh)
	cs.wg.Wait()
}

func (cs *CronScheduler) loop() {
	defer cs.wg.Done()
	for {
		next := cs.nextTriggerTime()
		delay := time.Until(next)
		if delay < 0 {
			// Already past today's trigger — schedule for tomorrow.
			delay += 24 * time.Hour
		}

		cs.logger.Info("cron: next daily run scheduled", "at", next.Format(time.RFC3339), "in", delay)

		select {
		case <-cs.stopCh:
			cs.logger.Info("cron: scheduler stopped")
			return
		case <-time.After(delay):
			tradingDate := time.Now().In(cs.schedule.Location).Format("2006-01-02")
			cs.logger.Info("cron: triggering daily workflow", "date", tradingDate)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
			if _, err := cs.orchestrator.RunFull(ctx, tradingDate); err != nil {
				cs.logger.Error("cron: daily workflow returned error", "err", err)
			}
			cancel()
		}
	}
}

// nextTriggerTime computes today's macro-brief start time in the configured
// timezone (the earliest step in the schedule).
func (cs *CronScheduler) nextTriggerTime() time.Time {
	loc := cs.schedule.Location
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	t, err := time.ParseInLocation("2006-01-02 15:04", now.Format("2006-01-02")+" "+cs.schedule.MacroBriefTime, loc)
	if err != nil {
		// Fallback — 09:00 today.
		t = time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, loc)
	}
	if t.Before(now) {
		t = t.Add(24 * time.Hour)
	}
	return t
}
