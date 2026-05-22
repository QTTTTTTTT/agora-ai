package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

const (
	defaultSinaNewsBaseURL = "https://feed.mix.sina.com.cn"
	// lid=2516 -> 财经-A股, the most relevant rolling feed for A-share news.
	defaultSinaNewsLid = "2516"
)

// sinaNewsProvider returns a news provider that hits Sina's rolling finance
// feed. It is keyless, returns Chinese-language headlines, and serves as a
// backup when Eastmoney is unreachable or returns nothing for the keyword.
//
// Sina's roll feed is *market-wide*, not symbol-specific, so we apply a soft
// keyword filter (stock code, normalized symbol, or free-text symbol) and fall
// back to the top-N rolling items when no headlines match. This still gives
// the analyst useful "what is happening in A-share right now" context, which
// is far more relevant than US-locale Google News results.
//
// Endpoint:
//
//	GET https://feed.mix.sina.com.cn/api/roll/get?pageid=153&lid=2516&num=20&page=1
func (s *Service) sinaNewsProvider() newsProviderFunc {
	baseURL := firstNonEmpty(s.cfg.SinaNewsBaseURL, defaultSinaNewsBaseURL)
	return func(ctx context.Context, instrument InstrumentRef, limit int) ([]NewsItem, error) {
		if normalizeMarket(instrument.Market, instrument.AssetClass) != "cnstock" {
			return nil, fmt.Errorf("sina: unsupported market %q", instrument.Market)
		}
		if limit <= 0 {
			limit = 10
		}
		// Pull more than the limit so we have headroom for soft filtering.
		fetchSize := limit * 3
		if fetchSize < 20 {
			fetchSize = 20
		}
		endpoint, err := url.Parse(strings.TrimRight(baseURL, "/") + "/api/roll/get")
		if err != nil {
			return nil, fmt.Errorf("sina: parse url: %w", err)
		}
		q := endpoint.Query()
		q.Set("pageid", "153")
		q.Set("lid", defaultSinaNewsLid)
		q.Set("num", strconv.Itoa(fetchSize))
		q.Set("page", "1")
		endpoint.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("sina: build request: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Referer", "https://finance.sina.com.cn/")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")

		resp, err := s.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("sina: http: %w", err)
		}
		defer resp.Body.Close()
		if isThrottleStatus(resp.StatusCode) {
			return nil, fmt.Errorf("%w: sina: http %d", ErrUpstreamThrottled, resp.StatusCode)
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("sina: http %d", resp.StatusCode)
		}
		// Sina's roll feed sometimes serves GBK-encoded JSON; detect via
		// content-type and fall back to GBK decoding when needed.
		body, err := readSinaBody(resp)
		if err != nil {
			return nil, fmt.Errorf("sina: read body: %w", err)
		}
		var payload struct {
			Result struct {
				Status struct {
					Code int    `json:"code"`
					Msg  string `json:"msg"`
				} `json:"status"`
				Data []map[string]any `json:"data"`
			} `json:"result"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("sina: decode: %w", err)
		}
		if payload.Result.Status.Code != 0 && payload.Result.Status.Msg != "" {
			return nil, fmt.Errorf("sina: api error: %s", payload.Result.Status.Msg)
		}
		if len(payload.Result.Data) == 0 {
			return nil, fmt.Errorf("sina: empty feed")
		}

		keywords := sinaSoftKeywords(instrument)
		all := make([]NewsItem, 0, len(payload.Result.Data))
		for _, raw := range payload.Result.Data {
			title := strings.TrimSpace(stringValue(raw, "title"))
			if title == "" {
				continue
			}
			item := NewsItem{
				Title:   title,
				Summary: strings.TrimSpace(stringValue(raw, "intro")),
				URL:     firstNonEmpty(stringValue(raw, "url"), stringValue(raw, "wapurl"), stringValue(raw, "link")),
				Source:  "sina",
				Symbols: []string{instrument.NormalizedSymbol()},
			}
			if ts := strings.TrimSpace(stringValue(raw, "ctime")); ts != "" {
				if epoch, err := strconv.ParseInt(ts, 10, 64); err == nil && epoch > 0 {
					item.PublishedAt = time.Unix(epoch, 0).UTC()
				} else if t, err := time.ParseInLocation("2006-01-02 15:04:05", ts, shanghaiLocation()); err == nil {
					item.PublishedAt = t.UTC()
				}
			}
			all = append(all, item)
		}
		if len(all) == 0 {
			return nil, fmt.Errorf("sina: no parseable items")
		}

		// Soft filter: prefer items whose title/intro mentions one of the
		// candidate keywords (stock code, raw symbol). If none match, fall
		// back to the unfiltered top-N as market context.
		filtered := make([]NewsItem, 0, limit)
		if len(keywords) > 0 {
			for _, item := range all {
				if matchesAnyKeyword(item, keywords) {
					filtered = append(filtered, item)
					if len(filtered) >= limit {
						break
					}
				}
			}
		}
		if len(filtered) == 0 {
			if len(all) > limit {
				all = all[:limit]
			}
			return tagNewsItemsWithLanguage(withNewsSource(all, "sina"), NewsLanguageZH), nil
		}
		return tagNewsItemsWithLanguage(withNewsSource(filtered, "sina"), NewsLanguageZH), nil
	}
}

// sinaSoftKeywords builds the candidate strings used to soft-filter the Sina
// rolling feed. For ticker-shaped symbols we use the bare 6-digit code; for
// free-text symbols (e.g. company name "贵州茅台") we use the raw input.
func sinaSoftKeywords(instrument InstrumentRef) []string {
	raw := strings.TrimSpace(instrument.Symbol)
	if raw == "" {
		return nil
	}
	keywords := make([]string, 0, 3)
	if !IsTickerLikeSymbol(raw) {
		keywords = append(keywords, raw)
		return dedupeStrings(keywords)
	}
	canonical := CanonicalSymbol(instrument)
	for _, prefix := range []string{"SH", "SZ", "BJ"} {
		if strings.HasPrefix(canonical, prefix) {
			digits := strings.TrimPrefix(canonical, prefix)
			if isAllDigits(digits) {
				keywords = append(keywords, digits)
			}
		}
	}
	upper := strings.ToUpper(raw)
	if idx := strings.IndexByte(upper, '.'); idx > 0 {
		upper = upper[:idx]
	}
	if isAllDigits(upper) {
		keywords = append(keywords, upper)
	}
	return dedupeStrings(keywords)
}

func matchesAnyKeyword(item NewsItem, keywords []string) bool {
	haystack := strings.ToLower(item.Title + " " + item.Summary)
	for _, kw := range keywords {
		needle := strings.ToLower(strings.TrimSpace(kw))
		if needle == "" {
			continue
		}
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}

// readSinaBody reads the response body, auto-detecting GBK encoding for Sina's
// occasionally Chinese-encoded responses. It defaults to UTF-8.
func readSinaBody(resp *http.Response) ([]byte, error) {
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "gbk") || strings.Contains(contentType, "gb2312") {
		decoded, err := io.ReadAll(transform.NewReader(resp.Body, simplifiedchinese.GBK.NewDecoder()))
		if err != nil {
			return nil, err
		}
		return decoded, nil
	}
	return io.ReadAll(resp.Body)
}
