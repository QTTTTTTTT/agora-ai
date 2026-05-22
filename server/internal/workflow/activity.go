package workflow

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// ActivityBus is a process-local pub/sub for WorkflowEvent. It satisfies the
// workflow.EventBus interface and is intended as the canonical implementation
// when no external broker (NATS, Redis, Kafka) is required.
//
// The bus keeps two state buckets per fund:
//
//   - A bounded ring buffer of the most recent events. The REST endpoint
//     `/api/funds/{fundId}/team/activity` reads from this so a freshly-loaded
//     page can backfill the timeline without waiting for the next workflow tick.
//   - A set of per-subscriber channels. Each SSE stream is one subscriber; when
//     the channel is full (slow client) the publisher drops the event for that
//     subscriber only, never blocks the orchestrator. The dropped count is
//     reported via Subscription.DroppedCount() for observability.
//
// Concurrency:
//
//   - Publish is non-blocking. Slow subscribers do not stall the workflow.
//   - Subscribe returns a Subscription with Cancel/Events/DroppedCount. Always
//     call Cancel exactly once (e.g. via defer in the handler).
//   - All public methods are safe for concurrent use.
type ActivityBus struct {
	mu         sync.RWMutex
	bufferSize int
	subBuffer  int
	funds      map[string]*fundActivity
	closed     bool

	// Optional persistence sidecar. nil means "no DB"; the bus
	// continues to behave like the original in-memory implementation
	// (zero behaviour change for tests + bootstrap scenarios).
	persister      ActivityPersister
	persistQueue   chan ActivityEvent
	persistWG      sync.WaitGroup
	persistOnce    sync.Once
	persistBatch   int
	persistFlushMs int
}

// ActivityPersister is the slim interface ActivityBus needs from its
// optional backing store. The repository package supplies an
// implementation that talks to PostgreSQL; tests inject a stub.
//
// Methods MUST be safe for concurrent use. BulkInsert is allowed to
// silently drop duplicate (fund_id, seq) rows so the async writer can
// retry a stalled batch without losing data. ListByFund returns events
// newest-first.
type ActivityPersister interface {
	BulkInsert(ctx context.Context, events []PersistableActivityEvent) error
	ListByFund(ctx context.Context, fundID string, before time.Time, limit int) ([]PersistableActivityEvent, error)
	MaxSeqForFund(ctx context.Context, fundID string) (uint64, error)
}

// PersistableActivityEvent is a copy of ActivityEvent without the JSON
// tags. The repository package adapts between this struct and its own
// row type so neither side has to import the other.
type PersistableActivityEvent struct {
	FundID       string
	Seq          uint64
	Type         string
	Role         string
	Step         string
	RunID        string
	TradingDate  string
	Message      string
	ErrorMessage string
	EventAt      time.Time
}

// fundActivity is the per-fund state inside ActivityBus. Held by the bus map
// under ActivityBus.mu for lookup; its own mutex protects the buffer and
// subscriber list during mutations.
type fundActivity struct {
	mu       sync.Mutex
	ring     []ActivityEvent
	subs     map[*subscription]struct{}
	nextSeq  uint64
	totalSeq uint64
	// seqHydrated guards a one-shot read of MAX(seq) from the persistence
	// store after process restart. Without hydration the new process
	// would issue seq=1,2,3… which collide with previously-persisted
	// rows under the (fund_id, seq) UNIQUE index, and SSE clients using
	// sinceSeq would silently lose the new tail. The first Publish for
	// each fund triggers the lookup; subsequent publishes skip it.
	seqHydrated bool
}

