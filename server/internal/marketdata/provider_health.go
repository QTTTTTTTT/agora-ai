package marketdata

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// providerHealthTracker is a small per-process circuit breaker for upstream
// providers. It is intentionally simple: every provider name has a tiny
// state machine (closed -> open -> half-open -> closed) that trips after
// `failureThreshold` consecutive failures and stays open for `cooldown`
// before the next call is allowed to retry once. A single success closes
// the circuit back.
//
// Why so simple:
//   - We have at most ~10 provider names, no need for fancy bucketed metrics.
//   - The cost of a wrong "skip" is one cache TTL cycle (10s for quotes,
//     2 minutes for news), which is acceptable.
//   - The cost of a wrong "retry" is one extra HTTP call, also fine.
//
// All public methods are safe for concurrent use from request handlers.
type providerHealthTracker struct {
	mu               sync.Mutex
	failureThreshold int
	cooldown         time.Duration
	// throttleCooldown is the extended break applied when an upstream
	// signals an explicit rate-limit / IP block (HTTP 429/451 or
	// ErrUpstreamThrottled). Trips immediately on the first throttle hit
	// (no consecutive-failure threshold) because a 429 is an authoritative
	// "stop calling me" signal — keeping the bucket warm would just make
	// it worse.
	throttleCooldown time.Duration
	states           map[string]*providerHealthState
}

type providerHealthState struct {
	consecutiveFailures int
	openUntil           time.Time
	lastError           string
	totalCalls          int64
	totalFailures       int64
	totalSuccesses      int64
	totalThrottled      int64
	lastSuccessAt       time.Time
	lastFailureAt       time.Time
	lastThrottledAt     time.Time
	// Exponential moving average of provider latency (ms). Smoothed with
	// alpha=0.2 so a single slow call doesn't dominate the signal.
	latencyEMA  float64
	lastLatency time.Duration
}

func newProviderHealthTracker(failureThreshold int, cooldown, throttleCooldown time.Duration) *providerHealthTracker {
	if failureThreshold <= 0 {
		failureThreshold = 3
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	if throttleCooldown <= 0 {
		throttleCooldown = 5 * time.Minute
	}
	if throttleCooldown < cooldown {
		throttleCooldown = cooldown
	}
	return &providerHealthTracker{
		failureThreshold: failureThreshold,
		cooldown:         cooldown,
		throttleCooldown: throttleCooldown,
		states:           make(map[string]*providerHealthState),
	}
}

// shouldSkip returns true when the named provider's circuit is currently
// open at `now` and the caller should bypass it. The second return value is
// the moment the circuit will allow the next attempt, useful for logging.
func (t *providerHealthTracker) shouldSkip(name string, now time.Time) (bool, time.Time) {
	if t == nil {
		return false, time.Time{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.states[name]
	if state == nil {
		return false, time.Time{}
	}
	if state.openUntil.IsZero() || !now.Before(state.openUntil) {
		return false, time.Time{}
	}
	return true, state.openUntil
}

func (t *providerHealthTracker) recordSuccess(name string, latency time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.stateForLocked(name)
	state.consecutiveFailures = 0
	state.openUntil = time.Time{}
	state.lastError = ""
	state.totalCalls++
	state.totalSuccesses++
	state.lastSuccessAt = time.Now().UTC()
	state.updateLatencyEMA(latency)
}

func (t *providerHealthTracker) recordFailure(name string, err error, now time.Time, latency time.Duration) {
	if t == nil {
		return
	}
	throttled := isThrottleError(err)
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.stateForLocked(name)
	state.consecutiveFailures++
	state.totalCalls++
	state.totalFailures++
	state.lastFailureAt = now
	if err != nil {
		state.lastError = err.Error()
	}
	state.updateLatencyEMA(latency)
	if throttled {
		// Authoritative back-off signal. Trip the circuit straight to the
		// throttle cooldown ignoring the consecutive-failure threshold and
		// also reset the counter so a subsequent success after cooldown
		// starts fresh instead of replaying the old failure tally.
		state.openUntil = now.Add(t.throttleCooldown)
		state.totalThrottled++
		state.lastThrottledAt = now
		return
	}
	if state.consecutiveFailures >= t.failureThreshold {
		state.openUntil = now.Add(t.cooldown)
	}
}

// isThrottleError tests whether err is a marketdata upstream-throttle
// signal. It accepts both the sentinel ErrUpstreamThrottled (preferred) and
// a few legacy substrings ("http 429", "rate limited") so older provider
// implementations that haven't been migrated still get the long cooldown.
func isThrottleError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrUpstreamThrottled) {
		return true
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "http 429") || strings.Contains(msg, "http 451") {
		return true
	}
	if strings.Contains(msg, "rate limited") || strings.Contains(msg, "too many requests") {
		return true
	}
	return false
}

// isThrottleStatus is the HTTP-side counterpart used by providers to decide
// whether to wrap the upstream error with ErrUpstreamThrottled.
func isThrottleStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusUnavailableForLegalReasons
}

