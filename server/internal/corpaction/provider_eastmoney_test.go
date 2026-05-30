package corpaction

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fixtureEastmoneyTengjingMay2026 is the JSON shape East Money
// returns for 688195 around 2026-05-29 (the same 10转4 + 1.64元/10
// event that triggered Card A in the first place). Captured from a
// real call in 2026-05 and trimmed to the columns the parser uses.
//
// Field semantics (verified live):
//   - PRETAX_BONUS_RMB = 派现 per 10 shares (含税)
//   - BONUS_IT_RATIO   = 送+转 combined per 10 shares
//   - ASSIGN_PROGRESS  = 实施分配 / 不分配 / 预案 / ...
const fixtureEastmoneyTengjingMay2026 = `{
  "version": "f7c3...",
  "result": {
    "pages": 1,
    "data": [
      {
        "SECURITY_CODE": "688195",
        "SECURITY_NAME_ABBR": "腾景科技",
        "EX_DIVIDEND_DATE": "2026-05-29 00:00:00",
        "PRETAX_BONUS_RMB": 1.64,
        "BONUS_IT_RATIO": 4,
        "IT_RATIO": 4,
        "ASSIGN_PROGRESS": "实施分配",
        "IMPL_PLAN_PROFILE": "10转4股派1.64元(含税,扣税后1.476元)"
      },
      {
        "SECURITY_CODE": "688195",
        "SECURITY_NAME_ABBR": "腾景科技",
        "EX_DIVIDEND_DATE": "2025-04-29 00:00:00",
        "PRETAX_BONUS_RMB": 0.7,
        "BONUS_IT_RATIO": null,
        "IT_RATIO": null,
        "ASSIGN_PROGRESS": "实施分配"
      }
    ]
  },
  "success": true
}`

// fixtureEastmoneyAnnouncedNotImplemented covers the rare row that
// is filed but the listed company has not yet executed. We must
// drop it — applying a yet-to-happen split would produce a future
// cost-basis change that's wrong today.
const fixtureEastmoneyAnnouncedNotImplemented = `{
  "result": {
    "data": [
      {
        "SECURITY_CODE": "300750",
        "EX_DIVIDEND_DATE": "2026-08-01 00:00:00",
        "PRETAX_BONUS_RMB": 5.0,
        "BONUS_IT_RATIO": null,
        "ASSIGN_PROGRESS": "董事会决议通过"
      }
    ]
  },
  "success": true
}`

// fixtureEastmoneyTransferOnly covers a pure 转股 row (10转5),
// no cash. Modeled on BYD-style 002594 history. The combined
// BONUS_IT_RATIO captures it (5) and the parser must classify
// this as "stock_dividend" with split_ratio=1.5 / cash=0.
const fixtureEastmoneyTransferOnly = `{
  "result": {
    "data": [
      {
        "SECURITY_CODE": "002594",
        "EX_DIVIDEND_DATE": "2024-06-13 00:00:00",
        "PRETAX_BONUS_RMB": 0,
        "BONUS_IT_RATIO": 5,
        "IT_RATIO": 5,
        "ASSIGN_PROGRESS": "实施分配"
      }
    ]
  },
  "success": true
}`

// fixtureEastmoneyAllZero is the degenerate "no payout, no shares
// changed" row some tickers carry as filler. It must produce zero
// events so an idempotent sync run doesn't insert noise.
const fixtureEastmoneyAllZero = `{
  "result": {"data": [{
    "SECURITY_CODE": "600000",
    "EX_DIVIDEND_DATE": "2023-04-12 00:00:00",
    "PRETAX_BONUS_RMB": 0,
    "BONUS_IT_RATIO": 0,
    "ASSIGN_PROGRESS": "实施分配"
  }]},
  "success": true
}`

// fixtureEastmoneyApiError mirrors what East Money returns when the
// backend (rarely) bombs out. The provider should surface this as
// an error rather than swallow it as zero rows.
const fixtureEastmoneyApiError = `{
  "result": {"data": []},
  "success": false,
  "message": "system busy, retry later"
}`

