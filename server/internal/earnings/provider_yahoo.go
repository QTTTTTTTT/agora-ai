package earnings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// YahooProvider hits Yahoo Finance's keyless v10/quoteSummary
// endpoint to discover the next scheduled earnings date for a
// given symbol. Cheapest practical earnings source: zero auth,
// JSON response, predictable shape.
//
// Endpoint:
//
//	GET https://query2.finance.yahoo.com/v10/finance/quoteSummary/{SYM}
//	    ?modules=calendarEvents&corsDomain=finance.yahoo.com
//	    &formatted=false&lang=en-US
//
// Response (truncated to the shape we read):
//
//	{
//	  "quoteSummary": {
//	    "result": [{
//	      "calendarEvents": {
//	        "earnings": {
//	          "earningsDate": [1722384000, 1722470400],
//	          "isEarningsDateEstimate": true
//	        }
//	      }
//	    }]
//	  }
//	}
//
// `earningsDate` is a Unix-second array: 1 element when confirmed,
// 2 elements when a date range (Yahoo's estimate window). We take
// the earliest because the prompt cares most about "the soonest
// catalyst could hit".
//
// Quirks worth knowing:
//   - Yahoo's v10 endpoint is keyless but it WILL 401 / 429 if you
//     send a default Go User-Agent. We send a real browser UA.
//   - The "BMO / AMC" tag is NOT exposed by quoteSummary; we
//     therefore set TimeOfDay=TimeUnknown and let the prompt rules
//     handle it. (A future upgrade can scrape the HTML calendar
//     page to recover the tag.)
//   - Yahoo is best for US tickers. A-share / HK tickers route
//     through YahooSymbol (.SS / .HK suffix); CN ticker symbols
//     without a market disambiguator are skipped.
//
// Concurrency: per-symbol fetches run through a small worker pool
// (default 3) to keep latency low without tripping the rate
// limiter. A nil per-symbol result is fine — we accumulate
// whatever succeeded and let the upstream Service deal with an
// empty slice.
type YahooProvider struct {
	// BaseURL overrides the default Yahoo endpoint. Tests point
	// it at httptest.Server; production leaves it empty.
	BaseURL string
	// HTTPClient is the HTTP client used for outbound calls.
	// When nil a sensible default with a 4s per-call timeout is
	// constructed lazily.
	HTTPClient *http.Client
	// Concurrency caps the in-flight per-symbol fetch count.
	// 0 → default of 3 (low enough to stay polite, high enough
	// that 7-symbol universes finish in ~1 RTT).
	Concurrency int
	// UserAgent overrides the browser UA. Empty → real-browser
	// default; Yahoo silently rejects bare Go-http-client UAs.
	UserAgent string

	once       sync.Once
	httpClient *http.Client
}

const defaultYahooEarningsBaseURL = "https://query2.finance.yahoo.com"

const defaultYahooUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

// Fetch implements the earnings.Fetcher interface. Returns the
// per-symbol earnings events the provider could resolve; symbols
// that returned errors are SKIPPED (logged at debug), not
// surfaced as an error — the upstream service treats an empty
// slice as "no signal" which is exactly what we want when one
// symbol fails to resolve while the rest succeed.
func (p *YahooProvider) Fetch(ctx context.Context, req FetchRequest) ([]Event, error) {
	if p == nil {
		return nil, nil
	}
	p.lazyInit()
	symbols := req.Symbols
	if len(symbols) == 0 {
		return nil, nil
	}
	concurrency := p.Concurrency
	if concurrency <= 0 {
		concurrency = 3
	}
	if concurrency > len(symbols) {
		concurrency = len(symbols)
	}

	type result struct {
		event *Event
		err   error
		sym   string
	}
	in := make(chan string, len(symbols))
	out := make(chan result, len(symbols))
	for _, s := range symbols {
		in <- s
	}
	close(in)

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sym := range in {
				ev, err := p.fetchOne(ctx, sym, req.Market)
				out <- result{event: ev, err: err, sym: sym}
			}
		}()
	}
	wg.Wait()
	close(out)

	events := make([]Event, 0, len(symbols))
	for r := range out {
		if r.err != nil {
			// Soft failure. Throttling and "Symbol not found"
			// are both common; logging at warn would spam,
			// so debug is right.
			slog.Debug("earnings yahoo: per-symbol fetch failed",
				slog.String("symbol", r.sym),
				slog.String("err", r.err.Error()))
			continue
		}
		if r.event != nil {
			events = append(events, *r.event)
		}
	}
	return events, nil
}

