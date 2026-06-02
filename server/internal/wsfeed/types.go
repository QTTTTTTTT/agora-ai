// Package wsfeed implements the S6.5 WebSocket market-data
// feed. Real-time ticks replace REST polling on the broker /
// position-refresh hot paths.
//
// Layering
//
//	┌──────────────────┐
//	│ Upstream WS      │  (Polygon, Alpaca, mock provider, …)
//	│ Provider         │
//	└────────┬─────────┘
//	         │ Ticks
//	         ▼
//	┌──────────────────┐
//	│ wsfeed.Manager   │  owns N providers, multiplexes
//	│ - sub ref-count  │  symbol subscriptions, runs the
//	│ - fan-out        │  reconnect supervisor, fans ticks
//	│ - reconnect      │  out to in-process subscribers.
//	└────────┬─────────┘
//	         │ Tick events
//	         ▼
//	┌──────────────────┐
//	│ quotecache       │  last-tick-per-symbol with TTL.
//	└────────┬─────────┘
//	         │ sync lookup
//	         ▼
//	┌──────────────────┐
//	│ broker.Simulator │  hot-path quote lookup. Falls back
//	│ + holdings loop  │  to REST when cache is cold / stale.
//	└──────────────────┘
//
// What this package owns
//
//   - Tick / TickType / ConnState / Subscription value types.
//   - Provider interface + provider lifecycle contract.
//   - Manager (subscription ref counting, fan-out, reconnect
//     supervisor, observability snapshots).
//
// What it does NOT own
//
//   - Provider implementations (see internal/wsfeed/provider/…)
//   - The last-tick cache (internal/quotecache).
//   - Web / admin wiring (cmd/server).

package wsfeed

import (
	"errors"
	"strings"
	"time"
)

// TickType is the closed enum used to discriminate the payload
// in a Tick. Most consumers care only about the "trade" type
// (last price + size) but the broker / risk layer can also
// listen for "quote" updates to refresh bid/ask spreads.
type TickType string

const (
	// TickTrade — a fill happened on the exchange. Last / Size
	// are set.
	TickTrade TickType = "trade"
	// TickQuote — top-of-book quote changed. Bid / BidSize /
	// Ask / AskSize are set; Last may be 0 if the provider
	// separates trade and quote streams.
	TickQuote TickType = "quote"
	// TickSnapshot — a one-shot full snapshot pushed after
	// (re)subscribe. All fields may be populated.
	TickSnapshot TickType = "snapshot"
	// TickStatus — non-price status change (halt / resume /
	// session change) the provider chose to surface in-band.
	// Last / Bid / Ask are 0; Status carries the payload.
	TickStatus TickType = "status"
)

// Tick is the normalised payload every provider must emit.
// Pointer fields are forbidden — Ticks travel through channels
// and we want value semantics for ringbuffer / batch handling.
type Tick struct {
	Symbol        string    // canonical instrument_key (always upper-case)
	DisplaySymbol string    // human-friendly ticker, e.g. "AAPL"
	Market        string    // "US" / "HK" / "CN" / …
	Provider      string    // "polygon", "alpaca", "mock", …
	EventType     TickType
	Last          float64
	Size          float64
	Bid           float64
	BidSize       float64
	Ask           float64
	AskSize       float64
	Volume        float64   // cumulative session volume (may be 0)
	Sequence      uint64    // monotonic per (provider, symbol)
	Timestamp     time.Time // when the event happened upstream
	ReceivedAt    time.Time // when the manager observed the event
	Status        string    // for TickStatus
}

// ConnState is the closed enum for a provider connection.
type ConnState string

const (
	StateUnknown      ConnState = "unknown"
	StateConnecting   ConnState = "connecting"
	StateConnected    ConnState = "connected"
	StateReconnecting ConnState = "reconnecting"
	StateBackoff      ConnState = "backoff"
	StateDisconnected ConnState = "disconnected"
	StateClosed       ConnState = "closed" // terminal: Stop() was called
)

// IsHealthy returns true when the connection can accept and
// emit ticks. Used by the manager to gate subscription churn.
func (s ConnState) IsHealthy() bool {
	return s == StateConnected
}

// IsTerminal returns true when the connection will not retry
// on its own (admin must restart). Today only StateClosed is
// terminal; StateDisconnected always backs off and retries.
func (s ConnState) IsTerminal() bool {
	return s == StateClosed
}

