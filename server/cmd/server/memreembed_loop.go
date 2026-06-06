package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/memreembed"
	"github.com/fundai/server/internal/recall"
)

// W6-1 — wire the W3-18 memreembed package into the runtime.
//
// MOTIVATION
// ----------
// W3-18 introduced a re-embed queue + worker so consolidated
// memories get fresh embeddings, but the package sat unused —
// nothing called Enqueue, no worker drained the queue, and
// `services.EmbedLimiter` (W5-3) didn't yet apply to this code
// path. This loop closes the gap:
//
//   * Owns the long-lived Queue.
//   * Periodically calls memreembed.ProcessOnce with the
//     SAME quota-gated recall.Embedder every other embed call
//     site uses, so the daily token ledger is shared across
//     memoryEmbedLoop + workflow semantic recall + this worker.
//   * Implements memreembed.EmbedWriter against the existing
//     memories table (same SQL columns as memoryEmbedLoop.writeBack
//     so the row layout invariant stays single-sourced).
//   * Leader-gated: only one server in a multi-replica deployment
//     drains the queue, mirroring memoryEmbedLoop.

const (
	// MemReembedLeaseName is the lease key used by the
	// leaderChecker. Distinct from MemoryEmbedLeaseName so an
	// operator can pin re-embed work to a different replica
	// than the cold-start backfill if needed.
	MemReembedLeaseName = "memory-reembed"

	defaultMemReembedInterval = 10 * time.Second
)

// memReembedWriter persists the new vector back into the
// memories row. Mirrors memoryEmbedLoop.writeBack so any future
// schema change to the embedding columns updates one source of
// truth (the helpers, not two diverging UPDATE strings).
type memReembedWriter struct {
	db      *sql.DB
	modelID string
}

func newMemReembedWriter(db *sql.DB, modelID string) *memReembedWriter {
	return &memReembedWriter{db: db, modelID: strings.TrimSpace(modelID)}
}

// WriteEmbedding satisfies memreembed.EmbedWriter.
func (w *memReembedWriter) WriteEmbedding(ctx context.Context, memoryID string, embedding []float32) error {
	if w == nil || w.db == nil {
		return errors.New("memreembed: nil writer or db")
	}
	if memoryID == "" {
		return errors.New("memreembed: empty memory id")
	}
	if len(embedding) == 0 {
		return errors.New("memreembed: empty embedding")
	}
	literal := vectorLiteralForPg(embedding)
	_, err := w.db.ExecContext(ctx, `
UPDATE memories
   SET embedding = $1::vector,
       embedding_model = $2,
       embedded_at = NOW()
 WHERE id = $3`,
		literal, w.modelID, memoryID,
	)
	return err
}

// memReembedLoop is the long-lived worker that drains the
// memreembed queue. It mirrors the lifecycle conventions of
// memoryEmbedLoop (Start / Stop / leader gating) so operators
// only have to learn one shape.
type memReembedLoop struct {
	queue    *memreembed.Queue
	embedder recall.Embedder
	writer   memreembed.EmbedWriter
	leader   leaderChecker
	interval time.Duration

	stopCh  chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
	started bool
}

func newMemReembedLoop(queue *memreembed.Queue, embedder recall.Embedder, writer memreembed.EmbedWriter) *memReembedLoop {
	return &memReembedLoop{
		queue:    queue,
		embedder: embedder,
		writer:   writer,
		interval: defaultMemReembedInterval,
		stopCh:   make(chan struct{}),
	}
}

// SetLeaderChecker plugs in the cluster-wide lease so only one
// replica drains the queue.
func (l *memReembedLoop) SetLeaderChecker(checker leaderChecker) {
	if l == nil {
		return
	}
	l.leader = checker
}

func (l *memReembedLoop) isLeader() bool {
	if l == nil || l.leader == nil {
		return true
	}
	return l.leader.IsLeader(MemReembedLeaseName)
}

// Start spins up the drain goroutine. Required dependencies
// missing → no-op so wiring code can call this unconditionally.
func (l *memReembedLoop) Start() {
	if l == nil || l.queue == nil || l.embedder == nil || l.writer == nil {
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

func (l *memReembedLoop) Stop() {
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

// runOnce drains up to BufferSize requests, calling
// memreembed.ProcessOnce with the configured client+writer.
// Soft-fails: a transient provider error logs but doesn't tear
// down the loop. The queue itself owns retry / dead-letter so
// we don't reimplement that logic here.
func (l *memReembedLoop) runOnce() {
	if l == nil || !l.isLeader() {
		return
	}
	stats := l.queue.Stats()
	if stats.Pending == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	processed, dead, err := memreembed.ProcessOnce(ctx, l.queue, l.embedder, l.writer)
	if err != nil {
		slog.Warn("memory re-embed pass failed", "err", err)
	}
	if processed > 0 || dead > 0 {
		slog.Info("memory re-embed pass",
			"processed", processed,
			"dead_letter", dead,
			"pending_after", l.queue.Stats().Pending,
		)
	}
}
