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
)

const defaultEastmoneyNewsBaseURL = "https://search-api-web.eastmoney.com"

// eastmoneyNewsProvider returns a news provider that hits Eastmoney's CMS
// article search API. The endpoint is JSONP-wrapped JSON and is the canonical
// data source A-share retail investors use for stock-specific news. It is
// keyless and returns Chinese-language results that are far more relevant than
// the Google News RSS for A-share instruments.
//
// Endpoint:
//
//	GET https://search-api-web.eastmoney.com/search/jsonp?cb=jQuery1&param=<json>&_=<ms>
//
// Response (after stripping the jQuery1(...) wrapper):
//
//	{
//	  "result": {
//	    "cmsArticleWebOld": {
//	      "data": [
//	        {"title": "...", "content": "...", "url": "...", "date": "2025-05-18 09:30:01"},
//	        ...
//	      ]
//	    }
//	  }
//	}
func (s *Service) eastmoneyNewsProvider() newsProviderFunc {
	baseURL := firstNonEmpty(s.cfg.EastmoneyNewsBaseURL, defaultEastmoneyNewsBaseURL)
	return func(ctx context.Context, instrument InstrumentRef, limit int) ([]NewsItem, error) {
		if normalizeMarket(instrument.Market, instrument.AssetClass) != "cnstock" {
			return nil, fmt.Errorf("eastmoney: unsupported market %q", instrument.Market)
		}
		keyword := eastmoneyKeyword(instrument)
		if keyword == "" {
			return nil, fmt.Errorf("eastmoney: empty keyword")
		}
		if limit <= 0 {
			limit = 10
		}
		paramJSON, err := json.Marshal(map[string]any{
			"uid":           "",
			"keyword":       keyword,
			"type":          []string{"cmsArticleWebOld"},
			"client":        "web",
			"clientVersion": "curr",
			"param": map[string]any{
				"cmsArticleWebOld": map[string]any{
					"searchScope": "default",
					"sort":        "time",
					"pageIndex":   1,
					"pageSize":    limit,
					"preTag":      "",
					"postTag":     "",
				},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("eastmoney: marshal param: %w", err)
		}
		endpoint, err := url.Parse(strings.TrimRight(baseURL, "/") + "/search/jsonp")
		if err != nil {
			return nil, fmt.Errorf("eastmoney: parse url: %w", err)
		}
		q := endpoint.Query()
		q.Set("cb", "jQuery1")
		q.Set("param", string(paramJSON))
		q.Set("_", strconv.FormatInt(time.Now().UnixMilli(), 10))
		endpoint.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("eastmoney: build request: %w", err)
		}
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Referer", "https://so.eastmoney.com/")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")

		resp, err := s.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("eastmoney: http: %w", err)
		}
		defer resp.Body.Close()
		if isThrottleStatus(resp.StatusCode) {
			return nil, fmt.Errorf("%w: eastmoney: http %d", ErrUpstreamThrottled, resp.StatusCode)
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("eastmoney: http %d", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("eastmoney: read body: %w", err)
		}
		rawJSON, err := stripJSONPWrapper(body)
		if err != nil {
			return nil, fmt.Errorf("eastmoney: %w", err)
		}
		articles, err := parseEastmoneyArticles(rawJSON)
		if err != nil {
			return nil, fmt.Errorf("eastmoney: %w", err)
		}
		if len(articles) == 0 {
			return nil, fmt.Errorf("eastmoney: empty results for %q", keyword)
		}
		items := make([]NewsItem, 0, len(articles))
		for _, art := range articles {
			title := stripHTMLTags(strings.TrimSpace(stringValue(art, "title")))
			if title == "" {
				continue
			}
			item := NewsItem{
				Title:   title,
				Summary: stripHTMLTags(strings.TrimSpace(stringValue(art, "content"))),
				URL:     firstNonEmpty(stringValue(art, "url"), stringValue(art, "url_w"), stringValue(art, "link")),
				Source:  "eastmoney",
				Symbols: []string{instrument.NormalizedSymbol()},
			}
			if ts := strings.TrimSpace(stringValue(art, "date")); ts != "" {
				if t, err := time.ParseInLocation("2006-01-02 15:04:05", ts, shanghaiLocation()); err == nil {
					item.PublishedAt = t.UTC()
				} else if t, err := time.ParseInLocation("2006-01-02", ts, shanghaiLocation()); err == nil {
					item.PublishedAt = t.UTC()
				}
			}
			items = append(items, item)
			if len(items) >= limit {
				break
			}
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("eastmoney: no parseable items")
		}
		return tagNewsItemsWithLanguage(items, NewsLanguageZH), nil
	}
}

// parseEastmoneyArticles tolerates two real-world response shapes from
// Eastmoney's CMS search:
//
//   - `result.cmsArticleWebOld` as a flat JSON array (the live API today).
//   - `result.cmsArticleWebOld.data` as an array (older / cached docs).
//
// Returning a slice of generic maps keeps the per-field parsing in the
// caller and matches stringValue's contract.
func parseEastmoneyArticles(rawJSON []byte) ([]map[string]any, error) {
	var envelope struct {
		Result map[string]json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(rawJSON, &envelope); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	raw, ok := envelope.Result["cmsArticleWebOld"]
	if !ok || len(raw) == 0 {
		return nil, nil
	}
	// Try the flat-array shape first since that is what the live API returns.
	var asArray []map[string]any
	if err := json.Unmarshal(raw, &asArray); err == nil {
		return asArray, nil
	}
	// Fall back to {data: [...]}.
	var asObject struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &asObject); err == nil {
		return asObject.Data, nil
	}
	return nil, fmt.Errorf("unknown cmsArticleWebOld shape")
}

// stripHTMLTags removes the simple highlight tags Eastmoney embeds in
// title/content (`<em>...</em>`) plus any other plain HTML tag so the
// rendered headlines are clean. Intentionally minimal - no entity
// decoding - so we don't accidentally mangle CJK punctuation.
func stripHTMLTags(s string) string {
	if s == "" || !strings.ContainsRune(s, '<') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// eastmoneyKeyword picks the best search keyword for the Eastmoney CMS search.
// It prefers the 6-digit stock code when the symbol is ticker-shaped, and
// falls back to free-text otherwise (so users can search by company name).
func eastmoneyKeyword(instrument InstrumentRef) string {
	raw := strings.TrimSpace(instrument.Symbol)
	if raw == "" {
		return ""
	}
	if !IsTickerLikeSymbol(raw) {
		return raw
	}
	canonical := CanonicalSymbol(instrument)
	for _, prefix := range []string{"SH", "SZ", "BJ"} {
		if strings.HasPrefix(canonical, prefix) {
			digits := strings.TrimPrefix(canonical, prefix)
			if isAllDigits(digits) {
				return digits
			}
		}
	}
	upper := strings.ToUpper(raw)
	if idx := strings.IndexByte(upper, '.'); idx > 0 {
		upper = upper[:idx]
	}
	if isAllDigits(upper) {
		return upper
	}
	return raw
}

// stripJSONPWrapper extracts the JSON payload from a JSONP response of the
// form `callback(...)`. It tolerates leading/trailing whitespace and trailing
// semicolons.
func stripJSONPWrapper(body []byte) ([]byte, error) {
	text := strings.TrimSpace(string(body))
	text = strings.TrimRight(text, ";")
	start := strings.IndexByte(text, '(')
	end := strings.LastIndexByte(text, ')')
	if start < 0 || end <= start {
		return nil, fmt.Errorf("malformed jsonp response")
	}
	inner := strings.TrimSpace(text[start+1 : end])
	if inner == "" {
		return nil, fmt.Errorf("empty jsonp body")
	}
	return []byte(inner), nil
}
