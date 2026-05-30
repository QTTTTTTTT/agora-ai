package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubAssistService is the minimal in-memory stand-in for
// FundAssistService used across these tests. Reply is the canned
// chat output (raw JSON or fenced block); Err short-circuits to an
// LLM error path. Calls counts how many times Chat was invoked so
// tests can assert on retry / no-retry behaviour.
type stubAssistService struct {
	Reply string
	Err   error
	Calls int
	// LastUser captures the userID passed in so the test can check
	// we're routing billing to the caller, not a hard-coded user.
	LastUser string
	// LastSystem / LastUser capture prompts so prompt-shape tests
	// don't have to expose the buildAssist* helpers.
	LastSystemPrompt string
	LastUserPrompt   string
}

func (s *stubAssistService) Chat(_ context.Context, userID, sys, user string) (string, error) {
	s.Calls++
	s.LastUser = userID
	s.LastSystemPrompt = sys
	s.LastUserPrompt = user
	return s.Reply, s.Err
}

// fundHandlerWithAssist is the boilerplate the AssistCreateFund tests
// share: a FundHandler wired with the smallest possible service set
// + a configurable assist service. Returning the stubs alongside the
// mux lets the assertions inspect what the handler actually called.
func fundHandlerWithAssist(t *testing.T, fundSvc stubFundService, teamSvc stubTeamService, assist FundAssistService) *http.ServeMux {
	t.Helper()
	handler := NewFundHandler(
		fundSvc,
		teamSvc,
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	).WithFundAssistService(assist)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	return mux
}

func TestAssistCreateFund_DryRunReturns200WithDefaultedPlan(t *testing.T) {
	stub := &stubAssistService{
		Reply: `{"fund":{"name":"美股 AI","market":"us_equity"},"agents":[{"role":"pm","name":"PM"},{"role":"researcher","name":"NVDA","focus":"NVDA"}]}`,
	}
	mux := fundHandlerWithAssist(t, stubFundService{
		createFundFn: func(string, CreateFundInput) (*Fund, error) {
			t.Fatal("dryRun must NOT call CreateFund")
			return nil, nil
		},
	}, stubTeamService{}, stub)

	body := bytes.NewBufferString(`{"prompt":"做一个美股 AI 基金","dryRun":true}`)
	req := authRequest(httptest.NewRequest(http.MethodPost, "/api/companies/c1/funds:assist", body), "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if stub.Calls != 1 {
		t.Fatalf("expected exactly 1 LLM call, got %d", stub.Calls)
	}
	var resp FundAssistResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp.FundID != "" || resp.Fund != nil {
		t.Fatalf("dryRun must return empty fund, got %+v", resp)
	}
	if resp.Plan.Fund.InitialCapital != 1_000_000 {
		t.Errorf("expected default initial capital 1M, got %v", resp.Plan.Fund.InitialCapital)
	}
	if resp.Plan.Fund.BaseCurrency != "USD" {
		t.Errorf("us_equity should default base currency to USD, got %q", resp.Plan.Fund.BaseCurrency)
	}
	if resp.Plan.Fund.Specialization == nil || len(resp.Plan.Fund.Specialization.Markets) != 1 || resp.Plan.Fund.Specialization.Markets[0] != "us_equity" {
		t.Errorf("specialization.markets should default to fund.market, got %+v", resp.Plan.Fund.Specialization)
	}
}

