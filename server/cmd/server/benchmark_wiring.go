package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/benchmark"
	"github.com/fundai/server/internal/ohlc"
	"github.com/fundai/server/internal/repository"
)

// benchmarkServiceAdapter implements api.BenchmarkService.
//
// Pieces it stitches together:
//
//   - authorizeFundAccess: shared fund-membership gate (same as the
//     fund / corp-action adapters).
//   - NavSnapshotRepo:     reads the fund's daily NAV history.
//   - HoldingPositionsRepo: reads current symbols so the recommender
//     can upweight sector benchmarks.
//   - benchmark.Catalog:   curated index list and recommendation
//     rules (pure data, no IO).
//   - ohlc.Fetcher:        market-data chain that already powers
//     the quant indicator pipeline. We reuse it instead of opening
//     a new chain because (a) it has the same provider list we
//     trust elsewhere and (b) its TTL cache already absorbs repeat
//     calls if multiple users open the same fund's chart.
//
// Failure semantics: a single benchmark series that 404s / times
// out becomes a PartialFailure entry rather than a 500, so the
// chart still renders the fund line and any series that succeeded.
type benchmarkServiceAdapter struct {
	fundRepo     *repository.FundRepo
	companyRepo  *repository.FundCompanyRepo
	navRepo      *repository.NavSnapshotRepo
	holdingsRepo *repository.PositionRepo
	fetcher      ohlc.Fetcher
}

// Compile-time check: benchmarkServiceAdapter implements both the
// benchmark and the holdings-series API contracts. The two read
// surfaces share repos / fetcher so collapsing them into one
// adapter halves the wiring noise in main.go.
var (
	_ api.BenchmarkService      = (*benchmarkServiceAdapter)(nil)
	_ api.HoldingsSeriesService = (*benchmarkServiceAdapter)(nil)
)

// newBenchmarkServiceAdapter constructs the adapter. Returns nil
// when ohlc is disabled at the env level — the handler will then
// 503 cleanly. Cheap: only allocates wrappers, no goroutines / pools.
func newBenchmarkServiceAdapter(svc *Services) api.BenchmarkService {
	if svc == nil || svc.DB == nil {
		return nil
	}
	fetcher := buildOHLCFetcherFromEnv()
	if fetcher == nil {
		// OHLC chain is opt-out; if disabled, benchmarks have no
		// data source and we'd rather 503 than ship a fund-only
		// chart that misleads the user about benchmark coverage.
		return nil
	}
	return &benchmarkServiceAdapter{
		fundRepo:     repository.NewFundRepo(svc.DB),
		companyRepo:  repository.NewFundCompanyRepo(svc.DB),
		navRepo:      repository.NewNavSnapshotRepo(svc.DB),
		holdingsRepo: repository.NewPositionRepo(svc.DB),
		fetcher:      fetcher,
	}
}

