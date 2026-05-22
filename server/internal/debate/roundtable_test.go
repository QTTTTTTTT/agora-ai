package debate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/llm"
)

// stubResearcher is a Researcher whose Debate output is canned per
// round. Lets us exercise the orchestrator without an LLM.
type stubResearcher struct {
	role     AgentRole
	rounds   []*AgentView
	errs     []error
	rounds_  int // call counter
	seenPeer [][]AgentView
}

func (s *stubResearcher) Role() AgentRole { return s.role }

func (s *stubResearcher) Debate(_ context.Context, _ DebateInput, round int, peers []AgentView) (*AgentView, error) {
	s.seenPeer = append(s.seenPeer, append([]AgentView(nil), peers...))
	defer func() { s.rounds_++ }()
	if s.errs != nil && round < len(s.errs) && s.errs[round] != nil {
		return nil, s.errs[round]
	}
	if round >= len(s.rounds) {
		return nil, errors.New("stub: no canned view for round")
	}
	view := *s.rounds[round]
	view.Round = round
	view.Role = s.role
	return &view, nil
}

// Three-agent debate converges in round 1 when both rounds produce
// effectively identical views. Verifies the orchestrator stops early
// and exposes ConvergedRounds.
func TestRoundtableConvergesEarlyWhenViewsStable(t *testing.T) {
	bull := &stubResearcher{role: RoleBull, rounds: []*AgentView{
		{Stance: "buy AAPL", Verdicts: []SymbolVerdict{{Symbol: "AAPL", Direction: "bull", Confidence: 0.8, KeyPoints: []string{"earnings momentum"}}}, Confidence: 0.8},
		{Stance: "buy AAPL", Verdicts: []SymbolVerdict{{Symbol: "AAPL", Direction: "bull", Confidence: 0.8, KeyPoints: []string{"earnings momentum"}}}, Confidence: 0.8},
		{Stance: "buy AAPL", Verdicts: []SymbolVerdict{{Symbol: "AAPL", Direction: "bull", Confidence: 0.8, KeyPoints: []string{"earnings momentum"}}}, Confidence: 0.8},
	}}
	bear := &stubResearcher{role: RoleBear, rounds: []*AgentView{
		{Stance: "fade pop", Verdicts: []SymbolVerdict{{Symbol: "AAPL", Direction: "bear", Confidence: 0.6, KeyPoints: []string{"crowded long"}}}, Confidence: 0.6},
		{Stance: "fade pop", Verdicts: []SymbolVerdict{{Symbol: "AAPL", Direction: "bear", Confidence: 0.6, KeyPoints: []string{"crowded long"}}}, Confidence: 0.6},
		{Stance: "fade pop", Verdicts: []SymbolVerdict{{Symbol: "AAPL", Direction: "bear", Confidence: 0.6, KeyPoints: []string{"crowded long"}}}, Confidence: 0.6},
	}}
	quant := &stubResearcher{role: RoleQuant, rounds: []*AgentView{
		{Stance: "mixed signals", Verdicts: []SymbolVerdict{{Symbol: "AAPL", Direction: "neutral", Confidence: 0.55, KeyPoints: []string{"MACD flat"}}}, Confidence: 0.55},
		{Stance: "mixed signals", Verdicts: []SymbolVerdict{{Symbol: "AAPL", Direction: "neutral", Confidence: 0.55, KeyPoints: []string{"MACD flat"}}}, Confidence: 0.55},
		{Stance: "mixed signals", Verdicts: []SymbolVerdict{{Symbol: "AAPL", Direction: "neutral", Confidence: 0.55, KeyPoints: []string{"MACD flat"}}}, Confidence: 0.55},
	}}

	rt := &LLMRoundtable{Researchers: []Researcher{bull, bear, quant}}
	out, err := rt.Run(context.Background(), DebateInput{
		Universe:  []string{"AAPL"},
		MaxRounds: 3,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Converged {
		t.Errorf("expected converged=true when rounds are identical, got false")
	}
	if out.ConvergedRounds != 2 {
		t.Errorf("expected to converge after round 2 (round 0 + round 1 identical), got %d", out.ConvergedRounds)
	}
	if len(out.Symbols) != 1 {
		t.Fatalf("expected 1 symbol entry, got %d", len(out.Symbols))
	}
}

// Majority vote resolves the verdict; quant breaks ties. The bull
// and bear disagree, but quant says neutral → expect neutral with
// dissent=2 (bull+bear both vote against neutral).
func TestRoundtableResolvesSymbolVerdictByMajorityWithQuantTiebreak(t *testing.T) {
	bull := &stubResearcher{role: RoleBull, rounds: []*AgentView{
		{Verdicts: []SymbolVerdict{{Symbol: "NVDA", Direction: "bull", Confidence: 0.8, KeyPoints: []string{"AI capex"}}}},
	}}
	bear := &stubResearcher{role: RoleBear, rounds: []*AgentView{
		{Verdicts: []SymbolVerdict{{Symbol: "NVDA", Direction: "bear", Confidence: 0.7, KeyPoints: []string{"valuation stretched"}}}},
	}}
	quant := &stubResearcher{role: RoleQuant, rounds: []*AgentView{
		{Verdicts: []SymbolVerdict{{Symbol: "NVDA", Direction: "neutral", Confidence: 0.6, KeyPoints: []string{"trend extended"}}}},
	}}

	rt := &LLMRoundtable{Researchers: []Researcher{bull, bear, quant}}
	out, err := rt.Run(context.Background(), DebateInput{Universe: []string{"NVDA"}, MaxRounds: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out.Symbols) != 1 {
		t.Fatalf("expected 1 symbol, got %d", len(out.Symbols))
	}
	sd := out.Symbols[0]
	if sd.Verdict != "neutral" {
		t.Errorf("Verdict = %q, want neutral (3-way tie → quant breaks)", sd.Verdict)
	}
	if sd.DissentVotes != 2 {
		t.Errorf("DissentVotes = %d, want 2 (bull + bear vote against quant)", sd.DissentVotes)
	}
	if sd.BullCase != "AI capex" {
		t.Errorf("BullCase = %q, want 'AI capex'", sd.BullCase)
	}
	if sd.BearCase != "valuation stretched" {
		t.Errorf("BearCase = %q, want 'valuation stretched'", sd.BearCase)
	}
	if sd.QuantCase != "trend extended" {
		t.Errorf("QuantCase = %q, want 'trend extended'", sd.QuantCase)
	}
}

// A 2-vs-1 majority (bull + quant agree → bull wins, bear dissents).
func TestRoundtableTwoVotesWinMajority(t *testing.T) {
	bull := &stubResearcher{role: RoleBull, rounds: []*AgentView{
		{Verdicts: []SymbolVerdict{{Symbol: "AAPL", Direction: "bull", Confidence: 0.8}}},
	}}
	bear := &stubResearcher{role: RoleBear, rounds: []*AgentView{
		{Verdicts: []SymbolVerdict{{Symbol: "AAPL", Direction: "bear", Confidence: 0.7}}},
	}}
	quant := &stubResearcher{role: RoleQuant, rounds: []*AgentView{
		{Verdicts: []SymbolVerdict{{Symbol: "AAPL", Direction: "bull", Confidence: 0.7}}},
	}}
	rt := &LLMRoundtable{Researchers: []Researcher{bull, bear, quant}}
	out, _ := rt.Run(context.Background(), DebateInput{Universe: []string{"AAPL"}, MaxRounds: 1})
	if out.Symbols[0].Verdict != "bull" {
		t.Errorf("Verdict = %q, want bull (2 votes vs 1)", out.Symbols[0].Verdict)
	}
	if out.Symbols[0].DissentVotes != 1 {
		t.Errorf("DissentVotes = %d, want 1", out.Symbols[0].DissentVotes)
	}
}

// When one researcher errors on every round, the orchestrator must
// still emit a best-effort RoundtableOutput from the survivors. The
// failed role contributes no per-symbol case; the resolved verdict
// is whatever the surviving roles tally to.
func TestRoundtableTolerantOfSinglePersonaFailure(t *testing.T) {
	bull := &stubResearcher{role: RoleBull, rounds: []*AgentView{
		{Stance: "buy MSFT cloud", Verdicts: []SymbolVerdict{{Symbol: "MSFT", Direction: "bull", Confidence: 0.7, KeyPoints: []string{"azure growth"}}}},
	}}
	bear := &stubResearcher{role: RoleBear, errs: []error{errors.New("provider down")}}
	quant := &stubResearcher{role: RoleQuant, rounds: []*AgentView{
		{Stance: "trend ok", Verdicts: []SymbolVerdict{{Symbol: "MSFT", Direction: "neutral", Confidence: 0.6, KeyPoints: []string{"trend ok"}}}},
	}}
	rt := &LLMRoundtable{Researchers: []Researcher{bull, bear, quant}}
	out, err := rt.Run(context.Background(), DebateInput{Universe: []string{"MSFT"}, MaxRounds: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.BullCase == "" || out.QuantCase == "" {
		t.Errorf("survivors should still populate role cases, got %+v", out)
	}
	if out.BearCase != "" {
		t.Errorf("BearCase must be empty when bear failed, got %q", out.BearCase)
	}
	if out.Symbols[0].Verdict == "" {
		t.Errorf("symbol verdict should still resolve when 2/3 researchers reported")
	}
	if out.Symbols[0].BullCase != "azure growth" {
		t.Errorf("bull keyPoints should still aggregate; got %q", out.Symbols[0].BullCase)
	}
	if out.Symbols[0].BearCase != "" {
		t.Errorf("bear case must be empty when bear failed, got %q", out.Symbols[0].BearCase)
	}
}

// Bull/Bear/Quant all error → orchestrator returns an error so the
// caller can fall back to the legacy text-concat path.
func TestRoundtableErrorsWhenAllPersonasFail(t *testing.T) {
	makeFailing := func(role AgentRole) *stubResearcher {
		return &stubResearcher{role: role, errs: []error{errors.New("rate limited"), errors.New("rate limited")}}
	}
	rt := &LLMRoundtable{Researchers: []Researcher{makeFailing(RoleBull), makeFailing(RoleBear), makeFailing(RoleQuant)}}
	if _, err := rt.Run(context.Background(), DebateInput{Universe: []string{"AAPL"}, MaxRounds: 2}); err == nil {
		t.Fatal("expected error when every persona fails every round")
	}
}

// Peer views from round 0 must be fed into round 1's input so each
// agent can rebut the others. Verifies serializePeerViews drops the
// caller's own role.
func TestRoundtableFeedsPeerViewsBetweenRounds(t *testing.T) {
	bull := &stubResearcher{role: RoleBull, rounds: []*AgentView{
		{Stance: "round0 bull", Verdicts: []SymbolVerdict{{Symbol: "AAPL", Direction: "bull", Confidence: 0.8, KeyPoints: []string{"a"}}}},
		{Stance: "round1 bull", Verdicts: []SymbolVerdict{{Symbol: "AAPL", Direction: "neutral", Confidence: 0.5, KeyPoints: []string{"b"}}}},
	}}
	bear := &stubResearcher{role: RoleBear, rounds: []*AgentView{
		{Stance: "round0 bear", Verdicts: []SymbolVerdict{{Symbol: "AAPL", Direction: "bear", Confidence: 0.7, KeyPoints: []string{"x"}}}},
		{Stance: "round1 bear", Verdicts: []SymbolVerdict{{Symbol: "AAPL", Direction: "bear", Confidence: 0.7, KeyPoints: []string{"x"}}}},
	}}

	rt := &LLMRoundtable{Researchers: []Researcher{bull, bear}}
	_, err := rt.Run(context.Background(), DebateInput{Universe: []string{"AAPL"}, MaxRounds: 2, ConvergenceThreshold: 1.5})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(bull.seenPeer) < 2 {
		t.Fatalf("bull should have run at least 2 rounds, got %d", len(bull.seenPeer))
	}
	round1Peers := bull.seenPeer[1]
	foundBear := false
	for _, p := range round1Peers {
		if p.Role == RoleBear && p.Round == 0 {
			foundBear = true
		}
		if p.Role == RoleBull {
			t.Errorf("bull's round1 peers must NOT contain its own role, got %v", p)
		}
	}
	if !foundBear {
		t.Errorf("bull's round1 peers should include bear round-0 view; got %v", round1Peers)
	}
}

// Cosine similarity sanity: identical strings → 1, unrelated
// strings → low; empty either side → 0.
func TestCosineSimilarityShape(t *testing.T) {
	if s := cosineSimilarity("buy AAPL on earnings", "buy AAPL on earnings"); s < 0.999 {
		t.Errorf("identical strings should be ~1, got %v", s)
	}
	if s := cosineSimilarity("bull AAPL momentum", "bear TSLA rotation hedge"); s > 0.2 {
		t.Errorf("unrelated strings should be near 0, got %v", s)
	}
	if s := cosineSimilarity("", "anything"); s != 0 {
		t.Errorf("empty side must return 0, got %v", s)
	}
}

// fakeChatClient is a minimal llm.LLMClient used to exercise the
// LLMResearcher's parsing + system-prompt path without going to a
// real provider.
type fakeChatClient struct {
	respond  func(req llm.ChatRequest) (*llm.ChatResponse, error)
	lastReq  llm.ChatRequest
	requests []llm.ChatRequest
}

func (f *fakeChatClient) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	f.lastReq = req
	f.requests = append(f.requests, req)
	if f.respond == nil {
		return &llm.ChatResponse{Content: `{"stance":"none","confidence":0.5,"verdicts":[]}`}, nil
	}
	return f.respond(req)
}

func (f *fakeChatClient) ListModels(_ context.Context) ([]llm.ModelInfo, error) { return nil, nil }

// LLMResearcher parses a well-formed JSON reply into an AgentView
// with normalized fields and clamps confidence to [0,1].
func TestLLMResearcherParsesWellFormedReply(t *testing.T) {
	fc := &fakeChatClient{respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) {
		return &llm.ChatResponse{Content: `{
			"stance":"buy on dip",
			"confidence":0.91,
			"verdicts":[
				{"symbol":"AAPL","direction":"BULL","confidence":0.78,"keyPoints":["a","b"]},
				{"symbol":"NVDA","direction":"yolo","confidence":1.4,"keyPoints":["x"]}
			]
		}`}, nil
	}}
	r := &LLMResearcher{PersonaRole: RoleBull, Client: fc}
	view, err := r.Debate(context.Background(), DebateInput{
		Universe:    []string{"AAPL", "NVDA"},
		TradingDate: time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC),
	}, 0, nil)
	if err != nil {
		t.Fatalf("Debate: %v", err)
	}
	if view.Confidence < 0.9 || view.Confidence > 1 {
		t.Errorf("Confidence = %v, want clamped to ~0.91", view.Confidence)
	}
	if view.Verdicts[0].Direction != "bull" {
		t.Errorf("first verdict direction should be lowercased bull, got %q", view.Verdicts[0].Direction)
	}
	if view.Verdicts[1].Direction != "neutral" {
		t.Errorf("unknown direction should coerce to neutral, got %q", view.Verdicts[1].Direction)
	}
	if view.Verdicts[1].Confidence != 1 {
		t.Errorf("confidence should clamp to 1, got %v", view.Verdicts[1].Confidence)
	}
	// User prompt should mention the universe symbols and round.
	user := fc.lastReq.Messages[1].Content
	for _, want := range []string{"AAPL", "NVDA", "2026-05-20"} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q\n--- prompt ---\n%s", want, user)
		}
	}
}

