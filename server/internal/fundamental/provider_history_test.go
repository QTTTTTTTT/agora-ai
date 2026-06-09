package fundamental

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestYahooHistoryParserHappy(t *testing.T) {
	// Synthetic Yahoo quoteSummary payload with 3 years of data.
	payload := map[string]any{
		"quoteSummary": map[string]any{
			"result": []any{
				map[string]any{
					"incomeStatementHistory": map[string]any{
						"incomeStatementHistory": []any{
							yearRow(2024, "totalRevenue", 100.0, "grossProfit", 42.0, "operatingIncome", 30.0, "netIncome", 24.0),
							yearRow(2023, "totalRevenue", 95.0, "grossProfit", 41.0, "operatingIncome", 28.0, "netIncome", 22.0),
							yearRow(2022, "totalRevenue", 88.0, "grossProfit", 39.0, "operatingIncome", 25.0, "netIncome", 19.0),
						},
					},
					"balanceSheetHistory": map[string]any{
						"balanceSheetStatements": []any{
							yearRow(2024, "totalStockholderEquity", 120.0, "totalDebt", 30.0, "totalCurrentAssets", 60, "totalCurrentLiabilities", 20),
							yearRow(2023, "totalStockholderEquity", 110.0, "totalDebt", 35.0, "totalCurrentAssets", 55, "totalCurrentLiabilities", 22),
							yearRow(2022, "totalStockholderEquity", 100.0, "totalDebt", 40.0, "totalCurrentAssets", 50, "totalCurrentLiabilities", 25),
						},
					},
					"cashflowStatementHistory": map[string]any{
						"cashflowStatements": []any{
							yearRow(2024, "totalCashFromOperatingActivities", 35.0, "capitalExpenditures", -10.0),
							yearRow(2023, "totalCashFromOperatingActivities", 33.0, "capitalExpenditures", -9.0),
							yearRow(2022, "totalCashFromOperatingActivities", 30.0, "capitalExpenditures", -8.0),
						},
					},
					"earningsHistory": map[string]any{
						"history": []any{
							yearRow(2024, "epsActual", 6.0),
							yearRow(2023, "epsActual", 5.5),
							yearRow(2022, "epsActual", 4.8),
						},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(payload)
	series, err := parseYahooHistory(body, 10)
	if err != nil {
		t.Fatalf("parseYahooHistory err: %v", err)
	}
	if len(series) != 3 {
		t.Fatalf("expected 3 years, got %d", len(series))
	}
	if series[0].Year != 2024 {
		t.Fatalf("expected most-recent year first, got %d", series[0].Year)
	}
	if series[0].ReturnOnEquity == 0 {
		t.Fatalf("expected ROE non-zero")
	}
	if series[0].FreeCashFlow != 25 {
		t.Fatalf("expected FCF=25 (35-10), got %.2f", series[0].FreeCashFlow)
	}
	if series[0].EPS != 6 {
		t.Fatalf("expected EPS=6, got %.2f", series[0].EPS)
	}
}

func TestYahooHistoryFetcherWiring(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "quoteSummary/AAPL") {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"quoteSummary":{"result":[{"incomeStatementHistory":{"incomeStatementHistory":[]}}]}}`))
	}))
	defer srv.Close()

	p := &YahooHistoryProvider{BaseURL: srv.URL}
	if !p.Supports("us_equity") {
		t.Fatalf("expected Yahoo to support us_equity")
	}
	if p.Supports("a_share") {
		t.Fatalf("Yahoo should not support a_share by default")
	}
	_, err := p.FetchHistory(context.Background(), FetchRequest{Symbol: "AAPL", Market: "us_equity"}, 5)
	if err == nil || !errors.Is(err, ErrNoData) {
		t.Fatalf("expected ErrNoData on empty payload, got %v", err)
	}
}

func TestAkshareHistoryParserHappy(t *testing.T) {
	payload := []map[string]any{
		{"year": 2024, "roe": 18.5, "free_cash_flow": 1_500_000_000.0, "eps": 6.2, "毛利率": 42.1, "debt_to_equity": 0.4},
		{"year": 2023, "roe": 16.4, "free_cash_flow": 1_200_000_000.0, "eps": 5.5, "毛利率": 41.0, "debt_to_equity": 0.5},
		{"year": 2022, "roe": 14.2, "free_cash_flow": 1_000_000_000.0, "eps": 4.8, "毛利率": 39.5, "debt_to_equity": 0.6},
	}
	body, _ := json.Marshal(payload)
	series, err := parseAkshareHistory(body, 5)
	if err != nil {
		t.Fatalf("parseAkshareHistory err: %v", err)
	}
	if len(series) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(series))
	}
	if series[0].Year != 2024 {
		t.Fatalf("expected most-recent year first")
	}
	if series[0].ReturnOnEquity > 1.0 || series[0].ReturnOnEquity < 0.10 {
		t.Fatalf("expected ROE percent converted to fraction, got %.4f", series[0].ReturnOnEquity)
	}
}

func TestAkshareHistoryProviderWiring(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"year":2024,"roe":18.5}]`))
	}))
	defer srv.Close()
	p := &AkshareHistoryProvider{BaseURL: srv.URL}
	if !p.Supports("a_share") {
		t.Fatalf("expected Akshare to support a_share")
	}
	if p.Supports("us_equity") {
		t.Fatalf("Akshare should not support us_equity")
	}
	series, err := p.FetchHistory(context.Background(), FetchRequest{Symbol: "600519", Market: "a_share"}, 5)
	if err != nil {
		t.Fatalf("FetchHistory err: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("expected 1 row, got %d", len(series))
	}
}