// History is the meat of Card B.
//
// Algorithm:
//  1. Authorize: fund-membership check (uniform with rest of the
//     fund-scope endpoints).
//  2. Date range: [now - days, now], floor 7d / cap 5y already
//     enforced upstream in the handler.
//  3. Fund NAV: pull nav_snapshots in range, normalize to 100.
//  4. Series: if the caller didn't pick any, derive a recommended
//     set from the fund's market + held symbols.
//  5. For each series id: catalog lookup → ohlc.Fetcher call →
//     normalize. Collect partial failures rather than aborting.
//  6. Build the response, including the catalog + recommended list
//     so the UI can populate its picker without a second round trip.
func (s *benchmarkServiceAdapter) History(ctx context.Context, userID, fundID string, days int, ids []string) (api.BenchmarkHistoryResponse, error) {
	if s == nil {
		return api.BenchmarkHistoryResponse{}, errors.New("benchmark service: not configured")
	}
	fund, err := authorizeFundAccess(ctx, s.fundRepo, s.companyRepo, userID, fundID)
	if err != nil {
		return api.BenchmarkHistoryResponse{}, err
	}

	to := time.Now().UTC()
	from := to.AddDate(0, 0, -days)

	// 3. Fund NAV history. nav_snapshots is the canonical daily
	// truth — we use NAV (not total_assets) because the chart's
	// rebase to 100 only makes sense for the per-share metric.
	navs, err := s.navRepo.ListByFund(ctx, fund.ID, from, to)
	if err != nil {
		return api.BenchmarkHistoryResponse{}, mapRepositoryError(err)
	}
	fundDates, fundValues := navsToSeries(navs)
	fundPoints, fundErr := benchmark.Normalize(fundDates, fundValues)
	// A fund with zero NAV history (brand-new fund, never run a
	// daily) should not error — the UI shows "no NAV yet" empty
	// state. We pass an empty Points slice in that case.
	if errors.Is(fundErr, benchmark.ErrEmptySeries) {
		fundPoints = nil
	} else if fundErr != nil {
		return api.BenchmarkHistoryResponse{}, fundErr
	}

	// 4. Recommend if no explicit picks.
	profile := decodeFundMarketProfile(fund.Config)
	market := mapFundMarketToCatalog(profile.Market)
	if len(ids) == 0 {
		symbols := s.fundSymbols(ctx, fund.ID)
		ids = benchmark.Recommend(benchmark.FundProfile{
			Market:  market,
			Symbols: symbols,
		})
	}

	// 5. Fetch each series. Provider failures become PartialFailures
	// so the chart still renders the fund + whichever series did
	// succeed.
	bSeries := make([]api.BenchmarkSeriesDTO, 0, len(ids))
	failures := make([]api.BenchmarkPartialFailure, 0)
	for _, id := range ids {
		def, ok := benchmark.ByID(id)
		if !ok {
			failures = append(failures, api.BenchmarkPartialFailure{
				ID: id, Reason: "unknown",
			})
			continue
		}
		bars, err := s.fetcher.Fetch(ctx, ohlc.FetchRequest{
			Symbol:    def.Symbol,
			Market:    def.Market,
			Interval:  ohlc.IntervalDay,
			LookbackN: days + 5, // small buffer so a non-trading-day at the boundary doesn't cut us short
			EndTime:   to,
		})
		if err != nil || len(bars) == 0 {
			failures = append(failures, api.BenchmarkPartialFailure{
				ID:     id,
				Reason: classifyFetchError(err),
			})
			continue
		}
		// Yahoo's chart endpoint returns ONLY a single regular-market
		// bar for a handful of A-share indices (e.g., chinext /
		// star50 / csi500) whose `validRanges` upstream is just
		// ["1d","5d"] — no historical depth. A single normalized
		// point would render as a flat dot at value=100 and confuse
		// users into thinking the benchmark "didn't move at all".
		// Surface this as `no-data` instead so the UI shows the
		// "skipped" toast (and so the registry — if a richer
		// provider is wired later — can decide to retry).
		if len(bars) < 2 {
			failures = append(failures, api.BenchmarkPartialFailure{
				ID:     id,
				Reason: "no-data",
			})
			continue
		}
		dates, values := barsToSeries(bars, from)
		pts, err := benchmark.Normalize(dates, values)
		if err != nil {
			failures = append(failures, api.BenchmarkPartialFailure{
				ID: id, Reason: "no-data-in-range",
			})
			continue
		}
		bSeries = append(bSeries, api.BenchmarkSeriesDTO{
			ID:       def.ID,
			Label:    def.Label,
			Symbol:   def.Symbol,
			Market:   def.Market,
			Currency: def.Currency,
			Points:   pointsToDTO(pts),
		})
	}

	// 6. Build response.
	available := make([]api.BenchmarkCatalogItem, 0, len(benchmark.Catalog))
	for _, c := range benchmark.Catalog {
		available = append(available, api.BenchmarkCatalogItem{
			ID:     c.ID,
			Label:  c.Label,
			Symbol: c.Symbol,
			Market: c.Market,
		})
	}
	recommended := benchmark.Recommend(benchmark.FundProfile{
		Market:  market,
		Symbols: s.fundSymbols(ctx, fund.ID),
	})

	// 7. Compute holding-vs-benchmark overlap so the UI can render
	// the "switch to Alpha view" hint when the fund is structurally
	// the benchmark (e.g., a futures fund 100% in BTCUSDT). We
	// pass the rendered benchmark slice (not the requested ids)
	// so the hint only appears for series that actually drew on
	// the chart — pointing at a benchmark that failed to load
	// would be confusing.
	overlap := s.computeHoldingOverlap(ctx, fund.ID, bSeries)

	return api.BenchmarkHistoryResponse{
		FundID: fundID,
		From:   api.FormatDateForBenchmark(from),
		To:     api.FormatDateForBenchmark(to),
		Fund: api.BenchmarkSeriesDTO{
			ID:       "fund:" + fundID,
			Label:    fund.Name,
			Symbol:   fundID,
			Market:   market,
			Currency: profile.BaseCurrency,
			Points:   pointsToDTO(fundPoints),
		},
		Benchmarks:      bSeries,
		Recommended:     recommended,
		Available:       available,
		PartialFailures: failures,
		HoldingOverlap:  overlap,
	}, nil
}

