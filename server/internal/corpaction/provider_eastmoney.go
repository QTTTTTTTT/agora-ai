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

// EastmoneyProvider fetches A-share corporate actions from East
// Money's data-center HTTP API:
//
//	https://datacenter-web.eastmoney.com/api/data/v1/get?
//	    reportName=RPT_SHAREBONUS_DET
//	    &columns=...
//	    &filter=(SECURITY_CODE="688195")
//	    &client=APP&source=DataCenter
//
// # Why East Money rather than akshare / Tushare
//
//   - akshare is a Python library; calling it from Go would mean
//     spawning a Python sidecar or shelling out, both of which add
//     deployment friction with no benefit. akshare's actual A-share
//     dividend feed wraps East Money's data-center anyway.
//   - Tushare requires a paid token + IP whitelist, both of which
//     are environment-specific. Acceptable for a future paid lane,
//     not as the default unauthenticated source.
//   - Sina / Tencent return per-stock HTML pages that need scraping;
//     East Money's JSON API is structured and stable.
//
// # Coverage
//
// SSE (688/600/603/605), SZSE (000/001/002/300/301), and BJSE
// (8xx, 920xxx, 9xxxxx). The provider covers stock dividends
// (送股), capital reserve transfers (转增), and cash dividends
// (派现) — A-share doesn't have "splits" in the US sense; 送转股
// is the functional equivalent.
//
// # Reliability
//
// East Money's column naming has shifted historically (PRETAX_BONUS
// vs PRETAX_BONUS_RMB, BONUS_IT_RATIO vs BONUS_IT_RATIO_RMB). The
// parser reads through `json.RawMessage` per-row and tries each
// alias before giving up; a row with all-zero numbers is silently
// dropped rather than surfacing as an empty event.
type EastmoneyProvider struct {
	HTTPClient *http.Client
	BaseURL    string // override for tests; defaults to datacenter-web.eastmoney.com
}

// FetchEvents pulls every share bonus / cash dividend / transfer
// row East Money has on file for the given A-share code, filtered
// to events on or after `since`. Returns a flat sorted slice the
// applier can consume directly.
//
// `symbol` accepts:
//   - bare 6-digit code: "688195"
//   - explicit suffix:   "688195.SH" or "688195.SS"
//
// The InstrumentKey on returned events is always
// "{EXCHANGE_PREFIX}:{6-digit code}" using the 30-rule below.
func (p *EastmoneyProvider) FetchEvents(ctx context.Context, symbol string, since time.Time) ([]Event, error) {
	code := stripCSIExchangeSuffix(symbol)
	if code == "" {
		return nil, fmt.Errorf("eastmoney corpaction: empty symbol")
	}

	endpoint, err := p.endpoint(code)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	// Mimic a regular browser. East Money returns 412/451 to
	// crawlers without these headers.
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (compatible; FundAI/1.0)")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Referer", "https://data.eastmoney.com/")

	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("eastmoney corpaction: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("eastmoney corpaction: status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("eastmoney corpaction: read: %w", err)
	}
	return parseEastmoneyShareBonus(body, code, since)
}

