package main

// P2-T2 routing tests. The runDecisionEngine path (P2-T1) already
// honours per-agent model_provider / model_name selections by
// forwarding pmAgent.UserID + AgentID through DecisionInput. T2
// closes the loop for the two other LLM-driven workflow steps —
// debate (bull / bear / quant researchers) and sentiment scoring —
// which both used to send blank UserID / AgentID to the LLM router
// and therefore always fell through to the platform .env default.
//
// The contract these tests pin:
//
//   1. resolveFundOperatorRouting prefers any researcher in the team
//      and returns (researcher.UserID, researcher.AgentID). When the
//      team has no researcher it falls back to the first member's
//      UserID with a blank AgentID, so sentiment can still resolve
//      userDefaults[simple] even though no agent-level override
//      exists.
//   2. resolveFundOperatorRouting returns ("", "") for a missing or
//      empty team — callers should leave the routing fields blank
//      and the router will hit the platform .env default, which is
//      the previous behaviour.
//   3. buildDebateRoundtable stamps the same (UserID, AgentID) onto
//      all three LLMResearcher personas — they're rhetorical roles
//      inside one researcher's brain, not distinct agents.
//   4. buildSentimentScorerFromRuntime stamps UserID onto LLMScorer
//      while keeping AgentID as the "sentiment-scorer" sentinel so
//      the router falls through agentDefaults → userDefaults.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/debate"
	"github.com/fundai/server/internal/llm"
	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/sentiment"
)

// expectTeamRows mocks teamRepo.ListByFund(fundID) with the given
// (agentID, role) pairs. Tests inject members in the order the repo
// is expected to scan them.
func expectTeamRows(mock sqlmock.Sqlmock, fundID string, members []struct {
	AgentID string
	Role    string
}) {
	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{
		"id", "fund_id", "agent_id", "role", "focus", "joined_at", "status", "updated_at",
	})
	for i, m := range members {
		rows.AddRow(
			"member-"+m.AgentID,
			fundID,
			m.AgentID,
			m.Role,
			nil,
			now.Add(time.Duration(i)*time.Minute),
			"active",
			now,
		)
	}
	mock.ExpectQuery(`(?s)SELECT.*FROM fund_team_members WHERE fund_id`).
		WithArgs(fundID).WillReturnRows(rows)
}

// expectAgentByID mocks agentRepo.GetByID(agentID) with the row the
// resolver scans. The projection here mirrors the patched query in
// fund_repo.go (P2-T1 fix that re-added user_id + marketplace
// snapshot columns).
//
// pending_marketplace_snapshot is a json.RawMessage which can't be
// nil-scanned by database/sql (it doesn't implement Scanner for nil
// values), so the mock always returns a literal "null" byte slice
// for that column. Same trick the agent_repo unit tests use.
func expectAgentByID(mock sqlmock.Sqlmock, agentID, userID, role string) {
	now := time.Now().UTC()
	mock.ExpectQuery(`(?s)SELECT.*FROM agents WHERE id = \$1`).
		WithArgs(agentID).WillReturnRows(
		sqlmock.NewRows([]string{
			"id", "user_id", "name", "role", "focus", "llm_model", "model_provider", "model_name", "system_prompt",
			"skill_config", "domain_config", "evolution_config", "pending_marketplace_snapshot",
			"marketplace_snapshot_imported_at", "status", "created_at", "updated_at",
		}).AddRow(
			agentID, userID, "agent-"+agentID, role, nil, nil, nil, nil, nil,
			[]byte(`{}`), []byte(`{}`), []byte(`{}`), []byte(`null`), nil, "active", now, now,
		),
	)
}

