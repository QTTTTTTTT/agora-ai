package main

import (
	"context"
	"testing"
	"time"

	"github.com/fundai/server/internal/advisor"
	"github.com/fundai/server/internal/agentreputation"
)

func TestMasterVerdictToDirection(t *testing.T) {
	tests := []struct {
		in   string
		want agentreputation.Direction
	}{
		{"STRONG_BUY", agentreputation.DirBuy},
		{"buy", agentreputation.DirBuy},
		{"AVOID", agentreputation.DirAvoid},
		{"short", agentreputation.DirAvoid},
		{"hold", agentreputation.DirSkip},
		{"PASS", agentreputation.DirSkip},
		{"SKIP", agentreputation.DirSkip},
		{"", ""},
		{"GARBAGE", ""},
	}
	for _, tc := range tests {
		if got := masterVerdictToDirection(tc.in); got != tc.want {
			t.Errorf("masterVerdictToDirection(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTacticVerdictToDirection(t *testing.T) {
	tests := []struct {
		in   string
		want agentreputation.Direction
	}{
		{"BUY_TAIL", agentreputation.DirBuy},
		{"buy_dip", agentreputation.DirBuy},
		{"CHASE_LIMIT_UP", agentreputation.DirBuy},
		{"BUY_PULLBACK", agentreputation.DirBuy},
		{"WAIT_FOR_WINDOW", agentreputation.DirWait},
		{"WAIT_FOR_CONFIRMATION", agentreputation.DirWait},
		{"skip", agentreputation.DirSkip},
		{"", ""},
		{"UNKNOWN", ""},
	}
	for _, tc := range tests {
		if got := tacticVerdictToDirection(tc.in); got != tc.want {
			t.Errorf("tacticVerdictToDirection(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAdvisorBenchmarkForMarket(t *testing.T) {
	tests := map[string]string{
		"us":     "SPY",
		"nasdaq": "SPY",
		"cn":     "000300",
		"sh":     "000300",
		"hk":     "2800",
		"xx":     "",
	}
	for in, want := range tests {
		if got := advisorBenchmarkForMarket(in); got != want {
			t.Errorf("advisorBenchmarkForMarket(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGuessMarketFromSymbol(t *testing.T) {
	tests := map[string]string{
		"600519": "cn",
		"000001": "cn",
		"AAPL":   "us",
		"":       "us",
		"12345":  "us", // not 6 digits
		"60051a": "us", // not all digits
	}
	for in, want := range tests {
		if got := guessMarketFromSymbol(in); got != want {
			t.Errorf("guessMarketFromSymbol(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildMasterOutcomeSkipsUnknownVerdict(t *testing.T) {
	c := advisor.ReputationConsultation{
		ID:        "abc-123",
		Symbol:    "AAPL",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	m := advisor.MasterReportRow{
		MasterKey: "buffett",
		Verdict:   "MAYBE",
	}
	if out := buildMasterOutcome(c, m, 5, 0.05, 0.01); out != nil {
		t.Fatalf("expected nil for unknown verdict, got %+v", out)
	}
}

func TestBuildMasterOutcomeWritesAdvisorRow(t *testing.T) {
	c := advisor.ReputationConsultation{
		ID:        "abc-123",
		Symbol:    "AAPL",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	m := advisor.MasterReportRow{
		MasterKey:  "buffett",
		Verdict:    "BUY",
		Confidence: 78,
	}
	out := buildMasterOutcome(c, m, 5, 0.05, 0.01)
	if out == nil {
		t.Fatal("expected outcome, got nil")
	}
	if out.FundID != "" {
		t.Errorf("advisor outcome must carry empty FundID, got %q", out.FundID)
	}
	if out.AgentID != "master:buffett" {
		t.Errorf("AgentID = %q, want master:buffett", out.AgentID)
	}
	if out.AgentKind != agentreputation.KindMaster {
		t.Errorf("AgentKind = %q, want master", out.AgentKind)
	}
	if out.Direction != agentreputation.DirBuy {
		t.Errorf("Direction = %q, want buy", out.Direction)
	}
	if got := out.Alpha; got <= 0 {
		t.Errorf("Alpha = %v, want positive (realised 0.05 - benchmark 0.01)", got)
	}
	if err := out.Validate(); err != nil {
		t.Errorf("Outcome.Validate(): %v", err)
	}
}

func TestBuildTacticOutcomeWritesAdvisorRow(t *testing.T) {
	c := advisor.ReputationConsultation{
		ID:        "xyz-456",
		Symbol:    "600519",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	tk := advisor.TacticReportRow{
		TacticKey:  "tail_sniper",
		Verdict:    "BUY_TAIL",
		Confidence: 60,
	}
	out := buildTacticOutcome(c, tk, 1, 0.03, 0.005)
	if out == nil {
		t.Fatal("expected outcome, got nil")
	}
	if out.AgentID != "tactic:tail_sniper" {
		t.Errorf("AgentID = %q, want tactic:tail_sniper", out.AgentID)
	}
	if out.AgentKind != agentreputation.KindTactic {
		t.Errorf("AgentKind = %q, want tactic", out.AgentKind)
	}
	if out.Direction != agentreputation.DirBuy {
		t.Errorf("Direction = %q, want buy", out.Direction)
	}
	if err := out.Validate(); err != nil {
		t.Errorf("Outcome.Validate(): %v", err)
	}
}

func TestNullAdvisorRealisedReturn(t *testing.T) {
	r, b, ok, err := nullAdvisorRealisedReturn(context.Background(), "us", "AAPL", time.Now(), 5)
	if err != nil || ok || r != 0 || b != 0 {
		t.Errorf("nullAdvisorRealisedReturn returned (%v,%v,%v,%v); want all zero/false/nil", r, b, ok, err)
	}
}
