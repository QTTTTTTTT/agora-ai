package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/contradiction"
)

func TestBuildSemanticRecallQueryTextCombinesMacroAndUniverse(t *testing.T) {
	macro := "Market is risk-on after CPI miss."
	universe := []string{"AAPL", "MSFT", "NVDA"}
	out := buildSemanticRecallQueryText(macro, universe)
	if !strings.Contains(out, "risk-on") {
		t.Fatalf("expected macro substring, got %q", out)
	}
	if !strings.Contains(out, "Universe: AAPL, MSFT, NVDA") {
		t.Fatalf("expected universe line, got %q", out)
	}
}

func TestBuildSemanticRecallQueryTextTruncatesLongMacro(t *testing.T) {
	macro := strings.Repeat("x", 1000)
	out := buildSemanticRecallQueryText(macro, nil)
	if len(out) > 700 {
		t.Fatalf("expected truncated macro, got len %d", len(out))
	}
}

func TestBuildSemanticRecallQueryTextEmpty(t *testing.T) {
	if got := buildSemanticRecallQueryText("   ", nil); got != "" {
		t.Fatalf("expected blank, got %q", got)
	}
}

func TestBuildContradictionViewsDropsBlanks(t *testing.T) {
	views := buildContradictionViews("BUY AAPL strong", "", "neutral on macro", "long")
	if len(views) != 2 {
		t.Fatalf("expected 2 views, got %d", len(views))
	}
	if views[0].Role != "bull" || views[1].Role != "quant" {
		t.Fatalf("roles: %+v", views)
	}
}

func TestBuildContradictionViewsReturnsEmptyWhenAllBlank(t *testing.T) {
	views := buildContradictionViews("", "", "", "")
	if len(views) != 0 {
		t.Fatalf("expected zero views, got %d", len(views))
	}
}

func TestContradictionRunnerDisable(t *testing.T) {
	r := newContradictionRunner(nil)
	r.Disable()
	out := r.Check(context.Background(), "f", time.Now(), nil, "", "", []contradiction.ResearcherView{
		{Role: "bull", Body: "x"},
		{Role: "bear", Body: "y"},
	}, "u", "a")
	if out != nil {
		t.Fatalf("disabled runner should return nil, got %v", out)
	}
}

func TestContradictionRunnerSkipsSingleResearcher(t *testing.T) {
	r := newContradictionRunner(nil)
	out := r.Check(context.Background(), "f", time.Now(), nil, "", "", []contradiction.ResearcherView{
		{Role: "bull", Body: "x"},
	}, "u", "a")
	if out != nil {
		t.Fatalf("single-researcher should return nil, got %v", out)
	}
}