func TestParseEastmoneyShareBonus_TengjingHappyPath(t *testing.T) {
	events, err := parseEastmoneyShareBonus([]byte(fixtureEastmoneyTengjingMay2026), "688195", time.Time{})
	if err != nil {
		t.Fatalf("parse err = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2; got=%+v", len(events), events)
	}

	// Sorted ascending by ex_date — first row is 2025-04-29 cash-only,
	// second is 2026-05-29 combined.
	if !events[0].ExDate.Equal(time.Date(2025, 4, 29, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("event[0].ExDate = %v", events[0].ExDate)
	}
	if events[0].ActionType != "cash_dividend" {
		t.Errorf("event[0].ActionType = %q, want cash_dividend", events[0].ActionType)
	}
	if events[0].SplitRatio != 1.0 {
		t.Errorf("event[0].SplitRatio = %v, want 1.0", events[0].SplitRatio)
	}
	// 0.7 / 10 = 0.07
	if events[0].CashDividend != 0.07 {
		t.Errorf("event[0].CashDividend = %v, want 0.07", events[0].CashDividend)
	}

	got := events[1]
	if !got.ExDate.Equal(time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("event[1].ExDate = %v", got.ExDate)
	}
	if got.ActionType != "combined" {
		t.Errorf("event[1].ActionType = %q, want combined", got.ActionType)
	}
	// 1 + (4 + 0) / 10 = 1.4
	if got.SplitRatio != 1.4 {
		t.Errorf("event[1].SplitRatio = %v, want 1.4", got.SplitRatio)
	}
	// 1.64 / 10 = 0.164
	if got.CashDividend != 0.164 {
		t.Errorf("event[1].CashDividend = %v, want 0.164", got.CashDividend)
	}
	if got.Source != "eastmoney" {
		t.Errorf("event[1].Source = %q, want eastmoney", got.Source)
	}
	if got.InstrumentKey != "SSE:688195" {
		t.Errorf("event[1].InstrumentKey = %q, want SSE:688195", got.InstrumentKey)
	}
}

func TestParseEastmoneyShareBonus_DropsNotImplemented(t *testing.T) {
	events, err := parseEastmoneyShareBonus([]byte(fixtureEastmoneyAnnouncedNotImplemented), "300750", time.Time{})
	if err != nil {
		t.Fatalf("parse err = %v", err)
	}
	if len(events) != 0 {
		t.Errorf("len(events) = %d, want 0; not-yet-实施 rows must be dropped", len(events))
	}
}

func TestParseEastmoneyShareBonus_TransferIsStockDividend(t *testing.T) {
	events, err := parseEastmoneyShareBonus([]byte(fixtureEastmoneyTransferOnly), "002594", time.Time{})
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
	// 1 + 5 / 10 = 1.5
	if got.SplitRatio != 1.5 {
		t.Errorf("SplitRatio = %v, want 1.5", got.SplitRatio)
	}
	if got.CashDividend != 0 {
		t.Errorf("CashDividend = %v, want 0", got.CashDividend)
	}
	if got.InstrumentKey != "SZSE:002594" {
		t.Errorf("InstrumentKey = %q, want SZSE:002594", got.InstrumentKey)
	}
}

func TestParseEastmoneyShareBonus_DropsAllZero(t *testing.T) {
	events, err := parseEastmoneyShareBonus([]byte(fixtureEastmoneyAllZero), "600000", time.Time{})
	if err != nil {
		t.Fatalf("parse err = %v", err)
	}
	if len(events) != 0 {
		t.Errorf("len(events) = %d, want 0", len(events))
	}
}

func TestParseEastmoneyShareBonus_SurfacesApiError(t *testing.T) {
	_, err := parseEastmoneyShareBonus([]byte(fixtureEastmoneyApiError), "600000", time.Time{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestParseEastmoneyShareBonus_SinceFilter(t *testing.T) {
	// The Tengjing fixture has events on 2025-04-29 and 2026-05-29.
	// since=2026-01-01 should drop the older row.
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	events, err := parseEastmoneyShareBonus([]byte(fixtureEastmoneyTengjingMay2026), "688195", since)
	if err != nil {
		t.Fatalf("parse err = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	if !events[0].ExDate.Equal(time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("event[0].ExDate = %v, want 2026-05-29", events[0].ExDate)
	}
}

func TestEastmoneyProvider_FetchEvents_HTTPRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sanity: client headers must be set so East Money doesn't 412.
		if got := r.Header.Get("User-Agent"); got == "" {
			t.Errorf("User-Agent missing")
		}
		if got := r.Header.Get("Referer"); got == "" {
			t.Errorf("Referer missing")
		}
		// And the filter must contain the bare 6-digit code.
		if got := r.URL.Query().Get("filter"); got != `(SECURITY_CODE="688195")` {
			t.Errorf("filter = %q, want (SECURITY_CODE=\"688195\")", got)
		}
		_, _ = w.Write([]byte(fixtureEastmoneyTengjingMay2026))
	}))
	defer srv.Close()

	p := EastmoneyProvider{BaseURL: srv.URL}
	events, err := p.FetchEvents(context.Background(), "688195.SH", time.Time{})
	if err != nil {
		t.Fatalf("FetchEvents err = %v", err)
	}
	if len(events) != 2 {
		t.Errorf("len(events) = %d, want 2", len(events))
	}
}

func TestEastmoneyProvider_FetchEvents_404TreatedAsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := EastmoneyProvider{BaseURL: srv.URL}
	events, err := p.FetchEvents(context.Background(), "999999", time.Time{})
	if err != nil {
		t.Fatalf("404 should not error; got %v", err)
	}
	if len(events) != 0 {
		t.Errorf("len(events) = %d, want 0", len(events))
	}
}

