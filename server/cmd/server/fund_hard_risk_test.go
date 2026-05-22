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

// TestMergeFundHardRiskRejectsOutOfRangePatch is the contract that
// out-of-range PATCH values must NEVER clobber a previously-valid cap.
// The pre-fix behaviour was: store the junk pointer in `merged`, let
// normalizeFundHardRisk silently drop it (returns nil), then have the
// platform-default kick in downstream — that turned an apparent "raise
// my limit" into a hidden "fall back to a much looser default".
//
// Per-field ranges (from normalizedRiskFloatPtr call sites in
// mergeFundHardRisk): DailyLossLimit (0, 0.50], MaxSinglePosition
// (0, 1], MaxSectorExposure (0, 1], MaxTotalExposure (0, 1.5],
// MaxOrderPctOfAssets (0, 1]. Anything <=0, >upper, NaN, or Inf is
// out-of-range and the existing value must be preserved.
func TestMergeFundHardRiskRejectsOutOfRangePatch(t *testing.T) {
	cases := []struct {
		name     string
		existing *api.FundHardRiskConfig
		patch    *api.FundHardRiskConfig
		check    func(*testing.T, *api.FundHardRiskConfig)
	}{
		{
			name:     "DailyLossLimit above 0.50 keeps existing",
			existing: &api.FundHardRiskConfig{DailyLossLimit: hardRiskFloatPtr(0.05)},
			patch:    &api.FundHardRiskConfig{DailyLossLimit: hardRiskFloatPtr(0.99)},
			check: func(t *testing.T, merged *api.FundHardRiskConfig) {
				if merged == nil || merged.DailyLossLimit == nil || *merged.DailyLossLimit != 0.05 {
					t.Fatalf("existing 0.05 must survive an out-of-range 0.99 patch, got %+v", merged)
				}
			},
		},
		{
			name:     "DailyLossLimit at 0 keeps existing (lower bound exclusive)",
			existing: &api.FundHardRiskConfig{DailyLossLimit: hardRiskFloatPtr(0.05)},
			patch:    &api.FundHardRiskConfig{DailyLossLimit: hardRiskFloatPtr(0)},
			check: func(t *testing.T, merged *api.FundHardRiskConfig) {
				if merged == nil || merged.DailyLossLimit == nil || *merged.DailyLossLimit != 0.05 {
					t.Fatalf("existing 0.05 must survive a zero patch (zero is out-of-range, NOT a clear-sentinel for floats), got %+v", merged)
				}
			},
		},
		{
			name:     "MaxSinglePosition above 1 keeps existing",
			existing: &api.FundHardRiskConfig{MaxSinglePosition: hardRiskFloatPtr(0.20)},
			patch:    &api.FundHardRiskConfig{MaxSinglePosition: hardRiskFloatPtr(1.50)},
			check: func(t *testing.T, merged *api.FundHardRiskConfig) {
				if merged == nil || merged.MaxSinglePosition == nil || *merged.MaxSinglePosition != 0.20 {
					t.Fatalf("existing 0.20 must survive an out-of-range 1.50 patch, got %+v", merged)
				}
			},
		},
		{
			name:     "MaxOrderPctOfAssets above 1 keeps existing",
			existing: &api.FundHardRiskConfig{MaxOrderPctOfAssets: hardRiskFloatPtr(0.10)},
			patch:    &api.FundHardRiskConfig{MaxOrderPctOfAssets: hardRiskFloatPtr(2.00)},
			check: func(t *testing.T, merged *api.FundHardRiskConfig) {
				if merged == nil || merged.MaxOrderPctOfAssets == nil || *merged.MaxOrderPctOfAssets != 0.10 {
					t.Fatalf("existing 0.10 must survive an out-of-range 2.00 patch, got %+v", merged)
				}
			},
		},
		{
			name:     "MaxTotalExposure above 1.5 keeps existing",
			existing: &api.FundHardRiskConfig{MaxTotalExposure: hardRiskFloatPtr(1.00)},
			patch:    &api.FundHardRiskConfig{MaxTotalExposure: hardRiskFloatPtr(3.00)},
			check: func(t *testing.T, merged *api.FundHardRiskConfig) {
				if merged == nil || merged.MaxTotalExposure == nil || *merged.MaxTotalExposure != 1.00 {
					t.Fatalf("existing 1.00 must survive an out-of-range 3.00 patch, got %+v", merged)
				}
			},
		},
		{
			name: "Int caps reject out-of-range positive (>10000) but keep zero-as-clear",
			existing: &api.FundHardRiskConfig{
				MaxTradesPerDay:       intPtr(50),
				MaxTradesPerSymbolDay: intPtr(10),
				MaxQuoteAgeSeconds:    intPtr(120),
			},
			patch: &api.FundHardRiskConfig{
				MaxTradesPerDay:       intPtr(99999),  // out of range, preserve existing
				MaxTradesPerSymbolDay: intPtr(-5),     // negative -> ignore
				MaxQuoteAgeSeconds:    intPtr(100000), // out of range, preserve existing
			},
			check: func(t *testing.T, merged *api.FundHardRiskConfig) {
				if merged == nil {
					t.Fatal("merged is nil")
				}
				if merged.MaxTradesPerDay == nil || *merged.MaxTradesPerDay != 50 {
					t.Errorf("MaxTradesPerDay: out-of-range patch must preserve existing 50, got %+v", merged.MaxTradesPerDay)
				}
				if merged.MaxTradesPerSymbolDay == nil || *merged.MaxTradesPerSymbolDay != 10 {
					t.Errorf("MaxTradesPerSymbolDay: negative patch must preserve existing 10, got %+v", merged.MaxTradesPerSymbolDay)
				}
				if merged.MaxQuoteAgeSeconds == nil || *merged.MaxQuoteAgeSeconds != 120 {
					t.Errorf("MaxQuoteAgeSeconds: out-of-range patch must preserve existing 120, got %+v", merged.MaxQuoteAgeSeconds)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged := mergeFundHardRisk(tc.existing, tc.patch)
			tc.check(t, merged)
		})
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