// fundSymbols returns the bare symbols the fund currently holds.
// Failures are intentionally swallowed and degrade to nil — the
// recommender treats nil as "no extra signal" and falls back to
// market-only rules. We don't want a transient holdings-table error
// to fail the whole chart panel.
func (s *benchmarkServiceAdapter) fundSymbols(ctx context.Context, fundID string) []string {
	if s == nil || s.holdingsRepo == nil {
		return nil
	}
	positions, err := s.holdingsRepo.ListByFund(ctx, fundID)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(positions))
	for _, p := range positions {
		if p.Symbol != "" {
			out = append(out, p.Symbol)
		}
	}
	return out
}

// computeHoldingOverlap inspects the fund's current positions and
// figures out whether the rendered benchmark series overlap with
// any of them. The intent is to help the UI recognise the
// degenerate "fund == benchmark" case (a futures fund whose only
// holding is BTC, while the recommended benchmark is btc_usdt) so
// it can nudge the user toward the Alpha view where structural
// overlap is differenced out.
//
// Algorithm:
//  1. Load positions with quantities; if the holdings repo isn't
//     wired or returns an error, return nil — the chart will fall
//     back to its default "compare" mode and that's fine.
//  2. Build a normalized index from each benchmark's Symbol so we
//     can compare against fund symbols on equal footing (BTCUSDT
//     vs btcusdt, NVDA vs nasdaq:nvda, etc.).
//  3. Walk holdings sorted DESC by quantity. The FIRST holding
//     that matches a benchmark wins — that's the one most likely
//     to drag the fund curve flat. If that holding also has the
//     largest quantity in the fund, mark the overlap as "dominant";
//     otherwise "partial".
//  4. Return nil when nothing overlaps so the response stays
//     light (the field is omitempty on the wire).
//
// We use quantity (not market value) as the dominance proxy
// because mark-to-market lookups would require dragging the price
// fetcher through this code path, which is more complexity than
// the hint deserves. For a fund whose holdings are all the same
// asset class, quantity and notional rank identically.
func (s *benchmarkServiceAdapter) computeHoldingOverlap(ctx context.Context, fundID string, rendered []api.BenchmarkSeriesDTO) *api.BenchmarkHoldingOverlap {
	if s == nil || s.holdingsRepo == nil || len(rendered) == 0 {
		return nil
	}
	positions, err := s.holdingsRepo.ListByFund(ctx, fundID)
	if err != nil || len(positions) == 0 {
		return nil
	}
	type benchEntry struct {
		id     string
		symbol string
	}
	bench := make([]benchEntry, 0, len(rendered))
	for _, b := range rendered {
		sym := strings.ToUpper(strings.TrimSpace(b.Symbol))
		if sym == "" {
			continue
		}
		bench = append(bench, benchEntry{id: b.ID, symbol: sym})
	}
	if len(bench) == 0 {
		return nil
	}
	// Find the position with the largest quantity so we can answer
	// "is the matched holding actually dominant?" without a second
	// pass.
	largestQty := 0.0
	largestSym := ""
	for _, p := range positions {
		if p.Quantity > largestQty {
			largestQty = p.Quantity
			largestSym = strings.ToUpper(strings.TrimSpace(p.Symbol))
		}
	}
	// Sort positions DESC by quantity so the first match is the
	// most consequential one.
	sortedPositions := append([]repository.HoldingPosition(nil), positions...)
	sort.SliceStable(sortedPositions, func(i, j int) bool {
		return sortedPositions[i].Quantity > sortedPositions[j].Quantity
	})

	for _, p := range sortedPositions {
		holdingSym := normalizeHoldingSymbol(p.Symbol)
		if holdingSym == "" {
			continue
		}
		for _, b := range bench {
			if symbolsMatchForOverlap(holdingSym, b.symbol) {
				strength := "partial"
				if largestSym != "" && normalizeHoldingSymbol(largestSym) == holdingSym {
					strength = "dominant"
				}
				return &api.BenchmarkHoldingOverlap{
					PrimaryBenchmark: b.id,
					OverlapStrength:  strength,
					MatchedSymbols:   []string{p.Symbol},
				}
			}
		}
	}
	return nil
}

