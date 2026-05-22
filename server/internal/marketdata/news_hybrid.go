package marketdata

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// hybridNewsResult is the per-chain output captured by the hybrid aggregator.
// We surface the chain name and per-chain provider notes so the calling layer
// can attribute partial failures back to a specific language pipeline.
type hybridNewsResult struct {
	chain string
	items []NewsItem
	notes []string
	err   error
}

// fetchHybridNews runs the ZH-leaning and EN-leaning provider chains in
// parallel and merges their outputs. The "primary" chain (determined by the
// instrument's market) is preserved at the front of the result so users see
// the most market-relevant headlines first; the "secondary" chain fills the
// remainder up to limit. Items are deduped by URL (fallback: normalized
// title) so re-publications and translations of the same article don't crowd
// the list.
//
// When only one chain has any enabled providers, this collapses to a single
// chain fetch with no overhead.
func (s *Service) fetchHybridNews(ctx context.Context, instrument InstrumentRef, limit int) ([]NewsItem, []string, error) {
	if limit <= 0 {
		limit = 5
	}
	primaryLang, secondaryLang := primarySecondaryNewsLanguage(instrument)
	zhChain := s.languageNewsChain(instrument, NewsLanguageZH)
	enChain := s.languageNewsChain(instrument, NewsLanguageEN)
	chains := map[string][]newsProviderFunc{
		NewsLanguageZH: zhChain,
		NewsLanguageEN: enChain,
	}

	if len(chains[primaryLang]) == 0 && len(chains[secondaryLang]) == 0 {
		return nil, nil, ErrNewsUnavailable
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = make(map[string]hybridNewsResult, 2)
	)
	for lang, chain := range chains {
		if len(chain) == 0 {
			continue
		}
		wg.Add(1)
		go func(lang string, chain []newsProviderFunc) {
			defer wg.Done()
			items, notes, err := s.runNewsChain(ctx, chain, instrument, limit)
			mu.Lock()
			results[lang] = hybridNewsResult{chain: lang, items: items, notes: notes, err: err}
			mu.Unlock()
		}(lang, chain)
	}
	wg.Wait()

	primary := results[primaryLang]
	secondary := results[secondaryLang]
	merged := mergeHybridNews(primary.items, secondary.items, limit)
	notes := append([]string{}, primary.notes...)
	notes = append(notes, secondary.notes...)
	notes = dedupeStrings(notes)

	if len(merged) == 0 {
		if primary.err != nil && secondary.err != nil {
			return nil, notes, fmt.Errorf("%w: %s; %s", ErrNewsUnavailable, primary.err.Error(), secondary.err.Error())
		}
		if primary.err != nil {
			return nil, notes, primary.err
		}
		if secondary.err != nil {
			return nil, notes, secondary.err
		}
		return nil, notes, ErrNewsUnavailable
	}
	return merged, notes, nil
}

// languageNewsChain builds the prioritised list of news providers for the
// requested article language. The chain is constructed *per call* so it can
// honour the live config (some providers may be disabled), and the locale of
// the search-MCP variant is forced via fetchSearchNewsWithLocale so a Chinese
// chain always reads Chinese results even when the parent context says EN.
func (s *Service) languageNewsChain(instrument InstrumentRef, lang string) []newsProviderFunc {
	var providers []newsProviderFunc
	switch lang {
	case NewsLanguageZH:
		if s.cfg.newsProviderEnabled("eastmoney") && normalizeMarket(instrument.Market, instrument.AssetClass) == "cnstock" {
			providers = append(providers, s.eastmoneyNewsProvider())
		}
		if s.cfg.newsProviderEnabled("sina") && normalizeMarket(instrument.Market, instrument.AssetClass) == "cnstock" {
			providers = append(providers, s.sinaNewsProvider())
		}
		if s.cfg.newsProviderEnabled("web-search") {
			providers = append(providers, s.localeSearchNewsProvider("web-search", s.cfg.WebSearchURL, zhWebSearchLocale()))
		}
	case NewsLanguageEN:
		if s.cfg.newsProviderEnabled("web-search") {
			providers = append(providers, s.localeSearchNewsProvider("web-search", s.cfg.WebSearchURL, enWebSearchLocale()))
		}
		if s.cfg.newsProviderEnabled("serpapi") {
			providers = append(providers, s.fetchSerpAPINews)
		}
		if s.cfg.newsProviderEnabled("tavily") {
			providers = append(providers, s.fetchTavilyNews)
		}
	}
	return providers
}

// localeSearchNewsProvider wraps fetchSearchNewsWithLocale into a newsProviderFunc
// that always forces the given locale. It is used by the hybrid aggregator to
// pull each language's coverage from the same MCP backend in parallel.
func (s *Service) localeSearchNewsProvider(source, baseURL string, locale webSearchLocale) newsProviderFunc {
	return func(ctx context.Context, instrument InstrumentRef, limit int) ([]NewsItem, error) {
		return s.fetchSearchNewsWithLocale(ctx, baseURL, source, instrument, limit, locale)
	}
}

func zhWebSearchLocale() webSearchLocale {
	return webSearchLocale{hl: "zh-CN", gl: "CN", ceid: "CN:zh-Hans"}
}

func enWebSearchLocale() webSearchLocale {
	return webSearchLocale{hl: "en-US", gl: "US", ceid: "US:en"}
}

