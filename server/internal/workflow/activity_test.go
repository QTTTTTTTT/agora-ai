package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeActivityPersister is the in-memory stub used to drive the
// persistence-related tests. It records every BulkInsert call and
// exposes ListByFund / MaxSeqForFund so the bus's hydration and
// pagination paths can be exercised without a real DB.
type fakeActivityPersister struct {
	mu     sync.Mutex
	events map[string][]PersistableActivityEvent
	maxSeq map[string]uint64
	// insertErr is returned from BulkInsert when set, lets tests cover
	// the "DB outage degrades gracefully" promise.
	insertErr error
}

func newFakeActivityPersister() *fakeActivityPersister {
	return &fakeActivityPersister{
		events: make(map[string][]PersistableActivityEvent),
		maxSeq: make(map[string]uint64),
	}
}

func (p *fakeActivityPersister) BulkInsert(_ context.Context, events []PersistableActivityEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.insertErr != nil {
		return p.insertErr
	}
	for _, evt := range events {
		p.events[evt.FundID] = append(p.events[evt.FundID], evt)
		if evt.Seq > p.maxSeq[evt.FundID] {
			p.maxSeq[evt.FundID] = evt.Seq
		}
	}
	return nil
}

func (p *fakeActivityPersister) ListByFund(_ context.Context, fundID string, before time.Time, limit int) ([]PersistableActivityEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	src := p.events[fundID]
	out := make([]PersistableActivityEvent, 0, len(src))
	for _, evt := range src {
		if !before.IsZero() && !evt.EventAt.Before(before) {
			continue
		}
		out = append(out, evt)
	}
	// Newest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (p *fakeActivityPersister) MaxSeqForFund(_ context.Context, fundID string) (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxSeq[fundID], nil
}

func (p *fakeActivityPersister) count(fundID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.events[fundID])
}

