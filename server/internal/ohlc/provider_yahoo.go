package ohlc

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

// YahooProvider fetches daily/intraday bars from Yahoo Finance's
// chart endpoint:
//
//	https://query1.finance.yahoo.com/v8/finance/chart/{symbol}?interval=1d&range=6mo
//
// Yahoo is the cheapest path for US / HK / cross-listed equities
// because it is key-less and globally available. Rate limits aren't
// published; we set Timeout to 8s and rely on the Cache wrapper
// (15-minute TTL in production) to keep upstream load nominal.
type YahooProvider struct {
	HTTPClient *http.Client
	// BaseURL lets tests point at an httptest server. Empty falls
	// back to the public host.
	BaseURL string
	// Markets is the list of canonical market tags this provider
	// claims to cover. Defaults to {"us_equity", "hk_equity",
	// "a_share"} when empty (Yahoo's Chart endpoint serves index
	// data for the major A-share benchmarks via the .SS / .SZ
	// suffixes — see Supports for details). Operators can override
	// to restrict (e.g., "us_equity" only when a dedicated
	// Akshare-MCP is wired).
	Markets []string
}

// Name implements Provider.
func (p *YahooProvider) Name() string { return "yahoo" }

// Supports implements Provider.
func (p *YahooProvider) Supports(market string) bool {
	markets := p.Markets
	if len(markets) == 0 {
		// Yahoo's Chart endpoint serves index data for A-shares
		// (000300.SS / 000905.SS / 399006.SZ / 000688.SS) and HK
		// (^HSI / ^HSCE) in addition to its native US coverage,
		// so we claim all three by default. Operators with a
		// dedicated Akshare-MCP for A-share STOCKS should still
		// register that provider FIRST in cmd/server wiring so
		// the registry tries it before falling through to Yahoo
		// (Akshare has more reliable individual-stock coverage
		// than Yahoo for the A-share market).
		markets = []string{"us_equity", "hk_equity", "a_share"}
	}
	for _, m := range markets {
		if strings.EqualFold(m, market) {
			return true
		}
	}
	return false
}

// Fetch implements Provider. Returns oldest-first bars or ErrNoData.
func (p *YahooProvider) Fetch(ctx context.Context, req FetchRequest) ([]Bar, error) {
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
	// Yahoo returns 403 to default Go UA; mimic a desktop browser.
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	httpReq.Header.Set("Accept", "application/json")

	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("yahoo: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoData
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("yahoo: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("yahoo: read: %w", err)
	}
	return parseYahooChart(body, req.LookbackN)
}

func (p *YahooProvider) endpoint(req FetchRequest) (string, error) {
	base := p.BaseURL
	if strings.TrimSpace(base) == "" {
		base = "https://query1.finance.yahoo.com"
	}
	base = strings.TrimRight(base, "/")
	u, err := url.Parse(base + "/v8/finance/chart/" + url.PathEscape(req.Symbol))
	if err != nil {
		return "", fmt.Errorf("yahoo: build url: %w", err)
	}
	q := u.Query()
	q.Set("interval", yahooIntervalString(req.Interval))
	q.Set("range", yahooRangeForLookback(req.Interval, req.LookbackN))
	q.Set("includePrePost", "false")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// yahooIntervalString maps our canonical Interval to Yahoo's
// vendor-specific strings. The two systems agree on "1d", differ on
// intraday: Yahoo accepts "1m", "5m", "15m", "60m", "1d", "1wk".
func yahooIntervalString(i Interval) string {
	switch i {
	case Interval1m:
		return "1m"
	case Interval5m:
		return "5m"
	case Interval15m:
		return "15m"
	case Interval1h:
		return "60m"
	case Interval1w:
		return "1wk"
	default:
		return "1d"
	}
}

// yahooRangeForLookback picks the smallest Yahoo range that covers
// LookbackN bars at the requested interval, with a buffer so weekend
// gaps don't truncate our window.
func yahooRangeForLookback(i Interval, n int) string {
	switch i {
	case Interval1m, Interval5m, Interval15m:
		// Yahoo limits intraday history. "1mo" is the max for 15m.
		return "1mo"
	case Interval1h:
		return "3mo"
	case Interval1w:
		return "2y"
	default:
		// Daily — 250 bars covers a year of trading days. Round up.
		// Note: Yahoo silently down-samples to monthly bars when
		// the range is "max" + interval=1d, so we cap at "10y" to
		// preserve the daily cadence consumers expect. Callers
		// needing >10y should slice the request into windows.
		switch {
		case n <= 30:
			return "3mo"
		case n <= 80:
			return "6mo"
		case n <= 200:
			return "1y"
		case n <= 400:
			return "2y"
		case n <= 1300:
			return "5y"
		default:
			return "10y"
		}
	}
}

// parseYahooChart consumes the Yahoo chart JSON and returns up to
// keep bars in oldest-first order. Missing-data points (vendor uses
// nulls when a session was halted) are skipped to keep the indicator
// math well-behaved.
func parseYahooChart(body []byte, keep int) ([]Bar, error) {
	var dto struct {
		Chart struct {
			Result []struct {
				Timestamp  []int64 `json:"timestamp"`
				Indicators struct {
					Quote []struct {
						Open   []*float64 `json:"open"`
						High   []*float64 `json:"high"`
						Low    []*float64 `json:"low"`
						Close  []*float64 `json:"close"`
						Volume []*float64 `json:"volume"`
					} `json:"quote"`
				} `json:"indicators"`
			} `json:"result"`
			Error any `json:"error"`
		} `json:"chart"`
	}
	if err := json.Unmarshal(body, &dto); err != nil {
		return nil, fmt.Errorf("yahoo: decode: %w", err)
	}
	if len(dto.Chart.Result) == 0 || len(dto.Chart.Result[0].Indicators.Quote) == 0 {
		return nil, ErrNoData
	}
	res := dto.Chart.Result[0]
	quote := res.Indicators.Quote[0]
	bars := make([]Bar, 0, len(res.Timestamp))
	for i, ts := range res.Timestamp {
		if i >= len(quote.Close) {
			break
		}
		if quote.Close[i] == nil || quote.Open[i] == nil || quote.High[i] == nil || quote.Low[i] == nil {
			continue
		}
		var volume float64
		if i < len(quote.Volume) && quote.Volume[i] != nil {
			volume = *quote.Volume[i]
		}
		bars = append(bars, Bar{
			Time:   time.Unix(ts, 0).UTC(),
			Open:   *quote.Open[i],
			High:   *quote.High[i],
			Low:    *quote.Low[i],
			Close:  *quote.Close[i],
			Volume: volume,
		})
	}
	if len(bars) == 0 {
		return nil, ErrNoData
	}
	return trimToLast(bars, keep), nil
}

// trimToLast keeps the last n bars (oldest-first input → oldest-first
// output). Returning a fresh slice avoids holding a reference to the
// caller-supplied backing array.
func trimToLast(bars []Bar, n int) []Bar {
	if n <= 0 || len(bars) <= n {
		out := make([]Bar, len(bars))
		copy(out, bars)
		return out
	}
	out := make([]Bar, n)
	copy(out, bars[len(bars)-n:])
	return out
}
