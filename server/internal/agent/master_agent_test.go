package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestLoadMasterPersonas asserts that every JSON file under
// internal/agent/masters/ parses cleanly and yields a usable
// MasterPersona. Acts as a build-time guard against malformed
// persona templates — a missing field would otherwise only surface
// at the first /advisor consultation in production.
func TestLoadMasterPersonas(t *testing.T) {
	ResetMasterPersonaCache()
	personas, err := LoadMasterPersonas()
	if err != nil {
		t.Fatalf("LoadMasterPersonas: %v", err)
	}
	want := []string{
		"buffett", "munger", "graham", "lynch", "marks",
		"dalio", "oneil", "greenblatt", "wood", "druckenmiller",
	}
	for _, key := range want {
		p, ok := personas[key]
		if !ok {
			t.Errorf("missing persona %q", key)
			continue
		}
		if p.Key != key {
			t.Errorf("persona %q: key mismatch %q", key, p.Key)
		}
		if strings.TrimSpace(p.NameEn) == "" {
			t.Errorf("persona %q: empty name_en", key)
		}
		if strings.TrimSpace(p.Philosophy) == "" {
			t.Errorf("persona %q: empty philosophy", key)
		}
		if len(p.Raw) == 0 {
			t.Errorf("persona %q: empty Raw map", key)
		}
	}
}