// Subscription is a request to receive ticks for one symbol
// from one or more providers. The manager refs-counts these so
// multiple in-process consumers can share one upstream sub.
type Subscription struct {
	Symbol        string   // canonical instrument_key (upper-case)
	DisplaySymbol string   // for logging
	Market        string
	Types         []TickType // filter: empty = all
}

// Normalize lower-cases / strips whitespace as needed and
// guarantees Symbol is upper-case so the manager's map key is
// stable across callers.
func (s Subscription) Normalize() Subscription {
	out := s
	out.Symbol = strings.ToUpper(strings.TrimSpace(out.Symbol))
	out.Market = strings.ToUpper(strings.TrimSpace(out.Market))
	if out.DisplaySymbol == "" {
		out.DisplaySymbol = out.Symbol
	}
	return out
}

// Valid reports whether the subscription has the minimum
// fields required to route a sub request to a provider.
func (s Subscription) Valid() bool {
	return strings.TrimSpace(s.Symbol) != ""
}

// ConnStats is an observability snapshot of one provider
// connection. Returned by Manager.ConnectionsSnapshot().
type ConnStats struct {
	Provider        string
	State           ConnState
	ConnectedAt     time.Time
	DisconnectedAt  time.Time
	LastTickAt      time.Time
	TickCount       uint64
	ReconnectCount  uint64
	LastError       string
	Subscriptions   int       // # of unique symbols currently subscribed
}

// SubStats is one row of "what symbols are we currently
// subscribed to, and how many in-process consumers each has".
type SubStats struct {
	Symbol     string
	Market     string
	Consumers  int
	LastTickAt time.Time
}

// ----- Provider contract -----

// Provider is what a concrete WS provider (Polygon, Alpaca,
// mock, …) implements. The manager owns lifecycle; providers
// just expose async sub/unsub/emit.
//
// Implementations MUST be safe to call concurrently. Emit
// must block on a closed Out channel only briefly — providers
// are expected to drop on backpressure rather than wedge the
// network reader.
type Provider interface {
	// Name returns the stable identifier ("polygon", "mock",
	// …) used in Tick.Provider and ConnStats.Provider.
	Name() string

	// Start opens the connection. Returns nil on first
	// successful handshake; subsequent reconnects are
	// internal. The provider must continue emitting ticks
	// (and state events) until Stop is called.
	//
	// The `events` channel is the provider's only outbound
	// path. Implementations should NEVER close it; the
	// manager owns shutdown ordering.
	Start(events chan<- Event) error

	// Stop terminates the provider permanently. Must be
	// idempotent. After Stop returns the provider must not
	// emit further events.
	Stop()

	// Subscribe asks the provider to start streaming the
	// listed symbols. Idempotent; a duplicate Subscribe must
	// be cheap. Implementations should send a state event if
	// a re-subscribe fails (e.g. connection just dropped).
	Subscribe(subs []Subscription) error

	// Unsubscribe is the inverse. Implementations are not
	// required to actually un-subscribe upstream (some providers
	// charge per active sub regardless); the manager uses this
	// only as a hint.
	Unsubscribe(symbols []string) error

	// State returns the current ConnState. Cheap; called
	// frequently by the admin endpoint.
	State() ConnState
}

// Event is what providers push on the shared channel — either
// a Tick or a state change. Discrimination is via Tick.EventType
// (TickStatus) plus the Kind field for connection lifecycle.
type Event struct {
	Kind     EventKind
	Tick     Tick      // populated when Kind == EventTick
	State    ConnState // populated when Kind == EventState
	Error    string    // populated on EventError / EventState
}

// EventKind discriminates the union shape.
type EventKind string

const (
	EventTick  EventKind = "tick"
	EventState EventKind = "state"
	EventError EventKind = "error"
)

// ----- errors -----

// ErrNotConnected is returned by Subscribe / Unsubscribe when
// the connection isn't healthy. Manager backs off and retries.
var ErrNotConnected = errors.New("wsfeed: provider not connected")

// ErrUnknownProvider is returned when a Manager method
// references a provider name that isn't registered.
var ErrUnknownProvider = errors.New("wsfeed: unknown provider")

// ErrManagerStopped is returned by Subscribe / Unsubscribe
// after Stop has been called. The caller should treat this as
// a no-op (the manager is shutting down anyway).
var ErrManagerStopped = errors.New("wsfeed: manager stopped")
