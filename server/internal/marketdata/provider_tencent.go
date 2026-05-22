package marketdata

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// tencentQuoteProvider returns a quote provider that hits qt.gtimg.cn, the
// public Tencent finance endpoint. It supports A-share (SH/SZ/BJ) and HK
// equities. No API key required. Response payload is GBK-encoded.
//
// Endpoint format: https://qt.gtimg.cn/q=sh600519,sz000001
// Each line:       v_sh600519="1~symbol~name~code~price~...";
func (s *Service) tencentQuoteProvider() quoteProviderFunc {
	return func(ctx context.Context, instrument InstrumentRef) (*QuoteSnapshot, error) {
		market := normalizeMarket(instrument.Market, instrument.AssetClass)
		if market != "cnstock" && market != "hkstock" {
			return nil, fmt.Errorf("tencent: unsupported market %q", instrument.Market)
		}
		sym := TencentSymbol(instrument)
		if sym == "" {
			return nil, fmt.Errorf("tencent: cannot derive tencent symbol from %q", instrument.Symbol)
		}
		endpoint := "https://qt.gtimg.cn/q=" + sym
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("tencent: build request: %w", err)
		}
		req.Header.Set("Referer", "https://gu.qq.com/")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
		resp, err := s.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("tencent: http: %w", err)
		}
		defer resp.Body.Close()
		if isThrottleStatus(resp.StatusCode) {
			return nil, fmt.Errorf("%w: tencent: http %d", ErrUpstreamThrottled, resp.StatusCode)
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("tencent: http %d", resp.StatusCode)
		}
		decoded, err := io.ReadAll(transform.NewReader(resp.Body, simplifiedchinese.GBK.NewDecoder()))
		if err != nil {
			return nil, fmt.Errorf("tencent: decode: %w", err)
		}
		body := strings.TrimSpace(string(decoded))
		quote, err := parseTencentLine(body, instrument, sym)
		if err != nil {
			return nil, err
		}
		return quote, nil
	}
}

func parseTencentLine(body string, instrument InstrumentRef, sym string) (*QuoteSnapshot, error) {
	// Find the substring between the first " and the closing ";
	startIdx := strings.IndexByte(body, '"')
	if startIdx < 0 {
		return nil, fmt.Errorf("tencent: empty payload for %s", sym)
	}
	endIdx := strings.LastIndex(body, "\"")
	if endIdx <= startIdx {
		return nil, fmt.Errorf("tencent: malformed payload for %s", sym)
	}
	payload := body[startIdx+1 : endIdx]
	if payload == "" {
		return nil, fmt.Errorf("tencent: blank payload for %s (delisted or wrong code?)", sym)
	}
	parts := strings.Split(payload, "~")
	// Expected layout (A-share):
	//   1: prefix (1)
	//   2: name (chinese)
	//   3: code
	//   4: price
	//   5: prev_close
	//   6: open
	//   7: volume (lots)
	//   ...
	if len(parts) < 5 {
		return nil, fmt.Errorf("tencent: insufficient fields (%d) for %s", len(parts), sym)
	}
	priceStr := strings.TrimSpace(parts[3])
	price, err := strconv.ParseFloat(priceStr, 64)
	if err != nil {
		return nil, fmt.Errorf("tencent: parse price %q: %w", priceStr, err)
	}
	if price <= 0 {
		return nil, fmt.Errorf("tencent: non-positive price for %s", sym)
	}
	quote := &QuoteSnapshot{
		Symbol:        CanonicalSymbol(instrument),
		InstrumentKey: instrument.InstrumentKey,
		Market:        instrument.Market,
		Exchange:      instrument.Exchange,
		AssetClass:    instrument.AssetClass,
		Price:         price,
		AsOf:          time.Now().UTC(),
		Source:        "tencent",
		QuoteCurrency: instrument.QuoteCurrency,
	}
	if len(parts) > 6 {
		if v, err := strconv.ParseFloat(strings.TrimSpace(parts[6]), 64); err == nil {
			// Tencent reports volume in 手 (lots, 100 shares each)
			quote.Volume = int64(v * 100)
		}
	}
	if len(parts) > 30 {
		// parts[30] is YYYYMMDDhhmmss for A-share
		if t, err := time.ParseInLocation("20060102150405", strings.TrimSpace(parts[30]), shanghaiLocation()); err == nil {
			quote.AsOf = t.UTC()
		}
	}
	if quote.QuoteCurrency == "" {
		switch normalizeMarket(instrument.Market, instrument.AssetClass) {
		case "cnstock":
			quote.QuoteCurrency = "CNY"
		case "hkstock":
			quote.QuoteCurrency = "HKD"
		}
	}
	return quote, nil
}

func shanghaiLocation() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}
