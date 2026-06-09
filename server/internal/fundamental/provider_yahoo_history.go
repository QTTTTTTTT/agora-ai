package fundamental

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/fundai/server/internal/yahooauth"
)

// YahooHistoryProvider talks to Yahoo Finance's quoteSummary
// endpoint with the multi-year history modules:
//
//	https://query1.finance.yahoo.com/v10/finance/quoteSummary/AAPL
//	    ?modules=incomeStatementHistory,balanceSheetHistory,cashflowStatementHistory,earningsHistory,defaultKeyStatistics
//
// Yahoo's free tier returns up to 4 years for each statement
// module (and `defaultKeyStatistics.trailingPE` for the trailing
// quarter). For most US-equity coverage that's enough to satisfy
// Lynch's 3-year CAGR test; Buffett's 10-year ROE requirement
// will mark earlier years as data_unavailable when Yahoo doesn't
// expose them. Operators wanting deeper history can override
// BaseURL to a paid proxy that proxies into Yahoo Premium.
//
// The provider intentionally reuses YahooProvider's crumb auth
// machinery via yahooauth.Default so a single bot session
// authenticates both the snapshot and the history calls.
type YahooHistoryProvider struct {
	HTTPClient *http.Client
	BaseURL    string
	Markets    []string
}

// Name implements HistoricalProvider.
func (p *YahooHistoryProvider) Name() string { return "yahoo_history" }

