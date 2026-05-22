package main

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/debate"
	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/workflow"
)

// stubDebateRoundtable lets the wiring tests control what
// runDebateRoundtable returns without spinning up an LLM.
type stubDebateRoundtable struct {
	out   *debate.RoundtableOutput
	err   error
	calls int
	last  debate.DebateInput
}

func (s *stubDebateRoundtable) Run(_ context.Context, input debate.DebateInput) (*debate.RoundtableOutput, error) {
	s.calls++
	s.last = input
	return s.out, s.err
}

func expectFundRow(mock sqlmock.Sqlmock, fundID, config string) {
	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
			 FROM funds WHERE id = $1`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow(fundID, "company-1", "Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(config), now, now))
}

// When fund.config.researchTier == "advanced" and a debate engine is
// wired, Roundtable must call the engine and surface its structured
// output back to the workflow.
func TestRoundtableRunsDebateWhenFundOptsIn(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	// The gate calls fundRepo.GetByID once, runDebateRoundtable
	// calls it again to read the universe.
	cfg := `{"market":"us_equity","researchTier":"advanced","universe":{"symbols":["AAPL","NVDA"]}}`
	expectFundRow(mock, "fund-1", cfg)
	expectFundRow(mock, "fund-1", cfg)

	stub := &stubDebateRoundtable{out: &debate.RoundtableOutput{
		Rounds:        2,
		OverallStance: "net long, defensive",
		BullCase:      "earnings momentum",
		BearCase:      "valuation stretched",
		QuantCase:     "trend strong",
		Symbols: []debate.SymbolDebate{
			{Symbol: "AAPL", Verdict: "bull", BullCase: "iPhone cycle", BearCase: "macro headwinds", QuantCase: "uptrend", DissentVotes: 1},
		},
	}}

	pool := runtimeResearcherPool{
		fundRepo:         repository.NewFundRepo(db),
		debateRoundtable: stub,
	}
	result, err := pool.Roundtable(context.Background(), "fund-1", []workflow.ResearchReport{
		{Focus: workflow.FocusMacro, Content: "macro brief"},
		{Focus: workflow.FocusStock, Content: "AAPL: strong cycle"},
	}, 2)
	if err != nil {
		t.Fatalf("Roundtable: %v", err)
	}
	if stub.calls != 1 {
		t.Errorf("expected debate engine to be called once, got %d", stub.calls)
	}
	if result.OverallStance != "net long, defensive" {
		t.Errorf("OverallStance = %q, want 'net long, defensive'", result.OverallStance)
	}
	if len(result.Symbols) != 1 || result.Symbols[0].Verdict != "bull" {
		t.Errorf("expected 1 symbol verdict bull, got %+v", result.Symbols)
	}
	if result.Symbols[0].DissentVotes != 1 {
		t.Errorf("DissentVotes = %d, want 1", result.Symbols[0].DissentVotes)
	}
	// Consensus must contain the role one-liners so legacy
	// consumers see something useful, plus the per-symbol verdict.
	gotBull := false
	gotVerdict := false
	for _, line := range result.Consensus {
		if line == "BULL: earnings momentum" {
			gotBull = true
		}
		if line == "AAPL → bull (dissent=1)" {
			gotVerdict = true
		}
	}
	if !gotBull {
		t.Errorf("Consensus should contain 'BULL: ...' line, got %v", result.Consensus)
	}
	if !gotVerdict {
		t.Errorf("Consensus should contain per-symbol verdict, got %v", result.Consensus)
	}
	// Input forwarded to the engine should include the universe symbols.
	if len(stub.last.Universe) != 2 {
		t.Errorf("debate input universe = %v, want [AAPL, NVDA]", stub.last.Universe)
	}
	if stub.last.MacroBrief != "macro brief" {
		t.Errorf("MacroBrief forwarded = %q, want 'macro brief'", stub.last.MacroBrief)
	}
	if len(stub.last.StockReports) != 1 {
		t.Errorf("StockReports forwarded = %v", stub.last.StockReports)
	}

	assertMockExpectations(t, mock)
}

// When the fund stays on the default tier ("standard"/empty), the
// debate engine must NOT be consulted — the legacy text-concat path
// is the cheap default.
func TestRoundtableSkipsDebateForStandardTier(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	expectFundRow(mock, "fund-2", `{"market":"a_share","universe":{"symbols":["600519"]}}`)

	stub := &stubDebateRoundtable{}
	pool := runtimeResearcherPool{
		fundRepo:         repository.NewFundRepo(db),
		debateRoundtable: stub,
	}
	result, err := pool.Roundtable(context.Background(), "fund-2", []workflow.ResearchReport{
		{Focus: workflow.FocusMacro, Content: "macro a"},
		{Focus: workflow.FocusStock, Content: "茅台 ok"},
	}, 1)
	if err != nil {
		t.Fatalf("Roundtable: %v", err)
	}
	if stub.calls != 0 {
		t.Errorf("debate engine must NOT be called for standard tier, got %d calls", stub.calls)
	}
	if len(result.Consensus) != 2 {
		t.Errorf("legacy consensus should pass reports through, got %v", result.Consensus)
	}
	if result.OverallStance != "" {
		t.Errorf("legacy path must leave OverallStance empty, got %q", result.OverallStance)
	}
	assertMockExpectations(t, mock)
}

// When the debate engine errors, the pool must fall back to the
// legacy text-concat consensus instead of bubbling the error up.
// This is the "best-effort" contract documented in shouldRunDebate.
func TestRoundtableFallsBackWhenDebateErrors(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	cfg := `{"market":"us_equity","researchTier":"advanced","universe":{"symbols":["AAPL"]}}`
	expectFundRow(mock, "fund-3", cfg)
	expectFundRow(mock, "fund-3", cfg)

	stub := &stubDebateRoundtable{err: errors.New("LLM rate limited")}
	pool := runtimeResearcherPool{
		fundRepo:         repository.NewFundRepo(db),
		debateRoundtable: stub,
	}
	result, err := pool.Roundtable(context.Background(), "fund-3", []workflow.ResearchReport{
		{Focus: workflow.FocusMacro, Content: "macro brief"},
	}, 2)
	if err != nil {
		t.Fatalf("Roundtable should fall back not error: %v", err)
	}
	if stub.calls != 1 {
		t.Errorf("expected one attempt before fallback, got %d", stub.calls)
	}
	if len(result.Consensus) != 1 || result.Consensus[0] != "macro brief" {
		t.Errorf("fallback should produce legacy consensus, got %v", result.Consensus)
	}
	if result.OverallStance != "" {
		t.Errorf("legacy fallback must NOT populate OverallStance, got %q", result.OverallStance)
	}
	assertMockExpectations(t, mock)
}

// With no debate engine wired, Roundtable behaves exactly like the
// pre-Phase-2B implementation (no DB roundtrip needed because
// shouldRunDebate short-circuits on nil engine).
func TestRoundtableSkipsDebateWhenEngineNil(t *testing.T) {
	db, _ := newMockDB(t)
	defer db.Close()

	pool := runtimeResearcherPool{
		fundRepo:         repository.NewFundRepo(db),
		debateRoundtable: nil,
	}
	result, err := pool.Roundtable(context.Background(), "fund-4", []workflow.ResearchReport{
		{Focus: workflow.FocusMacro, Content: "x"},
	}, 1)
	if err != nil {
		t.Fatalf("Roundtable: %v", err)
	}
	if len(result.Consensus) != 1 || result.Consensus[0] != "x" {
		t.Errorf("legacy consensus = %v", result.Consensus)
	}
}

// debateForceEnabled overrides the per-fund tier so a fleet-wide
// canary can route ALL funds through debate even when nobody opted
// in.
func TestRoundtableForceEnabledBypassesPerFundTier(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	// shouldRunDebate hits the env flag first; with forceEnabled
	// true the fundRepo lookup for the gate is skipped, but
	// runDebateRoundtable still needs the fund row to read
	// universe / market.
	cfg := `{"market":"crypto","universe":{"symbols":["BTC-USD"]}}`
	expectFundRow(mock, "fund-5", cfg)

	stub := &stubDebateRoundtable{out: &debate.RoundtableOutput{Rounds: 1, OverallStance: "neutral"}}
	pool := runtimeResearcherPool{
		fundRepo:           repository.NewFundRepo(db),
		debateRoundtable:   stub,
		debateForceEnabled: true,
	}
	result, err := pool.Roundtable(context.Background(), "fund-5", nil, 1)
	if err != nil {
		t.Fatalf("Roundtable: %v", err)
	}
	if stub.calls != 1 {
		t.Errorf("force-enabled debate should run, got %d calls", stub.calls)
	}
	if result.OverallStance != "neutral" {
		t.Errorf("OverallStance = %q, want 'neutral'", result.OverallStance)
	}
	assertMockExpectations(t, mock)
}

// normalizeFundResearchTier collapses anything outside the
// "advanced" / "standard" pair to "standard" so a typo can never
// silently flip the gate on.
func TestNormalizeFundResearchTierCollapsesUnknownValues(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"advanced", "advanced"},
		{"ADVANCED", "advanced"},
		{" Advanced ", "advanced"},
		{"standard", "standard"},
		{"", "standard"},
		{"deep", "standard"},
		{"experimental", "standard"},
		{"  ", "standard"},
	}
	for _, c := range cases {
		got := normalizeFundResearchTier(c.in)
		if got != c.want {
			t.Errorf("normalizeFundResearchTier(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// debateForceEnabledFromEnv recognizes the truthy strings
// documented in the function. Anything else stays off.
func TestDebateForceEnabledFromEnv(t *testing.T) {
	for _, on := range []string{"1", "true", "TRUE", "yes", "On"} {
		t.Setenv("FUND_DEBATE_ROUNDTABLE", on)
		if !debateForceEnabledFromEnv() {
			t.Errorf("expected truthy for %q", on)
		}
	}
	for _, off := range []string{"", "0", "false", "no", "off", "weird"} {
		t.Setenv("FUND_DEBATE_ROUNDTABLE", off)
		if debateForceEnabledFromEnv() {
			t.Errorf("expected falsy for %q", off)
		}
	}
}
