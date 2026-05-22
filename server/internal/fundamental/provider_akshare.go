package fundamental

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

// AkshareProvider talks to a self-hosted akshare-MCP HTTP service
// for A-share / HK-share fundamentals. Mirrors the ohlc
// AkshareProvider's pattern: BaseURL must be set for the provider
// to claim a market, and we try multiple candidate endpoints
// because different MCP forks expose slightly different routes.
type AkshareProvider struct {
	HTTPClient *http.Client
	// BaseURL is the akshare-MCP root URL. Empty disables this
	// provider entirely.
	BaseURL string
	// Markets defaults to {"a_share"} when empty. Operators with a
	// HK-capable MCP can set Markets explicitly.
	Markets []string
}

// Name implements Provider.
func (p *AkshareProvider) Name() string { return "akshare" }

// Supports implements Provider.
func (p *AkshareProvider) Supports(market string) bool {
	if strings.TrimSpace(p.BaseURL) == "" {
		return false
	}
	markets := p.Markets
	if len(markets) == 0 {
		markets = []string{"a_share"}
	}
	for _, m := range markets {
		if strings.EqualFold(m, market) {
			return true
		}
	}
	return false
}

// Fetch implements Provider.
func (p *AkshareProvider) Fetch(ctx context.Context, req FetchRequest) (*Metrics, error) {
	req = req.Normalize()
	if req.Symbol == "" || strings.TrimSpace(p.BaseURL) == "" {
		return nil, ErrNoData
	}
	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	candidates := []string{"/api/fundamental", "/fundamental", "/api/key_metrics", "/key_metrics"}
	var lastErr error
	for _, path := range candidates {
		endpoint, err := p.endpoint(path, req)
		if err != nil {
			lastErr = err
			continue
		}
		m, err := p.fetchOne(ctx, client, endpoint, req.Symbol)
		if err == nil && m != nil {
			return m, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrNoData
}

func (p *AkshareProvider) endpoint(path string, req FetchRequest) (string, error) {
	u, err := url.Parse(strings.TrimRight(p.BaseURL, "/") + path)
	if err != nil {
		return "", fmt.Errorf("akshare fundamental: build url: %w", err)
	}
	q := u.Query()
	q.Set("symbol", req.Symbol)
	q.Set("market", req.Market)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (p *AkshareProvider) fetchOne(ctx context.Context, client *http.Client, endpoint, symbol string) (*Metrics, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("akshare fundamental: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoData
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("akshare fundamental: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("akshare fundamental: read: %w", err)
	}
	return parseAkshareMetrics(body, symbol)
}

// parseAkshareMetrics tolerates the same two response shapes the
// ohlc AkshareProvider does: raw object and {data: {...}} wrapped.
// Field aliases ("pe_ratio" / "pe" / "PE" / "市盈率") are tolerated
// so the provider keeps working across MCP forks and across the
// English / Chinese akshare endpoints.
func parseAkshareMetrics(body []byte, symbol string) (*Metrics, error) {
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return nil, ErrNoData
	}
	var row map[string]any
	if body[0] == '{' {
		var direct map[string]any
		if err := json.Unmarshal(body, &direct); err != nil {
			return nil, fmt.Errorf("akshare fundamental: decode: %w", err)
		}
		if inner, ok := direct["data"].(map[string]any); ok {
			row = inner
		} else {
			row = direct
		}
	} else if body[0] == '[' {
		var arr []map[string]any
		if err := json.Unmarshal(body, &arr); err != nil {
			return nil, fmt.Errorf("akshare fundamental: decode array: %w", err)
		}
		if len(arr) == 0 {
			return nil, ErrNoData
		}
		row = arr[0]
	} else {
		return nil, ErrNoData
	}
	if len(row) == 0 {
		return nil, ErrNoData
	}

	m := &Metrics{Symbol: symbol, Source: "akshare", AsOf: time.Now().UTC()}
	m.PE = akFloat(row, "pe", "pe_ratio", "PE", "trailing_pe", "市盈率")
	m.ForwardPE = akFloat(row, "forward_pe", "forwardPE", "动态市盈率")
	m.PB = akFloat(row, "pb", "pb_ratio", "PB", "市净率")
	m.DividendYield = akFloat(row, "dividend_yield", "dividendYield", "股息率")
	m.ProfitMargin = akFloat(row, "profit_margin", "净利率", "net_profit_margin")
	m.OperatingMargin = akFloat(row, "operating_margin", "营业利润率")
	m.ReturnOnEquity = akFloat(row, "roe", "ROE", "净资产收益率")
	m.RevenueGrowth = akFloat(row, "revenue_growth", "营收增长率", "营业收入同比")
	m.EarningsGrowth = akFloat(row, "earnings_growth", "净利润增长率", "净利润同比")
	m.DebtToEquity = akFloat(row, "debt_to_equity", "资产负债率")
	m.MarketCap = akFloat(row, "market_cap", "marketCap", "总市值")
	m.Beta = akFloat(row, "beta", "贝塔")
	if curr, ok := row["currency"].(string); ok {
		m.Currency = curr
	} else if curr, ok := row["calc_currency"].(string); ok {
		m.Currency = curr
	} else {
		m.Currency = "CNY"
	}
	if isZeroMetrics(m) {
		return nil, ErrNoData
	}
	return m, nil
}

// akFloat tolerates the same numeric soup the ohlc Akshare parser
// handles (number / string / json.Number).
func akFloat(row map[string]any, keys ...string) float64 {
	for _, key := range keys {
		v, ok := row[key]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case float64:
			return t
		case int:
			return float64(t)
		case int64:
			return float64(t)
		case json.Number:
			f, _ := t.Float64()
			return f
		case string:
			s := strings.TrimSpace(strings.TrimSuffix(t, "%"))
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return f
			}
		}
	}
	return 0
}
