// wiring_wsfeed.go — S6.5 WebSocket real-time market data
// wiring.
//
// What lives here
//
//   - newQuoteCache: constructs the in-memory last-tick cache
//     used by the broker hot path.
//   - newWSFeedManager: constructs the wsfeed.Manager and
//     wires its default provider chain. When the env-driven
//     provider list is empty we install a NopProvider so the
//     subscription / fan-out plumbing stays live.
//   - newCacheAwareQuoteFn: wraps the existing REST-backed
//     marketdata QuoteFn so that the broker simulator reads
//     the WS cache first and only falls back to REST on a
//     miss or stale entry. The fallback path itself is
//     untouched, so behaviour with no providers configured
//     is byte-identical to pre-S6.5.
//   - newWSFeedSubscriptionBridge: a small ticker that
//     compares "symbols held in any active fund" against
//     "currently subscribed via wsfeed" and reconciles. This
//     is what causes a new position to start receiving WS
//     ticks within a few seconds of opening, without each
//     workflow having to know about the WS feed.
//
// What it does NOT do
//
//   - It does not own provider implementations beyond the
//     mock + nop already shipped. Real providers (Polygon,
//     Alpaca, IEX, …) plug in via wsfeed.Provider and get
//     constructed from env config in their own files.
//   - It does not persist tick history. Replay is a future
//     PR.

package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/broker"
	"github.com/fundai/server/internal/matching"
	"github.com/fundai/server/internal/quotecache"
	"github.com/fundai/server/internal/wsfeed"
	wsfeedprovider "github.com/fundai/server/internal/wsfeed/provider"
)

// wsFeedConfig is the env-driven WS feed configuration.
type wsFeedConfig struct {
	// Enabled gates the entire WS feed wiring. False means
	// no manager, no cache, no broker wrap — the system
	// behaves exactly as pre-S6.5. Default false until
	// operators have a real provider plugged in.
	Enabled bool
	// CacheStaleAfter is how long a cached snapshot remains
	// fresh before the broker falls back to REST. Should be
	// at least 2× the expected tick interval for active
	// symbols. Default 10s.
	CacheStaleAfter time.Duration
	// CacheMaxEntries soft-caps cached symbols. Default
	// 5000 — enough for the watchlists of every fund in a
	// mid-size deployment.
	CacheMaxEntries int
	// ProviderNames is the comma-separated list of providers
	// to attempt. Today only "mock" and "nop" exist; future
	// PRs add "polygon", "alpaca", "iex". Default "nop".
	ProviderNames []string
	// SubscriptionReconcileInterval is how often the
	// subscription bridge reconciles "held symbols" against
	// "subscribed symbols". 0 disables. Default 30s.
	SubscriptionReconcileInterval time.Duration
	// SubscriptionInitialDelay defers the first reconcile
	// pass so the rest of the boot sequence has time to
	// finish. Default 15s.
	SubscriptionInitialDelay time.Duration
}

// wsFeedConfigFromEnv loads the config from the surrounding
// env. Kept tiny — the WS feed is opt-in until a real
// provider is wired.
func wsFeedConfigFromEnv(getenv func(string) string) wsFeedConfig {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	cfg := wsFeedConfig{
		Enabled:                       parseBoolEnvDefault(getenv("WSFEED_ENABLED"), false),
		CacheStaleAfter:               parseDurationEnvDefault(getenv("WSFEED_CACHE_STALE_AFTER"), 10*time.Second),
		CacheMaxEntries:               parseIntEnvDefault(getenv("WSFEED_CACHE_MAX_ENTRIES"), 5000),
		ProviderNames:                 splitCSV(getenv("WSFEED_PROVIDERS")),
		SubscriptionReconcileInterval: parseDurationEnvDefault(getenv("WSFEED_RECONCILE_INTERVAL"), 30*time.Second),
		SubscriptionInitialDelay:      parseDurationEnvDefault(getenv("WSFEED_RECONCILE_INITIAL_DELAY"), 15*time.Second),
	}
	if len(cfg.ProviderNames) == 0 {
		cfg.ProviderNames = []string{"nop"}
	}
	return cfg
}

