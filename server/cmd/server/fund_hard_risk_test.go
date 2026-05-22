package main

import (
	"testing"
	"time"

	"github.com/fundai/server/internal/api"
)

func hardRiskFloatPtr(v float64) *float64 { return &v }

func TestNormalizeFundHardRiskKeepsValidMaxQuoteAge(t *testing.T) {
	cfg := &api.FundHardRiskConfig{MaxQuoteAgeSeconds: intPtr(45)}

	got := normalizeFundHardRisk(cfg)
	if got == nil {
		t.Fatalf("expected normalized config, got nil")
	}
	if got.MaxQuoteAgeSeconds == nil || *got.MaxQuoteAgeSeconds != 45 {
		t.Fatalf("expected MaxQuoteAgeSeconds=45, got %v", got.MaxQuoteAgeSeconds)
	}
}

func TestNormalizeFundHardRiskDropsOutOfRangeMaxQuoteAge(t *testing.T) {
	cases := []struct {
		name string
		in   int
	}{
		{"zero", 0},
		{"negative", -10},
		{"too large", 86401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &api.FundHardRiskConfig{MaxQuoteAgeSeconds: intPtr(tc.in)}
			got := normalizeFundHardRisk(cfg)
			if got != nil {
				t.Fatalf("expected nil normalized config for input %d, got %+v", tc.in, got)
			}
		})
	}
}

func TestNormalizeFundHardRiskAllowsBoundaries(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"lower bound", 1, 1},
		{"upper bound", 86400, 86400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &api.FundHardRiskConfig{MaxQuoteAgeSeconds: intPtr(tc.in)}
			got := normalizeFundHardRisk(cfg)
			if got == nil || got.MaxQuoteAgeSeconds == nil || *got.MaxQuoteAgeSeconds != tc.want {
				t.Fatalf("expected %d, got %+v", tc.want, got)
			}
		})
	}
}

func TestMergeFundHardRiskClearsViaZeroSentinel(t *testing.T) {
	existing := &api.FundHardRiskConfig{MaxQuoteAgeSeconds: intPtr(120)}
	patch := &api.FundHardRiskConfig{MaxQuoteAgeSeconds: intPtr(0)}

	merged := mergeFundHardRisk(existing, patch)
	if merged != nil && merged.MaxQuoteAgeSeconds != nil {
		t.Fatalf("expected zero sentinel to clear override, got %+v", merged.MaxQuoteAgeSeconds)
	}
}

func TestMergeFundHardRiskKeepsExistingWhenPatchNil(t *testing.T) {
	existing := &api.FundHardRiskConfig{MaxQuoteAgeSeconds: intPtr(120)}
	patch := &api.FundHardRiskConfig{}

	merged := mergeFundHardRisk(existing, patch)
	if merged == nil || merged.MaxQuoteAgeSeconds == nil || *merged.MaxQuoteAgeSeconds != 120 {
		t.Fatalf("expected existing maxQuoteAge preserved, got %+v", merged)
	}
}

func TestMergeFundHardRiskOverwritesWithValidPatch(t *testing.T) {
	existing := &api.FundHardRiskConfig{MaxQuoteAgeSeconds: intPtr(120), DailyLossLimit: hardRiskFloatPtr(0.05)}
	patch := &api.FundHardRiskConfig{MaxQuoteAgeSeconds: intPtr(300)}

	merged := mergeFundHardRisk(existing, patch)
	if merged == nil || merged.MaxQuoteAgeSeconds == nil || *merged.MaxQuoteAgeSeconds != 300 {
		t.Fatalf("expected MaxQuoteAgeSeconds=300, got %+v", merged)
	}
	if merged.DailyLossLimit == nil || *merged.DailyLossLimit != 0.05 {
		t.Fatalf("expected DailyLossLimit preserved, got %+v", merged.DailyLossLimit)
	}
}

func TestRiskHardConfigFromAPIPassesMaxQuoteAge(t *testing.T) {
	cfg := &api.FundHardRiskConfig{MaxQuoteAgeSeconds: intPtr(60)}
	got := riskHardConfigFromAPI(cfg)
	if got.MaxQuoteAge != 60*time.Second {
		t.Fatalf("expected MaxQuoteAge=60s, got %s", got.MaxQuoteAge)
	}
}

func TestRiskHardConfigFromAPIFallsBackToPlatformDefault(t *testing.T) {
	cfg := &api.FundHardRiskConfig{MaxQuoteAgeSeconds: intPtr(0)}
	got := riskHardConfigFromAPI(cfg)
	// 0 is normalized away; default kicks in -> 15 minutes per risk.DefaultHardRiskConfig.
	if got.MaxQuoteAge != 15*time.Minute {
		t.Fatalf("expected platform default MaxQuoteAge=15m, got %s", got.MaxQuoteAge)
	}
}
