package main

import (
	"context"
	"testing"
	"time"

	"github.com/fundai/server/internal/agentreputation"
)

func TestBiasedDirectionDeterministic(t *testing.T) {
	// Same salt twice -> same answer.
	a := biasedDirection("buffett-AAPL-2024", 0.5, agentreputation.DirBuy, agentreputation.DirSkip)
	b := biasedDirection("buffett-AAPL-2024", 0.5, agentreputation.DirBuy, agentreputation.DirSkip)
	if a != b {
		t.Errorf("biasedDirection not deterministic: %s vs %s", a, b)
	}
}

func TestMasterColdStartDirectionBuffettBullishOnAAPL(t *testing.T) {
	asof := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	got := masterColdStartDirection("buffett", "AAPL", asof)
	if got != agentreputation.DirBuy && got != agentreputation.DirSkip {
		t.Errorf("Buffett-AAPL expected buy/skip, got %s", got)
	}
}

func TestMasterColdStartDirectionBuffettAvoidsTSLA(t *testing.T) {
	asof := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	got := masterColdStartDirection("buffett", "TSLA", asof)
	if got != agentreputation.DirAvoid {
		t.Errorf("Buffett-TSLA expected avoid, got %s", got)
	}
}

func TestMasterColdStartDirectionWoodAvoidsBRK(t *testing.T) {
	asof := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	got := masterColdStartDirection("wood", "BRK-B", asof)
	if got != agentreputation.DirAvoid {
		t.Errorf("Wood-BRK-B expected avoid, got %s", got)
	}
}

func TestTacticColdStartDirection(t *testing.T) {
	asof := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	got := tacticColdStartDirection("tail_sniper", "600519", asof)
	if got != agentreputation.DirBuy && got != agentreputation.DirSkip {
		t.Errorf("tail_sniper-600519 expected buy/skip, got %s", got)
	}
}

func TestGenerateColdStartAsofsRespectsHorizon(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	asofs := generateColdStartAsofs(now, 36, 4)
	if len(asofs) == 0 {
		t.Fatal("expected at least one asof")
	}
	for _, a := range asofs {
		if !a.Before(now) {
			t.Errorf("asof %s must be in the past", a)
		}
		if now.Sub(a) < 30*24*time.Hour {
			t.Errorf("asof %s must be at least 30 days before now (for 21d horizon to settle)", a)
		}
	}
}

func TestRunAdvisorColdStartNoRepo(t *testing.T) {
	if _, err := runAdvisorColdStart(context.Background(), nil, nullAdvisorRealisedReturn, defaultColdStartConfig()); err == nil {
		t.Error("expected error when repo is nil")
	}
}

func TestRunAdvisorColdStartNoReturns(t *testing.T) {
	repo := &agentreputation.Repo{}
	if _, err := runAdvisorColdStart(context.Background(), repo, nil, defaultColdStartConfig()); err == nil {
		t.Error("expected error when returns lookup is nil")
	}
}

func TestRunAdvisorColdStartNoOpWhenReturnsAlwaysFail(t *testing.T) {
	repo := &agentreputation.Repo{}
	// Use the null lookup so every (symbol, asof, h) is skipped.
	n, err := runAdvisorColdStart(context.Background(), repo, nullAdvisorRealisedReturn, defaultColdStartConfig())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 outcomes when no realised data available, got %d", n)
	}
}

func TestMasterDisplayName(t *testing.T) {
	if got := masterDisplayName("buffett"); got != "Warren Buffett" {
		t.Errorf("masterDisplayName(buffett) = %q", got)
	}
	if got := masterDisplayName("UnKnOwN"); got != "UnKnOwN" {
		t.Errorf("masterDisplayName fallback failed: %q", got)
	}
}

func TestTacticDisplayName(t *testing.T) {
	if got := tacticDisplayName("tail_sniper"); got != "尾盘狙击手" {
		t.Errorf("tacticDisplayName(tail_sniper) = %q", got)
	}
}