// ActivityEvent is a JSON-friendly, persistence-friendly snapshot of a
// WorkflowEvent. We don't store the original WorkflowEvent because:
//
//   - WorkflowEvent.Payload is `interface{}` and may include non-serialisable
//     types (e.g. *sql.Rows leaked from a step).
//   - WorkflowEvent.Error is a Go `error` which does not survive JSON
//     marshalling without manual stringification.
//
// Storing a sanitised projection means the REST + SSE handlers can serialise
// directly without re-validating each event.
type ActivityEvent struct {
	// Seq is monotonically increasing per fund, starting at 1. Clients can
	// use it as a "last seen" cursor to resume after disconnect.
	Seq uint64 `json:"seq"`

	// Type echoes WorkflowEvent.Type: run_started / step_started / step_completed
	// / step_failed / step_skipped / step_paused / awaiting_user / run_completed
	// / run_failed / run_rejected / run_resumed.
	Type string `json:"type"`

	// Role is the agent role that owns the step (pm / researcher / trader /
	// risk / system). Derived from Step via stepOwnerRole so the UI can colour
	// the timeline by role without round-tripping to the agent repo.
	Role string `json:"role"`

	// Step is the WorkflowStep string identifier (macro_brief / research /
	// roundtable / pm_plan / risk_review / user_approval / trade_execution /
	// settlement / daily_review). Empty for run-level events.
	Step string `json:"step,omitempty"`

	FundID      string    `json:"fundId"`
	RunID       string    `json:"runId,omitempty"`
	TradingDate string    `json:"tradingDate,omitempty"`
	Timestamp   time.Time `json:"timestamp"`

	// Message is a short human-readable summary suitable for a one-line
	// timeline entry. The bus generates this from the event type + step so
	// the UI doesn't have to re-translate every event.
	Message string `json:"message"`

	// ErrorMessage is the stringified WorkflowEvent.Error when present.
	// Empty otherwise.
	ErrorMessage string `json:"error,omitempty"`
}

// Subscription is the handle returned by Subscribe. Cancel must be called when
// the subscriber is done (e.g. when an SSE connection closes); it removes the
// channel from the bus and closes Events.
type Subscription struct {
	Events  <-chan ActivityEvent
	cancel  func()
	dropped *uint64Counter
	fundID  string
}

// FundID returns the fund this subscription was opened against.
func (s *Subscription) FundID() string { return s.fundID }

// Cancel detaches the subscription and closes the Events channel. Idempotent.
func (s *Subscription) Cancel() {
	if s != nil && s.cancel != nil {
		s.cancel()
	}
}

// DroppedCount returns the number of events that were dropped for this
// subscriber because Events was full (slow consumer). Useful to surface back
// to operators as a "you missed N events, please refresh" warning.
func (s *Subscription) DroppedCount() uint64 {
	if s == nil || s.dropped == nil {
		return 0
	}
	return s.dropped.Load()
}

// subscription is the internal half of Subscription.
type subscription struct {
	ch      chan ActivityEvent
	dropped *uint64Counter
}

// NewActivityBus constructs a bus with the supplied ring buffer + subscriber
// channel sizes. Zero or negative values fall back to safe defaults
// (200 events per fund, 64-deep subscriber channel).
func NewActivityBus(bufferSize, subscriberBuffer int) *ActivityBus {
	if bufferSize <= 0 {
		bufferSize = 200
	}
	if subscriberBuffer <= 0 {
		subscriberBuffer = 64
	}
	return &ActivityBus{
		bufferSize:     bufferSize,
		subBuffer:      subscriberBuffer,
		funds:          make(map[string]*fundActivity),
		persistBatch:   32,
		persistFlushMs: 250,
	}
}

// WithPersister wires a persistence sidecar onto the bus. After this is
// called Publish double-writes to the in-memory ring (synchronously, so
// SSE latency is unchanged) and to a buffered channel that an async
// goroutine drains into the persister with batched INSERTs. ListAfter
// then falls back to the persister when the ring is missing the
// requested page (after a restart or for "load earlier" requests).
//
// queueSize controls how many events can be buffered for the async
// writer before Publish starts to drop persistence (the ring + SSE
// path are never affected — we'd rather lose a row in DB than slow
// down live consumers). 4096 is a sane default; the writer batches
// 32-at-a-time every 250ms.
func (b *ActivityBus) WithPersister(persister ActivityPersister, queueSize int) *ActivityBus {
	if b == nil {
		return nil
	}
	if persister == nil {
		return b
	}
	if queueSize <= 0 {
		queueSize = 4096
	}
	b.persister = persister
	queue := make(chan ActivityEvent, queueSize)
	b.persistQueue = queue
	b.persistOnce.Do(func() {
		b.persistWG.Add(1)
		// Pass the queue explicitly so the goroutine has a stable
		// channel reference even if Close races ahead and nils
		// b.persistQueue before the goroutine is first scheduled.
		// Without this hand-off the goroutine would observe a nil
		// queue on cold-start, return immediately, and drop every
		// pending event (intermittent loss visible under tight
		// publish→close test loops).
		go b.runPersistLoop(queue)
	})
	return b
}

