// fx_yahoo.go — Yahoo-Finance backed FX provider (P1-4).
//
// Why Yahoo
//
// Yahoo's quote endpoint already powers the rest of the platform's
// market-data pulls (positions/instrument metadata) so reusing it
// keeps the rate-limit + auth surface minimal. We're not pinned
// to it — the Provider interface lets us swap to ECB / openexchangerates
// later without touching any caller.
//
// Endpoint
//
//   https://query2.finance.yahoo.com/v7/finance/quote?symbols=USDCNY=X
//
// The "USDCNY=X" symbol convention means "1 USD costs X CNY",
// matching the in-DB convention (base=USD, quote=CNY, rate=N).
//
// Caveats
//
//   - The endpoint is rate-limited per IP. The scheduler should
//     fetch a small batch at a time (we have ~10 supported pairs).
//   - Yahoo occasionally returns regular-market-price = 0 for
//     pairs that haven't traded yet today. We treat that as
//     ErrRateUnavailable, NOT a successful 0 rate.

package fx

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

// YahooProvider talks to query2.finance.yahoo.com and returns Rate
// values consumable by the Repo / scheduler.
type YahooProvider struct {
	httpClient *http.Client
	baseURL    string
	// crumb / cookie are optional. Yahoo's FX endpoint usually
	// works without auth for the small set of major pairs we use,
	// but the production loop populates these from yahooauth.Session
	// to mirror the rest of the platform's quote pulls and survive
	// rate-limit ramps.
	crumb   string
	cookies string
}

// YahooProviderOptions configures the provider. baseURL is
// overridable so tests can point at an httptest.Server.
type YahooProviderOptions struct {
	HTTPClient *http.Client
	BaseURL    string
	Crumb      string
	Cookies    string
}

func NewYahooProvider(opts YahooProviderOptions) *YahooProvider {
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		base = "https://query2.finance.yahoo.com"
	}
	return &YahooProvider{
		httpClient: hc,
		baseURL:    base,
		crumb:      opts.Crumb,
		cookies:    opts.Cookies,
	}
}

// Name returns the provider id used as fx_rates.source.
func (p *YahooProvider) Name() string { return "yahoo" }

// Fetch resolves a single pair. Uses the Yahoo "USDCNY=X" convention.
// We only ask Yahoo for USD-anchored pairs; cross-pairs are
// triangulated by the Repo. That keeps the request volume small
// AND makes the source attribution honest (we never claim Yahoo
// "gave us" a CNY/HKD rate).
func (p *YahooProvider) Fetch(ctx context.Context, base, quote string) (*Rate, error) {
	if p == nil {
		return nil, fmt.Errorf("yahoo_fx: nil provider")
	}
	base = canonicalCurrency(base)
	quote = canonicalCurrency(quote)
	if !IsSupported(base) || !IsSupported(quote) {
		return nil, fmt.Errorf("yahoo_fx: %w (%s/%s)", ErrUnsupportedPair, base, quote)
	}
	if SameCurrency(base, quote) {
		return &Rate{Base: base, Quote: quote, Rate: 1.0, RateAt: time.Now().UTC(), Source: p.Name()}, nil
	}
	if base != AnchorCurrency && quote != AnchorCurrency {
		return nil, fmt.Errorf("yahoo_fx: cross-pair %s/%s is not directly fetchable; triangulate via USD: %w",
			base, quote, ErrUnsupportedPair)
	}

	symbol := base + quote + "=X"
	q := url.Values{}
	q.Set("symbols", symbol)
	if p.crumb != "" {
		q.Set("crumb", p.crumb)
	}
	endpoint := fmt.Sprintf("%s/v7/finance/quote?%s", p.baseURL, q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("yahoo_fx: build req: %w", err)
	}
	req.Header.Set("User-Agent", "fundai-fx/1 (+https://fund.example.com)")
	req.Header.Set("Accept", "application/json")
	if p.cookies != "" {
		req.Header.Set("Cookie", p.cookies)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("yahoo_fx: %w: http: %v", ErrRateUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("yahoo_fx: %w: 429", ErrRateUnavailable)
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("yahoo_fx: %w: 5xx %d", ErrRateUnavailable, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("yahoo_fx: status %d: %s", resp.StatusCode, string(body))
	}

	var payload yahooQuoteEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("yahoo_fx: decode: %w", err)
	}
	if payload.QuoteResponse.Error != nil {
		return nil, fmt.Errorf("yahoo_fx: provider error: %s",
			payload.QuoteResponse.Error.Description)
	}
	if len(payload.QuoteResponse.Result) == 0 {
		return nil, fmt.Errorf("yahoo_fx: %w: empty result", ErrRateUnavailable)
	}
	q0 := payload.QuoteResponse.Result[0]
	rate := q0.RegularMarketPrice
	if rate <= 0 {
		// Yahoo sometimes returns 0 / negative for a pair that
		// hasn't traded yet — treat as transient.
		return nil, fmt.Errorf("yahoo_fx: %w: zero rate for %s", ErrRateUnavailable, symbol)
	}
	rateAt := time.Unix(q0.RegularMarketTime, 0).UTC()
	if rateAt.IsZero() || q0.RegularMarketTime == 0 {
		rateAt = time.Now().UTC()
	}
	return &Rate{
		Base:   base,
		Quote:  quote,
		Rate:   rate,
		RateAt: rateAt,
		Source: p.Name(),
	}, nil
}

// yahooQuoteEnvelope is the slim subset of the Yahoo quote
// response we care about. We deliberately don't share a type with
// the equity-quote pull because that one carries fields we'd have
// to keep in sync (PE, market cap, …).
type yahooQuoteEnvelope struct {
	QuoteResponse struct {
		Result []struct {
			Symbol             string  `json:"symbol"`
			RegularMarketPrice float64 `json:"regularMarketPrice"`
			RegularMarketTime  int64   `json:"regularMarketTime"`
			Currency           string  `json:"currency"`
		} `json:"result"`
		Error *struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	} `json:"quoteResponse"`
}
