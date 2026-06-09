// health.go — small per-provider circuit breaker for the
// cnmarketstructure providers. Mirrors marketdata's tracker but is
// self-contained so we don't pull in the marketdata package just for
// 100 lines of state.

package cnmarketstructure

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// ProviderHealthStats is the externally-visible per-provider record.
// The admin probe handler renders it as JSON.
type ProviderHealthStats struct {
	TotalCalls          int64     `json:"total_calls"`
	TotalSuccesses      int64     `json:"total_successes"`
	TotalFailures       int64     `json:"total_failures"`
	TotalThrottled      int64     `json:"total_throttled,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	CircuitOpenUntil    time.Time `json:"circuit_open_until,omitempty"`
	LastError           string    `json:"last_error,omitempty"`
	LastSuccessAt       time.Time `json:"last_success_at,omitempty"`
	LastFailureAt       time.Time `json:"last_failure_at,omitempty"`
	LastThrottledAt     time.Time `json:"last_throttled_at,omitempty"`
	LastLatencyMs       int64     `json:"last_latency_ms,omitempty"`
	EMALatencyMs        int64     `json:"ema_latency_ms,omitempty"`
}

type healthTracker struct {
	mu               sync.Mutex
	failureThreshold int
	cooldown         time.Duration
	throttleCooldown time.Duration
	states           map[string]*healthState
}

type healthState struct {
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
	latencyEMA          float64
	lastLatency         time.Duration
}

func newHealthTracker(failureThreshold int, cooldown, throttleCooldown time.Duration) *healthTracker {
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
	return &healthTracker{
		failureThreshold: failureThreshold,
		cooldown:         cooldown,
		throttleCooldown: throttleCooldown,
		states:           make(map[string]*healthState),
	}
}

func (t *healthTracker) shouldSkip(name string, now time.Time) (bool, time.Time) {
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

func (t *healthTracker) recordSuccess(name string, latency time.Duration) {
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

func (t *healthTracker) recordFailure(name string, err error, now time.Time, latency time.Duration) {
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
		state.openUntil = now.Add(t.throttleCooldown)
		state.totalThrottled++
		state.lastThrottledAt = now
		return
	}
	if state.consecutiveFailures >= t.failureThreshold {
		state.openUntil = now.Add(t.cooldown)
	}
}

func (t *healthTracker) Snapshot() map[string]ProviderHealthStats {
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

func (t *healthTracker) stateForLocked(name string) *healthState {
	state, ok := t.states[name]
	if !ok {
		state = &healthState{}
		t.states[name] = state
	}
	return state
}

func (s *healthState) updateLatencyEMA(latency time.Duration) {
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

// ErrUpstreamThrottled lets providers signal an explicit
// rate-limit / IP-block back to the registry so the circuit
// jumps straight to the throttle cooldown.
var ErrUpstreamThrottled = errors.New("cnmarketstructure: upstream throttled")

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