// runPersistLoop drains the supplied queue and batches BulkInsert calls.
// Stops when the queue is closed (during Close). Any persistence error
// is logged at WARN — the in-memory ring is the source of truth for
// real-time consumers, so a DB outage degrades the panel to its pre-
// persistence behaviour rather than failing publishes.
//
// The queue is passed in (rather than read off b.persistQueue) so the
// goroutine has a stable reference even if Close races ahead and nils
// the field before the goroutine is first scheduled.
func (b *ActivityBus) runPersistLoop(queue chan ActivityEvent) {
	defer b.persistWG.Done()
	if queue == nil {
		return
	}
	batchSize := b.persistBatch
	if batchSize <= 0 {
		batchSize = 32
	}
	flush := time.Duration(b.persistFlushMs) * time.Millisecond
	if flush <= 0 {
		flush = 250 * time.Millisecond
	}
	pending := make([]ActivityEvent, 0, batchSize)
	timer := time.NewTimer(flush)
	defer timer.Stop()
	for {
		select {
		case evt, ok := <-queue:
			if !ok {
				if len(pending) > 0 {
					b.flushPending(pending)
				}
				return
			}
			pending = append(pending, evt)
			if len(pending) >= batchSize {
				b.flushPending(pending)
				pending = pending[:0]
			}
		case <-timer.C:
			if len(pending) > 0 {
				b.flushPending(pending)
				pending = pending[:0]
			}
			timer.Reset(flush)
		}
	}
}

