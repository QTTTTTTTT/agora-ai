package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/fundai/server/internal/marketdata"
	"github.com/fundai/server/internal/sentiment"
	"github.com/fundai/server/internal/social"
)

type stubSocialProvider struct {
	platform social.Platform
	items    map[string][]sentiment.Item
	err      error
}

func (s *stubSocialProvider) Platform() social.Platform { return s.platform }
func (s *stubSocialProvider) FetchPosts(_ context.Context, req social.Request) ([]sentiment.Item, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.items[req.Symbol], nil
}

func socialSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCollectSocialItems_FetchesPerSymbolAndDedups(t *testing.T) {
	provider := &stubSocialProvider{
		platform: social.PlatformXueqiu,
		items: map[string][]sentiment.Item{
			"AAPL": {
				{ID: "x:1", Title: "AAPL bull", Source: "xueqiu", PublishedAt: time.Now()},
				{ID: "x:2", Title: "AAPL bear", Source: "xueqiu", PublishedAt: time.Now()},
			},
			"TSLA": {
				{ID: "x:1", Title: "duplicate id should be skipped", Source: "xueqiu", PublishedAt: time.Now()},
				{ID: "x:3", Title: "TSLA bull", Source: "xueqiu", PublishedAt: time.Now()},
			},
		},
	}
	registry := social.NewRegistry([]social.Provider{provider}, social.RegistryOptions{
		PerProviderTimeout: time.Second,
		PerProviderLimit:   10,
		MaxAge:             time.Hour,
	}, socialSilentLogger())
	pool := runtimeResearcherPool{socialRegistry: registry}

	got := pool.collectSocialItems(context.Background(), []marketdata.InstrumentRef{
		{Symbol: "AAPL", Market: "US"},
		{Symbol: "TSLA", Market: "US"},
	}, 5)
	if len(got) != 3 {
		t.Fatalf("expected 3 unique items (x:1 dedup'd), got %d (%+v)", len(got), got)
	}
}

func TestCollectSocialItems_DropsNonTickerSymbols(t *testing.T) {
	provider := &stubSocialProvider{
		platform: social.PlatformXueqiu,
		items: map[string][]sentiment.Item{
			"AAPL": {{ID: "x:1", Title: "ok", Source: "xueqiu", PublishedAt: time.Now()}},
		},
	}
	registry := social.NewRegistry([]social.Provider{provider}, social.RegistryOptions{}, socialSilentLogger())
	pool := runtimeResearcherPool{socialRegistry: registry}

	got := pool.collectSocialItems(context.Background(), []marketdata.InstrumentRef{
		{Symbol: "macro_news"},
		{Symbol: "AAPL", Market: "US"},
	}, 5)
	if len(got) != 1 || got[0].ID != "x:1" {
		t.Fatalf("expected only AAPL item, got %+v", got)
	}
}

func TestCollectSocialItems_NilRegistry_ReturnsNil(t *testing.T) {
	pool := runtimeResearcherPool{}
	if got := pool.collectSocialItems(context.Background(), []marketdata.InstrumentRef{{Symbol: "AAPL"}}, 5); got != nil {
		t.Fatalf("expected nil with no registry, got %+v", got)
	}
}

func TestCollectSocialItems_EmptyRegistry_ReturnsNil(t *testing.T) {
	registry := social.NewRegistry(nil, social.RegistryOptions{}, socialSilentLogger())
	pool := runtimeResearcherPool{socialRegistry: registry}
	if got := pool.collectSocialItems(context.Background(), []marketdata.InstrumentRef{{Symbol: "AAPL"}}, 5); got != nil {
		t.Fatalf("expected nil with empty registry, got %+v", got)
	}
}

func TestBuildSocialRegistryFromEnv_KillSwitchOff(t *testing.T) {
	t.Setenv("SOCIAL_PROVIDERS_ENABLED", "")
	if got := buildSocialRegistryFromEnv(socialSilentLogger()); got != nil {
		t.Fatalf("expected nil when kill-switch off, got %v", got)
	}
}

func TestBuildSocialRegistryFromEnv_GlobalOnButNoProviders(t *testing.T) {
	for _, k := range []string{"SOCIAL_PROVIDER_XUEQIU", "SOCIAL_PROVIDER_STOCKTWITS", "SOCIAL_PROVIDER_REDDIT"} {
		t.Setenv(k, "")
	}
	t.Setenv("SOCIAL_PROVIDERS_ENABLED", "1")
	if got := buildSocialRegistryFromEnv(socialSilentLogger()); got != nil {
		t.Fatalf("expected nil with no providers, got %v", got)
	}
}

func TestBuildSocialRegistryFromEnv_EnableAll(t *testing.T) {
	t.Setenv("SOCIAL_PROVIDERS_ENABLED", "1")
	t.Setenv("SOCIAL_PROVIDER_XUEQIU", "1")
	t.Setenv("SOCIAL_PROVIDER_STOCKTWITS", "1")
	t.Setenv("SOCIAL_PROVIDER_REDDIT", "1")
	reg := buildSocialRegistryFromEnv(socialSilentLogger())
	if reg == nil {
		t.Fatalf("expected registry, got nil")
	}
	if !reg.HasProviders() {
		t.Fatalf("expected HasProviders=true")
	}
}

func TestEnvFlagEnabled(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"FALSE", false},
		{"no", false},
		{"off", false},
		{"disabled", false},
		{"1", true},
		{"true", true},
		{"yes", true},
		{"on", true},
		{"enabled", true},
		{"anything-else", true},
	}
	for _, c := range cases {
		_ = os.Setenv("__FUNDAI_TEST_FLAG__", c.v)
		got := envFlagEnabled("__FUNDAI_TEST_FLAG__")
		if got != c.want {
			t.Errorf("envFlagEnabled(%q)=%v want %v", c.v, got, c.want)
		}
	}
	_ = os.Unsetenv("__FUNDAI_TEST_FLAG__")
}