// fetchOne resolves the next earnings event for a single symbol.
// Returns (nil, nil) when the symbol has no upcoming earnings
// inside the calendar window (e.g. ETFs, post-print quiet
// period); (nil, err) for hard failures we want logged.
func (p *YahooProvider) fetchOne(ctx context.Context, symbol, market string) (*Event, error) {
	if symbol == "" {
		return nil, nil
	}
	mappedSymbol := mapToYahooSymbol(symbol, market)
	if mappedSymbol == "" {
		// Best-effort gating: A-share or unknown-market symbols
		// without a Yahoo-style suffix can't be resolved by
		// this provider. Soft-skip.
		return nil, nil
	}
	base := strings.TrimRight(p.BaseURL, "/")
	if base == "" {
		base = defaultYahooEarningsBaseURL
	}
	endpoint, err := url.Parse(base + "/v10/finance/quoteSummary/" + url.PathEscape(mappedSymbol))
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	q := endpoint.Query()
	q.Set("modules", "calendarEvents")
	q.Set("corsDomain", "finance.yahoo.com")
	q.Set("formatted", "false")
	q.Set("lang", "en-US")
	endpoint.RawQuery = q.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	ua := p.UserAgent
	if ua == "" {
		ua = defaultYahooUserAgent
	}
	httpReq.Header.Set("User-Agent", ua)
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// "Symbol not found" is normal for ETFs / index symbols.
		return nil, nil
	}
	if resp.StatusCode == http.StatusUnauthorized ||
		resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("yahoo throttled: http %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("yahoo: http %d", resp.StatusCode)
	}

	var payload yahooQuoteSummaryResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if payload.QuoteSummary.Error != nil {
		return nil, fmt.Errorf("yahoo: %v", payload.QuoteSummary.Error)
	}
	if len(payload.QuoteSummary.Result) == 0 {
		return nil, nil
	}
	cal := payload.QuoteSummary.Result[0].CalendarEvents
	if cal == nil || cal.Earnings == nil {
		return nil, nil
	}
	earnings := cal.Earnings
	earliest, err := earliestEarningsDate(earnings)
	if err != nil {
		return nil, err
	}
	if earliest.IsZero() {
		return nil, nil
	}
	ev := Event{
		Symbol:    symbol,
		Market:    strings.ToLower(strings.TrimSpace(market)),
		EventDate: earliest,
		// quoteSummary v10 doesn't expose BMO/AMC. The prompt
		// tolerates "unknown" — it just doesn't shade the
		// daysUntil+1 math.
		TimeOfDay: TimeUnknown,
		Source:    "yahoo",
	}
	return &ev, nil
}

// earliestEarningsDate picks the first valid epoch second from the
// `earningsDate` array. Yahoo's API has multiple historical
// shapes for this field — formatted=false returns raw int64
// seconds, formatted=true wraps each entry in a {raw, fmt}
// object. We tolerate both, with the bare-int path preferred
// because we request formatted=false explicitly.
func earliestEarningsDate(e *yahooEarningsBlock) (time.Time, error) {
	if e == nil || len(e.EarningsDate) == 0 {
		return time.Time{}, nil
	}
	for _, raw := range e.EarningsDate {
		ts := raw.epoch()
		if ts > 0 {
			return time.Unix(ts, 0).UTC(), nil
		}
	}
	return time.Time{}, errors.New("earningsDate present but no valid epoch found")
}