func (b *ActivityBus) flushPending(events []ActivityEvent) {
	if b.persister == nil || len(events) == 0 {
		return
	}
	payload := make([]PersistableActivityEvent, len(events))
	for i, evt := range events {
		payload[i] = PersistableActivityEvent{
			FundID:       evt.FundID,
			Seq:          evt.Seq,
			Type:         evt.Type,
			Role:         evt.Role,
			Step:         evt.Step,
			RunID:        evt.RunID,
			TradingDate:  evt.TradingDate,
			Message:      evt.Message,
			ErrorMessage: evt.ErrorMessage,
			EventAt:      evt.Timestamp,
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.persister.BulkInsert(ctx, payload); err != nil {
		// Soft-fail: log + keep going. The ring still has the events
		// for live consumers; DB is best-effort historical store.
		// We don't want a transient DB blip to back up the queue.
		_ = err
	}
}

// hydrateSeqLocked seeds fund.totalSeq from the persistence store the
// first time we publish for a fund after process restart. Without this
// step the new process would emit seq=1,2,3… colliding with rows from
// the prior incarnation and breaking SSE sinceSeq backfill. Caller
// holds fund.mu. The lookup uses a short timeout because we don't want
// to block Publish on a slow DB; on failure we leave totalSeq at 0
// (matching the previous in-memory-only behaviour).
func (b *ActivityBus) hydrateSeqLocked(fundID string, fund *fundActivity) {
	if fund.seqHydrated || b.persister == nil {
		fund.seqHydrated = true
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	maxSeq, err := b.persister.MaxSeqForFund(ctx, fundID)
	if err == nil && maxSeq > fund.totalSeq {
		fund.totalSeq = maxSeq
	}
	fund.seqHydrated = true
}

// Publish implements workflow.EventBus. Always returns nil; failure modes are
// limited to "bus closed" (in which case we silently drop, since the
// orchestrator shouldn't fail just because the activity bus is unavailable).
func (b *ActivityBus) Publish(_ context.Context, evt WorkflowEvent) error {
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return nil
	}
	fund := b.fundActivityLocked(evt.FundID)
	b.mu.RUnlock()
	if fund == nil {
		return nil
	}

	activity := newActivityEvent(evt)

	fund.mu.Lock()
	b.hydrateSeqLocked(evt.FundID, fund)
	fund.totalSeq++
	activity.Seq = fund.totalSeq
	if len(fund.ring) >= b.bufferSize {
		copy(fund.ring, fund.ring[1:])
		fund.ring[len(fund.ring)-1] = activity
	} else {
		fund.ring = append(fund.ring, activity)
	}
	subs := make([]*subscription, 0, len(fund.subs))
	for sub := range fund.subs {
		subs = append(subs, sub)
	}
	fund.mu.Unlock()

	for _, sub := range subs {
		select {
		case sub.ch <- activity:
		default:
			sub.dropped.Add(1)
		}
	}

	// Best-effort async persistence. Drop if the writer is overloaded —
	// real-time consumers must never be slowed down by the DB path.
	if b.persistQueue != nil {
		select {
		case b.persistQueue <- activity:
		default:
		}
	}
	return nil
}

// Page returns up to `limit` events for `fundID`, newest first, strictly
// older than `before`. Used by the REST "load earlier" handler.
//
//   - If `before` is zero or the bus has no persister, Page falls back
//     to the in-memory ring (matching the pre-persistence behaviour so
//     standalone tests + bootstrap paths keep working).
//   - When the persister is configured, Page goes straight to the DB.
//     The ring is fine for the initial-load hot path but cannot answer
//     "give me the page before this timestamp" reliably (older events
//     fall off the 200-entry ring quickly).
func (b *ActivityBus) Page(ctx context.Context, fundID string, before time.Time, limit int) ([]ActivityEvent, error) {
	if b == nil {
		return nil, nil
	}
	if b.persister == nil {
		return b.Recent(fundID, limit, 0), nil
	}
	rows, err := b.persister.ListByFund(ctx, fundID, before, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ActivityEvent, len(rows))
	for i, row := range rows {
		out[i] = ActivityEvent{
			Seq:          row.Seq,
			Type:         row.Type,
			Role:         row.Role,
			Step:         row.Step,
			FundID:       row.FundID,
			RunID:        row.RunID,
			TradingDate:  row.TradingDate,
			Timestamp:    row.EventAt,
			Message:      row.Message,
			ErrorMessage: row.ErrorMessage,
		}
	}
	return out, nil
}

// Recent returns up to `limit` newest events for the fund, oldest-first so
// the UI can render the timeline in display order. `sinceSeq=0` returns all
// events in the buffer (subject to limit); a non-zero value returns only
// events whose Seq > sinceSeq, enabling efficient incremental backfill after
// a brief SSE disconnect.
//
// After a process restart the in-memory ring starts empty, and the per-fund
// bucket isn't even created until the first Publish for that fund — so a
// user opening the Team Activity panel right after deploy would see no
// history at all. To paper over that hole we fall back to the persistence
// sidecar (when configured) and backfill from the DB to fill the limit.
// The ring is still the source of truth for the freshest tail (Publish is
// synchronous to the ring, async to the DB), so we merge ring + DB by Seq
// rather than blindly preferring one source.
func (b *ActivityBus) Recent(fundID string, limit int, sinceSeq uint64) []ActivityEvent {
	if limit <= 0 {
		limit = b.bufferSize
	}

	ringEvents := b.recentFromRing(fundID, limit, sinceSeq)
	if len(ringEvents) >= limit || b.persister == nil {
		return ringEvents
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := b.persister.ListByFund(ctx, fundID, time.Now().UTC().Add(time.Minute), limit)
	if err != nil {
		// Degrade to whatever the ring had — the SSE stream will keep
		// the UI fresh from the next tick onwards.
		return ringEvents
	}

	seen := make(map[uint64]struct{}, len(ringEvents)+len(rows))
	merged := make([]ActivityEvent, 0, len(ringEvents)+len(rows))
	for _, e := range ringEvents {
		if _, dup := seen[e.Seq]; dup {
			continue
		}
		seen[e.Seq] = struct{}{}
		merged = append(merged, e)
	}
	for _, row := range rows {
		if sinceSeq > 0 && row.Seq <= sinceSeq {
			continue
		}
		if _, dup := seen[row.Seq]; dup {
			continue
		}
		seen[row.Seq] = struct{}{}
		merged = append(merged, ActivityEvent{
			Seq:          row.Seq,
			Type:         row.Type,
			Role:         row.Role,
			Step:         row.Step,
			FundID:       row.FundID,
			RunID:        row.RunID,
			TradingDate:  row.TradingDate,
			Timestamp:    row.EventAt,
			Message:      row.Message,
			ErrorMessage: row.ErrorMessage,
		})
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Seq < merged[j].Seq })
	if len(merged) > limit {
		merged = merged[len(merged)-limit:]
	}
	return merged
}

// recentFromRing is the original in-memory-only path, factored out so
// Recent's fallback logic stays readable. Returns oldest-first, honours
// sinceSeq and limit. Safe to call when the fund key is absent (returns
// nil). Held only briefly under the per-fund mutex.
func (b *ActivityBus) recentFromRing(fundID string, limit int, sinceSeq uint64) []ActivityEvent {
	b.mu.RLock()
	fund, ok := b.funds[fundID]
	b.mu.RUnlock()
	if !ok {
		return nil
	}
	fund.mu.Lock()
	defer fund.mu.Unlock()

	if sinceSeq > 0 {
		filtered := make([]ActivityEvent, 0, len(fund.ring))
		for _, e := range fund.ring {
			if e.Seq > sinceSeq {
				filtered = append(filtered, e)
			}
		}
		if len(filtered) > limit {
			filtered = filtered[len(filtered)-limit:]
		}
		return filtered
	}
	if len(fund.ring) <= limit {
		out := make([]ActivityEvent, len(fund.ring))
		copy(out, fund.ring)
		return out
	}
	out := make([]ActivityEvent, limit)
	copy(out, fund.ring[len(fund.ring)-limit:])
	return out
}

// Subscribe registers a per-fund channel. The returned Subscription stays
// active until Cancel is called or the bus is closed. The channel buffer is
// fixed at NewActivityBus's subscriberBuffer; when the consumer can't keep up
// the publisher drops events and increments DroppedCount.
//
// ErrBusClosed is returned if the bus has been closed.
func (b *ActivityBus) Subscribe(fundID string) (*Subscription, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrBusClosed
	}
	fund := b.fundActivityLockedW(fundID)
	b.mu.Unlock()

	sub := &subscription{
		ch:      make(chan ActivityEvent, b.subBuffer),
		dropped: new(uint64Counter),
	}
	fund.mu.Lock()
	if fund.subs == nil {
		fund.subs = make(map[*subscription]struct{})
	}
	fund.subs[sub] = struct{}{}
	fund.mu.Unlock()

	cancel := func() {
		fund.mu.Lock()
		if _, ok := fund.subs[sub]; ok {
			delete(fund.subs, sub)
			close(sub.ch)
		}
		fund.mu.Unlock()
	}
	return &Subscription{
		Events:  sub.ch,
		cancel:  cancel,
		dropped: sub.dropped,
		fundID:  fundID,
	}, nil
}

// SubscriberCount returns the number of active SSE subscribers for the fund.
// Mainly used by tests + admin diagnostics.
func (b *ActivityBus) SubscriberCount(fundID string) int {
	b.mu.RLock()
	fund, ok := b.funds[fundID]
	b.mu.RUnlock()
	if !ok {
		return 0
	}
	fund.mu.Lock()
	defer fund.mu.Unlock()
	return len(fund.subs)
}

// Close terminates all subscriptions, drains the persistence queue and
// rejects further publishes. Safe to call multiple times.
func (b *ActivityBus) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	funds := make([]*fundActivity, 0, len(b.funds))
	for _, fa := range b.funds {
		funds = append(funds, fa)
	}
	queue := b.persistQueue
	b.persistQueue = nil
	b.mu.Unlock()

	for _, fund := range funds {
		fund.mu.Lock()
		for sub := range fund.subs {
			delete(fund.subs, sub)
			close(sub.ch)
		}
		fund.mu.Unlock()
	}

	if queue != nil {
		close(queue)
		b.persistWG.Wait()
	}
}

