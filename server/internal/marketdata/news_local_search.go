package marketdata

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type rotatingKeySelector struct {
	mu     sync.Mutex
	keys   []string
	next   int
	errors map[string]int
}

func newRotatingKeySelector(keys []string) *rotatingKeySelector {
	normalized := normalizeProviderSecrets(keys)
	return &rotatingKeySelector{
		keys:   normalized,
		errors: make(map[string]int, len(normalized)),
	}
}

func (s *rotatingKeySelector) nextKey() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.keys) == 0 {
		return ""
	}
	for i := 0; i < len(s.keys); i++ {
		idx := (s.next + i) % len(s.keys)
		key := s.keys[idx]
		if s.errors[key] < 3 {
			s.next = (idx + 1) % len(s.keys)
			return key
		}
	}
	for _, key := range s.keys {
		s.errors[key] = 0
	}
	key := s.keys[s.next%len(s.keys)]
	s.next = (s.next + 1) % len(s.keys)
	return key
}

func (s *rotatingKeySelector) recordSuccess(key string) {
	if s == nil || strings.TrimSpace(key) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errors[key] > 0 {
		s.errors[key]--
	}
}

func (s *rotatingKeySelector) recordError(key string) {
	if s == nil || strings.TrimSpace(key) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors[key]++
}

func (s *Service) localSearchNewsProvider() newsProviderFunc {
	serpKeys := newRotatingKeySelector(s.cfg.SerpAPIKeys)
	tavilyKeys := newRotatingKeySelector(s.cfg.TavilyAPIKeys)
	serpBaseURL := firstNonEmpty(s.cfg.SerpAPIBaseURL, "https://serpapi.com")
	tavilyBaseURL := firstNonEmpty(s.cfg.TavilyBaseURL, "https://api.tavily.com")

	searchers := []struct {
		name  string
		fetch func(context.Context, InstrumentRef, int) ([]NewsItem, error)
	}{}
	if s.cfg.newsProviderEnabled("tavily") {
		searchers = append(searchers, struct {
			name  string
			fetch func(context.Context, InstrumentRef, int) ([]NewsItem, error)
		}{
			name: "tavily",
			fetch: func(ctx context.Context, instrument InstrumentRef, limit int) ([]NewsItem, error) {
				return s.fetchLocalTavilyNews(ctx, tavilyBaseURL, tavilyKeys, instrument, limit)
			},
		})
	}
	if s.cfg.newsProviderEnabled("serpapi") {
		searchers = append(searchers, struct {
			name  string
			fetch func(context.Context, InstrumentRef, int) ([]NewsItem, error)
		}{
			name: "serpapi",
			fetch: func(ctx context.Context, instrument InstrumentRef, limit int) ([]NewsItem, error) {
				return s.fetchLocalSerpAPINews(ctx, serpBaseURL, serpKeys, instrument, limit)
			},
		})
	}

	return func(ctx context.Context, instrument InstrumentRef, limit int) ([]NewsItem, error) {
		queries := buildLocalSearchQueries(instrument)
		aggregated := make([]NewsItem, 0, limit)
		seen := make(map[string]struct{}, limit)
		var errs []string

		for _, queryText := range queries {
			for _, searcher := range searchers {
				items, err := searcher.fetch(ctx, instrumentWithQuery(instrument, queryText), limit)
				if err != nil {
					errs = append(errs, fmt.Sprintf("%s query %q: %v", searcher.name, queryText, err))
					continue
				}
				for _, item := range items {
					key := localNewsDedupKey(item)
					if _, ok := seen[key]; ok {
						continue
					}
					seen[key] = struct{}{}
					aggregated = append(aggregated, item)
					if len(aggregated) >= limit {
						sortLocalNewsItems(aggregated)
						return tagNewsItemsWithLanguage(withNewsSource(aggregated[:limit], "local-search"), ""), nil
					}
				}
			}
			if len(aggregated) > 0 {
				sortLocalNewsItems(aggregated)
				if len(aggregated) > limit {
					aggregated = aggregated[:limit]
				}
				return tagNewsItemsWithLanguage(withNewsSource(aggregated, "local-search"), ""), nil
			}
		}
		if len(aggregated) > 0 {
			sortLocalNewsItems(aggregated)
			if len(aggregated) > limit {
				aggregated = aggregated[:limit]
			}
			return tagNewsItemsWithLanguage(withNewsSource(aggregated, "local-search"), ""), nil
		}
		if len(errs) == 0 {
			return nil, fmt.Errorf("local-search: %w", ErrNewsUnavailable)
		}
		return nil, fmt.Errorf("local-search: %w: %s", ErrNewsUnavailable, strings.Join(dedupeStrings(errs), "; "))
	}
}