// wsFeedBundle is what the wiring layer hands to Services so
// admin handlers + the subscription bridge can reach the
// manager + cache. nil-safe: fields may be zero when the
// feed is disabled.
type wsFeedBundle struct {
	Config  wsFeedConfig
	Manager *wsfeed.Manager
	Cache   *quotecache.Cache
	Bridge  *wsFeedSubscriptionBridge
}

// newQuoteCache constructs the last-tick cache.
func newQuoteCache(cfg wsFeedConfig) *quotecache.Cache {
	return quotecache.New(quotecache.Config{
		StaleAfter: cfg.CacheStaleAfter,
		MaxEntries: cfg.CacheMaxEntries,
	})
}

// newWSFeedManager constructs the manager and registers
// providers per cfg. The manager is NOT Started here — the
// caller decides when to Start (typically right before HTTP
// listen so the rest of the boot can finish first).
func newWSFeedManager(cfg wsFeedConfig, cache *quotecache.Cache, metrics interface {
	RecordWSFeedEvent(event string)
}) *wsfeed.Manager {
	mgr := wsfeed.NewManager(wsfeed.ManagerConfig{
		InboundBuffer: 4096,
		OnError: func(err error) {
			slog.Warn("wsfeed manager error", "err", err)
			if metrics != nil {
				metrics.RecordWSFeedEvent("manager_error")
			}
		},
	})
	// Cache: every Tick the manager fans out lands in the
	// cache so the broker hot path can read it synchronously.
	if cache != nil {
		mgr.AddTickHandler(func(t wsfeed.Tick) {
			cache.Apply(quotecache.Tick{
				Symbol:        t.Symbol,
				DisplaySymbol: t.DisplaySymbol,
				Market:        t.Market,
				Provider:      t.Provider,
				EventKind:     string(t.EventType),
				Last:          t.Last,
				Bid:           t.Bid,
				Ask:           t.Ask,
				Volume:        t.Volume,
				Timestamp:     t.Timestamp,
				ReceivedAt:    t.ReceivedAt,
			})
			if metrics != nil {
				metrics.RecordWSFeedEvent("tick_applied")
			}
		})
	}
	// State handler: surface connect / disconnect / reconnect
	// to metrics so we can alert on flaps.
	if metrics != nil {
		mgr.AddStateHandler(func(provider string, state wsfeed.ConnState, errStr string) {
			slog.Info("wsfeed provider state", "provider", provider, "state", string(state), "err", errStr)
			metrics.RecordWSFeedEvent("state_" + string(state))
		})
	}
	for _, name := range cfg.ProviderNames {
		name = strings.TrimSpace(strings.ToLower(name))
		if name == "" {
			continue
		}
		var p wsfeed.Provider
		switch name {
		case "mock":
			p = wsfeedprovider.NewMock("mock")
		case "nop":
			p = wsfeedprovider.NewNop("nop")
		default:
			slog.Warn("wsfeed: unknown provider, skipping", "provider", name)
			continue
		}
		if err := mgr.AddProvider(p); err != nil {
			slog.Warn("wsfeed: provider register failed", "provider", name, "err", err)
		}
	}
	return mgr
}

// newCacheAwareQuoteFn wraps the existing REST-backed
// QuoteFn so reads hit the WS-populated cache first.
//
// Decision tree on each call:
//
//	cache hit, fresh   → return cached
//	cache hit, stale   → fall back to REST (cached value
//	                     becomes a fast fallback if REST
//	                     errors below)
//	cache miss         → fall back to REST
//
// On REST error after a stale-hit we return the stale
// cached value rather than ErrQuoteUnavailable — for the
// broker that's the difference between a fill at a slightly-
// old price (acceptable) vs no fill at all (rejects every
// order until upstream comes back).
//
// If `cache` is nil we delegate straight to the fallback —
// equivalent to pre-S6.5 behaviour.
func newCacheAwareQuoteFn(cache *quotecache.Cache, fallback broker.QuoteFn, metrics interface {
	RecordWSFeedEvent(event string)
}) broker.QuoteFn {
	if cache == nil || fallback == nil {
		return fallback
	}
	return func(ctx context.Context, instrumentKey, symbol, market string) (matching.Quote, error) {
		key := pickFirstNonEmpty(instrumentKey, symbol)
		snap, ok, stale := cache.Lookup(key)
		if ok && !stale && !snap.IsZero() {
			if metrics != nil {
				metrics.RecordWSFeedEvent("quote_cache_hit")
			}
			return matching.Quote{
				Last: snap.Last,
				Bid:  snap.Bid,
				Ask:  snap.Ask,
			}, nil
		}
		// Miss or stale: go to REST.
		quote, err := fallback(ctx, instrumentKey, symbol, market)
		if err == nil {
			if metrics != nil {
				if ok && stale {
					metrics.RecordWSFeedEvent("quote_stale_fallback_ok")
				} else {
					metrics.RecordWSFeedEvent("quote_miss_fallback_ok")
				}
			}
			return quote, nil
		}
		// REST error + we have a stale cached value → serve
		// the stale value rather than rejecting the read.
		if ok && !snap.IsZero() {
			if metrics != nil {
				metrics.RecordWSFeedEvent("quote_stale_served_on_error")
			}
			return matching.Quote{Last: snap.Last, Bid: snap.Bid, Ask: snap.Ask}, nil
		}
		if metrics != nil {
			metrics.RecordWSFeedEvent("quote_miss_fallback_err")
		}
		return quote, err
	}
}

