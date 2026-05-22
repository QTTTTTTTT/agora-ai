// Unit tests for the per-fund auto-execute guardrail engine.
//
// Scope: `runAutoExecuteGuardrails` (the pure-ish predicate that powers
// the runtimeApprovalGateway fast path), the SlippageBouncePolicy
// resolver, the plan-confidence extractor, and the config normalize /
// merge / clone trio. All tests drive the predicate with hand-built
// inputs (no DB) so the failure mode that fires becomes obvious from
// the assertion message — see autoExecuteDecision.reasonCode.
//
// The companion runtime test (TestRuntimeApprovalGatewayRequestApproval
// in main_test.go) exercises the same code path end-to-end via sqlmock.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/repository"
)

func floatPtrLocal(v float64) *float64 { return &v }

// Baseline fund + plan + actions used by most tests. Each test mutates
// only the bits it cares about, so the boilerplate stays out of the
// assertion area.
func newAutoExecuteFixture() (*repository.Fund, *repository.InvestmentPlan, []repository.PlanAction) {
	fund := &repository.Fund{
		ID:          "fund-1",
		TotalAssets: 1_000_000,
		Config:      json.RawMessage(`{}`),
	}
	plan := &repository.InvestmentPlan{
		ID:         "plan-1",
		FundID:     "fund-1",
		RiskReview: json.RawMessage(`{"confidence":0.75}`),
	}
	actions := []repository.PlanAction{{
		ID:     "action-1",
		PlanID: "plan-1",
		Symbol: "AAPL",
		Action: "buy",
		Market: sql.NullString{String: "us_equity", Valid: true},
		Amount: sql.NullFloat64{Float64: 30_000, Valid: true},
	}}
	return fund, plan, actions
}

func newGatewayWithFrozenClock() *runtimeApprovalGateway {
	frozen := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	return &runtimeApprovalGateway{
		now: func() time.Time { return frozen },
	}
}

// Happy path: every guardrail passes, decision.passed = true and the
// audit metadata is fully populated.
func TestRunAutoExecuteGuardrailsHappyPath(t *testing.T) {
	gw := newGatewayWithFrozenClock()
	fund, plan, actions := newAutoExecuteFixture()
	cfg := resolveAutoExecuteConfig(&api.FundAutoExecuteConfig{Enabled: true})

	d := gw.runAutoExecuteGuardrails(context.Background(), cfg, fund, plan, actions, fundMarketProfile{})

	if !d.passed {
		t.Fatalf("expected pass, got reasonCode=%q reason=%q", d.reasonCode, d.reason)
	}
	if d.confidence < 0.7 || d.confidence > 0.8 {
		t.Errorf("confidence = %v, want ~0.75", d.confidence)
	}
	if d.planNotional != 30_000 {
		t.Errorf("planNotional = %v, want 30000", d.planNotional)
	}
	if d.planPctNAV != 0.03 {
		t.Errorf("planPctNAV = %v, want 0.03", d.planPctNAV)
	}
}

// totalAssets = 0 → can't even evaluate percentage caps. Conservative
// path is "refuse to bypass approval".
func TestRunAutoExecuteGuardrailsNAVUnavailable(t *testing.T) {
	gw := newGatewayWithFrozenClock()
	fund, plan, actions := newAutoExecuteFixture()
	fund.TotalAssets = 0
	cfg := resolveAutoExecuteConfig(&api.FundAutoExecuteConfig{Enabled: true})

	d := gw.runAutoExecuteGuardrails(context.Background(), cfg, fund, plan, actions, fundMarketProfile{})

	if d.passed {
		t.Fatal("expected refusal when NAV is 0")
	}
	if d.reasonCode != "nav_unavailable" {
		t.Errorf("reasonCode = %q, want nav_unavailable", d.reasonCode)
	}
}

