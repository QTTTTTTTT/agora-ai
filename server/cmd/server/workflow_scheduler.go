package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/marketcalendar"
	"github.com/fundai/server/internal/repository"
)

// schedulerService is the narrow surface area the fund workflow
// scheduler needs from the workflow service. Defining it as an
// interface (rather than dereferencing *workflowServiceAdapter
// directly) lets unit tests plug in a stub that exercises the
// triggerDueFunds matrix — due/notDue × fresh/completed/failed/running
// — without spinning up a database, calendar, or LLM runtime.
//
// The production implementation is satisfied by workflowServiceAdapter
// via the small set of pass-through methods below; behaviour is
// unchanged.
type schedulerService interface {
	ListActiveFunds(ctx context.Context) ([]repository.Fund, error)
	GetWorkflowRun(ctx context.Context, fundID string, tradingDate time.Time) (*repository.WorkflowRun, error)
	NextWorkflowStart(now time.Time, profile marketcalendar.Profile) (time.Time, time.Time, error)
	SessionForDate(date time.Time, profile marketcalendar.Profile) (*marketcalendar.TradingSession, error)
	TradingProfileForFund(fund *repository.Fund) marketcalendar.Profile
	// TradingTriggerSlots returns the full list of interval slots
	// scheduled for the given session at the given cadence. Used by
	// the catch-up path to look back at slots that should have fired
	// while the scheduler was unavailable (server restart, failover,
	// leader churn) and re-attempt the most recent one if it's still
	// within the freshness window.
	TradingTriggerSlots(session *marketcalendar.TradingSession, intervalMinutes int) []time.Time
	// StartWorkflowForFund kicks off the daily decision workflow.
	// `slotTime` is the trigger time the scheduler intended for this
	// run; when interval mode is active, it pins the run's started_at
	// so the next scheduler tick can distinguish "ran for slot N"
	// from "ran for slot N+1". Pass the zero time for legacy
	// one-shot daily triggers (the implementation falls back to
	// time.Now in that case).
	StartWorkflowForFund(fund *repository.Fund, tradingDate, slotTime time.Time) (*api.WorkflowStatus, error)
}

const schedulerNextTradingDayLookahead = 26 * time.Hour

// schedulerMaxIdleDelay caps how long the scheduler loop sleeps between
// ticks even when the next computed trigger is far in the future. Acts as
// the floor on responsiveness to manual fund creation, lease handovers, and
// clock corrections; see F10.2.
const schedulerMaxIdleDelay = 10 * time.Minute

// clampLoopDelay enforces the [0, schedulerMaxIdleDelay] envelope on the
// scheduler's sleep interval. Negative inputs (next trigger already past)
// become 0 so the loop fires immediately; arbitrarily large inputs (e.g.
// "next session is Monday morning, 60h away") are clamped so the loop still
// wakes every schedulerMaxIdleDelay as a safety net. Pure function — kept
// separate so the F10.2 regression test can exercise both bounds without
// spinning real timers.
func clampLoopDelay(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > schedulerMaxIdleDelay {
		return schedulerMaxIdleDelay
	}
	return d
}

// leaderChecker reports whether the local replica currently owns the
// scheduler's distributed lease. A nil checker is treated as "always
// leader" so unit tests and single-replica setups don't need to wire
// up the lease manager.
type leaderChecker interface {
	IsLeader(name string) bool
}

// SchedulerLeaseName is the row id used in scheduler_leases for the
// fund workflow scheduler.
const SchedulerLeaseName = "workflow-scheduler"

