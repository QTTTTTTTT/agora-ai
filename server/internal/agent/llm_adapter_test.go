// llm_adapter_test.go — covers the S8.3 LLMAdapter bridge from
// agent.LLMClient / SchemaLLMClient to llm.LLMClient.

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fundai/server/internal/llm"
)

// chatStub records the most recent ChatRequest and returns a
// canned response.
type chatStub struct {
	lastReq llm.ChatRequest
	resp    *llm.ChatResponse
	err     error
}

func (s *chatStub) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	s.lastReq = req
	return s.resp, s.err
}

func (s *chatStub) ListModels(_ context.Context) ([]llm.ModelInfo, error) { return nil, nil }

func TestLLMAdapter_Complete_PassesPromptsAndTags(t *testing.T) {
	stub := &chatStub{resp: &llm.ChatResponse{Content: "hello world", InputTokens: 1, OutputTokens: 2}}
	a := NewLLMAdapter(stub, "fund-123",
		WithLLMAdapterUser("user-1"),
		WithLLMAdapterAgent("ag-1"),
		WithLLMAdapterStep("analyst:test"),
		WithLLMAdapterTier(llm.TierStandard),
		WithLLMAdapterMaxTokens(512),
		WithLLMAdapterTemperature(0.3),
	)
	out, err := a.Complete(context.Background(), "be helpful", "do the thing")
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello world" {
		t.Errorf("got = %q", out)
	}
	if stub.lastReq.FundID != "fund-123" {
		t.Errorf("fund = %q", stub.lastReq.FundID)
	}
	if stub.lastReq.UserID != "user-1" || stub.lastReq.AgentID != "ag-1" || stub.lastReq.StepName != "analyst:test" {
		t.Errorf("tagging missing: %+v", stub.lastReq)
	}
	if stub.lastReq.ModelTier != llm.TierStandard || stub.lastReq.MaxTokens != 512 || stub.lastReq.Temperature != 0.3 {
		t.Errorf("model knobs wrong: %+v", stub.lastReq)
	}
	if stub.lastReq.ResponseFormat != "" || len(stub.lastReq.ResponseSchema) != 0 {
		t.Errorf("Complete must not set structured-output knobs: %+v", stub.lastReq)
	}
	if len(stub.lastReq.Messages) != 2 ||
		stub.lastReq.Messages[0].Role != "system" || stub.lastReq.Messages[0].Content != "be helpful" ||
		stub.lastReq.Messages[1].Role != "user" || stub.lastReq.Messages[1].Content != "do the thing" {
		t.Errorf("messages wrong: %+v", stub.lastReq.Messages)
	}
}

func TestLLMAdapter_CompleteWithSchema_SetsResponseFormat(t *testing.T) {
	stub := &chatStub{resp: &llm.ChatResponse{Content: `{"ok":true}`}}
	a := NewLLMAdapter(stub, "fund-x")
	schema := []byte(`{"type":"object"}`)
	out, err := a.CompleteWithSchema(context.Background(), "sys", "user", schema)
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"ok":true}` {
		t.Errorf("got = %q", out)
	}
	if stub.lastReq.ResponseFormat != "json_schema" {
		t.Errorf("ResponseFormat = %q", stub.lastReq.ResponseFormat)
	}
	if string(stub.lastReq.ResponseSchema) != string(schema) {
		t.Errorf("schema not propagated: %s", stub.lastReq.ResponseSchema)
	}
}

func TestLLMAdapter_CompleteWithSchema_EmptySchema_JSONObject(t *testing.T) {
	stub := &chatStub{resp: &llm.ChatResponse{Content: `{"ok":true}`}}
	a := NewLLMAdapter(stub, "fund-x")
	if _, err := a.CompleteWithSchema(context.Background(), "sys", "user", nil); err != nil {
		t.Fatal(err)
	}
	if stub.lastReq.ResponseFormat != "json_object" {
		t.Errorf("ResponseFormat = %q, want json_object when no schema given", stub.lastReq.ResponseFormat)
	}
}

func TestLLMAdapter_Complete_RejectsEmptyUser(t *testing.T) {
	a := NewLLMAdapter(&chatStub{resp: &llm.ChatResponse{}}, "fund-x")
	if _, err := a.Complete(context.Background(), "sys", "   "); err == nil {
		t.Error("expected error on empty user prompt")
	}
}

func TestLLMAdapter_Complete_NilClientErrors(t *testing.T) {
	a := NewLLMAdapter(nil, "fund-x")
	if _, err := a.Complete(context.Background(), "sys", "go"); err == nil {
		t.Error("expected error when adapter has nil client")
	}
}

func TestLLMAdapter_PropagatesUpstreamError(t *testing.T) {
	stub := &chatStub{err: errors.New("upstream boom")}
	a := NewLLMAdapter(stub, "fund-x")
	_, err := a.Complete(context.Background(), "sys", "go")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want wraps 'boom'", err)
	}
}

// --- Schema integration with analyst code path ----------------------------

// schemaStub implements SchemaLLMClient so we can assert that
// callLLMForReport routes through the schema path when the
// underlying client supports it.
type schemaStub struct {
	chatStub
	schemaSeen []byte
	schemaResp string
}

func (s *schemaStub) Complete(_ context.Context, _ string, _ string) (string, error) {
	return "", errors.New("schemaStub: Complete should not be called; the analyst must use CompleteWithSchema")
}

func (s *schemaStub) CompleteWithSchema(_ context.Context, _, _ string, schema []byte) (string, error) {
	s.schemaSeen = append(s.schemaSeen[:0], schema...)
	return s.schemaResp, nil
}

func TestAnalystBase_CallLLMForReport_PrefersSchemaPath(t *testing.T) {
	stub := &schemaStub{schemaResp: `{"direction":"bullish","confidence":80,"thesis":"ok","key_findings":["a"],"risks":["b"]}`}
	b := &analystBase{id: "a", name: "A", fundID: "f", llm: stub}
	rep, err := b.callLLMForReport(context.Background(), "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Direction != "bullish" || rep.Confidence != 80 {
		t.Errorf("rep = %+v", rep)
	}
	if !strings.Contains(string(stub.schemaSeen), "direction") {
		t.Errorf("AnalystReportJSONSchema not propagated, got: %s", stub.schemaSeen)
	}
}

func TestAdvocate_CallAdvocateLLM_PrefersSchemaPath(t *testing.T) {
	stub := &schemaStub{schemaResp: `{"direction":"bullish","confidence":70,"thesis":"ok","key_findings":["sp"],"risks":["rb"]}`}
	reply, err := callAdvocateLLM(context.Background(), stub, "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if reply.Direction != "bullish" || reply.Confidence != 70 {
		t.Errorf("reply = %+v", reply)
	}
	if !strings.Contains(string(stub.schemaSeen), "support_points") {
		t.Errorf("AdvocateArgumentJSONSchema not propagated, got: %s", stub.schemaSeen)
	}
}