// A single action over the per-order cap kicks the whole plan back.
func TestRunAutoExecuteGuardrailsPerOrderCapExceeded(t *testing.T) {
	gw := newGatewayWithFrozenClock()
	fund, plan, actions := newAutoExecuteFixture()
	// 60k = 6% of 1M, well above 5% default
	actions[0].Amount = sql.NullFloat64{Float64: 60_000, Valid: true}
	cfg := resolveAutoExecuteConfig(&api.FundAutoExecuteConfig{Enabled: true})

	d := gw.runAutoExecuteGuardrails(context.Background(), cfg, fund, plan, actions, fundMarketProfile{})

	if d.passed {
		t.Fatal("expected refusal due to per-order cap")
	}
	if d.reasonCode != "order_pct_exceeded" {
		t.Errorf("reasonCode = %q, want order_pct_exceeded", d.reasonCode)
	}
	if !strings.Contains(d.reason, "AAPL") {
		t.Errorf("reason should name offending symbol, got %q", d.reason)
	}
}

// Watch-only / hold-only plans are deliberate PM "no-op today"
// verdicts. They should bypass the confidence floor (and every other
// capital-movement gate) entirely — gating them was a UX bug that
// caused the storage fund's watch-only plans to surface as "已驳回"
// in the Decision Center on 2026-05-22, even though the PM was
// correctly choosing to monitor instead of trade. The fast path
// returns passed=true with reasonCode="no_actionable_trade" so the
// audit JSON makes the no-op explicit and downstream WaitForDecision
// flows the plan through to completed.
func TestRunAutoExecuteGuardrailsWatchOnlyPlanPassesWithNoActionableTradeCode(t *testing.T) {
	gw := newGatewayWithFrozenClock()
	fund, plan, _ := newAutoExecuteFixture()
	// Replace the lone buy with a pair of watch / hold rows — no
	// amount, no quantity, just audit records of the PM choosing to
	// stand pat. Confidence is intentionally set BELOW the default
	// floor (0.6) to prove the watch fast-path bypasses the
	// confidence gate.
	plan.RiskReview = json.RawMessage(`{"confidence":0.4}`)
	actions := []repository.PlanAction{
		{ID: "watch-1", PlanID: "plan-1", Symbol: "MU", Action: "watch", Market: sql.NullString{String: "us_equity", Valid: true}},
		{ID: "hold-1", PlanID: "plan-1", Symbol: "SNDK", Action: "hold", Market: sql.NullString{String: "us_equity", Valid: true}},
	}
	cfg := resolveAutoExecuteConfig(&api.FundAutoExecuteConfig{Enabled: true})

	d := gw.runAutoExecuteGuardrails(context.Background(), cfg, fund, plan, actions, fundMarketProfile{})

	if !d.passed {
		t.Fatalf("watch-only plan should pass the gate, got reasonCode=%q reason=%q", d.reasonCode, d.reason)
	}
	if d.reasonCode != "no_actionable_trade" {
		t.Errorf("reasonCode = %q, want no_actionable_trade", d.reasonCode)
	}
	if !strings.Contains(d.reason, "观察") && !strings.Contains(d.reason, "watch") {
		t.Errorf("reason should explain the no-op verdict, got %q", d.reason)
	}
}

