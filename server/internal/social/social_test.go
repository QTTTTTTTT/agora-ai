package social

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/sentiment"
)

type stubProvider struct {
	platform Platform
	items    []sentiment.Item
	err      error
	delay    time.Duration
}

func (s *stubProvider) Platform() Platform { return s.platform }
func (s *stubProvider) FetchPosts(ctx context.Context, req Request) ([]sentiment.Item, error) {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if s.err != nil {
		return nil, s.err
	}
	return append([]sentiment.Item(nil), s.items...), nil
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRegistry_HappyPath_MergesAndSortsByPublishedDesc(t *testing.T) {
	now := time.Now().UTC()
	a := &stubProvider{
		platform: PlatformXueqiu,
		items: []sentiment.Item{
			{ID: "x:1", Title: "old", PublishedAt: now.Add(-2 * time.Hour), Symbols: []string{"AAPL"}},
			{ID: "x:2", Title: "new", PublishedAt: now.Add(-1 * time.Hour), Symbols: []string{"AAPL"}},
		},
	}
	b := &stubProvider{
		platform: PlatformStockTwits,
		items: []sentiment.Item{
			{ID: "s:1", Title: "newest", PublishedAt: now.Add(-30 * time.Minute), Symbols: []string{"AAPL"}},
		},
	}
	reg := NewRegistry([]Provider{a, b}, RegistryOptions{}, silentLogger())

	items, err := reg.FetchPosts(context.Background(), Request{Symbol: "AAPL", Limit: 10, MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatalf("FetchPosts err=%v", err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	if items[0].ID != "s:1" || items[1].ID != "x:2" || items[2].ID != "x:1" {
		t.Fatalf("order broken: %+v", items)
	}
}

func TestRegistry_DropsItemsOlderThanMaxAge(t *testing.T) {
	now := time.Now().UTC()
	a := &stubProvider{
		platform: PlatformRedditWSB,
		items: []sentiment.Item{
			{ID: "fresh", PublishedAt: now.Add(-1 * time.Hour)},
			{ID: "stale", PublishedAt: now.Add(-30 * time.Hour)},
		},
	}
	reg := NewRegistry([]Provider{a}, RegistryOptions{MaxAge: 6 * time.Hour}, silentLogger())
	got, err := reg.FetchPosts(context.Background(), Request{Symbol: "TSLA"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(got) != 1 || got[0].ID != "fresh" {
		t.Fatalf("expected only 'fresh', got %+v", got)
	}
}

func TestRegistry_OneProviderFails_OthersStillReturned(t *testing.T) {
	now := time.Now().UTC()
	good := &stubProvider{
		platform: PlatformXueqiu,
		items:    []sentiment.Item{{ID: "g:1", PublishedAt: now}},
	}
	bad := &stubProvider{platform: PlatformRedditWSB, err: errors.New("boom")}

	reg := NewRegistry([]Provider{good, bad}, RegistryOptions{}, silentLogger())
	got, err := reg.FetchPosts(context.Background(), Request{Symbol: "AAPL"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(got) != 1 || got[0].ID != "g:1" {
		t.Fatalf("expected good only, got %+v", got)
	}
}

func TestRegistry_AllFail_ReturnsSentinel(t *testing.T) {
	a := &stubProvider{platform: PlatformXueqiu, err: errors.New("x boom")}
	b := &stubProvider{platform: PlatformRedditWSB, err: errors.New("r boom")}
	reg := NewRegistry([]Provider{a, b}, RegistryOptions{}, silentLogger())

	_, err := reg.FetchPosts(context.Background(), Request{Symbol: "AAPL"})
	if !errors.Is(err, ErrAllProvidersFailed) {
		t.Fatalf("expected ErrAllProvidersFailed got %v", err)
	}
}

func TestRegistry_EmptyProviders_NilNil(t *testing.T) {
	reg := NewRegistry(nil, RegistryOptions{}, silentLogger())
	if reg.HasProviders() {
		t.Fatalf("expected HasProviders=false")
	}
	got, err := reg.FetchPosts(context.Background(), Request{Symbol: "AAPL"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != nil {
		t.Fatalf("expected nil items, got %+v", got)
	}
}

func TestRegistry_SymbolRequired(t *testing.T) {
	reg := NewRegistry([]Provider{&stubProvider{platform: PlatformXueqiu}}, RegistryOptions{}, silentLogger())
	_, err := reg.FetchPosts(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "Symbol") {
		t.Fatalf("expected symbol required err, got %v", err)
	}
}

func TestRegistry_PerProviderTimeoutKillsSlowProvider(t *testing.T) {
	now := time.Now().UTC()
	slow := &stubProvider{
		platform: PlatformXueqiu,
		items:    []sentiment.Item{{ID: "x:1", PublishedAt: now}},
		delay:    100 * time.Millisecond,
	}
	fast := &stubProvider{
		platform: PlatformRedditWSB,
		items:    []sentiment.Item{{ID: "r:1", PublishedAt: now}},
	}
	reg := NewRegistry(
		[]Provider{slow, fast},
		RegistryOptions{PerProviderTimeout: 5 * time.Millisecond},
		silentLogger(),
	)
	got, err := reg.FetchPosts(context.Background(), Request{Symbol: "AAPL"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(got) != 1 || got[0].ID != "r:1" {
		t.Fatalf("expected only fast provider's item, got %+v", got)
	}
}

func TestRegistry_DropsNilProvider(t *testing.T) {
	reg := NewRegistry([]Provider{nil, &stubProvider{platform: PlatformXueqiu}}, RegistryOptions{}, silentLogger())
	if got := len(reg.providers); got != 1 {
		t.Fatalf("expected 1 provider after nil-strip, got %d", got)
	}
}

func TestRegistry_NilReceiverFetchPosts(t *testing.T) {
	var reg *Registry
	got, err := reg.FetchPosts(context.Background(), Request{Symbol: "AAPL"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}
