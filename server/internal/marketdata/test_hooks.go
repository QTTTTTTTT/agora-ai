package marketdata

import (
	"errors"
	"time"
)

// RecordTestProviderFailure and RecordTestProviderSuccess are test-only
// helpers exported for use from sibling packages that want to seed the
// provider health tracker without spinning up a real network round-trip.
//
// They are safe to call from any package, but the names start with
// "RecordTest" to make accidental production use easy to spot in review.
// Production code paths instead go through GetQuote which records into the
// same tracker automatically.

// RecordTestProviderFailure increments the failure counter for the named
// provider and may trip the circuit breaker depending on the configured
// threshold. Intended only for unit / integration tests of observability
// surfaces (Prometheus exporter, admin endpoint, etc.).
func (s *Service) RecordTestProviderFailure(name string) {
	if s == nil || s.providerHealth == nil {
		return
	}
	s.providerHealth.recordFailure(name, errors.New("synthetic failure (test hook)"), time.Now().UTC(), 25*time.Millisecond)
}

// RecordTestProviderSuccess increments the success counter for the named
// provider and resets any tripped circuit state. Latency is fixed at 50ms
// for deterministic test assertions.
func (s *Service) RecordTestProviderSuccess(name string) {
	if s == nil || s.providerHealth == nil {
		return
	}
	s.providerHealth.recordSuccess(name, 50*time.Millisecond)
}
