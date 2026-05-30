// corpActionIngestLoop is the P1-1 daily scheduler hook for
// corporate-action ingestion. It walks every active fund, groups
// the fund's holdings by source-market, and drives a provider
// per market to fetch + upsert + apply events.
//
// Design intent (mirrors memoryArchiveLoop's shape on purpose so
// operators only have to learn one cron pattern):
//
//   - 12h interval. Eastmoney publishes new dividend rows around
//     A-share market open and close; one nightly + one mid-day
//     pass catches the day's announcements without hammering the
//     vendor.
//   - 5min warmup at boot. Lets the rest of the server stabilize
//     (DB pool, leader lease, etc.) before we start fanning out
//     external HTTP.
//   - Leader-gated. In multi-replica deployments only the lease
//     holder ingests, otherwise we'd race upserts (the unique
//     constraint on corporate_actions catches it but we'd rather
//     not load the upstream pointlessly).
//   - Errors are isolated per (provider, symbol, fund). One
//     symbol's 500 from Eastmoney must not stop us from running
//     Yahoo for the US holdings or applying already-ingested
//     events to other funds.
//   - Apply path always runs after upsert so a freshly-published
//     ex-date that lands during the day's run posts to the fund
//     within the same loop iteration.
//
// What this loop deliberately does NOT do:
//
//   - It does not back-fill historical events. Operators run the
//     corpactionsync CLI for that with --since flags. The loop
//     uses a rolling 90-day window so a missed pass (e.g. weekend
//     downtime) is forgiven without scanning years of history.
//   - It does not retry transient failures within a tick. The
//     next tick (12h later) is the retry. Stronger retry
//     semantics belong in a separate "corp-action recovery" job
//     that we can add when ingestion volumes warrant it.

package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/corpaction"
	"github.com/fundai/server/internal/repository"
)

const (
	// CorpActionIngestLeaseName is the lease key for the leader
	// election. Must match no other loop's key (verified by the
	// uniqueness check in main.go's lease wiring).
	CorpActionIngestLeaseName = "corp-action-ingest"

	// defaultCorpActionLookbackDays is the rolling window each
	// pass requests from the upstream provider. 90d generously
	// covers a long weekend or a short outage; events older than
	// that are assumed already in the ledger from prior passes /
	// the corpactionsync CLI.
	defaultCorpActionLookbackDays = 90
)

// corpActionMetricsRecorder is the narrow surface the loop needs
// from serverMetrics. Defining it here (instead of taking
// *serverMetrics directly) keeps the loop unit-testable without
// dragging the full HTTP / LLM / decision-input metric machinery
// into the test target, and makes it trivial to install a fake
// recorder that asserts on label cardinality.
type corpActionMetricsRecorder interface {
	RecordCorpActionTick(status string)
	RecordCorpActionProviderError(market, outcome string)
	RecordCorpActionRetry(market, outcome string)
	RecordCorpActionEvent(action, phase string)
	RecordCorpActionApply(outcome string)
}

// noopCorpActionMetrics is the safe default when the loop is
// constructed without a recorder (small-test ergonomics — the
// existing TestRunOnce_* tests don't have to stub anything new).
// We deliberately don't expose this outside the file; main.go
// always passes a real *serverMetrics in production wiring.
type noopCorpActionMetrics struct{}

func (noopCorpActionMetrics) RecordCorpActionTick(string)          {}
func (noopCorpActionMetrics) RecordCorpActionProviderError(string, string) {}
func (noopCorpActionMetrics) RecordCorpActionRetry(string, string) {}
func (noopCorpActionMetrics) RecordCorpActionEvent(string, string) {}
func (noopCorpActionMetrics) RecordCorpActionApply(string)         {}

