package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/marketcalendar"
	"github.com/fundai/server/internal/repository"
)

// stubSchedulerService is a programmable test double for the narrow
// schedulerService interface. Each method returns whatever the fixture
// configures, and we record call counts so tests can assert that the
// scheduler did NOT, for example, call StartWorkflowForFund when a
// fund was already completed.
type stubSchedulerService struct {
	funds            []repository.Fund
	listErr          error
	runs             map[string]*repository.WorkflowRun
	runErr           error
	triggerAt        time.Time
	tradingDate      time.Time
	nextErr          error
	startStatus      *api.WorkflowStatus
	startErr         error
	startCalls       []startCall
	session          *marketcalendar.TradingSession
	slots            []time.Time
	profileByFundID  map[string]marketcalendar.Profile
	defaultProfile   marketcalendar.Profile
}

type startCall struct {
	FundID      string
	TradingDate string
	SlotTime    time.Time
}

func newStubSchedulerService() *stubSchedulerService {
	return &stubSchedulerService{
		runs:            map[string]*repository.WorkflowRun{},
		profileByFundID: map[string]marketcalendar.Profile{},
		defaultProfile:  marketcalendar.Profile{CalendarCode: "US", TimeZone: "America/New_York"},
		session:         &marketcalendar.TradingSession{IsTradingDay: true},
	}
}

func (s *stubSchedulerService) ListActiveFunds(ctx context.Context) ([]repository.Fund, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.funds, nil
}

func (s *stubSchedulerService) GetWorkflowRun(ctx context.Context, fundID string, tradingDate time.Time) (*repository.WorkflowRun, error) {
	if s.runErr != nil {
		return nil, s.runErr
	}
	if run, ok := s.runs[fundID]; ok {
		return run, nil
	}
	return nil, repository.ErrNotFound
}

func (s *stubSchedulerService) NextWorkflowStart(now time.Time, profile marketcalendar.Profile) (time.Time, time.Time, error) {
	if s.nextErr != nil {
		return time.Time{}, time.Time{}, s.nextErr
	}
	return s.triggerAt, s.tradingDate, nil
}

func (s *stubSchedulerService) SessionForDate(date time.Time, profile marketcalendar.Profile) (*marketcalendar.TradingSession, error) {
	return s.session, nil
}

// TradingTriggerSlots returns the canned slot list configured on
// the stub. Empty by default — tests that exercise the catch-up
// path set s.slots explicitly.
func (s *stubSchedulerService) TradingTriggerSlots(session *marketcalendar.TradingSession, intervalMinutes int) []time.Time {
	return s.slots
}

func (s *stubSchedulerService) TradingProfileForFund(fund *repository.Fund) marketcalendar.Profile {
	if fund == nil {
		return s.defaultProfile
	}
	if p, ok := s.profileByFundID[fund.ID]; ok {
		return p
	}
	return s.defaultProfile
}

func (s *stubSchedulerService) StartWorkflowForFund(fund *repository.Fund, tradingDate, slotTime time.Time) (*api.WorkflowStatus, error) {
	s.startCalls = append(s.startCalls, startCall{
		FundID:      fund.ID,
		TradingDate: tradingDate.Format("2006-01-02"),
		SlotTime:    slotTime,
	})
	if s.startErr != nil {
		return nil, s.startErr
	}
	return s.startStatus, nil
}

// fundFixture returns a list of N active funds with stable IDs so tests
// can assert per-fund decisions in the snapshot.
func fundFixture(n int) []repository.Fund {
	funds := make([]repository.Fund, 0, n)
	for i := 0; i < n; i++ {
		funds = append(funds, repository.Fund{ID: "fund-" + string(rune('A'+i)), Name: "Fund-" + string(rune('A'+i))})
	}
	return funds
}

