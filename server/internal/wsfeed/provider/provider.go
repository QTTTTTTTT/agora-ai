// Package wsfeedprovider houses provider implementations for
// the wsfeed manager. Today we ship two:
//
//   - MockProvider: deterministic, test-driven; pushes ticks
//     when the test calls Emit / EmitTrade. Used by every
//     unit test that exercises the WS hot path.
//
//   - NopProvider: connects "successfully" without an actual
//     network call and emits nothing. The default provider
//     in prod when no real WS endpoint is configured —
//     keeps the wsfeed infrastructure live so the broker can
//     subscribe / unsubscribe normally; quotes simply fall
//     through to REST via the cache miss path.
//
// Real provider implementations (Polygon, Alpaca, …) belong
// here as separate files; each is a thin wrapper over a WS
// client library plus the protocol-specific subscribe / parse
// logic. None ship in S6.5 — that's a follow-up PR per provider.
package wsfeedprovider

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/wsfeed"
)

// ----- NopProvider -----

// NopProvider is the default provider in prod environments
// that don't have a paid WS endpoint configured. It claims to
// be connected and silently ignores subscribe / unsubscribe
// calls.
//
// Why not just leave the manager empty? Because the broker
// and position refresher always Subscribe + always read from
// the cache. With NopProvider the read path falls through to
// REST naturally; without any provider, Subscribe would
// silently no-op and admin observability would lose visibility
// into "WS is not configured" vs "WS is broken".
type NopProvider struct {
	name   string
	events chan<- wsfeed.Event
	mu     sync.Mutex
	state  wsfeed.ConnState
}

// NewNop builds a Nop provider with the supplied name.
func NewNop(name string) *NopProvider {
	if name == "" {
		name = "nop"
	}
	return &NopProvider{name: name, state: wsfeed.StateUnknown}
}

func (p *NopProvider) Name() string { return p.name }

func (p *NopProvider) State() wsfeed.ConnState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

func (p *NopProvider) Start(events chan<- wsfeed.Event) error {
	if events == nil {
		return errors.New("nop: nil events channel")
	}
	p.mu.Lock()
	p.events = events
	p.state = wsfeed.StateConnected
	p.mu.Unlock()
	// Send a state event so the manager knows we're up.
	p.send(wsfeed.Event{
		Kind: wsfeed.EventState,
		Tick: wsfeed.Tick{Provider: p.name},
		State: wsfeed.StateConnected,
	})
	return nil
}

func (p *NopProvider) Stop() {
	p.mu.Lock()
	wasOpen := p.state != wsfeed.StateClosed
	p.state = wsfeed.StateClosed
	p.mu.Unlock()
	if wasOpen {
		p.send(wsfeed.Event{
			Kind:  wsfeed.EventState,
			Tick:  wsfeed.Tick{Provider: p.name},
			State: wsfeed.StateClosed,
		})
	}
}

func (p *NopProvider) Subscribe(_ []wsfeed.Subscription) error { return nil }
func (p *NopProvider) Unsubscribe(_ []string) error             { return nil }

func (p *NopProvider) send(ev wsfeed.Event) {
	p.mu.Lock()
	ch := p.events
	closed := p.state == wsfeed.StateClosed
	p.mu.Unlock()
	if ch == nil {
		return
	}
	// Non-blocking; Stop() may have raced.
	defer func() { _ = recover() }()
	if closed && ev.Kind != wsfeed.EventState {
		return
	}
	select {
	case ch <- ev:
	default:
	}
}

// ----- MockProvider -----

// MockProvider is the test-grade provider. Tests construct
// one, hand it to a Manager, then call EmitTrade / EmitQuote
// / Fail / Reconnect to drive scenarios deterministically.
type MockProvider struct {
	name string

	mu       sync.Mutex
	state    wsfeed.ConnState
	events   chan<- wsfeed.Event
	subs     map[string]wsfeed.Subscription
	startErr error

	// observability — tests assert against these.
	subscribeCalls   int
	unsubscribeCalls int
	resubscribeCalls int
	pendingResub     bool // set on Disconnect; consumed by next Subscribe
}

// NewMock builds a mock provider.
func NewMock(name string) *MockProvider {
	if name == "" {
		name = "mock"
	}
	return &MockProvider{
		name:  name,
		state: wsfeed.StateUnknown,
		subs:  make(map[string]wsfeed.Subscription),
	}
}

// SetStartError makes the next Start return err. Used by
// tests that exercise the "provider failed to come up" path.
func (p *MockProvider) SetStartError(err error) {
	p.mu.Lock()
	p.startErr = err
	p.mu.Unlock()
}

func (p *MockProvider) Name() string { return p.name }

func (p *MockProvider) State() wsfeed.ConnState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

func (p *MockProvider) Start(events chan<- wsfeed.Event) error {
	if events == nil {
		return errors.New("mock: nil events channel")
	}
	p.mu.Lock()
	if p.startErr != nil {
		err := p.startErr
		p.startErr = nil
		p.mu.Unlock()
		return err
	}
	p.events = events
	p.state = wsfeed.StateConnected
	p.mu.Unlock()
	p.sendEvent(wsfeed.Event{
		Kind:  wsfeed.EventState,
		Tick:  wsfeed.Tick{Provider: p.name},
		State: wsfeed.StateConnected,
	})
	return nil
}

