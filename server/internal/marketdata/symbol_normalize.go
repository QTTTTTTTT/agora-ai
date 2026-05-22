package marketdata

import (
	"strings"
)

// CanonicalSymbol returns an internal "Tencent-style" canonical symbol for an
// instrument. For A-share / HK / US tickers the format is:
//
//	A-share -> SH600519, SZ000001
//	HK     -> HK00700 (5 digits, zero-padded)
//	US     -> AAPL (uppercase, stripped)
//
// Other markets fall back to the uppercase trimmed symbol.
func CanonicalSymbol(instrument InstrumentRef) string {
	raw := strings.ToUpper(strings.TrimSpace(instrument.Symbol))
	if raw == "" {
		return ""
	}
	switch normalizeMarket(instrument.Market, instrument.AssetClass) {
	case "cnstock":
		return canonicalCNSymbol(raw)
	case "hkstock":
		return canonicalHKSymbol(raw)
	}
	return raw
}

func canonicalCNSymbol(raw string) string {
	// Strip common suffixes: 600519.SH, 600519.SS, 600519.SZ
	if idx := strings.IndexByte(raw, '.'); idx > 0 {
		raw = raw[:idx]
	}
	// Strip prefix variants: sh600519, SH600519
	switch {
	case strings.HasPrefix(raw, "SH"):
		return "SH" + strings.TrimPrefix(raw, "SH")
	case strings.HasPrefix(raw, "SZ"):
		return "SZ" + strings.TrimPrefix(raw, "SZ")
	case strings.HasPrefix(raw, "BJ"):
		return "BJ" + strings.TrimPrefix(raw, "BJ")
	}
	if isAllDigits(raw) && len(raw) == 6 {
		switch raw[0] {
		case '6', '9':
			return "SH" + raw
		case '0', '2', '3':
			return "SZ" + raw
		case '4', '8':
			return "BJ" + raw
		}
	}
	return raw
}

func canonicalHKSymbol(raw string) string {
	if idx := strings.IndexByte(raw, '.'); idx > 0 {
		raw = raw[:idx]
	}
	if strings.HasPrefix(raw, "HK") {
		raw = strings.TrimPrefix(raw, "HK")
	}
	if isAllDigits(raw) {
		// pad to 5 digits
		for len(raw) < 5 {
			raw = "0" + raw
		}
		return "HK" + raw
	}
	return raw
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// TencentSymbol returns the lowercase Tencent quote endpoint symbol, e.g.
// `sh600519`, `sz000001`, `hk00700`.
func TencentSymbol(instrument InstrumentRef) string {
	canonical := CanonicalSymbol(instrument)
	switch {
	case strings.HasPrefix(canonical, "SH"):
		return "sh" + strings.TrimPrefix(canonical, "SH")
	case strings.HasPrefix(canonical, "SZ"):
		return "sz" + strings.TrimPrefix(canonical, "SZ")
	case strings.HasPrefix(canonical, "BJ"):
		return "bj" + strings.TrimPrefix(canonical, "BJ")
	case strings.HasPrefix(canonical, "HK"):
		return "hk" + strings.TrimPrefix(canonical, "HK")
	}
	return ""
}

// YahooSymbol returns the Yahoo Finance symbol for an instrument. Examples:
//
//	US AAPL          -> AAPL
//	A-share SH600519 -> 600519.SS
//	A-share SZ000001 -> 000001.SZ
//	HK HK00700       -> 0700.HK (4-digit truncation if leading zero)
//	Futures GC       -> GC=F (when not already suffixed)
func YahooSymbol(instrument InstrumentRef) string {
	raw := strings.ToUpper(strings.TrimSpace(instrument.Symbol))
	if raw == "" {
		return ""
	}
	market := normalizeMarket(instrument.Market, instrument.AssetClass)
	switch market {
	case "cnstock":
		canonical := canonicalCNSymbol(raw)
		switch {
		case strings.HasPrefix(canonical, "SH"):
			return strings.TrimPrefix(canonical, "SH") + ".SS"
		case strings.HasPrefix(canonical, "SZ"):
			return strings.TrimPrefix(canonical, "SZ") + ".SZ"
		}
		return canonical
	case "hkstock":
		canonical := canonicalHKSymbol(raw)
		digits := strings.TrimPrefix(canonical, "HK")
		// Yahoo uses 4-digit zero-padded code for HK
		for len(digits) < 4 {
			digits = "0" + digits
		}
		return digits + ".HK"
	case "futures":
		if strings.Contains(raw, "=F") || strings.Contains(raw, ".") {
			return raw
		}
		return raw + "=F"
	}
	return raw
}