// TestTriggerDueFundsStartsFundWithNoRun is the happy-path matrix entry:
// fund is due (triggerAt <= now) and has no prior workflow_run row.
// Scheduler should call StartWorkflowForFund once and the snapshot
// should reflect a successful trigger.
func TestTriggerDueFundsStartsFundWithNoRun(t *testing.T) {
	stub := newStubSchedulerService()
	now := time.Date(2026, 5, 19, 9, 30, 0, 0, time.UTC)
	stub.funds = fundFixture(1)
	stub.triggerAt = now.Add(-5 * time.Minute)
	stub.tradingDate = now.Truncate(24 * time.Hour)
	stub.startStatus = &api.WorkflowStatus{State: "running", Step: "macro_brief"}

	sched := &fundWorkflowScheduler{service: stub}
	snap := sched.triggerDueFunds(now)

	if len(stub.startCalls) != 1 {
		t.Fatalf("expected exactly 1 StartWorkflowForFund call, got %d", len(stub.startCalls))
	}
	if stub.startCalls[0].FundID != "fund-A" {
		t.Fatalf("expected fund-A to be triggered, got %q", stub.startCalls[0].FundID)
	}
	if snap.TriggeredCount != 1 {
		t.Fatalf("expected TriggeredCount=1, got %d", snap.TriggeredCount)
	}
	if snap.TotalActive != 1 {
		t.Fatalf("expected TotalActive=1, got %d", snap.TotalActive)
	}
	if len(snap.Funds) != 1 || !snap.Funds[0].Started {
		t.Fatalf("expected snapshot row marked Started=true, got %+v", snap.Funds)
	}
	if snap.Funds[0].LastStatus != "running" {
		t.Fatalf("expected LastStatus=running, got %q", snap.Funds[0].LastStatus)
	}
}

// TestTriggerDueFundsSkipsCompletedRun guards the most important
// invariant — a completed daily run for the day's tradingDate must
// not be re-triggered. Snapshot must record skip reason="not-yet-due"
// (because nextFundStart rolls forward to tomorrow when the current
// trading day is already done).
func TestTriggerDueFundsSkipsCompletedRun(t *testing.T) {
	stub := newStubSchedulerService()
	now := time.Date(2026, 5, 19, 15, 0, 0, 0, time.UTC)
	stub.funds = fundFixture(1)
	stub.tradingDate = now.Truncate(24 * time.Hour)
	stub.triggerAt = now.Add(-1 * time.Hour) // today's trigger already passed
	stub.runs["fund-A"] = &repository.WorkflowRun{
		FundID:      "fund-A",
		Status:      "completed",
		TradingDate: stub.tradingDate,
	}

	// On the second NextWorkflowStart call (for tomorrow), return a
	// future trigger.
	tomorrow := stub.tradingDate.AddDate(0, 0, 1)
	stub.triggerAt = now.Add(20 * time.Hour) // intentionally in the future
	stub.tradingDate = tomorrow

	sched := &fundWorkflowScheduler{service: stub}
	snap := sched.triggerDueFunds(now)

	if len(stub.startCalls) != 0 {
		t.Fatalf("expected 0 trigger calls when run already completed, got %d", len(stub.startCalls))
	}
	if snap.TriggeredCount != 0 {
		t.Fatalf("expected TriggeredCount=0, got %d", snap.TriggeredCount)
	}
	if len(snap.Funds) != 1 || snap.Funds[0].Started || snap.Funds[0].SkipReason != "not-yet-due" {
		t.Fatalf("expected skip-reason=not-yet-due, got %+v", snap.Funds)
	}
}

