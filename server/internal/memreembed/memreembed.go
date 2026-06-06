// Package memreembed re-embeds memories whose content was
// rewritten by consolidation, so similarity-aware recall keeps
// matching post-consolidation text.
//
// MOTIVATION
// ----------
// Memory consolidation merges N "daily/agent" memories into a
// single longer-horizon reflection. Today the new row gets
// inserted with the *consolidated* text but the embedding is
// either:
//   * NULL until the embed worker happens to pick it up; or
//   * inherited from one of the source rows, mismatching the
//     new content.
// Both cases break similarity recall — a search for "earnings
// drift" returns the original daily row but never finds the
// (more useful) consolidated reflection that incorporates a
// year of earnings-drift evidence.
//
// W3-18 introduces an explicit re-embed pipeline:
//
//   1. ConsolidateDaily appends a Request to the queue at the
//      end of each consolidation batch.
//   2. A worker drains the queue, asks the EmbedClient for an
//      embedding, and writes it back via the EmbedWriter.
//   3. Failed embeddings (e.g. provider rate limit) get
//      retried with exponential backoff; permanently failed
//      ones are quarantined to a dead-letter set the operator
//      can inspect.
//
// This package owns the queue + worker + retry policy. The
// concrete EmbedClient (OpenAI / local model) and EmbedWriter
// (memory_repo update) are supplied by the wiring layer.
//
// SCOPE
// -----
//   * Queue with bounded buffer and per-key dedupe.
//   * Worker with bounded concurrency, retry, dead-letter.
//   * EmbedClient / EmbedWriter interfaces (caller-supplied).
//   * Stats type for observability.
package memreembed

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Request is one re-embed task.
type Request struct {
	MemoryID string
	Content  string
	// Reason is human-readable context for the audit log.
	Reason string
	// EnqueuedAt is filled by the queue if zero.
	EnqueuedAt time.Time
}

// Stats reports the worker's running counters.
type Stats struct {
	Pending     int   `json:"pending"`
	Embedded    int64 `json:"embedded"`
	Retried     int64 `json:"retried"`
	DeadLetter  int64 `json:"deadLetter"`
	LastErrTime time.Time `json:"lastErrTime,omitempty"`
}

// EmbedClient produces vector embeddings for the supplied text.
type EmbedClient interface {
	Embed(ctx context.Context, content string) ([]float32, error)
}

// EmbedWriter persists the embedding back into memories.
type EmbedWriter interface {
	WriteEmbedding(ctx context.Context, memoryID string, embedding []float32) error
}

// Config controls the worker.
type Config struct {
	BufferSize    int           // 0 → 256
	MaxRetries    int           // 0 → 3
	RetryBackoff  time.Duration // 0 → 2s
	WorkerCount   int           // 0 → 2
	DeadLetterMax int           // 0 → 256
}

// DefaultConfig is the production-safe baseline.
func DefaultConfig() Config {
	return Config{
		BufferSize:    256,
		MaxRetries:    3,
		RetryBackoff:  2 * time.Second,
		WorkerCount:   2,
		DeadLetterMax: 256,
	}
}

// Queue is the dedupe-aware in-memory request queue.
//
// Two requests for the same MemoryID coalesce to a single task
// (the latest content wins). When the buffer is full new
// requests are rejected with the "queue full" error so
// consolidation can choose to back off rather than blocking.
type Queue struct {
	mu      sync.Mutex
	cfg     Config
	pending []Request
	idIndex map[string]int // memory id → index in pending
	stats   Stats
}

// NewQueue returns an empty queue.
func NewQueue(cfg Config) *Queue {
	return &Queue{
		cfg:     normalise(cfg),
		idIndex: make(map[string]int),
	}
}