// On a team that contains both a PM and a researcher the resolver
// returns the researcher's identity (its model preference is what
// debate + sentiment should track). The PM agent must not be loaded
// at all — the resolver short-circuits as soon as the researcher
// scan succeeds.
func TestResolveFundOperatorRoutingPrefersResearcher(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	expectTeamRows(mock, "fund-1", []struct {
		AgentID string
		Role    string
	}{
		{AgentID: "agent-pm", Role: "pm"},
		{AgentID: "agent-researcher", Role: "researcher"},
	})
	// Only the researcher's GetByID is expected — the first-pass
	// short-circuits before the PM is ever inspected.
	expectAgentByID(mock, "agent-researcher", "user-tong", "researcher")

	user, agent := resolveFundOperatorRouting(
		context.Background(),
		"fund-1",
		repository.NewTeamRepo(db),
		repository.NewAgentRepo(db),
	)
	if user != "user-tong" {
		t.Errorf("user = %q, want user-tong", user)
	}
	if agent != "agent-researcher" {
		t.Errorf("agent = %q, want agent-researcher", agent)
	}
	assertMockExpectations(t, mock)
}

// When the team has no researcher we still want a UserID so the
// sentiment scorer can route via userDefaults[simple]. AgentID
// stays blank (no per-agent preference to honour).
func TestResolveFundOperatorRoutingFallsBackToAnyMemberWhenNoResearcher(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	expectTeamRows(mock, "fund-2", []struct {
		AgentID string
		Role    string
	}{
		{AgentID: "agent-pm", Role: "pm"},
		{AgentID: "agent-trader", Role: "trader"},
	})
	// First pass: no researcher row → no GetByID.
	// Second pass: first member (PM) is loaded.
	expectAgentByID(mock, "agent-pm", "user-tong", "pm")

	user, agent := resolveFundOperatorRouting(
		context.Background(),
		"fund-2",
		repository.NewTeamRepo(db),
		repository.NewAgentRepo(db),
	)
	if user != "user-tong" {
		t.Errorf("user = %q, want user-tong", user)
	}
	if agent != "" {
		t.Errorf("agent = %q, want '' (no per-agent override when no researcher)", agent)
	}
	assertMockExpectations(t, mock)
}

// Empty team yields blank routing hints. Callers fall through to
// the platform .env default — the safe behaviour we had before T2.
func TestResolveFundOperatorRoutingEmptyTeamYieldsBlank(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	expectTeamRows(mock, "fund-3", nil)

	user, agent := resolveFundOperatorRouting(
		context.Background(),
		"fund-3",
		repository.NewTeamRepo(db),
		repository.NewAgentRepo(db),
	)
	if user != "" || agent != "" {
		t.Errorf("expected ('','') for empty team, got (%q,%q)", user, agent)
	}
	assertMockExpectations(t, mock)
}

// Nil repos / blank fundID short-circuit without panicking. Belt-
// and-suspenders for the resolveSentimentScorer fast path that
// calls into here with a freshly-constructed teamRepo even when
// the adapter's db is nil.
func TestResolveFundOperatorRoutingDefensiveAgainstNil(t *testing.T) {
	if u, a := resolveFundOperatorRouting(context.Background(), "fund-x", nil, nil); u != "" || a != "" {
		t.Errorf("nil repos: expected ('',''), got (%q,%q)", u, a)
	}
	db, _ := newMockDB(t)
	defer db.Close()
	if u, a := resolveFundOperatorRouting(context.Background(), "", repository.NewTeamRepo(db), repository.NewAgentRepo(db)); u != "" || a != "" {
		t.Errorf("blank fundID: expected ('',''), got (%q,%q)", u, a)
	}
}

// buildDebateRoundtable stamps the same (UserID, AgentID) onto all
// three personas. The router will see one consistent agent identity
// for the whole debate; if the operator picked claude-opus-4-7 on
// the researcher, all three roles use it.
func TestBuildDebateRoundtableStampsRoutingOnAllPersonas(t *testing.T) {
	runtime := &llmRuntime{client: llm.NewMultiProviderClient(nil, nil)}
	rt := buildDebateRoundtable(runtime, "fund-1", "user-tong", "agent-researcher")
	if rt == nil {
		t.Fatal("buildDebateRoundtable returned nil")
	}
	llmRT, ok := rt.(*debate.LLMRoundtable)
	if !ok {
		t.Fatalf("expected *debate.LLMRoundtable, got %T", rt)
	}
	if len(llmRT.Researchers) != 3 {
		t.Fatalf("expected 3 researchers, got %d", len(llmRT.Researchers))
	}
	wantRoles := []debate.AgentRole{debate.RoleBull, debate.RoleBear, debate.RoleQuant}
	for i, r := range llmRT.Researchers {
		lr, ok := r.(*debate.LLMResearcher)
		if !ok {
			t.Errorf("researcher[%d]: expected *LLMResearcher, got %T", i, r)
			continue
		}
		if lr.PersonaRole != wantRoles[i] {
			t.Errorf("researcher[%d]: role = %q, want %q", i, lr.PersonaRole, wantRoles[i])
		}
		if lr.UserID != "user-tong" {
			t.Errorf("researcher[%d]: UserID = %q, want user-tong", i, lr.UserID)
		}
		if lr.AgentID != "agent-researcher" {
			t.Errorf("researcher[%d]: AgentID = %q, want agent-researcher", i, lr.AgentID)
		}
		if lr.FundID != "fund-1" {
			t.Errorf("researcher[%d]: FundID = %q, want fund-1", i, lr.FundID)
		}
	}
}