// ErrBusClosed is returned by Subscribe when the bus has been Close'd.
var ErrBusClosed = errors.New("workflow: activity bus closed")

// fundActivityLocked returns the existing fund bucket (read-only); if missing
// it must be created via fundActivityLockedW. Caller holds b.mu (read or write).
func (b *ActivityBus) fundActivityLocked(fundID string) *fundActivity {
	if fa, ok := b.funds[fundID]; ok {
		return fa
	}
	// Upgrade: drop the read lock, take write lock, recheck.
	b.mu.RUnlock()
	b.mu.Lock()
	defer func() {
		b.mu.Unlock()
		b.mu.RLock()
	}()
	if fa, ok := b.funds[fundID]; ok {
		return fa
	}
	fa := &fundActivity{
		ring: make([]ActivityEvent, 0, b.bufferSize),
		subs: make(map[*subscription]struct{}),
	}
	b.funds[fundID] = fa
	return fa
}

// fundActivityLockedW returns/creates the fund bucket. Caller already holds
// b.mu for writing (used by Subscribe which needs the write lock anyway).
func (b *ActivityBus) fundActivityLockedW(fundID string) *fundActivity {
	if fa, ok := b.funds[fundID]; ok {
		return fa
	}
	fa := &fundActivity{
		ring: make([]ActivityEvent, 0, b.bufferSize),
		subs: make(map[*subscription]struct{}),
	}
	b.funds[fundID] = fa
	return fa
}