// LLMResearcher tolerates ```json fences + leading prose the way
// decision.parseDecisionOutput does. One contract test is enough —
// stripJSONNoise is shared.
func TestLLMResearcherStripsMarkdownFences(t *testing.T) {
	fc := &fakeChatClient{respond: func(_ llm.ChatRequest) (*llm.ChatResponse, error) {
		return &llm.ChatResponse{Content: "Here you go:\n```json\n{\"stance\":\"x\",\"confidence\":0.5,\"verdicts\":[]}\n```"}, nil
	}}
	r := &LLMResearcher{PersonaRole: RoleQuant, Client: fc}
	view, err := r.Debate(context.Background(), DebateInput{Universe: []string{"AAPL"}}, 0, nil)
	if err != nil {
		t.Fatalf("Debate: %v", err)
	}
	if view.Stance != "x" {
		t.Errorf("expected stripped JSON to yield stance=x, got %q", view.Stance)
	}
}

// Nil llm client → graceful neutral abstain (not an error). Keeps
// dev environments with no LLM key alive on the legacy path.
func TestLLMResearcherWithoutClientReturnsNeutral(t *testing.T) {
	r := &LLMResearcher{PersonaRole: RoleBull, Client: nil}
	view, err := r.Debate(context.Background(), DebateInput{Universe: []string{"AAPL", "MSFT"}}, 0, nil)
	if err != nil {
		t.Fatalf("Debate: %v", err)
	}
	if len(view.Verdicts) != 2 {
		t.Fatalf("expected 2 verdicts, got %d", len(view.Verdicts))
	}
	for _, v := range view.Verdicts {
		if v.Direction != "neutral" {
			t.Errorf("expected neutral when LLM is unwired, got %q for %s", v.Direction, v.Symbol)
		}
	}
}