// TestNextFundStartCatchesUpMissedIntervalSlot pins the catch-up
// invariant: when an interval-mode fund has a slot that fired
// within the last `intervalMinutes` window but was never claimed
// (e.g. server restart, leader churn), the next scheduler tick
// should return that missed slot as the trigger — not the next
// future slot. Without this the 10:30 / 14:00 BJT slots got
// silently skipped on 2026-05-22 because a low-confidence plan
// at 10:00 wedged the workflow and a server restart at 14:01
// killed the in-flight run.
func TestNextFundStartCatchesUpMissedIntervalSlot(t *testing.T) {
	stub := newStubSchedulerService()
	loc, _ := time.LoadLocation("Asia/Shanghai")
	// Wall-clock at 14:08 BJT — 14:00 slot already past but well
	// within the 30-min freshness window.
	now := time.Date(2026, 5, 22, 14, 8, 0, 0, loc)
	stub.funds = fundFixture(1)
	interval := 30
	stub.defaultProfile = marketcalendar.Profile{
		CalendarCode:            "CN-SSE",
		TimeZone:                "Asia/Shanghai",
		DecisionIntervalMinutes: &interval,
	}
	stub.session = &marketcalendar.TradingSession{
		TradingDate:  "2026-05-22",
		Location:     loc,
		IsTradingDay: true,
	}
	// All today's slots — the catch-up logic only needs the slot
	// list, the session itself is opaque to it.
	stub.slots = []time.Time{
		time.Date(2026, 5, 22, 13, 30, 0, 0, loc),
		time.Date(2026, 5, 22, 14, 0, 0, 0, loc), // the missed one
		time.Date(2026, 5, 22, 14, 30, 0, 0, loc),
	}
	// NextWorkflowStart returns the next future slot (14:30) as
	// the calendar normally would.
	stub.triggerAt = time.Date(2026, 5, 22, 14, 30, 0, 0, loc)
	stub.tradingDate = time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC)
	// Existing run completed for an earlier slot (13:30).
	stub.runs["fund-A"] = &repository.WorkflowRun{
		FundID:      "fund-A",
		Status:      "completed",
		StartedAt:   sql.NullTime{Time: time.Date(2026, 5, 22, 13, 30, 0, 0, loc), Valid: true},
		TradingDate: stub.tradingDate,
	}

	sched := &fundWorkflowScheduler{service: stub}
	triggerAt, _, due, err := sched.nextFundStart(now, &stub.funds[0])
	if err != nil {
		t.Fatalf("nextFundStart: %v", err)
	}
	wantSlot := time.Date(2026, 5, 22, 14, 0, 0, 0, loc)
	if !triggerAt.Equal(wantSlot) {
		t.Fatalf("expected catch-up trigger at 14:00, got %s", triggerAt.Format(time.RFC3339))
	}
	if !due {
		t.Fatal("missed slot must be reported due")
	}
}

// TestNextFundStartSkipsCatchUpBeyondFreshness ensures the catch-up
// path does NOT replay slots older than one interval window — a
// scheduler that's been down for 2 hours shouldn't dump every
// stale slot back into the queue.
func TestNextFundStartSkipsCatchUpBeyondFreshness(t *testing.T) {
	stub := newStubSchedulerService()
	loc, _ := time.LoadLocation("Asia/Shanghai")
	now := time.Date(2026, 5, 22, 14, 8, 0, 0, loc)
	stub.funds = fundFixture(1)
	interval := 30
	stub.defaultProfile = marketcalendar.Profile{
		CalendarCode:            "CN-SSE",
		TimeZone:                "Asia/Shanghai",
		DecisionIntervalMinutes: &interval,
	}
	stub.session = &marketcalendar.TradingSession{
		TradingDate:  "2026-05-22",
		Location:     loc,
		IsTradingDay: true,
	}
	// Only slots that are >30 min old — outside the catch-up window.
	stub.slots = []time.Time{
		time.Date(2026, 5, 22, 11, 0, 0, 0, loc),
		time.Date(2026, 5, 22, 11, 30, 0, 0, loc),
	}
	stub.triggerAt = time.Date(2026, 5, 22, 14, 30, 0, 0, loc)
	stub.tradingDate = time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC)
	stub.runs["fund-A"] = &repository.WorkflowRun{
		FundID:      "fund-A",
		Status:      "completed",
		StartedAt:   sql.NullTime{Time: time.Date(2026, 5, 22, 9, 30, 0, 0, loc), Valid: true},
		TradingDate: stub.tradingDate,
	}

	sched := &fundWorkflowScheduler{service: stub}
	triggerAt, _, due, err := sched.nextFundStart(now, &stub.funds[0])
	if err != nil {
		t.Fatalf("nextFundStart: %v", err)
	}
	if !triggerAt.Equal(time.Date(2026, 5, 22, 14, 30, 0, 0, loc)) {
		t.Fatalf("expected next future slot 14:30, got %s", triggerAt.Format(time.RFC3339))
	}
	if due {
		t.Fatal("stale slot must NOT be reported due")
	}
}