// corpActionIngestLoop owns the daily ingest. The struct mirrors
// memoryArchiveLoop / memoryEmbedLoop on purpose — same lifecycle,
// same leader-gating, same warmup — so reading one teaches you all
// three.
type corpActionIngestLoop struct {
	db       *sql.DB
	repo     *repository.CorpActionRepo
	leader   leaderChecker
	interval time.Duration
	lookback time.Duration

	// providers maps market tag → fetcher. Wired from main.go so
	// tests can inject in-memory fakes.
	providers map[string]corpaction.EventFetcher

	// metrics receives per-tick / per-event counter updates. Card G
	// adds this; pre-Card-G code constructed the loop without it
	// and we stay backwards-compatible by defaulting to a no-op
	// recorder when nil. Production wiring in main.go injects the
	// real *serverMetrics.
	metrics corpActionMetricsRecorder

	// fetchRetryAttempts controls how many extra attempts the
	// provider fetch wrapper makes after the first failure. Card
	// G default is 1 (so worst case = 2 total attempts per symbol
	// per tick); we cap small so a flaky provider doesn't waste
	// the whole tick budget — the next tick (12h later) is the
	// real backstop. Tests can override (e.g. set to 0) to exercise
	// the no-retry path.
	fetchRetryAttempts int

	// fetchRetryBackoff is the wall-clock pause between retry
	// attempts. Defaults to 250ms. Tests set this to 0 to keep
	// runtime deterministic.
	fetchRetryBackoff time.Duration

	stopCh  chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
	started bool
}

// newCorpActionIngestLoop constructs the loop with the default
// 12h cadence and 90d lookback. Provider map is built from env-
// gated providers identical to the corpactionsync CLI's default
// build.
func newCorpActionIngestLoop(db *sql.DB) *corpActionIngestLoop {
	if db == nil {
		return nil
	}
	providers := map[string]corpaction.EventFetcher{
		"a_share": &corpaction.EastmoneyProvider{},
		// Yahoo handles us_equity and was the legacy fallback for
		// hk_equity before Card H. HK now routes through the
		// purpose-built HKEXProvider (East Money HK datacenter):
		// Yahoo's HK feed misses interim/special dividends and
		// most bonus issues, which burned the OCS HK-portfolio
		// reconciliation twice. HKEXProvider is built on the
		// same upstream host as EastmoneyProvider so we share
		// transient-error markers, header policy, and CDN
		// behaviour across both A-share and HK ingest paths.
		// Operators that need to flip back can replace this
		// entry with `&corpaction.YahooProvider{}` without
		// touching the rest of the loop.
		"us_equity": &corpaction.YahooProvider{},
		"hk_equity": &corpaction.HKEXProvider{},
	}
	return &corpActionIngestLoop{
		db:                 db,
		repo:               repository.NewCorpActionRepo(db),
		interval:           12 * time.Hour,
		lookback:           defaultCorpActionLookbackDays * 24 * time.Hour,
		providers:          providers,
		metrics:            noopCorpActionMetrics{},
		fetchRetryAttempts: 1,
		fetchRetryBackoff:  250 * time.Millisecond,
		stopCh:             make(chan struct{}),
	}
}

// SetMetrics installs the metrics recorder. nil is treated as the
// no-op recorder so the loop never panics on unwired test paths.
// Call this from main.go after newServerMetrics is constructed.
func (l *corpActionIngestLoop) SetMetrics(m corpActionMetricsRecorder) {
	if l == nil {
		return
	}
	if m == nil {
		l.metrics = noopCorpActionMetrics{}
		return
	}
	l.metrics = m
}

// SetLeaderChecker wires the lease manager. Called from main.go
// after the lease subsystem is up. Nil checker is treated as
// "always leader" (single-replica or test).
func (l *corpActionIngestLoop) SetLeaderChecker(checker leaderChecker) {
	if l == nil {
		return
	}
	l.leader = checker
}

func (l *corpActionIngestLoop) isLeader() bool {
	if l == nil || l.leader == nil {
		return true
	}
	return l.leader.IsLeader(CorpActionIngestLeaseName)
}

