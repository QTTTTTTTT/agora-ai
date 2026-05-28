package earnings

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// YahooHistoryProvider hits Yahoo Finance's keyless
// v10/quoteSummary endpoint with the earningsHistory module to
// pull the trailing 4 quarters of EPS actual + estimate +
// surprise%. Sister to YahooProvider above; same UA / concurrency
// / soft-failure contract.
//
// Endpoint:
//
//	GET https://query2.finance.yahoo.com/v10/finance/quoteSummary/{SYM}
//	    ?modules=earningsHistory&corsDomain=finance.yahoo.com
//	    &formatted=false&lang=en-US
//
// Response (truncated):
//
//	{
//	  "quoteSummary": {
//	    "result": [{
//	      "earningsHistory": {
//	        "history": [
//	          {
//	            "epsActual": 1.62, "epsEstimate": 1.49,
//	            "epsDifference": 0.13, "surprisePercent": 0.087,
//	            "quarter": 1722384000, "period": "-1q"
//	          },
//	          ...
//	        ]
//	      }
//	    }]
//	  }
//	}
//
// Quirks:
//   - `surprisePercent` is reported as a DECIMAL fraction (0.087 =
//     8.7%) in some payloads and as a literal percent (8.7 = 8.7%)
//     in others — Yahoo flips this depending on the cache layer.
//     The PEAD service tolerates both by computing surprise from
//     epsActual / epsEstimate when |surprisePercent| > 1 (i.e.
//     looks like literal percent) and keeping the field otherwise.
//   - `quarter` is the period END (not the announcement date) for
//     a few names; close enough for PEAD's day-granularity drift
//     window. The forward-calendar `Snapshot` uses earningsDate
//     for actual release timing; here we only need a coarse
//     "when did this print roughly land" anchor.
//   - We request the LAST 8 quarters (Yahoo caps at 4 anyway) so
//     the upstream filter inside HistoryService picks the most
//     recent in-window event per symbol.
type YahooHistoryProvider struct {
	BaseURL     string
	HTTPClient  *http.Client
	Concurrency int
	UserAgent   string

	once       sync.Once
	httpClient *http.Client
}

// FetchHistory implements HistoryFetcher.
func (p *YahooHistoryProvider) FetchHistory(ctx context.Context, req HistoryRequest) ([]HistoricalEvent, error) {
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
		events []HistoricalEvent
		err    error
		sym    string
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
				events, err := p.fetchOne(ctx, sym, req.Market)
				out <- result{events: events, err: err, sym: sym}
			}
		}()
	}
	wg.Wait()
	close(out)

	combined := make([]HistoricalEvent, 0, len(symbols)*4)
	for r := range out {
		if r.err != nil {
			slog.Debug("earnings yahoo-history: per-symbol fetch failed",
				slog.String("symbol", r.sym),
				slog.String("err", r.err.Error()))
			continue
		}
		combined = append(combined, r.events...)
	}
	return combined, nil
}

func (p *YahooHistoryProvider) fetchOne(ctx context.Context, symbol, market string) ([]HistoricalEvent, error) {
	if symbol == "" {
		return nil, nil
	}
	mappedSymbol := mapToYahooSymbol(symbol, market)
	if mappedSymbol == "" {
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
	q.Set("modules", "earningsHistory")
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
		return nil, nil
	}
	if resp.StatusCode == http.StatusUnauthorized ||
		resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("yahoo throttled: http %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("yahoo: http %d", resp.StatusCode)
	}

	var payload yahooHistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if payload.QuoteSummary.Error != nil {
		return nil, fmt.Errorf("yahoo: %v", payload.QuoteSummary.Error)
	}
	if len(payload.QuoteSummary.Result) == 0 {
		return nil, nil
	}
	hist := payload.QuoteSummary.Result[0].EarningsHistory
	if hist == nil || len(hist.History) == 0 {
		return nil, nil
	}
	out := make([]HistoricalEvent, 0, len(hist.History))
	for _, h := range hist.History {
		ts := h.Quarter.epoch()
		if ts <= 0 {
			continue
		}
		surprise := h.SurprisePercent.float()
		// Yahoo flips between decimal (0.087) and literal percent
		// (8.7) — when |surprisePercent| > 1 the value is almost
		// certainly a literal percent (real earnings surprises
		// > 100% basically never happen in well-followed names),
		// so coerce to decimal.
		if surprise > 1 || surprise < -1 {
			surprise = surprise / 100.0
		}
		// If surprisePercent missing and we have actual + estimate,
		// derive: (actual - estimate) / |estimate|.
		actual := h.EpsActual.float()
		estimate := h.EpsEstimate.float()
		if surprise == 0 && estimate != 0 {
			abs := estimate
			if abs < 0 {
				abs = -abs
			}
			if abs > 0 {
				surprise = (actual - estimate) / abs
			}
		}
		out = append(out, HistoricalEvent{
			Symbol:          symbol,
			Market:          strings.ToLower(strings.TrimSpace(market)),
			EventDate:       time.Unix(ts, 0).UTC(),
			EpsActual:       actual,
			EpsEstimate:     estimate,
			SurprisePercent: surprise,
			Source:          "yahoo",
		})
	}
	return out, nil
}

func (p *YahooHistoryProvider) lazyInit() {
	p.once.Do(func() {
		if p.HTTPClient == nil {
			p.httpClient = &http.Client{Timeout: 4 * time.Second}
		} else {
			p.httpClient = p.HTTPClient
		}
	})
}

// ---------------------------------------------------------------------------
// Internal payload types
// ---------------------------------------------------------------------------

type yahooHistoryResponse struct {
	QuoteSummary struct {
		Result []struct {
			EarningsHistory *yahooEarningsHistoryBlock `json:"earningsHistory"`
		} `json:"result"`
		Error any `json:"error"`
	} `json:"quoteSummary"`
}

type yahooEarningsHistoryBlock struct {
	History []yahooEarningsHistoryRow `json:"history"`
}

type yahooEarningsHistoryRow struct {
	EpsActual       yahooFlexibleFloat      `json:"epsActual"`
	EpsEstimate     yahooFlexibleFloat      `json:"epsEstimate"`
	SurprisePercent yahooFlexibleFloat      `json:"surprisePercent"`
	Quarter         yahooNumericOrFormatted `json:"quarter"`
}

// yahooFlexibleFloat tolerates both Yahoo response shapes for
// numeric scalars: a bare float OR a {raw, fmt} object.
type yahooFlexibleFloat struct {
	raw float64
}

func (y *yahooFlexibleFloat) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	var num float64
	if err := json.Unmarshal(b, &num); err == nil {
		y.raw = num
		return nil
	}
	var obj struct {
		Raw float64 `json:"raw"`
	}
	if err := json.Unmarshal(b, &obj); err == nil {
		y.raw = obj.Raw
		return nil
	}
	return fmt.Errorf("earnings-history: unexpected float shape: %s", string(b))
}

func (y yahooFlexibleFloat) float() float64 { return y.raw }
