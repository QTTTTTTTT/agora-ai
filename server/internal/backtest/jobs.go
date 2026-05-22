package backtest

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// JobStore is the in-memory ledger for async backtest runs. The
// API handler hands a Request to Submit, gets back a JobID, and
// the caller polls Get(JobID) for progress + final result.
//
// Why in-memory instead of DB-backed? Two reasons:
//   1. Backtest jobs are operator-driven and short-lived. Crashing
//      mid-run is acceptable — the operator just re-submits.
//   2. Persisting NavCurve + Trades to Postgres adds a non-trivial
//      schema (jobs / nav_points / trade_events) for marginal
//      durability gain. We defer that to a follow-up PR if real
//      operator workflows demand it.
//
// Concurrency: all reads/writes guarded by mu; the long-running
// goroutine that calls Engine.Run does so OUTSIDE the lock (the
// job's Progress object has its own mutex).
type JobStore struct {
	mu     sync.RWMutex
	jobs   map[string]*Job
	engine Engine
	now    func() time.Time
	// maxJobs caps the in-memory ledger. When exceeded the oldest
	// COMPLETED jobs are evicted; in-progress jobs are never
	// evicted because that'd leak goroutines. Default 50.
	maxJobs int
	// OnSubmit fires synchronously after a job is queued but BEFORE
	// the goroutine spins up. The adapter (Phase 2F) uses this to
	// persist the initial 'queued' row to Postgres. Returning an
	// error from OnSubmit aborts Submit and the job is removed
	// from the in-memory ledger — callers see the error as if the
	// store had refused them.
	OnSubmit func(job *Job) error
	// OnFinal fires AFTER the runner goroutine settles into a
	// terminal state (completed / failed / cancelled). The
	// adapter persists NAV + Trades + final metrics here. Errors
	// are logged-not-propagated since the runner already settled
	// — surfacing the error would be misleading.
	OnFinal func(job *Job)
}

// NewJobStore wires an in-memory store backed by the given Engine.
// The store owns the goroutines it spawns; the API handler should
// not call go-routines on its own.
func NewJobStore(engine Engine) *JobStore {
	return &JobStore{
		jobs:    make(map[string]*Job, 16),
		engine:  engine,
		now:     time.Now,
		maxJobs: 50,
	}
}

// Job is the per-submission record. Progress is exposed for
// polling; Result is only populated after Status transitions to
// "completed". Error is populated on "failed".
type Job struct {
	ID          string
	Request     Request
	SubmittedAt time.Time
	StartedAt   time.Time
	CompletedAt time.Time
	Progress    *Progress
	Result      *Result
	Err         error
	cancel      context.CancelFunc
}

// Submit enqueues a new backtest and starts the goroutine
// immediately. The returned Job has Status="queued" until the
// runner thread bumps it to "running" inside Engine.Run.
//
// Note: there is no separate worker pool — we trust the operator
// not to fire thousands of concurrent backtests. If that turns
// out to matter we can bolt a semaphore on here without changing
// the public API.
func (s *JobStore) Submit(ctx context.Context, req Request) (*Job, error) {
	if s == nil || s.engine == nil {
		return nil, errors.New("backtest: job store missing engine")
	}
	job := &Job{
		ID:          uuid.NewString(),
		Request:     req,
		SubmittedAt: s.now(),
		Progress:    &Progress{Status: "queued"},
	}
	jobCtx, cancel := context.WithCancel(context.Background())
	job.cancel = cancel
	// Propagate the submitter's deadline if any so a /backtests
	// POST with a 30s timeout doesn't keep running on the
	// backend for hours.
	if dl, ok := ctx.Deadline(); ok {
		jobCtx, cancel = context.WithDeadline(jobCtx, dl)
		job.cancel = cancel
	}
	s.mu.Lock()
	s.jobs[job.ID] = job
	s.evictIfNeededLocked()
	s.mu.Unlock()
	// The OnSubmit hook runs synchronously so a persistence
	// failure aborts the submit. Without that contract a flaky
	// DB would let in-memory state diverge from the journaled
	// audit trail.
	if s.OnSubmit != nil {
		if err := s.OnSubmit(job); err != nil {
			s.mu.Lock()
			delete(s.jobs, job.ID)
			s.mu.Unlock()
			job.cancel()
			return nil, err
		}
	}
	go s.run(jobCtx, job)
	return job, nil
}

// run is the background goroutine for one job.
func (s *JobStore) run(ctx context.Context, job *Job) {
	defer job.cancel()
	defer func() {
		if s.OnFinal != nil {
			s.OnFinal(job)
		}
	}()
	job.StartedAt = s.now()
	result, err := s.engine.Run(ctx, job.Request, job.Progress)
	job.CompletedAt = s.now()
	if err != nil {
		if errors.Is(err, ErrCancelled) {
			job.Progress.markStatus("cancelled", err)
		} else {
			job.Progress.markStatus("failed", err)
		}
		job.Err = err
		return
	}
	job.Result = result
	job.Progress.markStatus("completed", nil)
}

// Get returns the job snapshot suitable for the API handler.
// Returns nil when the ID isn't known.
func (s *JobStore) Get(id string) *Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.jobs[id]
}

// List returns all jobs filtered by fundID, newest first. Empty
// fundID returns every job.
func (s *JobStore) List(fundID string) []*Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		if fundID != "" && j.Request.FundID != fundID {
			continue
		}
		out = append(out, j)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].SubmittedAt.After(out[j].SubmittedAt)
	})
	return out
}

// Cancel triggers the job's cancellation and returns whether the
// job was found.
func (s *JobStore) Cancel(id string) bool {
	s.mu.RLock()
	job, ok := s.jobs[id]
	s.mu.RUnlock()
	if !ok || job.cancel == nil {
		return false
	}
	job.cancel()
	return true
}

// AddJobForTest is a test-only seam that injects a pre-built Job
// directly into the in-memory map without going through the full
// Submit pipeline (which would also fire OnSubmit / spawn a
// runner goroutine). Production code MUST NOT use this — it's
// exported only because the *_test.go files in cmd/server/ and
// the api package need to drive multi-job scenarios without
// spinning up a real engine.
//
// The added job is tracked by its ID; ID collisions overwrite
// the prior entry. The eviction policy still applies.
func (s *JobStore) AddJobForTest(job *Job) {
	if s == nil || job == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.ID] = job
	s.evictIfNeededLocked()
}

// evictIfNeededLocked drops the oldest completed/cancelled/failed
// jobs once the ledger exceeds maxJobs. Active jobs are never
// touched. Must be called with mu held.
func (s *JobStore) evictIfNeededLocked() {
	if len(s.jobs) <= s.maxJobs {
		return
	}
	type ageEntry struct {
		id  string
		at  time.Time
	}
	candidates := make([]ageEntry, 0, len(s.jobs))
	for id, j := range s.jobs {
		status := j.Progress.Snapshot().Status
		if status == "running" || status == "queued" {
			continue
		}
		t := j.CompletedAt
		if t.IsZero() {
			t = j.SubmittedAt
		}
		candidates = append(candidates, ageEntry{id, t})
	}
	if len(candidates) == 0 {
		return
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].at.Before(candidates[j].at)
	})
	drop := len(s.jobs) - s.maxJobs
	for i := 0; i < drop && i < len(candidates); i++ {
		delete(s.jobs, candidates[i].id)
	}
}