func (s *providerHealthState) updateLatencyEMA(latency time.Duration) {
	if latency <= 0 {
		return
	}
	ms := float64(latency.Milliseconds())
	s.lastLatency = latency
	if s.latencyEMA <= 0 {
		s.latencyEMA = ms
		return
	}
	const alpha = 0.2
	s.latencyEMA = alpha*ms + (1-alpha)*s.latencyEMA
}

// Snapshot returns a copy of the per-provider counters useful for
// observability endpoints. Map keys are provider names. Safe to call
// concurrently.
func (t *providerHealthTracker) Snapshot() map[string]ProviderHealthStats {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make(map[string]ProviderHealthStats, len(t.states))
	for name, state := range t.states {
		result[name] = ProviderHealthStats{
			TotalCalls:          state.totalCalls,
			TotalSuccesses:      state.totalSuccesses,
			TotalFailures:       state.totalFailures,
			TotalThrottled:      state.totalThrottled,
			ConsecutiveFailures: state.consecutiveFailures,
			CircuitOpenUntil:    state.openUntil,
			LastError:           state.lastError,
			LastSuccessAt:       state.lastSuccessAt,
			LastFailureAt:       state.lastFailureAt,
			LastThrottledAt:     state.lastThrottledAt,
			LastLatencyMs:       state.lastLatency.Milliseconds(),
			EMALatencyMs:        int64(state.latencyEMA + 0.5),
		}
	}
	return result
}

// ProviderHealthStats is the externally-visible per-provider health record.
type ProviderHealthStats struct {
	TotalCalls          int64     `json:"totalCalls"`
	TotalSuccesses      int64     `json:"totalSuccesses"`
	TotalFailures       int64     `json:"totalFailures"`
	TotalThrottled      int64     `json:"totalThrottled,omitempty"`
	ConsecutiveFailures int       `json:"consecutiveFailures"`
	CircuitOpenUntil    time.Time `json:"circuitOpenUntil,omitempty"`
	LastError           string    `json:"lastError,omitempty"`
	LastSuccessAt       time.Time `json:"lastSuccessAt,omitempty"`
	LastFailureAt       time.Time `json:"lastFailureAt,omitempty"`
	LastThrottledAt     time.Time `json:"lastThrottledAt,omitempty"`
	LastLatencyMs       int64     `json:"lastLatencyMs,omitempty"`
	EMALatencyMs        int64     `json:"emaLatencyMs,omitempty"`
}

func (t *providerHealthTracker) stateForLocked(name string) *providerHealthState {
	state, ok := t.states[name]
	if !ok {
		state = &providerHealthState{}
		t.states[name] = state
	}
	return state
}

// isQuoteStale returns true when the quote's recorded AsOf timestamp is older
// than maxAge relative to now. Used to mark IsStale on QuoteSnapshot so the
// risk / execution UI can warn the user before trading on an outdated price.
// A non-positive maxAge disables the check (returns false unconditionally).
func isQuoteStale(asOf time.Time, now time.Time, maxAge time.Duration) bool {
	if maxAge <= 0 || asOf.IsZero() {
		return false
	}
	return now.Sub(asOf) > maxAge
}

// staleQuoteNote returns the user-visible provider note attached to a
// research context when the quote is marked stale. The age is rounded to a
// nearby unit so the message stays readable ("quote outdated (age: 2h15m)").
func staleQuoteNote(quote *QuoteSnapshot, now time.Time) string {
	if quote == nil || quote.AsOf.IsZero() {
		return "quote outdated"
	}
	age := now.Sub(quote.AsOf)
	switch {
	case age >= 24*time.Hour:
		days := age / (24 * time.Hour)
		return "quote outdated (age: " + formatDuration(int64(days), "d") + ")"
	case age >= time.Hour:
		return "quote outdated (age: " + age.Round(time.Minute).String() + ")"
	default:
		return "quote outdated (age: " + age.Round(time.Second).String() + ")"
	}
}

func formatDuration(value int64, unit string) string {
	if value <= 0 {
		return "<1" + unit
	}
	return strconv.FormatInt(value, 10) + unit
}