func (p *MockProvider) Stop() {
	p.mu.Lock()
	was := p.state
	p.state = wsfeed.StateClosed
	p.mu.Unlock()
	if was != wsfeed.StateClosed {
		p.sendEvent(wsfeed.Event{
			Kind:  wsfeed.EventState,
			Tick:  wsfeed.Tick{Provider: p.name},
			State: wsfeed.StateClosed,
		})
	}
}

func (p *MockProvider) Subscribe(subs []wsfeed.Subscription) error {
	if len(subs) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != wsfeed.StateConnected {
		return wsfeed.ErrNotConnected
	}
	for _, s := range subs {
		s = s.Normalize()
		if !s.Valid() {
			continue
		}
		p.subs[s.Symbol] = s
	}
	p.subscribeCalls++
	if p.pendingResub {
		p.resubscribeCalls++
		p.pendingResub = false
	}
	return nil
}

func (p *MockProvider) Unsubscribe(symbols []string) error {
	if len(symbols) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != wsfeed.StateConnected {
		return wsfeed.ErrNotConnected
	}
	for _, sym := range symbols {
		sym = strings.ToUpper(strings.TrimSpace(sym))
		delete(p.subs, sym)
	}
	p.unsubscribeCalls++
	return nil
}

// SubscribedSymbols returns a snapshot of currently subscribed
// symbols. Test helper.
func (p *MockProvider) SubscribedSymbols() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.subs))
	for sym := range p.subs {
		out = append(out, sym)
	}
	return out
}

// SubscribeCalls returns how many times Subscribe was invoked
// with at least one symbol (ignoring re-subscribes).
func (p *MockProvider) SubscribeCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.subscribeCalls
}

// UnsubscribeCalls returns how many times Unsubscribe was
// invoked with at least one symbol.
func (p *MockProvider) UnsubscribeCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unsubscribeCalls
}

// ResubscribeCalls returns how many times Subscribe was
// invoked when there were already existing subs (the manager's
// reconnect-resubscribe path).
func (p *MockProvider) ResubscribeCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resubscribeCalls
}

// EmitTrade pushes a trade tick. Test helper.
func (p *MockProvider) EmitTrade(symbol string, price, size float64) {
	p.emit(wsfeed.TickTrade, symbol, price, size, 0, 0, 0, 0)
}

// EmitQuote pushes a top-of-book quote tick.
func (p *MockProvider) EmitQuote(symbol string, bid, bidSize, ask, askSize float64) {
	p.emit(wsfeed.TickQuote, symbol, 0, 0, bid, bidSize, ask, askSize)
}

// EmitSnapshot pushes a snapshot tick with both trade and
// quote fields populated.
func (p *MockProvider) EmitSnapshot(symbol string, last, bid, ask float64) {
	p.emit(wsfeed.TickSnapshot, symbol, last, 0, bid, 0, ask, 0)
}

func (p *MockProvider) emit(kind wsfeed.TickType, symbol string, last, size, bid, bidSize, ask, askSize float64) {
	now := time.Now().UTC()
	p.sendEvent(wsfeed.Event{
		Kind: wsfeed.EventTick,
		Tick: wsfeed.Tick{
			Symbol:        strings.ToUpper(strings.TrimSpace(symbol)),
			DisplaySymbol: symbol,
			Provider:      p.name,
			EventType:     kind,
			Last:          last,
			Size:          size,
			Bid:           bid,
			BidSize:       bidSize,
			Ask:           ask,
			AskSize:       askSize,
			Timestamp:     now,
			ReceivedAt:    now,
		},
	})
}

// Disconnect simulates an upstream socket drop. The manager
// will see the state event and bump reconnect counters.
func (p *MockProvider) Disconnect(reason string) {
	p.mu.Lock()
	p.state = wsfeed.StateReconnecting
	p.pendingResub = true
	p.mu.Unlock()
	p.sendEvent(wsfeed.Event{
		Kind:  wsfeed.EventState,
		Tick:  wsfeed.Tick{Provider: p.name},
		State: wsfeed.StateReconnecting,
		Error: reason,
	})
}

// Reconnect simulates a successful reconnect handshake.
func (p *MockProvider) Reconnect() {
	p.mu.Lock()
	p.state = wsfeed.StateConnected
	p.mu.Unlock()
	p.sendEvent(wsfeed.Event{
		Kind:  wsfeed.EventState,
		Tick:  wsfeed.Tick{Provider: p.name},
		State: wsfeed.StateConnected,
	})
}

func (p *MockProvider) sendEvent(ev wsfeed.Event) {
	p.mu.Lock()
	ch := p.events
	closed := p.state == wsfeed.StateClosed
	p.mu.Unlock()
	if ch == nil {
		return
	}
	defer func() { _ = recover() }()
	if closed && ev.Kind != wsfeed.EventState {
		return
	}
	ch <- ev
}