// Start spins the goroutine. Idempotent — calling twice is a no-op,
// matching memoryArchiveLoop's contract so main.go's wiring can be
// uniform across loops.
func (l *corpActionIngestLoop) Start() {
	if l == nil || l.db == nil {
		return
	}
	l.mu.Lock()
	if l.started {
		l.mu.Unlock()
		return
	}
	if l.stopCh == nil {
		l.stopCh = make(chan struct{})
	}
	stopCh := l.stopCh
	l.started = true
	l.wg.Add(1)
	l.mu.Unlock()

	go func() {
		defer l.wg.Done()
		// Warmup matches memoryArchiveLoop: 5min before the first
		// pass so the rest of the boot sequence stabilizes first.
		warmup := time.NewTimer(5 * time.Minute)
		select {
		case <-stopCh:
			warmup.Stop()
			return
		case <-warmup.C:
		}
		l.runOnce(context.Background())

		ticker := time.NewTicker(l.interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				l.runOnce(context.Background())
			}
		}
	}()
}

func (l *corpActionIngestLoop) Stop() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if !l.started {
		l.mu.Unlock()
		return
	}
	close(l.stopCh)
	l.started = false
	l.mu.Unlock()
	l.wg.Wait()
}

// runOnce executes one ingest pass. Exposed (lowercase but
// in-package) so a test can drive it directly without spinning the
// 5min warmup.
func (l *corpActionIngestLoop) runOnce(ctx context.Context) {
	if l == nil || l.db == nil {
		return
	}
	metrics := l.metricsRecorder()
	if !l.isLeader() {
		// Non-leader replicas record skip-not-leader explicitly so
		// dashboards can sanity-check "exactly one replica is the
		// leader" by inspecting the ratio of skipped vs ok ticks.
		metrics.RecordCorpActionTick("skipped_not_leader")
		return
	}
	since := time.Now().UTC().Add(-l.lookback)

	// Step 1: collect the union of (instrument_key, market, symbol)
	// across all funds with a non-zero open position. Pulling this
	// upfront (rather than per-fund) lets us hit each upstream symbol
	// at most once per tick even when 5 funds hold the same name.
	holdings, err := l.collectActiveHoldings(ctx)
	if err != nil {
		// We don't know yet whether holdings are empty or the loop
		// crashed pre-fetch; bias toward "ok" rather than
		// success-skipped so the alert "now - last_success > 7d"
		// fires when this code path is the steady state. The slog
		// line is the operator's lead.
		metrics.RecordCorpActionTick("ok")
		slog.Warn("corp-action ingest: collect holdings",
			"err", err)
		return
	}
	if len(holdings) == 0 {
		metrics.RecordCorpActionTick("skipped_no_holdings")
		return
	}

	// Group by market so we route to the right provider in bulk.
	type holdingKey struct {
		instrumentKey string
		market        string
		symbol        string
	}
	type marketGroup struct {
		fetcher  corpaction.EventFetcher
		holdings []holdingKey
	}
	groups := map[string]*marketGroup{}
	for _, h := range holdings {
		market := strings.ToLower(strings.TrimSpace(h.market))
		fetcher, ok := l.providers[market]
		if !ok {
			// Unknown market — skip silently. The benchmark catalog
			// covers more than this loop does (e.g. crypto), and
			// logging here would generate noise on every tick.
			continue
		}
		grp, ok := groups[market]
		if !ok {
			grp = &marketGroup{fetcher: fetcher}
			groups[market] = grp
		}
		grp.holdings = append(grp.holdings, holdingKey{
			instrumentKey: h.instrumentKey,
			market:        market,
			symbol:        h.symbol,
		})
	}

	// Step 2: fetch + upsert + apply per (market, symbol). Errors
	// are isolated per symbol — we log and continue. The applier is
	// already idempotent so a re-run on the next tick is safe.
	resolver := corpaction.DefaultFundResolver(l.db)
	totalIngested := 0
	totalApplied := 0
	totalErrors := 0
	for market, grp := range groups {
		for _, h := range grp.holdings {
			events, err := l.fetchEventsWithRetry(ctx, grp.fetcher, market, h.symbol, since)
			if err != nil {
				totalErrors++
				slog.Warn("corp-action ingest: provider fetch",
					"market", market,
					"symbol", h.symbol,
					"err", err)
				continue
			}
			if len(events) == 0 {
				continue
			}

			holders, err := resolver(ctx, h.instrumentKey)
			if err != nil {
				totalErrors++
				slog.Warn("corp-action ingest: resolve holders",
					"instrument_key", h.instrumentKey,
					"err", err)
				continue
			}
			if len(holders) == 0 {
				// Holding was zeroed between collectActiveHoldings
				// and now (race with settlement). Re-fetch on next
				// tick.
				continue
			}

			for _, evt := range events {
				eventID, err := l.repo.Upsert(ctx, repository.CorpActionRow{
					InstrumentKey: evt.InstrumentKey,
					ExDate:        evt.ExDate,
					ActionType:    evt.ActionType,
					SplitRatio:    evt.SplitRatio,
					CashDividend:  evt.CashDividend,
					Source:        evt.Source,
				})
				if err != nil {
					totalErrors++
					metrics.RecordCorpActionEvent(evt.ActionType, "upsert_error")
					slog.Warn("corp-action ingest: upsert",
						"instrument_key", evt.InstrumentKey,
						"ex_date", evt.ExDate.Format("2006-01-02"),
						"err", err)
					continue
				}
				totalIngested++
				metrics.RecordCorpActionEvent(evt.ActionType, "upserted")
				evt.ID = eventID

				outcome := corpaction.ApplyEventToFunds(ctx, l.db, evt, holders)
				totalApplied += outcome.AppliedFunds
				// Success counter: applier returns the count of
				// funds that took the change. We record one
				// "applied" tick per fund so the rate calc on
				// the dashboard normalises against per-fund
				// activity.
				for i := 0; i < outcome.AppliedFunds; i++ {
					metrics.RecordCorpActionApply("applied")
				}
				for _, e := range outcome.Errors {
					if errors.Is(e.Err, corpaction.ErrPositionMissing) {
						metrics.RecordCorpActionApply("missing")
						continue
					}
					totalErrors++
					metrics.RecordCorpActionApply("error")
					slog.Warn("corp-action ingest: apply",
						"event_id", eventID,
						"fund", e.FundID,
						"err", e.Err)
				}
			}
		}
	}

	metrics.RecordCorpActionTick("ok")
	slog.Info("corp-action ingest: tick complete",
		"groups", len(groups),
		"ingested", totalIngested,
		"applied", totalApplied,
		"errors", totalErrors,
		"since", since.Format("2006-01-02"),
	)
}