type fundWorkflowScheduler struct {
	service schedulerService
	leader  leaderChecker
	stopCh  chan struct{}
	wakeCh  chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
	started bool

	// snapshotMu guards the most recent poll snapshot. Decoupled from
	// the lifecycle mutex (mu) so /api/admin/workflow/scheduler reads
	// can never block a Start/Stop transition, and vice versa.
	snapshotMu sync.RWMutex
	snapshot   FundSchedulerSnapshot

	// recoveryRetries records which (fund, trading-day, slot) tuples
	// this process has already auto-retried after a restart-induced
	// failure. Bounds the catch-up auto-recovery at ONE retry per
	// slot per process so a crashloop can't re-fire forever. See
	// claimRecoveryRetry for the per-tick prune logic.
	recoveryRetryMu sync.Mutex
	recoveryRetries map[string]struct{}
}

// FundSchedulerSnapshot is what the admin endpoint returns. It captures
// the most recent scheduler tick — what funds were looked at, the next
// trigger time the calendar produced for each, and any error/warning
// that came back from the per-fund decision.
//
// Operators read this to answer two questions:
//   - "did the scheduler actually evaluate fund X today?"
//   - "when will it fire next?"
//
// The snapshot is overwritten on every leader tick so it always reflects
// the latest evaluation; non-leader replicas keep an empty snapshot until
// they win leadership.
type FundSchedulerSnapshot struct {
	LastPollAt     time.Time              `json:"lastPollAt"`
	IsLeader       bool                   `json:"isLeader"`
	NextPollAt     time.Time              `json:"nextPollAt"`
	TotalActive    int                    `json:"totalActive"`
	TriggeredCount int                    `json:"triggeredCount"`
	Funds          []FundSchedulerStatus  `json:"funds"`
	Warnings       []string               `json:"warnings,omitempty"`
	LastError      string                 `json:"lastError,omitempty"`
}

// FundSchedulerStatus is one row in the snapshot — one per active fund
// the scheduler considered during its last tick.
type FundSchedulerStatus struct {
	FundID         string    `json:"fundId"`
	FundName       string    `json:"fundName,omitempty"`
	CalendarCode   string    `json:"calendarCode,omitempty"`
	TimeZone       string    `json:"timeZone,omitempty"`
	NextTradingDay string    `json:"nextTradingDay,omitempty"`
	NextTriggerAt  time.Time `json:"nextTriggerAt"`
	Due            bool      `json:"due"`
	Started        bool      `json:"started"`
	// LastStatus is the workflow_run status as observed at poll time
	// (empty if no run exists yet for the next trading day).
	LastStatus string `json:"lastStatus,omitempty"`
	// SkipReason records why the scheduler chose NOT to trigger this
	// fund on this tick. Empty means it either triggered or is simply
	// not yet due.
	SkipReason string `json:"skipReason,omitempty"`
	Error      string `json:"error,omitempty"`
}

func newFundWorkflowScheduler(service schedulerService) *fundWorkflowScheduler {
	return &fundWorkflowScheduler{
		service: service,
		stopCh:  make(chan struct{}),
		wakeCh:  make(chan struct{}, 1),
	}
}

// SetLeaderChecker installs the lease manager used to gate scheduler
// ticks. Must be called before Start to take effect.
func (s *fundWorkflowScheduler) SetLeaderChecker(checker leaderChecker) {
	if s == nil {
		return
	}
	s.leader = checker
}

func (s *fundWorkflowScheduler) isLeader() bool {
	if s == nil || s.leader == nil {
		return true
	}
	return s.leader.IsLeader(SchedulerLeaseName)
}

func (s *workflowServiceAdapter) StartBackgroundScheduler() {
	if s == nil || s.scheduler == nil {
		return
	}
	s.scheduler.Start()
}

func (s *workflowServiceAdapter) StopBackgroundScheduler() {
	if s == nil || s.scheduler == nil {
		return
	}
	s.scheduler.Stop()
}

// WakeScheduler nudges the background scheduler to re-evaluate funds on its
// next loop iteration. Safe to call on a nil receiver or before the scheduler
// has been started. Intended to be called from fund CRUD code paths so newly
// created funds don't have to wait for the next scheduled poll to be picked
// up — see F10.1.
func (s *workflowServiceAdapter) WakeScheduler() {
	if s == nil || s.scheduler == nil {
		return
	}
	s.scheduler.Wake()
}