// TestMasterAgentFallback exercises the LLM-less path: with a nil
// LLM client the agent must still return a HOLD shell report that
// passes Validate, so the panel never sees a missing row.
func TestMasterAgentFallback(t *testing.T) {
	ResetMasterPersonaCache()
	personas, err := LoadMasterPersonas()
	if err != nil {
		t.Fatalf("LoadMasterPersonas: %v", err)
	}
	persona := personas["buffett"]
	agent, err := NewMasterAgent(persona, nil)
	if err != nil {
		t.Fatalf("NewMasterAgent: %v", err)
	}
	rep, err := agent.Analyze(context.Background(), MasterInput{
		Symbol: "AAPL",
		AsOf:   time.Now(),
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if err := rep.Validate(); err != nil {
		t.Errorf("rep.Validate: %v", err)
	}
	if rep.MasterKey != "buffett" {
		t.Errorf("MasterKey = %q want buffett", rep.MasterKey)
	}
	if rep.Verdict != "HOLD" {
		t.Errorf("Verdict = %q want HOLD (fallback)", rep.Verdict)
	}
}

type retrySchemaLLM struct {
	replies []string
	errs    []error
	calls   []string
}

func (f *retrySchemaLLM) Complete(ctx context.Context, sys, user string) (string, error) {
	return f.CompleteWithSchema(ctx, sys, user, nil)
}

func (f *retrySchemaLLM) CompleteWithSchema(_ context.Context, _, user string, _ []byte) (string, error) {
	f.calls = append(f.calls, user)
	i := len(f.calls) - 1
	if i < len(f.errs) && f.errs[i] != nil {
		return "", f.errs[i]
	}
	if i < len(f.replies) {
		return f.replies[i], nil
	}
	return "", errors.New("unexpected extra call")
}

func TestMasterAgentRetriesEmptyLLMReply(t *testing.T) {
	ResetMasterPersonaCache()
	personas, err := LoadMasterPersonas()
	if err != nil {
		t.Fatalf("LoadMasterPersonas: %v", err)
	}
	llm := &retrySchemaLLM{replies: []string{
		"",
		`{"verdict":"BUY","confidence":72,"thesis":"Durable business quality supports a constructive view despite incomplete data.","key_reasons":["Strong profitability is visible","Business scale is substantial","Technical trend is supportive"],"key_risks":["Valuation may be demanding","Some long-term fields are unavailable"],"red_lines_hit":[],"master_specific":{"quality_score":8}}`,
	}}
	agent, err := NewMasterAgent(personas["munger"], llm)
	if err != nil {
		t.Fatalf("NewMasterAgent: %v", err)
	}
	rep, err := agent.Analyze(context.Background(), MasterInput{Symbol: "SLB", AsOf: testClock})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(llm.calls) != 2 {
		t.Fatalf("expected one retry after empty reply, got %d calls", len(llm.calls))
	}
	if !strings.Contains(llm.calls[1], "RETRY_INSTRUCTION") {
		t.Fatalf("retry call should alter prompt to avoid cached bad response")
	}
	if rep.LLMModel != "llm" || rep.Verdict != "BUY" {
		t.Fatalf("expected successful LLM report after retry, got model=%q verdict=%q risks=%v", rep.LLMModel, rep.Verdict, rep.KeyRisks)
	}
}

func TestParseMasterLLMToleratesStringConfidence(t *testing.T) {
	raw := `{"verdict":"AVOID","confidence":"76","thesis":"High leverage and valuation fail the persona screen.","key_reasons":["Debt is above limit","Valuation is not cheap","History is incomplete"],"key_risks":["Leverage raises downside risk","Missing data limits conviction"],"red_lines_hit":[],"master_specific":{"passes_defensive_criteria":"2/10"}}`
	parsed, err := parseMasterLLM(raw)
	if err != nil {
		t.Fatalf("parseMasterLLM: %v", err)
	}
	if parsed.Confidence != 76 {
		t.Fatalf("Confidence = %d want 76", parsed.Confidence)
	}
}

// TestMasterPanelAggregate verifies that vote aggregation collapses
// a mix of BUYs / HOLDs / AVOIDs into the expected headline + a
// sensible consensus score. We construct reports directly rather
// than running real agents — keeps the test deterministic and
// fast.
func TestMasterPanelAggregate(t *testing.T) {
	reports := []MasterReport{
		{MasterKey: "buffett", Verdict: "BUY", Confidence: 80, Thesis: "x", Symbol: "X"},
		{MasterKey: "lynch", Verdict: "BUY", Confidence: 70, Thesis: "x", Symbol: "X"},
		{MasterKey: "graham", Verdict: "HOLD", Confidence: 50, Thesis: "x", Symbol: "X"},
		{MasterKey: "marks", Verdict: "AVOID", Confidence: 60, Thesis: "x", Symbol: "X"},
	}
	agg := AggregateMasterReports(reports)
	if agg.MasterCount != 4 {
		t.Errorf("MasterCount = %d want 4", agg.MasterCount)
	}
	if agg.BuyCount != 2 {
		t.Errorf("BuyCount = %d want 2", agg.BuyCount)
	}
	if agg.AvoidCount != 1 {
		t.Errorf("AvoidCount = %d want 1", agg.AvoidCount)
	}
	// Weighted score = (1*80 + 1*70 + 0*50 - 1*60) / (80+70+50+60) = 90/260 ≈ 0.346
	// In (-0.5, 0.5) and both buy + sell present → MIXED.
	if agg.Verdict != "MIXED" && agg.Verdict != "HOLD" {
		t.Errorf("Verdict = %q want MIXED or HOLD", agg.Verdict)
	}
	if agg.Consensus < 0 || agg.Consensus > 100 {
		t.Errorf("Consensus = %.2f out of [0,100]", agg.Consensus)
	}
}

// TestMasterPanelStrongBuy: when every master agrees BUY/STRONG_BUY,
// the aggregate should be STRONG_BUY or BUY with high consensus.
func TestMasterPanelStrongBuy(t *testing.T) {
	reports := []MasterReport{
		{MasterKey: "buffett", Verdict: "STRONG_BUY", Confidence: 90, Thesis: "x", Symbol: "X"},
		{MasterKey: "lynch", Verdict: "BUY", Confidence: 85, Thesis: "x", Symbol: "X"},
		{MasterKey: "graham", Verdict: "BUY", Confidence: 80, Thesis: "x", Symbol: "X"},
	}
	agg := AggregateMasterReports(reports)
	if agg.Verdict != "STRONG_BUY" && agg.Verdict != "BUY" {
		t.Errorf("Verdict = %q want STRONG_BUY or BUY", agg.Verdict)
	}
	if agg.Consensus < 50 {
		t.Errorf("Consensus = %.2f want >= 50 for unanimous bullish", agg.Consensus)
	}
}

// TestPersonaPresetSeedIntegrity makes sure every persona key
// referenced by Phase 0 seeds in migration 098 actually exists in
// the embed FS. Mirrors the seed list — keep them in sync.
func TestPersonaPresetSeedIntegrity(t *testing.T) {
	ResetMasterPersonaCache()
	personas, err := LoadMasterPersonas()
	if err != nil {
		t.Fatalf("LoadMasterPersonas: %v", err)
	}
	seedRefs := []string{
		"buffett", "munger", "graham", "lynch", "oneil",
		"greenblatt", "wood", "marks", "dalio", "druckenmiller",
	}
	for _, key := range seedRefs {
		if _, ok := personas[key]; !ok {
			t.Errorf("migration 098 references master %q but no JSON file exists", key)
		}
	}
}