// runNewsChain runs a sequence of providers in order, stopping at the first
// non-empty success. It also gathers human-readable notes for partial
// failures so the caller can attach them to the response metadata.
//
// Unlike the quote path, news providers are not behind the circuit breaker
// today: the chain is already bounded by NewsTTL (results are cached for
// minutes, not per request) so an outage of a single provider only costs
// one extra HTTP call per cache window — well below the threshold where
// breaking is worth the operational complexity.
func (s *Service) runNewsChain(ctx context.Context, providers []newsProviderFunc, instrument InstrumentRef, limit int) ([]NewsItem, []string, error) {
	if len(providers) == 0 {
		return nil, nil, ErrNewsUnavailable
	}
	var notes []string
	var lastErr error
	for _, provider := range providers {
		providerCtx, cancel := s.providerContext(ctx)
		items, err := provider(providerCtx, instrument, limit)
		cancel()
		if err != nil {
			notes = append(notes, userVisibleResearchNote(err, "news temporarily unavailable"))
			lastErr = err
			continue
		}
		if len(items) == 0 {
			notes = append(notes, "news temporarily unavailable")
			continue
		}
		return items, notes, nil
	}
	if lastErr == nil {
		lastErr = ErrNewsUnavailable
	}
	return nil, notes, lastErr
}

// primarySecondaryNewsLanguage returns the (primary, secondary) news language
// pair for an instrument. Cn/Hk stocks lead with Chinese coverage and treat
// English as supplementary; everything else does the inverse. For futures
// and crypto we lean EN because most of the global commentary is written
// in English, with ZH as supplementary local color.
func primarySecondaryNewsLanguage(instrument InstrumentRef) (string, string) {
	switch normalizeMarket(instrument.Market, instrument.AssetClass) {
	case "cnstock", "hkstock":
		return NewsLanguageZH, NewsLanguageEN
	default:
		return NewsLanguageEN, NewsLanguageZH
	}
}

// mergeHybridNews interleaves primary and secondary so the user sees the most
// market-relevant coverage *and* at least some cross-language context even
// when the primary chain is rich enough to fill every slot on its own.
//
// Policy: roughly one third of `limit` is reserved for the secondary chain
// (with a floor of one slot when secondary has anything to contribute). The
// remaining slots go to primary. When either chain falls short, the other
// chain tops up the remaining capacity so we always return up to `limit`
// items. Dedupe is by canonical URL (host+path, scheme/query-stripped);
// items without a URL fall back to a lowercased title match.
//
// The merged result is stable-sorted by PublishedAt descending so the most
// recent headlines surface first regardless of which chain produced them.
func mergeHybridNews(primary, secondary []NewsItem, limit int) []NewsItem {
	if limit <= 0 {
		limit = len(primary) + len(secondary)
	}
	secondaryQuota := limit / 3
	if secondaryQuota == 0 && len(secondary) > 0 {
		secondaryQuota = 1
	}
	if secondaryQuota > len(secondary) {
		secondaryQuota = len(secondary)
	}
	primaryQuota := limit - secondaryQuota
	if primaryQuota > len(primary) {
		primaryQuota = len(primary)
	}

	merged := make([]NewsItem, 0, limit)
	seen := make(map[string]struct{}, limit)
	addUnique := func(item NewsItem) bool {
		key := canonicalNewsKey(item)
		if key == "" {
			return false
		}
		if _, dup := seen[key]; dup {
			return false
		}
		seen[key] = struct{}{}
		merged = append(merged, item)
		return true
	}

	primaryTaken := 0
	for _, item := range primary {
		if primaryTaken >= primaryQuota {
			break
		}
		if addUnique(item) {
			primaryTaken++
		}
	}
	secondaryTaken := 0
	for _, item := range secondary {
		if secondaryTaken >= secondaryQuota {
			break
		}
		if addUnique(item) {
			secondaryTaken++
		}
	}
	// Top up any leftover capacity (caused by dedupe collisions or one chain
	// being shorter than its quota) with remaining items from either chain,
	// primary first.
	if len(merged) < limit {
		for _, item := range primary {
			if len(merged) >= limit {
				break
			}
			addUnique(item)
		}
	}
	if len(merged) < limit {
		for _, item := range secondary {
			if len(merged) >= limit {
				break
			}
			addUnique(item)
		}
	}

	sort.SliceStable(merged, func(i, j int) bool {
		left, right := merged[i].PublishedAt, merged[j].PublishedAt
		if left.IsZero() && right.IsZero() {
			return false
		}
		if left.IsZero() {
			return false
		}
		if right.IsZero() {
			return true
		}
		return left.After(right)
	})
	return merged
}

// canonicalNewsKey reduces a NewsItem to a comparable key for dedupe. It
// strips scheme/query and lower-cases host+path so http vs https and
// utm tags don't cause near-duplicate articles to slip past.
func canonicalNewsKey(item NewsItem) string {
	if raw := strings.TrimSpace(item.URL); raw != "" {
		if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
			host := strings.ToLower(strings.TrimPrefix(parsed.Host, "www."))
			path := strings.TrimRight(parsed.Path, "/")
			return host + path
		}
		return strings.ToLower(raw)
	}
	if title := strings.TrimSpace(item.Title); title != "" {
		return strings.ToLower(title)
	}
	return ""
}
