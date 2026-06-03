package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/fundai/server/internal/decision"
	"github.com/fundai/server/internal/decision/errorclass"
)

// Sprint 11.2 — the engine-shape → decision_source mapping is the
// single source of truth for what the persisted tag will look like.
// Pin the contract so a future refactor of the engine struct hierarchy
// doesn't silently downgrade three-stage rows to llm_pm.
func TestLLMDecisionSourceFor_PinsKnownShapes(t *testing.T) {
	cases := []struct {
		name   string
		engine decision.DecisionEngine
		want   string
	}{
		{
			name:   "three_stage",
			engine: &decision.ThreeStageEngine{},
			want:   "llm_three_stage",
		},
		{
			name:   "llm_single_shot",
			engine: &decision.LLMDecisionEngine{},
			want:   "llm_pm",
		},
		{
			name:   "fallback_engine",
			engine: decision.FallbackEngine{},
			want:   "fallback_no_llm",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := llmDecisionSourceFor(c.engine); got != c.want {
				t.Fatalf("expected %s, got %s", c.want, got)
			}
		})
	}
}

func TestRecordAndConsumeDecisionSource_RoundTrip(t *testing.T) {
	a := &runtimePMAgent{}

	a.recordDecisionSource("fund-A", "llm_pm", errorclass.Detail{})
	rec, ok := a.consumeDecisionSource("fund-A")
	if !ok {
		t.Fatalf("expected record after store")
	}
	if rec.Source != "llm_pm" {
		t.Fatalf("expected source=llm_pm, got %q", rec.Source)
	}
	if rec.ReasonJSON != nil {
		t.Fatalf("expected nil reason for zero Detail, got %s", string(rec.ReasonJSON))
	}

	// Consume is delete-on-read.
	if _, ok := a.consumeDecisionSource("fund-A"); ok {
		t.Fatalf("expected consume to delete record")
	}
}

func TestRecordDecisionSource_WithReasonRoundTrips(t *testing.T) {
	a := &runtimePMAgent{}
	detail := errorclass.Detail{
		Category: errorclass.CategoryRateLimited,
		Provider: "claude",
		Model:    "claude-opus-4",
		Summary:  "rate-limited at 429",
		At:       time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC),
	}
	a.recordDecisionSource("fund-B", "fallback_after_llm_error", detail)
	rec, ok := a.consumeDecisionSource("fund-B")
	if !ok {
		t.Fatalf("expected record")
	}
	if rec.Source != "fallback_after_llm_error" {
		t.Fatalf("expected fallback source, got %q", rec.Source)
	}
	var decoded errorclass.Detail
	if err := json.Unmarshal(rec.ReasonJSON, &decoded); err != nil {
		t.Fatalf("reason json unmarshal: %v", err)
	}
	if decoded.Category != errorclass.CategoryRateLimited {
		t.Fatalf("expected category=rate_limited, got %s", decoded.Category)
	}
	if decoded.Provider != "claude" || decoded.Model != "claude-opus-4" {
		t.Fatalf("expected provider/model preserved, got %+v", decoded)
	}
}

func TestRecordDecisionSource_NilSafety(t *testing.T) {
	var a *runtimePMAgent
	// Must not panic on nil receiver / empty args.
	a.recordDecisionSource("fund-X", "llm_pm", errorclass.Detail{})
	if _, ok := a.consumeDecisionSource("fund-X"); ok {
		t.Fatalf("expected ok=false from nil receiver")
	}
	a2 := &runtimePMAgent{}
	a2.recordDecisionSource("", "llm_pm", errorclass.Detail{})
	if _, ok := a2.consumeDecisionSource(""); ok {
		t.Fatalf("expected ok=false for empty fund id")
	}
}

func TestConsumeDecisionSource_Empty(t *testing.T) {
	a := &runtimePMAgent{}
	if _, ok := a.consumeDecisionSource("never-stored"); ok {
		t.Fatalf("expected ok=false when nothing was stored")
	}
}