// Zero-amount buy/sell rows (e.g. quantity=0 because lot-size rounded
// down to 0) are economically the same as watch — they don't move
// any capital. Treat them as no-op so the plan doesn't get rejected
// for a confidence-floor reason that's irrelevant.
func TestRunAutoExecuteGuardrailsZeroAmountBuySkipsCapitalGates(t *testing.T) {
	gw := newGatewayWithFrozenClock()
	fund, plan, _ := newAutoExecuteFixture()
	plan.RiskReview = json.RawMessage(`{"confidence":0.4}`)
	actions := []repository.PlanAction{
		{ID: "zero-buy", PlanID: "plan-1", Symbol: "MU", Action: "buy", Market: sql.NullString{String: "us_equity", Valid: true}, Amount: sql.NullFloat64{Float64: 0, Valid: true}, Quantity: sql.NullFloat64{Float64: 0, Valid: true}},
	}
	cfg := resolveAutoExecuteConfig(&api.FundAutoExecuteConfig{Enabled: true})

	d := gw.runAutoExecuteGuardrails(context.Background(), cfg, fund, plan, actions, fundMarketProfile{})

	if !d.passed {
		t.Fatalf("zero-amount buy should be treated as no-op and pass, got reasonCode=%q reason=%q", d.reasonCode, d.reason)
	}
	if d.reasonCode != "no_actionable_trade" {
		t.Errorf("reasonCode = %q, want no_actionable_trade", d.reasonCode)
	}
}

// Gate reasons must describe the *cause* only, not the consequence —
// the consequence ("已退回人工审批" vs "已自动驳回") is added later by
// RequestApproval based on autoCfg.Enabled. Before this fix every
// gate reason hard-coded "已退回人工审批" which lied to the user
// whenever autoExecute was enabled (plan was actually rejected, not
// pending approval). This test pins the new contract so a future
// refactor doesn't quietly re-introduce the misleading suffix.
func TestRunAutoExecuteGuardrailsReasonOmitsConsequenceSuffix(t *testing.T) {
	gw := newGatewayWithFrozenClock()
	fund, plan, actions := newAutoExecuteFixture()
	plan.RiskReview = json.RawMessage(`{"confidence":0.4}`)
	cfg := resolveAutoExecuteConfig(&api.FundAutoExecuteConfig{Enabled: true})

	d := gw.runAutoExecuteGuardrails(context.Background(), cfg, fund, plan, actions, fundMarketProfile{})

	if d.passed {
		t.Fatal("expected refusal due to low confidence")
	}
	for _, forbidden := range []string{"已退回人工审批", "已自动驳回", "等待下次决策窗口"} {
		if strings.Contains(d.reason, forbidden) {
			t.Errorf("gate reason should NOT contain consequence suffix %q (RequestApproval appends it later), got %q", forbidden, d.reason)
		}
	}
}

// Confidence floor catches low-quality plans even when notional caps
// pass.
func TestRunAutoExecuteGuardrailsConfidenceFloor(t *testing.T) {
	gw := newGatewayWithFrozenClock()
	fund, plan, actions := newAutoExecuteFixture()
	plan.RiskReview = json.RawMessage(`{"confidence":0.4}`)
	cfg := resolveAutoExecuteConfig(&api.FundAutoExecuteConfig{Enabled: true})

	d := gw.runAutoExecuteGuardrails(context.Background(), cfg, fund, plan, actions, fundMarketProfile{})

	if d.passed {
		t.Fatal("expected refusal due to low confidence")
	}
	if d.reasonCode != "confidence_below_floor" {
		t.Errorf("reasonCode = %q, want confidence_below_floor", d.reasonCode)
	}
}

// Legacy plans have no confidence in risk_review — must fall back to
// action-level average instead of treating as 0 (which would always
// fail).
func TestExtractPlanConfidenceFallsBackToActionMean(t *testing.T) {
	actions := []repository.PlanAction{
		{Confidence: sql.NullFloat64{Float64: 0.8, Valid: true}},
		{Confidence: sql.NullFloat64{Float64: 0.6, Valid: true}},
	}
	// no typed column, empty risk_review → mean of action confidences
	got := extractPlanConfidence(&repository.InvestmentPlan{RiskReview: json.RawMessage(`{}`)}, actions)
	if got < 0.69 || got > 0.71 {
		t.Errorf("mean confidence = %v, want ~0.70", got)
	}

	// completely missing all sources → 0 (caller will fail the gate)
	if extractPlanConfidence(nil, nil) != 0 {
		t.Errorf("empty input should produce 0 confidence")
	}

	// typed column wins over JSON and action mean
	typed := extractPlanConfidence(&repository.InvestmentPlan{
		Confidence: sql.NullFloat64{Float64: 0.91, Valid: true},
		RiskReview: json.RawMessage(`{"confidence":0.4}`),
	}, actions)
	if typed != 0.91 {
		t.Errorf("typed column should win, got %v", typed)
	}

	// JSON wins over action mean when typed column is null
	jsonOnly := extractPlanConfidence(&repository.InvestmentPlan{
		RiskReview: json.RawMessage(`{"confidence":0.42}`),
	}, actions)
	if jsonOnly != 0.42 {
		t.Errorf("json should win when typed is null, got %v", jsonOnly)
	}
}