// TestNextFundStartCatchUpDedupesAgainstFailedRunForSameSlot pins
// the "don't loop on a claimed slot" invariant. After the catch-up
// fires once (status -> running, started_at -> slotTime), every
// subsequent scheduler tick must see the row as already-claimed and
// stop firing. Reproduces the 52k-fires-in-2-minutes hot loop we
// observed when the dedupe check was reading the wrong tradingDate's
// run row.
func TestNextFundStartCatchUpDedupesAgainstFailedRunForSameSlot(t *testing.T) {
	stub := newStubSchedulerService()
	loc, _ := time.LoadLocation("Asia/Shanghai")
	now := time.Date(2026, 5, 22, 14, 46, 0, 0, loc)
	stub.funds = fundFixture(1)
	interval := 30
	stub.defaultProfile = marketcalendar.Profile{
		CalendarCode:            "CN-SSE",
		TimeZone:                "Asia/Shanghai",
		DecisionIntervalMinutes: &interval,
	}
	stub.session = &marketcalendar.TradingSession{
		TradingDate:  "2026-05-22",
		Location:     loc,
		IsTradingDay: true,
	}
	stub.slots = []time.Time{
		time.Date(2026, 5, 22, 14, 0, 0, 0, loc),
		time.Date(2026, 5, 22, 14, 30, 0, 0, loc),
	}
	// NextWorkflowStart returns next business day — we're past
	// today's last slot.
	stub.triggerAt = time.Date(2026, 5, 25, 9, 0, 0, 0, loc)
	stub.tradingDate = time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	// TODAY's run row claimed 14:30 already (e.g. the previous
	// catch-up fire took it). Even though status is "failed"
	// (the workflow crashed mid-run), the slot was claimed and
	// must not be re-fired.
	stub.runs["fund-A"] = &repository.WorkflowRun{
		FundID:      "fund-A",
		Status:      "failed",
		StartedAt:   sql.NullTime{Time: time.Date(2026, 5, 22, 14, 30, 0, 0, loc).UTC(), Valid: true},
		TradingDate: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
	}

	sched := &fundWorkflowScheduler{service: stub}
	triggerAt, _, due, err := sched.nextFundStart(now, &stub.funds[0])
	if err != nil {
		t.Fatalf("nextFundStart: %v", err)
	}
	if due {
		t.Fatalf("must not re-fire claimed slot (got due=true, trigger=%s)", triggerAt.Format(time.RFC3339))
	}
}

// TestNextFundStartCatchUpRetriesRecoveryFailedSlotOnce locks in the
// "auto-recover after server restart" path: a workflow_run marked
// failed by the recovery code (step_results.{step}.error has a
// "recovery: " prefix) IS retried by the next scheduler tick within
// the freshness window, even though the slot is technically already
// claimed. Crucially, the retry is one-shot per process — a second
// call returns due=false so a crashing process can't loop on the
// same slot every tick.
func TestNextFundStartCatchUpRetriesRecoveryFailedSlotOnce(t *testing.T) {
	stub := newStubSchedulerService()
	loc, _ := time.LoadLocation("Asia/Shanghai")
	now := time.Date(2026, 5, 22, 14, 46, 0, 0, loc)
	stub.funds = fundFixture(1)
	interval := 30
	stub.defaultProfile = marketcalendar.Profile{
		CalendarCode:            "CN-SSE",
		TimeZone:                "Asia/Shanghai",
		DecisionIntervalMinutes: &interval,
	}
	stub.session = &marketcalendar.TradingSession{
		TradingDate:  "2026-05-22",
		Location:     loc,
		IsTradingDay: true,
	}
	stub.slots = []time.Time{
		time.Date(2026, 5, 22, 14, 0, 0, 0, loc),
		time.Date(2026, 5, 22, 14, 30, 0, 0, loc),
	}
	stub.triggerAt = time.Date(2026, 5, 25, 9, 0, 0, 0, loc)
	stub.tradingDate = time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)

	// Today's run failed via the recovery code, NOT a genuine
	// workflow error. step_results MUST have at least one entry
	// whose error starts with "recovery: " — that is the trigger
	// for the auto-retry path.
	stepResults := map[string]any{
		"roundtable": map[string]any{
			"step":   "roundtable",
			"status": "failed",
			"error":  "recovery: server restart interrupted in-flight step",
		},
	}
	encoded, _ := json.Marshal(stepResults)
	stub.runs["fund-A"] = &repository.WorkflowRun{
		FundID:      "fund-A",
		Status:      "failed",
		StartedAt:   sql.NullTime{Time: time.Date(2026, 5, 22, 14, 30, 0, 0, loc).UTC(), Valid: true},
		TradingDate: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
		StepResults: encoded,
	}

	sched := &fundWorkflowScheduler{service: stub}

	triggerAt, _, due, err := sched.nextFundStart(now, &stub.funds[0])
	if err != nil {
		t.Fatalf("nextFundStart #1: %v", err)
	}
	if !due {
		t.Fatalf("first recovery retry must fire; got due=false trigger=%s", triggerAt.Format(time.RFC3339))
	}
	wantSlot := time.Date(2026, 5, 22, 14, 30, 0, 0, loc)
	if !triggerAt.Equal(wantSlot) {
		t.Fatalf("first retry should target the failed slot 14:30, got %s", triggerAt.Format(time.RFC3339))
	}

	// Second tick: the in-memory dedupe must shut us off so a
	// crashing process can't replay the slot every 30s.
	_, _, due2, err := sched.nextFundStart(now.Add(30*time.Second), &stub.funds[0])
	if err != nil {
		t.Fatalf("nextFundStart #2: %v", err)
	}
	if due2 {
		t.Fatal("second recovery retry must NOT fire (per-process one-shot dedupe broken)")
	}
}

