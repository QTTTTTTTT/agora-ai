package modelab

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/llm"
)

// chatFanoutClient is a stub that satisfies BOTH llm.LLMClient
// (so we can hand it to NewShadowDispatcher as Inner) AND
// ConfigChatClient (so the dispatcher will fan out shadow arms
// through it). Every call records its (model, isShadow) pair so
// the assertion can verify which arm was the primary vs shadow.
type chatFanoutClient struct {
	mu              sync.Mutex
	innerCalls      []string // model names received via Chat (the "primary" path)
	configCalls     []string // model names received via ChatWithConfig (shadow path)
	primaryAnswer   *llm.ChatResponse
	shadowAnswer    *llm.ChatResponse
	primaryErr      error
	shadowErr       error
	shadowSleep     time.Duration
	configCallCount int32
}

func (c *chatFanoutClient) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	c.mu.Lock()
	c.innerCalls = append(c.innerCalls, req.Model)
	c.mu.Unlock()
	if c.primaryErr != nil {
		return nil, c.primaryErr
	}
	r := c.primaryAnswer
	if r == nil {
		r = &llm.ChatResponse{Content: "primary-ok", Model: req.Model}
	}
	return r, nil
}

func (c *chatFanoutClient) ListModels(_ context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}

func (c *chatFanoutClient) ChatWithConfig(_ context.Context, _ llm.ChatRequest, cfg *llm.ModelConfig) (*llm.ChatResponse, error) {
	atomic.AddInt32(&c.configCallCount, 1)
	if c.shadowSleep > 0 {
		time.Sleep(c.shadowSleep)
	}
	c.mu.Lock()
	c.configCalls = append(c.configCalls, cfg.ModelName)
	c.mu.Unlock()
	if c.shadowErr != nil {
		return nil, c.shadowErr
	}
	r := c.shadowAnswer
	if r == nil {
		r = &llm.ChatResponse{
			Content:      `{"verdict":"buy"}`,
			Model:        cfg.ModelName,
			Provider:     string(cfg.Provider),
			InputTokens:  10,
			OutputTokens: 20,
			TotalCost:    0.001,
		}
	}
	return r, nil
}

func (c *chatFanoutClient) snapshotCalls() (inner []string, config []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	inner = append([]string(nil), c.innerCalls...)
	config = append([]string(nil), c.configCalls...)
	return
}

