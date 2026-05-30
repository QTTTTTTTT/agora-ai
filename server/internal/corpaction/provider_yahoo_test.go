package corpaction

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// nvdaSplitFixture is a trimmed real Yahoo response captured for
// NVDA between 2020-01-01 and 2026-01-01. It contains:
//   - 2021-07-20 4:1 forward split (numerator 4, denominator 1)
//   - 2024-06-10 10:1 forward split (numerator 10, denominator 1)
//   - 4 quarterly cash dividends in 2024-25
//
// Trimmed to the fields parseChartEvents reads. We intentionally
// leave the json keys with their Yahoo-side casing so a parser
// regression that, say, reads the wrong key surfaces immediately.
const nvdaSplitFixture = `{
  "chart": {
    "result": [{
      "meta": { "currency": "USD", "symbol": "NVDA" },
      "events": {
        "dividends": {
          "1717428600": { "amount": 0.01, "date": 1717428600 },
          "1726234200": { "amount": 0.01, "date": 1726234200 },
          "1734533400": { "amount": 0.01, "date": 1734533400 },
          "1742229000": { "amount": 0.01, "date": 1742229000 }
        },
        "splits": {
          "1626787800": {
            "date": 1626787800,
            "numerator": 4,
            "denominator": 1,
            "splitRatio": "4:1"
          },
          "1717977600": {
            "date": 1717977600,
            "numerator": 10,
            "denominator": 1,
            "splitRatio": "10:1"
          }
        }
      }
    }],
    "error": null
  }
}`

// reverseSplitFixture covers a 1:5 reverse split (small-cap
// post-delisting-threat scenario). split_ratio must come out as
// 0.2, not 5 — that sign error is the most common production bug.
const reverseSplitFixture = `{
  "chart": {
    "result": [{
      "events": {
        "splits": {
          "1640995200": {
            "date": 1640995200,
            "numerator": 1,
            "denominator": 5,
            "splitRatio": "1:5"
          }
        }
      }
    }]
  }
}`

// emptyEventsFixture is what Yahoo returns for tickers that simply
// haven't had any dividends or splits in the window — an empty
// events block, not a missing one. parseChartEvents must return an
// empty slice cleanly.
const emptyEventsFixture = `{ "chart": { "result": [{ "events": {} }] } }`

// TestParseChartEvents_NvidiaSplitsAndDivs is the primary parser
// regression. NVDA's two stock splits are the canonical reference
// for any US-equity adjustment library and the dividend stream is
// dense enough to exercise the dict-keyed-by-timestamp shape that
// fooled an earlier implementation.
func TestParseChartEvents_NvidiaSplitsAndDivs(t *testing.T) {
	got, err := parseChartEvents([]byte(nvdaSplitFixture), "NVDA")
	if err != nil {
		t.Fatalf("parseChartEvents: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("got %d events, want 6 (2 splits + 4 divs); events=%+v", len(got), got)
	}

	var splits, divs int
	for _, e := range got {
		if e.InstrumentKey != "NASDAQ:NVDA" {
			t.Errorf("InstrumentKey = %q, want NASDAQ:NVDA", e.InstrumentKey)
		}
		if e.Source != "yahoo" {
			t.Errorf("Source = %q, want yahoo", e.Source)
		}
		switch e.ActionType {
		case "split":
			splits++
			if e.SplitRatio != 4.0 && e.SplitRatio != 10.0 {
				t.Errorf("split ratio = %v, want 4 or 10", e.SplitRatio)
			}
			if e.CashDividend != 0 {
				t.Errorf("split has nonzero CashDividend = %v", e.CashDividend)
			}
		case "cash_dividend":
			divs++
			if e.SplitRatio != 1.0 {
				t.Errorf("cash dividend SplitRatio = %v, want 1", e.SplitRatio)
			}
			if e.CashDividend != 0.01 {
				t.Errorf("CashDividend = %v, want 0.01", e.CashDividend)
			}
		default:
			t.Errorf("unexpected ActionType %q", e.ActionType)
		}
	}
	if splits != 2 || divs != 4 {
		t.Errorf("split=%d div=%d, want 2/4", splits, divs)
	}

	// Sort invariant — events come out ascending by ex_date.
	for i := 1; i < len(got); i++ {
		if got[i-1].ExDate.After(got[i].ExDate) {
			t.Errorf("events not sorted ascending: idx %d ex %v > idx %d ex %v",
				i-1, got[i-1].ExDate, i, got[i].ExDate)
		}
	}
}

// TestParseChartEvents_ReverseSplitSignConvention is the regression
// for the most expensive sign-mistake in this domain: a 1:5 reverse
// split must serialise as split_ratio=0.2, NOT 5.0. If the parser
// gets this wrong the applier will multiply shares by 5 instead of
// dividing them, inventing 4x phantom shares and crashing the fund
// audit.
func TestParseChartEvents_ReverseSplitSignConvention(t *testing.T) {
	got, err := parseChartEvents([]byte(reverseSplitFixture), "XYZ")
	if err != nil {
		t.Fatalf("parseChartEvents: %v", err)
	}
	if len(got) != 1 || got[0].ActionType != "split" {
		t.Fatalf("want exactly 1 split, got %+v", got)
	}
	if got[0].SplitRatio != 0.2 {
		t.Errorf("SplitRatio = %v, want 0.2 (1:5 reverse)", got[0].SplitRatio)
	}
}

// TestParseChartEvents_NoEvents pins the "well-listed but quiet"
// branch: tickers like BRK-A that haven't split or paid a dividend
// in the window must come back as a cleanly empty slice, not nil
// with a parse error.
func TestParseChartEvents_NoEvents(t *testing.T) {
	got, err := parseChartEvents([]byte(emptyEventsFixture), "BRK-A")
	if err != nil {
		t.Fatalf("parseChartEvents: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 events, got %+v", got)
	}
}

// TestParseChartEvents_RejectsApiError covers the case where Yahoo
// replies 200 OK with an embedded {"error": {...}} payload (rate
// limit, geo-block, malformed symbol). We must surface this as a
// real error so the daily sweep retries / reports it; silently
// returning [] would create a confusing "no events" misdiagnosis.
func TestParseChartEvents_RejectsApiError(t *testing.T) {
	body := `{"chart":{"result":[],"error":{"code":"Bad Request","description":"Symbol invalid"}}}`
	_, err := parseChartEvents([]byte(body), "BAD")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Bad Request") {
		t.Errorf("error = %v, want it to mention Bad Request", err)
	}
}

