package main

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"
)

// Sprint 3 / M4: memory 归档 nightly job.
//
// 设计思路：
//
//   - layer='daily' 是最常 burst 的层（每个 fund 每天一行 + 每个
//     agent 每天一行）。90 天后这些行对实时决策几乎无意义 — PM 只
//     看 14 天内的；reflection 用 30 天窗口。让它们继续待在 hot
//     table 里只会拖慢 ListByFund 的全表扫描成本。
//
//   - 但是直接 DELETE 是不可逆的、丢审计、丢回溯训练数据。所以
//     先把行原子搬到 memories_archive，再删原表行。两者在同一个事务
//     里完成，所以 crash 不会导致 "归档了一半"。
//
//   - 归档窗口 = 90 天可配置；先用一个常量，未来按 fund 配也可加。
//
//   - leader-gated：多副本下只有一个 worker 真正搬数据，否则会有重复
//     archived_at + delete race。
//
//   - 一次最多搬 batchSize 行 ➜ 避免一次性锁太长。每个 cron tick
//     滚动直到这一轮无更多 due 行为止。

const (
	MemoryArchiveLeaseName = "memory-archive"

	defaultMemoryArchiveAgeDays = 90
	defaultMemoryArchiveBatch   = 500
)

type memoryArchiveLoop struct {
	db       *sql.DB
	leader   leaderChecker
	interval time.Duration
	ageDays  int
	batch    int

	stopCh  chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
	started bool
}

func newMemoryArchiveLoop(db *sql.DB) *memoryArchiveLoop {
	return &memoryArchiveLoop{
		db:       db,
		interval: 12 * time.Hour, // 每天最多归档两次，足够
		ageDays:  defaultMemoryArchiveAgeDays,
		batch:    defaultMemoryArchiveBatch,
		stopCh:   make(chan struct{}),
	}
}

func (l *memoryArchiveLoop) SetLeaderChecker(checker leaderChecker) {
	if l == nil {
		return
	}
	l.leader = checker
}

func (l *memoryArchiveLoop) isLeader() bool {
	if l == nil || l.leader == nil {
		return true
	}
	return l.leader.IsLeader(MemoryArchiveLeaseName)
}

func (l *memoryArchiveLoop) Start() {
	if l == nil || l.db == nil {
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
		// 5 分钟 warmup — 启动期不抢资源。
		warmup := time.NewTimer(5 * time.Minute)
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

func (l *memoryArchiveLoop) Stop() {
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

// runOnce 是一轮全 fund / 全 layer 滚动归档。每轮反复调用 archiveBatch
// 直到这一轮没有更多 due 行为止；这样可以摊薄大积压的归档负担。
func (l *memoryArchiveLoop) runOnce() {
	if l == nil || l.db == nil || !l.isLeader() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cutoff := time.Now().UTC().AddDate(0, 0, -l.ageDays)
	totalArchived := 0
	for {
		moved, err := l.archiveBatch(ctx, cutoff, l.batch)
		if err != nil {
			slog.Warn("memory archive: batch failed",
				"cutoff", cutoff,
				"err", err,
			)
			break
		}
		if moved == 0 {
			break
		}
		totalArchived += moved
	}
	if totalArchived > 0 {
		slog.Info("memory archive pass",
			"cutoff", cutoff.Format("2006-01-02"),
			"rows_archived", totalArchived,
		)
	}
}

// archiveBatch 把单批 daily layer 行原子地搬到 archive 表然后删原表。
// 返回搬过的行数；返回 0 表示没有更多 due 行。
//
// 我们用一个 INSERT … SELECT … RETURNING + DELETE 在同一个 tx 里完成，
// 而不是 client-side 先查后插再删 — 那样在 batch 之间会有 race，
// 同一行可能被双副本同时见到。Postgres 的事务隔离 + leader-gated
// 双重保险。
//
// 只归档 layer='daily'。layer='agent' 同样可压（同样按 90d），但更
// 保守一些先只归 fund-level daily summary；agent layer 在 prompt 里
// 还在用 7 天窗口，14 天外就已经被 collectRecentLessonContexts 过滤
// 掉了，hot table 体积影响小。后续如需要扩展把 layer IN ('daily','agent')。
func (l *memoryArchiveLoop) archiveBatch(ctx context.Context, cutoff time.Time, batch int) (int, error) {
	if batch <= 0 {
		batch = defaultMemoryArchiveBatch
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	const moveSQL = `
WITH due AS (
    SELECT id
      FROM memories
     WHERE layer = 'daily'
       AND created_at < $1
     ORDER BY created_at ASC
     LIMIT $2
     FOR UPDATE SKIP LOCKED
), moved AS (
    INSERT INTO memories_archive (
        id, fund_id, agent_id, layer, title, content,
        trading_date, tags, owner_user_id, visibility, sensitivity,
        origin_kind, source_listing_id, created_at, updated_at
    )
    SELECT m.id, m.fund_id, m.agent_id, m.layer, m.title, m.content,
           m.trading_date, m.tags, m.owner_user_id, m.visibility, m.sensitivity,
           m.origin_kind, m.source_listing_id, m.created_at, m.updated_at
      FROM memories m
      JOIN due ON due.id = m.id
    ON CONFLICT (id) DO NOTHING
    RETURNING id
), dropped AS (
    DELETE FROM memories
     WHERE id IN (SELECT id FROM moved)
     RETURNING id
)
SELECT COUNT(*) FROM dropped`

	var moved int
	if err := tx.QueryRowContext(ctx, moveSQL, cutoff, batch).Scan(&moved); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return moved, nil
}