func TestAssistCreateFund_CommitCallsCreateFundAndAddsAgents(t *testing.T) {
	stub := &stubAssistService{
		Reply: `{"fund":{"name":"美股芯片","market":"us_equity","initialCapital":500000},"agents":[{"role":"pm","name":"PM","systemPrompt":"组合经理"},{"role":"researcher","name":"R","focus":"NVDA","systemPrompt":"覆盖 NVDA"}]}`,
	}
	var capturedInput CreateFundInput
	addCalls := []string{}
	updCalls := 0
	mux := fundHandlerWithAssist(t,
		stubFundService{
			createFundFn: func(_ string, in CreateFundInput) (*Fund, error) {
				capturedInput = in
				return &Fund{ID: "fund-new", CompanyID: in.CompanyID, Name: in.Name, Market: in.Market, TradingMode: in.TradingMode}, nil
			},
		},
		stubTeamService{
			addAgentFn: func(_, _, role, focus string) (*Agent, error) {
				addCalls = append(addCalls, role+":"+focus)
				return &Agent{ID: "ag-" + role, Role: role, Focus: focus}, nil
			},
			updateAgentFn: func(_, _, agentID string, _ AgentConfig) (*Agent, error) {
				updCalls++
				return &Agent{ID: agentID}, nil
			},
		}, stub)

	body := bytes.NewBufferString(`{"prompt":"做一个美股芯片基金，需要 PM 和一个 NVDA 研究员","dryRun":false}`)
	req := authRequest(httptest.NewRequest(http.MethodPost, "/api/companies/c1/funds:assist", body), "user-2")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	if capturedInput.CompanyID != "c1" || capturedInput.TradingMode != "simulation" {
		t.Errorf("expected companyId=c1 + simulation forced; got %+v", capturedInput)
	}
	if capturedInput.InitialCapital != 500000 {
		t.Errorf("expected LLM-provided 500K to flow through; got %v", capturedInput.InitialCapital)
	}
	if len(addCalls) != 2 || addCalls[0] != "pm:" || addCalls[1] != "researcher:NVDA" {
		t.Errorf("unexpected AddAgent calls: %v", addCalls)
	}
	// Both agents had non-empty systemPrompts → expect 2 update
	// calls.
	if updCalls != 2 {
		t.Errorf("expected systemPrompt updates for 2 agents; got %d", updCalls)
	}

	var resp FundAssistResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp.FundID != "fund-new" || len(resp.Agents) != 2 {
		t.Errorf("unexpected response shape: %+v", resp)
	}
}

func TestAssistCreateFund_RejectsCrossMarketPlanWith422(t *testing.T) {
	// LLM returns a US fund with an A-share researcher → server-
	// side validator must intercept and return 422 with structured
	// issues. The fund must NOT be created.
	stub := &stubAssistService{
		Reply: `{"fund":{"name":"X","market":"us_equity"},"agents":[{"role":"pm"},{"role":"researcher","focus":"600519"}]}`,
	}
	mux := fundHandlerWithAssist(t, stubFundService{
		createFundFn: func(string, CreateFundInput) (*Fund, error) {
			t.Fatal("CreateFund must not run when validation fails")
			return nil, nil
		},
	}, stubTeamService{}, stub)

	body := bytes.NewBufferString(`{"prompt":"做美股基金"}`)
	req := authRequest(httptest.NewRequest(http.MethodPost, "/api/companies/c1/funds:assist", body), "u")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d body=%s", rr.Code, rr.Body.String())
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["error"] != "plan_rejected" {
		t.Errorf("expected error=plan_rejected, got %v", payload["error"])
	}
	issues, ok := payload["issues"].([]interface{})
	if !ok || len(issues) == 0 {
		t.Fatalf("expected non-empty issues array, got %v", payload["issues"])
	}
}

