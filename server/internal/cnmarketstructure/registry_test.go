package cnmarketstructure

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRegistryFallthroughOnNoData(t *testing.T) {
	reg := NewRegistry()
	reg.Register(stubProvider{
		name:           "primary",
		fetchIntradayFn: func(ctx context.Context, symbol string) (*IntradaySnapshot, error) { return nil, ErrNoData },
	})
	reg.Register(stubProvider{
		name: "secondary",
		fetchIntradayFn: func(ctx context.Context, symbol string) (*IntradaySnapshot, error) {
			return &IntradaySnapshot{Symbol: symbol, LimitUpToday: true}, nil
		},
	})
	snap, err := reg.FetchIntraday(context.Background(), "600519")
	if err != nil {
		t.Fatalf("expected fallthrough success, got %v", err)
	}
	if !snap.LimitUpToday {
		t.Fatalf("expected secondary result, got %+v", snap)
	}
	if snap.Source != "secondary" {
		t.Fatalf("expected source=secondary, got %q", snap.Source)
	}
}

func TestRegistryCircuitOpensAfterFailures(t *testing.T) {
	reg := NewRegistry()
	reg.Register(stubProvider{
		name:           "flaky",
		fetchIntradayFn: func(ctx context.Context, symbol string) (*IntradaySnapshot, error) { return nil, errors.New("boom") },
	})
	for i := 0; i < 3; i++ {
		_, _ = reg.FetchIntraday(context.Background(), "600519")
	}
	stats := reg.HealthStats()
	if stats["flaky"].ConsecutiveFailures != 3 {
		t.Fatalf("expected 3 consecutive failures, got %d", stats["flaky"].ConsecutiveFailures)
	}
	if stats["flaky"].CircuitOpenUntil.IsZero() {
		t.Fatalf("expected circuit to be open")
	}
}

func TestCacheTTLHit(t *testing.T) {
	calls := 0
	upstream := stubProvider{
		name: "test",
		fetchIntradayFn: func(ctx context.Context, symbol string) (*IntradaySnapshot, error) {
			calls++
			return &IntradaySnapshot{Symbol: symbol}, nil
		},
	}
	cache := NewCache(upstream, CacheOptions{IntradayTTL: time.Minute})
	_, _ = cache.FetchIntraday(context.Background(), "600519")
	_, _ = cache.FetchIntraday(context.Background(), "600519")
	if calls != 1 {
		t.Fatalf("expected single upstream call, got %d", calls)
	}
	_, _ = cache.FetchIntraday(context.Background(), "000333")
	if calls != 2 {
		t.Fatalf("expected new symbol to miss cache, got %d", calls)
	}
}