// ---- subscription bridge ----

// wsFeedSubscriptionBridge keeps the manager's set of
// subscribed symbols in sync with the set of symbols
// actively held by any fund in the DB. Without this the
// manager would never know which symbols to ask the upstream
// provider about — every consumer would have to remember to
// Subscribe themselves.
type wsFeedSubscriptionBridge struct {
	db       *sql.DB
	manager  *wsfeed.Manager
	interval time.Duration
	delay    time.Duration
	metrics  interface{ RecordWSFeedEvent(event string) }

	mu         sync.Mutex
	subscribed map[string]struct{} // bridge-owned subscriptions, for unsubscribe
	stopCh     chan struct{}
	doneCh     chan struct{}
	started    bool
}

func newWSFeedSubscriptionBridge(db *sql.DB, mgr *wsfeed.Manager, cfg wsFeedConfig, metrics interface{ RecordWSFeedEvent(event string) }) *wsFeedSubscriptionBridge {
	return &wsFeedSubscriptionBridge{
		db:         db,
		manager:    mgr,
		interval:   cfg.SubscriptionReconcileInterval,
		delay:      cfg.SubscriptionInitialDelay,
		metrics:    metrics,
		subscribed: make(map[string]struct{}),
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
}

// Start spawns the reconcile loop. Idempotent.
func (b *wsFeedSubscriptionBridge) Start(ctx context.Context) {
	if b == nil || b.manager == nil || b.db == nil || b.interval <= 0 {
		return
	}
	b.mu.Lock()
	if b.started {
		b.mu.Unlock()
		return
	}
	b.started = true
	b.mu.Unlock()
	go b.run(ctx)
}

// Stop signals the loop. Idempotent.
func (b *wsFeedSubscriptionBridge) Stop() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.started {
		return
	}
	b.started = false
	select {
	case <-b.stopCh:
		// already stopped
	default:
		close(b.stopCh)
	}
}

func (b *wsFeedSubscriptionBridge) run(ctx context.Context) {
	defer close(b.doneCh)
	if b.delay > 0 {
		select {
		case <-time.After(b.delay):
		case <-ctx.Done():
			return
		case <-b.stopCh:
			return
		}
	}
	t := time.NewTicker(b.interval)
	defer t.Stop()
	// Fire one reconcile immediately on first wake so we
	// don't wait an entire interval to pick up symbols held
	// at process start.
	b.reconcileOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.stopCh:
			return
		case <-t.C:
			b.reconcileOnce(ctx)
		}
	}
}