func (p *EastmoneyProvider) endpoint(code string) (string, error) {
	base := p.BaseURL
	if base == "" {
		base = "https://datacenter-web.eastmoney.com"
	}
	u, err := url.Parse(base + "/api/data/v1/get")
	if err != nil {
		return "", fmt.Errorf("eastmoney corpaction: bad base url: %w", err)
	}
	q := u.Query()
	q.Set("reportName", "RPT_SHAREBONUS_DET")
	// East Money column semantics — verified live 2026-05 with
	// 688195 (10转4派1.64元) and 002594 (10送8转12派39.74元):
	//   PRETAX_BONUS_RMB — 派现 per 10 shares (含税).
	//   BONUS_IT_RATIO   — combined 送 + 转 ratio per 10 shares
	//                      (so 002594's 8+12=20 lands here).
	//   IT_RATIO         — 转股 part only (not used directly; the
	//                      schema only cares about the combined
	//                      shares-multiplier the holder receives).
	//   ASSIGN_PROGRESS  — 实施分配 / 不分配 / 预案 etc.
	//   IMPL_PLAN_PROFILE — human-readable summary, kept for notes.
	q.Set("columns",
		"SECURITY_CODE,SECURITY_NAME_ABBR,EX_DIVIDEND_DATE,"+
			"PRETAX_BONUS_RMB,BONUS_IT_RATIO,IT_RATIO,"+
			"ASSIGN_PROGRESS,IMPL_PLAN_PROFILE")
	q.Set("filter", fmt.Sprintf(`(SECURITY_CODE="%s")`, code))
	q.Set("pageNumber", "1")
	q.Set("pageSize", "100")
	q.Set("sortColumns", "NOTICE_DATE")
	q.Set("sortTypes", "-1")
	q.Set("client", "APP")
	q.Set("source", "DataCenter")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// parseEastmoneyShareBonus is exposed at package scope so tests can
// drive it from a fixture without a network call. Defensive against
// the column-rename history described on EastmoneyProvider, and
// drops rows that haven't moved past the proposal stage (PROGRESS
// ∉ {"实施", "完成"}) so we don't apply an event the listed company
// is still announcing.
func parseEastmoneyShareBonus(body []byte, code string, since time.Time) ([]Event, error) {
	var raw struct {
		Result struct {
			Data []map[string]json.RawMessage `json:"data"`
		} `json:"result"`
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("eastmoney corpaction: decode: %w", err)
	}
	if !raw.Success {
		// Empty result_set is `success:true,data:[]` — Success
		// false implies a real upstream error.
		return nil, fmt.Errorf("eastmoney corpaction: api error: %s", raw.Message)
	}

	out := make([]Event, 0, len(raw.Result.Data))
	for _, row := range raw.Result.Data {
		// Skip not-yet-implemented announcements.
		// East Money uses ASSIGN_PROGRESS (and ships compound
		// values like "实施分配"). Older rows occasionally use
		// the bare "PROGRESS" column with values like "实施" or
		// "完成", so we read both for forward-compat.
		progress := decodeRawString(row["ASSIGN_PROGRESS"])
		if progress == "" {
			progress = decodeRawString(row["PROGRESS"])
		}
		if progress != "" &&
			!strings.Contains(progress, "实施") &&
			!strings.Contains(progress, "完成") {
			continue
		}

		exDateStr := decodeRawString(row["EX_DIVIDEND_DATE"])
		if exDateStr == "" {
			continue
		}
		exDate, err := parseEastmoneyDate(exDateStr)
		if err != nil {
			continue
		}
		if !since.IsZero() && exDate.Before(since) {
			continue
		}

		// East Money quotes "per 10 shares" by convention. Convert
		// to per-old-share so the wire shape matches what the
		// applier expects (Yahoo's div feed is already per share).
		// round8 to clamp the float-division noise (1.64/10 in
		// IEEE-754 is 0.16399999999999998); the applier rounds
		// at the same precision, so this keeps both wire and
		// audit values literally equal.
		//
		// BONUS_IT_RATIO is the COMBINED 送+转 ratio (the holder
		// gets that many extra shares per 10 old shares total).
		// IT_RATIO further breaks out the 转股 portion but we
		// don't need that split: the holding effect is identical.
		cashPer10 := decodeRawFloat(row, "PRETAX_BONUS_RMB", "PRETAX_BONUS")
		stockDivPer10 := decodeRawFloat(row, "BONUS_IT_RATIO", "BONUS_IT_RATIO_RMB")

		cashPerShare := round8(cashPer10 / 10.0)
		stockDivPerShare := round8(stockDivPer10 / 10.0)

		actionType := classifyAShareAction(stockDivPerShare, cashPerShare)
		if actionType == "" {
			// All-zero row: announced and "implemented" but no
			// actual movement. Skip rather than persist a
			// degenerate row that would then need to be
			// excluded everywhere downstream.
			continue
		}

		out = append(out, Event{
			InstrumentKey: instrumentKeyForCSI(code),
			ExDate:        exDate.UTC().Truncate(24 * time.Hour),
			ActionType:    actionType,
			SplitRatio:    round8(1.0 + stockDivPerShare),
			CashDividend:  cashPerShare,
			Source:        "eastmoney",
		})
	}
	sortEventsByExDate(out)
	return out, nil
}

// decodeRawString tolerates East Money's mixed conventions where
// some fields arrive as JSON strings and others as bare numbers
// already parsed by an upstream layer.
func decodeRawString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

// decodeRawFloat tries every alias in `keys` in order and returns
// the first one that decodes to a finite float (string or number
// shape). Returns 0 if none match — a sane default since East
// Money omits zero-valued cells from some report variants.
func decodeRawFloat(row map[string]json.RawMessage, keys ...string) float64 {
	for _, k := range keys {
		raw, ok := row[k]
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var f float64
		if err := json.Unmarshal(raw, &f); err == nil {
			return f
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				return v
			}
		}
	}
	return 0
}

// classifyAShareAction picks an actionType label from the per-share
// magnitudes. Returns "" for an all-zero row so the caller can drop it.
func classifyAShareAction(stockDivPerShare, cashPerShare float64) string {
	switch {
	case stockDivPerShare > 0 && cashPerShare > 0:
		return "combined"
	case stockDivPerShare > 0:
		return "stock_dividend"
	case cashPerShare > 0:
		return "cash_dividend"
	default:
		return ""
	}
}

func parseEastmoneyDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty date")
	}
	// East Money sometimes drops the time portion. Try both shapes.
	formats := []string{"2006-01-02 15:04:05", "2006-01-02"}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable date %q", s)
}

// stripCSIExchangeSuffix removes any of the exchange-suffix forms
// users / scripts might pass in. The result is the bare 6-digit
// security code East Money's filter expects.
func stripCSIExchangeSuffix(code string) string {
	upper := strings.ToUpper(strings.TrimSpace(code))
	for _, suffix := range []string{".SH", ".SS", ".SZ", ".BJ"} {
		upper = strings.TrimSuffix(upper, suffix)
	}
	return upper
}

// instrumentKeyForCSI maps a 6-digit code to the canonical exchange
// prefix our schema uses. Heuristic only — there are edge cases
// (HK-listed dual-class A-shares, treasury tickers) that need
// manual override; routing the daily sweep through admin's
// pre-existing instrument_key whitelist will catch them.
func instrumentKeyForCSI(code string) string {
	if len(code) < 6 {
		return "SSE:" + code
	}
	prefix3 := code[:3]
	switch prefix3 {
	case "688", "600", "603", "605":
		return "SSE:" + code
	case "300", "301":
		return "SZSE:" + code
	}
	prefix2 := code[:2]
	switch prefix2 {
	case "60":
		return "SSE:" + code
	case "00":
		return "SZSE:" + code
	}
	prefix1 := code[:1]
	if prefix1 == "8" || prefix1 == "9" {
		return "BJSE:" + code
	}
	// Default to SSE for unmapped prefixes — historical SH listings
	// (5xxxxx funds, etc.) all live there and we'd rather have a
	// Match-or-skip downstream than route to a wrong exchange.
	return "SSE:" + code
}