// TestNextFundStartCatchUpDoesNotRetryGenuineFailures guards against
// over-eager retry: a run that failed for a NON-recovery reason
// (LLM error, data outage, etc.) must NOT be auto-retried — those
// need a human to look at the audit log first.
func TestNextFundStartCatchUpDoesNotRetryGenuineFailures(t *testing.T) {
	stub := newStubSchedulerService()
	loc, _ := time.LoadLocation("Asia/Shanghai")
	now := time.Date(2026, 5, 22, 14, 46, 0, 0, loc)
	stub.funds = fundFixture(1)
	interval := 30
	stub.defaultProfile = marketcalendar.Profile{
		CalendarCode:            "CN-SSE",
		TimeZone:                "Asia/Shanghai",
		DecisionIntervalMinutes: &interval,
	}
	stub.session = &marketcalendar.TradingSession{
		TradingDate:  "2026-05-22",
		Location:     loc,
		IsTradingDay: true,
	}
	stub.slots = []time.Time{
		time.Date(2026, 5, 22, 14, 0, 0, 0, loc),
		time.Date(2026, 5, 22, 14, 30, 0, 0, loc),
	}
	stub.triggerAt = time.Date(2026, 5, 25, 9, 0, 0, 0, loc)
	stub.tradingDate = time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)

	stepResults := map[string]any{
		"roundtable": map[string]any{
			"step":   "roundtable",
			"status": "failed",
			"error":  "llm provider returned 503 after 3 retries",
		},
	}
	encoded, _ := json.Marshal(stepResults)
	stub.runs["fund-A"] = &repository.WorkflowRun{
		FundID:      "fund-A",
		Status:      "failed",
		StartedAt:   sql.NullTime{Time: time.Date(2026, 5, 22, 14, 30, 0, 0, loc).UTC(), Valid: true},
		TradingDate: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
		StepResults: encoded,
	}

	sched := &fundWorkflowScheduler{service: stub}
	_, _, due, err := sched.nextFundStart(now, &stub.funds[0])
	if err != nil {
		t.Fatalf("nextFundStart: %v", err)
	}
	if due {
		t.Fatal("genuine LLM failure must NOT auto-retry; got due=true")
	}
}

// TestNextFundStartCatchUpSkipsActivelyRunningRun ensures that an
// in-flight run on TODAY's row blocks catch-up even when an earlier
// slot would otherwise be eligible. The "running" status is the
// scheduler's lock — don't fork a parallel workflow on top of it.
func TestNextFundStartCatchUpSkipsActivelyRunningRun(t *testing.T) {
	stub := newStubSchedulerService()
	loc, _ := time.LoadLocation("Asia/Shanghai")
	now := time.Date(2026, 5, 22, 14, 35, 0, 0, loc)
	stub.funds = fundFixture(1)
	interval := 30
	stub.defaultProfile = marketcalendar.Profile{
		CalendarCode:            "CN-SSE",
		TimeZone:                "Asia/Shanghai",
		DecisionIntervalMinutes: &interval,
	}
	stub.session = &marketcalendar.TradingSession{
		TradingDate:  "2026-05-22",
		Location:     loc,
		IsTradingDay: true,
	}
	stub.slots = []time.Time{
		time.Date(2026, 5, 22, 14, 0, 0, 0, loc),
		time.Date(2026, 5, 22, 14, 30, 0, 0, loc),
	}
	stub.triggerAt = time.Date(2026, 5, 25, 9, 0, 0, 0, loc)
	stub.tradingDate = time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	stub.runs["fund-A"] = &repository.WorkflowRun{
		FundID:      "fund-A",
		Status:      "running",
		StartedAt:   sql.NullTime{Time: time.Date(2026, 5, 22, 14, 30, 0, 0, loc).UTC(), Valid: true},
		TradingDate: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
	}

	sched := &fundWorkflowScheduler{service: stub}
	_, _, due, err := sched.nextFundStart(now, &stub.funds[0])
	if err != nil {
		t.Fatalf("nextFundStart: %v", err)
	}
	if due {
		t.Fatal("must not catch-up while today's run is actively running")
	}
}

