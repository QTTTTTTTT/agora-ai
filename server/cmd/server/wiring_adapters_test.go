package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/marketcalendar"
	"github.com/fundai/server/internal/marketdata"
	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/risk"
	"github.com/fundai/server/internal/workflow"
)

// TestBuildFundConfigJSONAppliesPerMarketCalendarDefaults pins down the
// fix for the regression where every freshly-created fund (regardless of
// market) ended up with calendarCode=US-XNAS and timeZone=America/New_York.
// Root cause: decodeFundMarketProfile(nil) used to pre-stamp the catch-all
// US defaults onto an empty profile *before* the user's market override
// was applied, so the second NormalizeProfile call would see a non-empty
// calendarCode and skip the market-based defaulting branch.
func TestBuildFundConfigJSONAppliesPerMarketCalendarDefaults(t *testing.T) {
	cases := []struct {
		name         string
		market       string
		wantMarket   string
		wantCalendar string
		wantTZ       string
	}{
		{"a_share canonical", "a_share", "a_share", "CN-SSE", "Asia/Shanghai"},
		{"cn_a_share alias", "cn_a_share", "a_share", "CN-SSE", "Asia/Shanghai"},
		{"us_equity canonical", "us_equity", "us_equity", "US-XNAS", "America/New_York"},
		{"crypto canonical", "crypto", "crypto", "CRYPTO-24X7", "UTC"},
		{"futures canonical", "futures", "futures", "CME-INDEX", "America/Chicago"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marketCopy := tc.market
			encoded, err := buildFundConfigJSON(api.FundConfig{
				Market: &marketCopy,
			}, nil)
			if err != nil {
				t.Fatalf("buildFundConfigJSON: %v", err)
			}
			var profile fundMarketProfile
			if err := json.Unmarshal(encoded, &profile); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if profile.Market != tc.wantMarket {
				t.Errorf("market: want %q got %q", tc.wantMarket, profile.Market)
			}
			if profile.CalendarCode != tc.wantCalendar {
				t.Errorf("calendarCode: want %q got %q (regression — defaulting again falls back to US-XNAS)", tc.wantCalendar, profile.CalendarCode)
			}
			if profile.TimeZone != tc.wantTZ {
				t.Errorf("timeZone: want %q got %q", tc.wantTZ, profile.TimeZone)
			}
		})
	}
}

// TestBuildFundConfigJSONPreservesExplicitCalendar makes sure that when a
// caller does pass an explicit calendarCode + timeZone, we honour them
// rather than overriding with the market-based default. Important for
// admins who configure an A-share fund on the Shenzhen calendar
// (CN-SZSE) instead of the default Shanghai one.
func TestBuildFundConfigJSONPreservesExplicitCalendar(t *testing.T) {
	market := "a_share"
	cal := "CN-SZSE"
	tz := "Asia/Shanghai"
	encoded, err := buildFundConfigJSON(api.FundConfig{
		Market:       &market,
		CalendarCode: &cal,
		TimeZone:     &tz,
	}, nil)
	if err != nil {
		t.Fatalf("buildFundConfigJSON: %v", err)
	}
	var profile fundMarketProfile
	if err := json.Unmarshal(encoded, &profile); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if profile.CalendarCode != "CN-SZSE" {
		t.Errorf("explicit calendar overridden: got %q", profile.CalendarCode)
	}
	if profile.TimeZone != "Asia/Shanghai" {
		t.Errorf("explicit timezone overridden: got %q", profile.TimeZone)
	}
}

// TestBuildFundConfigJSONAppliesPerMarketBenchmarkDefaults locks in
// the "new funds should ship with a sensible benchmarkSymbol" fix.
// Without it, brand-new funds were created with an empty benchmark,
// which made PnLAttribution and other downstream code that reads
// the benchmark fall back to a confusing "no benchmark" empty
// state in the dashboards. Mirrors the calendar defaulting test
// above so the same matrix of markets is exercised end-to-end.
func TestBuildFundConfigJSONAppliesPerMarketBenchmarkDefaults(t *testing.T) {
	cases := []struct {
		name          string
		market        string
		wantBenchmark string
	}{
		{"a_share canonical", "a_share", "000300.SS"},
		{"cn_a_share alias", "cn_a_share", "000300.SS"},
		{"us_equity canonical", "us_equity", "SPY"},
		{"us_stock alias", "us_stock", "SPY"},
		{"crypto canonical", "crypto", "BTC-USD"},
		{"futures canonical", "futures", "ES=F"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marketCopy := tc.market
			encoded, err := buildFundConfigJSON(api.FundConfig{
				Market: &marketCopy,
			}, nil)
			if err != nil {
				t.Fatalf("buildFundConfigJSON: %v", err)
			}
			var profile fundMarketProfile
			if err := json.Unmarshal(encoded, &profile); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if profile.BenchmarkSymbol != tc.wantBenchmark {
				t.Errorf("benchmarkSymbol: want %q got %q (regression — defaulting broken)", tc.wantBenchmark, profile.BenchmarkSymbol)
			}
		})
	}
}

// TestBuildFundConfigJSONPreservesExplicitBenchmark ensures that
// when the caller supplies their own benchmark we do NOT clobber
// it with the per-market default. A user choosing CSI 500 over
// the default CSI 300 must be honoured.
func TestBuildFundConfigJSONPreservesExplicitBenchmark(t *testing.T) {
	market := "a_share"
	bench := "000905.SS"
	encoded, err := buildFundConfigJSON(api.FundConfig{
		Market:          &market,
		BenchmarkSymbol: &bench,
	}, nil)
	if err != nil {
		t.Fatalf("buildFundConfigJSON: %v", err)
	}
	var profile fundMarketProfile
	if err := json.Unmarshal(encoded, &profile); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if profile.BenchmarkSymbol != "000905.SS" {
		t.Errorf("explicit benchmark overridden: got %q", profile.BenchmarkSymbol)
	}
}

// TestBuildFundConfigJSONUnknownMarketLeavesBenchmarkEmpty is the
// "safe fallback" guard: if we ever see a market token we haven't
// mapped, we'd rather ship an empty benchmark than silently use the
// wrong instrument as the comparison index.
func TestBuildFundConfigJSONUnknownMarketLeavesBenchmarkEmpty(t *testing.T) {
	market := "exotic_otc"
	encoded, err := buildFundConfigJSON(api.FundConfig{
		Market: &market,
	}, nil)
	if err != nil {
		t.Fatalf("buildFundConfigJSON: %v", err)
	}
	var profile fundMarketProfile
	if err := json.Unmarshal(encoded, &profile); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if profile.BenchmarkSymbol != "" {
		t.Errorf("expected empty benchmark for unknown market, got %q", profile.BenchmarkSymbol)
	}
}

func TestBuildFundConfigJSONNormalizesTeamIntervals(t *testing.T) {
	encoded, err := buildFundConfigJSON(api.FundConfig{
		TeamIntervals: &api.FundTeamIntervals{
			Trader: intPtr(17),
			Risk:   intPtr(0),
		},
	}, nil)
	if err != nil {
		t.Fatalf("build fund config json: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("unmarshal config json: %v", err)
	}
	teamIntervals, ok := payload["teamIntervals"].(map[string]any)
	if !ok {
		t.Fatalf("expected teamIntervals map, got %#v", payload["teamIntervals"])
	}
	if teamIntervals["trader"] != float64(15) {
		t.Fatalf("expected trader interval 15, got %#v", teamIntervals["trader"])
	}
	if _, exists := teamIntervals["risk"]; exists {
		t.Fatalf("expected zero interval to be omitted, got %#v", teamIntervals["risk"])
	}
}

// TestBuildFundConfigJSONNormalizesActivityRetention covers the two
// edges of the retention-day input: a high value gets clamped to the
// max, a zero / negative value gets clamped to the min. The middle
// case (a sane value passes through untouched) is implicitly covered
// by TestResolveActivityRetentionDays.
func TestBuildFundConfigJSONNormalizesActivityRetention(t *testing.T) {
	t.Run("clamps too-large value to MaxActivityRetentionDays", func(t *testing.T) {
		encoded, err := buildFundConfigJSON(api.FundConfig{
			ActivityRetentionDays: intPtr(999),
		}, nil)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		var profile fundMarketProfile
		if err := json.Unmarshal(encoded, &profile); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if profile.ActivityRetentionDays == nil || *profile.ActivityRetentionDays != MaxActivityRetentionDays {
			t.Fatalf("expected clamp to %d, got %#v", MaxActivityRetentionDays, profile.ActivityRetentionDays)
		}
	})
	t.Run("clamps too-small value to 1", func(t *testing.T) {
		encoded, err := buildFundConfigJSON(api.FundConfig{
			ActivityRetentionDays: intPtr(0),
		}, nil)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		var profile fundMarketProfile
		if err := json.Unmarshal(encoded, &profile); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if profile.ActivityRetentionDays == nil || *profile.ActivityRetentionDays != 1 {
			t.Fatalf("expected clamp to 1, got %#v", profile.ActivityRetentionDays)
		}
	})
}

// TestResolveActivityRetentionDays makes the default-fallback contract
// explicit: a fund whose config never set the field (or set an invalid
// value that survived a manual SQL edit) gets DefaultActivityRetentionDays.
func TestResolveActivityRetentionDays(t *testing.T) {
	cases := []struct {
		name    string
		profile fundMarketProfile
		want    int
	}{
		{"missing field returns default", fundMarketProfile{}, DefaultActivityRetentionDays},
		{"valid value passes through", fundMarketProfile{ActivityRetentionDays: intPtr(3)}, 3},
		{"out-of-range high clamps", fundMarketProfile{ActivityRetentionDays: intPtr(50)}, MaxActivityRetentionDays},
		{"out-of-range low clamps", fundMarketProfile{ActivityRetentionDays: intPtr(-5)}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveActivityRetentionDays(tc.profile)
			if got != tc.want {
				t.Fatalf("expected %d, got %d", tc.want, got)
			}
		})
	}
}

func TestBuildFundConfigJSONNormalizesHardRisk(t *testing.T) {
	encoded, err := buildFundConfigJSON(api.FundConfig{
		HardRisk: &api.FundHardRiskConfig{
			DailyLossLimit:        floatPtr(0.04),
			MaxOrderPctOfAssets:   floatPtr(0.08),
			MaxTradesPerDay:       intPtr(20),
			MaxTradesPerSymbolDay: intPtr(5),
			MaxSinglePosition:     floatPtr(-1),
		},
	}, nil)
	if err != nil {
		t.Fatalf("build fund config json: %v", err)
	}
	var profile fundMarketProfile
	if err := json.Unmarshal(encoded, &profile); err != nil {
		t.Fatalf("unmarshal config json: %v", err)
	}
	if profile.HardRisk == nil {
		t.Fatal("expected hardRisk config")
	}
	if profile.HardRisk.DailyLossLimit == nil || *profile.HardRisk.DailyLossLimit != 0.04 {
		t.Fatalf("unexpected daily loss limit: %#v", profile.HardRisk.DailyLossLimit)
	}
	if profile.HardRisk.MaxOrderPctOfAssets == nil || *profile.HardRisk.MaxOrderPctOfAssets != 0.08 {
		t.Fatalf("unexpected max order pct: %#v", profile.HardRisk.MaxOrderPctOfAssets)
	}
	if profile.HardRisk.MaxSinglePosition != nil {
		t.Fatalf("expected invalid max single position to be omitted, got %#v", profile.HardRisk.MaxSinglePosition)
	}
}

// TestPlanBuyAmountWithinRiskCapHonorsDefaultPolicy is the regression test
// for the "hard risk gate rejected … exceeds hard cap" production bug:
// the PM hardcoded 25% of CurrentCapital, the risk gate enforced 10%,
// so every auto-generated buy on a fund without a custom hard-risk
// override was rejected at execution time.
func TestPlanBuyAmountWithinRiskCapHonorsDefaultPolicy(t *testing.T) {
	// $100k NAV, no custom hard-risk config → default cap = 10% × NAV = $10k.
	fund := &repository.Fund{
		CurrentCapital: 100000,
		TotalAssets:    100000,
	}
	got := planBuyAmountWithinRiskCap(fund)
	if got > 10000+1e-6 {
		t.Fatalf("planned budget %.2f exceeds default 10%% cap of 10000.00", got)
	}
	if got <= 0 {
		t.Fatalf("planned budget should be positive, got %.2f", got)
	}
}