// Whitespace in either routing field gets trimmed away so a stray
// space in the team row doesn't accidentally turn into an agentID
// the router will never match.
func TestBuildDebateRoundtableTrimsRoutingWhitespace(t *testing.T) {
	runtime := &llmRuntime{client: llm.NewMultiProviderClient(nil, nil)}
	rt := buildDebateRoundtable(runtime, "fund-1", "  user-tong  ", "\tagent-researcher\n")
	llmRT := rt.(*debate.LLMRoundtable)
	first := llmRT.Researchers[0].(*debate.LLMResearcher)
	if first.UserID != "user-tong" {
		t.Errorf("UserID not trimmed: got %q", first.UserID)
	}
	if first.AgentID != "agent-researcher" {
		t.Errorf("AgentID not trimmed: got %q", first.AgentID)
	}
}

// Sentiment scorer keeps the sentinel AgentID ("sentiment-scorer")
// — it intentionally never matches a real agent row, so the router
// skips agentDefaults and uses userDefaults[simple] when one is
// configured. UserID is what makes that fallback possible.
func TestBuildSentimentScorerFromRuntimeStampsUserID(t *testing.T) {
	t.Setenv("SENTIMENT_DISABLED", "")
	t.Setenv("SENTIMENT_LLM_DISABLED", "")
	runtime := &llmRuntime{client: llm.NewMultiProviderClient(nil, nil)}
	got := buildSentimentScorerFromRuntime(runtime, "fund-1", "user-tong")
	comp, ok := got.(*sentiment.CompositeScorer)
	if !ok {
		t.Fatalf("expected CompositeScorer, got %T", got)
	}
	llmScorer, ok := comp.Primary.(*sentiment.LLMScorer)
	if !ok {
		t.Fatalf("expected LLMScorer as primary, got %T", comp.Primary)
	}
	if llmScorer.UserID != "user-tong" {
		t.Errorf("UserID = %q, want user-tong", llmScorer.UserID)
	}
	if llmScorer.AgentID != "sentiment-scorer" {
		t.Errorf("AgentID = %q, want 'sentiment-scorer' sentinel", llmScorer.AgentID)
	}
	if llmScorer.FundID != "fund-1" {
		t.Errorf("FundID = %q, want fund-1", llmScorer.FundID)
	}
}

// Whitespace-only UserID is normalised to blank so the router
// treats the call as "no user context", matching the empty-team
// branch.
func TestBuildSentimentScorerFromRuntimeTrimsBlankUserID(t *testing.T) {
	t.Setenv("SENTIMENT_DISABLED", "")
	t.Setenv("SENTIMENT_LLM_DISABLED", "")
	runtime := &llmRuntime{client: llm.NewMultiProviderClient(nil, nil)}
	got := buildSentimentScorerFromRuntime(runtime, "fund-1", "   ")
	comp := got.(*sentiment.CompositeScorer)
	llmScorer := comp.Primary.(*sentiment.LLMScorer)
	if llmScorer.UserID != "" {
		t.Errorf("blank UserID should normalise to '', got %q", llmScorer.UserID)
	}
}

// Compile-time guard that sql.NullString remains the agent column
// type — if the schema migration ever changes type we want this
// test file's mock rows to break loudly so we don't silently lose
// the routing fix.
var _ sql.NullString = (repository.Agent{}).LLMModel