func (s *Service) fetchLocalSerpAPINews(ctx context.Context, baseURL string, selector *rotatingKeySelector, instrument InstrumentRef, limit int) ([]NewsItem, error) {
	queryText := instrumentQuery(instrument)
	var errs []string
	attempts := len(selector.keys)
	if attempts == 0 {
		return nil, fmt.Errorf("serpapi: no api keys configured")
	}
	for i := 0; i < attempts; i++ {
		apiKey := selector.nextKey()
		if apiKey == "" {
			break
		}
		endpoint, err := buildEndpoint(baseURL, "/search.json", url.Values{
			"engine":  []string{"google_news"},
			"q":       []string{queryText},
			"num":     []string{strconv.Itoa(limit)},
			"api_key": []string{apiKey},
		})
		if err != nil {
			selector.recordError(apiKey)
			errs = append(errs, err.Error())
			continue
		}
		body, err := s.fetchJSON(ctx, endpoint)
		if err != nil {
			selector.recordError(apiKey)
			errs = append(errs, err.Error())
			continue
		}
		items := parseNewsItems(body, instrument, limit)
		if len(items) == 0 {
			selector.recordError(apiKey)
			errs = append(errs, "serpapi: empty news results")
			continue
		}
		selector.recordSuccess(apiKey)
		return tagNewsItemsWithLanguage(withNewsSource(items, "serpapi"), ""), nil
	}
	return nil, fmt.Errorf("serpapi: %w: %s", ErrNewsUnavailable, strings.Join(dedupeStrings(errs), "; "))
}

func (s *Service) fetchLocalTavilyNews(ctx context.Context, baseURL string, selector *rotatingKeySelector, instrument InstrumentRef, limit int) ([]NewsItem, error) {
	queryText := instrumentQuery(instrument)
	attempts := len(selector.keys)
	if attempts == 0 {
		return nil, fmt.Errorf("tavily: no api keys configured")
	}
	var errs []string
	for i := 0; i < attempts; i++ {
		apiKey := selector.nextKey()
		if apiKey == "" {
			break
		}
		payload := map[string]any{
			"api_key":        apiKey,
			"query":          queryText,
			"topic":          "news",
			"search_depth":   "advanced",
			"max_results":    limit,
			"include_answer": false,
		}
		endpoint, err := buildEndpoint(baseURL, "/search", nil)
		if err != nil {
			selector.recordError(apiKey)
			errs = append(errs, err.Error())
			continue
		}
		body, err := s.postJSON(ctx, endpoint, payload, nil)
		if err != nil {
			selector.recordError(apiKey)
			errs = append(errs, err.Error())
			continue
		}
		items := parseNewsItems(body, instrument, limit)
		if len(items) == 0 {
			selector.recordError(apiKey)
			errs = append(errs, "tavily: empty news results")
			continue
		}
		selector.recordSuccess(apiKey)
		return tagNewsItemsWithLanguage(withNewsSource(items, "tavily"), ""), nil
	}
	return nil, fmt.Errorf("tavily: %w: %s", ErrNewsUnavailable, strings.Join(dedupeStrings(errs), "; "))
}

func buildLocalSearchQueries(instrument InstrumentRef) []string {
	queryText := strings.TrimSpace(searchQueryForNews(instrument))
	queries := []string{queryText}
	rawSymbol := strings.TrimSpace(instrument.Symbol)
	market := normalizeMarket(instrument.Market, instrument.AssetClass)
	if rawSymbol != "" && !IsTickerLikeSymbol(rawSymbol) {
		switch market {
		case "cnstock":
			queries = append(queries,
				queryText+" 财经 新闻",
				queryText+" 行业 新闻",
			)
		case "usstock":
			queries = append(queries,
				queryText+" stock market news",
				queryText+" sector news",
			)
		case "crypto":
			queries = append(queries,
				queryText+" crypto market news",
			)
		case "futures":
			queries = append(queries,
				queryText+" futures market news",
			)
		default:
			queries = append(queries, queryText+" market news")
		}
		return dedupeStrings(queries)
	}
	symbol := instrument.NormalizedSymbol()
	if symbol != "" {
		switch market {
		case "cnstock":
			queries = append(queries,
				fmt.Sprintf("%s 最新 财经 新闻", symbol),
				fmt.Sprintf("%s A股 公司 公告 新闻", symbol),
			)
		case "usstock":
			queries = append(queries,
				fmt.Sprintf("%s stock news latest", symbol),
				fmt.Sprintf("%s earnings guidance analyst news", symbol),
			)
		case "crypto":
			queries = append(queries,
				fmt.Sprintf("%s crypto news latest", symbol),
				fmt.Sprintf("%s token market news", symbol),
			)
		case "futures":
			queries = append(queries,
				fmt.Sprintf("%s futures market news", symbol),
				fmt.Sprintf("%s commodity futures analysis", symbol),
			)
		default:
			queries = append(queries, fmt.Sprintf("%s financial news", symbol))
		}
	}
	return dedupeStrings(queries)
}

func instrumentWithQuery(instrument InstrumentRef, query string) InstrumentRef {
	clone := instrument
	clone.InstrumentKey = ""
	clone.Exchange = ""
	clone.Market = ""
	clone.AssetClass = ""
	if strings.TrimSpace(query) != "" {
		clone.Symbol = query
	}
	return clone
}

func instrumentQuery(instrument InstrumentRef) string {
	if symbol := strings.TrimSpace(instrument.Symbol); symbol != "" {
		return symbol
	}
	return searchQueryForNews(instrument)
}

func localNewsDedupKey(item NewsItem) string {
	if strings.TrimSpace(item.URL) != "" {
		return strings.ToLower(strings.TrimSpace(item.URL))
	}
	return strings.ToLower(strings.TrimSpace(item.Title))
}

func sortLocalNewsItems(items []NewsItem) {
	sort.SliceStable(items, func(i, j int) bool {
		left := items[i].PublishedAt
		right := items[j].PublishedAt
		if left.Equal(right) {
			return items[i].Title < items[j].Title
		}
		if left.IsZero() {
			return false
		}
		if right.IsZero() {
			return true
		}
		return left.After(right)
	})
}
