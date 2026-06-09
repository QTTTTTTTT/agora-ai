// registry.go — the per-process selector that holds N Providers and
// dispatches each Fetch* call to the first healthy provider that
// answers.
//
// The pattern mirrors fundamental.Registry and marketdata's provider
// chain: register providers in priority order; the registry skips
// any provider whose circuit is open and falls through to the next
// one on ErrNoData. Only ErrNoData (and circuit-open) is treated as
// "try the next provider"; hard transport errors propagate.
//
// HealthStats is exposed so the admin probe handler can render a
// per-provider table in the dashboard.

package cnmarketstructure

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Registry composes one or more Provider implementations.
type Registry struct {
	mu        sync.RWMutex
	providers []Provider
	health    *healthTracker
}

// NewRegistry constructs an empty registry with default circuit
// settings tuned for akshare's flakiness (3 failures = open, 30s
// cooldown, 5m throttle cooldown for 429s).
func NewRegistry() *Registry {
	return &Registry{
		health: newHealthTracker(3, 30*time.Second, 5*time.Minute),
	}
}

// Register adds a provider, replacing any prior entry with the
// same Name(). Order matters: the first matching healthy provider
// wins.
func (r *Registry) Register(p Provider) {
	if p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.providers {
		if existing.Name() == p.Name() {
			r.providers[i] = p
			return
		}
	}
	r.providers = append(r.providers, p)
}

// HealthStats returns the per-provider health counters. Used by
// the admin probe handler.
func (r *Registry) HealthStats() map[string]ProviderHealthStats {
	return r.health.Snapshot()
}

// Names returns the registered provider Name()s, in priority order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, p.Name())
	}
	return out
}

// FetchIntraday implements Provider by routing to the first healthy
// provider that answers. ErrNotConfigured when no providers are
// registered.
func (r *Registry) FetchIntraday(ctx context.Context, symbol string) (*IntradaySnapshot, error) {
	providers := r.snapshotProviders()
	if len(providers) == 0 {
		return nil, ErrNotConfigured
	}
	now := time.Now()
	var lastErr error
	for _, p := range providers {
		if open, _ := r.health.shouldSkip(p.Name(), now); open {
			continue
		}
		start := time.Now()
		snap, err := p.FetchIntraday(ctx, symbol)
		latency := time.Since(start)
		if err == nil && snap != nil {
			r.health.recordSuccess(p.Name(), latency)
			snap.Source = p.Name()
			return snap, nil
		}
		if err == nil {
			// nil snap + nil err — treat as ErrNoData.
			err = ErrNoData
		}
		if errors.Is(err, ErrNoData) {
			// Soft miss — neither success nor failure for the
			// health tracker. We still try the next provider.
			continue
		}
		r.health.recordFailure(p.Name(), err, time.Now(), latency)
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrNoData
}

// FetchDragonTiger implements Provider by routing through the chain.
func (r *Registry) FetchDragonTiger(ctx context.Context, symbol string, lookbackDays int) ([]DragonTigerEntry, error) {
	providers := r.snapshotProviders()
	if len(providers) == 0 {
		return nil, ErrNotConfigured
	}
	now := time.Now()
	var lastErr error
	for _, p := range providers {
		if open, _ := r.health.shouldSkip(p.Name(), now); open {
			continue
		}
		start := time.Now()
		entries, err := p.FetchDragonTiger(ctx, symbol, lookbackDays)
		latency := time.Since(start)
		if err == nil {
			r.health.recordSuccess(p.Name(), latency)
			return entries, nil
		}
		if errors.Is(err, ErrNoData) {
			continue
		}
		r.health.recordFailure(p.Name(), err, time.Now(), latency)
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrNoData
}

// FetchMarketRegime implements Provider by routing through the chain.
func (r *Registry) FetchMarketRegime(ctx context.Context) (*MarketRegime, error) {
	providers := r.snapshotProviders()
	if len(providers) == 0 {
		return nil, ErrNotConfigured
	}
	now := time.Now()
	var lastErr error
	for _, p := range providers {
		if open, _ := r.health.shouldSkip(p.Name(), now); open {
			continue
		}
		start := time.Now()
		regime, err := p.FetchMarketRegime(ctx)
		latency := time.Since(start)
		if err == nil && regime != nil {
			r.health.recordSuccess(p.Name(), latency)
			regime.Source = p.Name()
			return regime, nil
		}
		if err == nil {
			err = ErrNoData
		}
		if errors.Is(err, ErrNoData) {
			continue
		}
		r.health.recordFailure(p.Name(), err, time.Now(), latency)
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrNoData
}

// FetchSectorStrength implements Provider by routing through the chain.
func (r *Registry) FetchSectorStrength(ctx context.Context, topN int) ([]SectorStrength, error) {
	providers := r.snapshotProviders()
	if len(providers) == 0 {
		return nil, ErrNotConfigured
	}
	now := time.Now()
	var lastErr error
	for _, p := range providers {
		if open, _ := r.health.shouldSkip(p.Name(), now); open {
			continue
		}
		start := time.Now()
		sectors, err := p.FetchSectorStrength(ctx, topN)
		latency := time.Since(start)
		if err == nil {
			r.health.recordSuccess(p.Name(), latency)
			return sectors, nil
		}
		if errors.Is(err, ErrNoData) {
			continue
		}
		r.health.recordFailure(p.Name(), err, time.Now(), latency)
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrNoData
}

// Name returns a stable id for the composed registry. Useful so the
// registry itself can be logged like any other provider.
func (r *Registry) Name() string { return "cnmarketstructure_registry" }

func (r *Registry) snapshotProviders() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Provider, len(r.providers))
	copy(out, r.providers)
	return out
}
