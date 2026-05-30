package ohlc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEastmoneySecidMapping pins the symbol→secid translation table
// because a wrong prefix is invisible: East Money silently returns
// klines=null for unknown secids, which surfaces to the dashboard
// as "no-data" rather than a loud error. Catching prefix bugs
// here prevents that silent regression.
func TestEastmoneySecidMapping(t *testing.T) {
	cases := []struct {
		symbol string
		want   string
		ok     bool
	}{
		// SSE indices.
		{"000300.SS", "1.000300", true},
		{"000688.SS", "1.000688", true},
		{"000905.SS", "1.000905", true},
		// SZSE index (ChiNext).
		{"399006.SZ", "0.399006", true},
		// SSE stock.
		{"688195.SS", "1.688195", true},
		// SZSE stock (ChiNext company).
		{"300750.SZ", "0.300750", true},
		// HK stock — left-pad to 5 digits.
		{"700.HK", "116.00700", true},
		{"00700.HK", "116.00700", true},
		// Beijing stock.
		{"920002.BJ", "0.920002", true},
		// Lower-case suffix.
		{"000300.ss", "1.000300", true},
		// US ticker — not in scheme, must return ok=false.
		{"AAPL", "", false},
		{"BTCUSDT", "", false},
		// Empty / malformed.
		{"", "", false},
		{".SS", "", false},
		{"abc.SS", "", false},
	}
	for _, tc := range cases {
		got, ok := emSecid(tc.symbol)
		if ok != tc.ok || got != tc.want {
			t.Errorf("emSecid(%q) = (%q, %v), want (%q, %v)", tc.symbol, got, ok, tc.want, tc.ok)
		}
	}
}

// TestEastmoneyParsesKLineJSON exercises the full Fetch path against
// a stub server returning a real-shape East Money response. The
// klines list is canonical: oldest-first, comma-separated, with
// extra fields after the columns we care about.
func TestEastmoneyParsesKLineJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("secid"); got != "0.399006" {
			t.Errorf("secid = %q, want 0.399006", got)
		}
		if got := r.URL.Query().Get("klt"); got != "101" {
			t.Errorf("klt = %q, want 101", got)
		}
		if got := r.URL.Query().Get("fqt"); got != "1" {
			t.Errorf("fqt = %q, want 1 (forward-adjusted)", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"rc":0,"rt":17,"svr":2887,"lt":1,"full":1,
			"data":{
				"code":"399006","market":0,"name":"创业板指",
				"klines":[
					"2024-01-02,3050.10,3025.30,3055.20,3010.40,1234567,12345678,1.5,-1.0,-25.30,0.5",
					"2024-01-03,3025.30,3060.50,3070.10,3020.00,1334567,13345678,1.6,1.2,35.20,0.6",
					"2024-01-04,3060.50,3045.20,3080.40,3040.10,1434567,14345678,1.3,-0.5,-15.30,0.4"
				]
			}
		}`))
	}))
	defer srv.Close()

	p := &EastmoneyProvider{BaseURL: srv.URL}
	bars, err := p.Fetch(context.Background(), FetchRequest{
		Symbol:    "399006.SZ",
		Market:    "a_share",
		LookbackN: 60,
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got, want := len(bars), 3; got != want {
		t.Fatalf("bars = %d, want %d", got, want)
	}
	if bars[0].Time.Format("2006-01-02") != "2024-01-02" {
		t.Errorf("bars[0].Time = %v, want 2024-01-02", bars[0].Time)
	}
	if bars[2].Close != 3045.20 {
		t.Errorf("bars[2].Close = %v, want 3045.20", bars[2].Close)
	}
}

// TestEastmoneyReturnsErrNoDataOnEmptyKlines guards the silent
// "delisted symbol" path: East Money returns 200 with an empty
// klines array rather than a 404. Treating that as ErrNoData lets
// the registry try the next provider.
func TestEastmoneyReturnsErrNoDataOnEmptyKlines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rc":0,"rt":17,"data":{"code":"999999","klines":[]}}`))
	}))
	defer srv.Close()

	p := &EastmoneyProvider{BaseURL: srv.URL}
	_, err := p.Fetch(context.Background(), FetchRequest{
		Symbol: "999999.SS",
		Market: "a_share",
	})
	if err == nil {
		t.Fatal("expected ErrNoData, got nil")
	}
	if !strings.Contains(err.Error(), "no data") && err != ErrNoData {
		t.Errorf("err = %v, want ErrNoData", err)
	}
}

