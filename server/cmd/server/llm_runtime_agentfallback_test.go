package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/llm"
	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/subscription"
)

// fakeAgentLister is a stub for agentModelLister that returns a fixed
// fleet of agents per user without touching the database.
type fakeAgentLister struct {
	byUser map[string][]repository.Agent
	owners []string
	err    error
}

func (f *fakeAgentLister) ListByUser(_ context.Context, userID string) ([]repository.Agent, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byUser[userID], nil
}

func (f *fakeAgentLister) ListDistinctOwners(_ context.Context) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return append([]string(nil), f.owners...), nil
}

func newAgentRow(id, userID, role, provider, modelName string) repository.Agent {
	return repository.Agent{
		ID:            id,
		UserID:        userID,
		Role:          role,
		ModelProvider: sql.NullString{String: provider, Valid: provider != ""},
		ModelName:     sql.NullString{String: modelName, Valid: modelName != ""},
	}
}

func newRuntimeWithEmptyConfigs(t *testing.T, lister agentModelLister) (*llmRuntime, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	router := llm.NewModelRouter(map[llm.Provider]string{
		llm.ProviderClaude: "system-claude-key",
		llm.ProviderGemini: "system-gemini-key",
	}, llm.DefaultModels, nil, nil)
	runtime := &llmRuntime{
		router:       router,
		modelConfigs: subscription.NewModelConfigService(db),
		syncedUsers:  map[string]struct{}{},
		agentRepo:    lister,
	}
	cleanup := func() { db.Close() }
	return runtime, mock, cleanup
}

// expectEmptyRuntimeConfigsQuery primes the sqlmock to return zero rows
// for the ListRuntimeConfigs SELECT. Use this when the test is about
// the agents-table fallback path and we don't want any explicit
// user_model_configs rows interfering.
func expectEmptyRuntimeConfigsQuery(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`
		SELECT user_id, agent_id, config_type, tier, provider, model_name, base_url, api_key_encrypted, is_active
		FROM user_model_configs
		WHERE is_active = true
		ORDER BY user_id ASC, agent_id ASC NULLS FIRST, tier ASC NULLS LAST, created_at ASC
	`).WillReturnRows(sqlmock.NewRows([]string{
		"user_id", "agent_id", "config_type", "tier", "provider", "model_name", "base_url", "api_key_encrypted", "is_active",
	}))
}

