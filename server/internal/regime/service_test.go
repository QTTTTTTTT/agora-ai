package regime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fundai/server/internal/ohlc"
)

// stubFetcher records call counts per CacheKey so tests can assert
// "did the service hit the upstream the expected number of times?".
type stubFetcher struct {
	mu       sync.Mutex
	bars     []ohlc.Bar
	err      error
	calls    atomic.Int64
	perKey   map[string]int
	delay    time.Duration
	perReq   func(ohlc.FetchRequest) ([]ohlc.Bar, error)
}

func (s *stubFetcher) Fetch(_ context.Context, req ohlc.FetchRequest) ([]ohlc.Bar, error) {
	s.calls.Add(1)
	s.mu.Lock()
	if s.perKey == nil {
		s.perKey = map[string]int{}
	}
	s.perKey[req.Symbol]++
	s.mu.Unlock()
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	if s.perReq != nil {
		return s.perReq(req)
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.bars, nil
}

// ---------------------------------------------------------------------------
// Service: cache behaviour
// ---------------------------------------------------------------------------

func TestServiceCachesClassifiedRegime(t *testing.T) {
	fetcher := &stubFetcher{bars: uptrend(260, 100)}
	svc := NewService(fetcher, WithCacheTTL(1*time.Hour))

	for i := 0; i < 5; i++ {
		r, err := svc.Classify(context.Background(), Instrument{Symbol: "NVDA", Market: "us_equity"})
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if r != TrendUp {
			t.Fatalf("iteration %d: regime got %q, want trend_up", i, r)
		}
	}
	if got := fetcher.calls.Load(); got != 1 {
		t.Fatalf("fetcher should be hit once via cache, got %d", got)
	}
}

func TestServiceCacheTTLExpires(t *testing.T) {
	fetcher := &stubFetcher{bars: uptrend(260, 100)}
	now := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	svc := NewService(fetcher,
		WithCacheTTL(30*time.Minute),
		WithClock(func() time.Time { return now }),
	)
	if _, err := svc.Classify(context.Background(), Instrument{Symbol: "NVDA", Market: "us_equity"}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Advance the clock past TTL.
	now = now.Add(31 * time.Minute)
	if _, err := svc.Classify(context.Background(), Instrument{Symbol: "NVDA", Market: "us_equity"}); err != nil {
		t.Fatalf("post-expiry call: %v", err)
	}
	if got := fetcher.calls.Load(); got != 2 {
		t.Fatalf("expected 2 fetcher calls after TTL expiry, got %d", got)
	}
}

func TestServiceCacheKeyNormalisesSymbolAndMarket(t *testing.T) {
	fetcher := &stubFetcher{bars: uptrend(260, 100)}
	svc := NewService(fetcher)

	for _, inst := range []Instrument{
		{Symbol: "NVDA", Market: "us_equity"},
		{Symbol: " nvda ", Market: " US_EQUITY "},
		{Symbol: "Nvda", Market: "Us_Equity"},
	} {
		if _, err := svc.Classify(context.Background(), inst); err != nil {
			t.Fatalf("inst %+v: %v", inst, err)
		}
	}
	if got := fetcher.calls.Load(); got != 1 {
		t.Fatalf("normalised cache key should collapse to 1 fetch, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Service: error paths
// ---------------------------------------------------------------------------

func TestServiceReturnsUnknownAndNoErrorWhenFetcherHasNoData(t *testing.T) {
	fetcher := &stubFetcher{err: ohlc.ErrNoData}
	svc := NewService(fetcher)
	r, err := svc.Classify(context.Background(), Instrument{Symbol: "X", Market: "us_equity"})
	if err != nil {
		t.Fatalf("ErrNoData should not surface, got %v", err)
	}
	if r != Unknown {
		t.Fatalf("expected Unknown, got %q", r)
	}
}

func TestServicePropagatesUnexpectedFetcherErrors(t *testing.T) {
	boom := errors.New("upstream 500")
	fetcher := &stubFetcher{err: boom}
	svc := NewService(fetcher)
	r, err := svc.Classify(context.Background(), Instrument{Symbol: "X", Market: "us_equity"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrFetcherUnavailable) {
		t.Fatalf("err should wrap ErrFetcherUnavailable, got %v", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err should wrap original cause, got %v", err)
	}
	if r != Unknown {
		t.Fatalf("expected Unknown on error, got %q", r)
	}
}

func TestServiceNilFetcherReturnsUnknown(t *testing.T) {
	svc := NewService(nil)
	r, err := svc.Classify(context.Background(), Instrument{Symbol: "X", Market: "us_equity"})
	if err != nil || r != Unknown {
		t.Fatalf("nil fetcher should yield (Unknown, nil), got (%q, %v)", r, err)
	}
}

func TestServiceEmptySymbolReturnsUnknown(t *testing.T) {
	fetcher := &stubFetcher{bars: uptrend(260, 100)}
	svc := NewService(fetcher)
	r, err := svc.Classify(context.Background(), Instrument{Symbol: "  ", Market: "us_equity"})
	if err != nil || r != Unknown {
		t.Fatalf("empty symbol should yield (Unknown, nil), got (%q, %v)", r, err)
	}
	if fetcher.calls.Load() != 0 {
		t.Fatalf("empty symbol should short-circuit before fetcher, got %d calls", fetcher.calls.Load())
	}
}

// ---------------------------------------------------------------------------
// Service: ClassifyBatch
// ---------------------------------------------------------------------------

func TestClassifyBatchDedupesAndOmitsUnknown(t *testing.T) {
	// First call returns trend bars, second returns insufficient bars.
	tooShort := uptrend(50, 100)
	fetcher := &stubFetcher{
		perReq: func(req ohlc.FetchRequest) ([]ohlc.Bar, error) {
			switch req.Symbol {
			case "NVDA":
				return uptrend(260, 100), nil
			case "TSLA":
				return downtrend(260, 100), nil
			case "RIVN":
				return tooShort, nil // unknown
			default:
				return nil, ohlc.ErrNoData
			}
		},
	}
	svc := NewService(fetcher)

	res, err := svc.ClassifyBatch(context.Background(), []Instrument{
		{Symbol: "NVDA", Market: "us_equity"},
		{Symbol: "nvda", Market: "us_equity"}, // duplicate
		{Symbol: "TSLA", Market: "us_equity"},
		{Symbol: "RIVN", Market: "us_equity"},
		{Symbol: "GME", Market: "us_equity"}, // no data
	})
	if err != nil {
		t.Fatalf("batch should not surface ErrNoData, got %v", err)
	}
	if got := res["NVDA|us_equity"]; got != TrendUp {
		t.Fatalf("NVDA: got %q, want trend_up", got)
	}
	if got := res["TSLA|us_equity"]; got != TrendDown {
		t.Fatalf("TSLA: got %q, want trend_down", got)
	}
	if _, ok := res["RIVN|us_equity"]; ok {
		t.Fatal("RIVN (unknown) should be omitted from map")
	}
	if _, ok := res["GME|us_equity"]; ok {
		t.Fatal("GME (no data) should be omitted from map")
	}
	// Dedupe + cache: 4 unique symbols → 4 fetcher calls (the
	// duplicate nvda hits the cache).
	if got := fetcher.calls.Load(); got != 4 {
		t.Fatalf("expected 4 fetcher calls, got %d", got)
	}
}

func TestClassifyBatchSurfacesFirstFatalError(t *testing.T) {
	boom := errors.New("provider down")
	fetcher := &stubFetcher{
		perReq: func(req ohlc.FetchRequest) ([]ohlc.Bar, error) {
			if req.Symbol == "BAD" {
				return nil, boom
			}
			return uptrend(260, 100), nil
		},
	}
	svc := NewService(fetcher)
	res, err := svc.ClassifyBatch(context.Background(), []Instrument{
		{Symbol: "NVDA", Market: "us_equity"},
		{Symbol: "BAD", Market: "us_equity"},
		{Symbol: "TSLA", Market: "us_equity"},
	})
	if err == nil {
		t.Fatal("expected the BAD error to surface, got nil")
	}
	if !errors.Is(err, ErrFetcherUnavailable) {
		t.Fatalf("err should wrap ErrFetcherUnavailable, got %v", err)
	}
	if _, ok := res["NVDA|us_equity"]; !ok {
		t.Fatal("NVDA should still be classified despite BAD failing")
	}
}

// ---------------------------------------------------------------------------
// Service: in-flight dedupe under contention
// ---------------------------------------------------------------------------

func TestServiceDedupesConcurrentFetches(t *testing.T) {
	bars := uptrend(260, 100)
	fetcher := &stubFetcher{bars: bars, delay: 20 * time.Millisecond}
	svc := NewService(fetcher)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Classify(context.Background(), Instrument{Symbol: "NVDA", Market: "us_equity"}); err != nil {
				t.Errorf("classify: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := fetcher.calls.Load(); got != 1 {
		t.Fatalf("in-flight dedupe should keep fetcher calls at 1, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Service: Lookup
// ---------------------------------------------------------------------------

func TestLookupReturnsCachedRegime(t *testing.T) {
	fetcher := &stubFetcher{bars: uptrend(260, 100)}
	svc := NewService(fetcher)
	if _, err := svc.Classify(context.Background(), Instrument{Symbol: "NVDA", Market: "us_equity"}); err != nil {
		t.Fatalf("classify: %v", err)
	}
	r, ok := svc.Lookup(Instrument{Symbol: "NVDA", Market: "us_equity"})
	if !ok || r != TrendUp {
		t.Fatalf("Lookup: got (%q, %v), want (trend_up, true)", r, ok)
	}
}

func TestLookupReturnsFalseWhenAbsent(t *testing.T) {
	svc := NewService(&stubFetcher{})
	r, ok := svc.Lookup(Instrument{Symbol: "MISSING", Market: "us_equity"})
	if ok || r != Unknown {
		t.Fatalf("Lookup miss: got (%q, %v), want (Unknown, false)", r, ok)
	}
}

func TestInvalidateAllForcesRefetch(t *testing.T) {
	fetcher := &stubFetcher{bars: uptrend(260, 100)}
	svc := NewService(fetcher)
	if _, err := svc.Classify(context.Background(), Instrument{Symbol: "NVDA", Market: "us_equity"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	svc.InvalidateAll()
	if _, err := svc.Classify(context.Background(), Instrument{Symbol: "NVDA", Market: "us_equity"}); err != nil {
		t.Fatalf("post-invalidate: %v", err)
	}
	if got := fetcher.calls.Load(); got != 2 {
		t.Fatalf("InvalidateAll should force second fetch, got %d", got)
	}
}