// AllowedMarkets is a whitelist: explicit list must include both the
// fund's market and every action's market. If any one slips through
// the plan goes back to manual.
func TestRunAutoExecuteGuardrailsMarketWhitelist(t *testing.T) {
	gw := newGatewayWithFrozenClock()
	fund, plan, actions := newAutoExecuteFixture()
	cfg := resolveAutoExecuteConfig(&api.FundAutoExecuteConfig{
		Enabled:        true,
		AllowedMarkets: []string{"a_share"}, // us_equity action will trip
	})
	profile := fundMarketProfile{Market: "us_equity"}

	d := gw.runAutoExecuteGuardrails(context.Background(), cfg, fund, plan, actions, profile)

	if d.passed {
		t.Fatal("expected refusal due to market whitelist")
	}
	if d.reasonCode != "market_not_allowed" {
		t.Errorf("reasonCode = %q, want market_not_allowed", d.reasonCode)
	}
}

// Default policy resolver returns "bounce_to_user" when fund has no
// config, and when fund has auto-execute disabled — both should never
// surprise a human-approved plan with a non-default behaviour.
func TestSlippageBouncePolicyForPlanFallbacks(t *testing.T) {
	tests := []struct {
		name    string
		fund    *repository.Fund
		actions []repository.PlanAction
		want    string
	}{
		{
			name:    "nil fund",
			fund:    nil,
			actions: nil,
			want:    "bounce_to_user",
		},
		{
			name:    "fund without auto-execute config",
			fund:    &repository.Fund{Config: json.RawMessage(`{}`)},
			actions: []repository.PlanAction{{AutoExecutedAt: sql.NullTime{Time: time.Now(), Valid: true}}},
			want:    "bounce_to_user",
		},
		{
			name: "auto-execute disabled even if actions stamped",
			fund: &repository.Fund{Config: json.RawMessage(`{"autoExecute":{"enabled":false,"slippageBouncePolicy":"reject"}}`)},
			actions: []repository.PlanAction{
				{AutoExecutedAt: sql.NullTime{Time: time.Now(), Valid: true}},
			},
			want: "bounce_to_user",
		},
		{
			name: "human-approved plan ignores fund's force_execute policy",
			fund: &repository.Fund{Config: json.RawMessage(`{"autoExecute":{"enabled":true,"slippageBouncePolicy":"force_execute"}}`)},
			actions: []repository.PlanAction{
				{AutoExecutedAt: sql.NullTime{}}, // not stamped → manual approval
			},
			want: "bounce_to_user",
		},
		{
			name: "auto-execute plan picks up fund's reject policy",
			fund: &repository.Fund{Config: json.RawMessage(`{"autoExecute":{"enabled":true,"slippageBouncePolicy":"reject"}}`)},
			actions: []repository.PlanAction{
				{AutoExecutedAt: sql.NullTime{Time: time.Now(), Valid: true}},
			},
			want: "reject",
		},
		{
			name: "auto-execute plan picks up fund's force_execute policy",
			fund: &repository.Fund{Config: json.RawMessage(`{"autoExecute":{"enabled":true,"slippageBouncePolicy":"force_execute"}}`)},
			actions: []repository.PlanAction{
				{AutoExecutedAt: sql.NullTime{Time: time.Now(), Valid: true}},
			},
			want: "force_execute",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := slippageBouncePolicyForPlan(tt.fund, tt.actions); got != tt.want {
				t.Errorf("policy = %q, want %q", got, tt.want)
			}
		})
	}
}