func TestAkshareProviderHappy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/stock_spot":
			fmt.Fprint(w, `{"symbol":"600519","change_pct":2.5,"turnover_rate":4.5,"volume_ratio":2.1,"circ_market_cap":280000000000,"name":"贵州茅台","所属行业":"白酒"}`)
		case r.URL.Path == "/api/limit_up_pool":
			fmt.Fprint(w, `{"symbol":"600519","seal_amount_yi":3.2,"reopen_count":0,"consecutive_count":1,"limit_up_time":"10:30:00"}`)
		case r.URL.Path == "/api/northbound_net_flow":
			fmt.Fprint(w, `{"net_flow":1500000000}`)
		case r.URL.Path == "/api/market_activity":
			fmt.Fprint(w, `{"limit_up":40,"limit_down":3,"fried_board":12,"shanghai_change_pct":1.2}`)
		case r.URL.Path == "/api/sector_strength":
			fmt.Fprint(w, `[{"name":"白酒","change_pct":3.2,"limit_up_count":3},{"name":"医药","change_pct":2.1}]`)
		case r.URL.Path == "/api/lhb_detail":
			fmt.Fprint(w, `[]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p := &AkshareProvider{BaseURL: srv.URL}

	snap, err := p.FetchIntraday(context.Background(), "600519")
	if err != nil {
		t.Fatalf("FetchIntraday: %v", err)
	}
	if snap.DailyGainPct != 2.5 {
		t.Fatalf("expected DailyGainPct=2.5, got %.2f", snap.DailyGainPct)
	}
	if !snap.LimitUpToday {
		t.Fatalf("expected limit-up flag true")
	}
	if snap.SealAmountYi != 3.2 {
		t.Fatalf("expected seal_amount_yi=3.2, got %.2f", snap.SealAmountYi)
	}
	if snap.ConsecutiveLimitUps != 1 {
		t.Fatalf("expected consecutive=1, got %d", snap.ConsecutiveLimitUps)
	}
	if snap.NorthboundNetInflow != 1.5e9 {
		t.Fatalf("expected northbound flow=1.5e9, got %.2f", snap.NorthboundNetInflow)
	}
	if snap.IsST {
		t.Fatalf("expected non-ST for 贵州茅台")
	}

	regime, err := p.FetchMarketRegime(context.Background())
	if err != nil {
		t.Fatalf("FetchMarketRegime: %v", err)
	}
	if regime.LimitUpCount != 40 {
		t.Fatalf("expected 40 limit ups, got %d", regime.LimitUpCount)
	}
	if regime.FriedBoardCount != 12 {
		t.Fatalf("expected 12 fried boards, got %d", regime.FriedBoardCount)
	}
	if regime.FriedBoardRatePct < 23 || regime.FriedBoardRatePct > 24 {
		t.Fatalf("expected fried board rate near 23%%, got %.2f", regime.FriedBoardRatePct)
	}

	sectors, err := p.FetchSectorStrength(context.Background(), 5)
	if err != nil {
		t.Fatalf("FetchSectorStrength: %v", err)
	}
	if len(sectors) != 2 {
		t.Fatalf("expected 2 sectors, got %d", len(sectors))
	}
	if sectors[0].SectorName != "白酒" {
		t.Fatalf("expected 白酒 first (highest gain), got %q", sectors[0].SectorName)
	}
}

func TestAkshareProviderThrottling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	p := &AkshareProvider{BaseURL: srv.URL}
	_, err := p.FetchIntraday(context.Background(), "600519")
	if err == nil {
		t.Fatalf("expected error on 429")
	}
	if !errors.Is(err, ErrUpstreamThrottled) {
		t.Fatalf("expected ErrUpstreamThrottled, got %v", err)
	}
}

func TestAkshareProviderNotConfigured(t *testing.T) {
	p := &AkshareProvider{BaseURL: ""}
	_, err := p.FetchIntraday(context.Background(), "600519")
	if err == nil || !errors.Is(err, ErrNoData) {
		t.Fatalf("expected ErrNoData on unconfigured, got %v", err)
	}
}

// --- stubProvider --------------------------------------------------

type stubProvider struct {
	name              string
	fetchIntradayFn   func(context.Context, string) (*IntradaySnapshot, error)
	fetchDragonFn     func(context.Context, string, int) ([]DragonTigerEntry, error)
	fetchRegimeFn     func(context.Context) (*MarketRegime, error)
	fetchSectorsFn    func(context.Context, int) ([]SectorStrength, error)
}

func (s stubProvider) Name() string { return s.name }
func (s stubProvider) FetchIntraday(ctx context.Context, symbol string) (*IntradaySnapshot, error) {
	if s.fetchIntradayFn != nil {
		return s.fetchIntradayFn(ctx, symbol)
	}
	return nil, ErrNoData
}
func (s stubProvider) FetchDragonTiger(ctx context.Context, symbol string, lookbackDays int) ([]DragonTigerEntry, error) {
	if s.fetchDragonFn != nil {
		return s.fetchDragonFn(ctx, symbol, lookbackDays)
	}
	return nil, ErrNoData
}
func (s stubProvider) FetchMarketRegime(ctx context.Context) (*MarketRegime, error) {
	if s.fetchRegimeFn != nil {
		return s.fetchRegimeFn(ctx)
	}
	return nil, ErrNoData
}
func (s stubProvider) FetchSectorStrength(ctx context.Context, topN int) ([]SectorStrength, error) {
	if s.fetchSectorsFn != nil {
		return s.fetchSectorsFn(ctx, topN)
	}
	return nil, ErrNoData
}