// TestTriggerDueFundsRetriesFailedRun verifies that a failed prior run
// is retried (schedulerRunNeedsTrigger treats "failed" as retriable).
// This is the "auto-recover after transient error" invariant.
func TestTriggerDueFundsRetriesFailedRun(t *testing.T) {
	stub := newStubSchedulerService()
	now := time.Date(2026, 5, 19, 9, 30, 0, 0, time.UTC)
	stub.funds = fundFixture(1)
	stub.triggerAt = now.Add(-1 * time.Minute)
	stub.tradingDate = now.Truncate(24 * time.Hour)
	stub.runs["fund-A"] = &repository.WorkflowRun{
		FundID:      "fund-A",
		Status:      "failed",
		TradingDate: stub.tradingDate,
	}
	stub.startStatus = &api.WorkflowStatus{State: "running", Step: "macro_brief"}

	sched := &fundWorkflowScheduler{service: stub}
	snap := sched.triggerDueFunds(now)

	if len(stub.startCalls) != 1 {
		t.Fatalf("expected failed run to be retried, got %d trigger calls", len(stub.startCalls))
	}
	if snap.TriggeredCount != 1 {
		t.Fatalf("expected TriggeredCount=1 after retry, got %d", snap.TriggeredCount)
	}
}

// TestTriggerDueFundsDoesNotTouchRunningRun makes sure that a workflow
// already in 'running' state is left alone — the scheduler must never
// preempt an in-flight workflow, otherwise we'd lose the per-step
// progress that fundamentally lives in the orchestrator's runtime
// memory.
func TestTriggerDueFundsDoesNotTouchRunningRun(t *testing.T) {
	stub := newStubSchedulerService()
	now := time.Date(2026, 5, 19, 9, 30, 0, 0, time.UTC)
	stub.funds = fundFixture(1)
	stub.triggerAt = now.Add(-30 * time.Second)
	stub.tradingDate = now.Truncate(24 * time.Hour)
	stub.runs["fund-A"] = &repository.WorkflowRun{
		FundID:      "fund-A",
		Status:      "running",
		TradingDate: stub.tradingDate,
	}

	sched := &fundWorkflowScheduler{service: stub}
	snap := sched.triggerDueFunds(now)

	if len(stub.startCalls) != 0 {
		t.Fatalf("expected scheduler to skip running workflows, got %d trigger calls", len(stub.startCalls))
	}
	if snap.TriggeredCount != 0 {
		t.Fatalf("expected TriggeredCount=0, got %d", snap.TriggeredCount)
	}
}

// TestTriggerDueFundsRecordsStartError ensures that a per-fund start
// error doesn't poison the snapshot — the failing fund's row carries
// the error string and SkipReason="start-error", subsequent funds are
// still evaluated.
func TestTriggerDueFundsRecordsStartError(t *testing.T) {
	stub := newStubSchedulerService()
	now := time.Date(2026, 5, 19, 9, 30, 0, 0, time.UTC)
	stub.funds = fundFixture(2) // fund-A and fund-B
	stub.triggerAt = now.Add(-1 * time.Minute)
	stub.tradingDate = now.Truncate(24 * time.Hour)
	stub.startErr = errors.New("boom")

	sched := &fundWorkflowScheduler{service: stub}
	snap := sched.triggerDueFunds(now)

	// Both funds were attempted (the loop continues past per-fund
	// errors) but neither was actually started.
	if len(stub.startCalls) != 2 {
		t.Fatalf("expected scheduler to attempt both funds, got %d", len(stub.startCalls))
	}
	if snap.TriggeredCount != 0 {
		t.Fatalf("expected TriggeredCount=0 when all starts fail, got %d", snap.TriggeredCount)
	}
	if len(snap.Funds) != 2 {
		t.Fatalf("expected 2 snapshot rows, got %d", len(snap.Funds))
	}
	for _, row := range snap.Funds {
		if row.SkipReason != "start-error" {
			t.Fatalf("expected skip-reason=start-error, got %q", row.SkipReason)
		}
		if row.Error == "" {
			t.Fatalf("expected Error to be set, got empty for fund %q", row.FundID)
		}
	}
}

