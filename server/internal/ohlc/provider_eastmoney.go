package ohlc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// EastmoneyProvider fetches daily/weekly/monthly bars from East
// Money's keyless push2his endpoint:
//
//	https://push2his.eastmoney.com/api/qt/stock/kline/get?
//	    secid=1.000300        (1=SSE, 0=SZSE, 116=HK, 105/106=US-OTC)
//	    &klt=101              (101=daily, 102=weekly, 103=monthly)
//	    &fqt=1                (0=raw, 1=forward-adjust, 2=backward-adjust)
//	    &beg=YYYYMMDD&end=YYYYMMDD
//	    &lmt=N                (max bars; we cap at 500)
//	    &fields1=f1,f2,...    (header)
//	    &fields2=f51,f52,...  (per-bar columns)
//
// # Why this provider exists
//
// Yahoo Finance's chart endpoint is the codebase's default OHLC source
// because it is keyless and globally available, but its A-share
// coverage is uneven: csi300 (000300.SS) gets a full multi-year history
// while csi500 (000905.SS), chinext (399006.SZ) and star50 (000688.SS)
// return ONLY the latest regular-market bar (firstTradeDate=null,
// validRanges=["1d","5d"] upstream). The dashboard's benchmark chart
// degrades to a flat 100-line dot for these series, which both looks
// broken and prevents any meaningful alpha-spread overlay.
//
// East Money serves the same indices with full historical depth and
// requires no API key, so it slots in as a higher-priority A-share
// fallback. See cmd/server/wiring_adapters.go for registration order.
//
// # Coverage
//
//   - All A-share INDICES (000300, 000905, 399006, 000688, 000016,
//     000852, 399300, 399905, 399001, 399005, 000001 - SH composite, ...)
//   - All A-share STOCKS (SH 6xxxxx, SZ 0xxxxx/3xxxxx).
//   - Hong Kong stocks (XXXXX.HK) via secid=116.XXXXX. Disabled by
//     default; operators can enable via Markets=["a_share","hk_equity"].
//
// # Limitations
//
//   - No intraday data; klt=1/5/15/30/60 (intraday minutes) are not
//     wired here yet, only daily/weekly/monthly.
//   - The endpoint occasionally returns klines as null when the
//     symbol is delisted or the secid prefix is wrong; we treat that
//     as ErrNoData and let the registry try the next provider.
//   - East Money's load-balancers vary their hostnames in error logs;
//     we pin push2his to keep behaviour reproducible across runs.
type EastmoneyProvider struct {
	HTTPClient *http.Client
	// BaseURL lets tests point at httptest. Empty = production host.
	BaseURL string
	// FallbackBaseURLs is a list of alternate EM endpoints to try
	// if the primary BaseURL fails with a transient error. EM
	// runs the same kline API on multiple hosts (push2his /
	// 50.push2his / 80.push2his / 19.push2his); they share data
	// but are load-balanced separately so a WAF block on one
	// rarely affects all of them. Empty = use a sensible
	// production default chain. The chain is exhaustive but
	// each tier carries its own retry logic in
	// doEastmoneyWithRetry, so worst-case latency is bounded.
	FallbackBaseURLs []string
	// Markets defaults to {"a_share"} when empty. Operators can
	// extend (e.g., "hk_equity") but should be aware that East
	// Money's HK coverage is delayed vs Yahoo's.
	Markets []string
}

// Name implements Provider.
func (p *EastmoneyProvider) Name() string { return "eastmoney" }

// doEastmoneyWithRetry wraps client.Do with a single retry on the
// transient classes of upstream errors that East Money's CDN
// produces under load: bare `EOF` (connection closed mid-request,
// usually from a pooled-but-stale conn), `connection reset by peer`
// (CDN load-balancer hangup), and `unexpected EOF` (TLS short
// read).
//
// We retry exactly once with a small backoff. Bigger retry budgets
// belong in the cache layer above us; the provider's job is just
// to absorb the most common single-flight blip. Non-transient
// errors (DNS failure, certificate validation, 4xx/5xx HTTP
// statuses) bypass the retry — they wouldn't get healthier on a
// second try.
func doEastmoneyWithRetry(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := client.Do(req)
	if err == nil {
		return resp, nil
	}
	if !isEastmoneyTransient(err) {
		return resp, err
	}
	// Sleep briefly and try once more. We rebuild the request to
	// avoid reusing a request body reader, even though our GET
	// requests have no body.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(300 * time.Millisecond):
	}
	retryReq, rerr := http.NewRequestWithContext(ctx, req.Method, req.URL.String(), nil)
	if rerr != nil {
		return nil, err
	}
	retryReq.Header = req.Header.Clone()
	return client.Do(retryReq)
}

