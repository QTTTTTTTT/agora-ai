package corpaction

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fixtureHKEXTencent2025FinalDividend models Tencent (00700.HK)
// declaring HKD 4.50/share final dividend with ex-date 2025-05-22.
// Tencent is a pure cash-dividend issuer (no bonus issues
// historically), so this anchors the cash-only branch.
const fixtureHKEXTencent2025FinalDividend = `{
  "version": "abc...",
  "result": {
    "pages": 1,
    "data": [
      {
        "SECUCODE": "00700.HK",
        "SECURITY_NAME_ABBR": "腾讯控股",
        "EX_DIVIDEND_DATE": "2025-05-22 00:00:00",
        "DIVIDEND_DATE": "2025-06-12 00:00:00",
        "DIVIDEND_RATIO": 4.5,
        "BONUS_IT_RATIO": null,
        "BONUS_RATIO": null,
        "EVENT_PROCESS": "实施",
        "IMPL_PLAN_PROFILE": "末期股息每股4.50港元"
      },
      {
        "SECUCODE": "00700.HK",
        "SECURITY_NAME_ABBR": "腾讯控股",
        "EX_DIVIDEND_DATE": "2024-05-23 00:00:00",
        "DIVIDEND_RATIO": 3.4,
        "EVENT_PROCESS": "派发"
      }
    ]
  },
  "success": true
}`

// fixtureHKEXBonusIssue models a "10 送 1" bonus issue. Hong Kong
// banks and small-cap industrials still use this shape (e.g. some
// older 03328 / 06183 announcements). No cash leg — pure stock
// dividend. The parser must classify as `stock_dividend` with
// split_ratio = 1.1.
const fixtureHKEXBonusIssue = `{
  "result": {
    "data": [
      {
        "SECUCODE": "06183.HK",
        "EX_DIVIDEND_DATE": "2025-09-15 00:00:00",
        "DIVIDEND_RATIO": 0,
        "BONUS_IT_RATIO": 1.0,
        "EVENT_PROCESS": "实施"
      }
    ]
  },
  "success": true
}`

// fixtureHKEXCombinedSpecialDividend models a row with both cash
// and a small bonus leg — modelled on HSBC-style declarations
// where they pay HKD 0.31 final + 1-in-10 bonus issue. Confirms
// the combined classification path.
const fixtureHKEXCombinedSpecialDividend = `{
  "result": {
    "data": [
      {
        "SECUCODE": "00005.HK",
        "EX_DIVIDEND_DATE": "2025-08-21 00:00:00",
        "DIVIDEND_RATIO": 0.31,
        "BONUS_IT_RATIO": 1.0,
        "EVENT_PROCESS": "实施"
      }
    ]
  },
  "success": true
}`

// fixtureHKEXProposalOnly is a row that's been announced but not
// implemented. Like the A-share "董事会决议通过" case, we drop
// these — applying a not-yet-paid dividend would invent cash on
// the holder's books today.
const fixtureHKEXProposalOnly = `{
  "result": {
    "data": [
      {
        "SECUCODE": "00388.HK",
        "EX_DIVIDEND_DATE": "2026-08-30 00:00:00",
        "DIVIDEND_RATIO": 5.4,
        "EVENT_PROCESS": "公告"
      }
    ]
  },
  "success": true
}`

// fixtureHKEXAllZero is the degenerate filler row (no payout, no
// shares). HK exchange filings include these for 委任公告 or
// 不派息声明; they must produce zero events.
const fixtureHKEXAllZero = `{
  "result": {"data": [{
    "SECUCODE": "01234.HK",
    "EX_DIVIDEND_DATE": "2024-09-01 00:00:00",
    "DIVIDEND_RATIO": 0,
    "BONUS_IT_RATIO": 0,
    "EVENT_PROCESS": "实施"
  }]},
  "success": true
}`

// fixtureHKEXBonusRatioAlias mirrors the older HK report layout
// where the column is named BONUS_RATIO instead of
// BONUS_IT_RATIO. The provider must read either alias.
const fixtureHKEXBonusRatioAlias = `{
  "result": {
    "data": [
      {
        "SECUCODE": "06183.HK",
        "EX_DIVIDEND_DATE": "2024-04-12 00:00:00",
        "DIVIDEND_RATIO": "0",
        "BONUS_RATIO": "2",
        "EVENT_PROCESS": "实施"
      }
    ]
  },
  "success": true
}`

// fixtureHKEXAPIError mirrors what East Money returns when its
// HK backend bombs. Surface as error so the daily sweep records
// it as a provider failure (and increments the alert counter)
// rather than swallowing it as zero events.
const fixtureHKEXAPIError = `{
  "result": {"data": []},
  "success": false,
  "message": "system busy, retry later"
}`

