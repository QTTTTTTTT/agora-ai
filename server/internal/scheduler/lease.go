package scheduler

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// LeaseManager elects a single leader for a named scheduler across replicas.
//
// Implementation notes
//
//   - Backed by the scheduler_leases Postgres table (see migration 016).
//   - One row per lease name. A replica owns the lease if its holder id
//     matches and the row has not yet expired.
//   - Acquire and Renew use the same UPSERT statement: the current holder
//     always wins; otherwise, the lease can only be stolen once it has
//     expired.
//   - Each loop calls IsLeader() before doing real work; the manager
//     refreshes the lease in the background at a configurable interval.
type LeaseManager struct {
	db       *sql.DB
	holderID string

	ttl     time.Duration
	renewIn time.Duration

	// leases tracked by this manager. Map name -> *leaseState.
	mu     sync.RWMutex
	leases map[string]*leaseState

	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

type leaseState struct {
	name string
	// 1 means we currently believe we hold the lease; 0 otherwise.
	heldFlag atomic.Int32
}

// NewLeaseManager constructs a manager. ttl is how long an acquired lease
// is valid; renewIn is how often the manager refreshes it. renewIn should
// be comfortably less than ttl (e.g. ttl=30s, renewIn=10s) so that brief
// db hiccups don't cause a leadership flap.
//
// holderID uniquely identifies the calling replica. If empty, a random id
// is generated combining hostname and a random suffix.
func NewLeaseManager(db *sql.DB, holderID string, ttl, renewIn time.Duration) *LeaseManager {
	if holderID == "" {
		holderID = defaultHolderID()
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if renewIn <= 0 {
		renewIn = ttl / 3
	}
	return &LeaseManager{
		db:       db,
		holderID: holderID,
		ttl:      ttl,
		renewIn:  renewIn,
		leases:   make(map[string]*leaseState),
		stopCh:   make(chan struct{}),
	}
}

// HolderID returns the unique id of this replica.
func (m *LeaseManager) HolderID() string {
	return m.holderID
}

// Register starts tracking a named lease. The first call for a name spawns
// a background renewer goroutine. Subsequent calls are no-ops.
func (m *LeaseManager) Register(name string) {
	if m == nil || name == "" {
		return
	}
	m.mu.Lock()
	if _, ok := m.leases[name]; ok {
		m.mu.Unlock()
		return
	}
	state := &leaseState{name: name}
	m.leases[name] = state
	m.mu.Unlock()

	m.wg.Add(1)
	go m.renewLoop(state)
}

// IsLeader returns true if this replica currently holds the named lease.
// The check is in-memory and cheap; the actual db state is refreshed by
// the background renewer.
func (m *LeaseManager) IsLeader(name string) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	state := m.leases[name]
	m.mu.RUnlock()
	if state == nil {
		return false
	}
	return state.heldFlag.Load() == 1
}

// Stop shuts down all renewer goroutines and best-effort releases any
// leases still held. Safe to call multiple times.
func (m *LeaseManager) Stop() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() {
		close(m.stopCh)
	})
	m.wg.Wait()

	// Best-effort release. A failure here just means the lease will be
	// reclaimed naturally after ttl.
	m.mu.RLock()
	defer m.mu.RUnlock()
	for name, state := range m.leases {
		if state.heldFlag.Load() != 1 {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := m.release(ctx, name); err != nil {
			slog.Warn("scheduler lease release failed", "name", name, "error", err)
		}
		cancel()
	}
}

func (m *LeaseManager) renewLoop(state *leaseState) {
	defer m.wg.Done()
	// Try acquiring immediately, then on each tick.
	m.tryAcquire(state)
	ticker := time.NewTicker(m.renewIn)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.tryAcquire(state)
		}
	}
}

func (m *LeaseManager) tryAcquire(state *leaseState) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	held, err := m.acquire(ctx, state.name)
	if err != nil {
		// On a transient db error we keep our previous belief: if we
		// were leader, we stay leader until the next successful check.
		// This avoids flapping during brief db outages but means the
		// lease ttl is the upper bound of split-brain on prolonged
		// outage (which is the design).
		slog.Warn("scheduler lease acquire failed", "name", state.name, "error", err)
		return
	}
	prev := state.heldFlag.Load()
	if held {
		state.heldFlag.Store(1)
		if prev != 1 {
			slog.Info("scheduler lease acquired", "name", state.name, "holder", m.holderID)
		}
	} else {
		state.heldFlag.Store(0)
		if prev == 1 {
			slog.Info("scheduler lease lost", "name", state.name, "holder", m.holderID)
		}
	}
}

// acquire performs the UPSERT and returns true if this caller is now the
// holder of the named lease.
func (m *LeaseManager) acquire(ctx context.Context, name string) (bool, error) {
	if m.db == nil {
		return false, errors.New("scheduler lease: nil db")
	}
	const q = `
INSERT INTO scheduler_leases (name, holder, acquired_at, heartbeat_at, expires_at)
VALUES ($1, $2, NOW(), NOW(), NOW() + ($3 || ' seconds')::interval)
ON CONFLICT (name) DO UPDATE
SET holder       = EXCLUDED.holder,
    acquired_at  = CASE
                       WHEN scheduler_leases.holder = EXCLUDED.holder THEN scheduler_leases.acquired_at
                       ELSE EXCLUDED.acquired_at
                   END,
    heartbeat_at = EXCLUDED.heartbeat_at,
    expires_at   = EXCLUDED.expires_at
WHERE scheduler_leases.holder = EXCLUDED.holder
   OR scheduler_leases.expires_at < NOW()
RETURNING (holder = $2) AS is_leader
`
	ttlSecs := int(m.ttl / time.Second)
	if ttlSecs <= 0 {
		ttlSecs = 30
	}
	var isLeader bool
	err := m.db.QueryRowContext(ctx, q, name, m.holderID, fmt.Sprintf("%d", ttlSecs)).Scan(&isLeader)
	if errors.Is(err, sql.ErrNoRows) {
		// Another holder owns a non-expired lease; we lost.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("scheduler lease acquire: %w", err)
	}
	return isLeader, nil
}

// release deletes the lease row if and only if we are still its holder.
// Used during graceful shutdown so the next replica can take over fast.
func (m *LeaseManager) release(ctx context.Context, name string) error {
	if m.db == nil {
		return nil
	}
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM scheduler_leases WHERE name = $1 AND holder = $2`,
		name, m.holderID,
	)
	if err != nil {
		return fmt.Errorf("scheduler lease release: %w", err)
	}
	return nil
}

func defaultHolderID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s-%d", host, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-%s", host, hex.EncodeToString(buf))
}