// reconcileOnce diffs DB-held symbols against the bridge's
// own subscription set and issues sub/unsub calls.
func (b *wsFeedSubscriptionBridge) reconcileOnce(ctx context.Context) {
	want, err := b.heldSymbols(ctx)
	if err != nil {
		if b.metrics != nil {
			b.metrics.RecordWSFeedEvent("reconcile_query_err")
		}
		slog.Warn("wsfeed reconcile: query failed", "err", err)
		return
	}
	wantSet := make(map[string]wsfeed.Subscription, len(want))
	for _, s := range want {
		wantSet[s.Symbol] = s
	}
	b.mu.Lock()
	have := make(map[string]struct{}, len(b.subscribed))
	for k := range b.subscribed {
		have[k] = struct{}{}
	}
	b.mu.Unlock()
	added, removed := 0, 0
	for sym, sub := range wantSet {
		if _, ok := have[sym]; ok {
			continue
		}
		if err := b.manager.Subscribe(sub); err != nil {
			if b.metrics != nil {
				b.metrics.RecordWSFeedEvent("reconcile_subscribe_err")
			}
			continue
		}
		b.mu.Lock()
		b.subscribed[sym] = struct{}{}
		b.mu.Unlock()
		added++
	}
	for sym := range have {
		if _, ok := wantSet[sym]; ok {
			continue
		}
		if err := b.manager.Unsubscribe(sym); err != nil {
			if b.metrics != nil {
				b.metrics.RecordWSFeedEvent("reconcile_unsubscribe_err")
			}
			continue
		}
		b.mu.Lock()
		delete(b.subscribed, sym)
		b.mu.Unlock()
		removed++
	}
	if b.metrics != nil {
		b.metrics.RecordWSFeedEvent("reconcile_ok")
		if added > 0 {
			b.metrics.RecordWSFeedEvent("reconcile_added")
		}
		if removed > 0 {
			b.metrics.RecordWSFeedEvent("reconcile_removed")
		}
	}
}

// heldSymbols returns the distinct (instrument_key, symbol,
// market) tuples currently held across all non-archived funds.
// Closed positions (qty == 0) are excluded so we don't
// subscribe to symbols we no longer care about.
func (b *wsFeedSubscriptionBridge) heldSymbols(ctx context.Context) ([]wsfeed.Subscription, error) {
	if b.db == nil {
		return nil, nil
	}
	const q = `
		SELECT DISTINCT
		    COALESCE(NULLIF(instrument_key, ''), UPPER(symbol)) AS sym_key,
		    symbol,
		    COALESCE(market, '') AS market
		FROM holding_positions
		WHERE COALESCE(quantity, 0) <> 0
		  AND COALESCE(symbol, '') <> ''
		ORDER BY sym_key
		LIMIT 20000
	`
	rows, err := b.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("heldSymbols query: %w", err)
	}
	defer rows.Close()
	var out []wsfeed.Subscription
	for rows.Next() {
		var sym, display, market string
		if err := rows.Scan(&sym, &display, &market); err != nil {
			return nil, fmt.Errorf("heldSymbols scan: %w", err)
		}
		out = append(out, wsfeed.Subscription{
			Symbol:        sym,
			DisplaySymbol: display,
			Market:        market,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("heldSymbols iter: %w", err)
	}
	return out, nil
}

// ---- small helpers (kept local; env parsing already exists elsewhere) ----

func parseBoolEnvDefault(s string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func parseIntEnvDefault(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	var n int
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return def
	}
	return n
}

func parseDurationEnvDefault(s string, def time.Duration) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

// quoteCacheLookupAdapter bridges *quotecache.Cache to the
// position refresher's wsCacheLookup interface. We can't make
// the cache implement the interface directly because that
// would couple the cache package to the refresher's value
// type; doing the projection here keeps the dependency arrow
// pointing the right way.
type quoteCacheLookupAdapter struct{ c *quotecache.Cache }

func (a quoteCacheLookupAdapter) Lookup(symbol string) (wsCacheSnap, bool, bool) {
	if a.c == nil {
		return wsCacheSnap{}, false, false
	}
	snap, ok, stale := a.c.Lookup(symbol)
	if !ok {
		return wsCacheSnap{}, false, false
	}
	return wsCacheSnap{
		Last: snap.Last,
		Bid:  snap.Bid,
		Ask:  snap.Ask,
		AsOf: snap.AsOf,
	}, true, stale
}

// newQuoteCacheLookup returns an adapter for the refresher
// when the cache is non-nil; nil otherwise so callers can
// safely pass the result to SetWSCache(nil).
func newQuoteCacheLookup(cache *quotecache.Cache) wsCacheLookup {
	if cache == nil {
		return nil
	}
	return quoteCacheLookupAdapter{c: cache}
}

func pickFirstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