// TestTriggerDueFundsRecordsListError verifies that a fatal ListActive
// error short-circuits the tick (no per-fund decisions) and is recorded
// on the snapshot.
func TestTriggerDueFundsRecordsListError(t *testing.T) {
	stub := newStubSchedulerService()
	stub.listErr = errors.New("db down")

	sched := &fundWorkflowScheduler{service: stub}
	snap := sched.triggerDueFunds(time.Now())

	if snap.LastError == "" {
		t.Fatalf("expected LastError to be set, got empty")
	}
	if len(snap.Funds) != 0 {
		t.Fatalf("expected no fund rows when list fails, got %d", len(snap.Funds))
	}
}

// TestSnapshotReturnsZeroBeforeFirstPoll is the "scheduler hasn't run
// yet" case — admin UI must render this without crashing. The snapshot
// should be returned by value (no nil pointer panics) and have a
// zero LastPollAt.
func TestSnapshotReturnsZeroBeforeFirstPoll(t *testing.T) {
	sched := &fundWorkflowScheduler{}
	snap := sched.Snapshot()
	if !snap.LastPollAt.IsZero() {
		t.Fatalf("expected zero LastPollAt before first poll, got %v", snap.LastPollAt)
	}
	if len(snap.Funds) != 0 {
		t.Fatalf("expected zero fund rows, got %d", len(snap.Funds))
	}
}

// TestSnapshotIsRaceFreeWithStore exercises the snapshotMu lock —
// concurrent Snapshot() reads and storeSnapshot writes must not race
// or panic. Detected automatically by `go test -race`.
func TestSnapshotIsRaceFreeWithStore(t *testing.T) {
	sched := &fundWorkflowScheduler{}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			sched.storeSnapshot(FundSchedulerSnapshot{
				LastPollAt: time.Now(),
				Funds:      []FundSchedulerStatus{{FundID: "fund-A"}},
			})
		}
		close(done)
	}()
	for i := 0; i < 100; i++ {
		snap := sched.Snapshot()
		// Mutating the returned slice must not affect the internal one
		// (the Snapshot copy-on-read contract).
		if snap.Funds != nil {
			snap.Funds[0].FundID = "mutated"
		}
	}
	<-done
}

// TestSchedulerRunNeedsTriggerMatrix locks the policy table that
// schedulerRunNeedsTrigger encodes: which workflow statuses are
// retriable vs sticky. Kept as a stand-alone test so a future
// refactor that accidentally flips one row (e.g. starts retrying
// "completed" runs) trips a single small failure.
func TestSchedulerRunNeedsTriggerMatrix(t *testing.T) {
	cases := map[string]bool{
		"":          true,
		"pending":   true,
		"failed":    true,
		"cancelled": true,
		"running":   false,
		"paused":    false,
		"rejected":  false,
		"completed": false,
	}
	for status, expectRetry := range cases {
		got := schedulerRunNeedsTrigger(&repository.WorkflowRun{Status: status})
		if got != expectRetry {
			t.Errorf("status=%q: expected needsTrigger=%v, got %v", status, expectRetry, got)
		}
	}
	if !schedulerRunNeedsTrigger(nil) {
		t.Errorf("nil run: expected needsTrigger=true (treat absent as new), got false")
	}
}

