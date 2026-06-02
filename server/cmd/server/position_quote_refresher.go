package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/fundai/server/internal/marketdata"
	"github.com/fundai/server/internal/repository"
)

// PositionQuoteRefreshLeaseName is the scheduler_leases row id used to gate
// the position-quote refresher loop. Only one server in the pool runs it.
const PositionQuoteRefreshLeaseName = "position-quote-refresh"

// positionQuoteRefresher walks every active fund's holdings on a steady
// cadence, re-quotes them through marketdata.GetQuotes, and updates the
// `holding_positions.current_price / market_value / unrealized_pnl` columns
// so the DB-cached price never lags more than `tickInterval`.
//
// Cadence (configurable via env):
//
//   - In-session  (US/CN/HK/EU regular hours, or always for crypto):
//     `tickInSession` (default 30s).
//   - Off-session (everyone closed, weekends): `tickOffSession`
//     (default 5min).
//
// We pick the cadence per-tick based on the *union* of holdings across all
// active funds (cheaper than per-fund timers and matches what the user
// actually sees on a multi-fund dashboard).
//
// The loop is leader-gated. On followers it ticks idly and re-checks
// leadership; on the leader it queries `funds_active` ∪ `holding_positions`,
// dedupes instruments, batches them through `marketdata.GetQuotes`, then
// writes the result back via `PositionRepo.UpdatePriceMetrics`.
//
// Safe to start without marketData wired — the run-loop simply exits each
// pass with a debug log so unit tests / dev builds with the service off
// still boot cleanly. Single-binary smoke runs without a database also
// short-circuit because fundRepo / positionRepo will be nil.
type positionQuoteRefresher struct {
	fundRepo     *repository.FundRepo
	positionRepo *repository.PositionRepo
	marketData   *marketdata.Service
	// lotRepo is OPTIONAL. When non-nil, every successful price
	// update also bumps the per-lot highest_price_seen /
	// lowest_price_seen extremes on position_lots — the data
	// the exit manager's trailing rule (Phase 3A-2) and the
	// closed_lots MFE/MAE (Phase 3A-1) feed off. Nil keeps the
	// refresher operating on holding_positions only, matching
	// the legacy pre-PR-3A1 behaviour.
	lotRepo *repository.LotRepo
	// wsCache is OPTIONAL. When non-nil, each refresh pass
	// first consults the WS-feed quote cache (S6.5) and
	// substitutes any fresh cached snapshot for the REST
	// quote. This eliminates upstream calls for actively-
	// traded symbols and keeps holding_positions in sync
	// with the same prices the broker hot path sees.
	wsCache wsCacheLookup
	metrics positionRefreshMetrics

	tickInSession  time.Duration
	tickOffSession time.Duration
	// perPassTimeout caps each pass so a misbehaving upstream / DB can't
	// stall the goroutine indefinitely. Default: 60s.
	perPassTimeout time.Duration

	leader  leaderChecker
	stopCh  chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
	started bool
	// nowFn / inSessionFn are injectable for tests; production uses
	// time.Now and the marketdata heuristic.
	nowFn       func() time.Time
	inSessionFn func(now time.Time) bool
}

// positionRefreshMetrics is the thin shim around Prometheus counters we
// wire from main.go. Keeping the interface here lets the test file pass
// a stub without dragging the Prom registry into the unit-test image.
type positionRefreshMetrics interface {
	RecordRefreshPass(rows int, duration time.Duration, failed bool)
}

type noopPositionRefreshMetrics struct{}

func (noopPositionRefreshMetrics) RecordRefreshPass(int, time.Duration, bool) {}

// wsCacheLookup is the minimal slice of *quotecache.Cache the
// refresher needs. Kept as an interface so unit tests can
// substitute a fake without depending on the live cache.
type wsCacheLookup interface {
	// Lookup returns (snapshot, ok, stale). ok=false means
	// the symbol has no WS data; stale=true means the symbol
	// has data but it's older than the cache's StaleAfter
	// window and the caller should fall back to REST.
	Lookup(symbol string) (snap wsCacheSnap, ok bool, stale bool)
}

// wsCacheSnap is the per-symbol view of cached WS data. Only
// the price/bid/ask + timestamp matter for the refresher;
// other fields (provider, market) are ignored.
type wsCacheSnap struct {
	Last       float64
	Bid        float64
	Ask        float64
	AsOf       time.Time
}