// TestSplitRatioToFloat covers every fallback branch. Yahoo's
// historical export sometimes omits numerator/denominator and only
// supplies the "10:1" string; sometimes the floats are wrong and
// the string is the only truth. The function must be defensive on
// both axes.
func TestSplitRatioToFloat(t *testing.T) {
	cases := []struct {
		name string
		num  float64
		den  float64
		raw  string
		want float64
	}{
		{"floats forward", 10, 1, "", 10.0},
		{"floats reverse", 1, 5, "", 0.2},
		{"string forward", 0, 0, "4:1", 4.0},
		{"string reverse", 0, 0, "1:5", 0.2},
		{"prefer floats over string", 10, 1, "garbage", 10.0},
		{"trim whitespace in string", 0, 0, " 7 : 2 ", 3.5},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := splitRatioToFloat(tc.num, tc.den, tc.raw)
			if err != nil {
				t.Fatalf("splitRatioToFloat: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSplitRatioToFloat_RejectsBadInput covers the malformed-
// string path, which we want to fail rather than silently return
// 0 (a 0 ratio in the applier is rejected by validateEvent but
// it's nicer to surface the parse error at the source).
func TestSplitRatioToFloat_RejectsBadInput(t *testing.T) {
	cases := []string{"", "abc", "1:0", "5", ":5"}
	for _, s := range cases {
		s := s
		t.Run("raw="+s, func(t *testing.T) {
			if _, err := splitRatioToFloat(0, 0, s); err == nil {
				t.Errorf("expected error for %q, got nil", s)
			}
		})
	}
}

// TestInstrumentKeyForYahoo locks the mapping between Yahoo
// suffixes and our schema's exchange prefix. Getting this wrong
// would make the applier fail to find any holding_positions row
// because the key wouldn't match.
func TestInstrumentKeyForYahoo(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"NVDA", "NASDAQ:NVDA"},
		{"nvda", "NASDAQ:NVDA"},
		{"AAPL", "NASDAQ:AAPL"},
		{"0700.HK", "HKEX:0700"},
		{"600519.SS", "SSE:600519"},
		{"000001.SZ", "SZSE:000001"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			got := instrumentKeyForYahoo(tc.in)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFetchEvents_HTTPRoundTrip stands up an httptest server that
// returns the NVDA fixture for the canonical chart endpoint and
// asserts FetchEvents stitches URL building + HTTP + parser
// together correctly. This is the only test in this file that
// exercises the network code path.
func TestFetchEvents_HTTPRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v8/finance/chart/NVDA") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("events"); got != "div|split" {
			t.Errorf("events query = %q, want div|split", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(nvdaSplitFixture))
	}))
	defer srv.Close()

	p := &YahooProvider{BaseURL: srv.URL}
	since := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	got, err := p.FetchEvents(context.Background(), "NVDA", since)
	if err != nil {
		t.Fatalf("FetchEvents: %v", err)
	}
	if len(got) != 6 {
		t.Errorf("len = %d, want 6", len(got))
	}
}

// TestFetchEvents_404TreatedAsEmpty confirms a delisted / unknown
// symbol response (Yahoo replies 404) is folded into an empty
// slice with no error. The daily sweep relies on this so a single
// stale ticker can't kill the batch.
func TestFetchEvents_404TreatedAsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	p := &YahooProvider{BaseURL: srv.URL}
	got, err := p.FetchEvents(context.Background(), "DELISTED", time.Now().AddDate(-1, 0, 0))
	if err != nil {
		t.Fatalf("FetchEvents: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 events, got %d", len(got))
	}
}

// TestFetchEvents_500SurfacesError makes sure a real Yahoo outage
// (500) propagates as an error so the sweeper retries with
// exponential backoff rather than treating it as "no events" and
// missing real data.
func TestFetchEvents_500SurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "yahoo melted", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := &YahooProvider{BaseURL: srv.URL}
	if _, err := p.FetchEvents(context.Background(), "NVDA", time.Time{}); err == nil {
		t.Fatal("want error, got nil")
	}
}
