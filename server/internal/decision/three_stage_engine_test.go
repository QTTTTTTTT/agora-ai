package decision

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/llm"
)

// sequencedLLM returns responses in order, one per Chat call. Used to
// stage the trader and risk LLM replies for ThreeStageEngine tests.
type sequencedLLM struct {
	responses []llm.ChatResponse
	requests  []llm.ChatRequest
	idx       int
	err       error
	delay     time.Duration
}

func (s *sequencedLLM) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	s.requests = append(s.requests, req)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.idx >= len(s.responses) {
		return &llm.ChatResponse{Content: "{}"}, nil
	}
	r := s.responses[s.idx]
	s.idx++
	return &r, nil
}

func (s *sequencedLLM) ListModels(_ context.Context) ([]llm.ModelInfo, error) { return nil, nil }

type stubInnerEngine struct {
	lastInput DecisionInput
	out       *DecisionOutput
	err       error
}

func (s *stubInnerEngine) Decide(_ context.Context, in DecisionInput) (*DecisionOutput, error) {
	s.lastInput = in
	if s.err != nil {
		return nil, s.err
	}
	return s.out, nil
}

func TestThreeStageEngine_HappyPath_FlowsProposalAndAssessmentToPM(t *testing.T) {
	traderResp := llm.ChatResponse{Content: `{
		"stance": "net long defensive",
		"confidence": 0.7,
		"actions": [
			{"symbol":"AAPL","side":"buy","qtyPct":0.05,"urgency":"today","confidence":0.8}
		],
		"reasoning": "earnings momentum + relative strength"
	}`}
	riskResp := llm.ChatResponse{Content: `{
		"verdict": "approve_with_mitigations",
		"concerns": [
			{"symbol":"AAPL","severity":"warn","reason":"position would breach single-name 5% cap"}
		],
		"mitigations": ["halve AAPL qty"],
		"commentary": "broadly fine, mitigations attached"
	}`}
	llmStub := &sequencedLLM{responses: []llm.ChatResponse{traderResp, riskResp}}
	innerStub := &stubInnerEngine{
		out: &DecisionOutput{
			Actions:    []DecisionAction{{Symbol: "AAPL", Action: "buy", QtyPct: 0.025, Confidence: 0.7}},
			Confidence: 0.7,
			Stance:     "net long defensive",
		},
	}
	engine := &ThreeStageEngine{
		Inner:        innerStub,
		Client:       llmStub,
		StageTimeout: 5 * time.Second,
	}

	out, err := engine.Decide(context.Background(), DecisionInput{
		FundID:      "f1",
		TradingDate: time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC),
		Universe:    []string{"AAPL"},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(out.Actions) != 1 || out.Actions[0].Symbol != "AAPL" {
		t.Fatalf("unexpected output: %+v", out)
	}
	if !strings.Contains(innerStub.lastInput.TraderProposal, "AAPL") {
		t.Fatalf("PM input missing trader proposal: %q", innerStub.lastInput.TraderProposal)
	}
	if !strings.Contains(innerStub.lastInput.RiskAssessment, "approve_with_mitigations") {
		t.Fatalf("PM input missing risk assessment: %q", innerStub.lastInput.RiskAssessment)
	}
	if !strings.Contains(innerStub.lastInput.RiskAssessment, "halve AAPL qty") {
		t.Fatalf("PM input missing mitigation: %q", innerStub.lastInput.RiskAssessment)
	}
	if len(llmStub.requests) != 2 {
		t.Fatalf("expected exactly 2 LLM calls (trader+risk), got %d", len(llmStub.requests))
	}
	if !strings.Contains(llmStub.requests[0].StepName, ".trader_propose") {
		t.Fatalf("first call step name wrong: %q", llmStub.requests[0].StepName)
	}
	if !strings.Contains(llmStub.requests[1].StepName, ".risk_assess") {
		t.Fatalf("second call step name wrong: %q", llmStub.requests[1].StepName)
	}
}

func TestThreeStageEngine_NilClient_DegradesToInnerOnly(t *testing.T) {
	inner := &stubInnerEngine{
		out: &DecisionOutput{Actions: []DecisionAction{{Symbol: "AAPL", Action: "buy", QtyPct: 0.05}}},
	}
	engine := &ThreeStageEngine{Inner: inner, Client: nil}
	out, err := engine.Decide(context.Background(), DecisionInput{FundID: "f1", TradingDate: time.Now()})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out == nil || len(out.Actions) != 1 {
		t.Fatalf("inner output not propagated")
	}
	if strings.TrimSpace(inner.lastInput.TraderProposal) != "" {
		t.Fatalf("expected no trader proposal when client nil, got %q", inner.lastInput.TraderProposal)
	}
}

func TestThreeStageEngine_TraderStageError_NoPMCall(t *testing.T) {
	llmStub := &sequencedLLM{err: errors.New("trader boom")}
	inner := &stubInnerEngine{out: &DecisionOutput{}}
	engine := &ThreeStageEngine{Inner: inner, Client: llmStub}
	_, err := engine.Decide(context.Background(), DecisionInput{FundID: "f1", TradingDate: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "trader proposal") {
		t.Fatalf("expected trader stage error, got %v", err)
	}
	if inner.lastInput.FundID != "" {
		t.Fatalf("PM stage should NOT have been invoked")
	}
}

func TestThreeStageEngine_RiskStageError_NoPMCall(t *testing.T) {
	llmStub := &sequencedLLM{
		responses: []llm.ChatResponse{
			{Content: `{"stance":"","confidence":0.5,"actions":[],"reasoning":""}`},
		},
		// Second call will hit out-of-range and return "{}", which
		// parseRiskAssessment normalises. Force an explicit error for clarity.
	}
	// Wire a second-call error by replacing Chat: easier with a wrapper.
	wrapped := &errOnNthLLM{inner: llmStub, errAtCall: 2, err: errors.New("risk boom")}
	inner := &stubInnerEngine{out: &DecisionOutput{}}
	engine := &ThreeStageEngine{Inner: inner, Client: wrapped}
	_, err := engine.Decide(context.Background(), DecisionInput{FundID: "f1", TradingDate: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "risk assessment") {
		t.Fatalf("expected risk stage error, got %v", err)
	}
	if inner.lastInput.FundID != "" {
		t.Fatalf("PM stage should NOT have been invoked")
	}
}

func TestThreeStageEngine_StageTimeout_ReturnsSentinel(t *testing.T) {
	llmStub := &sequencedLLM{delay: 50 * time.Millisecond, responses: []llm.ChatResponse{{Content: "{}"}}}
	inner := &stubInnerEngine{out: &DecisionOutput{}}
	engine := &ThreeStageEngine{Inner: inner, Client: llmStub, StageTimeout: 1 * time.Millisecond}
	_, err := engine.Decide(context.Background(), DecisionInput{FundID: "f1", TradingDate: time.Now()})
	if err == nil || !errors.Is(err, ErrStageTimedOut) {
		t.Fatalf("expected ErrStageTimedOut, got %v", err)
	}
}

func TestThreeStageEngine_PMError_PropagatesWrapped(t *testing.T) {
	llmStub := &sequencedLLM{
		responses: []llm.ChatResponse{
			{Content: `{"stance":"","confidence":0.5,"actions":[],"reasoning":""}`},
			{Content: `{"verdict":"approve","concerns":[],"mitigations":[],"commentary":""}`},
		},
	}
	inner := &stubInnerEngine{err: errors.New("pm boom")}
	engine := &ThreeStageEngine{Inner: inner, Client: llmStub}
	_, err := engine.Decide(context.Background(), DecisionInput{FundID: "f1", TradingDate: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "pm final") {
		t.Fatalf("expected pm final error, got %v", err)
	}
}

func TestThreeStageEngine_NilInnerEngine_Errors(t *testing.T) {
	engine := &ThreeStageEngine{}
	if _, err := engine.Decide(context.Background(), DecisionInput{}); err == nil {
		t.Fatalf("expected error on nil inner")
	}
}

func TestParseTraderProposal_NormalisesSideAndUrgency(t *testing.T) {
	raw := `{
		"stance": "  net long  ",
		"confidence": 1.4,
		"actions": [
			{"symbol":" AAPL ","side":"BUY","qtyPct":0.5,"urgency":"  YESTERDAY  ","confidence":0.9},
			{"symbol":"NVDA","side":"foo","qtyPct":0.2,"urgency":"today","confidence":0.5}
		],
		"reasoning":"x"
	}`
	got, err := parseTraderProposal(raw)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got.Confidence != 1.0 {
		t.Fatalf("confidence should clamp to 1.0, got %v", got.Confidence)
	}
	if len(got.Actions) != 1 {
		t.Fatalf("expected 1 valid action (NVDA dropped), got %d", len(got.Actions))
	}
	a := got.Actions[0]
	if a.Symbol != "AAPL" || a.Side != "buy" || a.Urgency != "today" {
		t.Fatalf("normalisation broken: %+v", a)
	}
}

func TestParseRiskAssessment_NormalisesVerdictAndSeverity(t *testing.T) {
	raw := `{
		"verdict": "  Mystery  ",
		"concerns": [
			{"symbol":" AAPL ","severity":"BLOCK","reason":"x"},
			{"symbol":"","severity":"foobar","reason":"  "}
		],
		"mitigations": [" m1 ", "  "],
		"commentary": " ok "
	}`
	got, err := parseRiskAssessment(raw)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got.Verdict != "approve_with_mitigations" {
		t.Fatalf("unknown verdict should default to approve_with_mitigations, got %q", got.Verdict)
	}
	if len(got.Concerns) != 1 {
		t.Fatalf("empty-reason concern should be dropped, got %d", len(got.Concerns))
	}
	if got.Concerns[0].Severity != "block" {
		t.Fatalf("severity not normalised: %+v", got.Concerns[0])
	}
	if len(got.Mitigations) != 1 || got.Mitigations[0] != "m1" {
		t.Fatalf("mitigations not normalised: %+v", got.Mitigations)
	}
	if got.Commentary != "ok" {
		t.Fatalf("commentary not trimmed: %q", got.Commentary)
	}
}

func TestRenderTraderProposalForPM_HandlesNil(t *testing.T) {
	if got := renderTraderProposalForPM(nil); got != "" {
		t.Fatalf("expected empty string on nil, got %q", got)
	}
}

func TestRenderRiskAssessmentForPM_HandlesNil(t *testing.T) {
	if got := renderRiskAssessmentForPM(nil); got != "" {
		t.Fatalf("expected empty string on nil, got %q", got)
	}
}

// errOnNthLLM lets tests force an error on the nth Chat call. Used to
// trigger the risk-stage error path while letting the trader stage
// succeed.
type errOnNthLLM struct {
	inner     llm.LLMClient
	errAtCall int
	err       error
	calls     int
}

func (e *errOnNthLLM) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	e.calls++
	if e.calls == e.errAtCall {
		return nil, e.err
	}
	return e.inner.Chat(ctx, req)
}

func (e *errOnNthLLM) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	return e.inner.ListModels(ctx)
}