// TestShadowDispatcher_Fanout exercises the happy path: 2-arm
// experiment, primary arm goes through Inner.Chat, shadow arm
// goes through ChatWithConfig, and the shadow result lands in
// model_ab_shadow_responses.
func TestShadowDispatcher_Fanout(t *testing.T) {
	db, mock := openMock(t)
	defer db.Close()

	armsJSON, _ := MarshalArms(stubArms())
	rows := experimentRows().AddRow(
		"00000000-0000-0000-0000-000000000001",
		"fanout", "",
		string(ScopeAgentRole), "pm",
		stringArray(),
		armsJSON,
		float64Array(1.0, 0.0), // arm 0 always wins → shadow on arm 1
		string(StatusRunning),
		time.Time{}, time.Time{},
		int64(0), int64(0), "",
		time.Now(), time.Now(),
	)
	mock.ExpectQuery(`FROM model_ab_experiments\s+WHERE status = ANY`).WillReturnRows(rows)
	mock.ExpectQuery(`INSERT INTO model_ab_assignments`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "arm_index", "arm_name", "assigned_at"}).
			AddRow("00000000-0000-0000-0000-000000000077", 0, "control", time.Now()))
	// Shadow response insert + tokens roll-up.
	mock.ExpectExec(`INSERT INTO model_ab_shadow_responses`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`UPDATE model_ab_experiments\s+SET tokens_used`).WillReturnResult(sqlmock.NewResult(1, 1))

	repo := NewRepo(db)
	resolver := NewResolver(repo)
	resolver.Logger = discardLogger()
	stub := &chatFanoutClient{}
	d := NewShadowDispatcher(stub, resolver, repo, HookContext{
		SystemAPIKeys: map[llm.Provider]string{
			llm.ProviderOpenAI: "sk-openai",
			llm.ProviderClaude: "sk-claude",
		},
	})
	d.Logger = discardLogger()

	resp, err := d.Chat(context.Background(), llm.ChatRequest{
		FundID: "f1", AgentID: "ag-7", AgentRole: "pm",
		StepName: "pm_decision", RunID: "run-1",
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp == nil || resp.Content != "primary-ok" {
		t.Fatalf("expected primary response, got %+v", resp)
	}
	inner, configCalls := stub.snapshotCalls()
	if len(inner) != 1 {
		t.Fatalf("expected exactly 1 Chat (primary), got %d (%v)", len(inner), inner)
	}
	if len(configCalls) != 1 {
		t.Fatalf("expected exactly 1 ChatWithConfig (shadow), got %d (%v)", len(configCalls), configCalls)
	}
	if configCalls[0] != "claude-opus" {
		t.Fatalf("shadow arm config call expected claude-opus, got %q", configCalls[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// TestShadowDispatcher_ShadowErrorDoesntFailPrimary verifies that
// an error in the shadow path leaves the caller-visible primary
// response intact. The shadow row is persisted with the error
// captured in error_text.
func TestShadowDispatcher_ShadowErrorDoesntFailPrimary(t *testing.T) {
	db, mock := openMock(t)
	defer db.Close()

	armsJSON, _ := MarshalArms(stubArms())
	rows := experimentRows().AddRow(
		"00000000-0000-0000-0000-000000000002",
		"shadow_fail", "",
		string(ScopeGlobal), "",
		stringArray(),
		armsJSON,
		float64Array(1.0, 0.0),
		string(StatusRunning),
		time.Time{}, time.Time{},
		int64(0), int64(0), "",
		time.Now(), time.Now(),
	)
	mock.ExpectQuery(`FROM model_ab_experiments\s+WHERE status = ANY`).WillReturnRows(rows)
	mock.ExpectQuery(`INSERT INTO model_ab_assignments`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "arm_index", "arm_name", "assigned_at"}).
			AddRow("00000000-0000-0000-0000-000000000078", 0, "control", time.Now()))
	mock.ExpectExec(`INSERT INTO model_ab_shadow_responses`).WillReturnResult(sqlmock.NewResult(1, 1))
	// No tokens UPDATE expected: shadow erred → resp is nil → no AddTokens call.

	repo := NewRepo(db)
	resolver := NewResolver(repo)
	resolver.Logger = discardLogger()
	stub := &chatFanoutClient{shadowErr: errors.New("provider 429")}
	d := NewShadowDispatcher(stub, resolver, repo, HookContext{})
	d.Logger = discardLogger()

	resp, err := d.Chat(context.Background(), llm.ChatRequest{
		FundID: "f1", AgentID: "ag", AgentRole: "pm", StepName: "pm_decision", RunID: "run-2",
	})
	if err != nil {
		t.Fatalf("primary call must succeed despite shadow error, got %v", err)
	}
	if resp == nil || resp.Content != "primary-ok" {
		t.Fatalf("expected primary-ok response, got %+v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// TestShadowDispatcher_PrimaryErrorReturnedToCaller verifies the
// inverse: a shadow that returned ok doesn't mask the primary's
// failure.
func TestShadowDispatcher_PrimaryErrorReturnedToCaller(t *testing.T) {
	db, mock := openMock(t)
	defer db.Close()

	armsJSON, _ := MarshalArms(stubArms())
	rows := experimentRows().AddRow(
		"00000000-0000-0000-0000-000000000003",
		"primary_fail", "",
		string(ScopeGlobal), "",
		stringArray(),
		armsJSON,
		float64Array(1.0, 0.0),
		string(StatusRunning),
		time.Time{}, time.Time{},
		int64(0), int64(0), "",
		time.Now(), time.Now(),
	)
	mock.ExpectQuery(`FROM model_ab_experiments\s+WHERE status = ANY`).WillReturnRows(rows)
	mock.ExpectQuery(`INSERT INTO model_ab_assignments`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "arm_index", "arm_name", "assigned_at"}).
			AddRow("00000000-0000-0000-0000-000000000079", 0, "control", time.Now()))
	mock.ExpectExec(`INSERT INTO model_ab_shadow_responses`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`UPDATE model_ab_experiments\s+SET tokens_used`).WillReturnResult(sqlmock.NewResult(1, 1))

	stub := &chatFanoutClient{primaryErr: errors.New("primary boom")}
	d := NewShadowDispatcher(stub, NewResolver(NewRepo(db)), NewRepo(db), HookContext{})
	d.Logger = discardLogger()

	_, err := d.Chat(context.Background(), llm.ChatRequest{
		FundID: "f1", AgentID: "ag", AgentRole: "pm", StepName: "pm_decision", RunID: "run-3",
	})
	if err == nil {
		t.Fatalf("expected primary error to propagate to caller")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}