func newPositionQuoteRefresher(fundRepo *repository.FundRepo, positionRepo *repository.PositionRepo, marketData *marketdata.Service) *positionQuoteRefresher {
	return &positionQuoteRefresher{
		fundRepo:       fundRepo,
		positionRepo:   positionRepo,
		marketData:     marketData,
		lotRepo:        nil,
		metrics:        noopPositionRefreshMetrics{},
		tickInSession:  30 * time.Second,
		tickOffSession: 5 * time.Minute,
		perPassTimeout: 60 * time.Second,
		stopCh:         make(chan struct{}),
		nowFn:          func() time.Time { return time.Now().UTC() },
		inSessionFn: func(now time.Time) bool {
			// Cheap union: if *any* major market is trading we tick fast.
			// We don't enumerate held instruments here (it would couple
			// the cadence to a SELECT call per tick); instead the
			// session check uses the same coarse helper as the news
			// adaptive TTL.
			return marketdata.IsMajorMarketActive(now)
		},
	}
}

// SetWSCache wires the S6.5 WebSocket quote cache. nil disables
// the WS overlay and the refresher behaves identically to pre-
// S6.5 (REST-only path).
func (r *positionQuoteRefresher) SetWSCache(cache wsCacheLookup) {
	if r == nil {
		return
	}
	r.wsCache = cache
}

// SetLeaderChecker wires the distributed leader-election check so only one
// replica runs the refresh sweep. Nil checker → permanent leader (used by
// the single-binary smoke flow).
func (r *positionQuoteRefresher) SetLeaderChecker(checker leaderChecker) {
	if r == nil {
		return
	}
	r.leader = checker
}

// SetLotRepo wires the Phase 3A-1 lot-ledger repository so every
// price refresh also extends the per-lot MFE/MAE tracking. Nil
// leaves the refresher in the pre-3A1 mode (price + market_value
// only). Safe to call multiple times — the new value replaces
// the old without disturbing the run loop.
func (r *positionQuoteRefresher) SetLotRepo(lotRepo *repository.LotRepo) {
	if r == nil {
		return
	}
	r.lotRepo = lotRepo
}

// SetIntervals overrides the default ticks. Zero values fall back to the
// defaults (30s in-session / 5m off-session). Useful for tests that need
// to drive the loop quickly.
func (r *positionQuoteRefresher) SetIntervals(inSession, offSession time.Duration) {
	if r == nil {
		return
	}
	if inSession > 0 {
		r.tickInSession = inSession
	}
	if offSession > 0 {
		r.tickOffSession = offSession
	}
}

// SetMetrics injects the Prometheus shim. Calling with nil restores the
// no-op default so the loop still runs without observability wired.
func (r *positionQuoteRefresher) SetMetrics(m positionRefreshMetrics) {
	if r == nil {
		return
	}
	if m == nil {
		r.metrics = noopPositionRefreshMetrics{}
		return
	}
	r.metrics = m
}

func (r *positionQuoteRefresher) isLeader() bool {
	if r == nil || r.leader == nil {
		return true
	}
	return r.leader.IsLeader(PositionQuoteRefreshLeaseName)
}

// Start launches the background goroutine. Idempotent: subsequent calls
// after the first are no-ops. The first sweep runs immediately so a
// freshly deployed cluster doesn't wait a full interval before refreshing
// stale prices.
func (r *positionQuoteRefresher) Start() {
	if r == nil || r.fundRepo == nil || r.positionRepo == nil || r.marketData == nil {
		return
	}
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return
	}
	if r.stopCh == nil {
		r.stopCh = make(chan struct{})
	}
	stopCh := r.stopCh
	r.started = true
	r.wg.Add(1)
	r.mu.Unlock()

	go func() {
		defer r.wg.Done()
		// Initial warmup. ~10s so the leader-election lease has
		// settled and other startup tasks (migrations, cron loops) have
		// quietened down.
		warmup := time.NewTimer(10 * time.Second)
		select {
		case <-stopCh:
			warmup.Stop()
			return
		case <-warmup.C:
		}
		r.runOnce()

		for {
			next := r.nextInterval()
			ticker := time.NewTimer(next)
			select {
			case <-stopCh:
				ticker.Stop()
				return
			case <-ticker.C:
				r.runOnce()
			}
		}
	}()
}

