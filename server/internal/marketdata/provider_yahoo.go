package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// yahooQuoteProvider returns a quote provider that hits Yahoo Finance.
//
// Yahoo's legacy `/v7/finance/quote` endpoint started returning HTTP 401
// in 2023 unless callers carry a session cookie + crumb pair. The
// `/v8/finance/chart/<symbol>` endpoint is still keyless and exposes the
// regular-market price, volume, currency, and exchange in `chart.result[0].meta`,
// which is exactly what QuoteSnapshot needs. We therefore prefer v8 and only
// keep v7 as a defensive fallback (e.g. if Yahoo restores it later).
//
// Endpoints:
//
//	GET https://query1.finance.yahoo.com/v8/finance/chart/AAPL?interval=1d&range=1d
//	GET https://query1.finance.yahoo.com/v7/finance/quote?symbols=AAPL  (legacy fallback)
func (s *Service) yahooQuoteProvider() quoteProviderFunc {
	return func(ctx context.Context, instrument InstrumentRef) (*QuoteSnapshot, error) {
		symbol := YahooSymbol(instrument)
		if symbol == "" {
			return nil, fmt.Errorf("yahoo: cannot derive yahoo symbol from %q", instrument.Symbol)
		}
		quote, err := s.yahooChartQuote(ctx, instrument, symbol)
		if err == nil {
			return quote, nil
		}
		chartErr := err
		quote, err = s.yahooLegacyQuote(ctx, instrument, symbol)
		if err == nil {
			return quote, nil
		}
		return nil, fmt.Errorf("yahoo: chart=%v; legacy=%v", chartErr, err)
	}
}

const yahooChartBaseURL = "https://query1.finance.yahoo.com"

// yahooChartQuote uses the v8 chart endpoint against the public Yahoo API.
// Internally it delegates to yahooChartQuoteAt so tests can point the same
// logic at httptest fixtures.
func (s *Service) yahooChartQuote(ctx context.Context, instrument InstrumentRef, symbol string) (*QuoteSnapshot, error) {
	return s.yahooChartQuoteAt(ctx, yahooChartBaseURL, instrument, symbol)
}