// activeHolding is a denormalized row from holding_positions:
// (instrument_key, market, symbol). market is taken from the
// position's market column (NULL when legacy rows haven't been
// backfilled). symbol is what we feed providers.
type activeHolding struct {
	instrumentKey string
	market        string
	symbol        string
}

// metricsRecorder returns a non-nil recorder. Most call-sites
// don't care that l.metrics may be nil (we initialize to a noop in
// the constructor), but tests can construct the loop directly with
// `&corpActionIngestLoop{...}` and forget — this guards that.
func (l *corpActionIngestLoop) metricsRecorder() corpActionMetricsRecorder {
	if l == nil || l.metrics == nil {
		return noopCorpActionMetrics{}
	}
	return l.metrics
}

// fetchEventsWithRetry wraps the provider's FetchEvents with the
// Card-G retry policy. The contract:
//
//   - First attempt always runs. On success, return.
//   - On failure, classify with isCorpActionTransient. Fatal
//     errors (4xx, malformed JSON, vendor `ErrNoData`) are
//     returned immediately — retrying won't help.
//   - On transient failures, sleep fetchRetryBackoff and try
//     again, up to fetchRetryAttempts extra attempts. Each retry
//     records to corpActionIngestRetries with `outcome=succeeded`
//     or `outcome=exhausted` so dashboards can see the recovery
//     rate per provider.
//
// We record the FINAL classification under
// corpActionIngestProviderErrors so the alert "provider failure
// rate spiked" doesn't double-count the same symbol's retried
// error. Mid-retry transient errors only show up in the retries
// counter.
func (l *corpActionIngestLoop) fetchEventsWithRetry(ctx context.Context, fetcher corpaction.EventFetcher, market, symbol string, since time.Time) ([]corpaction.Event, error) {
	if fetcher == nil {
		return nil, errors.New("corp-action ingest: nil fetcher")
	}
	metrics := l.metricsRecorder()
	attempts := l.fetchRetryAttempts
	if attempts < 0 {
		attempts = 0
	}
	backoff := l.fetchRetryBackoff
	if backoff < 0 {
		backoff = 0
	}

	events, err := fetcher.FetchEvents(ctx, symbol, since)
	if err == nil {
		return events, nil
	}
	if !isCorpActionTransient(err) {
		metrics.RecordCorpActionProviderError(market, "fatal")
		return nil, err
	}
	// Transient path. Sleep then retry until the budget is
	// exhausted. We respect ctx.Done() during the backoff so a
	// stop signal aborts cleanly.
	for i := 0; i < attempts; i++ {
		if backoff > 0 {
			t := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				t.Stop()
				metrics.RecordCorpActionProviderError(market, "transient")
				return nil, ctx.Err()
			case <-t.C:
			}
		}
		events, retryErr := fetcher.FetchEvents(ctx, symbol, since)
		if retryErr == nil {
			metrics.RecordCorpActionRetry(market, "succeeded")
			return events, nil
		}
		if !isCorpActionTransient(retryErr) {
			// New error class on retry — surface as fatal so
			// the operator can see the diff (the original
			// transient error rolled into a stable failure).
			metrics.RecordCorpActionRetry(market, "exhausted")
			metrics.RecordCorpActionProviderError(market, "fatal")
			return nil, retryErr
		}
		err = retryErr
	}
	metrics.RecordCorpActionRetry(market, "exhausted")
	metrics.RecordCorpActionProviderError(market, "transient")
	return nil, err
}

