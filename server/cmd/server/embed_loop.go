package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/recall"
)

// Sprint 3 / L3: memory embedding backfill worker.
//
// 一个 leader-gated 后台 goroutine，每小时扫 memories 表里
// embedding IS NULL 的行，调 recall.Embedder.Embed() 编码后写回
// embedding + embedding_model + embedded_at 列。
//
// 没配 embedder（缺 OPENAI_API_KEY）时 loop 不启动 — 不报错也不
// 空转。Operator 后续配上 key + 重启即可自动启用。
//
// 故意不在新写入 memory 的同步路径里 embed —— daily review 是时
// 延敏感的；让 embed 走 backfill 慢生效，对 PM recall 体验影响
// 不大（recall 是 24-hour 时间尺度的语义召回）。

const (
	MemoryEmbedLeaseName = "memory-embed"

	defaultMemoryEmbedBatch    = 50
	defaultMemoryEmbedInterval = time.Hour
	// OpenAI embed 输入有 8K token 限制；几千字符已远足够，
	// 长 memory body 前 800 字符就够生成代表性 embedding。
	memoryEmbedMaxInputChars = 800
)

type memoryEmbedLoop struct {
	db       *sql.DB
	embedder recall.Embedder
	leader   leaderChecker
	interval time.Duration
	batch    int

	stopCh  chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
	started bool
}

func newMemoryEmbedLoop(db *sql.DB, embedder recall.Embedder) *memoryEmbedLoop {
	return &memoryEmbedLoop{
		db:       db,
		embedder: embedder,
		interval: defaultMemoryEmbedInterval,
		batch:    defaultMemoryEmbedBatch,
		stopCh:   make(chan struct{}),
	}
}

func (l *memoryEmbedLoop) SetLeaderChecker(checker leaderChecker) {
	if l == nil {
		return
	}
	l.leader = checker
}

func (l *memoryEmbedLoop) isLeader() bool {
	if l == nil || l.leader == nil {
		return true
	}
	return l.leader.IsLeader(MemoryEmbedLeaseName)
}

// Start 启动后台 loop。embedder 缺失直接 return — 没 provider
// 不能启动。
func (l *memoryEmbedLoop) Start() {
	if l == nil || l.db == nil || l.embedder == nil {
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
		warmup := time.NewTimer(2 * time.Minute)
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

func (l *memoryEmbedLoop) Stop() {
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

func (l *memoryEmbedLoop) runOnce() {
	if l == nil || l.db == nil || l.embedder == nil || !l.isLeader() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rows, err := l.loadDue(ctx, l.batch)
	if err != nil {
		// pgvector 没装 / 列不存在 → 不要 log 烦人，
		// 大概率是 dev / smoke 没跑 047 迁移。
		if isPgvectorMissing(err) {
			slog.Debug("memory embed: pgvector not installed, loop sleeping", "err", err)
			return
		}
		slog.Warn("memory embed: load due failed", "err", err)
		return
	}
	embedded := 0
	failed := 0
	for _, row := range rows {
		text := truncForEmbed(row.content)
		if text == "" {
			continue
		}
		// W14-1 — derive a per-row ctx so the QuotaEmbedder side-
		// car attributes this call. Empty fundID returns ctx
		// unchanged, so legacy NULL rows still flow through.
		embedCtx := recall.WithFundID(ctx, row.fundID)
		vec, err := l.embedder.Embed(embedCtx, text)
		if err != nil {
			failed++
			slog.Warn("memory embed: provider failed", "id", row.id, "err", err)
			continue
		}
		if len(vec) == 0 {
			continue
		}
		if err := l.writeBack(ctx, row.id, vec); err != nil {
			failed++
			slog.Warn("memory embed: write back failed", "id", row.id, "err", err)
			continue
		}
		embedded++
	}
	if embedded > 0 || failed > 0 {
		slog.Info("memory embed pass",
			"due", len(rows),
			"embedded", embedded,
			"failed", failed,
		)
	}
}

type dueMemoryEmbed struct {
	id      string
	fundID  string // W14-1 — populated for per-fund embed observability.
	content string
}

func (l *memoryEmbedLoop) loadDue(ctx context.Context, batch int) ([]dueMemoryEmbed, error) {
	// W14-1 — also pull fund_id so the embed call can be
	// attributed per fund. Legacy rows (cold-start, pre-001
	// migration) may have NULL fund_id; we accept the value as
	// "" and the side-car will silently drop those calls from
	// per-fund metrics, which is the correct semantics.
	const q = `
SELECT id, COALESCE(fund_id::text, ''), content
  FROM memories
 WHERE embedding IS NULL
   AND content IS NOT NULL
 ORDER BY created_at DESC
 LIMIT $1`
	rows, err := l.db.QueryContext(ctx, q, batch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dueMemoryEmbed
	for rows.Next() {
		var d dueMemoryEmbed
		if err := rows.Scan(&d.id, &d.fundID, &d.content); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (l *memoryEmbedLoop) writeBack(ctx context.Context, id string, vec []float32) error {
	literal := vectorLiteralForPg(vec)
	_, err := l.db.ExecContext(ctx, `
UPDATE memories
   SET embedding = $1::vector,
       embedding_model = $2,
       embedded_at = NOW()
 WHERE id = $3`,
		literal, l.embedder.Model(), id,
	)
	return err
}

// vectorLiteralForPg 把 float32 数组格式化为 pgvector "[a,b,c,...]"
// 字面量。
func vectorLiteralForPg(v []float32) string {
	var sb strings.Builder
	sb.Grow(len(v) * 8)
	sb.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(fmt.Sprintf("%.6f", x))
	}
	sb.WriteByte(']')
	return sb.String()
}

// truncForEmbed 把 memory body 截断到 embedder 能接受的长度。
// 我们也顺手 trim 空白行，避免给 provider 送大量噪声字符。
func truncForEmbed(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if len(text) > memoryEmbedMaxInputChars {
		text = text[:memoryEmbedMaxInputChars]
	}
	return text
}

// isPgvectorMissing 用错误消息粗略嗅探 pgvector / embedding 列不存
// 在的情况。我们不让 loop 在这种环境里 spammy log。
func isPgvectorMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "column \"embedding\""):
		return true
	case strings.Contains(msg, "type \"vector\""):
		return true
	case strings.Contains(msg, "extension \"vector\""):
		return true
	}
	return false
}