// normalizeFundAutoExecute: bad slippage policy strings get rewritten to
// the default (i.e. the gateway never sees an unrecognised enum).
func TestNormalizeFundAutoExecuteRewritesBadPolicy(t *testing.T) {
	cfg := normalizeFundAutoExecute(&api.FundAutoExecuteConfig{
		Enabled:              true,
		SlippageBouncePolicy: "garbage",
	})
	if cfg == nil {
		t.Fatal("expected non-nil cfg")
	}
	// expect normalize to *drop* the bad value (empty string) — the
	// resolver downstream will then substitute the default.
	if cfg.SlippageBouncePolicy != "" {
		t.Errorf("expected bad policy to be cleared, got %q", cfg.SlippageBouncePolicy)
	}
}

// normalizeFundAutoExecute: out-of-range fractions get dropped so the
// resolver can substitute defaults.
func TestNormalizeFundAutoExecuteClampsBadNumbers(t *testing.T) {
	cfg := normalizeFundAutoExecute(&api.FundAutoExecuteConfig{
		Enabled:             true,
		MaxOrderPctOfAssets: floatPtrLocal(-0.05), // negative → drop
		MaxDailyPctOfAssets: floatPtrLocal(2.0),   // > 1 → drop
		MinConfidence:       floatPtrLocal(0.85),  // valid
	})
	if cfg == nil {
		t.Fatal("expected non-nil cfg")
	}
	if cfg.MaxOrderPctOfAssets != nil {
		t.Errorf("expected negative pct to be dropped, got %v", *cfg.MaxOrderPctOfAssets)
	}
	if cfg.MaxDailyPctOfAssets != nil {
		t.Errorf("expected > 1 pct to be dropped, got %v", *cfg.MaxDailyPctOfAssets)
	}
	if cfg.MinConfidence == nil || *cfg.MinConfidence != 0.85 {
		t.Errorf("expected MinConfidence=0.85, got %v", cfg.MinConfidence)
	}
}

// mergeFundAutoExecute: enabled flag always overwrites (the toggle is
// the canonical source), but other fields only patch when non-nil.
func TestMergeFundAutoExecutePreservesUnpatched(t *testing.T) {
	existing := &api.FundAutoExecuteConfig{
		Enabled:             true,
		MaxOrderPctOfAssets: floatPtrLocal(0.07),
		MaxDailyPctOfAssets: floatPtrLocal(0.25),
		MinConfidence:       floatPtrLocal(0.7),
	}
	patch := &api.FundAutoExecuteConfig{
		Enabled:       false,
		MinConfidence: floatPtrLocal(0.5),
	}
	merged := mergeFundAutoExecute(existing, patch)
	if merged == nil {
		t.Fatal("expected merged config")
	}
	if merged.Enabled {
		t.Errorf("Enabled should be overwritten to false")
	}
	if merged.MinConfidence == nil || *merged.MinConfidence != 0.5 {
		t.Errorf("MinConfidence should be 0.5, got %v", merged.MinConfidence)
	}
	if merged.MaxOrderPctOfAssets == nil || *merged.MaxOrderPctOfAssets != 0.07 {
		t.Errorf("MaxOrderPctOfAssets should be preserved at 0.07, got %v", merged.MaxOrderPctOfAssets)
	}
}