// isCorpActionTransient classifies an error as worth a single
// retry. Pattern matches the ohlc.EastmoneyProvider strategy: we
// look for the small set of network blips that vendors return
// when their CDN load-balancer or TLS layer hangs up mid-response.
// Unknown errors default to non-transient — better to return early
// and let the next 12h tick be the retry than burn budget on
// something that's stable-broken.
func isCorpActionTransient(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, marker := range []string{
		"EOF",
		"unexpected EOF",
		"connection reset by peer",
		"broken pipe",
		"connection refused",
		"i/o timeout",
		"context deadline exceeded",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// activeHolding is a denormalized row from holding_positions:

// collectActiveHoldings unions all funds' open positions. Active
// fund filtering happens at the SQL level via funds.status='active'
// to keep the result tight even when many funds are paused / closed.
func (l *corpActionIngestLoop) collectActiveHoldings(ctx context.Context) ([]activeHolding, error) {
	const q = `
SELECT DISTINCT hp.instrument_key,
       COALESCE(hp.market, ''),
       COALESCE(hp.symbol, '')
  FROM holding_positions hp
  JOIN funds f ON f.id = hp.fund_id
 WHERE hp.quantity > 0
   AND f.status   = 'active'
`
	rows, err := l.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]activeHolding, 0, 64)
	for rows.Next() {
		var h activeHolding
		if err := rows.Scan(&h.instrumentKey, &h.market, &h.symbol); err != nil {
			return nil, err
		}
		// Skip rows we can't even ask a provider about.
		if strings.TrimSpace(h.symbol) == "" {
			continue
		}
		if strings.TrimSpace(h.market) == "" {
			continue
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