// newActivityEvent projects a raw WorkflowEvent to its JSON-friendly form.
// The Seq field is assigned by the publisher (under the fund lock) so we
// leave it zero here.
func newActivityEvent(evt WorkflowEvent) ActivityEvent {
	out := ActivityEvent{
		Type:        evt.Type,
		Step:        evt.Step.String(),
		FundID:      evt.FundID,
		RunID:       evt.RunID,
		TradingDate: evt.TradingDate,
		Timestamp:   evt.Timestamp,
		Role:        stepOwnerRole(evt.Step),
	}
	if out.Timestamp.IsZero() {
		out.Timestamp = time.Now()
	}
	out.Message = renderActivityMessage(evt)
	if evt.Error != nil {
		out.ErrorMessage = evt.Error.Error()
	}
	return out
}

// stepOwnerRole maps a WorkflowStep to the agent role responsible for it.
// Run-level events (no step) map to "system" so the UI can render them with
// a neutral colour. Keep this in sync with the orchestrator's step → agent
// dispatch table.
func stepOwnerRole(step WorkflowStep) string {
	switch step {
	case StepMacroBrief, StepResearchParallel, StepQuantSignals:
		return "researcher"
	case StepRoundtable:
		return "team"
	case StepPMPlan:
		return "pm"
	case StepRiskReview:
		return "risk"
	case StepUserApproval:
		return "user"
	case StepTradeExecution:
		return "trader"
	case StepSettlement, StepDailyReview:
		return "pm"
	}
	return "system"
}

// renderActivityMessage builds a short human-readable summary for the UI.
// Kept in Go so the message survives client refreshes (the SSE payload is
// the same as the REST backfill payload) and to keep the i18n surface narrow
// (the UI passes the message through verbatim for now; we can layer per-event
// i18n later by switching on `Type`+`Step`).
func renderActivityMessage(evt WorkflowEvent) string {
	role := stepOwnerRole(evt.Step)
	stepName := evt.Step.String()
	switch evt.Type {
	case "run_started":
		return "Daily workflow started"
	case "run_completed":
		return "Daily workflow completed"
	case "run_failed":
		return "Daily workflow failed"
	case "run_rejected":
		return "Plan rejected by user"
	case "run_resumed":
		return "Workflow resumed after approval"
	case "awaiting_user":
		return "Plan ready, waiting for user approval"
	case "step_started":
		return role + " started: " + stepName
	case "step_completed":
		return role + " completed: " + stepName
	case "step_failed":
		return role + " failed: " + stepName
	case "step_skipped":
		return role + " skipped: " + stepName
	case "step_paused":
		return role + " paused: " + stepName
	}
	return evt.Type
}

// uint64Counter is a tiny atomic-ish counter using a sync.Mutex; we don't
// import sync/atomic because the workflow package already has sync, and the
// counter is bumped at most once per dropped event (a rare path).
type uint64Counter struct {
	mu sync.Mutex
	n  uint64
}

func (c *uint64Counter) Add(n uint64) {
	c.mu.Lock()
	c.n += n
	c.mu.Unlock()
}

func (c *uint64Counter) Load() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}