func TestEastmoneyProvider_FetchEvents_500SurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream backend down"))
	}))
	defer srv.Close()

	p := EastmoneyProvider{BaseURL: srv.URL}
	if _, err := p.FetchEvents(context.Background(), "600000", time.Time{}); err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestStripCSIExchangeSuffix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"688195", "688195"},
		{"688195.SH", "688195"},
		{"688195.SS", "688195"},
		{"002594.SZ", "002594"},
		{"  600000.SH  ", "600000"},
		{"688195.bj", "688195"},
	}
	for _, tc := range cases {
		if got := stripCSIExchangeSuffix(tc.in); got != tc.want {
			t.Errorf("strip(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestInstrumentKeyForCSI(t *testing.T) {
	cases := []struct {
		code, want string
	}{
		{"688195", "SSE:688195"},
		{"600000", "SSE:600000"},
		{"603001", "SSE:603001"},
		{"605008", "SSE:605008"},
		{"000001", "SZSE:000001"},
		{"002594", "SZSE:002594"},
		{"300750", "SZSE:300750"},
		{"301236", "SZSE:301236"},
		{"830799", "BJSE:830799"},
		{"920100", "BJSE:920100"},
		// Defensive default — strange prefixes route to SSE.
		{"500001", "SSE:500001"},
	}
	for _, tc := range cases {
		if got := instrumentKeyForCSI(tc.code); got != tc.want {
			t.Errorf("instrumentKeyForCSI(%q) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

func TestDecodeRawFloat_AliasFallback(t *testing.T) {
	// First key missing → fall back to second.
	rowMap := map[string]json.RawMessage{
		"PRETAX_BONUS_RMB": json.RawMessage("1.64"),
	}
	got := decodeRawFloat(rowMap, "PRETAX_BONUS", "PRETAX_BONUS_RMB")
	if got != 1.64 {
		t.Errorf("got %v, want 1.64", got)
	}

	// Numeric encoded as JSON string also decodes (East Money has
	// historically wrapped numbers in quotes for some reports).
	rowMap = map[string]json.RawMessage{
		"PRETAX_BONUS_RMB": json.RawMessage(`"3.50"`),
	}
	got = decodeRawFloat(rowMap, "PRETAX_BONUS_RMB")
	if got != 3.5 {
		t.Errorf("string-shape decode got %v, want 3.5", got)
	}

	// Null cells return 0 silently.
	rowMap = map[string]json.RawMessage{
		"PRETAX_BONUS_RMB": json.RawMessage("null"),
	}
	got = decodeRawFloat(rowMap, "PRETAX_BONUS_RMB")
	if got != 0 {
		t.Errorf("null cell got %v, want 0", got)
	}
}