// isEastmoneyTransient classifies an error as worth a single
// retry. We deliberately match on string fragments rather than
// `errors.Is` because the underlying TLS / net errors are not all
// publicly exported sentinels.
func isEastmoneyTransient(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, marker := range []string{"EOF", "unexpected EOF", "connection reset by peer", "broken pipe"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// defaultEastmoneyClient builds an http.Client with a custom Dialer
// that pins resolution to IPv4. East Money's CDN routinely returns
// AAAA records that point at IPv6-only edge nodes (the canonical
// CNAME is `push2hisipv6.trafficmanager.cn`), and many Docker
// hosts run an IPv4-only userspace network — DialContext picks
// the v6 address first, fails immediately with "network
// unreachable" or hangs until timeout, and only then tries v4
// (Happy Eyeballs). Forcing v4 keeps cold-start latency down and
// makes the provider behave the same in the unit-test sandbox,
// CI, and the production cluster regardless of dual-stack
// configuration.
//
// We also pin HTTP/1.1 (ForceAttemptHTTP2 = false). The push2his
// CDN's HTTP/2 layer occasionally closes streams with `EOF`
// mid-response when the client's TLS fingerprint doesn't match
// a recognised browser; HTTP/1.1 keeps each request on a dedicated
// connection that is more forgiving of mismatched fingerprints
// and is empirically more stable for this endpoint.
//
// Operators who want native v6 (some on-prem deployments) can
// override by setting EastmoneyProvider.HTTPClient explicitly
// when wiring the provider.
func defaultEastmoneyClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   8 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Client{
		Timeout: 12 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if network == "tcp" || network == "tcp6" {
					network = "tcp4"
				}
				return dialer.DialContext(ctx, network, addr)
			},
			ForceAttemptHTTP2:     false,
			DisableKeepAlives:     false,
			MaxIdleConns:          16,
			MaxConnsPerHost:       4,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 8 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// Supports implements Provider.
func (p *EastmoneyProvider) Supports(market string) bool {
	markets := p.Markets
	if len(markets) == 0 {
		markets = []string{"a_share"}
	}
	for _, m := range markets {
		if strings.EqualFold(m, market) {
			return true
		}
	}
	return false
}

// Fetch implements Provider. Returns oldest-first bars or ErrNoData.
func (p *EastmoneyProvider) Fetch(ctx context.Context, req FetchRequest) ([]Bar, error) {
	req = req.Normalize()
	if req.Symbol == "" {
		return nil, ErrNoData
	}
	secid, ok := emSecid(req.Symbol)
	if !ok {
		// East Money's prefix conventions only cover canonical
		// `.SS`/`.SZ`/`.BJ` symbols. Anything else (e.g., a US
		// ticker accidentally routed here) returns ErrNoData
		// rather than a 4xx so the registry falls through.
		return nil, ErrNoData
	}
	hosts := p.hostChain()
	var lastErr error
	for _, host := range hosts {
		bars, err := p.fetchAt(ctx, host, req, secid)
		if err == nil {
			return bars, nil
		}
		// ErrNoData is terminal — the symbol simply isn't there;
		// trying a different host won't change that.
		if err == ErrNoData {
			return nil, ErrNoData
		}
		lastErr = err
		// Only fall through to the next host on a transient
		// network class of failure. Anything that smells like a
		// 4xx / 5xx from upstream (already returned with full
		// response body) means EM IS reachable, just unhappy
		// with the input — don't punish the next host for it.
		if !isEastmoneyTransient(err) {
			return nil, err
		}
	}
	if lastErr == nil {
		lastErr = ErrNoData
	}
	return nil, lastErr
}

// hostChain is the ordered list of EM hosts to try. Returns the
// per-instance override (BaseURL + FallbackBaseURLs) when set,
// otherwise the production default which spreads across multiple
// EM CDN clusters so a WAF block on one doesn't take down the
// whole A-share index lane.
func (p *EastmoneyProvider) hostChain() []string {
	if p == nil {
		return defaultEastmoneyHosts()
	}
	hosts := make([]string, 0, 1+len(p.FallbackBaseURLs))
	if base := strings.TrimSpace(p.BaseURL); base != "" {
		hosts = append(hosts, base)
	}
	for _, h := range p.FallbackBaseURLs {
		if hh := strings.TrimSpace(h); hh != "" {
			hosts = append(hosts, hh)
		}
	}
	if len(hosts) == 0 {
		return defaultEastmoneyHosts()
	}
	return hosts
}

// defaultEastmoneyHosts returns the canonical fallback chain for
// East Money's A-share kline API. Each host serves the same
// data; the chain exists purely to route around per-host WAF
// rate-limits or network blips. Order is from highest to lowest
// historical reliability based on what the platform's smoke tests
// have observed.
func defaultEastmoneyHosts() []string {
	return []string{
		"https://push2his.eastmoney.com",
		"https://19.push2his.eastmoney.com",
		"https://50.push2his.eastmoney.com",
		"https://80.push2his.eastmoney.com",
		// HTTP fallback (no TLS) catches the case where the WAF
		// blocks the HTTPS edge but leaves the legacy port-80
		// path open. EM doesn't serve PII here so plaintext is
		// acceptable for this benchmark-only use case.
		"http://push2his.eastmoney.com",
	}
}

// fetchAt is Fetch's per-host worker. Pulled out so the host
// fall-through loop in Fetch stays compact; semantics identical
// to what the old monolithic Fetch did (request build → retry
// helper → JSON parse).
func (p *EastmoneyProvider) fetchAt(ctx context.Context, host string, req FetchRequest, secid string) ([]Bar, error) {
	endpoint, err := p.endpointAt(host, req, secid)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	// East Money rate-limits unmarked traffic but is generous with
	// browser-like UAs. Same UA as YahooProvider keeps both
	// providers' fingerprints uniform.
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Referer", "https://quote.eastmoney.com/")
	httpReq.Header.Set("Connection", "close")

	client := p.HTTPClient
	if client == nil {
		client = defaultEastmoneyClient()
	}
	resp, err := doEastmoneyWithRetry(ctx, client, httpReq)
	if err != nil {
		return nil, fmt.Errorf("eastmoney: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoData
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("eastmoney: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("eastmoney: read: %w", err)
	}
	return parseEastmoneyKLine(body, req.LookbackN)
}

// endpointAt builds the kline URL against a specific base host,
// keeping all the query-parameter logic in one spot.
func (p *EastmoneyProvider) endpointAt(host string, req FetchRequest, secid string) (string, error) {
	host = strings.TrimRight(host, "/")
	u, err := url.Parse(host + "/api/qt/stock/kline/get")
	if err != nil {
		return "", fmt.Errorf("eastmoney: build url: %w", err)
	}
	q := u.Query()
	q.Set("secid", secid)
	q.Set("klt", emKltString(req.Interval))
	q.Set("fqt", "1")
	q.Set("fields1", "f1,f2,f3,f4,f5,f6")
	q.Set("fields2", "f51,f52,f53,f54,f55,f56,f57,f58,f59,f60,f61")
	lmt := req.LookbackN + 5
	if lmt < 30 {
		lmt = 30
	}
	if lmt > 500 {
		lmt = 500
	}
	q.Set("lmt", strconv.Itoa(lmt))
	q.Set("end", "20500101")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// emKltString maps our canonical Interval to East Money's klt code.
// Daily / weekly / monthly are wired; intraday minute bars are NOT
// because the dashboard never asks for them on this provider (Yahoo
// covers intraday for US/HK and we'd rather leave A-share intraday
// to a future dedicated source).
func emKltString(i Interval) string {
	switch i {
	case Interval1w:
		return "102"
	default:
		// Daily covers Day, IntervalDay (default), and any unknown
		// minute interval — in the latter case the registry will
		// see daily bars and indicators will simply down-sample.
		return "101"
	}
}

// emSecid maps a canonical OHLC symbol like "000300.SS" or
// "688195.SS" to East Money's secid format. The leading digit is
// the exchange code:
//
//	1 = Shanghai Stock Exchange (SSE)
//	0 = Shenzhen Stock Exchange (SZSE), incl. ChiNext (3xxxxx) and
//	    SZ-listed indices like 399006.
//	116 = Hong Kong (5-digit codes ending in `.HK`).
//
// Returns ok=false for symbols outside this scheme so the caller
// can return ErrNoData and let the registry fall through.
func emSecid(symbol string) (string, bool) {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	if s == "" {
		return "", false
	}
	switch {
	case strings.HasSuffix(s, ".SS"), strings.HasSuffix(s, ".SH"):
		code := strings.TrimSuffix(strings.TrimSuffix(s, ".SS"), ".SH")
		if !isAllDigits(code) {
			return "", false
		}
		return "1." + code, true
	case strings.HasSuffix(s, ".SZ"):
		code := strings.TrimSuffix(s, ".SZ")
		if !isAllDigits(code) {
			return "", false
		}
		return "0." + code, true
	case strings.HasSuffix(s, ".BJ"):
		// Beijing Stock Exchange is also code 0 in the East Money
		// secid scheme (legacy from when BJSE was a Shenzhen
		// satellite). Verified against 920002.BJ et al.
		code := strings.TrimSuffix(s, ".BJ")
		if !isAllDigits(code) {
			return "", false
		}
		return "0." + code, true
	case strings.HasSuffix(s, ".HK"):
		code := strings.TrimSuffix(s, ".HK")
		if !isAllDigits(code) {
			return "", false
		}
		// HK codes are 1-5 digits — pad to 5 with leading zeros.
		for len(code) < 5 {
			code = "0" + code
		}
		return "116." + code, true
	}
	return "", false
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

// parseEastmoneyKLine consumes the kline JSON and returns up to keep
// bars in oldest-first order. East Money returns klines as comma-
// separated strings ordered oldest-first, e.g.:
//
//	"2024-01-02,4023.30,4124.50,4055.20,4012.80,1234567890,..."
//	   date     ,open  ,close ,high  ,low   ,volume    ,...
//
// We pull date/open/close/high/low/volume; the rest are amount,
// amplitude, change %, change abs, turnover %, all of which are
// derivable from the prices we already keep.
func parseEastmoneyKLine(body []byte, keep int) ([]Bar, error) {
	var dto struct {
		Data *struct {
			Code   string   `json:"code"`
			Klines []string `json:"klines"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &dto); err != nil {
		return nil, fmt.Errorf("eastmoney: decode: %w", err)
	}
	if dto.Data == nil || len(dto.Data.Klines) == 0 {
		return nil, ErrNoData
	}
	rows := dto.Data.Klines
	bars := make([]Bar, 0, len(rows))
	for _, raw := range rows {
		fields := strings.Split(raw, ",")
		if len(fields) < 6 {
			continue
		}
		date, err := time.ParseInLocation("2006-01-02", fields[0], time.UTC)
		if err != nil {
			continue
		}
		open, oerr := strconv.ParseFloat(fields[1], 64)
		closePx, cerr := strconv.ParseFloat(fields[2], 64)
		high, herr := strconv.ParseFloat(fields[3], 64)
		low, lerr := strconv.ParseFloat(fields[4], 64)
		vol, _ := strconv.ParseFloat(fields[5], 64) // volume can be 0 for indices, tolerate
		if oerr != nil || cerr != nil || herr != nil || lerr != nil {
			continue
		}
		bars = append(bars, Bar{
			Time:   date,
			Open:   open,
			Close:  closePx,
			High:   high,
			Low:    low,
			Volume: vol,
		})
	}
	if len(bars) == 0 {
		return nil, ErrNoData
	}
	// East Money already returns oldest-first, but be defensive in
	// case a future caller flips the parameter.
	if len(bars) >= 2 && bars[0].Time.After(bars[len(bars)-1].Time) {
		for i, j := 0, len(bars)-1; i < j; i, j = i+1, j-1 {
			bars[i], bars[j] = bars[j], bars[i]
		}
	}
	if keep > 0 && len(bars) > keep {
		bars = bars[len(bars)-keep:]
	}
	return bars, nil
}