// normalizeHoldingSymbol strips exchange prefixes / suffixes that
// don't survive into benchmark symbol form. Examples:
//
//	"NASDAQ:NVDA" -> "NVDA"
//	"688195.SS"   -> "688195"
//	"BTC-USD"     -> "BTCUSD"
//	"BTC/USDT"    -> "BTCUSDT"
//
// The result is uppercased and stripped of separators so
// symbolsMatchForOverlap can do a flat string comparison plus a
// few ticker-pair aliases for crypto.
func normalizeHoldingSymbol(raw string) string {
	s := strings.ToUpper(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	for _, suf := range []string{".SS", ".SH", ".SZ", ".BJ", ".HK"} {
		if strings.HasSuffix(s, suf) {
			s = strings.TrimSuffix(s, suf)
			break
		}
	}
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "/", "")
	return s
}

// symbolsMatchForOverlap compares a normalized holding symbol with
// a benchmark symbol (already uppercased by the caller). We allow
// a small alias table for crypto pairs so "BTCUSD" / "BTCUSDT" /
// "BTC" / "XBTUSDT" all collide with the btc_usdt benchmark
// symbol "BTCUSDT". For everything else we require an exact match
// after normalization — fuzzy matching on equity tickers risks
// false positives (e.g., META vs MET).
func symbolsMatchForOverlap(holdingNorm, benchSym string) bool {
	if holdingNorm == "" || benchSym == "" {
		return false
	}
	if holdingNorm == benchSym {
		return true
	}
	// Crypto aliasing: collapse common BTC / ETH variants. Using a
	// small static table is safer than parsing the prefix because
	// some indices share prefixes (e.g., BTCUSDT vs BTCUSD).
	cryptoAliases := map[string][]string{
		"BTCUSDT": {"BTC", "BTCUSD", "BTCUSDT", "XBTUSD", "XBTUSDT"},
		"ETHUSDT": {"ETH", "ETHUSD", "ETHUSDT"},
	}
	if alts, ok := cryptoAliases[benchSym]; ok {
		for _, a := range alts {
			if holdingNorm == a {
				return true
			}
		}
	}
	return false
}

// navsToSeries flattens NavSnapshot rows into the parallel
// (dates, values) slices benchmark.Normalize expects. We use NAV
// (not totalAssets) because the chart is on a per-share basis.
//
// Snapshots are sorted by trading_date ascending in the repo, but
// we don't depend on that — Normalize re-sorts.
func navsToSeries(navs []repository.NavSnapshot) ([]time.Time, []float64) {
	dates := make([]time.Time, 0, len(navs))
	values := make([]float64, 0, len(navs))
	for _, n := range navs {
		if n.NAV <= 0 {
			continue
		}
		dates = append(dates, n.TradingDate)
		values = append(values, n.NAV)
	}
	return dates, values
}

