package promotion

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// EngineSelection is the resolver's output: which decision engine
// to run for a given fund + the param bag to feed it. Source
// records where the selection came from so the audit /
// observability stack can attribute decisions correctly.
type EngineSelection struct {
	EngineKind   string
	EngineParams EngineParams
	// PromotionID is non-empty when the selection came from an
	// active promotion. Empty when the resolver fell back to the
	// fund's default engine (no active promotion).
	PromotionID string
	// Source labels where this selection came from for
	// observability: "promotion-active", "promotion-shadow",
	// "default", or "cache".
	Source string
}

// DefaultEngineLookup returns the fund's configured fallback
// engine when no active promotion exists. The cmd/server wiring
// supplies a closure that reads from the fund repo's `config`
// JSONB column (where engineKind has lived since Phase 2A).
//
// fundID is the only input — caller is expected to short-circuit
// authentication / authorisation before reaching the resolver.
type DefaultEngineLookup func(ctx context.Context, fundID string) (EngineSelection, error)

// Resolver is the read-side seam between the promotion lifecycle
// and the rest of the decision pipeline. It answers "which
// engine should fund X use right now?" with whichever is more
// specific: active promotion → shadow promotion (read-only) →
// fund default.
//
// Cached for a short TTL because the PMAgent calls this on every
// trading-day tick and a per-call DB round-trip would dominate
// latency. The active promotion changes only on operator action
// (approve / activate / rollback), so cache invalidation is
// done by version-bumping `Invalidate(fundID)` from the service
// when those transitions fire.
type Resolver struct {
	Service       *Service
	DefaultLookup DefaultEngineLookup
	// TTL bounds how long a cache entry survives in the absence
	// of an explicit invalidation. Defaults to 30s. Zero
	// disables caching entirely.
	TTL time.Duration

	mu    sync.RWMutex
	cache map[string]cachedSelection
}

type cachedSelection struct {
	sel       EngineSelection
	expiresAt time.Time
}

// NewResolver constructs a Resolver with sensible defaults. The
// caller wires Service + DefaultLookup; TTL of 30s is the
// recommended starting point for production.
func NewResolver(svc *Service, defaultLookup DefaultEngineLookup) *Resolver {
	return &Resolver{
		Service:       svc,
		DefaultLookup: defaultLookup,
		TTL:           30 * time.Second,
		cache:         make(map[string]cachedSelection),
	}
}

// Resolve returns the engine to use for fundID. Order:
//  1. Cached entry (if not expired).
//  2. Active promotion from the service.
//  3. Fund's default engine.
//
// If the service errors out (DB down), we degrade to the default
// engine rather than refusing decisions — getting decisions
// wrong is worse than not getting them at all.
func (r *Resolver) Resolve(ctx context.Context, fundID string) (EngineSelection, error) {
	if r == nil || r.Service == nil {
		return EngineSelection{}, nil
	}
	if cached, ok := r.lookupCache(fundID); ok {
		cached.Source = "cache:" + cached.Source
		return cached, nil
	}
	sel, err := r.resolveFresh(ctx, fundID)
	if err != nil {
		return sel, err
	}
	r.storeCache(fundID, sel)
	return sel, nil
}

// resolveFresh does the actual lookup without consulting the
// cache. Split out so tests can exercise the lookup logic with
// caching off.
func (r *Resolver) resolveFresh(ctx context.Context, fundID string) (EngineSelection, error) {
	active, err := r.Service.ResolveActive(ctx, fundID)
	if err != nil {
		slog.Warn("promotion resolver: lookup failed; falling back to default", "fund_id", fundID, "err", err)
		// Fall through to default lookup — better to keep
		// trading on the prior config than to bail.
	}
	if active != nil && active.Status == StatusActive {
		return EngineSelection{
			EngineKind:   active.EngineKind,
			EngineParams: cloneParams(active.EngineParams),
			PromotionID:  active.ID,
			Source:       "promotion-active",
		}, nil
	}
	if r.DefaultLookup == nil {
		return EngineSelection{Source: "default-missing"}, nil
	}
	sel, err := r.DefaultLookup(ctx, fundID)
	if err != nil {
		return EngineSelection{Source: "default-error"}, err
	}
	if sel.Source == "" {
		sel.Source = "default"
	}
	return sel, nil
}

// Invalidate drops the cached entry for a fund so the next
// Resolve call refetches. The Service calls this from every
// transition that could change the active promotion.
func (r *Resolver) Invalidate(fundID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, fundID)
}

// InvalidateAll wipes the cache wholesale. Useful for testing
// and for "rebuild the world after a config push" admin actions.
func (r *Resolver) InvalidateAll() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache = make(map[string]cachedSelection)
}

func (r *Resolver) lookupCache(fundID string) (EngineSelection, bool) {
	if r.TTL <= 0 {
		return EngineSelection{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.cache[fundID]
	if !ok {
		return EngineSelection{}, false
	}
	if time.Now().After(c.expiresAt) {
		return EngineSelection{}, false
	}
	return c.sel, true
}

func (r *Resolver) storeCache(fundID string, sel EngineSelection) {
	if r.TTL <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[fundID] = cachedSelection{sel: sel, expiresAt: time.Now().Add(r.TTL)}
}
