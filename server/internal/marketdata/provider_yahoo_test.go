package marketdata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestYahooChartQuoteParsesV8MetaResponse asserts that the chart-based v8
// quote path (Yahoo's only keyless price endpoint after the v7 401 deprecation)
// correctly extracts price, volume, currency and exchange.
func TestYahooChartQuoteParsesV8MetaResponse(t *testing.T) {
	const sample = `{
		"chart": {
			"result": [{
				"meta": {
					"currency": "USD",
					"symbol": "AAPL",
					"exchangeName": "NMS",
					"regularMarketPrice": 300.23,
					"regularMarketTime": 1778875201,
					"regularMarketVolume": 54862836,
					"chartPreviousClose": 298.21
				}
			}],
			"error": null
		}
	}`
	var capturedPath string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		_, _ = w.Write([]byte(sample))
	}))
	defer srv.Close()

	svc := &Service{httpClient: srv.Client()}
	quote, err := svc.yahooChartQuoteAt(context.Background(), srv.URL, InstrumentRef{Symbol: "AAPL", Market: "us_equity"}, "AAPL")
	if err != nil {
		t.Fatalf("yahooChartQuoteAt: %v", err)
	}
	if quote.Price != 300.23 {
		t.Fatalf("expected price 300.23, got %v", quote.Price)
	}
	if quote.QuoteCurrency != "USD" {
		t.Fatalf("expected currency USD, got %q", quote.QuoteCurrency)
	}
	if quote.Volume != 54862836 {
		t.Fatalf("expected volume 54862836, got %d", quote.Volume)
	}
	if quote.Source != "yahoo" {
		t.Fatalf("expected source yahoo, got %q", quote.Source)
	}
	if !strings.Contains(capturedPath, "/v8/finance/chart/AAPL") {
		t.Fatalf("expected v8 chart path, got %q", capturedPath)
	}
}