func TestHistoricalCacheHits(t *testing.T) {
	calls := 0
	stub := historicalFetcherFunc(func(ctx context.Context, req FetchRequest, lookback int) ([]YearlyMetrics, error) {
		calls++
		return []YearlyMetrics{{Year: 2024, ReturnOnEquity: 0.18}}, nil
	})
	cache := NewHistoricalCache(stub, time.Minute)
	_, _ = cache.FetchHistory(context.Background(), FetchRequest{Symbol: "AAPL", Market: "us_equity"}, 5)
	_, _ = cache.FetchHistory(context.Background(), FetchRequest{Symbol: "AAPL", Market: "us_equity"}, 5)
	if calls != 1 {
		t.Fatalf("expected upstream called once, got %d", calls)
	}
	_, _ = cache.FetchHistory(context.Background(), FetchRequest{Symbol: "AAPL", Market: "us_equity"}, 10)
	if calls != 2 {
		t.Fatalf("different lookback should trigger upstream, got %d calls", calls)
	}
}

func TestHistoricalRegistryFallthrough(t *testing.T) {
	reg := NewHistoricalRegistry()
	reg.Register(historicalProviderFunc{
		name:     "primary",
		supports: func(m string) bool { return m == "us_equity" },
		fetch:    func(ctx context.Context, req FetchRequest, lookback int) ([]YearlyMetrics, error) { return nil, ErrNoData },
	})
	reg.Register(historicalProviderFunc{
		name:     "secondary",
		supports: func(m string) bool { return m == "us_equity" },
		fetch: func(ctx context.Context, req FetchRequest, lookback int) ([]YearlyMetrics, error) {
			return []YearlyMetrics{{Year: 2024, ReturnOnEquity: 0.2}}, nil
		},
	})
	series, err := reg.FetchHistory(context.Background(), FetchRequest{Symbol: "AAPL", Market: "us_equity"}, 5)
	if err != nil {
		t.Fatalf("expected fallthrough success, got %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("expected secondary's row, got %d", len(series))
	}
}

func yearRow(year int, kv ...any) map[string]any {
	row := map[string]any{
		"endDate": map[string]any{"raw": float64(time.Date(year, 12, 31, 0, 0, 0, 0, time.UTC).Unix())},
	}
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			continue
		}
		row[key] = map[string]any{"raw": toFloat(kv[i+1])}
	}
	return row
}

func toFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	}
	return 0
}

type historicalFetcherFunc func(ctx context.Context, req FetchRequest, lookback int) ([]YearlyMetrics, error)

func (f historicalFetcherFunc) FetchHistory(ctx context.Context, req FetchRequest, lookback int) ([]YearlyMetrics, error) {
	return f(ctx, req, lookback)
}

type historicalProviderFunc struct {
	name     string
	supports func(string) bool
	fetch    func(ctx context.Context, req FetchRequest, lookback int) ([]YearlyMetrics, error)
}

func (f historicalProviderFunc) Name() string                  { return f.name }
func (f historicalProviderFunc) Supports(m string) bool        { return f.supports(m) }
func (f historicalProviderFunc) FetchHistory(ctx context.Context, req FetchRequest, lookback int) ([]YearlyMetrics, error) {
	return f.fetch(ctx, req, lookback)
}