// barsToSeries flattens ohlc.Bar rows into the parallel slices the
// normalizer expects, dropping anything strictly before `from` so
// the rebase anchor is the requested window's first trading day
// (not whatever ancient row the provider returned to satisfy
// LookbackN).
func barsToSeries(bars []ohlc.Bar, from time.Time) ([]time.Time, []float64) {
	dates := make([]time.Time, 0, len(bars))
	values := make([]float64, 0, len(bars))
	cutoff := from.UTC().Truncate(24 * time.Hour)
	for _, b := range bars {
		d := b.Time.UTC().Truncate(24 * time.Hour)
		if d.Before(cutoff) {
			continue
		}
		if b.Close <= 0 {
			continue
		}
		dates = append(dates, d)
		values = append(values, b.Close)
	}
	// Sort ascending so Normalize's first-element anchor is the
	// earliest trading day in the window.
	type pair struct {
		d time.Time
		v float64
	}
	pairs := make([]pair, len(dates))
	for i := range dates {
		pairs[i] = pair{dates[i], values[i]}
	}
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].d.Before(pairs[j].d) })
	for i, p := range pairs {
		dates[i] = p.d
		values[i] = p.v
	}
	return dates, values
}

func pointsToDTO(in []benchmark.Point) []api.BenchmarkPointDTO {
	out := make([]api.BenchmarkPointDTO, 0, len(in))
	for _, p := range in {
		out = append(out, api.BenchmarkPointDTO{
			Date:  api.FormatDateForBenchmark(p.Date),
			Value: p.Value,
		})
	}
	return out
}

// mapFundMarketToCatalog translates the fund-config market label
// into the benchmark catalog's tag. They overlap on the common
// values but legacy funds carry empty strings; we default to
// "mixed" so Recommend gives a sensible fallback.
func mapFundMarketToCatalog(market string) string {
	switch market {
	case "us_equity", "a_share", "hk_equity", "crypto", "futures":
		return market
	case "":
		return "mixed"
	default:
		return market
	}
}