// TestMergeFundAutoExecuteRejectsOutOfRangePatch is the contract that
// out-of-range PATCH values must NEVER clobber a previously-valid
// auto-execute guardrail. The original bug surfaced as "user PATCHes
// minConfidence:1.5 -> field silently dropped during normalize ->
// platform default 0.6 fills in -> auto-execute is now MORE permissive
// than the user's last valid setting (0.7)", which is the security-
// relevant inverse of what the user asked for.
//
// Per-field ranges (from normalizedRiskFloatPtr call sites in
// mergeFundAutoExecute): MaxOrderPctOfAssets (0, 1], MaxDailyPctOfAssets
// (0, 1], MinConfidence (0, 1]. Slippage policy: must be in
// validAutoExecuteSlippagePolicies; unknown strings ignored.
func TestMergeFundAutoExecuteRejectsOutOfRangePatch(t *testing.T) {
	existing := &api.FundAutoExecuteConfig{
		Enabled:              true,
		MaxOrderPctOfAssets:  floatPtrLocal(0.07),
		MaxDailyPctOfAssets:  floatPtrLocal(0.25),
		MinConfidence:        floatPtrLocal(0.70),
		SlippageBouncePolicy: "reject",
	}
	patch := &api.FundAutoExecuteConfig{
		Enabled:              true,
		MaxOrderPctOfAssets:  floatPtrLocal(1.50),         // out of range
		MaxDailyPctOfAssets:  floatPtrLocal(-0.10),        // out of range
		MinConfidence:        floatPtrLocal(1.50),         // out of range
		SlippageBouncePolicy: "make_user_happy_somehow",   // unknown policy
	}
	merged := mergeFundAutoExecute(existing, patch)
	if merged == nil {
		t.Fatal("merged is nil")
	}
	if merged.MaxOrderPctOfAssets == nil || *merged.MaxOrderPctOfAssets != 0.07 {
		t.Errorf("MaxOrderPctOfAssets: out-of-range 1.50 must NOT clobber existing 0.07, got %+v", merged.MaxOrderPctOfAssets)
	}
	if merged.MaxDailyPctOfAssets == nil || *merged.MaxDailyPctOfAssets != 0.25 {
		t.Errorf("MaxDailyPctOfAssets: negative patch must NOT clobber existing 0.25, got %+v", merged.MaxDailyPctOfAssets)
	}
	if merged.MinConfidence == nil || *merged.MinConfidence != 0.70 {
		t.Errorf("MinConfidence: out-of-range 1.50 must NOT clobber existing 0.70, got %+v (the security-relevant case: silently relaxing auto-execute)", merged.MinConfidence)
	}
	if merged.SlippageBouncePolicy != "reject" {
		t.Errorf("SlippageBouncePolicy: unknown policy must NOT clobber existing 'reject', got %q", merged.SlippageBouncePolicy)
	}
}

// resolveAutoExecuteConfig: a nil input returns a fully-populated
// config so the gateway code never has to nil-check guardrails.
func TestResolveAutoExecuteConfigBackfillsDefaults(t *testing.T) {
	resolved := resolveAutoExecuteConfig(nil)
	if resolved.MaxOrderPctOfAssets == nil || *resolved.MaxOrderPctOfAssets != DefaultAutoExecuteMaxOrderPctOfAssets {
		t.Errorf("MaxOrderPctOfAssets default missing")
	}
	if resolved.MaxDailyPctOfAssets == nil || *resolved.MaxDailyPctOfAssets != DefaultAutoExecuteMaxDailyPctOfAssets {
		t.Errorf("MaxDailyPctOfAssets default missing")
	}
	if resolved.MinConfidence == nil || *resolved.MinConfidence != DefaultAutoExecuteMinConfidence {
		t.Errorf("MinConfidence default missing")
	}
	if resolved.SlippageBouncePolicy != DefaultAutoExecuteSlippageBouncePolicy {
		t.Errorf("SlippageBouncePolicy default missing, got %q", resolved.SlippageBouncePolicy)
	}
	if resolved.Enabled {
		t.Errorf("Enabled must default to false for nil input")
	}
}