func (s *fundWorkflowScheduler) Start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	if s.stopCh == nil {
		s.stopCh = make(chan struct{})
	}
	if s.wakeCh == nil {
		s.wakeCh = make(chan struct{}, 1)
	}
	stopCh := s.stopCh
	wakeCh := s.wakeCh
	s.started = true
	s.mu.Unlock()
	s.wg.Add(1)
	go s.loop(stopCh, wakeCh)
	s.wake()
}

func (s *fundWorkflowScheduler) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	stopCh := s.stopCh
	wakeCh := s.wakeCh
	s.started = false
	s.stopCh = nil
	s.wakeCh = nil
	s.mu.Unlock()
	if stopCh != nil {
		close(stopCh)
	}
	if wakeCh != nil {
		for {
			select {
			case <-wakeCh:
			default:
				s.wg.Wait()
				return
			}
		}
	}
	s.wg.Wait()
}

func (s *fundWorkflowScheduler) wake() {
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

// Wake nudges the scheduler loop to evaluate the world immediately on the
// next iteration instead of waiting out the current poll timer. Safe to call
// from any goroutine, on a nil receiver, or before Start. It is non-blocking
// and idempotent (a second Wake while one is already pending is a no-op).
//
// Use this after fund CRUD operations (create/update/delete/activate) so
// the newly created or reconfigured fund is picked up within milliseconds
// instead of sitting idle until the next scheduled poll (which may be 10+
// minutes away when no funds are due soon).
func (s *fundWorkflowScheduler) Wake() {
	if s == nil {
		return
	}
	s.mu.Lock()
	wakeCh := s.wakeCh
	s.mu.Unlock()
	if wakeCh == nil {
		return
	}
	select {
	case wakeCh <- struct{}{}:
	default:
	}
}

func (s *fundWorkflowScheduler) loop(stopCh, wakeCh <-chan struct{}) {
	defer s.wg.Done()
	for {
		now := time.Now()
		var nextAt time.Time
		if s.isLeader() {
			snap := s.triggerDueFunds(now)
			nextAt = s.nextTriggerTime(now)
			snap.NextPollAt = nextAt
			s.storeSnapshot(snap)
		} else {
			// Non-leaders poll for leadership without scanning funds or
			// hammering the database. The poll cadence is short enough
			// to keep failover quick (within a couple of lease ttls).
			nextAt = now.Add(15 * time.Second)
			s.storeSnapshot(FundSchedulerSnapshot{
				LastPollAt: now,
				NextPollAt: nextAt,
				IsLeader:   false,
				Warnings:   []string{"replica is not currently the scheduler leader"},
			})
		}
		delay := clampLoopDelay(time.Until(nextAt))
		slog.Info("workflow scheduler waiting", "nextTriggerAt", nextAt.Format(time.RFC3339), "delay", delay.String(), "isLeader", s.isLeader())
		timer := time.NewTimer(delay)
		select {
		case <-stopCh:
			timer.Stop()
			return
		case <-wakeCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	}
}

// Snapshot returns the latest scheduler tick result. Safe to call from
// any goroutine; if no tick has happened yet, returns a zero-valued
// snapshot with LastPollAt unset (the admin handler renders that as
// "scheduler has not polled yet" rather than an error).
func (s *fundWorkflowScheduler) Snapshot() FundSchedulerSnapshot {
	if s == nil {
		return FundSchedulerSnapshot{}
	}
	s.snapshotMu.RLock()
	defer s.snapshotMu.RUnlock()
	out := s.snapshot
	if out.Funds != nil {
		copied := make([]FundSchedulerStatus, len(out.Funds))
		copy(copied, out.Funds)
		out.Funds = copied
	}
	if out.Warnings != nil {
		copied := make([]string, len(out.Warnings))
		copy(copied, out.Warnings)
		out.Warnings = copied
	}
	return out
}

func (s *fundWorkflowScheduler) storeSnapshot(snap FundSchedulerSnapshot) {
	if s == nil {
		return
	}
	s.snapshotMu.Lock()
	s.snapshot = snap
	s.snapshotMu.Unlock()
}

func (s *fundWorkflowScheduler) nextTriggerTime(now time.Time) time.Time {
	if s.service == nil {
		return now.Add(10 * time.Minute)
	}
	funds, err := s.service.ListActiveFunds(context.Background())
	if err != nil || len(funds) == 0 {
		return now.Add(10 * time.Minute)
	}
	var next time.Time
	for i := range funds {
		nextAt, _, due, err := s.nextFundStart(now, &funds[i])
		if err != nil {
			continue
		}
		if due {
			return now
		}
		if next.IsZero() || nextAt.Before(next) {
			next = nextAt
		}
	}
	if next.IsZero() {
		return now.Add(10 * time.Minute)
	}
	return next
}

// triggerDueFunds walks every active fund, decides whether it is due to
// run, and starts the workflow when so. The returned snapshot records
// every per-fund decision (triggered / not due / errored / skipped) so
// the admin endpoint can later answer "what did the scheduler do at
// last poll?" without re-running the work.
//
// The function is deliberately single-pass and stateless — it doesn't
// remember anything across calls. Each tick is a clean re-evaluation of
// the current world, which keeps the failover story trivial (a new
// leader produces its own complete snapshot on its first tick).
func (s *fundWorkflowScheduler) triggerDueFunds(now time.Time) FundSchedulerSnapshot {
	snap := FundSchedulerSnapshot{
		LastPollAt: now,
		IsLeader:   true,
		Funds:      []FundSchedulerStatus{},
	}
	if s.service == nil {
		snap.Warnings = append(snap.Warnings, "workflow scheduler dependencies unavailable")
		return snap
	}
	funds, err := s.service.ListActiveFunds(context.Background())
	if err != nil {
		slog.Error("workflow scheduler failed to list funds", "error", err)
		snap.LastError = err.Error()
		return snap
	}
	snap.TotalActive = len(funds)

	for i := range funds {
		fund := &funds[i]
		profile := decodeFundMarketProfile(fund.Config)
		row := FundSchedulerStatus{
			FundID:       fund.ID,
			FundName:     fund.Name,
			CalendarCode: profile.CalendarCode,
			TimeZone:     profile.TimeZone,
		}

		triggerAt, tradingDate, due, err := s.nextFundStart(now, fund)
		if err != nil {
			slog.Warn("workflow scheduler skipped fund", "fundId", fund.ID, "error", err)
			row.Error = err.Error()
			row.SkipReason = "next-trigger-error"
			snap.Funds = append(snap.Funds, row)
			continue
		}
		row.NextTriggerAt = triggerAt
		row.NextTradingDay = tradingDate.Format("2006-01-02")
		row.Due = due

		// Best-effort observability: read any existing run for the
		// computed trading day so the operator UI shows whether the
		// last attempt succeeded/failed/etc. A failure here is
		// non-fatal — we still proceed with the trigger decision.
		if run, runErr := s.service.GetWorkflowRun(context.Background(), fund.ID, tradingDate); runErr == nil && run != nil {
			row.LastStatus = strings.ToLower(strings.TrimSpace(run.Status))
		}

		if !due {
			if row.SkipReason == "" {
				row.SkipReason = "not-yet-due"
			}
			snap.Funds = append(snap.Funds, row)
			continue
		}

		status, err := s.service.StartWorkflowForFund(fund, tradingDate, triggerAt)
		if err != nil {
			slog.Error("workflow scheduler failed to start workflow", "fundId", fund.ID, "tradingDate", tradingDate.Format("2006-01-02"), "error", err)
			row.Error = err.Error()
			row.SkipReason = "start-error"
			snap.Funds = append(snap.Funds, row)
			continue
		}
		row.Started = true
		if status != nil {
			row.LastStatus = strings.ToLower(strings.TrimSpace(status.State))
		}
		snap.TriggeredCount++

		isHalfDay := false
		if session, sessionErr := s.service.SessionForDate(tradingDate, s.service.TradingProfileForFund(fund)); sessionErr == nil && session != nil {
			isHalfDay = session.IsHalfDay
		}
		slog.Info(
			"workflow scheduler triggered workflow",
			"fundId", fund.ID,
			"tradingDate", tradingDate.Format("2006-01-02"),
			"resolvedTradingDate", tradingDate.Format("2006-01-02"),
			"calendarCode", profile.CalendarCode,
			"timeZone", profile.TimeZone,
			"nextTriggerAt", triggerAt.Format(time.RFC3339),
			"isHalfDay", isHalfDay,
			"state", row.LastStatus,
		)
		snap.Funds = append(snap.Funds, row)
	}
	return snap
}

func (s *fundWorkflowScheduler) nextFundStart(now time.Time, fund *repository.Fund) (time.Time, time.Time, bool, error) {
	if s == nil || s.service == nil || fund == nil {
		return time.Time{}, time.Time{}, false, errors.New("workflow scheduler unavailable")
	}
	profile := s.service.TradingProfileForFund(fund)
	triggerAt, tradingDate, err := s.service.NextWorkflowStart(now, profile)
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	run, err := s.service.GetWorkflowRun(context.Background(), fund.ID, tradingDate)
	switch {
	case errors.Is(err, repository.ErrNotFound) || run == nil:
		run = nil
	case err != nil:
		return time.Time{}, time.Time{}, false, err
	}

	// Interval mode: the same trading_date can host many slots, so we
	// dedupe by comparing the candidate slot to the existing run's
	// started_at. If the run already started for THIS slot (or later)
	// we must roll forward to the NEXT slot; the lookahead window is
	// just the interval itself, not the legacy 26h next-day jump.
	if profile.DecisionIntervalMinutes != nil {
		// A run that is actively in flight blocks any further triggers
		// regardless of slot — let it finish before kicking off the next.
		if workflowRunActivelyRunning(run) {
			intervalDelta := time.Duration(*profile.DecisionIntervalMinutes) * time.Minute
			if intervalDelta <= 0 {
				intervalDelta = time.Minute
			}
			lookFrom := triggerAt.Add(intervalDelta)
			if run != nil && run.StartedAt.Valid {
				lookFrom = laterOf(lookFrom, run.StartedAt.Time.Add(intervalDelta))
			}
			nextAt, nextTradingDate, err := s.service.NextWorkflowStart(lookFrom, profile)
			if err != nil {
				return time.Time{}, time.Time{}, false, err
			}
			return nextAt, nextTradingDate, false, nil
		}

		// Catch-up path: look back at the most recent slot that
		// SHOULD have fired but didn't (e.g. because the previous
		// process died, leader churned, or the scheduler missed a
		// tick). If that slot is within one interval period of now
		// AND today's run hasn't claimed it, fire it immediately
		// instead of waiting for the next future slot. This is the
		// fix for "10:30 / 14:00 silently skipped" — without it, a
		// missed slot waits 28+ minutes for the next cadence
		// cycle. Bounding the look-back at one interval period
		// avoids replaying market-stale slots from hours ago.
		//
		// Note that `run` above was loaded against the FUTURE
		// trading date returned by NextWorkflowStart (typically
		// the next business day when we're past today's last
		// slot). The catch-up path explicitly re-reads today's
		// run inside findCatchUpSlot — without that, the dedupe
		// check would always see run=nil and fire on every tick.
		if catchUpSlot, catchUpDate, ok := s.findCatchUpSlot(now, fund.ID, profile); ok {
			slog.Info(
				"workflow scheduler firing catch-up slot",
				"fundId", fund.ID,
				"missedSlot", catchUpSlot.Format(time.RFC3339),
				"firedAt", now.Format(time.RFC3339),
				"lagSeconds", int(now.Sub(catchUpSlot).Seconds()),
			)
			return catchUpSlot, catchUpDate, true, nil
		}

		if !workflowRunStartedAtOrAfter(run, triggerAt) {
			return triggerAt, tradingDate, !triggerAt.After(now), nil
		}
		// The current candidate slot is already taken; look one minute
		// past it (or past started_at, whichever is later) to find the
		// next slot. Capping lookahead at +1 min avoids skipping a
		// fast-cadence slot that lives in the same minute as triggerAt.
		lookFrom := triggerAt.Add(time.Minute)
		if run != nil && run.StartedAt.Valid {
			lookFrom = laterOf(lookFrom, run.StartedAt.Time.Add(time.Minute))
		}
		nextAt, nextTradingDate, err := s.service.NextWorkflowStart(lookFrom, profile)
		if err != nil {
			return time.Time{}, time.Time{}, false, err
		}
		return nextAt, nextTradingDate, false, nil
	}

	// Legacy one-shot daily mode.
	if run == nil {
		return triggerAt, tradingDate, !triggerAt.After(now), nil
	}
	if schedulerRunNeedsTrigger(run) {
		return triggerAt, tradingDate, !triggerAt.After(now), nil
	}
	nextAt, nextTradingDate, err := s.service.NextWorkflowStart(triggerAt.Add(schedulerNextTradingDayLookahead), profile)
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	return nextAt, nextTradingDate, false, nil
}

func schedulerRunNeedsTrigger(run *repository.WorkflowRun) bool {
	if run == nil {
		return true
	}
	status := strings.ToLower(strings.TrimSpace(run.Status))
	if workflowRunHasProgress(run) {
		return false
	}
	switch status {
	case "", "pending", "failed", "cancelled":
		return true
	case "paused", "rejected", "running", "completed":
		return false
	default:
		return false
	}
}

// workflowRunStartedAtOrAfter reports whether the existing run was
// claimed for the given slot (or a later one). Used by interval-mode
// dedupe to skip slots that have already been triggered without
// re-querying the workflow service.
func workflowRunStartedAtOrAfter(run *repository.WorkflowRun, slot time.Time) bool {
	if run == nil || !run.StartedAt.Valid {
		return false
	}
	return !run.StartedAt.Time.Before(slot)
}

// workflowRunActivelyRunning returns true while the workflow is mid-run
// (running or paused awaiting human input). Interval mode must respect
// in-flight runs and refuse to trigger another decision on top of them
// so we don't fork the same fund's plan stream.
func workflowRunActivelyRunning(run *repository.WorkflowRun) bool {
	if run == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(run.Status)) {
	case "running", "paused":
		return true
	default:
		return false
	}
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// findCatchUpSlot looks at the most recent past slot for the fund's
// local trading day and decides whether it should be fired now as a
// catch-up. Returns the slot, its trading date (in storage form),
// and true when a catch-up is warranted; false in every other case.
//
// A slot is catch-up-eligible when ALL of the following hold:
//  1. The slot is in the past (relative to now, with the existing
//     TriggerSlotGrace honoured so we don't double-fire a slot that
//     the main path is about to pick up).
//  2. The slot is within the last `intervalMinutes` window. Older
//     missed slots are intentionally NOT replayed — by the time
//     we've been down for a full interval, the market data the
//     LLM would chew on is stale enough that re-running on it
//     adds more noise than signal. We'd rather skip and wait for
//     the next live slot.
//  3. There is no existing workflow_run for today, OR the existing
//     run.started_at is strictly before the slot (i.e. the run was
//     for an earlier slot and the recent one was never claimed).
//
// The session lookup uses the same calendar profile the main next-
// slot path uses, so trading-day boundaries / holidays / half-days
// are honoured the same way. A missing or unreadable session is
// treated as "no catch-up available" — the main path's logic still
// applies.
func (s *fundWorkflowScheduler) findCatchUpSlot(now time.Time, fundID string, profile marketcalendar.Profile) (time.Time, time.Time, bool) {
	if profile.DecisionIntervalMinutes == nil || *profile.DecisionIntervalMinutes <= 0 {
		return time.Time{}, time.Time{}, false
	}
	intervalMinutes := *profile.DecisionIntervalMinutes
	loc, _ := time.LoadLocation(profile.TimeZone)
	if loc == nil {
		loc = time.UTC
	}
	localNow := now.In(loc)
	// storageDate semantics: SessionForDate expects a UTC-midnight
	// timestamp whose Y/M/D match the LOCAL trading day. Passing a
	// local-midnight timestamp converts wrong (e.g. 2026-05-22 00:00
	// BJT == 2026-05-21 16:00 UTC, and the calendar treats it as
	// the previous trading day's session). Mirror the exact
	// transform used by NextWorkflowStart so catch-up looks at the
	// SAME day as the main forward path.
	todayStorage := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, time.UTC)

	session, err := s.service.SessionForDate(todayStorage, profile)
	if err != nil || session == nil {
		slog.Debug("catch-up: no session for today", "tz", profile.TimeZone, "today", todayStorage.Format("2006-01-02"), "err", err)
		return time.Time{}, time.Time{}, false
	}
	slots := s.service.TradingTriggerSlots(session, intervalMinutes)
	if len(slots) == 0 {
		slog.Debug("catch-up: no slots in session", "tz", profile.TimeZone, "today", todayStorage.Format("2006-01-02"))
		return time.Time{}, time.Time{}, false
	}

	// Find the most recent slot that's already in the past (with
	// the trigger grace honoured so the main forward path keeps
	// owning "just-fired" slots).
	cutoff := now.Add(-marketcalendar.TriggerSlotGrace)
	freshness := time.Duration(intervalMinutes) * time.Minute
	var candidate time.Time
	for i := len(slots) - 1; i >= 0; i-- {
		slot := slots[i]
		if slot.After(cutoff) {
			continue
		}
		// Reject anything older than the freshness window so we
		// don't replay stale slots on a long-downtime recovery.
		if now.Sub(slot) > freshness {
			break
		}
		candidate = slot
		break
	}
	if candidate.IsZero() {
		return time.Time{}, time.Time{}, false
	}

	// Re-read TODAY's workflow_run row (the caller's `run` was
	// loaded against NextWorkflowStart's tradingDate, which is the
	// NEXT business day when we're past today's last slot — so
	// using it here would always see run=nil and re-fire on every
	// tick). With the correct row in hand, two dedupe rules
	// apply:
	//   1. status in {running, paused} → already in-flight,
	//      don't double-fire even if started_at is older than
	//      the candidate slot.
	//   2. started_at >= candidate → the slot was already
	//      claimed (whether the run later succeeded, failed, or
	//      is still going). One claim per slot is the contract.
	//
	// Exception to rule 2 ("recovery retry"): when the run is in
	// status=failed AND the failure was stamped by the
	// restart-recovery code (step_results.{step}.error has the
	// "recovery: " prefix), we allow ONE re-fire of the same slot
	// per process so a deploy that interrupted the orchestrator
	// doesn't leave the day stuck on a permanent "failed" tile.
	// ClaimStart already permits overwriting failed rows, so the
	// re-fire just needs to get past the dedupe gate here. The
	// per-process bound prevents a crash loop from re-firing
	// forever; the operator can still force a retry via
	// /api/admin/workflow/scheduler/trigger if they really mean it.
	todayRun, runErr := s.service.GetWorkflowRun(context.Background(), fundID, todayStorage)
	switch {
	case errors.Is(runErr, repository.ErrNotFound):
		todayRun = nil
	case runErr != nil:
		// Treat read errors as "don't catch up" — better to
		// skip a slot than spin on a transient DB hiccup.
		return time.Time{}, time.Time{}, false
	}
	if workflowRunActivelyRunning(todayRun) {
		return time.Time{}, time.Time{}, false
	}
	if todayRun != nil && todayRun.StartedAt.Valid && !todayRun.StartedAt.Time.Before(candidate) {
		if !workflowRunIsRecoveryFailure(todayRun) {
			return time.Time{}, time.Time{}, false
		}
		if !s.claimRecoveryRetry(fundID, todayStorage, candidate, now) {
			return time.Time{}, time.Time{}, false
		}
		slog.Info("catch-up: retrying recovery-failed slot",
			"fund_id", fundID,
			"trading_date", todayStorage.Format("2006-01-02"),
			"slot", candidate.Format(time.RFC3339),
		)
	}

	return candidate, todayStorage, true
}

// workflowRunIsRecoveryFailure returns true when the run is a
// "failed because the server restarted mid-step" row. Genuine
// workflow failures (LLM error, data outage, etc.) are intentionally
// NOT retried automatically — those need a human to look at the
// audit log first.
func workflowRunIsRecoveryFailure(run *repository.WorkflowRun) bool {
	if run == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(run.Status), "failed") {
		return false
	}
	// Recovery-style failures are stamped by markRunInterrupted in
	// workflow_recovery.go with a "recovery: " error prefix. The
	// step_results map can contain multiple entries (one per step
	// the workflow touched); we accept if ANY entry is a recovery
	// failure, since the failed step's key is what gets the prefix.
	for _, step := range decodeWorkflowStepResults(run.StepResults) {
		if strings.HasPrefix(strings.TrimSpace(step.Error), "recovery: ") {
			return true
		}
	}
	return false
}

