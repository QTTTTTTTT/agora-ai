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

// AkshareProvider talks to a self-hosted akshare-MCP HTTP service —
// the same pattern marketdata.Service uses for quote fetching. The
// MCP server wraps the Python akshare library and exposes simple
// REST endpoints; we try a small list of candidate paths because
// different forks of akshare-mcp use slightly different routes
// (mirroring the quote-fetching tolerance in marketdata.go).
//
// When BaseURL is empty the provider declines to handle any request
// (Supports returns false-equivalent via the empty-symbol guard in
// the registry); callers that care about A-share OHLC must wire
// AKSHARE_OHLC_URL or pass BaseURL explicitly.
type AkshareProvider struct {
	HTTPClient *http.Client
	// BaseURL is the akshare-MCP root URL. Empty disables the
	// provider entirely (Fetch returns ErrNoData).
	BaseURL string
	// Markets defaults to {"a_share", "futures"} when empty. The
	// futures coverage assumes the upstream MCP also exposes the
	// `futures_zh_daily_sina` endpoint or equivalent; operators
	// running an A-share-only MCP should set Markets explicitly.
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
		markets = []string{"a_share", "futures"}
	}
	for _, m := range markets {
		if strings.EqualFold(m, market) {
			return true
		}
	}
	return false
}

// Fetch tries each candidate endpoint and returns the first that
// produces non-empty bars. Differences in the MCP fork's routing
// (kline vs ohlc vs history) are absorbed here so the registry
// stays uniform.
func (p *AkshareProvider) Fetch(ctx context.Context, req FetchRequest) ([]Bar, error) {
	req = req.Normalize()
	if strings.TrimSpace(p.BaseURL) == "" {
		return nil, ErrNoData
	}
	if req.Symbol == "" {
		return nil, ErrNoData
	}
	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}

	candidates := []string{"/api/kline", "/kline", "/ohlc", "/history", "/api/history"}
	var lastErr error
	for _, path := range candidates {
		endpoint, err := p.endpoint(path, req)
		if err != nil {
			lastErr = err
			continue
		}
		bars, err := p.fetchOne(ctx, client, endpoint, req.LookbackN)
		if err == nil && len(bars) > 0 {
			return bars, nil
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
	base := strings.TrimRight(p.BaseURL, "/")
	u, err := url.Parse(base + path)
	if err != nil {
		return "", fmt.Errorf("akshare: build url: %w", err)
	}
	q := u.Query()
	q.Set("symbol", strings.ToUpper(strings.TrimSpace(req.Symbol)))
	q.Set("market", req.Market)
	q.Set("interval", akshareIntervalString(req.Interval))
	q.Set("period", akshareIntervalString(req.Interval))
	q.Set("limit", strconv.Itoa(req.LookbackN))
	if !req.EndTime.IsZero() {
		q.Set("end_date", req.EndTime.UTC().Format("20060102"))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (p *AkshareProvider) fetchOne(ctx context.Context, client *http.Client, endpoint string, keep int) ([]Bar, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("akshare: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoData
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("akshare: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("akshare: read: %w", err)
	}
	return parseAkshareBars(body, keep)
}

// akshareIntervalString maps to akshare's vendor strings. akshare
// uses "daily" / "weekly" / "60" (minutes) / "30" etc. across
// different stock APIs; "daily" works in the most common
// `stock_zh_a_hist` endpoint and "60", "30", "15" work for the
// intraday endpoints. We always send both `interval` and `period`
// because different MCP forks accept different keys.
func akshareIntervalString(i Interval) string {
	switch i {
	case Interval1m:
		return "1"
	case Interval5m:
		return "5"
	case Interval15m:
		return "15"
	case Interval1h:
		return "60"
	case Interval1w:
		return "weekly"
	default:
		return "daily"
	}
}

// parseAkshareBars handles two response shapes a MCP fork may use:
//
//  1. Array of objects:
//     [{"date":"2026-05-19","open":12.3,"high":12.5,"low":12.1,"close":12.4,"volume":42000}, ...]
//
//  2. Wrapped object:
//     {"data":[ ... same as above ... ], "code":0}
//
// Field name aliases ("date" / "datetime" / "trade_date";
// "open"/"o", etc.) are tolerated so the provider keeps working
// across akshare versions. Rows with non-positive close are skipped
// because the indicator math is undefined on them.
func parseAkshareBars(body []byte, keep int) ([]Bar, error) {
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return nil, ErrNoData
	}

	var rows []map[string]any
	if body[0] == '[' {
		if err := json.Unmarshal(body, &rows); err != nil {
			return nil, fmt.Errorf("akshare: decode array: %w", err)
		}
	} else {
		var wrapped struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(body, &wrapped); err != nil {
			return nil, fmt.Errorf("akshare: decode object: %w", err)
		}
		rows = wrapped.Data
	}
	if len(rows) == 0 {
		return nil, ErrNoData
	}

	bars := make([]Bar, 0, len(rows))
	for _, row := range rows {
		closePx, ok := lookupFloat(row, "close", "c", "Close")
		if !ok || closePx <= 0 {
			continue
		}
		open, _ := lookupFloat(row, "open", "o", "Open")
		high, _ := lookupFloat(row, "high", "h", "High")
		low, _ := lookupFloat(row, "low", "l", "Low")
		volume, _ := lookupFloat(row, "volume", "v", "vol", "Volume")
		ts := lookupTime(row, "date", "datetime", "trade_date", "time", "timestamp")
		bars = append(bars, Bar{
			Time:   ts,
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

func lookupFloat(row map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		v, ok := row[key]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case float64:
			return t, true
		case json.Number:
			f, err := t.Float64()
			if err == nil {
				return f, true
			}
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

func lookupTime(row map[string]any, keys ...string) time.Time {
	for _, key := range keys {
		v, ok := row[key]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case string:
			for _, layout := range []string{"2006-01-02", "20060102", time.RFC3339, "2006-01-02 15:04:05"} {
				if ts, err := time.Parse(layout, t); err == nil {
					return ts.UTC()
				}
			}
		case float64:
			// Heuristic: > 1e12 is ms epoch, < 1e10 is s epoch.
			if t > 1e12 {
				return time.UnixMilli(int64(t)).UTC()
			}
			return time.Unix(int64(t), 0).UTC()
		}
	}
	return time.Time{}
}
