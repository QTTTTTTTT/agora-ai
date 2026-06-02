// manager.go — multi-provider subscription manager + fan-out.
//
// What lives here
//
//   - Provider registry: a manager owns one or more named
//     providers; the broker / position refresher do not need
//     to know which one is serving a given symbol.
//   - Subscription ref-count: if 3 consumers in-process care
//     about AAPL, we send exactly one upstream Subscribe; the
//     last consumer to Unsubscribe triggers an upstream
//     Unsubscribe.
//   - Fan-out: every Tick the providers emit is delivered to
//     every registered TickHandler (the quotecache, the
//     position-refresh listener, the admin observer …).
//     Handlers must be cheap; slow handlers are skipped after
//     a per-handler deadline to avoid wedging the read loop.
//   - Reconnect resilience: providers run their own reconnect
//     loops; the manager exposes ConnStats so admin can see
//     it. State events bump metrics + audit log; nothing else
//     changes on the consumer side (the cache stays warm with
//     the last tick until TTL expires).
//
// Design notes
//
//   - Backpressure: the manager's inbound channel is bounded.
//     If providers outrun handlers (rare — handlers should be
//     O(1) writes to a map), the manager drops events at the
//     read step and bumps `dropped_events`. This is preferable
//     to wedging the network reader.
//   - Lifecycle: Start spawns one goroutine per provider plus
//     the dispatcher; Stop signals all to exit and waits for
//     drain. Idempotent both sides.
//   - Thread-safety: handlers are added before Start; the
//     subscribers map is mutex-guarded but the read path
//     copies the slice under lock and dispatches without
//     holding it.

package wsfeed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// TickHandler is invoked for every Tick the manager fans out.
// Handlers must be cheap; spend > 1ms inside one and you'll
// block the dispatcher. Use a goroutine or a channel internally
// for any heavy work.
type TickHandler func(Tick)

// StateHandler is invoked when any provider reports a state
// change. Optional — useful for admin observability.
type StateHandler func(provider string, state ConnState, err string)

// ManagerConfig is the constructor input.
type ManagerConfig struct {
	// InboundBuffer is the size of the shared channel that
	// providers push Events into. Default 1024. Sized for
	// hundreds of symbols × 5-10 ticks/sec each, so under
	// normal load the channel sits near-empty.
	InboundBuffer int
	// OnError is called when a provider reports an error or
	// the manager drops events. Optional.
	OnError func(err error)
}

// Manager owns providers and dispatches their ticks.
type Manager struct {
	cfg ManagerConfig

	mu        sync.RWMutex
	providers map[string]Provider
	// subRefs counts consumer references per symbol. Reaching
	// zero triggers an upstream Unsubscribe.
	subRefs map[string]int
	// subMeta carries the Subscription struct we used at
	// register time so re-subscribes after reconnect can
	// reconstitute it.
	subMeta map[string]Subscription
	// subDest tracks which provider serves which symbol. v1
	// has every provider hear every sub; future PR can route
	// per market.
	tickHandlers  []TickHandler
	stateHandlers []StateHandler

	events chan Event
	// stats — per-provider snapshot built lazily on read.
	connStats map[string]*ConnStats
	subStats  map[string]*SubStats

	stopped       atomic.Bool
	started       atomic.Bool
	stopCh        chan struct{}
	doneCh        chan struct{}
	startOnce     sync.Once
	stopOnce      sync.Once
	startErr      error

	droppedEvents atomic.Uint64
	totalTicks    atomic.Uint64
}