func TestParseHKEXDividendPlan_TencentCashOnly(t *testing.T) {
	events, err := parseHKEXDividendPlan([]byte(fixtureHKEXTencent2025FinalDividend), "00700", time.Time{})
	if err != nil {
		t.Fatalf("parse err = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}

	// Sorted ascending by ex-date — older first.
	older := events[0]
	if !older.ExDate.Equal(time.Date(2024, 5, 23, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("event[0].ExDate = %v, want 2024-05-23", older.ExDate)
	}
	if older.ActionType != "cash_dividend" {
		t.Errorf("event[0].ActionType = %q, want cash_dividend", older.ActionType)
	}
	if older.CashDividend != 3.4 {
		t.Errorf("event[0].CashDividend = %v, want 3.4 HKD", older.CashDividend)
	}
	if older.SplitRatio != 1.0 {
		t.Errorf("event[0].SplitRatio = %v, want 1.0", older.SplitRatio)
	}

	got := events[1]
	if !got.ExDate.Equal(time.Date(2025, 5, 22, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("event[1].ExDate = %v, want 2025-05-22", got.ExDate)
	}
	if got.ActionType != "cash_dividend" {
		t.Errorf("event[1].ActionType = %q, want cash_dividend", got.ActionType)
	}
	if got.CashDividend != 4.5 {
		t.Errorf("event[1].CashDividend = %v, want 4.5 HKD", got.CashDividend)
	}
	if got.SplitRatio != 1.0 {
		t.Errorf("event[1].SplitRatio = %v, want 1.0", got.SplitRatio)
	}
	if got.Source != "hkex_eastmoney" {
		t.Errorf("event[1].Source = %q, want hkex_eastmoney", got.Source)
	}
	if got.InstrumentKey != "HKEX:00700" {
		t.Errorf("event[1].InstrumentKey = %q, want HKEX:00700", got.InstrumentKey)
	}
}

func TestParseHKEXDividendPlan_BonusIssueOnly(t *testing.T) {
	events, err := parseHKEXDividendPlan([]byte(fixtureHKEXBonusIssue), "06183", time.Time{})
	if err != nil {
		t.Fatalf("parse err = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	got := events[0]
	if got.ActionType != "stock_dividend" {
		t.Errorf("ActionType = %q, want stock_dividend", got.ActionType)
	}
	// 10 送 1: holder gets 0.1 extra share per old share →
	// split_ratio = 1.1.
	if got.SplitRatio != 1.1 {
		t.Errorf("SplitRatio = %v, want 1.1", got.SplitRatio)
	}
	if got.CashDividend != 0 {
		t.Errorf("CashDividend = %v, want 0", got.CashDividend)
	}
}

func TestParseHKEXDividendPlan_CombinedCashAndBonus(t *testing.T) {
	events, err := parseHKEXDividendPlan([]byte(fixtureHKEXCombinedSpecialDividend), "00005", time.Time{})
	if err != nil {
		t.Fatalf("parse err = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	got := events[0]
	if got.ActionType != "combined" {
		t.Errorf("ActionType = %q, want combined", got.ActionType)
	}
	if got.SplitRatio != 1.1 {
		t.Errorf("SplitRatio = %v, want 1.1", got.SplitRatio)
	}
	if got.CashDividend != 0.31 {
		t.Errorf("CashDividend = %v, want 0.31 HKD", got.CashDividend)
	}
	if got.InstrumentKey != "HKEX:00005" {
		t.Errorf("InstrumentKey = %q, want HKEX:00005", got.InstrumentKey)
	}
}

func TestParseHKEXDividendPlan_DropsProposalStage(t *testing.T) {
	events, err := parseHKEXDividendPlan([]byte(fixtureHKEXProposalOnly), "00388", time.Time{})
	if err != nil {
		t.Fatalf("parse err = %v", err)
	}
	if len(events) != 0 {
		t.Errorf("len(events) = %d, want 0; proposal-stage rows must drop", len(events))
	}
}

func TestParseHKEXDividendPlan_DropsAllZero(t *testing.T) {
	events, err := parseHKEXDividendPlan([]byte(fixtureHKEXAllZero), "01234", time.Time{})
	if err != nil {
		t.Fatalf("parse err = %v", err)
	}
	if len(events) != 0 {
		t.Errorf("len(events) = %d, want 0", len(events))
	}
}

func TestParseHKEXDividendPlan_BonusRatioAliasIsRead(t *testing.T) {
	// Older report layout uses BONUS_RATIO instead of
	// BONUS_IT_RATIO, and ships everything as JSON-strings. The
	// alias-fallback in decodeRawFloat must still pick this up.
	events, err := parseHKEXDividendPlan([]byte(fixtureHKEXBonusRatioAlias), "06183", time.Time{})
	if err != nil {
		t.Fatalf("parse err = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	// 10 送 2 → 0.2 per share → split_ratio = 1.2.
	if events[0].SplitRatio != 1.2 {
		t.Errorf("SplitRatio = %v, want 1.2 (10送2 via BONUS_RATIO alias)", events[0].SplitRatio)
	}
}

func TestParseHKEXDividendPlan_SurfacesAPIError(t *testing.T) {
	if _, err := parseHKEXDividendPlan([]byte(fixtureHKEXAPIError), "00005", time.Time{}); err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestParseHKEXDividendPlan_SinceFilter(t *testing.T) {
	// Tencent fixture has 2024-05-23 + 2025-05-22. since=2025-01-01
	// must drop the older row.
	since := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	events, err := parseHKEXDividendPlan([]byte(fixtureHKEXTencent2025FinalDividend), "00700", since)
	if err != nil {
		t.Fatalf("parse err = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	if !events[0].ExDate.Equal(time.Date(2025, 5, 22, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("ExDate = %v, want 2025-05-22", events[0].ExDate)
	}
}

func TestHKEXProvider_FetchEvents_HTTPRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sanity-check the upstream contract: matching headers
		// + proper SECUCODE filter shape (with .HK suffix).
		if got := r.Header.Get("User-Agent"); got == "" {
			t.Errorf("User-Agent missing")
		}
		if got := r.Header.Get("Referer"); got == "" {
			t.Errorf("Referer missing")
		}
		if got := r.URL.Query().Get("filter"); got != `(SECUCODE="00700.HK")` {
			t.Errorf("filter = %q, want (SECUCODE=\"00700.HK\")", got)
		}
		if got := r.URL.Query().Get("reportName"); got != "RPT_HKF10_DIVIDENDPLAN" {
			t.Errorf("reportName = %q, want RPT_HKF10_DIVIDENDPLAN", got)
		}
		_, _ = w.Write([]byte(fixtureHKEXTencent2025FinalDividend))
	}))
	defer srv.Close()

	p := HKEXProvider{BaseURL: srv.URL}
	events, err := p.FetchEvents(context.Background(), "0700.HK", time.Time{})
	if err != nil {
		t.Fatalf("FetchEvents err = %v", err)
	}
	if len(events) != 2 {
		t.Errorf("len(events) = %d, want 2", len(events))
	}
}

func TestHKEXProvider_FetchEvents_404TreatedAsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := HKEXProvider{BaseURL: srv.URL}
	events, err := p.FetchEvents(context.Background(), "99999", time.Time{})
	if err != nil {
		t.Fatalf("404 should not error; got %v", err)
	}
	if len(events) != 0 {
		t.Errorf("len(events) = %d, want 0", len(events))
	}
}

func TestHKEXProvider_FetchEvents_500SurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream backend down"))
	}))
	defer srv.Close()

	p := HKEXProvider{BaseURL: srv.URL}
	if _, err := p.FetchEvents(context.Background(), "00700", time.Time{}); err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestHKEXProvider_FetchEvents_RejectsEmptySymbol(t *testing.T) {
	p := HKEXProvider{}
	_, err := p.FetchEvents(context.Background(), "  ", time.Time{})
	if err == nil {
		t.Fatal("expected error for empty symbol, got nil")
	}
	if !strings.Contains(err.Error(), "empty/invalid symbol") {
		t.Errorf("error = %q, want it to mention empty/invalid symbol", err.Error())
	}
}

func TestHKEXProvider_FetchEvents_RejectsNonNumericSymbol(t *testing.T) {
	p := HKEXProvider{}
	_, err := p.FetchEvents(context.Background(), "TENCENT", time.Time{})
	if err == nil {
		t.Fatal("expected error for non-numeric symbol, got nil")
	}
}

func TestNormalizeHKCode(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Tencent — every shape produces the same canonical 5-digit.
		{"00700", "00700"},
		{"0700", "00700"},
		{"700", "00700"},
		{"0700.HK", "00700"},
		{"00700.HK", "00700"},
		{"HKEX:00700", "00700"},
		{"HKEX:0700", "00700"},
		{"hkex:0700.hk", "00700"},
		// Single-digit edge: HKEX itself is 00388; pre-IPO unique
		// codes like "1" don't trade as listings, but if some
		// caller passes them through we still produce a valid
		// 5-digit form rather than panic.
		{"5", "00005"},
		// Whitespace / case noise — operator pasted from a
		// statement.
		{"  0700.HK  ", "00700"},
		// Garbage in → empty out (caller surfaces a clean
		// error before hitting the upstream).
		{"", ""},
		{"TENCENT", ""},
		{"00700.SH", ""},        // wrong-suffix should not silently match
		{"123456", ""},          // 6+ digits aren't HK listings
		{"08032ABC", ""},
	}
	for _, tc := range cases {
		if got := normalizeHKCode(tc.in); got != tc.want {
			t.Errorf("normalizeHKCode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestInstrumentKeyForHKEX(t *testing.T) {
	if got := instrumentKeyForHKEX("00700"); got != "HKEX:00700" {
		t.Errorf("instrumentKeyForHKEX(00700) = %q, want HKEX:00700", got)
	}
}
