package sectorflow

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fundai/server/internal/ohlc"
)

// stubProvider for registry/cache tests.
type stubProvider struct {
	name     string
	markets  []string
	snapshot *Snapshot
	err      error
	calls    atomic.Int32
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) Supports(market string) bool {
	for _, m := range s.markets {
		if strings.EqualFold(m, market) {
			return true
		}
	}
	return false
}
func (s *stubProvider) Fetch(_ context.Context, _ FetchRequest) (*Snapshot, error) {
	s.calls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	if s.snapshot == nil {
		return nil, nil
	}
	clone := *s.snapshot
	clone.Sectors = append([]Sector(nil), s.snapshot.Sectors...)
	return &clone, nil
}

// FetchRequest.Normalize lower-cases and trims market.
func TestFetchRequestNormalize(t *testing.T) {
	req := FetchRequest{Market: " US_Equity "}.Normalize()
	if req.Market != "us_equity" {
		t.Errorf("market = %q", req.Market)
	}
	if req.CacheKey() != "us_equity" {
		t.Errorf("key = %q", req.CacheKey())
	}
}

// Registry sorts by Return1d desc and returns the first matching
// provider's snapshot.
func TestRegistrySortsAndReturnsFirstSnapshot(t *testing.T) {
	src := &stubProvider{
		name:    "stub",
		markets: []string{"us_equity"},
		snapshot: &Snapshot{
			Market: "us_equity",
			Sectors: []Sector{
				{Name: "Energy", Return1d: -0.023},
				{Name: "Technology", Return1d: 0.018},
				{Name: "Health Care", Return1d: 0.005},
			},
		},
	}
	reg := NewRegistry()
	reg.Register(src)
	got, err := reg.Fetch(context.Background(), FetchRequest{Market: "us_equity"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Sectors[0].Name != "Technology" {
		t.Errorf("expected best at position 0, got %s", got.Sectors[0].Name)
	}
	if got.Sectors[len(got.Sectors)-1].Name != "Energy" {
		t.Errorf("expected worst at end, got %s", got.Sectors[len(got.Sectors)-1].Name)
	}
}

// Registry falls through ErrNoData and returns ErrNoProvider when
// no provider claims the market.
func TestRegistryFallthroughAndNoProvider(t *testing.T) {
	first := &stubProvider{name: "first", markets: []string{"us_equity"}, err: ErrNoData}
	second := &stubProvider{
		name:     "second",
		markets:  []string{"us_equity"},
		snapshot: &Snapshot{Market: "us_equity", Sectors: []Sector{{Name: "Energy", Return1d: 0.01}}},
	}
	reg := NewRegistry()
	reg.Register(first)
	reg.Register(second)
	got, err := reg.Fetch(context.Background(), FetchRequest{Market: "us_equity"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got == nil || len(got.Sectors) != 1 {
		t.Fatalf("got %+v", got)
	}

	_, err = reg.Fetch(context.Background(), FetchRequest{Market: "a_share"})
	if !errors.Is(err, ErrNoProvider) {
		t.Errorf("expected ErrNoProvider, got %v", err)
	}
}

// Cache memoizes within TTL; mutating result must not corrupt
// stored entry (clone semantics).
func TestCacheTTLAndClone(t *testing.T) {
	src := &stubProvider{
		name:     "src",
		markets:  []string{"us_equity"},
		snapshot: &Snapshot{Market: "us_equity", Sectors: []Sector{{Name: "Technology", Return1d: 0.018}}},
	}
	cache := NewCache(src, 50*time.Millisecond)
	req := FetchRequest{Market: "us_equity"}
	a, err := cache.Fetch(context.Background(), req)
	if err != nil {
		t.Fatalf("Fetch a: %v", err)
	}
	a.Sectors[0].Name = "mutated"
	b, err := cache.Fetch(context.Background(), req)
	if err != nil {
		t.Fatalf("Fetch b: %v", err)
	}
	if b.Sectors[0].Name != "Technology" {
		t.Errorf("cache leaked mutation: %s", b.Sectors[0].Name)
	}
	if src.calls.Load() != 1 {
		t.Errorf("expected one upstream call, got %d", src.calls.Load())
	}
	time.Sleep(60 * time.Millisecond)
	if _, err := cache.Fetch(context.Background(), req); err != nil {
		t.Fatalf("post-expiry: %v", err)
	}
	if src.calls.Load() != 2 {
		t.Errorf("expected refresh, calls=%d", src.calls.Load())
	}
}

// stubOHLC implements ohlc.Fetcher for the Yahoo sector provider.
type stubOHLC struct {
	bars map[string][]ohlc.Bar
}

func (s *stubOHLC) Fetch(_ context.Context, req ohlc.FetchRequest) ([]ohlc.Bar, error) {
	bars, ok := s.bars[strings.ToUpper(req.Symbol)]
	if !ok {
		return nil, ohlc.ErrNoData
	}
	return bars, nil
}

// YahooSectorProvider computes 1d/5d/20d returns from ohlc bars.
func TestYahooSectorProviderComputesReturns(t *testing.T) {
	// 22 bars so we have indices for 1d, 5d, and 20d returns.
	makeBars := func(closes []float64) []ohlc.Bar {
		now := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
		bars := make([]ohlc.Bar, len(closes))
		for i, c := range closes {
			bars[i] = ohlc.Bar{Time: now.AddDate(0, 0, -(len(closes) - 1 - i)), Close: c}
		}
		return bars
	}
	src := &stubOHLC{bars: map[string][]ohlc.Bar{
		"XLK": makeBars([]float64{
			95, 96, 97, 96, 95, 94, 96, 97, 98, 99,
			100, 101, 102, 103, 102, 101, 100, 99, 100, 100,
			99, 100,
		}),
		"XLE": makeBars([]float64{
			110, 109, 108, 107, 106, 105, 104, 103, 102, 101,
			100, 99, 98, 97, 96, 95, 94, 93, 92, 91,
			90, 89,
		}),
	}}
	p := &YahooSectorProvider{OHLC: src}
	snap, err := p.Fetch(context.Background(), FetchRequest{Market: "us_equity"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap == nil || len(snap.Sectors) < 2 {
		t.Fatalf("snapshot empty: %+v", snap)
	}
	var xlk, xle Sector
	for _, s := range snap.Sectors {
		switch s.Symbol {
		case "XLK":
			xlk = s
		case "XLE":
			xle = s
		}
	}
	// XLK Return1d = 100/99 - 1 = 0.0101...
	if xlk.Return1d == 0 {
		t.Errorf("XLK Return1d empty: %+v", xlk)
	}
	// XLE Return1d = 89/90 - 1 = -0.0111...
	if xle.Return1d >= 0 {
		t.Errorf("XLE Return1d should be negative: %+v", xle)
	}
	// XLE Return20d should be deeply negative (~89/108 - 1).
	if xle.Return20d > -0.1 {
		t.Errorf("XLE Return20d should be deeply negative: %+v", xle)
	}
}

// YahooSectorProvider returns ErrNoData when OHLC is empty.
func TestYahooSectorProviderErrorsCleanlyWithoutOHLC(t *testing.T) {
	p := &YahooSectorProvider{}
	if p.Supports("us_equity") {
		t.Errorf("Supports should be false when OHLC fetcher is nil")
	}
	_, err := p.Fetch(context.Background(), FetchRequest{Market: "us_equity"})
	if !errors.Is(err, ErrNoData) {
		t.Errorf("expected ErrNoData, got %v", err)
	}
}

// AkshareSectorProvider parses wrapped {data: [...]} response.
func TestAkshareSectorProviderParsesWrappedShape(t *testing.T) {
	const body = `{"data":[
		{"name":"半导体","change_pct":2.13,"net_inflow":1.5e9},
		{"name":"白酒","change_pct":-1.10,"net_inflow":-3.2e8},
		{"name":"银行","change_pct":0.40,"net_inflow":0.0}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	p := &AkshareSectorProvider{BaseURL: srv.URL}
	snap, err := p.Fetch(context.Background(), FetchRequest{Market: "a_share"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(snap.Sectors) != 3 {
		t.Fatalf("expected 3 sectors, got %d", len(snap.Sectors))
	}
	for _, s := range snap.Sectors {
		if s.Name == "半导体" && s.Return1d <= 0 {
			t.Errorf("半导体 Return1d should be positive: %+v", s)
		}
		if s.Name == "白酒" && s.Return1d >= 0 {
			t.Errorf("白酒 Return1d should be negative: %+v", s)
		}
	}
}

// AkshareSectorProvider parses bare-array response.
func TestAkshareSectorProviderParsesBareArray(t *testing.T) {
	const body = `[
		{"sector":"Energy","change_pct":-2.3},
		{"sector":"Tech","change_pct":1.8}
	]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	p := &AkshareSectorProvider{BaseURL: srv.URL}
	snap, err := p.Fetch(context.Background(), FetchRequest{Market: "a_share"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(snap.Sectors) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(snap.Sectors))
	}
}

// AkshareSectorProvider.Supports requires BaseURL.
func TestAkshareSectorProviderRequiresBaseURL(t *testing.T) {
	p := &AkshareSectorProvider{}
	if p.Supports("a_share") {
		t.Errorf("Supports should be false without BaseURL")
	}
}

// FormatForPrompt produces top+bottom lines.
func TestFormatForPromptTopBottom(t *testing.T) {
	s := &Snapshot{
		Market: "us_equity",
		Sectors: []Sector{
			{Name: "Technology", Return1d: 0.018},
			{Name: "Industrials", Return1d: 0.009},
			{Name: "Real Estate", Return1d: 0.012},
			{Name: "Materials", Return1d: -0.011},
			{Name: "Energy", Return1d: -0.023},
			{Name: "Utilities", Return1d: -0.015},
		},
	}
	sortSectorsByReturn(s.Sectors)
	got := s.FormatForPrompt(2, 2)
	if !strings.Contains(got, "Top 2") || !strings.Contains(got, "Bottom 2") {
		t.Errorf("missing top/bottom: %s", got)
	}
	if !strings.Contains(got, "Technology +1.80%") {
		t.Errorf("missing best sector: %s", got)
	}
	if !strings.Contains(got, "Energy -2.30%") {
		t.Errorf("missing worst sector: %s", got)
	}
}

// FormatForPrompt returns empty for nil / empty snapshot.
func TestFormatForPromptEmpty(t *testing.T) {
	if (&Snapshot{}).FormatForPrompt(3, 3) != "" {
		t.Errorf("empty snapshot should format to empty")
	}
	if (*Snapshot)(nil).FormatForPrompt(3, 3) != "" {
		t.Errorf("nil snapshot should format to empty")
	}
}

// FormatForPrompt with net inflow data adds inflow line.
func TestFormatForPromptWithNetInflow(t *testing.T) {
	s := &Snapshot{
		Market: "a_share",
		Sectors: []Sector{
			{Name: "半导体", Return1d: 0.02, NetInflow: 1.5e9, Currency: "CNY"},
			{Name: "白酒", Return1d: -0.011, NetInflow: -3.2e8, Currency: "CNY"},
		},
	}
	sortSectorsByReturn(s.Sectors)
	got := s.FormatForPrompt(1, 1)
	if !strings.Contains(got, "Strongest net inflow") {
		t.Errorf("missing inflow line: %s", got)
	}
	if !strings.Contains(got, "Weakest net inflow") {
		t.Errorf("missing outflow line: %s", got)
	}
}

// pct() converts percent-points to fractions only when |v|>1.5,
// leaving small fractions intact.
func TestPctNormalization(t *testing.T) {
	if got := pct(2.13); got < 0.0212 || got > 0.0214 {
		t.Errorf("2.13%% should become 0.0213, got %v", got)
	}
	if got := pct(0.013); got != 0.013 {
		t.Errorf("0.013 (already fraction) should stay 0.013, got %v", got)
	}
	if got := pct(0); got != 0 {
		t.Errorf("0 should stay 0, got %v", got)
	}
	if got := pct(-2.3); got > -0.022 || got < -0.024 {
		t.Errorf("-2.3%% should become -0.023, got %v", got)
	}
}
