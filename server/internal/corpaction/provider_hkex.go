package corpaction

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

// HKEXProvider fetches Hong Kong corporate actions from East
// Money's HK data-center HTTP API:
//
//	https://datacenter-web.eastmoney.com/api/data/v1/get?
//	    reportName=RPT_HKF10_DIVIDENDPLAN
//	    &columns=...
//	    &filter=(SECUCODE="00700.HK")
//
// # Why this source rather than HKEX direct / Futu / AAStocks
//
//   - HKEX's Disclosure Information (DI) Service is the canonical
//     feed but paywalled and requires a B2B contract — out of
//     reach for an open-data ingest path.
//   - Futu OpenAPI requires their SDK + a real broker account,
//     which is overkill for a once-a-day cron-shaped corp-action
//     sweep and pulls a heavy native dependency into the build.
//   - AAStocks publishes structured-ish HTML but the layout
//     drifts every quarter; scraping it would be the most
//     fragile branch in the system.
//   - Yahoo's `.HK` suffix already works (provider_yahoo.go)
//     but coverage is partial — interim dividends and special
//     dividends frequently slip through, and bonus issues
//     (送股) are typically missing entirely. Operators have
//     burned us on this twice.
//
// East Money already aggregates HK final / interim / special
// dividends, bonus issues, and consolidations into the same
// data-center JSON feed we hit for A-shares. Reusing the
// `datacenter-web.eastmoney.com` host means we share user-agent
// rules, transient-error markers, and CDN/geo behaviour with
// the existing EastmoneyProvider — one upstream to monitor,
// one block-rate to alert on.
//
// # Coverage
//
//   - HKEX main-board listings (codes 00001–09999, including the
//     post-2018 dual-class 8xxxx range; 5-digit zero-padded).
//   - Cash dividend: interim, final, special.
//   - Bonus issue (送股) and stock split — modelled as
//     `stock_dividend` with split_ratio = 1 + per-old-share new
//     shares.
//   - Combined `cash + bonus` rows produce a single Event with
//     ActionType="combined" so the applier mutates both
//     quantity and cash atomically.
//   - Scrip dividends (代息) are recorded as cash. Operators
//     who actually elect scrip would need to override the
//     credit at apply time; the default-cash assumption matches
//     what 99% of small holders see in their broker statements.
//
// # Currency
//
// East Money quotes HK dividends in HKD per share. The applier
// does not carry currency — funds.current_capital is a single
// numeric column — so we post the HKD amount verbatim as
// `cash_credit`. Single-currency funds (the common case today)
// see this as the right number; cross-currency funds need the
// full Card F redesign (fund_cash_balances + fx) before HK
// dividends post correctly. The `Source: "hkex_eastmoney"`
// label on the Event lets a future fx layer filter what to
// translate without re-parsing JSON.
//
// # Symbol shapes accepted
//
//   - "0700"        bare 4-digit (zero-padded to 5)
//   - "00700"       bare 5-digit
//   - "0700.HK"     Yahoo style
//   - "00700.HK"    Yahoo zero-padded
//   - "HKEX:00700"  canonical InstrumentKey shape
//
// Output `InstrumentKey` is always "HKEX:{5-digit zero-padded}",
// matching what `instrumentKeyForYahoo` produces for `.HK`
// inputs. Two providers feeding the same key keeps the
// idempotency PK in `corporate_actions` doing the right thing
// when an operator re-runs the same window through both.
type HKEXProvider struct {
	HTTPClient *http.Client
	BaseURL    string // override for tests; defaults to datacenter-web.eastmoney.com
}

// FetchEvents implements EventFetcher. Returns every cash
// dividend / bonus issue / split East Money has on file for the
// HK code, filtered to events on or after `since`. Output is a
// flat slice sorted by ex-date ascending.
//
// Implementation contract matches EastmoneyProvider.FetchEvents:
// 404 from upstream → empty slice + nil error (treat as "no
// events" so a single dead ticker doesn't poison the daily
// sweep), other 4xx/5xx → wrapped error.
func (p *HKEXProvider) FetchEvents(ctx context.Context, symbol string, since time.Time) ([]Event, error) {
	code := normalizeHKCode(symbol)
	if code == "" {
		return nil, fmt.Errorf("hkex corpaction: empty/invalid symbol %q", symbol)
	}

	endpoint, err := p.endpoint(code)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; FundAI/1.0)")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", "https://emweb.securities.eastmoney.com/")

	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hkex corpaction: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("hkex corpaction: status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("hkex corpaction: read: %w", err)
	}
	return parseHKEXDividendPlan(body, code, since)
}