// classifyFetchError turns provider errors into a UI-friendly
// reason code. We deliberately don't surface the raw error to
// avoid leaking provider names / URLs to the client.
func classifyFetchError(err error) string {
	if err == nil {
		return "no-data"
	}
	if errors.Is(err, ohlc.ErrNoData) {
		return "no-data"
	}
	if errors.Is(err, ohlc.ErrNoProvider) {
		return "unsupported-market"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	return "fetch-failed"
}

// HoldingsSeries implements api.HoldingsSeriesService.
//
// Algorithm:
//  1. Authorize fund-membership (uniform with other fund-scope
//     endpoints).
//  2. List the fund's open positions. We only chart names with
//     quantity > 0 because a closed-out holding has nothing to
//     overlay against the trailing window.
//  3. For each holding, fetch daily bars via the ohlc chain and
//     normalize to start = 100. Failures become PartialFailures so
//     a missing symbol doesn't blank the whole grid.
//
// Holdings are returned in the repo's natural order (instrument_key
// asc) so the UI grid is stable across reloads.
func (s *benchmarkServiceAdapter) HoldingsSeries(ctx context.Context, userID, fundID string, days int) (api.HoldingsSeriesResponse, error) {
	if s == nil {
		return api.HoldingsSeriesResponse{}, errors.New("holdings series: not configured")
	}
	fund, err := authorizeFundAccess(ctx, s.fundRepo, s.companyRepo, userID, fundID)
	if err != nil {
		return api.HoldingsSeriesResponse{}, err
	}

	positions, err := s.holdingsRepo.ListByFund(ctx, fund.ID)
	if err != nil {
		return api.HoldingsSeriesResponse{}, mapRepositoryError(err)
	}

	to := time.Now().UTC()
	from := to.AddDate(0, 0, -days)
	cutoff := from.UTC().Truncate(24 * time.Hour)

	items := make([]api.HoldingSeriesDTO, 0, len(positions))
	failures := make([]api.BenchmarkPartialFailure, 0)
	for _, p := range positions {
		if p.Quantity <= 0 {
			continue
		}
		market := strings.ToLower(strings.TrimSpace(p.Market.String))
		symbol := strings.TrimSpace(p.Symbol)
		if symbol == "" || market == "" {
			// Skip silently — without a market tag we can't route
			// to a provider and the user gains nothing from a
			// PartialFailure for an internally-broken row.
			continue
		}
		bars, err := s.fetcher.Fetch(ctx, ohlc.FetchRequest{
			Symbol:    symbol,
			Market:    market,
			Interval:  ohlc.IntervalDay,
			LookbackN: days + 5,
			EndTime:   to,
		})
		if err != nil || len(bars) == 0 {
			failures = append(failures, api.BenchmarkPartialFailure{
				ID:     p.InstrumentKey,
				Reason: classifyFetchError(err),
			})
			continue
		}
		dates, values := windowBarsToSeries(bars, cutoff)
		pts, err := benchmark.Normalize(dates, values)
		if err != nil {
			failures = append(failures, api.BenchmarkPartialFailure{
				ID:     p.InstrumentKey,
				Reason: "no-data-in-range",
			})
			continue
		}
		items = append(items, api.HoldingSeriesDTO{
			InstrumentKey: p.InstrumentKey,
			Symbol:        symbol,
			Name:          p.Name.String,
			Market:        market,
			EntryPrice:    p.CostPrice,
			Points:        pointsToDTO(pts),
		})
	}

	return api.HoldingsSeriesResponse{
		FundID:          fundID,
		From:            api.FormatDateForBenchmark(from),
		To:              api.FormatDateForBenchmark(to),
		Items:           items,
		PartialFailures: failures,
	}, nil
}

// windowBarsToSeries is the holdings-series counterpart to
// barsToSeries — same algorithm, kept distinct so future tweaks to
// either path don't accidentally couple them. (For example, the
// holdings path may eventually want to splice in synthetic
// "entry" anchors at the cost-basis date; that doesn't apply to
// benchmarks.)
func windowBarsToSeries(bars []ohlc.Bar, cutoff time.Time) ([]time.Time, []float64) {
	dates := make([]time.Time, 0, len(bars))
	values := make([]float64, 0, len(bars))
	for _, b := range bars {
		d := b.Time.UTC().Truncate(24 * time.Hour)
		if d.Before(cutoff) {
			continue
		}
		if b.Close <= 0 {
			continue
		}
		dates = append(dates, d)
		values = append(values, b.Close)
	}
	type pair struct {
		d time.Time
		v float64
	}
	pairs := make([]pair, len(dates))
	for i := range dates {
		pairs[i] = pair{dates[i], values[i]}
	}
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].d.Before(pairs[j].d) })
	for i, p := range pairs {
		dates[i] = p.d
		values[i] = p.v
	}
	return dates, values
}

// newHoldingsSeriesServiceAdapter returns the same struct that
// powers the benchmark endpoint — both surfaces share a fetcher
// and the membership repos. We expose them through two named
// constructors so main.go reads as "wire benchmark" + "wire
// holdings series" rather than "wire some bag of read services".
func newHoldingsSeriesServiceAdapter(svc *Services) api.HoldingsSeriesService {
	adapter, ok := newBenchmarkServiceAdapter(svc).(*benchmarkServiceAdapter)
	if !ok || adapter == nil {
		return nil
	}
	return adapter
}

// Helpful sanity check during local debugging — never wired to a
// real handler. Left here so future readers can run it from a unit
// test if they want to spot-check shape.
var _ = func() error {
	return fmt.Errorf("unused")
}