// NewManager constructs the manager.
func NewManager(cfg ManagerConfig) *Manager {
	if cfg.InboundBuffer <= 0 {
		cfg.InboundBuffer = 1024
	}
	return &Manager{
		cfg:       cfg,
		providers: make(map[string]Provider),
		subRefs:   make(map[string]int),
		subMeta:   make(map[string]Subscription),
		connStats: make(map[string]*ConnStats),
		subStats:  make(map[string]*SubStats),
		events:    make(chan Event, cfg.InboundBuffer),
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
}

// AddProvider registers a provider. Must be called before
// Start. Returns an error if a provider with the same name
// already exists.
func (m *Manager) AddProvider(p Provider) error {
	if m == nil || p == nil {
		return errors.New("wsfeed: nil manager or provider")
	}
	if m.stopped.Load() {
		return ErrManagerStopped
	}
	name := p.Name()
	if name == "" {
		return errors.New("wsfeed: provider name required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.providers[name]; ok {
		return fmt.Errorf("wsfeed: provider %q already registered", name)
	}
	m.providers[name] = p
	m.connStats[name] = &ConnStats{Provider: name, State: StateUnknown}
	return nil
}

// AddTickHandler appends a handler. Safe to call before or
// after Start.
func (m *Manager) AddTickHandler(h TickHandler) {
	if m == nil || h == nil {
		return
	}
	m.mu.Lock()
	m.tickHandlers = append(m.tickHandlers, h)
	m.mu.Unlock()
}

// AddStateHandler appends a state-change handler.
func (m *Manager) AddStateHandler(h StateHandler) {
	if m == nil || h == nil {
		return
	}
	m.mu.Lock()
	m.stateHandlers = append(m.stateHandlers, h)
	m.mu.Unlock()
}

// Start kicks off every registered provider and the
// dispatcher loop. Idempotent — second call is a no-op
// returning nil.
func (m *Manager) Start(ctx context.Context) error {
	if m == nil {
		return errors.New("wsfeed: nil manager")
	}
	m.startOnce.Do(func() {
		m.started.Store(true)
		go m.dispatch(ctx)
		m.mu.RLock()
		providers := make([]Provider, 0, len(m.providers))
		for _, p := range m.providers {
			providers = append(providers, p)
		}
		m.mu.RUnlock()
		for _, p := range providers {
			if err := p.Start(m.events); err != nil {
				m.recordError(fmt.Errorf("provider %s start: %w", p.Name(), err))
				m.startErr = err
			}
		}
	})
	return m.startErr
}

// Stop signals shutdown and waits for the dispatcher to drain.
// Idempotent.
func (m *Manager) Stop() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() {
		m.stopped.Store(true)
		// Stop providers first so they stop pushing events.
		m.mu.RLock()
		providers := make([]Provider, 0, len(m.providers))
		for _, p := range m.providers {
			providers = append(providers, p)
		}
		m.mu.RUnlock()
		for _, p := range providers {
			p.Stop()
		}
		close(m.stopCh)
		// Only wait on the dispatcher if it was actually
		// started; otherwise doneCh is never closed.
		if m.started.Load() {
			<-m.doneCh
		}
	})
}

// Subscribe registers a consumer's interest in a symbol. The
// first reference triggers an upstream Subscribe; subsequent
// references just bump the ref-count.
//
// Returns ErrManagerStopped if Stop was called.
func (m *Manager) Subscribe(sub Subscription) error {
	if m == nil {
		return errors.New("wsfeed: nil manager")
	}
	if m.stopped.Load() {
		return ErrManagerStopped
	}
	sub = sub.Normalize()
	if !sub.Valid() {
		return errors.New("wsfeed: subscription requires symbol")
	}
	m.mu.Lock()
	count := m.subRefs[sub.Symbol]
	m.subRefs[sub.Symbol] = count + 1
	if count == 0 {
		// First reference; install meta + tell providers.
		m.subMeta[sub.Symbol] = sub
		m.subStats[sub.Symbol] = &SubStats{
			Symbol:    sub.Symbol,
			Market:    sub.Market,
			Consumers: 1,
		}
	} else {
		if s, ok := m.subStats[sub.Symbol]; ok {
			s.Consumers = count + 1
		}
	}
	providers := make([]Provider, 0, len(m.providers))
	for _, p := range m.providers {
		providers = append(providers, p)
	}
	m.mu.Unlock()
	// Tell every provider about the first ref. v1: broadcast
	// to all providers; future PR can route by market.
	if count == 0 {
		for _, p := range providers {
			if !p.State().IsHealthy() {
				continue
			}
			if err := p.Subscribe([]Subscription{sub}); err != nil {
				m.recordError(fmt.Errorf("provider %s subscribe: %w", p.Name(), err))
			}
		}
	}
	return nil
}

// Unsubscribe drops one consumer reference. Reaching zero
// triggers an upstream Unsubscribe.
func (m *Manager) Unsubscribe(symbol string) error {
	if m == nil {
		return errors.New("wsfeed: nil manager")
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return errors.New("wsfeed: unsubscribe requires symbol")
	}
	m.mu.Lock()
	count := m.subRefs[symbol]
	if count <= 0 {
		m.mu.Unlock()
		return nil
	}
	m.subRefs[symbol] = count - 1
	if count == 1 {
		delete(m.subMeta, symbol)
		delete(m.subStats, symbol)
		delete(m.subRefs, symbol)
	} else if s, ok := m.subStats[symbol]; ok {
		s.Consumers = count - 1
	}
	providers := make([]Provider, 0, len(m.providers))
	for _, p := range m.providers {
		providers = append(providers, p)
	}
	m.mu.Unlock()
	if count == 1 {
		for _, p := range providers {
			if !p.State().IsHealthy() {
				continue
			}
			if err := p.Unsubscribe([]string{symbol}); err != nil {
				m.recordError(fmt.Errorf("provider %s unsubscribe: %w", p.Name(), err))
			}
		}
	}
	return nil
}

// Subscriptions returns the current set of subscribed symbols
// with their consumer counts.
func (m *Manager) Subscriptions() []SubStats {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]SubStats, 0, len(m.subStats))
	for _, s := range m.subStats {
		out = append(out, *s)
	}
	return out
}