func TestAssistCreateFund_503WhenServiceUnwired(t *testing.T) {
	mux := fundHandlerWithAssist(t, stubFundService{}, stubTeamService{}, nil)

	body := bytes.NewBufferString(`{"prompt":"x"}`)
	req := authRequest(httptest.NewRequest(http.MethodPost, "/api/companies/c1/funds:assist", body), "u")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAssistCreateFund_502WhenLLMOutputUnusable(t *testing.T) {
	stub := &stubAssistService{Reply: "I'm sorry, I can't help with that."}
	mux := fundHandlerWithAssist(t, stubFundService{}, stubTeamService{}, stub)

	body := bytes.NewBufferString(`{"prompt":"x"}`)
	req := authRequest(httptest.NewRequest(http.MethodPost, "/api/companies/c1/funds:assist", body), "u")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAssistCreateFund_400OnEmptyPrompt(t *testing.T) {
	stub := &stubAssistService{}
	mux := fundHandlerWithAssist(t, stubFundService{}, stubTeamService{}, stub)

	body := bytes.NewBufferString(`{"prompt":""}`)
	req := authRequest(httptest.NewRequest(http.MethodPost, "/api/companies/c1/funds:assist", body), "u")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	if stub.Calls != 0 {
		t.Errorf("LLM should not be invoked on empty prompt, got %d calls", stub.Calls)
	}
}

func TestAssistCreateFund_PartialAgentFailureKeepsFundAndReturnsWarning(t *testing.T) {
	// First AddAgent call succeeds, second fails. We expect:
	//   - fund still created (the user can fix up the team in the
	//     UI; rolling back the fund on a partial would be more
	//     surprising than helpful).
	//   - response includes the agents that DID succeed and a
	//     warning explaining what fell through.
	stub := &stubAssistService{
		Reply: `{"fund":{"name":"X","market":"us_equity"},"agents":[{"role":"pm","name":"PM"},{"role":"researcher","focus":"NVDA"}]}`,
	}
	addCalls := 0
	mux := fundHandlerWithAssist(t,
		stubFundService{
			createFundFn: func(_ string, in CreateFundInput) (*Fund, error) {
				return &Fund{ID: "fund-1", CompanyID: in.CompanyID}, nil
			},
		},
		stubTeamService{
			addAgentFn: func(_, _, role, focus string) (*Agent, error) {
				addCalls++
				if addCalls == 2 {
					return nil, errors.New("agent quota exhausted")
				}
				return &Agent{ID: "ag-" + role, Role: role, Focus: focus}, nil
			},
		}, stub)

	body := bytes.NewBufferString(`{"prompt":"x"}`)
	req := authRequest(httptest.NewRequest(http.MethodPost, "/api/companies/c1/funds:assist", body), "u")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 (partial success keeps fund), got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp FundAssistResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.FundID != "fund-1" {
		t.Fatalf("expected fund-1 returned despite agent failure, got %+v", resp)
	}
	if len(resp.Agents) != 1 {
		t.Fatalf("expected 1 successful agent, got %d", len(resp.Agents))
	}
	if len(resp.Warnings) == 0 {
		t.Fatal("expected a warning describing the failed agent")
	}
	if !strings.Contains(strings.Join(resp.Warnings, "|"), "agents[1]") {
		t.Errorf("warning should reference the failing agent index, got %v", resp.Warnings)
	}
}

// Ensure the stubAssistService satisfies FundAssistService at compile time.
var _ FundAssistService = (*stubAssistService)(nil)

// Suppress unused import noise when bring-up tests are stripped.
var _ = context.Background

func TestExtractAssistPlan_PureJSON(t *testing.T) {
	raw := `{"fund":{"name":"X","market":"us_equity"},"agents":[{"role":"pm","name":"PM"}]}`
	plan, err := extractAssistPlan(raw)
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if plan.Fund.Name != "X" || plan.Fund.Market != "us_equity" {
		t.Fatalf("plan not decoded: %+v", plan)
	}
	if len(plan.Agents) != 1 || plan.Agents[0].Role != "pm" {
		t.Fatalf("agents not decoded: %+v", plan.Agents)
	}
}

func TestExtractAssistPlan_FencedJSON(t *testing.T) {
	raw := "好的，这是计划：\n```json\n{\"fund\":{\"name\":\"Y\",\"market\":\"a_share\"},\"agents\":[{\"role\":\"pm\"}]}\n```\n"
	plan, err := extractAssistPlan(raw)
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if plan.Fund.Market != "a_share" {
		t.Fatalf("market not parsed: %+v", plan.Fund)
	}
}

func TestExtractAssistPlan_PreambleStrippedByBraceScan(t *testing.T) {
	raw := "Here is what I came up with:\n{\"fund\":{\"name\":\"Z\",\"market\":\"hk_equity\"},\"agents\":[{\"role\":\"pm\"}]}"
	plan, err := extractAssistPlan(raw)
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if plan.Fund.Name != "Z" {
		t.Fatalf("plan not decoded: %+v", plan)
	}
}

func TestExtractAssistPlan_EmptyOrGarbageReturnsSentinel(t *testing.T) {
	cases := []string{"", "   ", "I'm sorry, I can't help with that."}
	for _, raw := range cases {
		_, err := extractAssistPlan(raw)
		if !errors.Is(err, ErrFundAssistEmptyPlan) {
			t.Fatalf("input %q expected ErrFundAssistEmptyPlan, got %v", raw, err)
		}
	}
}

func TestExtractAssistPlan_BadJSONReturnsDecodeError(t *testing.T) {
	// Balanced braces but contents are not valid JSON (trailing
	// comma + missing colon). The depth-scan finds the boundaries
	// but json.Unmarshal then fails — we want to surface that as a
	// distinct error from "no plan at all".
	raw := `{"fund": {"name": "broken", "market":,},"agents":[]}`
	_, err := extractAssistPlan(raw)
	if err == nil || errors.Is(err, ErrFundAssistEmptyPlan) {
		t.Fatalf("expected decode error, got %v", err)
	}
}

func TestValidateAssistPlan_HappyPath(t *testing.T) {
	plan := FundAssistPlan{
		Fund: FundAssistPlanFund{
			Name:   "美股芯片精选",
			Market: "us_equity",
		},
		Agents: []FundAssistPlanAgent{
			{Role: "pm", Name: "Portfolio Manager"},
			{Role: "researcher", Name: "NVDA 研究员", Focus: "NVDA"},
			{Role: "trader", Name: "Trader"},
		},
	}
	warnings, err := validateAssistPlan(plan)
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("did not expect warnings, got %v", warnings)
	}
}

func TestValidateAssistPlan_RejectsCrossMarketResearcher(t *testing.T) {
	// US equity fund with a researcher focused on 600519 (A-share) —
	// the canonical "美股基金不能塞 A 股研究员" case the user asked
	// us to guard against.
	plan := FundAssistPlan{
		Fund: FundAssistPlanFund{Name: "Cross", Market: "us_equity"},
		Agents: []FundAssistPlanAgent{
			{Role: "pm", Name: "PM"},
			{Role: "researcher", Name: "白酒研究员", Focus: "600519"},
		},
	}
	_, err := validateAssistPlan(plan)
	if err == nil {
		t.Fatal("expected validation to reject cross-market researcher")
	}
	hasMatch := false
	for _, iss := range err.Issues {
		if iss.Code == "market_mismatch" && strings.Contains(iss.Field, "agents[1].focus") {
			hasMatch = true
		}
	}
	if !hasMatch {
		t.Fatalf("expected agents[1].focus market_mismatch issue, got %+v", err.Issues)
	}
}

func TestValidateAssistPlan_RejectsCrossMarketSpecialization(t *testing.T) {
	plan := FundAssistPlan{
		Fund: FundAssistPlanFund{
			Name:   "Cross",
			Market: "us_equity",
			Specialization: &FundAssistPlanSpecialization{
				Markets: []string{"us_equity", "a_share"}, // leaks A-share
			},
		},
		Agents: []FundAssistPlanAgent{{Role: "pm", Name: "PM"}},
	}
	_, err := validateAssistPlan(plan)
	if err == nil {
		t.Fatal("expected validation to reject specialization market leak")
	}
}

func TestValidateAssistPlan_RejectsMissingPM(t *testing.T) {
	plan := FundAssistPlan{
		Fund:   FundAssistPlanFund{Name: "X", Market: "us_equity"},
		Agents: []FundAssistPlanAgent{{Role: "researcher", Name: "R", Focus: "NVDA"}},
	}
	_, err := validateAssistPlan(plan)
	if err == nil {
		t.Fatal("expected validation to reject team without a PM")
	}
	hasMissing := false
	for _, iss := range err.Issues {
		if iss.Code == "missing_pm" {
			hasMissing = true
		}
	}
	if !hasMissing {
		t.Fatalf("expected missing_pm issue, got %+v", err.Issues)
	}
}

func TestValidateAssistPlan_RejectsUnsupportedMarket(t *testing.T) {
	plan := FundAssistPlan{
		Fund:   FundAssistPlanFund{Name: "X", Market: "real_estate"},
		Agents: []FundAssistPlanAgent{{Role: "pm"}},
	}
	_, err := validateAssistPlan(plan)
	if err == nil {
		t.Fatal("expected validation to reject unknown market")
	}
}

func TestValidateAssistPlan_WarnsOnUniverseSymbolMismatchButDoesNotBlock(t *testing.T) {
	// US fund with HK code in universe — strange but legal (could be
	// a cross-listed name). We warn but don't reject.
	plan := FundAssistPlan{
		Fund: FundAssistPlanFund{
			Name:   "X",
			Market: "us_equity",
			Universe: &FundAssistPlanUniverse{
				Mode:    "explicit",
				Symbols: []string{"NVDA", "0700"},
			},
		},
		Agents: []FundAssistPlanAgent{{Role: "pm", Name: "PM"}},
	}
	warnings, err := validateAssistPlan(plan)
	if err != nil {
		t.Fatalf("expected universe mismatch to only warn, got reject: %+v", err.Issues)
	}
	if len(warnings) == 0 {
		t.Fatal("expected at least one warning for cross-market universe symbol")
	}
}

func TestDetectFocusMarketMismatch_PassThroughOnAmbiguous(t *testing.T) {
	cases := []struct {
		sym  string
		mkt  string
		want string
	}{
		// Same-market, no mismatch
		{"NVDA", "us_equity", ""},
		{"600519", "a_share", ""},
		{"0700", "hk_equity", ""},
		// Cross-market mismatches
		{"NVDA", "a_share", "us_equity"},
		{"600519", "us_equity", "a_share"},
		// Ambiguous — no rule triggers, no rejection
		{"半导体", "us_equity", ""},
		{"futures-ESM5", "us_equity", ""},
		{"", "us_equity", ""},
	}
	for _, c := range cases {
		got := detectFocusMarketMismatch(c.sym, c.mkt)
		if got != c.want {
			t.Errorf("detectFocusMarketMismatch(%q, %q) = %q, want %q", c.sym, c.mkt, got, c.want)
		}
	}
}

func TestComputeAssistPlan_HappyPathInvokesLLMOnce(t *testing.T) {
	stub := &stubAssistService{
		Reply: `{"fund":{"name":"美股 AI 基金","market":"us_equity"},"agents":[{"role":"pm","name":"PM"},{"role":"researcher","name":"NVDA Research","focus":"NVDA"}]}`,
	}
	plan, warnings, err := computeAssistPlan(context.Background(), stub, "user-1", FundAssistRequest{Prompt: "做一个美股 AI 基金"})
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if stub.Calls != 1 {
		t.Fatalf("expected exactly 1 LLM call, got %d", stub.Calls)
	}
	if stub.LastUser != "user-1" {
		t.Fatalf("expected userID propagated, got %q", stub.LastUser)
	}
	if plan.Fund.Market != "us_equity" {
		t.Fatalf("plan not decoded: %+v", plan)
	}
	if len(warnings) != 0 {
		t.Fatalf("did not expect warnings, got %v", warnings)
	}
}

func TestComputeAssistPlan_LLMErrorBubblesUp(t *testing.T) {
	stub := &stubAssistService{Err: errors.New("boom")}
	_, _, err := computeAssistPlan(context.Background(), stub, "user-1", FundAssistRequest{Prompt: "x"})
	if err == nil {
		t.Fatal("expected error from llm")
	}
	if errors.Is(err, ErrFundAssistEmptyPlan) {
		t.Fatal("LLM error should not be classified as empty-plan")
	}
}

func TestComputeAssistPlan_ValidationErrorReturnsTypedFundAssistError(t *testing.T) {
	stub := &stubAssistService{
		// US fund with A-share researcher → must trip server validation.
		Reply: `{"fund":{"name":"X","market":"us_equity"},"agents":[{"role":"pm"},{"role":"researcher","focus":"600519"}]}`,
	}
	_, _, err := computeAssistPlan(context.Background(), stub, "u", FundAssistRequest{Prompt: "x"})
	if err == nil {
		t.Fatal("expected validation error")
	}
	var asErr *FundAssistError
	if !errors.As(err, &asErr) {
		t.Fatalf("expected *FundAssistError, got %T: %v", err, err)
	}
	if len(asErr.Issues) == 0 {
		t.Fatal("expected at least one issue")
	}
}

func TestComputeAssistPlan_RequiresNonEmptyPrompt(t *testing.T) {
	stub := &stubAssistService{}
	_, _, err := computeAssistPlan(context.Background(), stub, "u", FundAssistRequest{Prompt: "  "})
	if err == nil {
		t.Fatal("expected error for empty prompt")
	}
	if stub.Calls != 0 {
		t.Fatalf("LLM should not be invoked for empty prompt, got %d calls", stub.Calls)
	}
}

func TestComputeAssistPlan_NilServiceFails(t *testing.T) {
	_, _, err := computeAssistPlan(context.Background(), nil, "u", FundAssistRequest{Prompt: "x"})
	if err == nil {
		t.Fatal("expected error when service is nil")
	}
}

func TestBuildAssistSystemPrompt_PinsMarketWhitelist(t *testing.T) {
	got := buildAssistSystemPrompt("zh-CN")
	for _, m := range SupportedAssistMarkets {
		if !strings.Contains(got, m) {
			t.Errorf("system prompt should mention market %q so the LLM stays inside the whitelist; got prompt without it", m)
		}
	}
	// We also pin the cross-market guard text so a future "let me
	// shorten this prompt" change doesn't silently drop the rule
	// the user explicitly asked for.
	if !strings.Contains(got, "禁止出现") {
		t.Error("system prompt should ban cross-market researchers explicitly")
	}
}