// claimRecoveryRetry returns true the FIRST time this process is
// asked to retry the given (fund, trading day, slot), and false on
// every subsequent call. Bookkeeping is process-local — the
// rationale is that if the process restarts and forgets its slot
// map, the new process is by definition not the one that just
// restart-failed the run, so granting it a fresh retry budget is
// safe. The map is pruned of stale trading dates on each call so it
// can't grow without bound for long-running processes.
func (s *fundWorkflowScheduler) claimRecoveryRetry(fundID string, tradingDate, slot, now time.Time) bool {
	if s == nil {
		return false
	}
	s.recoveryRetryMu.Lock()
	defer s.recoveryRetryMu.Unlock()
	if s.recoveryRetries == nil {
		s.recoveryRetries = make(map[string]struct{})
	}
	// Use the caller-supplied `now` so the prune window stays
	// consistent with whatever clock the scheduler is operating
	// against (production = wall clock; tests = injected time).
	// Falling back to time.Now() would aggressively delete
	// dedupe rows whenever the test wall-clock is past
	// tradingDate+1d, leaking the slot back through every tick.
	cutoffBase := now
	if cutoffBase.IsZero() {
		cutoffBase = time.Now()
	}
	cutoff := cutoffBase.UTC().AddDate(0, 0, -1)
	for key := range s.recoveryRetries {
		parts := strings.SplitN(key, "|", 3)
		if len(parts) != 3 {
			delete(s.recoveryRetries, key)
			continue
		}
		ts, err := time.Parse("2006-01-02", parts[1])
		if err != nil || ts.Before(cutoff) {
			delete(s.recoveryRetries, key)
		}
	}
	key := fundID + "|" + tradingDate.Format("2006-01-02") + "|" + slot.Format(time.RFC3339)
	if _, seen := s.recoveryRetries[key]; seen {
		return false
	}
	s.recoveryRetries[key] = struct{}{}
	return true
}


func workflowRunHasProgress(run *repository.WorkflowRun) bool {
	if run == nil {
		return false
	}
	if run.StartedAt.Valid || run.CompletedAt.Valid {
		return true
	}
	if run.CurrentStep.Valid && strings.TrimSpace(run.CurrentStep.String) != "" {
		return true
	}
	return len(decodeWorkflowStepResults(run.StepResults)) > 0
}