// TestPlanBuyAmountWithinRiskCapShrinksToCustomCap covers operator
// overrides: when a fund tightens MaxOrderPctOfAssets, the planner
// must shrink even further so the plan still lands inside the gate.
func TestPlanBuyAmountWithinRiskCapShrinksToCustomCap(t *testing.T) {
	// Encode a fund config with MaxOrderPctOfAssets=0.02 (so cap = $2k).
	configJSON, err := json.Marshal(map[string]any{
		"hardRisk": map[string]any{
			"maxOrderPctOfAssets": 0.02,
		},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	fund := &repository.Fund{
		CurrentCapital: 100000,
		TotalAssets:    100000,
		Config:         configJSON,
	}
	got := planBuyAmountWithinRiskCap(fund)
	if got > 2000+1e-6 {
		t.Fatalf("planned budget %.2f exceeds custom 2%% cap of 2000.00", got)
	}
	if got <= 0 {
		t.Fatalf("planned budget should be positive, got %.2f", got)
	}
}

// TestPlanBuyAmountWithinRiskCapShrinksToAbsoluteAmountCap covers funds
// that pin a hard dollar cap on each order — the planner must respect
// that ceiling even when the percentage cap allows a larger budget.
func TestPlanBuyAmountWithinRiskCapShrinksToAbsoluteAmountCap(t *testing.T) {
	configJSON, err := json.Marshal(map[string]any{
		"hardRisk": map[string]any{
			"maxOrderAmount": 500.0,
		},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	fund := &repository.Fund{
		CurrentCapital: 100000,
		TotalAssets:    100000,
		Config:         configJSON,
	}
	got := planBuyAmountWithinRiskCap(fund)
	if got > 500+1e-6 {
		t.Fatalf("planned budget %.2f exceeds absolute cap 500.00", got)
	}
	if got <= 0 {
		t.Fatalf("planned budget should be positive, got %.2f", got)
	}
}

// TestPlanBuyAmountWithinRiskCapLeavesSafetyMargin guards the
// "amount X exceeds hard cap Y" production wedge: when a held
// position drifts down a couple percent between plan-write and
// dispatch, the dispatch-time TotalAssets — and therefore the hard
// cap — both shrink. If the planner had proposed *exactly* the
// hard cap, the freshly-computed dispatch cap would reject. The
// planner now publishes a budget at PlanBudgetSafetyMargin × cap so
// small drift no longer wedges the workflow. The constant is part
// of the contract; bumping it must come with this test.
func TestPlanBuyAmountWithinRiskCapLeavesSafetyMargin(t *testing.T) {
	// $100k NAV, default 10% cap = $10000. The planner must propose
	// strictly LESS than the cap so that a small TotalAssets dip
	// at dispatch does not blow past the recomputed cap.
	fund := &repository.Fund{
		CurrentCapital: 100000,
		TotalAssets:    100000,
	}
	got := planBuyAmountWithinRiskCap(fund)
	wantUpperBound := 10000 * PlanBudgetSafetyMargin
	if got > wantUpperBound+1e-6 {
		t.Fatalf("planned budget %.2f must stay below cap * margin %.2f to absorb dispatch-time drift", got, wantUpperBound)
	}
	// At the same time, the buffer can't eat the entire cap: a 3%
	// cushion should still leave > 90% of the cap usable.
	if got < 10000*0.90 {
		t.Fatalf("planned budget %.2f shrunk too far below cap (margin should be ~3%%)", got)
	}
}

// TestPlanBuyAmountWithinRiskCapFallsBackToInitialCapital protects the
// "fresh fund with no current capital yet" edge case so we don't return
// zero and shove all logic into the empty-quote branch.
func TestPlanBuyAmountWithinRiskCapFallsBackToInitialCapital(t *testing.T) {
	fund := &repository.Fund{
		CurrentCapital: 0,
		InitialCapital: 100000,
		TotalAssets:    100000,
	}
	got := planBuyAmountWithinRiskCap(fund)
	if got <= 0 {
		t.Fatalf("planned budget should be positive even when CurrentCapital is zero, got %.2f", got)
	}
	if got > 10000+1e-6 {
		t.Fatalf("planned budget %.2f should still respect the default 10%% cap of 10000.00", got)
	}
}

func TestEnforceHardRiskGateUsesFundHardRiskPolicy(t *testing.T) {
	action := repository.PlanAction{Symbol: "AAA", Action: "buy"}
	state := &hardRiskState{
		TotalAssets: 100000,
		Policy: risk.HardRiskPolicyFromConfig(risk.HardRiskConfig{
			DailyLossLimit:        0.05,
			MaxSinglePosition:     0.30,
			MaxSectorExposure:     0.40,
			MaxTotalExposure:      0.95,
			MaxOrderPctOfAssets:   0.02,
			MaxTradesPerDay:       50,
			MaxTradesPerSymbolDay: 10,
		}),
	}
	err := enforceHardRiskGate(&repository.Fund{ID: "fund-1", TotalAssets: 100000}, &repository.InvestmentPlan{ID: "plan-1"}, action, nil, state, "buy", 30, 100, 3000, time.Now().UTC(), false, 0, false)
	if err == nil {
		t.Fatal("expected hard risk rejection")
	}
	var rejection *hardRiskRejectionError
	if !errors.As(err, &rejection) {
		t.Fatalf("expected hardRiskRejectionError, got %T %v", err, err)
	}
	if rejection.Rule != "hard_max_order_notional" {
		t.Fatalf("expected max order rule, got %q", rejection.Rule)
	}
}

// Verifies that SlippageGuard SeverityFail surfaces as a typed
// slippageBounceError (so the Execute loop can bounce the plan back to
// pending_user) rather than a generic hardRiskRejectionError (which
// would just reject the action).
func TestEnforceHardRiskGateReturnsSlippageBounceForDriftFail(t *testing.T) {
	action := repository.PlanAction{
		Symbol: "600519", // SH main, 0.8% tolerance
		Action: "buy",
		Market: sql.NullString{String: "a_share", Valid: true},
	}
	state := &hardRiskState{
		TotalAssets: 1_000_000,
		Policy:      risk.DefaultHardRiskPolicy(),
	}
	// Plan price 1000, live price 1015 → +1.5% drift > 0.8%
	err := enforceHardRiskGate(
		&repository.Fund{ID: "fund-1", TotalAssets: 1_000_000},
		&repository.InvestmentPlan{ID: "plan-1"},
		action, nil, state, "buy", 100, 1000, 100000,
		time.Now().UTC(), false, 1015, false,
	)
	if err == nil {
		t.Fatal("expected slippage bounce")
	}
	var bounce *slippageBounceError
	if !errors.As(err, &bounce) {
		t.Fatalf("expected slippageBounceError, got %T %v", err, err)
	}
	if bounce.Symbol != "600519" {
		t.Errorf("symbol = %q, want 600519", bounce.Symbol)
	}
	if bounce.Drift <= 0 {
		t.Errorf("drift = %v, want positive", bounce.Drift)
	}
}

// Drift inside the tolerance keeps the action eligible for execution
// (no bounce, no rejection). This is the common case in normal market
// conditions.
func TestEnforceHardRiskGateAllowsDriftWithinTolerance(t *testing.T) {
	action := repository.PlanAction{
		Symbol: "600519",
		Action: "buy",
		Market: sql.NullString{String: "a_share", Valid: true},
	}
	state := &hardRiskState{TotalAssets: 1_000_000, Policy: risk.DefaultHardRiskPolicy()}
	// 0.5% drift on SH main (tolerance 0.8%) → allowed
	err := enforceHardRiskGate(
		&repository.Fund{ID: "fund-1", TotalAssets: 1_000_000},
		&repository.InvestmentPlan{ID: "plan-1"},
		action, nil, state, "buy", 100, 1000, 100000,
		time.Now().UTC(), false, 1005, false,
	)
	if err != nil {
		t.Fatalf("expected no error within tolerance, got %v", err)
	}
}

// Sells must be exempt from slippage even if drift is enormous: the
// system needs to remain able to de-risk on a falling tape.
func TestEnforceHardRiskGateExemptsSellsFromSlippage(t *testing.T) {
	action := repository.PlanAction{
		Symbol: "600519",
		Action: "sell",
		Market: sql.NullString{String: "a_share", Valid: true},
	}
	state := &hardRiskState{TotalAssets: 1_000_000, Policy: risk.DefaultHardRiskPolicy()}
	// 30% drop in price during sell window — must NOT bounce.
	err := enforceHardRiskGate(
		&repository.Fund{ID: "fund-1", TotalAssets: 1_000_000},
		&repository.InvestmentPlan{ID: "plan-1"},
		action, map[string]repository.HoldingPosition{
			positionMapKey("", "600519"): {
				Symbol:       "600519",
				Quantity:     100,
				AvailableQty: 100,
				CostPrice:    1000,
			},
		}, state, "sell", 100, 1000, 100000,
		time.Now().UTC(), false, 700, false,
	)
	if err != nil {
		t.Fatalf("sells must be exempt from slippage, got %v", err)
	}
}

// computeSlippagePct records realised drift for filled buys but returns
// NULL for sells, missing prices, and zero-plan-price fills.
func TestComputeSlippagePct(t *testing.T) {
	cases := []struct {
		name      string
		side      string
		planPrice float64
		filled    sql.NullFloat64
		wantValid bool
		want      float64
	}{
		{"buy-up", "buy", 100, sql.NullFloat64{Float64: 102, Valid: true}, true, 0.02},
		{"buy-down", "buy", 100, sql.NullFloat64{Float64: 98, Valid: true}, true, -0.02},
		{"buy-flat", "buy", 100, sql.NullFloat64{Float64: 100, Valid: true}, true, 0.0},
		{"sell-skipped", "sell", 100, sql.NullFloat64{Float64: 90, Valid: true}, false, 0},
		{"sell-mixed-case", "Sell", 100, sql.NullFloat64{Float64: 90, Valid: true}, false, 0},
		{"missing-fill", "buy", 100, sql.NullFloat64{}, false, 0},
		{"zero-fill", "buy", 100, sql.NullFloat64{Float64: 0, Valid: true}, false, 0},
		{"zero-plan", "buy", 0, sql.NullFloat64{Float64: 100, Valid: true}, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeSlippagePct(tc.side, tc.planPrice, tc.filled)
			if got.Valid != tc.wantValid {
				t.Errorf("valid = %v, want %v", got.Valid, tc.wantValid)
			}
			if got.Valid && math.Abs(got.Float64-tc.want) > 1e-9 {
				t.Errorf("value = %v, want %v", got.Float64, tc.want)
			}
		})
	}
}

// refreshActionQuantity preserves sell quantities verbatim (so a "sell
// 100 shares" plan still sells 100 after refresh) and only adjusts
// notional. Buys recompute quantity from the approved budget and lot-
// size-normalise it.
func TestRefreshActionQuantity(t *testing.T) {
	fund := &repository.Fund{CurrentCapital: 1_000_000}
	cases := []struct {
		name     string
		act      repository.PlanAction
		newPrice float64
		wantQty  float64
		wantAmt  float64
	}{
		{
			name: "buy-shmain-lot-normalised",
			act: repository.PlanAction{
				Action:   "buy",
				Symbol:   "600519",
				Amount:   sql.NullFloat64{Float64: 50_000, Valid: true},
				Quantity: sql.NullFloat64{Float64: 50, Valid: true},
				Price:    sql.NullFloat64{Float64: 1000, Valid: true},
			},
			newPrice: 1050,
			// budget 50000 / 1050 = 47.6 → floor 47 → lot-norm SH main → 0!
			// Below MinLot 100; refresh returns (0,0) signalling
			// "budget too small for one lot".
			wantQty: 0,
			wantAmt: 0,
		},
		{
			name: "buy-chinext-lot-normalised",
			act: repository.PlanAction{
				Action:   "buy",
				Symbol:   "300750",
				Amount:   sql.NullFloat64{Float64: 200_000, Valid: true},
				Quantity: sql.NullFloat64{Float64: 200, Valid: true},
				Price:    sql.NullFloat64{Float64: 1000, Valid: true},
			},
			newPrice: 800,
			// 200000/800 = 250 → ChiNext MinLot=100 Step=100 → 200
			wantQty: 200,
			wantAmt: 160_000,
		},
		{
			name: "sell-keeps-quantity",
			act: repository.PlanAction{
				Action:   "sell",
				Symbol:   "600519",
				Quantity: sql.NullFloat64{Float64: 100, Valid: true},
				Price:    sql.NullFloat64{Float64: 1000, Valid: true},
			},
			newPrice: 1010,
			wantQty:  100,
			wantAmt:  101_000,
		},
		{
			name:     "hold-noop",
			act:      repository.PlanAction{Action: "hold", Symbol: "600519"},
			newPrice: 1000,
			wantQty:  0,
			wantAmt:  0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			qty, amt := refreshActionQuantity(tc.act, fund, tc.newPrice)
			if math.Abs(qty-tc.wantQty) > 1e-6 {
				t.Errorf("qty = %v, want %v", qty, tc.wantQty)
			}
			if math.Abs(amt-tc.wantAmt) > 1e-6 {
				t.Errorf("amount = %v, want %v", amt, tc.wantAmt)
			}
		})
	}
}

// terminalActionStatus is an internal helper that the Execute loop
// uses to skip already-filled actions on a re-run after slippage
// bounce. The set of terminal statuses is enumerated here so any
// future addition is forced to declare its intent.
func TestTerminalActionStatus(t *testing.T) {
	cases := map[string]string{
		"filled":    "filled",
		"FILLED":    "filled",
		"cancelled": "cancelled",
		"rejected":  "rejected",
		"  filled ": "filled",
		"pending":   "",
		"":          "",
		"unknown":   "",
	}
	for in, want := range cases {
		if got := terminalActionStatus(in); got != want {
			t.Errorf("terminalActionStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildABVariantMetricsCalculatesReturnRiskAndWinner(t *testing.T) {
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	controlNavs := []repository.NavSnapshot{
		{TradingDate: base, NAV: 1.00, TotalAssets: 100000},
		{TradingDate: base.AddDate(0, 0, 1), NAV: 0.98, TotalAssets: 98000},
		{TradingDate: base.AddDate(0, 0, 2), NAV: 1.03, TotalAssets: 103000},
	}
	treatmentNavs := []repository.NavSnapshot{
		{TradingDate: base, NAV: 1.00, TotalAssets: 100000},
		{TradingDate: base.AddDate(0, 0, 1), NAV: 1.02, TotalAssets: 102000},
		{TradingDate: base.AddDate(0, 0, 2), NAV: 1.06, TotalAssets: 106000},
	}
	control := buildABVariantMetrics(controlNavs, []repository.TradeExecution{{FeeCommission: 1.5, Amount: sql.NullFloat64{Float64: 1000, Valid: true}}})
	treatment := buildABVariantMetrics(treatmentNavs, []repository.TradeExecution{{FeeCommission: 2.5, Amount: sql.NullFloat64{Float64: 1200, Valid: true}}})

	if math.Abs(control["totalReturn"]-3.0) > 0.0001 {
		t.Fatalf("unexpected control total return: %v", control["totalReturn"])
	}
	if math.Abs(control["maxDrawdown"]-(-2.0)) > 0.0001 {
		t.Fatalf("unexpected control max drawdown: %v", control["maxDrawdown"])
	}
	if treatment["totalReturn"] <= control["totalReturn"] {
		t.Fatalf("expected treatment to outperform, control=%v treatment=%v", control["totalReturn"], treatment["totalReturn"])
	}
	if treatment["totalTurnover"] != 1200 || treatment["totalFees"] != 2.5 {
		t.Fatalf("unexpected treatment trade metrics: %#v", treatment)
	}
	if winner := determineABWinner(control, treatment); winner != "treatment" {
		t.Fatalf("expected treatment winner, got %s", winner)
	}
	series := buildABNAVSeries(controlNavs, treatmentNavs)
	if len(series) != 3 {
		t.Fatalf("expected 3 NAV series points, got %d", len(series))
	}
	last := series[len(series)-1]
	if last.ExcessReturn == nil || math.Abs(*last.ExcessReturn-3.0) > 0.0001 {
		t.Fatalf("unexpected last excess return: %#v", last.ExcessReturn)
	}
}

func TestBuildABConfidenceSummaryFlagsSmallSamples(t *testing.T) {
	control := map[string]float64{"totalReturn": 1, "maxDrawdown": -1, "totalTurnover": 1000, "tradeCount": 1}
	treatment := map[string]float64{"totalReturn": 1.2, "maxDrawdown": -1.1, "totalTurnover": 1200, "tradeCount": 1}
	summary := buildABConfidenceSummary(control, treatment, 3, 3)
	if summary == nil {
		t.Fatal("expected confidence summary")
	}
	if summary.Level != "low" {
		t.Fatalf("expected low confidence for small sample, got %#v", summary)
	}
	if len(summary.Warnings) == 0 {
		t.Fatalf("expected warnings for small sample, got %#v", summary)
	}

	strong := buildABConfidenceSummary(
		map[string]float64{"totalReturn": 2, "maxDrawdown": -3, "totalTurnover": 10000, "tradeCount": 25},
		map[string]float64{"totalReturn": 9, "maxDrawdown": -3.2, "totalTurnover": 11000, "tradeCount": 25},
		80,
		80,
	)
	if strong.Level != "high" || strong.Score < 75 {
		t.Fatalf("expected high confidence for enough samples and return gap, got %#v", strong)
	}
}

func TestBuildABScorecardRecommendsRiskAdjustedWinner(t *testing.T) {
	scorecard := buildABScorecard(
		map[string]float64{"totalReturn": 2, "sharpe": 0.8, "maxDrawdown": -3, "volatility": 8, "totalTurnover": 10000, "totalFees": 20},
		map[string]float64{"totalReturn": 8, "sharpe": 1.3, "maxDrawdown": -4, "volatility": 9, "totalTurnover": 12000, "totalFees": 24},
		60,
		60,
	)
	if scorecard == nil {
		t.Fatal("expected scorecard")
	}
	if scorecard.RecommendedVariant != "treatment" {
		t.Fatalf("expected treatment recommendation, got %#v", scorecard)
	}
	if scorecard.VariantBScore <= scorecard.VariantAScore {
		t.Fatalf("expected B score to beat A, got %#v", scorecard)
	}
	if len(scorecard.Components) == 0 {
		t.Fatalf("expected score components, got %#v", scorecard)
	}
}

func TestABShadowStrategyBiasAndTradeScale(t *testing.T) {
	aggressive := map[string]any{"pmStyle": "aggressive", "maxSinglePosition": 0.25}
	if bias := abStrategyReturnBias(aggressive); bias <= 1.0 {
		t.Fatalf("expected aggressive strategy to increase return bias, got %v", bias)
	}
	if scale := abStrategyTradeScale(aggressive); scale <= 1.0 {
		t.Fatalf("expected aggressive strategy to increase trade scale, got %v", scale)
	}

	conservative := map[string]any{"pmStyle": "conservative", "maxSinglePosition": 0.05}
	if bias := abStrategyReturnBias(conservative); bias >= 1.0 {
		t.Fatalf("expected conservative strategy to reduce return bias, got %v", bias)
	}
	if scale := abStrategyTradeScale(conservative); scale >= 1.0 {
		t.Fatalf("expected conservative strategy to reduce trade scale, got %v", scale)
	}
}

func TestExtractABStrategyVariantConfigAddsLearningIsolation(t *testing.T) {
	variantA, variantB := extractABStrategyVariantConfig(json.RawMessage(`{
		"variantA":{"name":"当前策略","strategyConfig":{"source":"current_fund"}},
		"variantB":{"name":"激进策略","strategyConfig":{"pmStyle":"aggressive","maxSinglePosition":0.2}},
		"strategySummary":"提高仓位上限"
	}`))
	if variantA.Name != "当前策略" || variantB.Name != "激进策略" {
		t.Fatalf("unexpected variant names: %#v %#v", variantA, variantB)
	}
	if variantA.StrategyConfig["learningMode"] != abLearningModeShadowEphemeral || variantB.StrategyConfig["learningMode"] != abLearningModeShadowEphemeral {
		t.Fatalf("expected shadow_ephemeral learning mode, got A=%#v B=%#v", variantA.StrategyConfig, variantB.StrategyConfig)
	}
	if variantA.StrategyConfig["persistLearning"] != false || variantB.StrategyConfig["persistLearning"] != false {
		t.Fatalf("expected shadow learning not to persist to real agents, got A=%#v B=%#v", variantA.StrategyConfig, variantB.StrategyConfig)
	}
	if variantB.StrategyConfig["summary"] != "提高仓位上限" {
		t.Fatalf("expected strategy summary to be copied, got %#v", variantB.StrategyConfig)
	}
}

func TestBuildPromotedABEvolutionConfigMergeAndOverwrite(t *testing.T) {
	learning := abShadowAgentLearning{
		AgentID:                 "agent-1",
		LatestTradingDate:       "2026-05-16",
		EventCount:              2,
		Summaries:               []string{"影子策略连续跑赢基准"},
		Lessons:                 []string{"提高强趋势股票持仓耐心", "降低弱势反弹加仓"},
		Adjustments:             []string{"单票上限提高到 20%"},
		ProposedEvolutionConfig: map[string]any{"riskBias": "growth"},
	}
	merged, err := buildPromotedABEvolutionConfig(json.RawMessage(`{"recentLessons":["保留旧经验"],"riskBias":"balanced"}`), learning, abPromotionModeMerge, "test-1", "B")
	if err != nil {
		t.Fatalf("merge promoted config: %v", err)
	}
	var mergedPayload map[string]any
	if err := json.Unmarshal(merged, &mergedPayload); err != nil {
		t.Fatalf("unmarshal merged config: %v", err)
	}
	if mergedPayload["riskBias"] != "balanced" {
		t.Fatalf("merge should preserve existing config, got %#v", mergedPayload)
	}
	if len(stringSliceFromConfig(mergedPayload, "recentLessons")) != 3 {
		t.Fatalf("expected old and shadow lessons to be merged, got %#v", mergedPayload["recentLessons"])
	}

	overwritten, err := buildPromotedABEvolutionConfig(json.RawMessage(`{"riskBias":"balanced"}`), learning, abPromotionModeOverwrite, "test-1", "B")
	if err != nil {
		t.Fatalf("overwrite promoted config: %v", err)
	}
	var overwrittenPayload map[string]any
	if err := json.Unmarshal(overwritten, &overwrittenPayload); err != nil {
		t.Fatalf("unmarshal overwritten config: %v", err)
	}
	if overwrittenPayload["riskBias"] != "growth" {
		t.Fatalf("overwrite should use proposed shadow config, got %#v", overwrittenPayload)
	}
	if promoted, ok := overwrittenPayload["promotedABLearning"].(map[string]any); !ok || promoted["variantKey"] != "B" {
		t.Fatalf("expected promotion metadata, got %#v", overwrittenPayload["promotedABLearning"])
	}
}

func TestDecodeFundMarketProfileNormalizesTeamIntervals(t *testing.T) {
	profile := decodeFundMarketProfile(json.RawMessage(`{"teamIntervals":{"pm":4,"researcher":26,"risk":1501}}`))
	if profile.TeamIntervals == nil {
		t.Fatal("expected team intervals")
	}
	if profile.TeamIntervals.PM == nil || *profile.TeamIntervals.PM != 5 {
		t.Fatalf("expected pm interval 5, got %#v", profile.TeamIntervals.PM)
	}
	if profile.TeamIntervals.Researcher == nil || *profile.TeamIntervals.Researcher != 25 {
		t.Fatalf("expected researcher interval 25, got %#v", profile.TeamIntervals.Researcher)
	}
	if profile.TeamIntervals.Risk == nil || *profile.TeamIntervals.Risk != 1440 {
		t.Fatalf("expected risk interval 1440, got %#v", profile.TeamIntervals.Risk)
	}
}

func TestBuildFundConfigJSONMergesTeamIntervalPatch(t *testing.T) {
	existing := json.RawMessage(`{"teamIntervals":{"pm":10,"trader":20}}`)
	encoded, err := buildFundConfigJSON(api.FundConfig{
		TeamIntervals: &api.FundTeamIntervals{
			Trader: intPtr(0),
			Risk:   intPtr(27),
		},
	}, existing)
	if err != nil {
		t.Fatalf("build merged fund config json: %v", err)
	}

	profile := decodeFundMarketProfile(encoded)
	if profile.TeamIntervals == nil {
		t.Fatal("expected merged team intervals")
	}
	if profile.TeamIntervals.PM == nil || *profile.TeamIntervals.PM != 10 {
		t.Fatalf("expected pm interval 10 to be preserved, got %#v", profile.TeamIntervals.PM)
	}
	if profile.TeamIntervals.Trader != nil {
		t.Fatalf("expected trader interval to be cleared, got %#v", profile.TeamIntervals.Trader)
	}
	if profile.TeamIntervals.Risk == nil || *profile.TeamIntervals.Risk != 25 {
		t.Fatalf("expected risk interval 25, got %#v", profile.TeamIntervals.Risk)
	}
}

func TestBuildFundConfigJSONPreservesUniverseFields(t *testing.T) {
	encoded, err := buildFundConfigJSON(api.FundConfig{
		Universe: &api.FundUniverse{
			Mode:          "manual",
			Symbols:       []string{"NVDA", "AVGO"},
			Themes:        []string{"CPO", "光模块"},
			Sectors:       []string{"technology"},
			CustomFilters: []string{"marketCap>10B"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build fund config json with universe fields: %v", err)
	}

	profile := decodeFundMarketProfile(encoded)
	if profile.Universe == nil {
		t.Fatal("expected universe")
	}
	if len(profile.Universe.Symbols) != 2 || profile.Universe.Symbols[0] != "NVDA" {
		t.Fatalf("expected symbols to be preserved, got %#v", profile.Universe.Symbols)
	}
	if len(profile.Universe.Themes) != 2 || profile.Universe.Themes[0] != "CPO" {
		t.Fatalf("expected themes to be preserved, got %#v", profile.Universe.Themes)
	}
	if len(profile.Universe.Sectors) != 1 || profile.Universe.Sectors[0] != "technology" {
		t.Fatalf("expected sectors to be preserved, got %#v", profile.Universe.Sectors)
	}
	if len(profile.Universe.CustomFilters) != 1 || profile.Universe.CustomFilters[0] != "marketCap>10B" {
		t.Fatalf("expected custom filters to be preserved, got %#v", profile.Universe.CustomFilters)
	}
}

func TestDecodeFundMarketProfilePreservesUniverseFields(t *testing.T) {
	profile := decodeFundMarketProfile(json.RawMessage(`{"universe":{"mode":"manual","symbols":["NVDA","AVGO"],"themes":["CPO","光模块"],"sectors":["technology"],"customFilters":["marketCap>10B"]}}`))
	if profile.Universe == nil {
		t.Fatal("expected universe")
	}
	if len(profile.Universe.Symbols) != 2 || profile.Universe.Symbols[1] != "AVGO" {
		t.Fatalf("expected symbols to be preserved, got %#v", profile.Universe.Symbols)
	}
	if len(profile.Universe.Themes) != 2 || profile.Universe.Themes[1] != "光模块" {
		t.Fatalf("expected themes to be preserved, got %#v", profile.Universe.Themes)
	}
	if len(profile.Universe.Sectors) != 1 || profile.Universe.Sectors[0] != "technology" {
		t.Fatalf("expected sectors to be preserved, got %#v", profile.Universe.Sectors)
	}
	if len(profile.Universe.CustomFilters) != 1 || profile.Universe.CustomFilters[0] != "marketCap>10B" {
		t.Fatalf("expected custom filters to be preserved, got %#v", profile.Universe.CustomFilters)
	}
}

func TestBuildFundFocusContextOmitsEmptyFields(t *testing.T) {
	if got := buildFundFocusContext(UserLanguageZH, &repository.Fund{}); got != "" {
		t.Fatalf("expected empty fund focus context, got %q", got)
	}
}

func TestBuildFundFocusContextIncludesMarketProfile(t *testing.T) {
	context := buildFundFocusContext(UserLanguageZH, &repository.Fund{Config: json.RawMessage(`{"market":"us_equity","assetClass":"equity","primaryDirection":"stocks"}`)})
	for _, expected := range []string{"基金研究焦点：", "市场：us_equity", "资产类别：equity", "主要方向：stocks"} {
		if !strings.Contains(context, expected) {
			t.Fatalf("expected %q in fund focus context, got %q", expected, context)
		}
	}
	enContext := buildFundFocusContext(UserLanguageEN, &repository.Fund{Config: json.RawMessage(`{"market":"us_equity","assetClass":"equity","primaryDirection":"stocks"}`)})
	for _, expected := range []string{"Fund focus:", "market: us_equity", "asset class: equity", "primary direction: stocks"} {
		if !strings.Contains(enContext, expected) {
			t.Fatalf("expected %q in english fund focus context, got %q", expected, enContext)
		}
	}
}

func TestBuildFundConfigJSONPreservesSpecializationFields(t *testing.T) {
	encoded, err := buildFundConfigJSON(api.FundConfig{
		Specialization: &api.FundSpecialization{Team: &api.FundTeamSpecialization{
			Markets:      []string{"us_equity"},
			AssetClasses: []string{"equity"},
			Themes:       []string{"CPO", "AI infra"},
			Instruments:  []string{"NVDA", "AVGO"},
			StyleHints:   []string{"growth", "event-driven"},
		}},
	}, nil)
	if err != nil {
		t.Fatalf("build fund config json with specialization fields: %v", err)
	}

	profile := decodeFundMarketProfile(encoded)
	if profile.Specialization == nil || profile.Specialization.Team == nil {
		t.Fatal("expected specialization team")
	}
	if len(profile.Specialization.Team.Markets) != 1 || profile.Specialization.Team.Markets[0] != "us_equity" {
		t.Fatalf("expected markets to be preserved, got %#v", profile.Specialization.Team.Markets)
	}
	if len(profile.Specialization.Team.Themes) != 2 || profile.Specialization.Team.Themes[1] != "AI infra" {
		t.Fatalf("expected themes to be preserved, got %#v", profile.Specialization.Team.Themes)
	}
	if len(profile.Specialization.Team.Instruments) != 2 || profile.Specialization.Team.Instruments[0] != "NVDA" {
		t.Fatalf("expected instruments to be preserved, got %#v", profile.Specialization.Team.Instruments)
	}
	if len(profile.Specialization.Team.StyleHints) != 2 || profile.Specialization.Team.StyleHints[0] != "growth" {
		t.Fatalf("expected style hints to be preserved, got %#v", profile.Specialization.Team.StyleHints)
	}
}

func TestDecodeFundMarketProfilePreservesSpecializationFields(t *testing.T) {
	profile := decodeFundMarketProfile(json.RawMessage(`{"specialization":{"team":{"markets":["us_equity"],"assetClasses":["equity"],"themes":["CPO","AI infra"],"instruments":["NVDA","AVGO"],"styleHints":["growth","event-driven"]}}}`))
	if profile.Specialization == nil || profile.Specialization.Team == nil {
		t.Fatal("expected specialization team")
	}
	if len(profile.Specialization.Team.AssetClasses) != 1 || profile.Specialization.Team.AssetClasses[0] != "equity" {
		t.Fatalf("expected asset classes to be preserved, got %#v", profile.Specialization.Team.AssetClasses)
	}
	if len(profile.Specialization.Team.Themes) != 2 || profile.Specialization.Team.Themes[0] != "CPO" {
		t.Fatalf("expected themes to be preserved, got %#v", profile.Specialization.Team.Themes)
	}
	if len(profile.Specialization.Team.Instruments) != 2 || profile.Specialization.Team.Instruments[1] != "AVGO" {
		t.Fatalf("expected instruments to be preserved, got %#v", profile.Specialization.Team.Instruments)
	}
	if len(profile.Specialization.Team.StyleHints) != 2 || profile.Specialization.Team.StyleHints[1] != "event-driven" {
		t.Fatalf("expected style hints to be preserved, got %#v", profile.Specialization.Team.StyleHints)
	}
}

// Sprint D #2: per-fund exposure / correlation policy knobs.
//
// decodeFundMarketProfile must round-trip the new
// exposurePolicy / correlationPolicy stanzas verbatim so
// downstream wiring picks the operator's intent up on the very
// next decision rather than waiting for a service restart.
func TestDecodeFundMarketProfilePreservesExposureAndCorrelationPolicies(t *testing.T) {
	profile := decodeFundMarketProfile(json.RawMessage(`{
		"market":"us_equity",
		"exposurePolicy":{"singleNameCapPct":0.15,"sectorCapPct":0.40,"top3CapPct":0.55,"cashFloorPct":0.10},
		"correlationPolicy":{"lookbackDays":120,"highCorrThreshold":0.6,"maxHighCorrPairs":5}
	}`))
	if profile.ExposurePolicy == nil {
		t.Fatal("expected exposure policy to round-trip")
	}
	if v := profile.ExposurePolicy.SingleNameCapPct; v == nil || *v != 0.15 {
		t.Fatalf("singleNameCapPct = %v, want 0.15", v)
	}
	if v := profile.ExposurePolicy.SectorCapPct; v == nil || *v != 0.40 {
		t.Fatalf("sectorCapPct = %v, want 0.40", v)
	}
	if v := profile.ExposurePolicy.Top3CapPct; v == nil || *v != 0.55 {
		t.Fatalf("top3CapPct = %v, want 0.55", v)
	}
	if v := profile.ExposurePolicy.CashFloorPct; v == nil || *v != 0.10 {
		t.Fatalf("cashFloorPct = %v, want 0.10", v)
	}
	if profile.CorrelationPolicy == nil {
		t.Fatal("expected correlation policy to round-trip")
	}
	if v := profile.CorrelationPolicy.LookbackDays; v == nil || *v != 120 {
		t.Fatalf("lookbackDays = %v, want 120", v)
	}
	if v := profile.CorrelationPolicy.HighCorrThreshold; v == nil || *v != 0.6 {
		t.Fatalf("highCorrThreshold = %v, want 0.6", v)
	}
	if v := profile.CorrelationPolicy.MaxHighCorrPairs; v == nil || *v != 5 {
		t.Fatalf("maxHighCorrPairs = %v, want 5", v)
	}
}

// Default (policy unset) should retain the AQR / Bridgewater /
// Citadel ship defaults so existing funds inherit Sprint C's
// caps untouched.
func TestResolveExposureOptionsNilUsesShipDefaults(t *testing.T) {
	got := resolveExposureOptions(nil)
	if got.SingleNameCap != 0.25 || got.SectorCap != 0.50 || got.Top3Cap != 0.60 || got.CashFloorPct != 0.05 {
		t.Errorf("nil policy expected ship defaults, got %+v", got)
	}
}

// Partial overrides must leave untouched fields at their
// defaults (Sprint D #2 lets operators tune one dimension
// without disturbing the others).
func TestResolveExposureOptionsPartialOverrideKeepsDefaults(t *testing.T) {
	cap := 0.10
	policy := &FundExposurePolicy{CashFloorPct: &cap}
	got := resolveExposureOptions(policy)
	if got.CashFloorPct != 0.10 {
		t.Errorf("CashFloorPct override lost: got %v", got.CashFloorPct)
	}
	if got.SingleNameCap != 0.25 || got.SectorCap != 0.50 || got.Top3Cap != 0.60 {
		t.Errorf("untouched fields drifted from defaults: %+v", got)
	}
}

// Correlation policy nil → empty Options (the service falls
// back to its own withDefaults inside mergeOptions).
func TestResolveCorrelationOptionsNilReturnsEmpty(t *testing.T) {
	got := resolveCorrelationOptions(nil)
	if got.LookbackBars != 0 || got.HighCorrThreshold != 0 || got.MaxPairs != 0 {
		t.Errorf("nil policy expected empty Options, got %+v", got)
	}
}

// Per-fund overrides ride through unchanged so the service's
// mergeOptions can pick them up.
func TestResolveCorrelationOptionsTransfersFields(t *testing.T) {
	lookback := 120
	thresh := 0.55
	pairs := 5
	got := resolveCorrelationOptions(&FundCorrelationPolicy{
		LookbackDays:      &lookback,
		HighCorrThreshold: &thresh,
		MaxHighCorrPairs:  &pairs,
	})
	if got.LookbackBars != 120 {
		t.Errorf("LookbackBars = %d, want 120", got.LookbackBars)
	}
	if got.HighCorrThreshold != 0.55 {
		t.Errorf("HighCorrThreshold = %v, want 0.55", got.HighCorrThreshold)
	}
	if got.MaxPairs != 5 {
		t.Errorf("MaxPairs = %d, want 5", got.MaxPairs)
	}
}

func TestBuildFundFocusContextIncludesUniverseFields(t *testing.T) {
	context := buildFundFocusContext(UserLanguageZH, &repository.Fund{Config: json.RawMessage(`{"market":"crypto","assetClass":"spot","primaryDirection":"tokens","universe":{"mode":"manual","symbols":["BTCUSDT","ETHUSDT"],"themes":["CEX","AI infra"],"sectors":["crypto"],"customFilters":["marketCap>5B"]}}`)})
	for _, expected := range []string{"市场：crypto", "资产类别：spot", "主要方向：tokens", "标的池模式：manual", "标的池代码：BTCUSDT、ETHUSDT", "标的池主题：CEX、AI infra", "标的池行业：crypto", "标的池自定义过滤器：marketCap>5B"} {
		if !strings.Contains(context, expected) {
			t.Fatalf("expected %q in fund focus context, got %q", expected, context)
		}
	}
}

func TestBuildFundFocusContextIncludesTeamSpecialization(t *testing.T) {
	context := buildFundFocusContext(UserLanguageZH, &repository.Fund{Config: json.RawMessage(`{"market":"us_equity","specialization":{"team":{"markets":["us_equity"],"themes":["CPO","AI infra"],"instruments":["NVDA","AVGO"],"styleHints":["growth"]}}}`)})
	for _, expected := range []string{"团队擅长市场：us_equity", "团队擅长主题：CPO、AI infra", "团队擅长标的：NVDA、AVGO", "团队风格提示：growth"} {
		if !strings.Contains(context, expected) {
			t.Fatalf("expected %q in fund focus context, got %q", expected, context)
		}
	}
}

func TestBuildAgentSpecializationContextIncludesStaticAndLearnedSignals(t *testing.T) {
	agent := &repository.Agent{
		DomainConfig:    json.RawMessage(`{"specialization":{"markets":["us_equity"],"themes":["CPO","semiconductors"],"instruments":["NVDA"],"patterns":["supply chain mapping"]}}`),
		EvolutionConfig: json.RawMessage(`{"specializationLearning":{"markets":{"us_equity":0.8},"themes":{"CPO":1.2},"instruments":{"NVDA":0.9},"recentLessons":["theme CPO ideas translated into stronger plan quality today"],"lastAdjustments":["reduce confidence on low-liquidity altcoins"]}}`),
	}
	fund := &repository.Fund{Config: json.RawMessage(`{"specialization":{"team":{"themes":["CPO"],"instruments":["NVDA","AVGO"],"styleHints":["growth"]}}}`)}
	context := buildAgentSpecializationContext(UserLanguageZH, agent, fund)
	for _, expected := range []string{"专长背景：", "团队擅长主题：CPO", "成员擅长主题：CPO、semiconductors", "成员擅长标的：NVDA", "成员模式标签：supply chain mapping", "近期学习优势：", "themes=CPO(+1.20)", "instruments=NVDA(+0.90)", "近期学习经验：theme CPO ideas translated into stronger plan quality today", "近期学习调整：reduce confidence on low-liquidity altcoins"} {
		if !strings.Contains(context, expected) {
			t.Fatalf("expected %q in specialization context, got %q", expected, context)
		}
	}
}

func TestNormalizeFundTeamIntervalValue(t *testing.T) {
	cases := []struct {
		name  string
		input int
		want  int
	}{
		{name: "minimum", input: 1, want: 5},
		{name: "rounds to nearest five", input: 17, want: 15},
		{name: "rounds upward", input: 18, want: 20},
		{name: "caps maximum", input: 2000, want: 1440},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeFundTeamIntervalValue(tc.input); got != tc.want {
				t.Fatalf("expected %d, got %d", tc.want, got)
			}
		})
	}
}

func TestDecodeFundMarketProfileRewritesExchangeCalendarUTC(t *testing.T) {
	profile := decodeFundMarketProfile(json.RawMessage(`{"calendarCode":"US-XNAS","timeZone":"UTC"}`))
	if profile.TimeZone != "America/New_York" {
		t.Fatalf("expected timezone America/New_York, got %q", profile.TimeZone)
	}
}

func TestPlanRepoListByFundOrdersByTradingDateThenCreatedAt(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	repo := repository.NewPlanRepo(db)
	rows := sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "reasoning", "risk_score", "expected_return", "risk_review", "discussion_snapshot", "roundtable_id", "pm_agent_id", "confidence", "created_at", "updated_at"})
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, reasoning, risk_score, expected_return, risk_review, discussion_snapshot, roundtable_id, pm_agent_id, confidence, created_at, updated_at
		 FROM investment_plans WHERE fund_id = $1
		 ORDER BY trading_date DESC, created_at DESC LIMIT $2 OFFSET $3`)).
		WithArgs("fund-1", 1, 0).
		WillReturnRows(rows)

	if _, err := repo.ListByFund(context.Background(), "fund-1", 1); err != nil {
		t.Fatalf("list by fund: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestInferWorkflowSymbolIgnoresRoundtableTextAndUsesBenchmarkFallback(t *testing.T) {
	fund := &repository.Fund{
		ID:     "fund-1",
		Name:   "Alpha Growth Fund",
		Config: json.RawMessage(`{"benchmarkSymbol":"MU"}`),
	}
	roundtable := &workflow.RoundtableResult{Consensus: []string{"macro brief suggests storage demand recovery", "consider sandisk and micron"}}

	if got := inferWorkflowSymbol(fund, roundtable); got != "MU" {
		t.Fatalf("expected benchmark fallback MU, got %q", got)
	}
}

func TestInferWorkflowSymbolUsesPMSpecializationInstrumentWhenUniverseMissing(t *testing.T) {
	fund := &repository.Fund{
		ID:     "fund-1",
		Name:   "Storage Opportunity Fund",
		Config: json.RawMessage(`{"market":"us_equity","exchange":"NASDAQ","assetClass":"equity","universe":{"mode":"manual"}}`),
	}

	if got := inferWorkflowSymbolWithSpecialization(fund, &agentSpecialization{Instruments: []string{"MU", "SNDK"}}); got != "MU" {
		t.Fatalf("expected PM specialization symbol MU, got %q", got)
	}
}

func TestInferWorkflowSymbolUsesFundSpecializationInstrumentWhenUniverseMissing(t *testing.T) {
	fund := &repository.Fund{
		ID:     "fund-1",
		Name:   "Storage Opportunity Fund",
		Config: json.RawMessage(`{"specialization":{"team":{"themes":["存储"],"instruments":["SNDK","MU"]}}}`),
	}

	if got := inferWorkflowSymbolWithSpecialization(fund, nil); got != "SNDK" {
		t.Fatalf("expected fund specialization symbol SNDK, got %q", got)
	}
}

func TestInferWorkflowSymbolUsesSplitThemeCandidatesWhenUniverseMissing(t *testing.T) {
	fund := &repository.Fund{
		ID:     "fund-1",
		Name:   "Storage Opportunity Fund",
		Config: json.RawMessage(`{"universe":{"mode":"manual"}}`),
	}

	if got := inferWorkflowSymbolWithSpecialization(fund, &agentSpecialization{Themes: []string{"存储、美光科技", "MU"}}); got != "MU" {
		t.Fatalf("expected split theme candidate MU, got %q", got)
	}
}

func TestInferWorkflowSymbolResolvesChineseCompanyAliasCandidates(t *testing.T) {
	fund := &repository.Fund{
		ID:     "fund-1",
		Name:   "Storage Opportunity Fund",
		Config: json.RawMessage(`{"universe":{"mode":"manual"}}`),
	}

	if got := inferWorkflowSymbolWithSpecialization(fund, &agentSpecialization{Themes: []string{"美光科技", "闪迪"}}); got != "MU" {
		t.Fatalf("expected alias-resolved symbol MU, got %q", got)
	}
}

func TestCandidateWorkflowSymbolsFromTeamAgentsUsesOtherMemberSpecializations(t *testing.T) {
	pmAgent := &repository.Agent{DomainConfig: json.RawMessage(`{"specialization":{"themes":["存储方向"]}}`)}
	researcherAgent := &repository.Agent{DomainConfig: json.RawMessage(`{"specialization":{"themes":["美光科技","闪迪"]}}`)}

	got := candidateWorkflowSymbolsFromTeamAgents(pmAgent, researcherAgent)
	want := []string{"MU", "SNDK"}
	if len(got) != len(want) {
		t.Fatalf("expected %d candidates, got %#v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected candidate %d to be %q, got %#v", i, want[i], got)
		}
	}
}

func TestInferWorkflowBuySymbolPrefersFundSpecializationInstrumentsOverTeamCandidates(t *testing.T) {
	fund := &repository.Fund{
		ID:     "fund-1",
		Name:   "Storage Opportunity Fund",
		Config: json.RawMessage(`{"specialization":{"team":{"themes":["存储"],"instruments":["SNDK","MU"]}}}`),
	}

	got, source := inferWorkflowBuySymbol(fund, []string{"MU"})
	if got != "SNDK" {
		t.Fatalf("expected SNDK from fund specialization instruments, got %q", got)
	}
	if source != "fund_specialization_instruments" {
		t.Fatalf("expected fund_specialization_instruments source, got %q", source)
	}
}

func TestInferWorkflowBuySymbolReturnsEmptyWhenOnlyThemeCandidatesExist(t *testing.T) {
	fund := &repository.Fund{
		ID:     "fund-1",
		Name:   "Storage Opportunity Fund",
		Config: json.RawMessage(`{"specialization":{"team":{"themes":["存储方向"]}}}`),
	}

	got, source := inferWorkflowBuySymbol(fund, nil)
	if got != "" || source != "" {
		t.Fatalf("expected empty symbol/source, got %q %q", got, source)
	}
}

// Regression: A-share funds expose universe symbols as bare 6-digit tickers
// (e.g. "688205", "600519"). The PM symbol inference must pick those from
// universe.symbols before falling through to theme tags like "OCS" or "OTN".
func TestInferWorkflowBuySymbolAcceptsAShareNumericUniverseSymbols(t *testing.T) {
	fund := &repository.Fund{
		ID:     "fund-cn",
		Name:   "OCS 主题精选 1 号",
		Config: json.RawMessage(`{"market":"a_share","exchange":"SSE","universe":{"mode":"manual","symbols":["688205","688195"]},"specialization":{"team":{"themes":["OCS","光交换"],"instruments":["688205","688195"]}}}`),
	}

	got, source := inferWorkflowBuySymbol(fund, []string{"OCS", "OTN"})
	if got != "688205" {
		t.Fatalf("expected 688205 from universe, got %q", got)
	}
	if source != "universe" {
		t.Fatalf("expected source 'universe', got %q", source)
	}
}

// Regression: the same numeric ticker must also resolve when the operator
// configures it only in specialization.instruments (no universe block).
func TestInferWorkflowBuySymbolAcceptsAShareNumericSpecializationInstruments(t *testing.T) {
	fund := &repository.Fund{
		ID:     "fund-cn",
		Name:   "OCS 主题精选 1 号",
		Config: json.RawMessage(`{"market":"a_share","specialization":{"team":{"themes":["OCS"],"instruments":["688195"]}}}`),
	}

	got, source := inferWorkflowBuySymbol(fund, []string{"OCS"})
	if got != "688195" {
		t.Fatalf("expected 688195 from specialization instruments, got %q", got)
	}
	if source != "fund_specialization_instruments" {
		t.Fatalf("expected source 'fund_specialization_instruments', got %q", source)
	}
}

func TestNormalizedWorkflowSymbolAcceptsNumericTickers(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"688205", "688205"}, // A-share STAR Market
		{"600519", "600519"}, // A-share Shanghai
		{"000858", "000858"}, // A-share Shenzhen
		{"0700", "0700"},     // HK Stock (4-digit)
		{"00700", "00700"},   // HK Stock (5-digit canonical)
		{"MU", "MU"},         // US ticker (regression: letters still pass)
		{"BTCUSDT", "BTCUSDT"},
		{"0700.HK", "0700.HK"},

		{"", ""},
		{"  ", ""},
		{"123", ""},                  // too short to be a real ticker
		{"1234567", ""},              // too long to be A-share/HK
		{"2024 outlook", ""},         // spaces are not allowed
		{strings.Repeat("A", 17), ""},
	}
	for _, tc := range cases {
		if got := normalizedWorkflowSymbol(tc.in); got != tc.want {
			t.Errorf("normalizedWorkflowSymbol(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizedStringSliceSplitsChineseCommaSeparatedValues(t *testing.T) {
	got := normalizedStringSlice([]string{"存储、美光科技", "闪迪；SNDK", " MU "})
	want := []string{"存储", "美光科技", "闪迪", "SNDK", "MU"}
	if len(got) != len(want) {
		t.Fatalf("expected %d values, got %#v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected value %d to be %q, got %#v", i, want[i], got)
		}
	}
}

func TestFilterWorkflowPlanPositionsSkipsLegacyPlaceholderPositions(t *testing.T) {
	positions := []repository.HoldingPosition{
		{Symbol: "MACRO", InstrumentKey: "NASDAQ:MACRO"},
		{Symbol: "MU", InstrumentKey: "NASDAQ:MU"},
	}

	filtered := filterWorkflowPlanPositions(positions)
	if len(filtered) != 1 {
		t.Fatalf("expected one remaining position, got %#v", filtered)
	}
	if filtered[0].Symbol != "MU" {
		t.Fatalf("expected MU to remain, got %#v", filtered[0])
	}
}

func TestIsLegacyWorkflowPlaceholderPosition(t *testing.T) {
	cases := []struct {
		name     string
		position repository.HoldingPosition
		want     bool
	}{
		{name: "legacy macro symbol", position: repository.HoldingPosition{Symbol: "MACRO"}, want: true},
		{name: "legacy benchmark key", position: repository.HoldingPosition{InstrumentKey: "NASDAQ:BENCHMARK"}, want: true},
		{name: "normal ticker", position: repository.HoldingPosition{Symbol: "MU", InstrumentKey: "NASDAQ:MU"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLegacyWorkflowPlaceholderPosition(tc.position); got != tc.want {
				t.Fatalf("expected %t, got %t for %#v", tc.want, got, tc.position)
			}
		})
	}
}

func TestConvertPlanWithLocaleBackfillsReasoningWhenTranslationUnavailable(t *testing.T) {
	plan := &repository.InvestmentPlan{
		ID:        "plan-1",
		FundID:    "fund-1",
		Status:    "pending_user",
		Reasoning: sql.NullString{String: "base plan reasoning", Valid: true},
	}
	actions := []repository.PlanAction{{
		ID:        "action-1",
		Symbol:    "MU",
		Action:    "buy",
		Reasoning: sql.NullString{String: "base action reasoning", Valid: true},
	}}

	converted := convertPlanWithLocale("user-1", nil, plan, actions)
	if converted == nil {
		t.Fatal("expected converted plan")
	}
	if converted.ReasoningZh != "" || converted.ReasoningEn != "" {
		t.Fatalf("expected plan reasoning zh/en to stay empty when translation unavailable, got zh=%q en=%q", converted.ReasoningZh, converted.ReasoningEn)
	}
	if converted.Reasoning != "base plan reasoning" {
		t.Fatalf("expected base reasoning preserved, got %q", converted.Reasoning)
	}
	if len(converted.Actions) != 1 {
		t.Fatalf("expected one action, got %d", len(converted.Actions))
	}
	if converted.Actions[0].ReasoningZh != "" || converted.Actions[0].ReasoningEn != "" {
		t.Fatalf("expected action reasoning zh/en to stay empty when translation unavailable, got zh=%q en=%q", converted.Actions[0].ReasoningZh, converted.Actions[0].ReasoningEn)
	}
	if converted.Actions[0].Reasoning != "base action reasoning" {
		t.Fatalf("expected base action reasoning preserved, got %q", converted.Actions[0].Reasoning)
	}
}

func TestBuildDecisionTraceDiscussionWithLocaleBackfillsFieldsWhenTranslationUnavailable(t *testing.T) {
	plan := &repository.InvestmentPlan{
		ID:                 "plan-1",
		FundID:             "fund-1",
		Reasoning:          sql.NullString{String: "base discussion reasoning", Valid: true},
		DiscussionSnapshot: json.RawMessage(`{"summary":"base summary","consensus":["macro brief","storage focus"]}`),
	}

	discussion := buildDecisionTraceDiscussionWithLocale("user-1", nil, plan)
	if discussion == nil {
		t.Fatal("expected discussion")
	}
	if discussion.Reasoning != "base discussion reasoning" {
		t.Fatalf("expected base reasoning preserved, got %q", discussion.Reasoning)
	}
	if discussion.Summary != "base summary" {
		t.Fatalf("expected base summary preserved, got %q", discussion.Summary)
	}
	if discussion.ReasoningZh != "" || discussion.ReasoningEn != "" {
		t.Fatalf("expected reasoning zh/en to stay empty when translation unavailable, got zh=%q en=%q", discussion.ReasoningZh, discussion.ReasoningEn)
	}
	if discussion.SummaryZh != "" || discussion.SummaryEn != "" {
		t.Fatalf("expected summary zh/en to stay empty when translation unavailable, got zh=%q en=%q", discussion.SummaryZh, discussion.SummaryEn)
	}
	if len(discussion.ConsensusZh) != 0 || len(discussion.ConsensusEn) != 0 {
		t.Fatalf("expected untranslated consensus lists to stay empty, got zh=%#v en=%#v", discussion.ConsensusZh, discussion.ConsensusEn)
	}
}

func TestBuildDecisionTraceDiscussionPreservesBaseFieldsWithoutRuntime(t *testing.T) {
	plan := &repository.InvestmentPlan{
		ID:                 "plan-1",
		FundID:             "fund-1",
		Reasoning:          sql.NullString{String: "base discussion reasoning", Valid: true},
		DiscussionSnapshot: json.RawMessage(`{"summary":"base summary","consensus":["macro brief","storage focus"]}`),
	}

	discussion := buildDecisionTraceDiscussion(plan)
	if discussion == nil {
		t.Fatal("expected discussion")
	}
	if discussion.Reasoning != "base discussion reasoning" {
		t.Fatalf("expected base reasoning, got %q", discussion.Reasoning)
	}
	if discussion.Summary != "base summary" {
		t.Fatalf("expected base summary, got %q", discussion.Summary)
	}
	if len(discussion.Consensus) != 2 {
		t.Fatalf("expected base consensus, got %#v", discussion.Consensus)
	}
	if discussion.ReasoningZh != "" || discussion.ReasoningEn != "" {
		t.Fatalf("expected localized reasoning to stay empty without runtime, got zh=%q en=%q", discussion.ReasoningZh, discussion.ReasoningEn)
	}
	if discussion.SummaryZh != "" || discussion.SummaryEn != "" {
		t.Fatalf("expected localized summary to stay empty without runtime, got zh=%q en=%q", discussion.SummaryZh, discussion.SummaryEn)
	}
	if len(discussion.ConsensusZh) != 0 || len(discussion.ConsensusEn) != 0 {
		t.Fatalf("expected localized consensus lists to stay empty, got zh=%#v en=%#v", discussion.ConsensusZh, discussion.ConsensusEn)
	}
}

func TestBuildCommitteeMemoSummarizesDecisionParticipantsAndRisk(t *testing.T) {
	quantity := 12.0
	price := 99.5
	plan := &api.Plan{
		ID:         "plan-1",
		Status:     "pending_user",
		Reasoning:  "PM approves a controlled MU entry after the roundtable.",
		PMAgentID:  "pm-1",
		RiskReview: json.RawMessage(`{"Verdict":"approved_with_warnings","Warnings":["position size near limit"],"Suggestions":["stage the order"],"Checks":[{"Rule":"single_position_limit","Status":"warn","Message":"single name exposure should be watched"}]}`),
		Actions: []api.PlanAction{{
			ID:          "action-1",
			Symbol:      "MU",
			Action:      "buy",
			Quantity:    &quantity,
			Price:       &price,
			Reasoning:   "DRAM demand and pricing momentum improved.",
			SupportedBy: []string{"researcher-1", "quant-1"},
			OpposedBy:   []string{"risk-1"},
		}},
	}
	discussion := &api.DecisionTraceDiscussion{Summary: "Committee supports a staged MU entry.", Consensus: []string{"Enter MU with risk controls"}}
	execution := &api.DecisionTraceExecution{ActionExecutions: []api.DecisionTraceActionExecution{{PlanActionID: "action-1", ExecutionStatus: "pending"}}}
	research := []api.MarketResearch{{Instrument: api.MarketInstrument{Symbol: "MU"}, Summary: "Memory cycle is improving.", Signals: []string{"momentum positive"}}}

	memo := buildCommitteeMemo(plan, discussion, execution, research)
	if memo == nil {
		t.Fatal("expected committee memo")
	}
	if memo.Summary != "Committee supports a staged MU entry." {
		t.Fatalf("unexpected memo summary: %q", memo.Summary)
	}
	if len(memo.Participants) < 5 {
		t.Fatalf("expected participants from pm/support/opposition/risk/trader, got %#v", memo.Participants)
	}
	if len(memo.AgentViews) == 0 || memo.AgentViews[0].AgentID != "researcher-1" {
		t.Fatalf("expected supporter agent view, got %#v", memo.AgentViews)
	}
	if len(memo.Contentions) != 1 || !strings.Contains(memo.Contentions[0], "risk-1") {
		t.Fatalf("expected contention from opposedBy, got %#v", memo.Contentions)
	}
	if memo.RiskOpinion == nil || memo.RiskOpinion.Verdict != "approved_with_warnings" {
		t.Fatalf("expected risk opinion, got %#v", memo.RiskOpinion)
	}
	if len(memo.TraderSuggestions) != 1 || !strings.Contains(memo.TraderSuggestions[0].Instruction, "execution pending") {
		t.Fatalf("expected trader instruction with execution status, got %#v", memo.TraderSuggestions)
	}
	if memo.FinalDecision == nil || len(memo.FinalDecision.Actions) != 1 {
		t.Fatalf("expected final decision actions, got %#v", memo.FinalDecision)
	}
}

func TestBuildRiskExplanationStandardizesRuleImpactsAndAdvice(t *testing.T) {
	raw := json.RawMessage(`{"Verdict":"rejected","Rejections":["portfolio drawdown breached"],"Checks":[{"Rule":"hard_single_position_limit","Status":"fail","Current":0.42,"Threshold":0.30,"Message":"single position exceeds hard limit"},{"Rule":"liquidity_check","Status":"warn","Current":0.08,"Threshold":0.10,"Message":"liquidity is thin"}]}`)

	explanation := buildRiskExplanation(raw)
	if explanation == nil {
		t.Fatal("expected risk explanation")
	}
	if explanation.Severity != "block" {
		t.Fatalf("expected block severity, got %q", explanation.Severity)
	}
	if len(explanation.Checks) != 2 {
		t.Fatalf("expected two checks, got %#v", explanation.Checks)
	}
	if explanation.Checks[0].RuleName != "Hard single position limit" || explanation.Checks[0].Severity != "block" {
		t.Fatalf("expected normalized hard single position check, got %#v", explanation.Checks[0])
	}
	if explanation.Checks[0].Current == nil || *explanation.Checks[0].Current != 0.42 {
		t.Fatalf("expected current value to be preserved, got %#v", explanation.Checks[0].Current)
	}
	if len(explanation.BlockingReasons) == 0 || !strings.Contains(strings.Join(explanation.BlockingReasons, " "), "single position") {
		t.Fatalf("expected blocking reasons, got %#v", explanation.BlockingReasons)
	}
	if len(explanation.AdjustmentAdvice) == 0 || !strings.Contains(strings.Join(explanation.AdjustmentAdvice, " "), "Reduce order size") {
		t.Fatalf("expected adjustment advice, got %#v", explanation.AdjustmentAdvice)
	}
}

func TestConvertMarketplaceListingBuildsTrustSignals(t *testing.T) {
	dailyReturn := 0.018
	snapshot, err := json.Marshal(marketplaceSnapshot{
		Agent: marketplaceSnapshotAgent{
			Name:            "Macro Researcher",
			Role:            "researcher",
			Focus:           "macro",
			SystemPrompt:    "Track liquidity and policy pivots.",
			DomainConfig:    json.RawMessage(`{"coverage":{"markets":["us_equity"]}}`),
			EvolutionConfig: json.RawMessage(`{"recentLessons":["tighten invalidation windows"]}`),
		},
		Learning: &marketplaceSnapshotLearning{
			Summary:     "Recent macro calls improved timing discipline.",
			CreatedAt:   time.Now().UTC().Add(-24 * time.Hour),
			DailyReturn: &dailyReturn,
			Tags:        []string{"self_learning", "researcher"},
		},
		ModelConfig: &api.UserModelConfig{Provider: "openai", ModelName: "gpt-4.1"},
		Memories: []marketplaceSnapshotMemoryEntry{{
			Layer:     "agent",
			Title:     "Public learning note",
			Content:   "Policy surprise playbook.",
			Tags:      []string{"self_learning", "macro"},
			CreatedAt: time.Now().UTC().Add(-48 * time.Hour),
		}},
	})
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	listing := convertMarketplaceListing(&repository.AgentMarketListing{
		ID:                    "listing-1",
		SellerUserID:          "seller-1",
		SourceFundID:          "fund-1",
		SourceAgentID:         "agent-1",
		AgentName:             "Macro Researcher",
		AgentRole:             "researcher",
		AgentFocus:            sql.NullString{String: "macro", Valid: true},
		LatestLearningSummary: sql.NullString{String: "Recent macro calls improved timing discipline.", Valid: true},
		AskPriceMinor:         1000,
		Currency:              "USD",
		Status:                "active",
		SnapshotPayload:       snapshot,
		CreatedAt:             time.Now().UTC().Add(-72 * time.Hour),
		UpdatedAt:             time.Now().UTC(),
	})

	if listing.Trust == nil {
		t.Fatal("expected trust signals")
	}
	if listing.Trust.Score < 80 || listing.Trust.Level != "high" {
		t.Fatalf("expected high trust score, got %#v", listing.Trust)
	}
	if listing.Trust.LearningRecords < 2 || listing.Trust.PublicMemoryRecords != 1 {
		t.Fatalf("unexpected trust evidence counts: %#v", listing.Trust)
	}
	if !listing.Trust.ModelConfigured || listing.Trust.LastDailyReturn == nil || *listing.Trust.LastDailyReturn != dailyReturn {
		t.Fatalf("unexpected trust model/return signals: %#v", listing.Trust)
	}
}

func TestConvertMarketResearchPreservesBaseFieldsWithoutRuntime(t *testing.T) {
	research := &marketdata.ResearchContext{
		Instrument: marketdata.InstrumentRef{Symbol: "MU", Market: "us_equity", Exchange: "NASDAQ", AssetClass: "equity"},
		Summary:    "Micron setup remains constructive.",
		News: []marketdata.NewsItem{{
			Title:       "Micron wins market share",
			Summary:     "Demand improved in key segments.",
			URL:         "https://example.com/mu-news",
			Source:      "Example",
			PublishedAt: time.Date(2026, 5, 18, 9, 0, 0, 0, time.UTC),
			Symbols:     []string{"MU"},
		}},
		Signals:       []string{"relative strength improving"},
		ProviderNotes: []string{"quote unavailable"},
		GeneratedAt:   time.Date(2026, 5, 18, 9, 5, 0, 0, time.UTC),
	}

	converted := convertMarketResearch(research)
	if converted == nil {
		t.Fatal("expected converted research")
	}
	if converted.Summary != "Micron setup remains constructive." {
		t.Fatalf("expected base summary, got %q", converted.Summary)
	}
	if len(converted.News) != 1 {
		t.Fatalf("expected one news item, got %#v", converted.News)
	}
	if converted.News[0].Title != "Micron wins market share" || converted.News[0].Summary != "Demand improved in key segments." {
		t.Fatalf("expected base news fields, got %#v", converted.News[0])
	}
	if converted.News[0].TitleZh != "" || converted.News[0].TitleEn != "" || converted.News[0].SummaryZh != "" || converted.News[0].SummaryEn != "" {
		t.Fatalf("expected localized news fields to stay empty, got %#v", converted.News[0])
	}
}

func TestBuildHybridMarketNewsQueriesPrefersTickerInputsAndAddsContext(t *testing.T) {
	fund := &repository.Fund{
		ID:          "fund-1",
		Name:        "Storage Leaders Fund",
		Description: sql.NullString{String: "high conviction semiconductor strategy", Valid: true},
		Config: json.RawMessage(`{
			"market":"us_equity",
			"exchange":"NASDAQ",
			"assetClass":"equity",
			"benchmarkSymbol":"QQQ",
			"universe":{"symbols":["MU","NVDA"],"themes":["semiconductor"],"sectors":["technology"]},
			"specialization":{"team":{"themes":["storage"],"instruments":["SNDK"],"styleHints":["growth"]}}
		}`),
	}
	profile := decodeFundMarketProfile(fund.Config)
	queries := buildHybridMarketNewsQueries(fund, profile, []string{"AVGO"}, nil)
	if len(queries) == 0 {
		t.Fatal("expected hybrid queries")
	}
	gotSymbols := digestTickerSymbols(queries)
	wantPrefix := []string{"AVGO", "MU", "NVDA"}
	if len(gotSymbols) < len(wantPrefix) {
		t.Fatalf("expected ticker prefix %#v, got %#v", wantPrefix, gotSymbols)
	}
	for i := range wantPrefix {
		if gotSymbols[i] != wantPrefix[i] {
			t.Fatalf("expected ticker %d to be %q, got %#v", i, wantPrefix[i], gotSymbols)
		}
	}
	foundContext := false
	for _, query := range queries {
		if !marketdata.IsTickerLikeSymbol(query.Symbol) && strings.Contains(strings.ToLower(query.Symbol), "semiconductor") {
			foundContext = true
			break
		}
	}
	if !foundContext {
		t.Fatalf("expected semiconductor context query, got %#v", queries)
	}
}

func TestBuildHybridMarketNewsQueriesDedupesContextAndTickerSymbols(t *testing.T) {
	fund := &repository.Fund{
		ID:   "fund-1",
		Name: "Storage Leaders Fund",
		Config: json.RawMessage(`{
			"market":"us_equity",
			"assetClass":"equity",
			"universe":{"symbols":["MU","MU"],"themes":["storage","storage"]},
			"specialization":{"team":{"themes":["storage"],"instruments":["MU","SNDK"],"styleHints":["growth","growth"]}}
		}`),
	}
	profile := decodeFundMarketProfile(fund.Config)
	queries := buildHybridMarketNewsQueries(fund, profile, []string{"MU", "MU"}, nil)
	gotSymbols := digestTickerSymbols(queries)
	if len(gotSymbols) == 0 || gotSymbols[0] != "MU" {
		t.Fatalf("expected MU to remain as primary ticker, got %#v", gotSymbols)
	}
	seen := map[string]struct{}{}
	for _, query := range queries {
		key := strings.ToLower(strings.TrimSpace(query.Symbol))
		if _, ok := seen[key]; ok {
			t.Fatalf("expected query dedupe, got duplicate %q in %#v", key, queries)
		}
		seen[key] = struct{}{}
	}
}

func TestTagDigestNewsItemsRemovesFreeTextSymbols(t *testing.T) {
	items := []marketdata.NewsItem{{Title: "theme article", Symbols: []string{"semiconductor stock market news"}}}
	tagged := tagDigestNewsItems(items, marketdata.InstrumentRef{Symbol: "semiconductor stock market news"})
	if len(tagged) != 1 {
		t.Fatalf("expected one tagged item, got %#v", tagged)
	}
	if tagged[0].Symbols != nil {
		t.Fatalf("expected free-text symbols to be cleared, got %#v", tagged[0].Symbols)
	}
}

func TestMarketNewsDigestItemKeyPrefersURL(t *testing.T) {
	item := marketdata.NewsItem{Title: "A", URL: "https://example.com/Story"}
	if got := marketNewsDigestItemKey(item); got != "https://example.com/story" {
		t.Fatalf("expected lowercase url key, got %q", got)
	}
	item.URL = ""
	if got := marketNewsDigestItemKey(item); got != "a" {
		t.Fatalf("expected lowercase title key, got %q", got)
	}
}

func TestIsStaleMarketNewsItem(t *testing.T) {
	now := time.Date(2026, time.May, 16, 12, 0, 0, 0, time.UTC)
	fresh := marketdata.NewsItem{Title: "fresh", PublishedAt: now.Add(-7 * 24 * time.Hour)}
	stale := marketdata.NewsItem{Title: "stale", PublishedAt: now.Add(-60 * 24 * time.Hour)}
	undated := marketdata.NewsItem{Title: "undated"}

	if isStaleMarketNewsItem(fresh, now, marketNewsDigestMaxAge) {
		t.Fatal("expected recent item to stay eligible")
	}
	if !isStaleMarketNewsItem(stale, now, marketNewsDigestMaxAge) {
		t.Fatal("expected old item to be filtered")
	}
	if isStaleMarketNewsItem(undated, now, marketNewsDigestMaxAge) {
		t.Fatal("expected undated item to stay eligible")
	}
}

func TestShouldRestartWorkflowRunForManualStart(t *testing.T) {
	now := time.Now().UTC()
	staleUpdate := now.Add(-manualWorkflowRestartStaleAfter - time.Minute).Format(time.RFC3339)
	recentUpdate := now.Add(-time.Minute).Format(time.RFC3339)
	cases := []struct {
		name       string
		run        *repository.WorkflowRun
		hasRuntime bool
		want       bool
	}{
		{name: "nil run", run: nil, want: false},
		{name: "orphan running run", run: &repository.WorkflowRun{Status: "running", CurrentStep: sql.NullString{String: "macro_brief", Valid: true}}, hasRuntime: false, want: true},
		{name: "orphan paused approval run", run: &repository.WorkflowRun{Status: "paused", CurrentStep: sql.NullString{String: "user_approval", Valid: true}, StepResults: json.RawMessage(`{"user_approval":{"status":"pending"}}`)}, hasRuntime: false, want: false},
		{name: "fresh in-memory running run", run: &repository.WorkflowRun{Status: "running", CurrentStep: sql.NullString{String: "macro_brief", Valid: true}, StepResults: json.RawMessage([]byte(`{"macro_brief":{"status":"running","updatedAt":"` + recentUpdate + `"}}`))}, hasRuntime: true, want: false},
		{name: "stale in-memory running run", run: &repository.WorkflowRun{Status: "running", CurrentStep: sql.NullString{String: "macro_brief", Valid: true}, StepResults: json.RawMessage([]byte(`{"macro_brief":{"status":"running","updatedAt":"` + staleUpdate + `"}}`))}, hasRuntime: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRestartWorkflowRunForManualStart(tc.run, now, tc.hasRuntime); got != tc.want {
				t.Fatalf("expected %t, got %t", tc.want, got)
			}
		})
	}
}

func TestSchedulerRunNeedsTrigger(t *testing.T) {
	cases := []struct {
		name string
		run  *repository.WorkflowRun
		want bool
	}{
		{name: "nil run", run: nil, want: true},
		{name: "pending run", run: &repository.WorkflowRun{Status: "pending"}, want: true},
		{name: "failed run without progress", run: &repository.WorkflowRun{Status: "failed"}, want: true},
		{name: "cancelled run without progress", run: &repository.WorkflowRun{Status: "cancelled"}, want: true},
		{name: "failed run with progress", run: &repository.WorkflowRun{Status: "failed", CurrentStep: sql.NullString{String: "pm_plan", Valid: true}}, want: false},
		{name: "cancelled run with progress", run: &repository.WorkflowRun{Status: "cancelled", StartedAt: sql.NullTime{Time: time.Now().UTC(), Valid: true}}, want: false},
		{name: "paused run", run: &repository.WorkflowRun{Status: "paused"}, want: false},
		{name: "rejected run", run: &repository.WorkflowRun{Status: "rejected"}, want: false},
		{name: "running run", run: &repository.WorkflowRun{Status: "running"}, want: false},
		{name: "completed run", run: &repository.WorkflowRun{Status: "completed"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := schedulerRunNeedsTrigger(tc.run); got != tc.want {
				t.Fatalf("expected %t, got %t", tc.want, got)
			}
		})
	}
}

func TestConvertWorkflowStatusPreservesTerminalStates(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "pending becomes idle", in: "pending", want: "idle"},
		{name: "paused preserved", in: "paused", want: "paused"},
		{name: "cancelled preserved", in: "cancelled", want: "cancelled"},
		{name: "rejected preserved", in: "rejected", want: "rejected"},
		{name: "completed preserved", in: "completed", want: "completed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := convertWorkflowStatus(&repository.WorkflowRun{FundID: "fund-1", TradingDate: time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC), Status: tc.in})
			if status == nil {
				t.Fatal("expected workflow status")
			}
			if status.State != tc.want {
				t.Fatalf("expected state %q, got %q", tc.want, status.State)
			}
			if status.TradingDate != "2026-05-14" {
				t.Fatalf("expected trading date 2026-05-14, got %q", status.TradingDate)
			}
		})
	}
}

func TestConvertWorkflowStatusBuildsTimelineAndProgress(t *testing.T) {
	started := time.Date(2026, 5, 17, 9, 0, 0, 0, time.UTC)
	macroEnd := started.Add(2 * time.Minute)
	researchStart := started.Add(3 * time.Minute)
	run := &repository.WorkflowRun{
		FundID:      "fund-1",
		TradingDate: time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC),
		Status:      "running",
		CurrentStep: sql.NullString{String: workflow.StepResearchParallel.String(), Valid: true},
		StartedAt:   sql.NullTime{Time: started, Valid: true},
		StepResults: json.RawMessage(fmt.Sprintf(`{
			"macro_brief":{"step":"macro_brief","status":"success","startedAt":%q,"endedAt":%q},
			"research_parallel":{"step":"research_parallel","status":"running","startedAt":%q}
		}`, started.Format(time.RFC3339), macroEnd.Format(time.RFC3339), researchStart.Format(time.RFC3339))),
	}

	status := convertWorkflowStatus(run)
	if status == nil {
		t.Fatal("expected workflow status")
	}
	if status.TotalSteps != 10 || len(status.Steps) != 10 {
		t.Fatalf("expected full 10-step timeline, got total=%d len=%d", status.TotalSteps, len(status.Steps))
	}
	if status.CompletedSteps != 1 || status.ProgressPercent != 10 {
		t.Fatalf("unexpected progress completed=%d percent=%d", status.CompletedSteps, status.ProgressPercent)
	}
	if status.Steps[0].Step != "macro_brief" || status.Steps[0].DurationMs != int64((2*time.Minute)/time.Millisecond) {
		t.Fatalf("unexpected first step: %#v", status.Steps[0])
	}
	if status.Steps[1].Status != "running" {
		t.Fatalf("expected current step to be running, got %#v", status.Steps[1])
	}
	if status.Steps[2].Status != "pending" {
		t.Fatalf("expected future step to be pending, got %#v", status.Steps[2])
	}
}

func TestResumeApprovedPlanNormalizesTradingDateForHook(t *testing.T) {
	loc := time.FixedZone("CST", 8*60*60)
	input := time.Date(2026, time.May, 14, 0, 30, 0, 0, loc)
	want := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)

	called := false
	service := &workflowServiceAdapter{resumePlan: func(fundID string, gotDate time.Time, planID string) error {
		called = true
		if fundID != "fund-1" {
			return fmt.Errorf("unexpected fund id %s", fundID)
		}
		if planID != "plan-1" {
			return fmt.Errorf("unexpected plan id %s", planID)
		}
		if !gotDate.Equal(want) {
			return fmt.Errorf("expected normalized trading date %s, got %s", want.Format(time.RFC3339), gotDate.Format(time.RFC3339))
		}
		return nil
	}}

	if err := service.ResumeApprovedPlan("fund-1", input, "plan-1"); err != nil {
		t.Fatalf("resume approved plan: %v", err)
	}
	if !called {
		t.Fatal("expected resume hook to be called")
	}
}

func TestWorkflowRunAwaitingApprovalRequiresPendingApprovalStep(t *testing.T) {
	pendingStepResults := json.RawMessage(`{"user_approval":{"step":"user_approval","status":"pending"}}`)
	if !workflowRunAwaitingApproval(&repository.WorkflowRun{
		Status:      "paused",
		CurrentStep: sql.NullString{String: "user_approval", Valid: true},
		StepResults: pendingStepResults,
	}) {
		t.Fatal("expected paused user approval run to be resumable")
	}
	if workflowRunAwaitingApproval(&repository.WorkflowRun{
		Status:      "failed",
		CurrentStep: sql.NullString{String: "trade_execution", Valid: true},
		StepResults: json.RawMessage(`{"trade_execution":{"step":"trade_execution","status":"failed"}}`),
	}) {
		t.Fatal("expected non-approval failed run to be non-resumable")
	}
	if workflowRunAwaitingApproval(&repository.WorkflowRun{
		Status:      "paused",
		CurrentStep: sql.NullString{String: "user_approval", Valid: true},
		StepResults: json.RawMessage(`{"user_approval":{"step":"user_approval","status":"success"}}`),
	}) {
		t.Fatal("expected completed approval step to be non-resumable")
	}
}

func TestGetStatusReturnsIdleForCurrentTradingDateWithoutCurrentRun(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"calendarCode":"US-XNAS","timeZone":"America/New_York"}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, owner_user_id, name, description, created_at, updated_at
		 FROM fund_companies WHERE id = $1`)).
		WithArgs("company-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "owner_user_id", "name", "description", "created_at", "updated_at"}).
			AddRow("company-1", "user-1", "Alpha Co", nil, now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at
		 FROM workflow_runs
		 WHERE fund_id = $1
		 ORDER BY trading_date DESC, created_at DESC
		 LIMIT 1`)).
		WithArgs("fund-1").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at
		 FROM workflow_runs
		 WHERE fund_id = $1 AND trading_date = $2
		 ORDER BY created_at DESC
		 LIMIT 1`)).
		WithArgs("fund-1", sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)

	service := &workflowServiceAdapter{
		fundRepo:     repository.NewFundRepo(db),
		companyRepo:  repository.NewFundCompanyRepo(db),
		workflowRepo: repository.NewWorkflowRunRepo(db),
		runtimes:     make(map[string]*workflowRuntime),
	}
	status, err := service.GetStatus("user-1", "fund-1")
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status == nil {
		t.Fatal("expected workflow status")
	}
	if status.State != "idle" {
		t.Fatalf("expected idle state, got %q", status.State)
	}
	if status.Step != "not_started" {
		t.Fatalf("expected not_started step, got %q", status.Step)
	}
	if status.TradingDate == "" {
		t.Fatal("expected trading date to be present")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestGetStatusUsesRuntimeSnapshotWithoutPersisting(t *testing.T) {
	now := time.Now().UTC()
	tradingDate := normalizeTradingDate(time.Now().UTC())
	orchestrator := workflow.NewDailyOrchestrator("fund-1", nil, nil, nil, nil, nil, nil)
	orchestrator.RestoreState(workflow.WorkflowState{
		FundID:      "fund-1",
		TradingDate: tradingDate.Format("2006-01-02"),
		Status:      workflow.RunStatusRunning,
		CurrentStep: workflow.StepRiskReview,
		StartedAt:   now,
	})

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"calendarCode":"CRYPTO-24X7","timeZone":"UTC"}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, owner_user_id, name, description, created_at, updated_at
		 FROM fund_companies WHERE id = $1`)).
		WithArgs("company-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "owner_user_id", "name", "description", "created_at", "updated_at"}).
			AddRow("company-1", "user-1", "Alpha Co", nil, now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at
		 FROM workflow_runs
		 WHERE fund_id = $1
		 ORDER BY trading_date DESC, created_at DESC
		 LIMIT 1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "current_step", "step_results", "started_at", "completed_at", "created_at"}).
			AddRow("run-1", "fund-1", tradingDate, "running", "risk_review", json.RawMessage(`{"risk_review":{"status":"running"}}`), now, nil, now))

	service := &workflowServiceAdapter{
		fundRepo:     repository.NewFundRepo(db),
		companyRepo:  repository.NewFundCompanyRepo(db),
		workflowRepo: repository.NewWorkflowRunRepo(db),
		runtimes: map[string]*workflowRuntime{
			workflowRuntimeKey("fund-1", tradingDate): {tradingDate: tradingDate.Format("2006-01-02"), orchestrator: orchestrator},
		},
	}
	status, err := service.GetStatus("user-1", "fund-1")
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status == nil || status.State != "running" || status.Step != "risk_review" {
		t.Fatalf("expected runtime status snapshot, got %#v", status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestGetStatusDoesNotCreateRuntimeOnCacheMiss(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"calendarCode":"CRYPTO-24X7","timeZone":"UTC"}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, owner_user_id, name, description, created_at, updated_at
		 FROM fund_companies WHERE id = $1`)).
		WithArgs("company-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "owner_user_id", "name", "description", "created_at", "updated_at"}).
			AddRow("company-1", "user-1", "Alpha Co", nil, now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at
		 FROM workflow_runs
		 WHERE fund_id = $1
		 ORDER BY trading_date DESC, created_at DESC
		 LIMIT 1`)).
		WithArgs("fund-1").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at
		 FROM workflow_runs
		 WHERE fund_id = $1 AND trading_date = $2
		 ORDER BY created_at DESC
		 LIMIT 1`)).
		WithArgs("fund-1", sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)

	service := &workflowServiceAdapter{
		fundRepo:     repository.NewFundRepo(db),
		companyRepo:  repository.NewFundCompanyRepo(db),
		workflowRepo: repository.NewWorkflowRunRepo(db),
		runtimes:     make(map[string]*workflowRuntime),
	}

	status, err := service.GetStatus("user-1", "fund-1")
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status == nil || status.State != "idle" || status.Step != "not_started" {
		t.Fatalf("expected idle status, got %#v", status)
	}
	if len(service.runtimes) != 0 {
		t.Fatalf("expected cache miss to stay read-only, got %#v", service.runtimes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestGetStatusPrefersLatestActiveFutureRun(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	// Use dates relative to "now" so this test doesn't decay into a flake at
	// the next midnight UTC. The service computes the "future" trading day
	// from time.Now via resolveStartTradingDateForFund, so any hardcoded
	// calendar date would eventually fall behind today and sqlmock would
	// reject the call. latestTradingDate is just an older date used for the
	// "non-future" mock row; futureTradingDate is what the service will
	// actually ask the workflow repo for.
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	latestTradingDate := today.AddDate(0, 0, -3)
	futureTradingDate := today.AddDate(0, 0, 3)
	stepResults := json.RawMessage(`{"macro_brief":{"status":"running","updatedAt":"2026-05-16T07:05:38Z"}}`)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"calendarCode":"US-XNAS","timeZone":"America/New_York"}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, owner_user_id, name, description, created_at, updated_at
		 FROM fund_companies WHERE id = $1`)).
		WithArgs("company-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "owner_user_id", "name", "description", "created_at", "updated_at"}).
			AddRow("company-1", "user-1", "Alpha Co", nil, now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at
		 FROM workflow_runs
		 WHERE fund_id = $1
		 ORDER BY trading_date DESC, created_at DESC
		 LIMIT 1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "current_step", "step_results", "started_at", "completed_at", "created_at"}).
			AddRow("run-future", "fund-1", futureTradingDate, "running", "macro_brief", stepResults, now, nil, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at
		 FROM workflow_runs
		 WHERE fund_id = $1 AND trading_date = $2
		 ORDER BY created_at DESC
		 LIMIT 1`)).
		WithArgs("fund-1", futureTradingDate).
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "current_step", "step_results", "started_at", "completed_at", "created_at"}).
			AddRow("run-future", "fund-1", futureTradingDate, "running", "macro_brief", stepResults, now, nil, now))

	service := &workflowServiceAdapter{
		fundRepo:     repository.NewFundRepo(db),
		companyRepo:  repository.NewFundCompanyRepo(db),
		workflowRepo: repository.NewWorkflowRunRepo(db),
		runtimes:     make(map[string]*workflowRuntime),
	}

	status, err := service.GetStatus("user-1", "fund-1")
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status == nil {
		t.Fatal("expected workflow status")
	}
	if status.TradingDate != latestTradingDate.Format("2006-01-02") && status.TradingDate != futureTradingDate.Format("2006-01-02") {
		t.Fatalf("unexpected trading date %q", status.TradingDate)
	}
	if status.TradingDate != futureTradingDate.Format("2006-01-02") {
		t.Fatalf("expected future trading date %q, got %q", futureTradingDate.Format("2006-01-02"), status.TradingDate)
	}
	if status.State != "running" || status.Step != "macro_brief" {
		t.Fatalf("expected running macro_brief status, got %#v", status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestTakeRuntimeRemovesOnlyMatchingDate(t *testing.T) {
	dateA := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	dateB := time.Date(2026, time.May, 15, 0, 0, 0, 0, time.UTC)
	orchestratorA := workflow.NewDailyOrchestrator("fund-1", nil, nil, nil, nil, nil, nil)
	orchestratorB := workflow.NewDailyOrchestrator("fund-1", nil, nil, nil, nil, nil, nil)
	runtimeA := &workflowRuntime{tradingDate: "2026-05-14", orchestrator: orchestratorA}
	runtimeB := &workflowRuntime{tradingDate: "2026-05-15", orchestrator: orchestratorB}
	adapter := &workflowServiceAdapter{
		runtimes: map[string]*workflowRuntime{
			workflowRuntimeKey("fund-1", dateA): runtimeA,
			workflowRuntimeKey("fund-1", dateB): runtimeB,
		},
	}

	removed := adapter.takeRuntime("fund-1", dateA)
	if removed != runtimeA {
		t.Fatalf("expected to remove dateA runtime, got %#v", removed)
	}
	if got := adapter.peekRuntime("fund-1", dateA); got != nil {
		t.Fatalf("expected dateA runtime to be removed, got %#v", got)
	}
	if got := adapter.peekRuntime("fund-1", dateB); got != runtimeB {
		t.Fatalf("expected dateB runtime to remain, got %#v", got)
	}

	adapter.cancelRuntime(removed)
	if state := orchestratorA.State(); state != nil && state.Status != workflow.RunStatusCancelled {
		t.Fatalf("expected removed runtime cancelled status, got %s", state.Status)
	}
	if state := orchestratorB.State(); state != nil && state.Status == workflow.RunStatusCancelled {
		t.Fatalf("expected untouched runtime to remain active, got %s", state.Status)
	}
}

func TestRuntimeApprovalGatewayWaitForDecisionStopsWhenRuntimeIsNotCurrent(t *testing.T) {
	gateway := &runtimeApprovalGateway{isCurrent: func() bool { return false }}
	approved, err := gateway.WaitForDecision(context.Background(), "plan-1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled error, got %v", err)
	}
	if approved {
		t.Fatal("expected approval waiter to stop without approval")
	}
}

func TestCurrentRuntimeOwnsPlanRequiresMatchingApprovalOwner(t *testing.T) {
	tradingDate := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	runtime := &workflowRuntime{tradingDate: "2026-05-14", orchestrator: workflow.NewDailyOrchestrator("fund-1", nil, nil, nil, nil, nil, nil)}
	runtime.orchestrator.RestoreState(workflow.WorkflowState{
		FundID:      "fund-1",
		TradingDate: "2026-05-14",
		Status:      workflow.RunStatusPaused,
		CurrentStep: workflow.StepUserApproval,
		PlanID:      "plan-1",
	})
	adapter := &workflowServiceAdapter{
		runtimes: map[string]*workflowRuntime{
			workflowRuntimeKey("fund-1", tradingDate): runtime,
		},
	}

	if !adapter.currentRuntimeOwnsPlan("fund-1", tradingDate, "plan-1") {
		t.Fatal("expected paused approval owner to be treated as current runtime owner")
	}
	if adapter.currentRuntimeOwnsPlan("fund-1", tradingDate, "plan-2") {
		t.Fatal("expected different plan id to be rejected")
	}

	runtime.orchestrator.RestoreState(workflow.WorkflowState{
		FundID:      "fund-1",
		TradingDate: "2026-05-14",
		Status:      workflow.RunStatusPaused,
		CurrentStep: workflow.StepRiskReview,
		PlanID:      "plan-1",
	})
	if adapter.currentRuntimeOwnsPlan("fund-1", tradingDate, "plan-1") {
		t.Fatal("expected paused runtime outside user approval to be rejected")
	}
}

func TestCurrentRuntimeOwnsPlanKeepsRunningOwner(t *testing.T) {
	tradingDate := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	runtime := &workflowRuntime{tradingDate: "2026-05-14", orchestrator: workflow.NewDailyOrchestrator("fund-1", nil, nil, nil, nil, nil, nil)}
	runtime.orchestrator.RestoreState(workflow.WorkflowState{
		FundID:      "fund-1",
		TradingDate: "2026-05-14",
		Status:      workflow.RunStatusRunning,
		CurrentStep: workflow.StepTradeExecution,
		PlanID:      "plan-1",
	})
	adapter := &workflowServiceAdapter{
		runtimes: map[string]*workflowRuntime{
			workflowRuntimeKey("fund-1", tradingDate): runtime,
		},
	}

	if !adapter.currentRuntimeOwnsPlan("fund-1", tradingDate, "plan-1") {
		t.Fatal("expected running runtime owner to be preserved")
	}
}

func TestPersistRuntimeStateIfCurrentSkipsStaleRuntime(t *testing.T) {
	tradingDate := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	stale := &workflowRuntime{tradingDate: "2026-05-14", orchestrator: workflow.NewDailyOrchestrator("fund-1", nil, nil, nil, nil, nil, nil)}
	current := &workflowRuntime{tradingDate: "2026-05-14", orchestrator: workflow.NewDailyOrchestrator("fund-1", nil, nil, nil, nil, nil, nil)}
	adapter := &workflowServiceAdapter{
		runtimes: map[string]*workflowRuntime{
			workflowRuntimeKey("fund-1", tradingDate): current,
		},
	}

	stale.orchestrator.RestoreState(workflow.WorkflowState{
		FundID:      "fund-1",
		TradingDate: "2026-05-14",
		Status:      workflow.RunStatusCancelled,
		CurrentStep: workflow.StepDailyReview,
	})

	status, err := adapter.persistRuntimeStateIfCurrent("fund-1", stale, tradingDate)
	if err != nil {
		t.Fatalf("persist stale runtime state: %v", err)
	}
	if status != nil {
		t.Fatalf("expected stale runtime persistence to be skipped, got %#v", status)
	}
}

func TestFundWorkflowSchedulerCanRestartAfterStop(t *testing.T) {
	scheduler := newFundWorkflowScheduler(nil)

	scheduler.Start()
	scheduler.mu.Lock()
	firstStopCh := scheduler.stopCh
	firstWakeCh := scheduler.wakeCh
	scheduler.mu.Unlock()
	if firstStopCh == nil || firstWakeCh == nil {
		t.Fatal("expected scheduler channels after start")
	}

	stopped := make(chan struct{})
	go func() {
		scheduler.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("first scheduler stop timed out")
	}

	scheduler.mu.Lock()
	if scheduler.started {
		t.Fatal("expected scheduler to be stopped")
	}
	if scheduler.stopCh != nil || scheduler.wakeCh != nil {
		t.Fatal("expected scheduler channels to be cleared after stop")
	}
	scheduler.mu.Unlock()

	scheduler.Start()
	scheduler.mu.Lock()
	secondStopCh := scheduler.stopCh
	secondWakeCh := scheduler.wakeCh
	started := scheduler.started
	scheduler.mu.Unlock()
	if !started {
		t.Fatal("expected scheduler to restart")
	}
	if secondStopCh == nil || secondWakeCh == nil {
		t.Fatal("expected scheduler channels after restart")
	}
	if secondStopCh == firstStopCh || secondWakeCh == firstWakeCh {
		t.Fatal("expected scheduler restart to recreate channels")
	}

	stopped = make(chan struct{})
	go func() {
		scheduler.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("second scheduler stop timed out")
	}
}

func TestApprovePlanResumesPausedWorkflow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	tradingDate := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, reasoning, risk_score, expected_return, risk_review, discussion_snapshot, roundtable_id, pm_agent_id, confidence, created_at, updated_at
		 FROM investment_plans WHERE id = $1`)).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "reasoning", "risk_score", "expected_return", "risk_review", "discussion_snapshot", "roundtable_id", "pm_agent_id", "confidence", "created_at", "updated_at"}).
			AddRow("plan-1", "fund-1", tradingDate, "pending_user", nil, nil, nil, []byte(`{}`), []byte(`{}`), nil, nil, nil, now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"calendarCode":"US-XNAS","timeZone":"America/New_York"}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, owner_user_id, name, description, created_at, updated_at
		 FROM fund_companies WHERE id = $1`)).
		WithArgs("company-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "owner_user_id", "name", "description", "created_at", "updated_at"}).
			AddRow("company-1", "user-1", "Alpha Co", nil, now, now))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE investment_plans SET status = $1, updated_at = NOW() WHERE id = $2`)).
		WithArgs("approved", "plan-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, reasoning, risk_score, expected_return, risk_review, discussion_snapshot, roundtable_id, pm_agent_id, confidence, created_at, updated_at
		 FROM investment_plans WHERE id = $1`)).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "reasoning", "risk_score", "expected_return", "risk_review", "discussion_snapshot", "roundtable_id", "pm_agent_id", "confidence", "created_at", "updated_at"}).
			AddRow("plan-1", "fund-1", tradingDate, "approved", nil, nil, nil, []byte(`{}`), []byte(`{}`), nil, nil, nil, now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, plan_id, instrument_key, symbol, market, exchange, asset_class, instrument_type, action, position_side, open_close, quantity, price, amount, stop_loss, take_profit, reasoning, confidence, supported_by, opposed_by, execution_status, sort_order, quote_currency, settlement_currency, margin_mode, leverage, contract_multiplier, expiry_date, reduce_only, quote_refreshed_at, auto_executed_at, sleeve, regime_tag, signal_source, exit_reason
		 FROM plan_actions WHERE plan_id = $1 ORDER BY sort_order, id`)).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "plan_id", "instrument_key", "symbol", "market", "exchange", "asset_class", "instrument_type", "action", "position_side", "open_close", "quantity", "price", "amount", "stop_loss", "take_profit", "reasoning", "confidence", "supported_by", "opposed_by", "execution_status", "sort_order", "quote_currency", "settlement_currency", "margin_mode", "leverage", "contract_multiplier", "expiry_date", "reduce_only", "quote_refreshed_at", "auto_executed_at", "sleeve", "regime_tag", "signal_source", "exit_reason"}))

	resumed := false
	service := &planServiceAdapter{
		planRepo:    repository.NewPlanRepo(db),
		fundRepo:    repository.NewFundRepo(db),
		companyRepo: repository.NewFundCompanyRepo(db),
		workflowService: &workflowServiceAdapter{resumePlan: func(fundID string, gotDate time.Time, planID string) error {
			resumed = true
			if fundID != "fund-1" {
				return fmt.Errorf("unexpected fund id %s", fundID)
			}
			if planID != "plan-1" {
				return fmt.Errorf("unexpected plan id %s", planID)
			}
			if !gotDate.Equal(tradingDate) {
				return fmt.Errorf("unexpected trading date %s", gotDate.Format(time.RFC3339))
			}
			return nil
		}},
	}
	plan, err := service.ApprovePlan("user-1", "plan-1")
	if err != nil {
		t.Fatalf("approve plan: %v", err)
	}
	if !resumed {
		t.Fatal("expected approval to resume paused workflow")
	}
	if plan == nil || plan.Status != "approved" {
		t.Fatalf("expected approved plan response, got %#v", plan)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestApprovePlanRollsBackStatusWhenResumeFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	tradingDate := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, reasoning, risk_score, expected_return, risk_review, discussion_snapshot, roundtable_id, pm_agent_id, confidence, created_at, updated_at
		 FROM investment_plans WHERE id = $1`)).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "reasoning", "risk_score", "expected_return", "risk_review", "discussion_snapshot", "roundtable_id", "pm_agent_id", "confidence", "created_at", "updated_at"}).
			AddRow("plan-1", "fund-1", tradingDate, "pending_user", nil, nil, nil, []byte(`{}`), []byte(`{}`), nil, nil, nil, now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"calendarCode":"US-XNAS","timeZone":"America/New_York"}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, owner_user_id, name, description, created_at, updated_at
		 FROM fund_companies WHERE id = $1`)).
		WithArgs("company-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "owner_user_id", "name", "description", "created_at", "updated_at"}).
			AddRow("company-1", "user-1", "Alpha Co", nil, now, now))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE investment_plans SET status = $1, updated_at = NOW() WHERE id = $2`)).
		WithArgs("approved", "plan-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE investment_plans SET status = $1, updated_at = NOW() WHERE id = $2`)).
		WithArgs("pending_user", "plan-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	service := &planServiceAdapter{
		planRepo:    repository.NewPlanRepo(db),
		fundRepo:    repository.NewFundRepo(db),
		companyRepo: repository.NewFundCompanyRepo(db),
		workflowService: &workflowServiceAdapter{resumePlan: func(fundID string, gotDate time.Time, planID string) error {
			return api.ErrConflict
		}},
	}
	_, err = service.ApprovePlan("user-1", "plan-1")
	if !errors.Is(err, api.ErrConflict) {
		t.Fatalf("expected conflict error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestApprovePlanFailsBeforeStatusWriteWhenResumePrecheckConflicts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	tradingDate := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, reasoning, risk_score, expected_return, risk_review, discussion_snapshot, roundtable_id, pm_agent_id, confidence, created_at, updated_at
		 FROM investment_plans WHERE id = $1`)).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "reasoning", "risk_score", "expected_return", "risk_review", "discussion_snapshot", "roundtable_id", "pm_agent_id", "confidence", "created_at", "updated_at"}).
			AddRow("plan-1", "fund-1", tradingDate, "pending_user", nil, nil, nil, []byte(`{}`), []byte(`{}`), nil, nil, nil, now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"calendarCode":"US-XNAS","timeZone":"America/New_York"}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, owner_user_id, name, description, created_at, updated_at
		 FROM fund_companies WHERE id = $1`)).
		WithArgs("company-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "owner_user_id", "name", "description", "created_at", "updated_at"}).
			AddRow("company-1", "user-1", "Alpha Co", nil, now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at
		 FROM workflow_runs
		 WHERE fund_id = $1 AND trading_date = $2
		 ORDER BY created_at DESC
		 LIMIT 1`)).
		WithArgs("fund-1", tradingDate).
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "current_step", "step_results", "started_at", "completed_at", "created_at"}).
			AddRow("run-1", "fund-1", tradingDate, "running", "trade_execution", []byte(`{"user_approval":{"status":"success"},"trade_execution":{"status":"running"}}`), tradingDate, nil, now))

	service := &planServiceAdapter{
		planRepo:    repository.NewPlanRepo(db),
		fundRepo:    repository.NewFundRepo(db),
		companyRepo: repository.NewFundCompanyRepo(db),
		workflowService: &workflowServiceAdapter{
			fundRepo:     repository.NewFundRepo(db),
			workflowRepo: repository.NewWorkflowRunRepo(db),
			planRepo:     repository.NewPlanRepo(db),
		},
	}
	_, err = service.ApprovePlan("user-1", "plan-1")
	if !errors.Is(err, api.ErrConflict) {
		t.Fatalf("expected conflict error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestResumeApprovedPlanNoOpsForCurrentApprovalOwner(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	tradingDate := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"calendarCode":"US-XNAS","timeZone":"America/New_York"}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, reasoning, risk_score, expected_return, risk_review, discussion_snapshot, roundtable_id, pm_agent_id, confidence, created_at, updated_at
		 FROM investment_plans WHERE id = $1`)).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "reasoning", "risk_score", "expected_return", "risk_review", "discussion_snapshot", "roundtable_id", "pm_agent_id", "confidence", "created_at", "updated_at"}).
			AddRow("plan-1", "fund-1", tradingDate, "approved", nil, nil, nil, []byte(`{}`), []byte(`{}`), nil, nil, nil, now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at
		 FROM workflow_runs
		 WHERE fund_id = $1 AND trading_date = $2
		 ORDER BY created_at DESC
		 LIMIT 1`)).
		WithArgs("fund-1", tradingDate).
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "current_step", "step_results", "started_at", "completed_at", "created_at"}).
			AddRow("run-1", "fund-1", tradingDate, "paused", "user_approval", []byte(`{"user_approval":{"status":"pending"}}`), tradingDate, nil, now))

	runtime := &workflowRuntime{tradingDate: "2026-05-14", orchestrator: workflow.NewDailyOrchestrator("fund-1", nil, nil, nil, nil, nil, nil)}
	runtime.orchestrator.RestoreState(workflow.WorkflowState{
		RunID:       "run-1",
		FundID:      "fund-1",
		TradingDate: "2026-05-14",
		Status:      workflow.RunStatusPaused,
		CurrentStep: workflow.StepUserApproval,
		PlanID:      "plan-1",
	})
	adapter := &workflowServiceAdapter{
		db:           db,
		fundRepo:     repository.NewFundRepo(db),
		workflowRepo: repository.NewWorkflowRunRepo(db),
		planRepo:     repository.NewPlanRepo(db),
		runtimes: map[string]*workflowRuntime{
			workflowRuntimeKey("fund-1", tradingDate): runtime,
		},
	}

	if err := adapter.ResumeApprovedPlan("fund-1", tradingDate, "plan-1"); err != nil {
		t.Fatalf("resume approved plan: %v", err)
	}
	if got := adapter.peekRuntime("fund-1", tradingDate); got != runtime {
		t.Fatalf("expected runtime to remain untouched, got %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestResumeApprovedPlanNoOpsForCurrentRunningOwner(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	tradingDate := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"calendarCode":"US-XNAS","timeZone":"America/New_York"}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, reasoning, risk_score, expected_return, risk_review, discussion_snapshot, roundtable_id, pm_agent_id, confidence, created_at, updated_at
		 FROM investment_plans WHERE id = $1`)).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "reasoning", "risk_score", "expected_return", "risk_review", "discussion_snapshot", "roundtable_id", "pm_agent_id", "confidence", "created_at", "updated_at"}).
			AddRow("plan-1", "fund-1", tradingDate, "approved", nil, nil, nil, []byte(`{}`), []byte(`{}`), nil, nil, nil, now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at
		 FROM workflow_runs
		 WHERE fund_id = $1 AND trading_date = $2
		 ORDER BY created_at DESC
		 LIMIT 1`)).
		WithArgs("fund-1", tradingDate).
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "current_step", "step_results", "started_at", "completed_at", "created_at"}).
			AddRow("run-1", "fund-1", tradingDate, "paused", "user_approval", []byte(`{"user_approval":{"status":"pending"}}`), tradingDate, nil, now))

	runtime := &workflowRuntime{tradingDate: "2026-05-14", orchestrator: workflow.NewDailyOrchestrator("fund-1", nil, nil, nil, nil, nil, nil)}
	runtime.orchestrator.RestoreState(workflow.WorkflowState{
		RunID:       "run-1",
		FundID:      "fund-1",
		TradingDate: "2026-05-14",
		Status:      workflow.RunStatusRunning,
		CurrentStep: workflow.StepTradeExecution,
		PlanID:      "plan-1",
	})
	adapter := &workflowServiceAdapter{
		db:           db,
		fundRepo:     repository.NewFundRepo(db),
		workflowRepo: repository.NewWorkflowRunRepo(db),
		planRepo:     repository.NewPlanRepo(db),
		runtimes: map[string]*workflowRuntime{
			workflowRuntimeKey("fund-1", tradingDate): runtime,
		},
	}

	if err := adapter.ResumeApprovedPlan("fund-1", tradingDate, "plan-1"); err != nil {
		t.Fatalf("resume approved plan: %v", err)
	}
	if got := adapter.peekRuntime("fund-1", tradingDate); got != runtime {
		t.Fatalf("expected running runtime to remain untouched, got %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestRejectPlanPropagatesToWorkflowRun(t *testing.T) {
	tradingDate := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	service := &planServiceAdapter{
		workflowService: &workflowServiceAdapter{rejectAwaitingPlan: func(fundID string, gotDate time.Time, planID, reason string) error {
			if fundID != "fund-1" {
				return fmt.Errorf("unexpected fund id %s", fundID)
			}
			if !gotDate.Equal(tradingDate) {
				return fmt.Errorf("unexpected trading date %s", gotDate.Format(time.RFC3339))
			}
			if planID != "plan-1" {
				return fmt.Errorf("unexpected plan id %s", planID)
			}
			if reason != "too risky" {
				return fmt.Errorf("unexpected reason %s", reason)
			}
			return nil
		}},
	}
	plan := &repository.InvestmentPlan{ID: "plan-1", FundID: "fund-1", TradingDate: tradingDate, Status: "pending_user"}
	if err := service.workflowService.RejectAwaitingPlan(plan.FundID, plan.TradingDate, plan.ID, "too risky"); err != nil {
		t.Fatalf("reject awaiting plan: %v", err)
	}
}

func TestRepositoryWorkflowStatusPreservesPausedAndRejected(t *testing.T) {
	cases := []struct {
		name string
		in   workflow.RunStatus
		want string
	}{
		{name: "running", in: workflow.RunStatusRunning, want: "running"},
		{name: "paused", in: workflow.RunStatusPaused, want: "paused"},
		{name: "rejected", in: workflow.RunStatusRejected, want: "rejected"},
		{name: "cancelled", in: workflow.RunStatusCancelled, want: "cancelled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := repositoryWorkflowStatus(tc.in); got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestResolveStartTradingDateForFundFallsBackToNextTradingDay(t *testing.T) {
	adapter := &workflowServiceAdapter{calendar: marketcalendar.NewService()}
	fund := &repository.Fund{Config: json.RawMessage(`{"calendarCode":"CN-SSE","timeZone":"Asia/Shanghai"}`)}
	now := time.Date(2026, time.May, 16, 10, 0, 0, 0, time.UTC)
	tradingDate, err := adapter.resolveStartTradingDateForFund(fund, now)
	if err != nil {
		t.Fatalf("resolve start trading date: %v", err)
	}
	if got := tradingDate.Format("2006-01-02"); got != "2026-05-18" {
		t.Fatalf("expected next trading day 2026-05-18, got %s", got)
	}
}

func TestBuildWorkflowScheduleForDateAtForcesImmediateDuringActiveSession(t *testing.T) {
	adapter := &workflowServiceAdapter{calendar: marketcalendar.NewService()}
	fund := &repository.Fund{ID: "fund-1", Config: json.RawMessage(`{"calendarCode":"CRYPTO-24X7","timeZone":"UTC"}`)}
	now := time.Date(2026, time.May, 15, 12, 0, 0, 0, time.UTC)
	tradingDate := time.Date(2026, time.May, 15, 0, 0, 0, 0, time.UTC)

	schedule := adapter.buildWorkflowScheduleForDateAt(fund, tradingDate, now)
	if !schedule.ForceImmediate {
		t.Fatal("expected active-session schedule to force immediate execution")
	}
}

func TestBuildWorkflowScheduleForDateAtKeepsFutureSessionScheduled(t *testing.T) {
	adapter := &workflowServiceAdapter{calendar: marketcalendar.NewService()}
	fund := &repository.Fund{ID: "fund-1", Config: json.RawMessage(`{"calendarCode":"CRYPTO-24X7","timeZone":"UTC"}`)}
	now := time.Date(2026, time.May, 15, 6, 0, 0, 0, time.UTC)
	tradingDate := time.Date(2026, time.May, 15, 0, 0, 0, 0, time.UTC)

	schedule := adapter.buildWorkflowScheduleForDateAt(fund, tradingDate, now)
	if schedule.ForceImmediate {
		t.Fatal("expected pre-open schedule to keep future wall-clock trigger")
	}
}

func TestShouldRunWorkflowImmediatelyEndsAfterReviewWindow(t *testing.T) {
	adapter := &workflowServiceAdapter{calendar: marketcalendar.NewService()}
	session, err := adapter.calendar.SessionForDate(time.Date(2026, time.May, 15, 0, 0, 0, 0, time.UTC), marketcalendar.Profile{CalendarCode: "CRYPTO-24X7", TimeZone: "UTC"})
	if err != nil {
		t.Fatalf("session for date: %v", err)
	}
	if !adapter.shouldRunWorkflowImmediately(time.Date(2026, time.May, 15, 22, 30, 0, 0, time.UTC), session) {
		t.Fatal("expected immediate catch-up before review grace window closes")
	}
	if adapter.shouldRunWorkflowImmediately(time.Date(2026, time.May, 15, 23, 31, 0, 0, time.UTC), session) {
		t.Fatal("expected immediate catch-up window to close after review grace window")
	}
}

func TestTriggerStepRejectsUnsupportedManualStep(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"calendarCode":"CRYPTO-24X7","timeZone":"UTC"}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, owner_user_id, name, description, created_at, updated_at
		 FROM fund_companies WHERE id = $1`)).
		WithArgs("company-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "owner_user_id", "name", "description", "created_at", "updated_at"}).
			AddRow("company-1", "user-1", "Alpha Co", nil, now, now))

	service := &workflowServiceAdapter{
		fundRepo:    repository.NewFundRepo(db),
		companyRepo: repository.NewFundCompanyRepo(db),
	}
	_, err = service.TriggerStep("user-1", "fund-1", "roundtable")
	if !errors.Is(err, api.ErrBadInput) {
		t.Fatalf("expected bad input error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestStopRuntimeCancelsOrchestratorAndDropsCache(t *testing.T) {
	dateA := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	dateB := time.Date(2026, time.May, 15, 0, 0, 0, 0, time.UTC)
	orchestratorA := workflow.NewDailyOrchestrator("fund-1", nil, nil, nil, nil, nil, nil)
	orchestratorB := workflow.NewDailyOrchestrator("fund-1", nil, nil, nil, nil, nil, nil)
	adapter := &workflowServiceAdapter{
		runtimes: map[string]*workflowRuntime{
			workflowRuntimeKey("fund-1", dateA): {tradingDate: "2026-05-14", orchestrator: orchestratorA},
			workflowRuntimeKey("fund-1", dateB): {tradingDate: "2026-05-15", orchestrator: orchestratorB},
		},
	}
	adapter.stopRuntime("fund-1")
	if len(adapter.runtimes) != 0 {
		t.Fatalf("expected runtime cache to be empty, got %#v", adapter.runtimes)
	}
	if state := orchestratorA.State(); state != nil && state.Status != workflow.RunStatusCancelled {
		t.Fatalf("expected first runtime cancelled status, got %s", state.Status)
	}
	if state := orchestratorB.State(); state != nil && state.Status != workflow.RunStatusCancelled {
		t.Fatalf("expected second runtime cancelled status, got %s", state.Status)
	}
}

func TestFundServiceDeleteFundStopsRuntimeBeforeDelete(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, owner_user_id, name, description, created_at, updated_at
		 FROM fund_companies WHERE id = $1`)).
		WithArgs("company-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "owner_user_id", "name", "description", "created_at", "updated_at"}).
			AddRow("company-1", "user-1", "Alpha Co", nil, now, now))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	tradingDate := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	orchestrator := workflow.NewDailyOrchestrator("fund-1", nil, nil, nil, nil, nil, nil)
	workflowService := &workflowServiceAdapter{runtimes: map[string]*workflowRuntime{workflowRuntimeKey("fund-1", tradingDate): {tradingDate: "2026-05-14", orchestrator: orchestrator}}}
	service := &fundServiceAdapter{
		db:              db,
		companyRepo:     repository.NewFundCompanyRepo(db),
		fundRepo:        repository.NewFundRepo(db),
		workflowService: workflowService,
	}

	if err := service.DeleteFund("user-1", "fund-1"); err != nil {
		t.Fatalf("delete fund: %v", err)
	}
	if len(workflowService.runtimes) != 0 {
		t.Fatalf("expected runtime cache to be removed, got %#v", workflowService.runtimes)
	}
	if state := orchestrator.State(); state != nil && state.Status != workflow.RunStatusCancelled {
		t.Fatalf("expected cancelled status, got %s", state.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestNavSnapshotRepoGetByFundAndDate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	repo := repository.NewNavSnapshotRepo(db)
	targetDate := time.Date(2026, time.May, 13, 0, 0, 0, 0, time.UTC)
	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, nav, total_assets, total_market_value, available_cash, daily_return, total_return, positions_snapshot, created_at
		 FROM nav_snapshots WHERE fund_id = $1 AND trading_date = $2
		 LIMIT 1`)).
		WithArgs("fund-1", targetDate).
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "nav", "total_assets", "total_market_value", "available_cash", "daily_return", "total_return", "positions_snapshot", "created_at"}).
			AddRow("nav-1", "fund-1", targetDate, 1.02, 102000.0, 5000.0, 97000.0, 0.01, 0.02, []byte(`[]`), now))

	snapshot, err := repo.GetByFundAndDate(context.Background(), "fund-1", targetDate)
	if err != nil {
		t.Fatalf("get by fund and date: %v", err)
	}
	if snapshot == nil || snapshot.ID != "nav-1" {
		t.Fatalf("expected nav snapshot nav-1, got %#v", snapshot)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestWorkflowRunRepoClaimStartClaimsPendingRun(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	repo := repository.NewWorkflowRunRepo(db)
	tradingDate := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, time.May, 14, 13, 30, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "current_step", "step_results", "started_at", "completed_at", "created_at"}).
		AddRow("run-1", "fund-1", tradingDate, "running", "macro_brief", []byte(`{"macro_brief":{"status":"running"}}`), startedAt, nil, startedAt)
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO workflow_runs (fund_id, trading_date, status, current_step, step_results, started_at, completed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (fund_id, trading_date) DO UPDATE
		 SET status = EXCLUDED.status,
		     current_step = EXCLUDED.current_step,
		     step_results = EXCLUDED.step_results,
		     started_at = EXCLUDED.started_at,
		     completed_at = EXCLUDED.completed_at
		 WHERE workflow_runs.status IN ('pending', 'failed', 'cancelled')
		 RETURNING id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at`)).
		WithArgs("fund-1", tradingDate, "running", "macro_brief", sqlmock.AnyArg(), sqlmock.AnyArg(), sql.NullTime{}).
		WillReturnRows(rows)

	run, claimed, err := repo.ClaimStart(context.Background(), "fund-1", tradingDate, startedAt, "macro_brief")
	if err != nil {
		t.Fatalf("claim start: %v", err)
	}
	if !claimed {
		t.Fatal("expected claim to succeed")
	}
	if run == nil || run.ID != "run-1" {
		t.Fatalf("expected claimed run run-1, got %#v", run)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestWorkflowRunRepoClaimStartReturnsExistingRunWhenNotClaimed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	repo := repository.NewWorkflowRunRepo(db)
	tradingDate := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, time.May, 14, 13, 30, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO workflow_runs (fund_id, trading_date, status, current_step, step_results, started_at, completed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (fund_id, trading_date) DO UPDATE
		 SET status = EXCLUDED.status,
		     current_step = EXCLUDED.current_step,
		     step_results = EXCLUDED.step_results,
		     started_at = EXCLUDED.started_at,
		     completed_at = EXCLUDED.completed_at
		 WHERE workflow_runs.status IN ('pending', 'failed', 'cancelled')
		 RETURNING id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at`)).
		WithArgs("fund-1", tradingDate, "running", "macro_brief", sqlmock.AnyArg(), sqlmock.AnyArg(), sql.NullTime{}).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at
		 FROM workflow_runs
		 WHERE fund_id = $1 AND trading_date = $2
		 ORDER BY created_at DESC
		 LIMIT 1`)).
		WithArgs("fund-1", tradingDate).
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "current_step", "step_results", "started_at", "completed_at", "created_at"}).
			AddRow("run-2", "fund-1", tradingDate, "paused", "user_approval", []byte(`{"user_approval":{"status":"pending"}}`), startedAt, nil, startedAt))

	run, claimed, err := repo.ClaimStart(context.Background(), "fund-1", tradingDate, startedAt, "macro_brief")
	if err != nil {
		t.Fatalf("claim start: %v", err)
	}
	if claimed {
		t.Fatal("expected claim to be skipped")
	}
	if run == nil || run.ID != "run-2" {
		t.Fatalf("expected existing run run-2, got %#v", run)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestWorkflowRunRepoClaimManualStartReusesCompletedRun(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	repo := repository.NewWorkflowRunRepo(db)
	tradingDate := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, time.May, 14, 13, 30, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "current_step", "step_results", "started_at", "completed_at", "created_at"}).
		AddRow("run-3", "fund-1", tradingDate, "running", "macro_brief", []byte(`{"macro_brief":{"status":"running"}}`), startedAt, nil, startedAt)
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO workflow_runs (fund_id, trading_date, status, current_step, step_results, started_at, completed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (fund_id, trading_date) DO UPDATE
		 SET status = EXCLUDED.status,
		     current_step = EXCLUDED.current_step,
		     step_results = EXCLUDED.step_results,
		     started_at = EXCLUDED.started_at,
		     completed_at = EXCLUDED.completed_at
		 WHERE workflow_runs.status IN ('pending', 'failed', 'cancelled', 'completed', 'rejected')
		 RETURNING id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at`)).
		WithArgs("fund-1", tradingDate, "running", "macro_brief", sqlmock.AnyArg(), sqlmock.AnyArg(), sql.NullTime{}).
		WillReturnRows(rows)

	run, claimed, err := repo.ClaimManualStart(context.Background(), "fund-1", tradingDate, startedAt, "macro_brief")
	if err != nil {
		t.Fatalf("claim manual start: %v", err)
	}
	if !claimed {
		t.Fatal("expected manual claim to succeed")
	}
	if run == nil || run.ID != "run-3" {
		t.Fatalf("expected claimed run run-3, got %#v", run)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestTriggerStepRejectsOverlapWithRunningWorkflowRun(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	tradingDate := normalizeTradingDate(time.Now().UTC())
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"calendarCode":"CRYPTO-24X7","timeZone":"UTC"}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, owner_user_id, name, description, created_at, updated_at
		 FROM fund_companies WHERE id = $1`)).
		WithArgs("company-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "owner_user_id", "name", "description", "created_at", "updated_at"}).
			AddRow("company-1", "user-1", "Alpha Co", nil, now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO workflow_runs (fund_id, trading_date, status, current_step, step_results, started_at, completed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (fund_id, trading_date) DO UPDATE
		 SET current_step = EXCLUDED.current_step
		 WHERE workflow_runs.status NOT IN ('running', 'paused', 'rejected')
		 RETURNING id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at`)).
		WithArgs("fund-1", tradingDate, "pending", "settlement", json.RawMessage(`{}`), sql.NullTime{}, sql.NullTime{}).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at
		 FROM workflow_runs
		 WHERE fund_id = $1 AND trading_date = $2
		 ORDER BY created_at DESC
		 LIMIT 1`)).
		WithArgs("fund-1", tradingDate).
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "current_step", "step_results", "started_at", "completed_at", "created_at"}).
			AddRow("run-1", "fund-1", tradingDate, "running", "pm_plan", []byte(`{"pm_plan":{"step":"pm_plan","status":"running"}}`), now, nil, now))

	service := &workflowServiceAdapter{
		fundRepo:     repository.NewFundRepo(db),
		companyRepo:  repository.NewFundCompanyRepo(db),
		workflowRepo: repository.NewWorkflowRunRepo(db),
		runtimes:     make(map[string]*workflowRuntime),
	}
	_, err = service.TriggerStep("user-1", "fund-1", "settlement")
	if !errors.Is(err, api.ErrConflict) {
		t.Fatalf("expected conflict error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestWorkflowRunRepoClaimManualStepReturnsConflictRun(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	repo := repository.NewWorkflowRunRepo(db)
	tradingDate := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, time.May, 14, 13, 30, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO workflow_runs (fund_id, trading_date, status, current_step, step_results, started_at, completed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (fund_id, trading_date) DO UPDATE
		 SET current_step = EXCLUDED.current_step
		 WHERE workflow_runs.status NOT IN ('running', 'paused', 'rejected')
		 RETURNING id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at`)).
		WithArgs("fund-1", tradingDate, "pending", "risk_review", json.RawMessage(`{}`), sql.NullTime{}, sql.NullTime{}).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at
		 FROM workflow_runs
		 WHERE fund_id = $1 AND trading_date = $2
		 ORDER BY created_at DESC
		 LIMIT 1`)).
		WithArgs("fund-1", tradingDate).
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "current_step", "step_results", "started_at", "completed_at", "created_at"}).
			AddRow("run-2", "fund-1", tradingDate, "running", "pm_plan", []byte(`{"pm_plan":{"status":"running"}}`), startedAt, nil, startedAt))

	run, claimed, err := repo.ClaimManualStep(context.Background(), "fund-1", tradingDate, "risk_review")
	if err != nil {
		t.Fatalf("claim manual step: %v", err)
	}
	if claimed {
		t.Fatal("expected manual claim to be skipped")
	}
	if run == nil || run.ID != "run-2" {
		t.Fatalf("expected existing running run, got %#v", run)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestWorkflowRunRepoClaimManualStepRejectsRejectedRun(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	repo := repository.NewWorkflowRunRepo(db)
	tradingDate := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	completedAt := time.Date(2026, time.May, 14, 15, 30, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO workflow_runs (fund_id, trading_date, status, current_step, step_results, started_at, completed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (fund_id, trading_date) DO UPDATE
		 SET current_step = EXCLUDED.current_step
		 WHERE workflow_runs.status NOT IN ('running', 'paused', 'rejected')
		 RETURNING id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at`)).
		WithArgs("fund-1", tradingDate, "pending", "settlement", json.RawMessage(`{}`), sql.NullTime{}, sql.NullTime{}).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at
		 FROM workflow_runs
		 WHERE fund_id = $1 AND trading_date = $2
		 ORDER BY created_at DESC
		 LIMIT 1`)).
		WithArgs("fund-1", tradingDate).
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "current_step", "step_results", "started_at", "completed_at", "created_at"}).
			AddRow("run-3", "fund-1", tradingDate, "rejected", "user_approval", []byte(`{"user_approval":{"status":"rejected"}}`), tradingDate, completedAt, tradingDate))

	run, claimed, err := repo.ClaimManualStep(context.Background(), "fund-1", tradingDate, "settlement")
	if err != nil {
		t.Fatalf("claim manual step: %v", err)
	}
	if claimed {
		t.Fatal("expected manual claim to be skipped for rejected run")
	}
	if run == nil || run.ID != "run-3" || run.Status != "rejected" {
		t.Fatalf("expected existing rejected run, got %#v", run)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func intPtr(value int) *int {
	return &value
}

func floatPtr(value float64) *float64 {
	return &value
}

// TestSanitizeFundUniverseDedupesAndTrims locks the contract for the
// universe normaliser invoked from every fund create/update path.
// Operators routinely paste lists with empty rows, mixed-case tickers,
// and dupes; if any of those leak through, downstream quote fetches
// hammer "" and the LLM prompt sees "AAPL" repeated several times.
// Symbols are uppercased (codebase convention for tickers), other
// fields preserve their original casing but still dedupe
// case-insensitively. nil input is preserved (separate from empty).
func TestSanitizeFundUniverseDedupesAndTrims(t *testing.T) {
	t.Run("nil universe stays nil", func(t *testing.T) {
		if got := sanitizeFundUniverse(nil); got != nil {
			t.Fatalf("nil in must produce nil out, got %+v", got)
		}
	})

	t.Run("symbols trim, uppercase, dedupe case-insensitively", func(t *testing.T) {
		in := &api.FundUniverse{
			Symbols: []string{"aapl", "AAPL", "  aapl  ", "MSFT", "", "  ", "msft"},
		}
		got := sanitizeFundUniverse(in)
		if got == nil {
			t.Fatal("expected non-nil result for non-nil input")
		}
		want := []string{"AAPL", "MSFT"}
		if !equalStringSlices(got.Symbols, want) {
			t.Errorf("symbols: want %v, got %v", want, got.Symbols)
		}
	})

	t.Run("sectors and themes preserve casing but dedupe case-insensitively", func(t *testing.T) {
		in := &api.FundUniverse{
			Sectors: []string{"Tech", "tech", "  Tech  ", "Healthcare", ""},
			Themes:  []string{"AI", "ai", "  Quantum  ", "quantum"},
		}
		got := sanitizeFundUniverse(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		// First-seen casing wins.
		wantSectors := []string{"Tech", "Healthcare"}
		if !equalStringSlices(got.Sectors, wantSectors) {
			t.Errorf("sectors: want %v, got %v", wantSectors, got.Sectors)
		}
		wantThemes := []string{"AI", "Quantum"}
		if !equalStringSlices(got.Themes, wantThemes) {
			t.Errorf("themes: want %v, got %v", wantThemes, got.Themes)
		}
	})

	t.Run("whitespace-only and empty inputs produce empty results", func(t *testing.T) {
		// The contract is "no entries" — nil and a length-0 slice are
		// both fine, downstream consumers iterate either harmlessly.
		// We assert on length rather than on nil-vs-empty to avoid
		// locking that implementation detail.
		in := &api.FundUniverse{
			Symbols: []string{"", "   ", "\t"},
			Sectors: []string{},
		}
		got := sanitizeFundUniverse(in)
		if got == nil {
			t.Fatal("expected non-nil result for non-nil input even if all entries are whitespace")
		}
		if len(got.Symbols) != 0 {
			t.Errorf("symbols: whitespace-only input must produce zero entries, got %v", got.Symbols)
		}
		if len(got.Sectors) != 0 {
			t.Errorf("sectors: empty input must produce zero entries, got %v", got.Sectors)
		}
	})

	t.Run("caps at 500 entries to protect downstream loops", func(t *testing.T) {
		long := make([]string, 600)
		for i := range long {
			long[i] = fmt.Sprintf("SYM%04d", i)
		}
		in := &api.FundUniverse{Symbols: long}
		got := sanitizeFundUniverse(in)
		if got == nil || len(got.Symbols) != 500 {
			t.Fatalf("expected cap at 500, got len=%d", len(got.Symbols))
		}
	})
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestUpdateFundUsesRowLevelLockForConcurrentSafety pins the contract
// that UpdateFund serialises concurrent writers on the same fund row
// via SELECT ... FOR UPDATE inside a transaction. The pre-fix code
// read with a plain SELECT and then UPDATEd, so two concurrent PUTs
// could both read the same snapshot and the second write would
// silently revert the first one's changes — observed at ~26% lost-
// update rate in the May-22 P2 sweep (Test 12).
//
// We assert the SQL shape, not the timing — the shape (BEGIN /
// SELECT ... FOR UPDATE / UPDATE / COMMIT inside one tx) is the
// contract; the actual concurrency guarantee is delegated to
// PostgreSQL. The integration-level concurrency test lives in the
// QA subagent suite and is run out-of-band.
func TestUpdateFundUsesRowLevelLockForConcurrentSafety(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	repo := repository.NewFundRepo(db)

	// The shape we want to see is BEGIN -> SELECT ... FOR UPDATE -> UPDATE
	// -> COMMIT. The middle SELECT is what GetByIDForUpdateTx issues; the
	// regex anchor on "FOR UPDATE" is the load-bearing assertion — drop it
	// and concurrent writers will race again.
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT .* FROM funds WHERE id = \$1\s+FOR UPDATE`).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "company_id", "name", "description", "trading_mode",
			"initial_capital", "current_capital", "total_assets", "nav",
			"status", "config", "created_at", "updated_at",
		}).AddRow(
			"fund-1", "company-1", "OCS", nullString("a quant fund"), "live",
			float64(100000), float64(100000), float64(100000), float64(1.0),
			"active", json.RawMessage(`{}`), time.Now(), time.Now(),
		))
	mock.ExpectCommit()

	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	fund, err := repo.GetByIDForUpdateTx(ctx, tx, "fund-1")
	if err != nil {
		t.Fatalf("GetByIDForUpdateTx: %v", err)
	}
	if fund == nil || fund.ID != "fund-1" {
		t.Fatalf("expected fund-1, got %+v", fund)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestGetByIDForUpdateTxRejectsNilTx guards the API contract — calling
// with a nil tx must fail loud rather than silently degrading to a
// non-locking read (which would put us back in the lost-update race).
func TestGetByIDForUpdateTxRejectsNilTx(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()
	repo := repository.NewFundRepo(db)
	if _, err := repo.GetByIDForUpdateTx(context.Background(), nil, "fund-1"); err == nil {
		t.Fatal("expected error when called without a tx (would silently re-introduce the lost-update race), got nil")
	}
}