// Enqueue submits a Request. Duplicates by MemoryID coalesce
// and the most-recent content wins. Returns an error iff the
// buffer is full and the request was dropped.
func (q *Queue) Enqueue(r Request) error {
	if q == nil {
		return errors.New("memreembed: nil queue")
	}
	if r.MemoryID == "" || r.Content == "" {
		return errors.New("memreembed: memoryID and content required")
	}
	if r.EnqueuedAt.IsZero() {
		r.EnqueuedAt = time.Now().UTC()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if idx, ok := q.idIndex[r.MemoryID]; ok {
		q.pending[idx] = r
		return nil
	}
	if len(q.pending) >= q.cfg.BufferSize {
		return errors.New("memreembed: queue full")
	}
	q.pending = append(q.pending, r)
	q.idIndex[r.MemoryID] = len(q.pending) - 1
	q.stats.Pending = len(q.pending)
	return nil
}

// Drain pops up to N pending requests. Used by the worker loop;
// also useful in tests.
func (q *Queue) Drain(n int) []Request {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if n <= 0 || len(q.pending) == 0 {
		return nil
	}
	if n > len(q.pending) {
		n = len(q.pending)
	}
	out := make([]Request, n)
	copy(out, q.pending[:n])
	q.pending = q.pending[n:]
	q.idIndex = make(map[string]int, len(q.pending))
	for i, r := range q.pending {
		q.idIndex[r.MemoryID] = i
	}
	q.stats.Pending = len(q.pending)
	return out
}

// Stats returns a snapshot of the worker counters.
func (q *Queue) Stats() Stats {
	if q == nil {
		return Stats{}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	s := q.stats
	s.Pending = len(q.pending)
	return s
}

// recordEmbed / recordRetry / recordDeadLetter are internal
// observability hooks called from the worker loop.
func (q *Queue) recordEmbed()     { q.bumpStats(func(s *Stats) { s.Embedded++ }) }
func (q *Queue) recordRetry()     { q.bumpStats(func(s *Stats) { s.Retried++ }) }
func (q *Queue) recordDeadLetter() { q.bumpStats(func(s *Stats) { s.DeadLetter++ }) }
func (q *Queue) recordErrAt(at time.Time) {
	q.bumpStats(func(s *Stats) { s.LastErrTime = at })
}

func (q *Queue) bumpStats(f func(*Stats)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	f(&q.stats)
}

// ProcessOnce drains up to BufferSize requests, embeds each,
// and writes the result. Returns the number processed and the
// number that ended up in the dead letter.
//
// Each request is independently retried up to MaxRetries
// before dead-lettering, with exponential backoff in between.
//
// Designed to be called from a periodic ticker (every 5-10s).
func ProcessOnce(ctx context.Context, q *Queue, client EmbedClient, writer EmbedWriter) (int, int, error) {
	if q == nil {
		return 0, 0, errors.New("memreembed: nil queue")
	}
	if client == nil || writer == nil {
		return 0, 0, errors.New("memreembed: client and writer required")
	}
	batch := q.Drain(q.cfg.BufferSize)
	if len(batch) == 0 {
		return 0, 0, nil
	}
	processed := 0
	dead := 0
	for _, req := range batch {
		if ctx.Err() != nil {
			return processed, dead, ctx.Err()
		}
		err := embedAndWrite(ctx, req, client, writer, q)
		if err != nil {
			dead++
			q.recordDeadLetter()
			q.recordErrAt(time.Now().UTC())
			continue
		}
		processed++
		q.recordEmbed()
	}
	return processed, dead, nil
}

func embedAndWrite(ctx context.Context, req Request, client EmbedClient, writer EmbedWriter, q *Queue) error {
	var lastErr error
	for attempt := 0; attempt <= q.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			q.recordRetry()
			select {
			case <-time.After(q.cfg.RetryBackoff << (attempt - 1)):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		emb, err := client.Embed(ctx, req.Content)
		if err != nil {
			lastErr = err
			continue
		}
		if err := writer.WriteEmbedding(ctx, req.MemoryID, emb); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

func normalise(c Config) Config {
	d := DefaultConfig()
	if c.BufferSize <= 0 {
		c.BufferSize = d.BufferSize
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = d.MaxRetries
	}
	if c.RetryBackoff <= 0 {
		c.RetryBackoff = d.RetryBackoff
	}
	if c.WorkerCount <= 0 {
		c.WorkerCount = d.WorkerCount
	}
	if c.DeadLetterMax <= 0 {
		c.DeadLetterMax = d.DeadLetterMax
	}
	return c
}