// TestYahooChartQuoteRejectsHTTP4xx ensures we surface upstream errors rather
// than silently returning a zero-price quote.
func TestYahooChartQuoteRejectsHTTP4xx(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"chart":{"result":null,"error":{"code":"Unauthorized"}}}`))
	}))
	defer srv.Close()

	svc := &Service{httpClient: srv.Client()}
	_, err := svc.yahooChartQuoteAt(context.Background(), srv.URL, InstrumentRef{Symbol: "AAPL"}, "AAPL")
	if err == nil {
		t.Fatalf("expected error on 401 from chart endpoint")
	}
}

// TestYahooChartQuoteEndToEndDuringRegularSession is the regression test for
// the production bug: with the real Yahoo payload shape (containing
// currentTradingPeriod.regular plus a stale regularMarketTime), the
// returned quote's AsOf must be within a few seconds of `now` so the
// risk gate accepts the trade.
//
// We anchor the session window to runtime `time.Now()` rather than a
// fixed wall-clock, so the test stays deterministic regardless of when
// CI happens to run it.
func TestYahooChartQuoteEndToEndDuringRegularSession(t *testing.T) {
	now := time.Now().UTC()
	regularStart := now.Add(-30 * time.Minute).Unix()
	regularEnd := now.Add(6 * time.Hour).Unix()
	staleRegularMarketTime := now.Add(-16 * time.Hour).Unix()
	payload := map[string]any{
		"chart": map[string]any{
			"result": []any{
				map[string]any{
					"meta": map[string]any{
						"currency":            "USD",
						"symbol":              "MU",
						"exchangeName":        "NMS",
						"regularMarketPrice":  681.54,
						"regularMarketTime":   staleRegularMarketTime,
						"regularMarketVolume": 12345678,
						"currentTradingPeriod": map[string]any{
							"regular": map[string]any{
								"start": regularStart,
								"end":   regularEnd,
							},
						},
					},
				},
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	svc := &Service{httpClient: srv.Client()}
	quote, err := svc.yahooChartQuoteAt(context.Background(), srv.URL, InstrumentRef{Symbol: "MU", Market: "us_equity"}, "MU")
	if err != nil {
		t.Fatalf("yahooChartQuoteAt: %v", err)
	}
	age := time.Since(quote.AsOf)
	if age > 1*time.Minute || age < -1*time.Minute {
		t.Fatalf("AsOf %s drifted from now by %s; expected to be close to now during regular session", quote.AsOf, age)
	}
}

// TestYahooQuoteAsOfPrefersNowDuringRegularSession reproduces the production
// bug we hit on 2026-05-19: Yahoo's chart endpoint returned a regularMarketTime
// from the previous close (`2026-05-18T20:00:01Z`) even though the regular
// US session was already open (~10:06am EDT). The pre-fix code blindly
// stamped that 16h-old timestamp on the quote, so risk.StaleQuoteGuard
// rejected every risk-increasing order during the opening window.
//
// With the currentTradingPeriod-aware AsOf, the quote is stamped at
// `now` while we're inside the regular session window, which matches
// the truth that `regularMarketPrice` is the live tick.
func TestYahooQuoteAsOfPrefersNowDuringRegularSession(t *testing.T) {
	// Wall-clock: 2026-05-19 14:06 UTC == 10:06am EDT, market opened at 13:30 UTC.
	now := time.Date(2026, 5, 19, 14, 6, 0, 0, time.UTC)
	regularStart := time.Date(2026, 5, 19, 13, 30, 0, 0, time.UTC).Unix()
	regularEnd := time.Date(2026, 5, 19, 20, 0, 0, 0, time.UTC).Unix()
	previousClose := time.Date(2026, 5, 18, 20, 0, 1, 0, time.UTC).Unix()
	meta := map[string]any{
		"regularMarketPrice": 681.54,
		"regularMarketTime":  float64(previousClose),
		"currentTradingPeriod": map[string]any{
			"regular": map[string]any{
				"start": float64(regularStart),
				"end":   float64(regularEnd),
			},
		},
	}
	got := yahooQuoteAsOf(meta, now)
	if !got.Equal(now) {
		t.Fatalf("expected AsOf to use `now` during regular session, got %s (now=%s)", got, now)
	}
}

// TestYahooQuoteAsOfUsesRegularMarketTimeOutsideSession ensures we still
// surface the truthful (older) timestamp when the market is closed, so
// the StaleQuoteGuard can refuse to execute against literally-old prices.
func TestYahooQuoteAsOfUsesRegularMarketTimeOutsideSession(t *testing.T) {
	// Wall-clock 21:00 UTC: market closed at 20:00 UTC.
	now := time.Date(2026, 5, 19, 21, 0, 0, 0, time.UTC)
	regularStart := time.Date(2026, 5, 19, 13, 30, 0, 0, time.UTC).Unix()
	regularEnd := time.Date(2026, 5, 19, 20, 0, 0, 0, time.UTC).Unix()
	lastTradeTs := time.Date(2026, 5, 19, 19, 59, 58, 0, time.UTC).Unix()
	meta := map[string]any{
		"regularMarketTime": float64(lastTradeTs),
		"currentTradingPeriod": map[string]any{
			"regular": map[string]any{
				"start": float64(regularStart),
				"end":   float64(regularEnd),
			},
		},
	}
	got := yahooQuoteAsOf(meta, now)
	wanted := time.Unix(lastTradeTs, 0).UTC()
	if !got.Equal(wanted) {
		t.Fatalf("expected closed-market AsOf=%s, got %s", wanted, got)
	}
}

// TestYahooQuoteAsOfFallsBackWhenSessionMetaMissing covers older or partial
// fixtures that don't expose currentTradingPeriod (legacy responses,
// unit-test stubs, the v8 endpoint serving non-equity instruments). We
// must still stamp *something* sensible — the regularMarketTime if
// present, otherwise `now`.
func TestYahooQuoteAsOfFallsBackWhenSessionMetaMissing(t *testing.T) {
	now := time.Date(2026, 5, 19, 14, 6, 0, 0, time.UTC)
	lastTradeTs := time.Date(2026, 5, 18, 20, 0, 1, 0, time.UTC).Unix()
	meta := map[string]any{"regularMarketTime": float64(lastTradeTs)}
	got := yahooQuoteAsOf(meta, now)
	wanted := time.Unix(lastTradeTs, 0).UTC()
	if !got.Equal(wanted) {
		t.Fatalf("expected legacy-fallback AsOf=%s, got %s", wanted, got)
	}
	got = yahooQuoteAsOf(map[string]any{}, now)
	if !got.Equal(now.UTC()) {
		t.Fatalf("expected empty-meta AsOf=now, got %s", got)
	}
}

// TestYahooChartQuoteFallsBackWhenEmptyResult asserts that an empty result set
// returns an error so the higher-level fallback chain can try v7 / other
// providers.
func TestYahooChartQuoteFallsBackWhenEmptyResult(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(map[string]any{
			"chart": map[string]any{
				"result": []any{},
			},
		})
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	svc := &Service{httpClient: srv.Client()}
	_, err := svc.yahooChartQuoteAt(context.Background(), srv.URL, InstrumentRef{Symbol: "AAPL"}, "AAPL")
	if err == nil {
		t.Fatalf("expected error on empty result")
	}
}
