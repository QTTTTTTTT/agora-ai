package corpaction

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

// YahooProvider pulls split + cash-dividend events from Yahoo
// Finance's chart endpoint:
//
//	https://query1.finance.yahoo.com/v8/finance/chart/{symbol}?events=div|split&period1=...&period2=...
//
// The events block of the response is an object keyed by epoch
// timestamp; we flatten it into Event slices the applier can
// consume directly.
//
// # Why chart vs quoteSummary
//
// quoteSummary's calendarEvents.dividendDate gives the NEXT
// announced dividend but no historical splits. The chart endpoint
// returns the full event ledger between period1 and period2 in a
// single round-trip and is the canonical adjustment source for
// every charting library (yfinance, yahoo-finance2, etc.). It is
// also keyless — no crumb dance required — so we don't need to
// share auth state with the fundamental provider.
//
// # Coverage
//
// US (NASDAQ, NYSE, AMEX), HK (.HK suffix), LSE, ASX. Mainland
// A-shares are NOT covered — Yahoo's CN data is sparse and
// notoriously broken for splits. A-share corp actions need a
// separate Tushare/Tencent provider, which is sprint-N work.
type YahooProvider struct {
	HTTPClient *http.Client
	BaseURL    string // override for tests; defaults to query1.finance.yahoo.com
}

