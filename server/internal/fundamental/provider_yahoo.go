package fundamental

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// YahooProvider talks to Yahoo Finance's quoteSummary endpoint:
//
//	https://query1.finance.yahoo.com/v10/finance/quoteSummary/AAPL?modules=defaultKeyStatistics,financialData,summaryDetail,price
//
// Modules requested:
//   - defaultKeyStatistics: trailingPE, priceToBook, beta, marketCap
//   - financialData:        profitMargins, operatingMargins, returnOnEquity,
//                           revenueGrowth, earningsGrowth, debtToEquity, currency
//   - summaryDetail:        forwardPE, dividendYield
//   - price:                regularMarketTime (AsOf)
//
// Yahoo exposes most of these key-less under the public quoteSummary
// route; private clouds with a Yahoo Finance Pro key can override
// BaseURL to point at an authenticated proxy.
type YahooProvider struct {
	HTTPClient *http.Client
	// BaseURL lets tests redirect to an httptest server. Empty
	// falls back to https://query1.finance.yahoo.com.
	BaseURL string
	// Markets defaults to {"us_equity", "hk_equity"} when empty.
	Markets []string
}

// Name implements Provider.
func (p *YahooProvider) Name() string { return "yahoo" }

// Supports implements Provider.
func (p *YahooProvider) Supports(market string) bool {
	markets := p.Markets
	if len(markets) == 0 {
		markets = []string{"us_equity", "hk_equity"}
	}
	for _, m := range markets {
		if strings.EqualFold(m, market) {
			return true
		}
	}
	return false
}

// Fetch implements Provider. Returns ErrNoData when Yahoo replies
// "no module" or 404; other 4xx/5xx errors bubble up unchanged.
func (p *YahooProvider) Fetch(ctx context.Context, req FetchRequest) (*Metrics, error) {
	req = req.Normalize()
	if req.Symbol == "" {
		return nil, ErrNoData
	}
	endpoint, err := p.endpoint(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	// Yahoo 403s the default Go UA; mimic a desktop browser.
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	httpReq.Header.Set("Accept", "application/json")

	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("yahoo fundamental: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoData
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("yahoo fundamental: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("yahoo fundamental: read: %w", err)
	}
	return parseYahooQuoteSummary(body, req.Symbol)
}

func (p *YahooProvider) endpoint(req FetchRequest) (string, error) {
	base := p.BaseURL
	if strings.TrimSpace(base) == "" {
		base = "https://query1.finance.yahoo.com"
	}
	base = strings.TrimRight(base, "/")
	u, err := url.Parse(base + "/v10/finance/quoteSummary/" + url.PathEscape(req.Symbol))
	if err != nil {
		return "", fmt.Errorf("yahoo fundamental: build url: %w", err)
	}
	q := u.Query()
	q.Set("modules", "defaultKeyStatistics,financialData,summaryDetail,price")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// parseYahooQuoteSummary navigates Yahoo's two-format response:
// numeric fields can be either a bare number ("trailingPE": 28.5)
// or wrapped in {"raw": 28.5, "fmt": "28.50"} (their typical shape).
// rawFloat handles both.
func parseYahooQuoteSummary(body []byte, symbol string) (*Metrics, error) {
	var dto struct {
		QuoteSummary struct {
			Result []map[string]map[string]any `json:"result"`
			Error  any                         `json:"error"`
		} `json:"quoteSummary"`
	}
	if err := json.Unmarshal(body, &dto); err != nil {
		return nil, fmt.Errorf("yahoo fundamental: decode: %w", err)
	}
	if len(dto.QuoteSummary.Result) == 0 {
		return nil, ErrNoData
	}
	modules := dto.QuoteSummary.Result[0]

	m := &Metrics{Symbol: symbol, Source: "yahoo"}

	if stats, ok := modules["defaultKeyStatistics"]; ok {
		m.PE = rawFloat(stats["trailingPE"])
		m.PB = rawFloat(stats["priceToBook"])
		m.Beta = rawFloat(stats["beta"])
		m.MarketCap = rawFloat(stats["marketCap"])
	}
	if fin, ok := modules["financialData"]; ok {
		m.ProfitMargin = rawFloat(fin["profitMargins"])
		m.OperatingMargin = rawFloat(fin["operatingMargins"])
		m.ReturnOnEquity = rawFloat(fin["returnOnEquity"])
		m.RevenueGrowth = rawFloat(fin["revenueGrowth"])
		m.EarningsGrowth = rawFloat(fin["earningsGrowth"])
		m.DebtToEquity = rawFloat(fin["debtToEquity"])
		if curr, ok := fin["financialCurrency"].(string); ok {
			m.Currency = curr
		}
	}
	if sum, ok := modules["summaryDetail"]; ok {
		m.ForwardPE = rawFloat(sum["forwardPE"])
		m.DividendYield = rawFloat(sum["dividendYield"])
		if m.MarketCap == 0 {
			m.MarketCap = rawFloat(sum["marketCap"])
		}
	}
	if price, ok := modules["price"]; ok {
		if curr, ok := price["currency"].(string); ok && m.Currency == "" {
			m.Currency = curr
		}
		if ts := rawFloat(price["regularMarketTime"]); ts > 0 {
			m.AsOf = time.Unix(int64(ts), 0).UTC()
		}
	}
	if m.AsOf.IsZero() {
		m.AsOf = time.Now().UTC()
	}
	// If literally every metric is zero, treat as no data so the
	// fallback chain can try the next provider.
	if isZeroMetrics(m) {
		return nil, ErrNoData
	}
	return m, nil
}

// rawFloat reads a Yahoo numeric field. Yahoo wraps most numeric
// fields in {"raw": <float>, "fmt": <string>}; some terser ones
// come back bare. Returns 0 when the field is missing or the shape
// doesn't yield a number.
func rawFloat(v any) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	case map[string]any:
		// {"raw": N, "fmt": "...", "longFmt": "..."}
		if raw, ok := t["raw"]; ok {
			return rawFloat(raw)
		}
	}
	return 0
}

func isZeroMetrics(m *Metrics) bool {
	return m.PE == 0 && m.ForwardPE == 0 && m.PB == 0 && m.DividendYield == 0 &&
		m.ProfitMargin == 0 && m.OperatingMargin == 0 && m.ReturnOnEquity == 0 &&
		m.RevenueGrowth == 0 && m.EarningsGrowth == 0 && m.DebtToEquity == 0 &&
		m.MarketCap == 0 && m.Beta == 0
}