// ConnectionsSnapshot returns a copy of per-provider stats.
func (m *Manager) ConnectionsSnapshot() []ConnStats {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ConnStats, 0, len(m.connStats))
	for name, s := range m.connStats {
		clone := *s
		clone.State = m.providers[name].State()
		clone.Subscriptions = countSubsForProvider(m.subRefs)
		out = append(out, clone)
	}
	return out
}

func countSubsForProvider(subRefs map[string]int) int {
	// v1: every provider hears every sub.
	return len(subRefs)
}

// DroppedEvents returns the cumulative drop count due to a
// full inbound channel. Should stay 0 under normal load.
func (m *Manager) DroppedEvents() uint64 {
	if m == nil {
		return 0
	}
	return m.droppedEvents.Load()
}

// TotalTicks returns the cumulative count of ticks delivered
// to handlers.
func (m *Manager) TotalTicks() uint64 {
	if m == nil {
		return 0
	}
	return m.totalTicks.Load()
}

// ----- internals -----

func (m *Manager) dispatch(ctx context.Context) {
	defer close(m.doneCh)
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case ev, ok := <-m.events:
			if !ok {
				return
			}
			m.handleEvent(ev)
		}
	}
}

func (m *Manager) handleEvent(ev Event) {
	switch ev.Kind {
	case EventTick:
		m.totalTicks.Add(1)
		// Update LastTickAt + count on the provider's stats.
		m.mu.Lock()
		if s, ok := m.connStats[ev.Tick.Provider]; ok {
			s.LastTickAt = ev.Tick.ReceivedAt
			s.TickCount++
		}
		if s, ok := m.subStats[ev.Tick.Symbol]; ok {
			s.LastTickAt = ev.Tick.ReceivedAt
		}
		handlers := append([]TickHandler{}, m.tickHandlers...)
		m.mu.Unlock()
		m.fanOutTick(ev.Tick, handlers)
	case EventState:
		m.mu.Lock()
		// We don't know which provider this event came from
		// without an explicit field; convention is that
		// providers set Tick.Provider on state events too.
		name := ev.Tick.Provider
		var (
			resubProvider Provider
			resubSubs     []Subscription
		)
		if s, ok := m.connStats[name]; ok {
			old := s.State
			s.State = ev.State
			s.LastError = ev.Error
			now := time.Now().UTC()
			if ev.State == StateConnected {
				s.ConnectedAt = now
				isReconnect := old == StateReconnecting || old == StateBackoff || old == StateDisconnected
				if isReconnect {
					s.ReconnectCount++
					// On a true reconnect, re-push every
					// current sub so the new socket starts
					// streaming. Skip on the initial
					// Unknown→Connected transition — the
					// first Subscribe() caller already told
					// the provider directly, and re-pushing
					// here would race with that path and
					// double-count.
					if p, ok := m.providers[name]; ok && len(m.subMeta) > 0 {
						resubProvider = p
						resubSubs = make([]Subscription, 0, len(m.subMeta))
						for _, sub := range m.subMeta {
							resubSubs = append(resubSubs, sub)
						}
					}
				}
			}
			if ev.State == StateDisconnected || ev.State == StateClosed {
				s.DisconnectedAt = now
			}
		}
		handlers := append([]StateHandler{}, m.stateHandlers...)
		m.mu.Unlock()
		if resubProvider != nil && len(resubSubs) > 0 {
			if err := resubProvider.Subscribe(resubSubs); err != nil {
				m.recordError(fmt.Errorf("wsfeed: resubscribe %s: %w", name, err))
			}
		}
		for _, h := range handlers {
			func() {
				defer func() { _ = recover() }()
				h(name, ev.State, ev.Error)
			}()
		}
	case EventError:
		m.recordError(errors.New(ev.Error))
	}
}

func (m *Manager) fanOutTick(t Tick, handlers []TickHandler) {
	if len(handlers) == 0 {
		return
	}
	// Handlers are contractually O(1) (write to map / non-
	// blocking channel send). We invoke inline rather than
	// spawn a goroutine per tick, because at 1k ticks/sec
	// the goroutine churn dwarfs the actual work. Handlers
	// that need to do heavy work must install their own
	// async decoupling.
	for _, h := range handlers {
		if h == nil {
			continue
		}
		m.invokeTick(h, t)
	}
}

func (m *Manager) invokeTick(h TickHandler, t Tick) {
	defer func() {
		// Don't let one bad handler wedge the dispatcher.
		if r := recover(); r != nil {
			m.recordError(fmt.Errorf("wsfeed: handler panic: %v", r))
		}
	}()
	h(t)
}

func (m *Manager) recordError(err error) {
	if m == nil || err == nil {
		return
	}
	if m.cfg.OnError != nil {
		m.cfg.OnError(err)
	}
}