func (p *HKEXProvider) endpoint(code string) (string, error) {
	base := p.BaseURL
	if base == "" {
		base = "https://datacenter-web.eastmoney.com"
	}
	u, err := url.Parse(base + "/api/data/v1/get")
	if err != nil {
		return "", fmt.Errorf("hkex corpaction: bad base url: %w", err)
	}
	q := u.Query()
	// Report semantics — verified against East Money's HK F10
	// dividend module, 2026-05:
	//   SECUCODE          — e.g. "00700.HK" (5-digit + suffix).
	//   EX_DIVIDEND_DATE  — ex-date in `YYYY-MM-DD HH:MM:SS`.
	//   DIVIDEND_DATE     — pay date (informational; not used).
	//   DIVIDEND_RATIO    — cash per share, HKD.
	//   BONUS_IT_RATIO    — combined bonus + transfer per 10 old
	//                       shares (so a "10:1" bonus issue lands
	//                       here as 1.0). Some older rows use
	//                       BONUS_RATIO; we read either via the
	//                       alias logic in decodeRawFloat.
	//   EVENT_PROCESS     — "实施" / "派发" / "公告" / "建议".
	//                       We require contains("实施") OR
	//                       contains("派发") so unannounced or
	//                       proposal-stage rows are dropped.
	//   IMPL_PLAN_PROFILE — human-readable summary; logged but
	//                       not persisted on Event.
	q.Set("reportName", "RPT_HKF10_DIVIDENDPLAN")
	q.Set("columns",
		"SECUCODE,SECURITY_NAME_ABBR,EX_DIVIDEND_DATE,DIVIDEND_DATE,"+
			"DIVIDEND_RATIO,BONUS_IT_RATIO,BONUS_RATIO,"+
			"EVENT_PROCESS,IMPL_PLAN_PROFILE")
	q.Set("filter", fmt.Sprintf(`(SECUCODE="%s.HK")`, code))
	q.Set("pageNumber", "1")
	q.Set("pageSize", "100")
	q.Set("sortColumns", "EX_DIVIDEND_DATE")
	q.Set("sortTypes", "-1")
	q.Set("client", "APP")
	q.Set("source", "DataCenter")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// parseHKEXDividendPlan converts the East Money HK dividend feed
// into Event rows. Exposed at package scope so tests drive it
// from a fixture without standing up an httptest.Server.
//
// Defensive against:
//   - column rename history (BONUS_RATIO ↔ BONUS_IT_RATIO);
//   - mixed string/number encodings on numeric cells;
//   - rows still in proposal stage (EVENT_PROCESS != "实施"/"派发");
//   - all-zero placeholder rows that listed companies sometimes
//     file as a no-op disclosure.
func parseHKEXDividendPlan(body []byte, code string, since time.Time) ([]Event, error) {
	var raw struct {
		Result struct {
			Data []map[string]json.RawMessage `json:"data"`
		} `json:"result"`
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("hkex corpaction: decode: %w", err)
	}
	if !raw.Success {
		return nil, fmt.Errorf("hkex corpaction: api error: %s", raw.Message)
	}

	out := make([]Event, 0, len(raw.Result.Data))
	for _, row := range raw.Result.Data {
		// Drop proposal-stage rows. East Money uses EVENT_PROCESS
		// for HK ("实施" / "派发" / "公告" / "建议"); some older
		// rows surface as IMPL_PROGRESS which we read as a fallback.
		progress := decodeRawString(row["EVENT_PROCESS"])
		if progress == "" {
			progress = decodeRawString(row["IMPL_PROGRESS"])
		}
		if progress != "" &&
			!strings.Contains(progress, "实施") &&
			!strings.Contains(progress, "派发") &&
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

		// HK semantics:
		//   DIVIDEND_RATIO  = HKD per share (already per share,
		//                     unlike A-share which is per 10).
		//   BONUS_IT_RATIO  = bonus shares per 10 old shares
		//                     (matches A-share convention; a
		//                     "10送1" lands here as 1.0).
		// round8 clamps the floating-point div noise so wire
		// values match what the applier persists.
		cashPerShare := round8(decodeRawFloat(row, "DIVIDEND_RATIO", "DIVIDEND_RATIO_RMB"))
		bonusPer10 := decodeRawFloat(row, "BONUS_IT_RATIO", "BONUS_RATIO")
		bonusPerShare := round8(bonusPer10 / 10.0)

		actionType := classifyAShareAction(bonusPerShare, cashPerShare)
		if actionType == "" {
			continue
		}

		out = append(out, Event{
			InstrumentKey: instrumentKeyForHKEX(code),
			ExDate:        exDate.UTC().Truncate(24 * time.Hour),
			ActionType:    actionType,
			SplitRatio:    round8(1.0 + bonusPerShare),
			CashDividend:  cashPerShare,
			// Discriminator suffix lets a future operator query
			// `WHERE source = 'hkex_eastmoney'` to spot rows that
			// came from the HK feed specifically (vs. the A-share
			// feed, which just labels itself "eastmoney").
			Source: "hkex_eastmoney",
		})
	}
	sortEventsByExDate(out)
	return out, nil
}

// normalizeHKCode strips suffixes ("HKEX:", ".HK") and zero-pads
// the bare code to 5 digits — the canonical HKEX listing length.
// Returns "" if the input doesn't reduce to a valid all-digit
// code (so the caller surfaces a clean error instead of hitting
// the upstream with garbage).
//
// HK code shape rules:
//   - Bare codes are 1–5 digits (00001 = HKEX, 09988 = BABA,
//     08032 = a GEM ticker before the 2018 reform).
//   - Pre-2014 some indexes used 6-digit codes (e.g. "800000"
//     for HSI futures). Those don't trade as equities and don't
//     show up in holding_positions, so we don't normalize them
//     specially — they just fall through to the 5-digit branch
//     and look obviously wrong, which is the desired behaviour
//     (we'd rather error than apply a corp action to an index).
func normalizeHKCode(symbol string) string {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	if s == "" {
		return ""
	}
	s = strings.TrimPrefix(s, "HKEX:")
	s = strings.TrimSuffix(s, ".HK")
	for _, c := range s {
		if c < '0' || c > '9' {
			return ""
		}
	}
	if len(s) > 5 {
		return ""
	}
	for len(s) < 5 {
		s = "0" + s
	}
	return s
}

// instrumentKeyForHKEX returns the canonical instrument-key form
// our schema uses ("HKEX:00700"). Mirrors instrumentKeyForCSI's
// shape so the rest of the system can treat HK and A-share keys
// uniformly.
func instrumentKeyForHKEX(code string) string {
	return "HKEX:" + code
}