// mapToYahooSymbol translates an upper-cased fund-side ticker into
// the Yahoo-compatible symbol. Rules:
//   - US tickers pass through unchanged (AAPL → AAPL).
//   - A-share six-digit codes get `.SS` (Shanghai) or `.SZ`
//     (Shenzhen) based on the leading digit, IF market hints
//     suggest A-share. Without a market hint we punt (Yahoo
//     symbols for A-share aren't reliably resolvable without
//     the suffix).
//   - HK five-digit codes get `.HK`.
//   - Anything else with a market suffix is left untouched.
func mapToYahooSymbol(symbol, market string) string {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if sym == "" {
		return ""
	}
	if strings.ContainsRune(sym, '.') {
		// Already a market-suffixed symbol (e.g. 600519.SS).
		return sym
	}
	market = strings.ToLower(strings.TrimSpace(market))
	switch {
	case market == "us_equity" || market == "us" || market == "":
		// Most US tickers. Empty-market is the lib default;
		// we treat as US since this provider is US-first.
		// All-digit symbols are NEVER US (NYSE/NASDAQ tickers
		// are alphabetic) — they're almost always unmarked
		// A-share / HK codes. Punt so we don't waste a Yahoo
		// call on a guaranteed 404.
		if isAllDigits(sym) {
			return ""
		}
		return sym
	case market == "a_share" || market == "cn":
		if len(sym) == 6 && isAllDigits(sym) {
			switch sym[0] {
			case '6':
				return sym + ".SS"
			case '0', '3':
				return sym + ".SZ"
			}
		}
		return ""
	case market == "hk":
		if len(sym) <= 5 && isAllDigits(sym) {
			return strings.TrimLeft(sym, "0") + ".HK"
		}
		return sym
	default:
		return sym
	}
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// lazyInit ensures the http client exists. We avoid wiring it in
// NewYahooProvider so callers can use the zero value (which is
// what wiring_adapters.go does when constructing the provider
// from env defaults).
func (p *YahooProvider) lazyInit() {
	p.once.Do(func() {
		if p.HTTPClient == nil {
			p.httpClient = &http.Client{
				Timeout: 4 * time.Second,
			}
		} else {
			p.httpClient = p.HTTPClient
		}
	})
}

// ---------------------------------------------------------------------------
// Internal payload types
// ---------------------------------------------------------------------------

type yahooQuoteSummaryResponse struct {
	QuoteSummary struct {
		Result []struct {
			CalendarEvents *yahooCalendarEventsBlock `json:"calendarEvents"`
		} `json:"result"`
		Error any `json:"error"`
	} `json:"quoteSummary"`
}

type yahooCalendarEventsBlock struct {
	Earnings *yahooEarningsBlock `json:"earnings"`
}

type yahooEarningsBlock struct {
	EarningsDate           []yahooNumericOrFormatted `json:"earningsDate"`
	IsEarningsDateEstimate bool                      `json:"isEarningsDateEstimate"`
}

// yahooNumericOrFormatted tolerates both shapes Yahoo serves:
//   - bare int64 (when formatted=false is honored)
//   - {raw, fmt} object (some endpoints / clients regress to
//     this even when formatted=false is requested)
type yahooNumericOrFormatted struct {
	raw int64
}

func (y *yahooNumericOrFormatted) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	// Bare number path.
	var num int64
	if err := json.Unmarshal(b, &num); err == nil {
		y.raw = num
		return nil
	}
	// Object path.
	var obj struct {
		Raw int64 `json:"raw"`
	}
	if err := json.Unmarshal(b, &obj); err == nil {
		y.raw = obj.Raw
		return nil
	}
	return fmt.Errorf("earnings: unexpected earningsDate element shape: %s", string(b))
}

func (y yahooNumericOrFormatted) epoch() int64 { return y.raw }