// TestTriggerDueFundsIntervalModeFiresEachSlot validates the central
// invariant for the interval-mode scheduler: a completed run does NOT
// block the next slot of the same trading day. The dedupe key is
// started_at vs the candidate slot time, so a run whose started_at
// is BEFORE the new slot is treated as "previous slot finished, fire
// the next one".
func TestTriggerDueFundsIntervalModeFiresEachSlot(t *testing.T) {
	stub := newStubSchedulerService()
	interval := 30
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	stub.funds = fundFixture(1)
	stub.profileByFundID["fund-A"] = marketcalendar.Profile{
		CalendarCode:            "US-XNAS",
		TimeZone:                "America/New_York",
		DecisionIntervalMinutes: &interval,
	}
	// Current candidate slot is 10:00 UTC. A previous slot at 09:30
	// already ran and completed.
	stub.triggerAt = now
	stub.tradingDate = now.Truncate(24 * time.Hour)
	stub.runs["fund-A"] = &repository.WorkflowRun{
		FundID:      "fund-A",
		Status:      "completed",
		TradingDate: stub.tradingDate,
		StartedAt:   sql.NullTime{Time: now.Add(-30 * time.Minute), Valid: true},
	}
	stub.startStatus = &api.WorkflowStatus{State: "running", Step: "macro_brief"}

	sched := &fundWorkflowScheduler{service: stub}
	snap := sched.triggerDueFunds(now)

	if len(stub.startCalls) != 1 {
		t.Fatalf("expected interval-mode to re-fire on next slot, got %d trigger calls", len(stub.startCalls))
	}
	if got := stub.startCalls[0].SlotTime; !got.Equal(now) {
		t.Fatalf("expected slot time = %s, got %s", now, got)
	}
	if snap.TriggeredCount != 1 {
		t.Fatalf("expected TriggeredCount=1, got %d", snap.TriggeredCount)
	}
}

// TestTriggerDueFundsIntervalModeSkipsAlreadyTriggeredSlot guards
// against double-firing the SAME slot inside a single trading day —
// the run.started_at >= candidate slot test must hold.
func TestTriggerDueFundsIntervalModeSkipsAlreadyTriggeredSlot(t *testing.T) {
	stub := newStubSchedulerService()
	interval := 30
	now := time.Date(2026, 5, 19, 10, 0, 5, 0, time.UTC)
	slotTime := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	stub.funds = fundFixture(1)
	stub.profileByFundID["fund-A"] = marketcalendar.Profile{
		CalendarCode:            "US-XNAS",
		TimeZone:                "America/New_York",
		DecisionIntervalMinutes: &interval,
	}
	stub.triggerAt = slotTime
	stub.tradingDate = now.Truncate(24 * time.Hour)
	stub.runs["fund-A"] = &repository.WorkflowRun{
		FundID:      "fund-A",
		Status:      "completed",
		TradingDate: stub.tradingDate,
		StartedAt:   sql.NullTime{Time: slotTime, Valid: true}, // already ran for THIS slot
	}

	sched := &fundWorkflowScheduler{service: stub}
	snap := sched.triggerDueFunds(now)

	if len(stub.startCalls) != 0 {
		t.Fatalf("expected scheduler to skip already-triggered slot, got %d trigger calls", len(stub.startCalls))
	}
	if snap.TriggeredCount != 0 {
		t.Fatalf("expected TriggeredCount=0, got %d", snap.TriggeredCount)
	}
}

// TestTriggerDueFundsIntervalModeBlocksOnRunningSlot ensures interval
// mode does not start a fresh slot while a prior slot's workflow is
// still in flight — running/paused is sticky.
func TestTriggerDueFundsIntervalModeBlocksOnRunningSlot(t *testing.T) {
	stub := newStubSchedulerService()
	interval := 30
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	stub.funds = fundFixture(1)
	stub.profileByFundID["fund-A"] = marketcalendar.Profile{
		CalendarCode:            "US-XNAS",
		TimeZone:                "America/New_York",
		DecisionIntervalMinutes: &interval,
	}
	stub.triggerAt = now
	stub.tradingDate = now.Truncate(24 * time.Hour)
	stub.runs["fund-A"] = &repository.WorkflowRun{
		FundID:      "fund-A",
		Status:      "running",
		TradingDate: stub.tradingDate,
		StartedAt:   sql.NullTime{Time: now.Add(-5 * time.Minute), Valid: true},
	}

	sched := &fundWorkflowScheduler{service: stub}
	snap := sched.triggerDueFunds(now)

	if len(stub.startCalls) != 0 {
		t.Fatalf("expected scheduler to block on running slot, got %d trigger calls", len(stub.startCalls))
	}
	if snap.TriggeredCount != 0 {
		t.Fatalf("expected TriggeredCount=0 while a run is in flight, got %d", snap.TriggeredCount)
	}
}