func (r *positionQuoteRefresher) nextInterval() time.Duration {
	if r.inSessionFn != nil && r.inSessionFn(r.nowFn()) {
		return r.tickInSession
	}
	return r.tickOffSession
}

// runOnce is the body of a single sweep. Exported for tests via a thin
// wrapper in *_test.go so we can drive it deterministically.
func (r *positionQuoteRefresher) runOnce() {
	if r == nil {
		return
	}
	if !r.isLeader() {
		return
	}
	startedAt := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), r.perPassTimeout)
	defer cancel()

	funds, err := r.fundRepo.ListActive(ctx)
	if err != nil {
		slog.Warn("position quote refresh: list funds failed", "error", err)
		r.metrics.RecordRefreshPass(0, time.Since(startedAt), true)
		return
	}
	if len(funds) == 0 {
		r.metrics.RecordRefreshPass(0, time.Since(startedAt), false)
		return
	}

	// Step 1: enumerate (fund, position) pairs and the unique set of
	// instruments we need quotes for. Doing this up-front lets the
	// quote round-trip happen *once* per instrument across all funds.
	type fundPositions struct {
		fund      repository.Fund
		positions []repository.HoldingPosition
	}
	allByFund := make([]fundPositions, 0, len(funds))
	uniqueRefs := make(map[string]marketdata.InstrumentRef)
	for i := range funds {
		fund := funds[i]
		positions, err := r.positionRepo.ListByFund(ctx, fund.ID)
		if err != nil {
			slog.Warn("position quote refresh: list positions failed",
				"fund_id", fund.ID, "error", err,
			)
			continue
		}
		if len(positions) == 0 {
			continue
		}
		allByFund = append(allByFund, fundPositions{fund: fund, positions: positions})
		for j := range positions {
			ref := positionInstrumentRef(&positions[j])
			key := ref.CacheKey()
			if _, ok := uniqueRefs[key]; ok {
				continue
			}
			uniqueRefs[key] = ref
		}
	}
	if len(uniqueRefs) == 0 {
		r.metrics.RecordRefreshPass(0, time.Since(startedAt), false)
		return
	}

	// Step 2: batch GetQuotes. PR-1's singleflight + per-provider rate
	// limit + 10s cache make this safe even with hundreds of unique
	// instruments — most calls will hit the cache, and only the few
	// that fall through actually touch the upstream.
	refs := make([]marketdata.InstrumentRef, 0, len(uniqueRefs))
	for _, ref := range uniqueRefs {
		refs = append(refs, ref)
	}
	bySymbol := r.marketData.GetQuotes(ctx, refs)
	// S6.5 WS overlay: for any symbol that has a fresh WS
	// cache hit, use that snapshot instead of the REST result.
	// WS data is by definition more recent (push semantics) so
	// this only ever moves prices forward, never backwards.
	if r.wsCache != nil {
		now := r.nowFn()
		for _, ref := range refs {
			key := ref.NormalizedSymbol()
			snap, ok, stale := r.wsCache.Lookup(ref.NormalizedSymbol())
			if !ok || stale || snap.Last <= 0 {
				continue
			}
			existing := bySymbol[key]
			if existing != nil && !existing.AsOf.IsZero() && existing.AsOf.After(snap.AsOf) {
				// REST quote is somehow newer (e.g. WS just
				// reconnected and we haven't received the
				// first tick yet) — keep the REST value.
				continue
			}
			bySymbol[key] = &marketdata.QuoteSnapshot{
				Symbol:    ref.NormalizedSymbol(),
				Price:     snap.Last,
				Bid:       snap.Bid,
				Ask:       snap.Ask,
				AsOf:      snap.AsOf,
				IsStale:   false,
				Source:    "wsfeed",
			}
			_ = now
		}
	}
	if len(bySymbol) == 0 {
		slog.Debug("position quote refresh: no quotes returned",
			"unique_instruments", len(refs),
		)
		r.metrics.RecordRefreshPass(0, time.Since(startedAt), false)
		return
	}

	// Step 3: write back the fresh price + recomputed market_value /
	// unrealized_pnl per (fund_id, instrument_key). Failures are logged
	// and counted; one bad row doesn't abort the rest of the pass.
	updated := 0
	failed := 0
	for _, pair := range allByFund {
		for k := range pair.positions {
			position := pair.positions[k]
			ref := positionInstrumentRef(&position)
			quote := bySymbol[ref.NormalizedSymbol()]
			if quote == nil || quote.Price <= 0 {
				continue
			}
			marketValue, unrealized := computeRefreshedValuation(&position, quote.Price)
			if err := r.positionRepo.UpdatePriceMetrics(ctx, position.FundID, position.InstrumentKey, quote.Price, marketValue, unrealized); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					// Position vanished between our SELECT and the
					// UPDATE — benign, downgrade to debug.
					slog.Debug("position quote refresh: row missing on update",
						"fund_id", position.FundID,
						"instrument_key", position.InstrumentKey,
					)
					continue
				}
				failed++
				slog.Warn("position quote refresh: update failed",
					"fund_id", position.FundID,
					"instrument_key", position.InstrumentKey,
					"error", err,
				)
				continue
			}
			updated++
			// Phase 3A-1 / 3A-2: extend per-lot extremes so
			// closed_lots can record MFE/MAE and the exit
			// manager's trailing rule has a valid peak to
			// trail from. Failure is silent — the price metric
			// above is the operator-visible value; the lot
			// extremes feed a derived ledger and can drift
			// without breaking trading.
			if r.lotRepo != nil {
				if err := r.lotRepo.UpdateExcursion(ctx, position.FundID, position.InstrumentKey, quote.Price, quote.AsOf); err != nil {
					slog.Debug("position quote refresh: lot excursion update failed",
						"fund_id", position.FundID,
						"instrument_key", position.InstrumentKey,
						"error", err,
					)
				}
			}
		}
	}

	slog.Info("position quote refresh pass",
		"funds_seen", len(allByFund),
		"unique_instruments", len(refs),
		"quotes_obtained", len(bySymbol),
		"rows_updated", updated,
		"failures", failed,
		"duration_ms", time.Since(startedAt).Milliseconds(),
	)
	r.metrics.RecordRefreshPass(updated, time.Since(startedAt), failed > 0)
}

