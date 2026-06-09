package fundamental

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AkshareHistoryProvider talks to a self-hosted akshare-MCP for
// A-share multi-year financial abstracts. Different forks of the
// MCP expose the data under slightly different paths and field
// names — we try a small candidate list and fold every numeric
// value we recognise into YearlyMetrics.
//
// Recommended upstream calls (the MCP picks based on path):
//   * stock_financial_abstract        — 主要财务指标年表
//   * stock_financial_analysis_indicator — 综合财务指标
//   * stock_financial_us_analysis_indicator — US/HK fallback
type AkshareHistoryProvider struct {
	HTTPClient *http.Client
	// BaseURL is the akshare-MCP root. Empty disables this
	// provider entirely.
	BaseURL string
	// Markets defaults to {"a_share"} when empty.
	Markets []string
}

// Name implements HistoricalProvider.
func (p *AkshareHistoryProvider) Name() string { return "akshare_history" }

// Supports implements HistoricalProvider.
func (p *AkshareHistoryProvider) Supports(market string) bool {
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

// FetchHistory implements HistoricalProvider.
func (p *AkshareHistoryProvider) FetchHistory(ctx context.Context, req FetchRequest, lookbackYears int) ([]YearlyMetrics, error) {
	req = req.Normalize()
	if req.Symbol == "" || strings.TrimSpace(p.BaseURL) == "" {
		return nil, ErrNoData
	}
	if lookbackYears <= 0 {
		lookbackYears = 10
	}
	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 12 * time.Second}
	}
	candidates := []string{
		"/api/financial_abstract",
		"/financial_abstract",
		"/api/financial_indicator",
		"/financial_indicator",
	}
	var lastErr error
	for _, path := range candidates {
		endpoint, err := p.endpoint(path, req)
		if err != nil {
			lastErr = err
			continue
		}
		series, err := p.fetchOne(ctx, client, endpoint, lookbackYears)
		if err == nil && len(series) > 0 {
			return series, nil
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

func (p *AkshareHistoryProvider) endpoint(path string, req FetchRequest) (string, error) {
	u, err := url.Parse(strings.TrimRight(p.BaseURL, "/") + path)
	if err != nil {
		return "", fmt.Errorf("akshare history: build url: %w", err)
	}
	q := u.Query()
	q.Set("symbol", req.Symbol)
	q.Set("market", req.Market)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (p *AkshareHistoryProvider) fetchOne(ctx context.Context, client *http.Client, endpoint string, lookback int) ([]YearlyMetrics, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("akshare history: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoData
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("akshare history: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("akshare history: read: %w", err)
	}
	return parseAkshareHistory(body, lookback)
}

// parseAkshareHistory tolerates two response shapes:
//   * [{year:..., roe:..., ...}, ...] — array of yearly rows
//   * {data: [{...}, ...]}            — same wrapped in {data}
//
// Field aliases follow the snapshot parser's conventions (chinese
// and english key variants).
func parseAkshareHistory(body []byte, lookback int) ([]YearlyMetrics, error) {
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return nil, ErrNoData
	}
	var rows []map[string]any
	if body[0] == '[' {
		if err := json.Unmarshal(body, &rows); err != nil {
			return nil, fmt.Errorf("akshare history: decode array: %w", err)
		}
	} else if body[0] == '{' {
		var wrap map[string]any
		if err := json.Unmarshal(body, &wrap); err != nil {
			return nil, fmt.Errorf("akshare history: decode object: %w", err)
		}
		if arr, ok := wrap["data"].([]any); ok {
			for _, item := range arr {
				if row, ok := item.(map[string]any); ok {
					rows = append(rows, row)
				}
			}
		} else {
			// Some MCPs return {2024: {...}, 2023: {...}}; flatten.
			for key, value := range wrap {
				if row, ok := value.(map[string]any); ok {
					if _, hasYear := row["year"]; !hasYear {
						if y, err := strconv.Atoi(key); err == nil {
							row["year"] = float64(y)
						}
					}
					rows = append(rows, row)
				}
			}
		}
	} else {
		return nil, ErrNoData
	}
	if len(rows) == 0 {
		return nil, ErrNoData
	}

	out := make([]YearlyMetrics, 0, len(rows))
	for _, row := range rows {
		y := yearlyFromAkshareRow(row)
		if y.Year == 0 {
			continue
		}
		out = append(out, y)
	}
	if len(out) == 0 {
		return nil, ErrNoData
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Year > out[j].Year })
	if lookback > 0 && len(out) > lookback {
		out = out[:lookback]
	}
	return out, nil
}

func yearlyFromAkshareRow(row map[string]any) YearlyMetrics {
	y := YearlyMetrics{}
	y.Year = akInt(row, "year", "报告期", "报告年度", "annual_year")
	if y.Year == 0 {
		// Some MCPs serialise the report date as "2024-12-31".
		if s, ok := row["report_date"].(string); ok {
			if t, err := time.Parse("2006-01-02", s); err == nil {
				y.Year = t.Year()
			}
		}
		if s, ok := row["报告日期"].(string); ok && y.Year == 0 {
			if t, err := time.Parse("2006-01-02", s); err == nil {
				y.Year = t.Year()
			}
		}
	}
	y.ReturnOnEquity = akPercent(row, "roe", "ROE", "净资产收益率")
	y.ReturnOnCapital = akPercent(row, "roic", "ROIC", "投入资本回报率")
	y.GrossMargin = akPercent(row, "gross_margin", "毛利率")
	y.OperatingMargin = akPercent(row, "operating_margin", "营业利润率")
	y.ProfitMargin = akPercent(row, "net_profit_margin", "净利率", "销售净利率")
	y.FreeCashFlow = akFloat(row, "free_cash_flow", "自由现金流", "fcf")
	y.EPS = akFloat(row, "eps", "EPS", "每股收益", "基本每股收益")
	y.BookValuePerShare = akFloat(row, "bvps", "BVPS", "每股净资产")
	y.DividendPerShare = akFloat(row, "dps", "每股股利", "每股分红")
	y.CurrentRatio = akFloat(row, "current_ratio", "流动比率")
	y.DebtToEquity = akFloat(row, "debt_to_equity", "资产负债率")
	y.RevenueGrowthYoY = akPercent(row, "revenue_growth_yoy", "营业收入同比", "营业收入增长率")
	y.EarningsGrowthYoY = akPercent(row, "earnings_growth_yoy", "净利润同比", "净利润增长率")
	return y
}

// akInt extracts an integer field from the row using the same
// tolerant value parsing as akFloat.
func akInt(row map[string]any, keys ...string) int {
	for _, key := range keys {
		v, ok := row[key]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case float64:
			return int(t)
		case int:
			return t
		case int64:
			return int(t)
		case json.Number:
			if i, err := t.Int64(); err == nil {
				return int(i)
			}
		case string:
			if i, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
				return i
			}
		}
	}
	return 0
}

// akPercent extracts a percent-shaped field. Akshare typically
// returns ROE / margin / growth as percent strings like "15.32"
// (already in %) — we divide by 100 so the result is a fraction
// matching the snapshot conventions (0.15 = 15%).
func akPercent(row map[string]any, keys ...string) float64 {
	v := akFloat(row, keys...)
	if v == 0 {
		return 0
	}
	if v > 1.5 || v < -1.5 {
		// Heuristic: a value > 1.5 is almost certainly a percent
		// (ROE of 150% is implausible; ROE of 1500% basis-points
		// is impossible for any real company).
		return v / 100.0
	}
	return v
}
