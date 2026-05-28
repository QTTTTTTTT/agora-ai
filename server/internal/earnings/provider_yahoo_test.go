package earnings

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestYahooProviderFetchSuccess(t *testing.T) {
	// July 31 2024 / Aug 1 2024 — Yahoo's estimate-window shape.
	earliestEpoch := int64(1722384000)
	latestEpoch := int64(1722470400)
	server := newYahooStub(t, map[string]yahooStubResponse{
		"AAPL": {
			body: yahooFixture(earliestEpoch, latestEpoch, true),
		},
		"MSFT": {
			body: yahooFixture(int64(1722643200), 0, false),
		},
	})
	defer server.Close()

	p := &YahooProvider{BaseURL: server.URL}
	got, err := p.Fetch(context.Background(), FetchRequest{
		Symbols: []string{"AAPL", "MSFT"},
		Market:  "us_equity",
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d", len(got))
	}
	bySym := map[string]Event{}
	for _, e := range got {
		bySym[e.Symbol] = e
	}
	if !bySym["AAPL"].EventDate.Equal(time.Unix(earliestEpoch, 0).UTC()) {
		t.Errorf("AAPL date = %s, want %s", bySym["AAPL"].EventDate,
			time.Unix(earliestEpoch, 0).UTC())
	}
	if bySym["AAPL"].Source != "yahoo" {
		t.Errorf("AAPL source = %q, want %q", bySym["AAPL"].Source, "yahoo")
	}
	if bySym["AAPL"].TimeOfDay != TimeUnknown {
		t.Errorf("AAPL time-of-day = %q, want %q (v10 doesn't expose it)",
			bySym["AAPL"].TimeOfDay, TimeUnknown)
	}
}

func TestYahooProviderHandlesPartialFailure(t *testing.T) {
	// One symbol is fine, one throttled — expect the fine one to
	// land in the slice and the throttled one to be dropped
	// silently. This is the production contract: per-symbol
	// errors must not poison the whole batch.
	server := newYahooStub(t, map[string]yahooStubResponse{
		"AAPL": {
			body: yahooFixture(int64(1722384000), 0, false),
		},
		"NFLX": {
			status: http.StatusTooManyRequests,
			body:   `{"error":"too many requests"}`,
		},
	})
	defer server.Close()

	p := &YahooProvider{BaseURL: server.URL, Concurrency: 1}
	got, err := p.Fetch(context.Background(), FetchRequest{
		Symbols: []string{"AAPL", "NFLX"},
		Market:  "us_equity",
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event after throttle, got %d", len(got))
	}
	if got[0].Symbol != "AAPL" {
		t.Errorf("survivor = %q, want AAPL", got[0].Symbol)
	}
}

func TestYahooProviderHandlesEmptyCalendar(t *testing.T) {
	// ETFs / index symbols have no earnings — Yahoo returns
	// result with calendarEvents.earnings either nil or with an
	// empty earningsDate. Both shapes must map to "no event"
	// (nil, nil) without error.
	server := newYahooStub(t, map[string]yahooStubResponse{
		"SPY": {
			body: `{"quoteSummary":{"result":[{"calendarEvents":{}}],"error":null}}`,
		},
		"QQQ": {
			body: `{"quoteSummary":{"result":[{"calendarEvents":{"earnings":{"earningsDate":[]}}}],"error":null}}`,
		},
	})
	defer server.Close()

	p := &YahooProvider{BaseURL: server.URL}
	got, err := p.Fetch(context.Background(), FetchRequest{
		Symbols: []string{"SPY", "QQQ"},
		Market:  "us_equity",
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 events for ETFs, got %d", len(got))
	}
}

func TestYahooProviderTolerantOfFormattedObjectShape(t *testing.T) {
	// Older Yahoo edge caches sometimes return earningsDate as
	// {raw,fmt} objects even with formatted=false. Our custom
	// UnmarshalJSON must handle both.
	body := `{"quoteSummary":{"result":[{"calendarEvents":{"earnings":{"earningsDate":[{"raw":1722384000,"fmt":"2024-07-31"}]}}}],"error":null}}`
	server := newYahooStub(t, map[string]yahooStubResponse{
		"AAPL": {body: body},
	})
	defer server.Close()

	p := &YahooProvider{BaseURL: server.URL}
	got, err := p.Fetch(context.Background(), FetchRequest{
		Symbols: []string{"AAPL"},
		Market:  "us_equity",
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0].EventDate.Unix() != 1722384000 {
		t.Errorf("epoch = %d, want %d", got[0].EventDate.Unix(), 1722384000)
	}
}

func TestYahooProviderUsesRealUserAgent(t *testing.T) {
	var seenUA atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUA.Store(r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, yahooFixture(int64(1722384000), 0, false))
	}))
	defer srv.Close()

	p := &YahooProvider{BaseURL: srv.URL}
	_, err := p.Fetch(context.Background(), FetchRequest{
		Symbols: []string{"AAPL"},
		Market:  "us_equity",
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	ua, _ := seenUA.Load().(string)
	if !strings.Contains(ua, "Mozilla") {
		t.Errorf("UA = %q, expected a browser-shaped UA (Yahoo rejects bare clients)", ua)
	}
}

func TestYahooProviderHonoursConcurrencyDefault(t *testing.T) {
	// Verify that even with default Concurrency=0 the fetch
	// completes and respects the in-flight cap.
	const symbolCount = 6
	var inflight atomic.Int32
	var maxObserved atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			prev := maxObserved.Load()
			if cur <= prev || maxObserved.CompareAndSwap(prev, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, yahooFixture(int64(1722384000), 0, false))
	}))
	defer srv.Close()

	syms := make([]string, symbolCount)
	for i := range syms {
		syms[i] = fmt.Sprintf("SYM%d", i)
	}
	p := &YahooProvider{BaseURL: srv.URL} // Concurrency=0 → default 3
	got, err := p.Fetch(context.Background(), FetchRequest{
		Symbols: syms,
		Market:  "us_equity",
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != symbolCount {
		t.Errorf("got %d events, want %d", len(got), symbolCount)
	}
	if max := maxObserved.Load(); max > 3 {
		t.Errorf("max in-flight = %d, want <= 3 (default concurrency)", max)
	}
}

func TestMapToYahooSymbol(t *testing.T) {
	cases := []struct {
		sym, market, want string
	}{
		{"AAPL", "us_equity", "AAPL"},
		{"aapl", "us_equity", "AAPL"},
		{"AAPL", "", "AAPL"},
		{"600519.SS", "a_share", "600519.SS"},   // already suffixed
		{"600519", "a_share", "600519.SS"},      // SH
		{"000001", "a_share", "000001.SZ"},      // SZ
		{"300750", "a_share", "300750.SZ"},      // SZ ChiNext
		{"00700", "hk", "700.HK"},               // strip leading zeros
		{"700", "hk", "700.HK"},                 // unpadded HK
		{"600519", "", ""},                      // 6-digit, no market → punt
		{"AAPL", "crypto", "AAPL"},              // unknown markets pass through
	}
	for _, c := range cases {
		if got := mapToYahooSymbol(c.sym, c.market); got != c.want {
			t.Errorf("mapToYahooSymbol(%q, %q) = %q, want %q",
				c.sym, c.market, got, c.want)
		}
	}
}

func TestYahooProviderRespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
			fmt.Fprint(w, yahooFixture(int64(1722384000), 0, false))
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	p := &YahooProvider{BaseURL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	got, err := p.Fetch(ctx, FetchRequest{
		Symbols: []string{"AAPL"},
		Market:  "us_equity",
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d events after context timeout, want 0", len(got))
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

type yahooStubResponse struct {
	status int    // default 200
	body   string // raw JSON
}

func newYahooStub(t *testing.T, byPathSymbol map[string]yahooStubResponse) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// path = /v10/finance/quoteSummary/{SYM}
		parts := strings.Split(r.URL.Path, "/")
		sym := parts[len(parts)-1]
		entry, ok := byPathSymbol[sym]
		if !ok {
			http.Error(w, "no fixture", http.StatusNotFound)
			return
		}
		status := entry.status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, entry.body)
	}))
}

// yahooFixture builds a quoteSummary calendarEvents JSON blob with
// 1 or 2 earningsDate entries — mirroring the shapes Yahoo serves
// for confirmed (one date) vs estimated (date range) releases.
func yahooFixture(d1, d2 int64, estimate bool) string {
	dates := []int64{d1}
	if d2 > 0 {
		dates = append(dates, d2)
	}
	payload := map[string]any{
		"quoteSummary": map[string]any{
			"result": []map[string]any{
				{
					"calendarEvents": map[string]any{
						"earnings": map[string]any{
							"earningsDate":           dates,
							"isEarningsDateEstimate": estimate,
						},
					},
				},
			},
			"error": nil,
		},
	}
	out, _ := json.Marshal(payload)
	return string(out)
}
