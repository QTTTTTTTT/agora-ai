package ohlc

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

// BinanceProvider fetches klines from Binance's public REST endpoint:
//
//	https://api.binance.com/api/v3/klines?symbol=BTCUSDT&interval=1d&limit=120
//
// Klines are returned as a JSON array of arrays (12-tuple per row).
// The provider only consumes the columns it needs (open time, OHLCV)
// and ignores the rest. Binance rejects unsupported intervals, so we
// translate our canonical Interval to its vendor-specific spellings
// before the call.
//
// Symbols normalize to upper-case with the "-" / "/" separators
// stripped: "BTC-USD" / "BTC/USD" -> "BTCUSD" (Binance pairs against
// USDT mostly; callers who want USDT pairs should pass "BTCUSDT"
// directly). The provider preserves the caller's symbol when it
// doesn't recognize the format, letting the upstream surface the
// real error.
type BinanceProvider struct {
	HTTPClient *http.Client
	// BaseURL lets tests / private-cloud deployments override the
	// public host. Empty falls back to https://api.binance.com.
	BaseURL string
	// Markets defaults to {"crypto"} when empty.
	Markets []string
}

// Name implements Provider.
func (p *BinanceProvider) Name() string { return "binance" }

// Supports implements Provider.
func (p *BinanceProvider) Supports(market string) bool {
	markets := p.Markets
	if len(markets) == 0 {
		markets = []string{"crypto"}
	}
	for _, m := range markets {
		if strings.EqualFold(m, market) {
			return true
		}
	}
	return false
}

// Fetch implements Provider.
func (p *BinanceProvider) Fetch(ctx context.Context, req FetchRequest) ([]Bar, error) {
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
	httpReq.Header.Set("Accept", "application/json")

	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("binance: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest {
		// Binance returns 400 on unknown symbol; treat as no-data.
		return nil, ErrNoData
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("binance: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("binance: read: %w", err)
	}
	return parseBinanceKlines(body, req.LookbackN)
}

func (p *BinanceProvider) endpoint(req FetchRequest) (string, error) {
	base := p.BaseURL
	if strings.TrimSpace(base) == "" {
		base = "https://api.binance.com"
	}
	base = strings.TrimRight(base, "/")
	u, err := url.Parse(base + "/api/v3/klines")
	if err != nil {
		return "", fmt.Errorf("binance: build url: %w", err)
	}
	q := u.Query()
	q.Set("symbol", normalizeBinanceSymbol(req.Symbol))
	q.Set("interval", binanceIntervalString(req.Interval))
	limit := req.LookbackN
	if limit <= 0 {
		limit = 120
	}
	if limit > 1000 {
		limit = 1000
	}
	q.Set("limit", strconv.Itoa(limit))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// normalizeBinanceSymbol strips the separators we let users type
// ("BTC-USD", "BTC/USDT"). It does NOT try to remap USD -> USDT —
// that's a quote-currency decision the fund's universe config makes,
// not a data-layer concern.
func normalizeBinanceSymbol(symbol string) string {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "/", "")
	s = strings.ReplaceAll(s, ":", "")
	return s
}

// binanceIntervalString maps our canonical Interval to Binance's
// vendor-specific spellings. Binance uses "1d", "1h", "1m" — the
// only mismatch is our "1w" which Binance writes as "1w" too. The
// switch is exhaustive against our canonical values.
func binanceIntervalString(i Interval) string {
	switch i {
	case Interval1m:
		return "1m"
	case Interval5m:
		return "5m"
	case Interval15m:
		return "15m"
	case Interval1h:
		return "1h"
	case Interval1w:
		return "1w"
	default:
		return "1d"
	}
}

// parseBinanceKlines decodes the JSON-array-of-arrays into Bars.
// Each kline row is a 12-tuple: [openTime, open, high, low, close,
// volume, closeTime, ...]. We only consume the first six positions
// and the close time for the bar timestamp (Binance's openTime is
// already the bar's start; we stick with that to align with Yahoo's
// "bar starts at" semantics).
func parseBinanceKlines(body []byte, keep int) ([]Bar, error) {
	var raw [][]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("binance: decode: %w", err)
	}
	bars := make([]Bar, 0, len(raw))
	for _, row := range raw {
		if len(row) < 6 {
			continue
		}
		openTime, ok := numericAsMillis(row[0])
		if !ok {
			continue
		}
		open, ok := numericAsFloat(row[1])
		if !ok {
			continue
		}
		high, ok := numericAsFloat(row[2])
		if !ok {
			continue
		}
		low, ok := numericAsFloat(row[3])
		if !ok {
			continue
		}
		closePx, ok := numericAsFloat(row[4])
		if !ok {
			continue
		}
		volume, _ := numericAsFloat(row[5])
		bars = append(bars, Bar{
			Time:   time.UnixMilli(openTime).UTC(),
			Open:   open,
			High:   high,
			Low:    low,
			Close:  closePx,
			Volume: volume,
		})
	}
	if len(bars) == 0 {
		return nil, ErrNoData
	}
	return trimToLast(bars, keep), nil
}

// numericAsFloat handles Binance's quirk: numeric fields come back
// either as JSON numbers or as JSON strings depending on the field.
// We accept either to be robust against future schema drift.
func numericAsFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// numericAsMillis is numericAsFloat for integer-typed millisecond
// epochs. Returning int64 directly keeps the rounding behaviour
// well-defined.
func numericAsMillis(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case json.Number:
		i, err := t.Int64()
		return i, err == nil
	case string:
		i, err := strconv.ParseInt(t, 10, 64)
		return i, err == nil
	default:
		return 0, false
	}
}