// TestEastmoneySupportsDefault pins the default Markets list so a
// future "let's restrict East Money to indices" change surfaces in
// review rather than silently breaking A-share stock fallback.
func TestEastmoneySupportsDefault(t *testing.T) {
	p := &EastmoneyProvider{}
	if !p.Supports("a_share") {
		t.Error("default Markets should cover a_share")
	}
	if p.Supports("us_equity") {
		t.Error("default Markets should NOT include us_equity")
	}
	if p.Supports("crypto") {
		t.Error("default Markets should NOT include crypto")
	}
}

// TestEastmoneyRoutesNonAsharSymbolsToErrNoData makes sure a US
// ticker accidentally landed on this provider doesn't trigger a
// real upstream call (which would be wasteful and probably 4xx).
// We never reach the HTTP layer — the symbol check short-circuits.
func TestEastmoneyRoutesNonAshareSymbolsToErrNoData(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	p := &EastmoneyProvider{BaseURL: srv.URL}
	_, err := p.Fetch(context.Background(), FetchRequest{
		Symbol: "AAPL",
		Market: "a_share", // mismatch on purpose
	})
	if err != ErrNoData {
		t.Errorf("err = %v, want ErrNoData", err)
	}
	if called {
		t.Error("AAPL should NOT have hit the upstream")
	}
}

// TestEastmoneyFallsThroughToFallbackHostOnTransientError exercises
// the per-host fall-through chain. The primary host hangs up
// mid-request (closes the TCP connection before sending response
// headers — what push2his does under WAF rate-limit and what
// produces a bare "EOF" client error in Go); the fallback host
// returns a happy kline JSON. The provider must surface the
// fallback's data, not propagate the transient error.
func TestEastmoneyFallsThroughToFallbackHostOnTransientError(t *testing.T) {
	primaryCalls := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls++
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("server doesn't support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		_ = conn.Close()
	}))
	defer primary.Close()

	fallbackCalls := 0
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"rc":0,"rt":17,"data":{
				"code":"399006",
				"klines":[
					"2024-01-02,3050.10,3025.30,3055.20,3010.40,1234567",
					"2024-01-03,3025.30,3060.50,3070.10,3020.00,1334567"
				]
			}
		}`))
	}))
	defer fallback.Close()

	p := &EastmoneyProvider{
		BaseURL:          primary.URL,
		FallbackBaseURLs: []string{fallback.URL},
	}
	bars, err := p.Fetch(context.Background(), FetchRequest{
		Symbol:    "399006.SZ",
		Market:    "a_share",
		LookbackN: 30,
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got, want := len(bars), 2; got != want {
		t.Fatalf("bars = %d, want %d", got, want)
	}
	if primaryCalls < 1 {
		t.Errorf("primary should have been tried at least once, got %d", primaryCalls)
	}
	if fallbackCalls < 1 {
		t.Errorf("fallback should have been tried at least once, got %d", fallbackCalls)
	}
}

// TestEastmoneyDoesNotFallThroughOn4xx makes sure non-transient
// upstream errors (a deliberate "this symbol is bad" 400) stay
// on the primary host. Falling through would burn fallback
// budget on a request that will fail there too.
func TestEastmoneyDoesNotFallThroughOn4xx(t *testing.T) {
	primaryCalls := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls++
		http.Error(w, "bad symbol", http.StatusBadRequest)
	}))
	defer primary.Close()

	fallbackCalls := 0
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls++
	}))
	defer fallback.Close()

	p := &EastmoneyProvider{
		BaseURL:          primary.URL,
		FallbackBaseURLs: []string{fallback.URL},
	}
	_, err := p.Fetch(context.Background(), FetchRequest{
		Symbol:    "399006.SZ",
		Market:    "a_share",
		LookbackN: 30,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if primaryCalls != 1 {
		t.Errorf("primary calls = %d, want 1", primaryCalls)
	}
	if fallbackCalls != 0 {
		t.Errorf("fallback calls = %d, want 0 (4xx is non-transient)", fallbackCalls)
	}
}