// TestSyncUserAgentsTableFallbackPopulatesRouterAgentDefaults pins down
// the P2 fix: a user who configured PM agent's model via the agent
// editor (which writes agents.model_provider / model_name only) must
// still see those preferences reach ModelRouter.agentDefaults. Before
// the fix, the router only consulted user_model_configs and every PM
// LLM call routed to the platform default — that was tong's bug.
func TestSyncUserAgentsTableFallbackPopulatesRouterAgentDefaults(t *testing.T) {
	lister := &fakeAgentLister{
		byUser: map[string][]repository.Agent{
			"user-tong": {
				newAgentRow("agent-pm", "user-tong", "pm", "claude", "claude-sonnet-4-20250514"),
				newAgentRow("agent-bull", "user-tong", "researcher", "", ""), // provider blank → skipped
			},
		},
	}
	runtime, mock, cleanup := newRuntimeWithEmptyConfigs(t, lister)
	defer cleanup()
	expectEmptyRuntimeConfigsQuery(mock)

	if err := runtime.SyncUser(context.Background(), "user-tong"); err != nil {
		t.Fatalf("SyncUser: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}

	// Resolve via the router as the LLM decision engine would.
	cfg, err := runtime.router.ResolveModel(context.Background(), &llm.ChatRequest{
		UserID:    "user-tong",
		AgentID:   "agent-pm",
		ModelTier: llm.TierCritical,
	})
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if cfg == nil {
		t.Fatal("ResolveModel returned nil config")
	}
	if cfg.Provider != llm.ProviderClaude {
		t.Fatalf("provider = %q, want claude", cfg.Provider)
	}
	if cfg.ModelName != "claude-sonnet-4-20250514" {
		t.Fatalf("modelName = %q, want claude-sonnet-4-20250514", cfg.ModelName)
	}
}

// TestSyncUserExplicitConfigStillWins guards the merge precedence: an
// explicit user_model_configs row for an agent must override the
// agents-table fallback. Otherwise operators who deliberately picked
// a per-fund override in the model-config API would be silently
// overwritten by whatever model_name happens to be on the agents row.
func TestSyncUserExplicitConfigStillWins(t *testing.T) {
	lister := &fakeAgentLister{
		byUser: map[string][]repository.Agent{
			"user-x": {
				newAgentRow("agent-1", "user-x", "pm", "claude", "claude-sonnet-4-20250514"),
			},
		},
	}
	runtime, mock, cleanup := newRuntimeWithEmptyConfigs(t, lister)
	defer cleanup()

	mock.ExpectQuery(`
		SELECT user_id, agent_id, config_type, tier, provider, model_name, base_url, api_key_encrypted, is_active
		FROM user_model_configs
		WHERE is_active = true
		ORDER BY user_id ASC, agent_id ASC NULLS FIRST, tier ASC NULLS LAST, created_at ASC
	`).WillReturnRows(sqlmock.NewRows([]string{
		"user_id", "agent_id", "config_type", "tier", "provider", "model_name", "base_url", "api_key_encrypted", "is_active",
	}).AddRow("user-x", "agent-1", "agent_default", nil, "gemini", "gemini-3.1-pro-preview", "https://generativelanguage.googleapis.com/v1beta", nil, true))

	if err := runtime.SyncUser(context.Background(), "user-x"); err != nil {
		t.Fatalf("SyncUser: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}

	cfg, err := runtime.router.ResolveModel(context.Background(), &llm.ChatRequest{
		UserID:    "user-x",
		AgentID:   "agent-1",
		ModelTier: llm.TierCritical,
	})
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if cfg.Provider != llm.ProviderGemini {
		t.Fatalf("explicit user_model_configs row should win, got provider=%q", cfg.Provider)
	}
	if cfg.ModelName != "gemini-3.1-pro-preview" {
		t.Fatalf("explicit user_model_configs row should win, got modelName=%q", cfg.ModelName)
	}
}

// TestSyncAllDiscoversAgentsTableOnlyUsers ensures users who never
// touched user_model_configs (i.e. their entire model preference is
// in agents.*) still get synced at startup. Without this, the
// router's agentDefaults remains empty for them until something
// triggers an explicit SyncUser — which for read-only fund operators
// might be never.
func TestSyncAllDiscoversAgentsTableOnlyUsers(t *testing.T) {
	lister := &fakeAgentLister{
		owners: []string{"user-only-in-agents"},
		byUser: map[string][]repository.Agent{
			"user-only-in-agents": {
				newAgentRow("agent-pm", "user-only-in-agents", "pm", "claude", "claude-sonnet-4-20250514"),
			},
		},
	}
	runtime, mock, cleanup := newRuntimeWithEmptyConfigs(t, lister)
	defer cleanup()
	expectEmptyRuntimeConfigsQuery(mock)

	if err := runtime.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
	if _, ok := runtime.syncedUsers["user-only-in-agents"]; !ok {
		t.Fatalf("syncedUsers missing agents-only user, got %v", runtime.syncedUsers)
	}
	cfg, err := runtime.router.ResolveModel(context.Background(), &llm.ChatRequest{
		UserID:    "user-only-in-agents",
		AgentID:   "agent-pm",
		ModelTier: llm.TierCritical,
	})
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if cfg.Provider != llm.ProviderClaude {
		t.Fatalf("provider = %q, want claude (agents fallback should fire under SyncAll)", cfg.Provider)
	}
}

// TestSyncUserAgentRepoErrorDoesNotBreakRunningConfigs locks in the
// best-effort contract: if the agents-table fallback fails for any
// reason (DB blip, permissions), SyncUser still wires up the explicit
// user_model_configs rows for that user and returns no error. The
// pre-P2 behaviour is preserved on failure — that's the conservative
// thing to do for a fallback feature.
func TestSyncUserAgentRepoErrorDoesNotBreakRunningConfigs(t *testing.T) {
	lister := &fakeAgentLister{err: errors.New("simulated DB outage")}
	runtime, mock, cleanup := newRuntimeWithEmptyConfigs(t, lister)
	defer cleanup()

	mock.ExpectQuery(`
		SELECT user_id, agent_id, config_type, tier, provider, model_name, base_url, api_key_encrypted, is_active
		FROM user_model_configs
		WHERE is_active = true
		ORDER BY user_id ASC, agent_id ASC NULLS FIRST, tier ASC NULLS LAST, created_at ASC
	`).WillReturnRows(sqlmock.NewRows([]string{
		"user_id", "agent_id", "config_type", "tier", "provider", "model_name", "base_url", "api_key_encrypted", "is_active",
	}).AddRow("user-y", "agent-9", "agent_default", nil, "claude", "claude-sonnet-4-20250514", "https://api.anthropic.com", nil, true))

	if err := runtime.SyncUser(context.Background(), "user-y"); err != nil {
		t.Fatalf("SyncUser should not return error on agents-table outage: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
	cfg, err := runtime.router.ResolveModel(context.Background(), &llm.ChatRequest{
		UserID:    "user-y",
		AgentID:   "agent-9",
		ModelTier: llm.TierCritical,
	})
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if cfg.Provider != llm.ProviderClaude {
		t.Fatalf("explicit row should still resolve, got provider=%q", cfg.Provider)
	}
}

// TestAgentRowToModelConfigNormalisesProviderCase makes sure operator
// typos like "Claude" or "GEMINI" in the agents row aren't silently
// dropped because the router compares providers as lowercase strings.
func TestAgentRowToModelConfigNormalisesProviderCase(t *testing.T) {
	cases := []struct {
		in       string
		want     llm.Provider
		wantBase string // partial match — provider default base URL contains this
	}{
		{"Claude", llm.ProviderClaude, "anthropic"},
		{"GEMINI", llm.ProviderGemini, "googleapis"},
		{"openai", llm.ProviderOpenAI, "openai"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			cfg := agentRowToModelConfig(tc.in, "some-model")
			if cfg == nil {
				t.Fatalf("expected non-nil config for provider %q", tc.in)
			}
			if cfg.Provider != tc.want {
				t.Fatalf("provider = %q, want %q", cfg.Provider, tc.want)
			}
			if cfg.ModelName != "some-model" {
				t.Fatalf("modelName = %q, want some-model", cfg.ModelName)
			}
			if tc.wantBase != "" && !strings.Contains(cfg.BaseURL, tc.wantBase) {
				t.Fatalf("baseURL = %q, want substring %q", cfg.BaseURL, tc.wantBase)
			}
		})
	}
}

// TestAgentRowToModelConfigRejectsBlankInputs is a small sanity test
// that the helper refuses to fabricate configs from incomplete agent
// rows. Otherwise a half-edited row would silently override the
// platform default with an unusable empty model name.
func TestAgentRowToModelConfigRejectsBlankInputs(t *testing.T) {
	if cfg := agentRowToModelConfig("", "claude-sonnet"); cfg != nil {
		t.Fatalf("blank provider should not produce a config, got %+v", cfg)
	}
	if cfg := agentRowToModelConfig("claude", ""); cfg != nil {
		t.Fatalf("blank modelName should not produce a config, got %+v", cfg)
	}
}