// computeRefreshedValuation derives the new MarketValue / UnrealizedPnL
// for a position given a fresh price. Extracted so the refresher and the
// existing applyPositionValuation share an identical computation path —
// the difference is just where they write the result.
func computeRefreshedValuation(position *repository.HoldingPosition, freshPrice float64) (float64, sql.NullFloat64) {
	if position == nil || freshPrice <= 0 {
		return 0, sql.NullFloat64{}
	}
	multiplier := contractMultiplierValue(position.ContractMultiplier)
	marketValue := roundCurrency(position.Quantity * freshPrice * multiplier)
	if !isFuturesPosition(*position) {
		marketValue = roundCurrency(position.Quantity * freshPrice)
	}
	var unrealized sql.NullFloat64
	if position.Quantity != 0 && position.CostPrice > 0 {
		delta := freshPrice - position.CostPrice
		// Short positions earn from price drops. Match the convention
		// already encoded in applyPositionValuation so the user sees a
		// consistent number when a manual trigger and the background
		// refresher race.
		if isShortPosition(position) {
			delta = position.CostPrice - freshPrice
		}
		pnl := roundCurrency(delta * position.Quantity * multiplier)
		unrealized = sql.NullFloat64{Float64: pnl, Valid: true}
	}
	return marketValue, unrealized
}

func isShortPosition(position *repository.HoldingPosition) bool {
	if position == nil {
		return false
	}
	side := position.PositionSide.String
	if len(side) == 0 {
		return false
	}
	// Case-insensitive "short" match — same semantics as
	// applyPositionValuation.
	if len(side) == 5 && (side == "short" || side == "Short" || side == "SHORT") {
		return true
	}
	return false
}

// Stop halts the goroutine and waits for it to finish. Safe to call
// multiple times.
func (r *positionQuoteRefresher) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return
	}
	stopCh := r.stopCh
	r.stopCh = nil
	r.started = false
	r.mu.Unlock()
	close(stopCh)
	r.wg.Wait()
}