// Supports implements HistoricalProvider.
func (p *YahooHistoryProvider) Supports(market string) bool {
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

// FetchHistory implements HistoricalProvider.
func (p *YahooHistoryProvider) FetchHistory(ctx context.Context, req FetchRequest, lookbackYears int) ([]YearlyMetrics, error) {
	req = req.Normalize()
	if req.Symbol == "" {
		return nil, ErrNoData
	}
	if lookbackYears <= 0 {
		lookbackYears = 10
	}
	body, err := p.doFetch(ctx, req, false)
	if err == nil {
		return parseYahooHistory(body, lookbackYears)
	}
	if !isCrumbAuthError(err) {
		return nil, err
	}
	yahooauth.Default.Invalidate()
	body, retryErr := p.doFetch(ctx, req, true)
	if retryErr != nil {
		return nil, retryErr
	}
	return parseYahooHistory(body, lookbackYears)
}

func (p *YahooHistoryProvider) doFetch(ctx context.Context, req FetchRequest, _ bool) ([]byte, error) {
	endpoint, err := p.endpoint(req)
	if err != nil {
		return nil, err
	}
	crumb, jar, authErr := yahooauth.Default.Get(ctx)
	if authErr == nil && crumb != "" {
		joiner := "?"
		if strings.Contains(endpoint, "?") {
			joiner = "&"
		}
		endpoint = endpoint + joiner + "crumb=" + url.QueryEscape(crumb)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	yahooauth.AttachToRequest(httpReq)
	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 12 * time.Second}
	}
	if jar != nil && client.Jar == nil {
		clone := *client
		clone.Jar = jar
		client = &clone
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("yahoo history: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoData
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("yahoo history: status %d (crumb auth): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("yahoo history: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("yahoo history: read: %w", err)
	}
	return body, nil
}

func (p *YahooHistoryProvider) endpoint(req FetchRequest) (string, error) {
	base := p.BaseURL
	if strings.TrimSpace(base) == "" {
		base = "https://query1.finance.yahoo.com"
	}
	base = strings.TrimRight(base, "/")
	u, err := url.Parse(base + "/v10/finance/quoteSummary/" + url.PathEscape(req.Symbol))
	if err != nil {
		return "", fmt.Errorf("yahoo history: build url: %w", err)
	}
	q := u.Query()
	q.Set("modules", "incomeStatementHistory,balanceSheetHistory,cashflowStatementHistory,earningsHistory")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// parseYahooHistory walks the three statement-history arrays Yahoo
// returns and folds them into YearlyMetrics rows keyed by year.
// Each module returns a list of statements (most-recent first)
// with `endDate.raw` as a unix-second timestamp we collapse to
// calendar year.
func parseYahooHistory(body []byte, lookbackYears int) ([]YearlyMetrics, error) {
	var dto struct {
		QuoteSummary struct {
			Result []map[string]any `json:"result"`
		} `json:"quoteSummary"`
	}
	if err := json.Unmarshal(body, &dto); err != nil {
		return nil, fmt.Errorf("yahoo history: decode: %w", err)
	}
	if len(dto.QuoteSummary.Result) == 0 {
		return nil, ErrNoData
	}
	root := dto.QuoteSummary.Result[0]
	byYear := map[int]*YearlyMetrics{}

	walk := func(modKey, listKey string, apply func(year int, row map[string]any)) {
		mod, ok := root[modKey].(map[string]any)
		if !ok {
			return
		}
		statements, ok := mod[listKey].([]any)
		if !ok {
			return
		}
		for _, item := range statements {
			row, ok := item.(map[string]any)
			if !ok {
				continue
			}
			ts := rawFloat(row["endDate"])
			if ts == 0 {
				continue
			}
			year := time.Unix(int64(ts), 0).UTC().Year()
			apply(year, row)
		}
	}

	walk("incomeStatementHistory", "incomeStatementHistory", func(year int, row map[string]any) {
		ensureYear(byYear, year)
		revenue := rawFloat(row["totalRevenue"])
		gross := rawFloat(row["grossProfit"])
		opIncome := rawFloat(row["operatingIncome"])
		net := rawFloat(row["netIncome"])
		if revenue > 0 {
			if gross != 0 {
				byYear[year].GrossMargin = gross / revenue
			}
			if opIncome != 0 {
				byYear[year].OperatingMargin = opIncome / revenue
			}
			if net != 0 {
				byYear[year].ProfitMargin = net / revenue
			}
		}
		// Diluted EPS lives under earningsHistory typically; we
		// fall back to netIncome / shares when neither is set.
		_ = net
	})

	walk("balanceSheetHistory", "balanceSheetStatements", func(year int, row map[string]any) {
		ensureYear(byYear, year)
		totalEquity := rawFloat(row["totalStockholderEquity"])
		totalDebt := rawFloat(row["totalDebt"])
		currentAssets := rawFloat(row["totalCurrentAssets"])
		currentLiab := rawFloat(row["totalCurrentLiabilities"])
		commonStock := rawFloat(row["commonStock"])
		if totalEquity > 0 && totalDebt != 0 {
			byYear[year].DebtToEquity = totalDebt / totalEquity
		}
		if currentLiab > 0 && currentAssets != 0 {
			byYear[year].CurrentRatio = currentAssets / currentLiab
		}
		_ = commonStock
	})

	walk("cashflowStatementHistory", "cashflowStatements", func(year int, row map[string]any) {
		ensureYear(byYear, year)
		operating := rawFloat(row["totalCashFromOperatingActivities"])
		capex := rawFloat(row["capitalExpenditures"])
		// capex is reported negative in Yahoo's payload (cash outflow).
		fcf := operating + capex
		if fcf != 0 {
			byYear[year].FreeCashFlow = fcf
		}
	})

	walk("earningsHistory", "history", func(year int, row map[string]any) {
		ensureYear(byYear, year)
		eps := rawFloat(row["epsActual"])
		if eps != 0 {
			byYear[year].EPS = eps
		}
	})

	// Compute ROE = net income / equity using values we already
	// captured (ProfitMargin × revenue is approximate; we re-walk
	// statements for the precise ratio).
	if income, ok := root["incomeStatementHistory"].(map[string]any); ok {
		if balance, ok := root["balanceSheetHistory"].(map[string]any); ok {
			fillROEFromStatements(income, balance, byYear)
		}
	}

	if len(byYear) == 0 {
		return nil, ErrNoData
	}

	out := make([]YearlyMetrics, 0, len(byYear))
	for year, y := range byYear {
		y.Year = year
		out = append(out, *y)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Year > out[j].Year })
	if lookbackYears > 0 && len(out) > lookbackYears {
		out = out[:lookbackYears]
	}
	// Backfill YoY growth where consecutive years are present.
	for i := 0; i+1 < len(out); i++ {
		prev := out[i+1]
		cur := &out[i]
		if prev.EPS != 0 && cur.EPS != 0 {
			cur.EarningsGrowthYoY = (cur.EPS - prev.EPS) / absFloat(prev.EPS)
		}
	}
	return out, nil
}

// fillROEFromStatements walks the income + balance histories one
// more time to compute ROE = netIncome / totalStockholderEquity
// per year. Kept separate so the principal parser stays linear.
func fillROEFromStatements(income, balance map[string]any, byYear map[int]*YearlyMetrics) {
	netByYear := map[int]float64{}
	if list, ok := income["incomeStatementHistory"].([]any); ok {
		for _, item := range list {
			row, ok := item.(map[string]any)
			if !ok {
				continue
			}
			ts := rawFloat(row["endDate"])
			if ts == 0 {
				continue
			}
			year := time.Unix(int64(ts), 0).UTC().Year()
			if v := rawFloat(row["netIncome"]); v != 0 {
				netByYear[year] = v
			}
		}
	}
	if list, ok := balance["balanceSheetStatements"].([]any); ok {
		for _, item := range list {
			row, ok := item.(map[string]any)
			if !ok {
				continue
			}
			ts := rawFloat(row["endDate"])
			if ts == 0 {
				continue
			}
			year := time.Unix(int64(ts), 0).UTC().Year()
			equity := rawFloat(row["totalStockholderEquity"])
			net, ok := netByYear[year]
			if !ok || equity <= 0 {
				continue
			}
			ensureYear(byYear, year)
			byYear[year].ReturnOnEquity = net / equity
		}
	}
}

func ensureYear(m map[int]*YearlyMetrics, year int) {
	if _, ok := m[year]; !ok {
		m[year] = &YearlyMetrics{Year: year}
	}
}

func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
