package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/fundai/server/internal/repository"
)

// ActivityRetentionLeaseName is the scheduler_leases row id used to gate
// the activity retention loop. Only one server in the pool runs it.
const ActivityRetentionLeaseName = "activity-retention"

// activityRetentionLoop deletes workflow_activity_events rows that have
// fallen outside their fund's retention horizon (funds.config
// activityRetentionDays, 1..10 days, default 7).
//
// It runs once per (configurable) interval — the panel doesn't need
// minute-fresh cleanup: the disk cost of "one extra hour of events" is
// negligible compared to the cost of waking up every replica every
// minute to chew through every fund.
//
// The loop is leader-gated. On a follower it ticks idly and re-checks
// leadership; on the leader it walks ListActive(), parses each fund's
// retention setting (falling back to DefaultActivityRetentionDays for
// unconfigured funds), and runs WorkflowActivityRepo.DeleteOlderThan
// for each one. A per-fund failure is logged but does not abort the
// pass — one bad fund must not block the rest.
type activityRetentionLoop struct {
	fundRepo *repository.FundRepo
	repo     *repository.WorkflowActivityRepo
	leader   leaderChecker
	interval time.Duration

	stopCh  chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
	started bool
}

// newActivityRetentionLoop wires the retention sweep with safe
// production defaults. interval defaults to 1h; this is short enough
// that a "delete now" operator action takes effect within the hour
// without our needing an admin endpoint, and long enough that the
// cleanup overhead is invisible across hundreds of funds.
func newActivityRetentionLoop(fundRepo *repository.FundRepo, repo *repository.WorkflowActivityRepo) *activityRetentionLoop {
	return &activityRetentionLoop{
		fundRepo: fundRepo,
		repo:     repo,
		interval: time.Hour,
		stopCh:   make(chan struct{}),
	}
}

// SetLeaderChecker wires the distributed leader-election check so only
// one replica runs the sweep. The loop tolerates a nil checker (the
// single-binary smoke flow runs without leases) and behaves as a
// permanent leader in that case.
func (l *activityRetentionLoop) SetLeaderChecker(checker leaderChecker) {
	if l == nil {
		return
	}
	l.leader = checker
}

func (l *activityRetentionLoop) isLeader() bool {
	if l == nil || l.leader == nil {
		return true
	}
	return l.leader.IsLeader(ActivityRetentionLeaseName)
}

// Start launches the background goroutine. Idempotent: calling Start
// twice is a no-op. The first sweep runs immediately so a freshly
// deployed cluster doesn't wait a full interval before honouring a
// just-configured retention setting.
func (l *activityRetentionLoop) Start() {
	if l == nil || l.repo == nil || l.fundRepo == nil {
		return
	}
	l.mu.Lock()
	if l.started {
		l.mu.Unlock()
		return
	}
	if l.stopCh == nil {
		l.stopCh = make(chan struct{})
	}
	stopCh := l.stopCh
	l.started = true
	l.wg.Add(1)
	l.mu.Unlock()

	go func() {
		defer l.wg.Done()
		// Initial sweep deferred ~30s after boot so the leader-election
		// lease has a chance to settle and we don't double up with the
		// startup migrations.
		warmup := time.NewTimer(30 * time.Second)
		select {
		case <-stopCh:
			warmup.Stop()
			return
		case <-warmup.C:
		}
		l.runOnce()

		ticker := time.NewTicker(l.interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				l.runOnce()
			}
		}
	}()
}

// runOnce is the body of a single sweep. Exported for tests via a
// thin wrapper in *_test.go so we can drive it deterministically.
func (l *activityRetentionLoop) runOnce() {
	if !l.isLeader() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	funds, err := l.fundRepo.ListActive(ctx)
	if err != nil {
		slog.Warn("activity retention: list funds failed", "error", err)
		return
	}
	now := time.Now().UTC()
	totalDeleted := int64(0)
	failures := 0
	for i := range funds {
		fund := funds[i]
		profile := decodeFundMarketProfile(fund.Config)
		days := resolveActivityRetentionDays(profile)
		if days <= 0 {
			days = DefaultActivityRetentionDays
		}
		cutoff := now.AddDate(0, 0, -days)
		deleted, err := l.repo.DeleteOlderThan(ctx, fund.ID, cutoff)
		if err != nil {
			failures++
			slog.Warn("activity retention: delete failed",
				"fund_id", fund.ID,
				"cutoff", cutoff,
				"error", err,
			)
			continue
		}
		totalDeleted += deleted
	}
	slog.Info("activity retention pass",
		"funds_seen", len(funds),
		"rows_deleted", totalDeleted,
		"failures", failures,
	)
}

// Stop halts the goroutine and waits for it to finish. Safe to call
// multiple times.
func (l *activityRetentionLoop) Stop() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if !l.started {
		l.mu.Unlock()
		return
	}
	stopCh := l.stopCh
	l.stopCh = nil
	l.started = false
	l.mu.Unlock()
	close(stopCh)
	l.wg.Wait()
}
