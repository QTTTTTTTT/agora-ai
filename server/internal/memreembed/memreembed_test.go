package memreembed

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fakeEmbed struct {
	failTimes int32 // first N calls fail
	calls     int32
}

func (f *fakeEmbed) Embed(ctx context.Context, content string) ([]float32, error) {
	calls := atomic.AddInt32(&f.calls, 1)
	if calls <= atomic.LoadInt32(&f.failTimes) {
		return nil, errors.New("synthetic embed failure")
	}
	return []float32{0.1, 0.2, 0.3}, nil
}

type fakeWriter struct {
	wrote map[string][]float32
	mu    chan struct{}
}

func newFakeWriter() *fakeWriter {
	return &fakeWriter{wrote: make(map[string][]float32), mu: make(chan struct{}, 1)}
}

func (f *fakeWriter) WriteEmbedding(ctx context.Context, memoryID string, embedding []float32) error {
	f.mu <- struct{}{}
	defer func() { <-f.mu }()
	f.wrote[memoryID] = embedding
	return nil
}

func TestEnqueueDedupesByMemoryID(t *testing.T) {
	q := NewQueue(DefaultConfig())
	if err := q.Enqueue(Request{MemoryID: "m1", Content: "v1"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := q.Enqueue(Request{MemoryID: "m1", Content: "v2"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if got := q.Stats().Pending; got != 1 {
		t.Errorf("Pending: got %d, want 1 (dedup)", got)
	}
	got := q.Drain(10)
	if len(got) != 1 {
		t.Fatalf("Drain: got %d, want 1", len(got))
	}
	if got[0].Content != "v2" {
		t.Errorf("latest content should win, got %q", got[0].Content)
	}
}

func TestEnqueueRejectsEmpty(t *testing.T) {
	q := NewQueue(DefaultConfig())
	if err := q.Enqueue(Request{MemoryID: "", Content: "x"}); err == nil {
		t.Errorf("empty memory id should error")
	}
	if err := q.Enqueue(Request{MemoryID: "m", Content: ""}); err == nil {
		t.Errorf("empty content should error")
	}
}

func TestEnqueueRejectsWhenFull(t *testing.T) {
	q := NewQueue(Config{BufferSize: 2, MaxRetries: 1, RetryBackoff: time.Millisecond, WorkerCount: 1, DeadLetterMax: 10})
	q.Enqueue(Request{MemoryID: "m1", Content: "x"})
	q.Enqueue(Request{MemoryID: "m2", Content: "x"})
	if err := q.Enqueue(Request{MemoryID: "m3", Content: "x"}); err == nil {
		t.Errorf("expected queue full error")
	}
}

func TestProcessOnceSuccess(t *testing.T) {
	q := NewQueue(DefaultConfig())
	q.Enqueue(Request{MemoryID: "m1", Content: "x"})
	q.Enqueue(Request{MemoryID: "m2", Content: "y"})
	w := newFakeWriter()
	processed, dead, err := ProcessOnce(context.Background(), q, &fakeEmbed{}, w)
	if err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if processed != 2 || dead != 0 {
		t.Errorf("got processed=%d dead=%d, want 2, 0", processed, dead)
	}
	if len(w.wrote) != 2 {
		t.Errorf("expected 2 writes, got %d", len(w.wrote))
	}
	if q.Stats().Embedded != 2 {
		t.Errorf("Embedded stat: got %d, want 2", q.Stats().Embedded)
	}
}

func TestProcessOnceRetriesThenSucceeds(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RetryBackoff = time.Millisecond
	q := NewQueue(cfg)
	q.Enqueue(Request{MemoryID: "m1", Content: "x"})
	embed := &fakeEmbed{failTimes: 2}
	w := newFakeWriter()
	processed, dead, err := ProcessOnce(context.Background(), q, embed, w)
	if err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if processed != 1 || dead != 0 {
		t.Errorf("got processed=%d dead=%d, want 1, 0", processed, dead)
	}
	if q.Stats().Retried < 2 {
		t.Errorf("Retried: got %d, want >= 2", q.Stats().Retried)
	}
}

func TestProcessOnceDeadLetters(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxRetries = 1
	cfg.RetryBackoff = time.Millisecond
	q := NewQueue(cfg)
	q.Enqueue(Request{MemoryID: "m1", Content: "x"})
	embed := &fakeEmbed{failTimes: 100}
	w := newFakeWriter()
	processed, dead, err := ProcessOnce(context.Background(), q, embed, w)
	if err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if processed != 0 || dead != 1 {
		t.Errorf("got processed=%d dead=%d, want 0, 1", processed, dead)
	}
	if q.Stats().DeadLetter != 1 {
		t.Errorf("DeadLetter: got %d, want 1", q.Stats().DeadLetter)
	}
}

func TestProcessOnceRejectsNilArgs(t *testing.T) {
	q := NewQueue(DefaultConfig())
	if _, _, err := ProcessOnce(context.Background(), q, nil, nil); err == nil {
		t.Errorf("nil args should error")
	}
}

func TestProcessOnceEmptyQueueIsNoOp(t *testing.T) {
	q := NewQueue(DefaultConfig())
	processed, dead, err := ProcessOnce(context.Background(), q, &fakeEmbed{}, newFakeWriter())
	if err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if processed != 0 || dead != 0 {
		t.Errorf("expected 0/0, got %d/%d", processed, dead)
	}
}

func TestStatsCountersAreObservable(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RetryBackoff = time.Millisecond
	q := NewQueue(cfg)
	for i := 0; i < 5; i++ {
		q.Enqueue(Request{MemoryID: "m" + string(rune('a'+i)), Content: "x"})
	}
	embed := &fakeEmbed{failTimes: 2}
	ProcessOnce(context.Background(), q, embed, newFakeWriter())
	s := q.Stats()
	if s.Embedded == 0 {
		t.Errorf("Embedded should be > 0")
	}
}

func TestNilQueueIsNoOp(t *testing.T) {
	var q *Queue
	if err := q.Enqueue(Request{MemoryID: "m", Content: "x"}); err == nil {
		t.Errorf("nil queue should error")
	}
	if got := q.Stats(); got.Pending != 0 {
		t.Errorf("nil queue stats: %v", got)
	}
}

func TestDrainPreservesFIFO(t *testing.T) {
	q := NewQueue(DefaultConfig())
	q.Enqueue(Request{MemoryID: "a", Content: "1"})
	q.Enqueue(Request{MemoryID: "b", Content: "2"})
	q.Enqueue(Request{MemoryID: "c", Content: "3"})
	got := q.Drain(2)
	if len(got) != 2 || got[0].MemoryID != "a" || got[1].MemoryID != "b" {
		t.Errorf("FIFO violated: %+v", got)
	}
	if q.Stats().Pending != 1 {
		t.Errorf("Pending: got %d, want 1", q.Stats().Pending)
	}
}