// yahooChartQuoteAt fetches a single-symbol quote from a Yahoo-compatible
// `/v8/finance/chart/<symbol>` endpoint. The response shape is:
//
//	{ "chart": { "result": [ { "meta": { "regularMarketPrice": ..., "currency": "USD", ... } } ] } }
//
// We only read fields under `meta`, since the time-series candles are not
// needed for the QuoteSnapshot we return.
func (s *Service) yahooChartQuoteAt(ctx context.Context, baseURL string, instrument InstrumentRef, symbol string) (*QuoteSnapshot, error) {
	endpoint, err := url.Parse(strings.TrimRight(baseURL, "/") + "/v8/finance/chart/" + url.PathEscape(symbol))
	if err != nil {
		return nil, fmt.Errorf("yahoo chart: parse url: %w", err)
	}
	q := endpoint.Query()
	q.Set("interval", "1d")
	q.Set("range", "1d")
	endpoint.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("yahoo chart: build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("yahoo chart: http: %w", err)
	}
	defer resp.Body.Close()
	if isThrottleStatus(resp.StatusCode) {
		return nil, fmt.Errorf("%w: yahoo chart: http %d", ErrUpstreamThrottled, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("yahoo chart: http %d", resp.StatusCode)
	}
	var payload struct {
		Chart struct {
			Result []struct {
				Meta map[string]any `json:"meta"`
			} `json:"result"`
			Error any `json:"error"`
		} `json:"chart"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("yahoo chart: decode: %w", err)
	}
	if len(payload.Chart.Result) == 0 || payload.Chart.Result[0].Meta == nil {
		return nil, fmt.Errorf("yahoo chart: empty result for %s", symbol)
	}
	meta := payload.Chart.Result[0].Meta
	price := firstPositiveFloat(meta, "regularMarketPrice", "chartPreviousClose", "previousClose")
	if price <= 0 {
		return nil, fmt.Errorf("yahoo chart: non-positive price for %s", symbol)
	}
	quote := &QuoteSnapshot{
		Symbol:        firstNonEmpty(stringValue(meta, "symbol"), symbol),
		InstrumentKey: instrument.InstrumentKey,
		Market:        instrument.Market,
		Exchange:      firstNonEmpty(stringValue(meta, "exchangeName"), stringValue(meta, "fullExchangeName"), instrument.Exchange),
		AssetClass:    instrument.AssetClass,
		Price:         price,
		Volume:        int64(firstPositiveFloat(meta, "regularMarketVolume")),
		QuoteCurrency: firstNonEmpty(stringValue(meta, "currency"), instrument.QuoteCurrency),
		AsOf:          yahooQuoteAsOf(meta, time.Now()),
		Source:        "yahoo",
	}
	return quote, nil
}

// yahooQuoteAsOf decides what timestamp to stamp on the quote.
//
// Background: Yahoo's `/v8/finance/chart/<symbol>` `meta.regularMarketTime`
// is the timestamp of the last regular-session trade observed in the daily
// candle. During pre-open and the first few minutes after the US bell,
// Yahoo's response can still expose the *previous* close in this field
// even when `regularMarketPrice` already reflects today's tick. That
// produced 16h-old AsOf values in production, which then tripped
// risk.StaleQuoteGuard and rejected every risk-increasing order during
// the first minutes of trading.
//
// We solve it by reading `meta.currentTradingPeriod.regular.{start,end}`
// (Yahoo's authoritative window for the regular session, returned in
// unix seconds). If the current wall-clock falls inside that window the
// price is live — AsOf=now is the truthful statement. Outside the
// window (closed market, pre/post session) we fall back to the
// `regularMarketTime` so the staleness signal accurately reflects how
// old the *last actual print* is. When the field is absent (older
// fixtures, malformed responses) we degrade to the previous behaviour.
func yahooQuoteAsOf(meta map[string]any, now time.Time) time.Time {
	nowUTC := now.UTC()
	regularStart, regularEnd := yahooRegularSessionWindow(meta)
	if !regularStart.IsZero() && !regularEnd.IsZero() {
		if (nowUTC.Equal(regularStart) || nowUTC.After(regularStart)) && nowUTC.Before(regularEnd) {
			return nowUTC
		}
	}
	if ts := firstPositiveFloat(meta, "regularMarketTime"); ts > 0 {
		return time.Unix(int64(ts), 0).UTC()
	}
	return nowUTC
}

// yahooRegularSessionWindow extracts the regular-session start/end from the
// meta block. Returns zero times when the field is missing or malformed so
// the caller can fall back to the legacy timestamp.
func yahooRegularSessionWindow(meta map[string]any) (time.Time, time.Time) {
	periods, ok := meta["currentTradingPeriod"].(map[string]any)
	if !ok {
		return time.Time{}, time.Time{}
	}
	regular, ok := periods["regular"].(map[string]any)
	if !ok {
		return time.Time{}, time.Time{}
	}
	start := firstPositiveFloat(regular, "start")
	end := firstPositiveFloat(regular, "end")
	if start <= 0 || end <= 0 || start >= end {
		return time.Time{}, time.Time{}
	}
	return time.Unix(int64(start), 0).UTC(), time.Unix(int64(end), 0).UTC()
}

func (s *Service) yahooLegacyQuote(ctx context.Context, instrument InstrumentRef, symbol string) (*QuoteSnapshot, error) {
	endpoint, _ := url.Parse("https://query1.finance.yahoo.com/v7/finance/quote")
	q := endpoint.Query()
	q.Set("symbols", symbol)
	endpoint.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("yahoo v7: build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("yahoo v7: http: %w", err)
	}
	defer resp.Body.Close()
	if isThrottleStatus(resp.StatusCode) {
		return nil, fmt.Errorf("%w: yahoo v7: http %d", ErrUpstreamThrottled, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("yahoo v7: http %d", resp.StatusCode)
	}
	var payload struct {
		QuoteResponse struct {
			Result []map[string]any `json:"result"`
			Error  any              `json:"error"`
		} `json:"quoteResponse"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("yahoo v7: decode: %w", err)
	}
	if len(payload.QuoteResponse.Result) == 0 {
		return nil, fmt.Errorf("yahoo v7: empty result for %s", symbol)
	}
	row := payload.QuoteResponse.Result[0]
	price := firstPositiveFloat(row, "regularMarketPrice", "postMarketPrice", "preMarketPrice", "ask", "bid", "previousClose")
	if price <= 0 {
		return nil, fmt.Errorf("yahoo v7: non-positive price for %s", symbol)
	}
	// The v7 quote endpoint shares the same `marketState` semantics as v8.
	// When marketState=="REGULAR" the price field is live, so AsOf=now is
	// the truthful statement (matches the v8 yahooQuoteAsOf logic). For
	// PRE/POST/CLOSED we keep regularMarketTime so the staleness signal
	// reflects the last actual print.
	asOf := time.Now().UTC()
	state := strings.ToUpper(strings.TrimSpace(stringValue(row, "marketState")))
	if state != "REGULAR" {
		if ts := firstPositiveFloat(row, "regularMarketTime"); ts > 0 {
			asOf = time.Unix(int64(ts), 0).UTC()
		}
	}
	quote := &QuoteSnapshot{
		Symbol:        firstNonEmpty(stringValue(row, "symbol"), symbol),
		InstrumentKey: instrument.InstrumentKey,
		Market:        instrument.Market,
		Exchange:      firstNonEmpty(stringValue(row, "exchange"), instrument.Exchange),
		AssetClass:    instrument.AssetClass,
		Price:         price,
		Bid:           firstPositiveFloat(row, "bid"),
		Ask:           firstPositiveFloat(row, "ask"),
		Volume:        int64(firstPositiveFloat(row, "regularMarketVolume", "averageDailyVolume3Month")),
		QuoteCurrency: firstNonEmpty(stringValue(row, "currency"), instrument.QuoteCurrency),
		AsOf:          asOf,
		Source:        "yahoo",
	}
	return quote, nil
}