// FetchEvents returns every dividend and split between `since` and
// now for the given Yahoo-formatted symbol (e.g. "NVDA", "BABA",
// "0700.HK"). Events on or after `since` are included.
//
// The returned slice is sorted by ex-date ascending so callers can
// apply them in order. Multiple events on the same ex-date are
// returned individually — a same-day stock_dividend + cash_dividend
// will surface as two Event rows with `combined` semantics handled
// at insert time.
func (p *YahooProvider) FetchEvents(ctx context.Context, symbol string, since time.Time) ([]Event, error) {
	if strings.TrimSpace(symbol) == "" {
		return nil, fmt.Errorf("yahoo corpaction: empty symbol")
	}
	endpoint, err := p.endpoint(symbol, since)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	// Yahoo blocks the default Go user-agent on the chart route;
	// we present a regular browser UA. Anything containing
	// "Mozilla/5.0" works.
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (compatible; FundAI/1.0)")
	httpReq.Header.Set("Accept", "application/json")

	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("yahoo corpaction: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// Yahoo replies 404 for unlisted tickers; treat as "no
		// events" so the daily sweep doesn't error out for a
		// single bad symbol.
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("yahoo corpaction: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("yahoo corpaction: read: %w", err)
	}
	return parseChartEvents(body, symbol)
}

func (p *YahooProvider) endpoint(symbol string, since time.Time) (string, error) {
	base := p.BaseURL
	if base == "" {
		base = "https://query1.finance.yahoo.com"
	}
	if since.IsZero() {
		// Default: pull 5 years of history. Splits older than
		// that are usually already reflected in our cost basis
		// because positions are at most a few months old.
		since = time.Now().AddDate(-5, 0, 0)
	}
	u, err := url.Parse(base + "/v8/finance/chart/" + url.PathEscape(symbol))
	if err != nil {
		return "", fmt.Errorf("yahoo corpaction: bad base url: %w", err)
	}
	q := u.Query()
	q.Set("events", "div|split")
	q.Set("interval", "1d")
	q.Set("period1", strconv.FormatInt(since.Unix(), 10))
	q.Set("period2", strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10))
	q.Set("includeAdjustedClose", "true")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// parseChartEvents converts Yahoo's nested events object into a
// flat sorted Event slice. Exposed at package scope so the test
// suite can drive it from a fixture file without spinning up an
// httptest server.
func parseChartEvents(body []byte, symbol string) ([]Event, error) {
	var raw struct {
		Chart struct {
			Result []struct {
				Events struct {
					Dividends map[string]struct {
						Amount float64 `json:"amount"`
						Date   int64   `json:"date"`
					} `json:"dividends"`
					Splits map[string]struct {
						Date        int64   `json:"date"`
						Numerator   float64 `json:"numerator"`
						Denominator float64 `json:"denominator"`
						SplitRatio  string  `json:"splitRatio"`
					} `json:"splits"`
				} `json:"events"`
			} `json:"result"`
			Error *struct {
				Code        string `json:"code"`
				Description string `json:"description"`
			} `json:"error"`
		} `json:"chart"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("yahoo corpaction: decode: %w", err)
	}
	if raw.Chart.Error != nil {
		return nil, fmt.Errorf("yahoo corpaction: api error %s: %s",
			raw.Chart.Error.Code, raw.Chart.Error.Description)
	}
	if len(raw.Chart.Result) == 0 {
		return nil, nil
	}
	events := raw.Chart.Result[0].Events

	out := make([]Event, 0, len(events.Splits)+len(events.Dividends))

	for _, s := range events.Splits {
		ratio, err := splitRatioToFloat(s.Numerator, s.Denominator, s.SplitRatio)
		if err != nil {
			// Skip a single malformed event rather than failing
			// the whole batch — bad data is common in Yahoo's
			// older history and we'd rather log + move on.
			continue
		}
		out = append(out, Event{
			InstrumentKey: instrumentKeyForYahoo(symbol),
			ExDate:        time.Unix(s.Date, 0).UTC().Truncate(24 * time.Hour),
			ActionType:    "split",
			SplitRatio:    ratio,
			CashDividend:  0,
			Source:        "yahoo",
		})
	}

	for _, d := range events.Dividends {
		if d.Amount <= 0 {
			continue
		}
		out = append(out, Event{
			InstrumentKey: instrumentKeyForYahoo(symbol),
			ExDate:        time.Unix(d.Date, 0).UTC().Truncate(24 * time.Hour),
			ActionType:    "cash_dividend",
			SplitRatio:    1.0,
			CashDividend:  d.Amount,
			Source:        "yahoo",
		})
	}

	// Stable order so the caller's "is this newer than my last
	// recorded event?" check is deterministic.
	sortEventsByExDate(out)
	return out, nil
}

// splitRatioToFloat extracts the new/old multiplier from one of
// Yahoo's three encodings:
//
//   - numerator/denominator floats (e.g. 10, 1 → 10.0).
//   - "10:1" string (parsed if the floats are missing/zero).
//
// A 10:1 forward split returns 10.0; a 1:5 reverse split returns 0.2.
func splitRatioToFloat(numerator, denominator float64, ratio string) (float64, error) {
	if numerator > 0 && denominator > 0 {
		return numerator / denominator, nil
	}
	parts := strings.SplitN(ratio, ":", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("malformed splitRatio %q", ratio)
	}
	num, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	den, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err1 != nil || err2 != nil || den == 0 {
		return 0, fmt.Errorf("unparseable splitRatio %q", ratio)
	}
	return num / den, nil
}

// instrumentKeyForYahoo prepends the exchange prefix our schema
// uses ("NASDAQ:NVDA"). Yahoo symbols are exchange-suffix encoded
// (no suffix → US, ".HK" → Hong Kong, etc.) — for sprint-N we
// only handle the bare US case here. HK / SS / SZ ingestion will
// extend this with a proper suffix-to-exchange map.
func instrumentKeyForYahoo(symbol string) string {
	upper := strings.ToUpper(strings.TrimSpace(symbol))
	switch {
	case strings.HasSuffix(upper, ".HK"):
		return "HKEX:" + strings.TrimSuffix(upper, ".HK")
	case strings.HasSuffix(upper, ".SS"):
		return "SSE:" + strings.TrimSuffix(upper, ".SS")
	case strings.HasSuffix(upper, ".SZ"):
		return "SZSE:" + strings.TrimSuffix(upper, ".SZ")
	default:
		// US listings are ambiguous between NASDAQ/NYSE without
		// a separate lookup; the daily sweep will resolve the
		// canonical instrument_key by joining against
		// holding_positions on the bare symbol when needed.
		return "NASDAQ:" + upper
	}
}

func sortEventsByExDate(events []Event) {
	for i := 1; i < len(events); i++ {
		for j := i; j > 0 && events[j-1].ExDate.After(events[j].ExDate); j-- {
			events[j-1], events[j] = events[j], events[j-1]
		}
	}
}