// TestActivityBusPersistsToWritePath is the regression test for the
// "team activity panel is empty after restart" production bug: every
// published event should land in the persister so a follow-up Page()
// (post-restart) can still surface it.
func TestActivityBusPersistsToWritePath(t *testing.T) {
	persister := newFakeActivityPersister()
	bus := NewActivityBus(10, 4).WithPersister(persister, 64)
	for i := 0; i < 5; i++ {
		evt := WorkflowEvent{Type: "step_started", FundID: "fund-A", Timestamp: time.Now()}
		if err := bus.Publish(context.Background(), evt); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	// Close drains the persist queue, so once it returns we know all
	// events have been flushed into the persister.
	bus.Close()
	if got := persister.count("fund-A"); got != 5 {
		t.Fatalf("expected 5 events persisted, got %d", got)
	}
}

// TestActivityBusPageReturnsFromPersister verifies the "load earlier"
// path: events older than the in-memory ring window can still be
// fetched via Page() because the persister has the full history.
func TestActivityBusPageReturnsFromPersister(t *testing.T) {
	persister := newFakeActivityPersister()
	bus := NewActivityBus(2, 4).WithPersister(persister, 64) // tiny ring
	base := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		evt := WorkflowEvent{
			Type:      "step_started",
			FundID:    "fund-A",
			Timestamp: base.Add(time.Duration(i) * time.Minute),
		}
		if err := bus.Publish(context.Background(), evt); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	// Drain so the persister sees everything before we Page().
	bus.Close()
	// Ring only has the newest 2 events. The "load earlier" call asks
	// for events strictly before 12:03 (i.e. seq 1,2,3) — must come
	// from the persister.
	page, err := bus.Page(context.Background(), "fund-A", base.Add(3*time.Minute), 10)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if len(page) != 3 {
		t.Fatalf("expected 3 older events from persister, got %d", len(page))
	}
	for _, e := range page {
		if !e.Timestamp.Before(base.Add(3 * time.Minute)) {
			t.Errorf("page returned event at %s that is not older than cursor", e.Timestamp)
		}
	}
}

// TestActivityBusHydratesSeqFromPersister is the cross-restart seq
// continuity test: after a process restart the new bus must pick up
// totalSeq where the old one left off, otherwise SSE sinceSeq breaks
// and the (fund_id, seq) UNIQUE index in the persister explodes.
func TestActivityBusHydratesSeqFromPersister(t *testing.T) {
	persister := newFakeActivityPersister()
	// Simulate a prior process by seeding the persister with seq=5.
	if err := persister.BulkInsert(context.Background(), []PersistableActivityEvent{
		{FundID: "fund-A", Seq: 5, Type: "step_started", EventAt: time.Now()},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// New bus, fresh in-memory totalSeq=0.
	bus := NewActivityBus(10, 4).WithPersister(persister, 64)
	defer bus.Close()
	if err := bus.Publish(context.Background(), WorkflowEvent{Type: "step_started", FundID: "fund-A"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	recent := bus.Recent("fund-A", 10, 0)
	// Recent merges ring + DB so the historical seq=5 row is also
	// surfaced now (per the "panel empty after restart" fix); the
	// invariant this test is really about is the seq assigned to the
	// new publish.
	if len(recent) != 2 {
		t.Fatalf("expected merged ring+DB = 2 events, got %d (%+v)", len(recent), recent)
	}
	if recent[1].Seq != 6 {
		t.Fatalf("expected hydrated seq=6 (max=5 +1) on the latest event, got %d", recent[1].Seq)
	}
}

// TestActivityBusRecentBackfillsFromPersisterAfterRestart guards the
// production bug "team activity panel is empty after restart". When the
// in-memory ring is empty (process just started, no Publish for this
// fund yet) Recent() must fall back to the persistence sidecar instead
// of returning nil. Without the fallback the UI sees a blank panel
// until the next workflow tick fires, which can be hours away.
func TestActivityBusRecentBackfillsFromPersisterAfterRestart(t *testing.T) {
	persister := newFakeActivityPersister()
	// Pre-seed the DB to look like a previous process had written 4
	// events for fund-A.
	now := time.Now().UTC()
	seed := []PersistableActivityEvent{
		{FundID: "fund-A", Seq: 1, Type: "run_started", EventAt: now.Add(-4 * time.Minute)},
		{FundID: "fund-A", Seq: 2, Type: "step_started", EventAt: now.Add(-3 * time.Minute)},
		{FundID: "fund-A", Seq: 3, Type: "step_completed", EventAt: now.Add(-2 * time.Minute)},
		{FundID: "fund-A", Seq: 4, Type: "step_started", EventAt: now.Add(-1 * time.Minute)},
	}
	if err := persister.BulkInsert(context.Background(), seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Fresh bus — ring is empty, fund-A key not even allocated.
	bus := NewActivityBus(10, 4).WithPersister(persister, 64)
	defer bus.Close()

	got := bus.Recent("fund-A", 10, 0)
	if len(got) != 4 {
		t.Fatalf("expected DB backfill to return 4 events, got %d", len(got))
	}
	// Output contract: oldest-first.
	for i, e := range got {
		if e.Seq != uint64(i+1) {
			t.Errorf("event %d: expected seq %d, got %d (output must be oldest-first)", i, i+1, e.Seq)
		}
	}
}

// TestActivityBusRecentMergesRingAndPersister covers the case where
// the ring has a few fresh events but not enough to fill `limit` — we
// must merge ring + DB by Seq, dedupe, and still return oldest-first.
func TestActivityBusRecentMergesRingAndPersister(t *testing.T) {
	persister := newFakeActivityPersister()
	now := time.Now().UTC()
	// DB has the older history (seq 1-3).
	if err := persister.BulkInsert(context.Background(), []PersistableActivityEvent{
		{FundID: "fund-A", Seq: 1, Type: "run_started", EventAt: now.Add(-5 * time.Minute)},
		{FundID: "fund-A", Seq: 2, Type: "step_started", EventAt: now.Add(-4 * time.Minute)},
		{FundID: "fund-A", Seq: 3, Type: "step_completed", EventAt: now.Add(-3 * time.Minute)},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	bus := NewActivityBus(10, 4).WithPersister(persister, 64)
	defer bus.Close()
	// One new publish fires after restart. Seq hydration → 4.
	if err := bus.Publish(context.Background(), WorkflowEvent{Type: "step_started", FundID: "fund-A"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	got := bus.Recent("fund-A", 10, 0)
	if len(got) != 4 {
		t.Fatalf("expected merge of ring+DB = 4 events, got %d", len(got))
	}
	seqs := make([]uint64, len(got))
	for i, e := range got {
		seqs[i] = e.Seq
	}
	want := []uint64{1, 2, 3, 4}
	for i := range want {
		if seqs[i] != want[i] {
			t.Fatalf("expected merged seqs %v, got %v", want, seqs)
		}
	}
}

// TestActivityBusRecentDBSinceSeq verifies that when the in-memory ring
// has no rows after sinceSeq, the DB fallback still honours the cursor.
func TestActivityBusRecentDBSinceSeq(t *testing.T) {
	persister := newFakeActivityPersister()
	now := time.Now().UTC()
	if err := persister.BulkInsert(context.Background(), []PersistableActivityEvent{
		{FundID: "fund-A", Seq: 1, Type: "run_started", EventAt: now.Add(-3 * time.Minute)},
		{FundID: "fund-A", Seq: 2, Type: "step_started", EventAt: now.Add(-2 * time.Minute)},
		{FundID: "fund-A", Seq: 3, Type: "step_completed", EventAt: now.Add(-1 * time.Minute)},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	bus := NewActivityBus(10, 4).WithPersister(persister, 64)
	defer bus.Close()

	got := bus.Recent("fund-A", 10, 2)
	if len(got) != 1 || got[0].Seq != 3 {
		t.Fatalf("expected only seq=3 after sinceSeq=2, got %+v", got)
	}
}

// TestActivityBusPublishAssignsMonotonicSeqs guards the contract that Seq
// is monotonically increasing per fund so the UI can use it as a "last seen"
// cursor for backfill.
func TestActivityBusPublishAssignsMonotonicSeqs(t *testing.T) {
	bus := NewActivityBus(10, 4)
	defer bus.Close()

	for i := 0; i < 5; i++ {
		evt := WorkflowEvent{
			Type:   "step_started",
			Step:   StepMacroBrief,
			FundID: "fund-A",
		}
		if err := bus.Publish(context.Background(), evt); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	recent := bus.Recent("fund-A", 10, 0)
	if len(recent) != 5 {
		t.Fatalf("expected 5 events, got %d", len(recent))
	}
	for i, e := range recent {
		if e.Seq != uint64(i+1) {
			t.Errorf("event %d: expected seq %d, got %d", i, i+1, e.Seq)
		}
	}
}

// TestActivityBusRecentRespectsSinceSeq verifies the cursor-based backfill
// path used after SSE reconnects.
func TestActivityBusRecentRespectsSinceSeq(t *testing.T) {
	bus := NewActivityBus(10, 4)
	defer bus.Close()
	for i := 0; i < 5; i++ {
		_ = bus.Publish(context.Background(), WorkflowEvent{Type: "step_started", FundID: "f"})
	}
	got := bus.Recent("f", 10, 2)
	if len(got) != 3 {
		t.Fatalf("expected 3 events after seq 2, got %d", len(got))
	}
	for i, e := range got {
		if e.Seq != uint64(i+3) {
			t.Errorf("event %d: expected seq %d, got %d", i, i+3, e.Seq)
		}
	}
}

// TestActivityBusRingBufferEvicts asserts that the buffer is bounded so a
// long-running fund cannot OOM the process.
func TestActivityBusRingBufferEvicts(t *testing.T) {
	bus := NewActivityBus(3, 4)
	defer bus.Close()
	for i := 0; i < 5; i++ {
		_ = bus.Publish(context.Background(), WorkflowEvent{Type: "step_started", FundID: "f"})
	}
	got := bus.Recent("f", 10, 0)
	if len(got) != 3 {
		t.Fatalf("expected buffer cap of 3, got %d", len(got))
	}
	// Should be the LAST 3 events (seq 3, 4, 5).
	wantSeqs := []uint64{3, 4, 5}
	for i, e := range got {
		if e.Seq != wantSeqs[i] {
			t.Errorf("ring evict mismatch at %d: got seq %d want %d", i, e.Seq, wantSeqs[i])
		}
	}
}

// TestActivityBusSubscribeReceivesLiveEvents covers the happy-path SSE
// subscription: events published after Subscribe arrive on the channel.
func TestActivityBusSubscribeReceivesLiveEvents(t *testing.T) {
	bus := NewActivityBus(10, 4)
	defer bus.Close()

	sub, err := bus.Subscribe("fund-A")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Cancel()

	_ = bus.Publish(context.Background(), WorkflowEvent{Type: "step_started", FundID: "fund-A", Step: StepMacroBrief})

	select {
	case evt := <-sub.Events:
		if evt.Type != "step_started" || evt.Seq != 1 || evt.Role != "researcher" {
			t.Fatalf("unexpected event: %#v", evt)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected event within 200ms")
	}
}

// TestActivityBusSubscribeIsolatedByFund proves that a subscriber for fund A
// does NOT see events for fund B (the per-fund tenancy guarantee).
func TestActivityBusSubscribeIsolatedByFund(t *testing.T) {
	bus := NewActivityBus(10, 4)
	defer bus.Close()
	subA, _ := bus.Subscribe("fund-A")
	defer subA.Cancel()
	subB, _ := bus.Subscribe("fund-B")
	defer subB.Cancel()

	_ = bus.Publish(context.Background(), WorkflowEvent{Type: "step_started", FundID: "fund-B"})

	select {
	case evt := <-subA.Events:
		t.Fatalf("fund-A subscriber should NOT see fund-B events, got %#v", evt)
	case <-time.After(100 * time.Millisecond):
		// expected: no event for A
	}
	select {
	case evt := <-subB.Events:
		if evt.FundID != "fund-B" {
			t.Fatalf("expected fund-B event, got %#v", evt)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("fund-B subscriber missed the event")
	}
}

// TestActivityBusCancelClosesChannelAndStopsDelivery confirms the cleanup
// contract: after Cancel, the channel is closed and no further events arrive.
func TestActivityBusCancelClosesChannelAndStopsDelivery(t *testing.T) {
	bus := NewActivityBus(10, 4)
	defer bus.Close()
	sub, _ := bus.Subscribe("fund-A")
	sub.Cancel()

	// Channel should be closed.
	select {
	case _, alive := <-sub.Events:
		if alive {
			t.Fatal("expected channel to be closed after Cancel")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected close signal within 100ms")
	}

	// Subsequent Cancel is a no-op.
	sub.Cancel()

	// Publish should not panic even with no subscribers.
	_ = bus.Publish(context.Background(), WorkflowEvent{Type: "step_started", FundID: "fund-A"})
	if bus.SubscriberCount("fund-A") != 0 {
		t.Fatalf("expected 0 subscribers, got %d", bus.SubscriberCount("fund-A"))
	}
}

// TestActivityBusSlowSubscriberDropsInsteadOfBlocking is the critical
// resilience test: a stuck SSE client must never wedge the orchestrator.
func TestActivityBusSlowSubscriberDropsInsteadOfBlocking(t *testing.T) {
	bus := NewActivityBus(10, 2) // subscriber buffer = 2
	defer bus.Close()

	sub, _ := bus.Subscribe("fund-A")
	defer sub.Cancel()

	// Never drain. Publish more events than the buffer can hold.
	for i := 0; i < 20; i++ {
		if err := bus.Publish(context.Background(), WorkflowEvent{Type: "step_started", FundID: "fund-A"}); err != nil {
			t.Fatalf("publish %d should not block/error: %v", i, err)
		}
	}
	if dropped := sub.DroppedCount(); dropped == 0 {
		t.Fatalf("expected at least one dropped event for slow consumer, got 0")
	}
}

// TestActivityBusCloseTerminatesAllSubscribers verifies graceful shutdown:
// every active SSE channel is closed so handlers can return cleanly.
func TestActivityBusCloseTerminatesAllSubscribers(t *testing.T) {
	bus := NewActivityBus(10, 4)
	subs := make([]*Subscription, 3)
	for i := range subs {
		s, _ := bus.Subscribe(fmt.Sprintf("fund-%d", i))
		subs[i] = s
	}
	bus.Close()
	for i, s := range subs {
		select {
		case _, alive := <-s.Events:
			if alive {
				t.Fatalf("sub %d: expected channel closed after Close()", i)
			}
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("sub %d: channel did not close within 200ms", i)
		}
	}
	// Subscribe after close should fail explicitly so callers can surface the
	// shutdown to clients.
	if _, err := bus.Subscribe("fund-X"); !errors.Is(err, ErrBusClosed) {
		t.Fatalf("expected ErrBusClosed after Close, got %v", err)
	}
	if err := bus.Publish(context.Background(), WorkflowEvent{FundID: "fund-X"}); err != nil {
		t.Fatalf("Publish should be a silent no-op after Close, got %v", err)
	}
}

// TestActivityBusConcurrentPublishersAreSafe ensures the bus survives heavy
// concurrent traffic from multiple orchestrators publishing simultaneously.
// Failure modes would be panics from concurrent map writes, data races caught
// by `-race`, or missing seqs (gaps would indicate lost events under load).
func TestActivityBusConcurrentPublishersAreSafe(t *testing.T) {
	bus := NewActivityBus(2000, 64)
	defer bus.Close()

	var wg sync.WaitGroup
	const publishers = 8
	const eventsPerPublisher = 200
	wg.Add(publishers)
	for p := 0; p < publishers; p++ {
		go func() {
			defer wg.Done()
			for i := 0; i < eventsPerPublisher; i++ {
				_ = bus.Publish(context.Background(), WorkflowEvent{Type: "step_started", FundID: "fund-A"})
			}
		}()
	}
	wg.Wait()

	got := bus.Recent("fund-A", 2000, 0)
	if len(got) != publishers*eventsPerPublisher {
		t.Fatalf("expected %d events, got %d", publishers*eventsPerPublisher, len(got))
	}
	// Seqs are dense from 1..N with no duplicates (per-fund monotonic).
	seen := make(map[uint64]struct{}, len(got))
	for _, e := range got {
		if _, dup := seen[e.Seq]; dup {
			t.Fatalf("duplicate seq %d", e.Seq)
		}
		seen[e.Seq] = struct{}{}
	}
	for i := uint64(1); i <= uint64(publishers*eventsPerPublisher); i++ {
		if _, ok := seen[i]; !ok {
			t.Fatalf("missing seq %d", i)
		}
	}
}

// TestStepOwnerRoleCoversAllKnownSteps documents the role mapping so future
// step additions remember to update the dispatch table (otherwise the UI
// silently shows "system" for the new step's events).
func TestStepOwnerRoleCoversAllKnownSteps(t *testing.T) {
	cases := []struct {
		step WorkflowStep
		want string
	}{
		{StepMacroBrief, "researcher"},
		{StepResearchParallel, "researcher"},
		{StepQuantSignals, "researcher"},
		{StepRoundtable, "team"},
		{StepPMPlan, "pm"},
		{StepRiskReview, "risk"},
		{StepUserApproval, "user"},
		{StepTradeExecution, "trader"},
		{StepSettlement, "pm"},
		{StepDailyReview, "pm"},
	}
	for _, tc := range cases {
		if got := stepOwnerRole(tc.step); got != tc.want {
			t.Errorf("stepOwnerRole(%v) = %q, want %q", tc.step, got, tc.want)
		}
	}
}

// TestNewActivityEventMessagesAreNonEmpty ensures every event type emitted by
// the orchestrator yields a non-empty UI message — saves us from blank rows
// on the timeline if a new event type is added without updating the renderer.
func TestNewActivityEventMessagesAreNonEmpty(t *testing.T) {
	types := []string{
		"run_started", "run_completed", "run_failed", "run_rejected", "run_resumed",
		"awaiting_user",
		"step_started", "step_completed", "step_failed", "step_skipped", "step_paused",
	}
	for _, eventType := range types {
		evt := WorkflowEvent{Type: eventType, FundID: "f", Step: StepMacroBrief, Timestamp: time.Now()}
		out := newActivityEvent(evt)
		if out.Message == "" {
			t.Errorf("event type %q produced empty message", eventType)
		}
	}
}

// TestNewActivityEventStringifiesError verifies that `error` survives the
// projection to ActivityEvent (where it becomes a JSON-safe string field).
func TestNewActivityEventStringifiesError(t *testing.T) {
	evt := WorkflowEvent{
		Type:   "step_failed",
		FundID: "f",
		Step:   StepMacroBrief,
		Error:  errors.New("upstream provider 503"),
	}
	out := newActivityEvent(evt)
	if out.ErrorMessage != "upstream provider 503" {
		t.Fatalf("expected error stringified, got %q", out.ErrorMessage)
	}
}
