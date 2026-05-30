package api

import (
	"context"
	"net/http"
)

// ABShadowAgentService is the read-only contract behind
// GET /api/abtests/{testId}/shadow-agents.
//
// It surfaces — for both A and B variants — the per-agent
// shadow learning timeline that the analyzer wrote into
// ab_test_agent_learning_events plus the variant_memory
// rows. The data is already in the database; this endpoint
// only exposes it to the comparison UI so users can see
// "what did the alternative strategy's agents actually
// learn?" without having to dry-run a promotion first.
//
// Permissions: implementations MUST authorise the caller
// against BOTH the control fund and the treatment fund of
// the test (matches the existing ABTestService pattern).
//
// nil-safety: the handler returns 503 when this field is
// not wired so legacy deployments degrade cleanly.
type ABShadowAgentService interface {
	ShadowAgents(ctx context.Context, userID, testID string) (ABTestShadowAgentResponse, error)
}

// ABTestShadowAgentResponse is the wire envelope returned
// by GetABShadowAgents.
//
// Variants is always exactly two elements, ordered A then
// B, even when one side has no learning events — the empty
// side gets an empty Agents slice so the client can render
// a side-by-side comparison without nil checks.
type ABTestShadowAgentResponse struct {
	TestID   string                     `json:"testId"`
	Variants []ABTestShadowAgentVariant `json:"variants"`
}

// ABTestShadowAgentVariant is one side of the comparison.
// StrategyConfig echoes what the variant was configured
// with at start-time so the UI can label the column with
// the original B-side parameter delta.
type ABTestShadowAgentVariant struct {
	VariantKey     string              `json:"variantKey"`
	VariantName    string              `json:"variantName"`
	StrategyConfig map[string]any      `json:"strategyConfig,omitempty"`
	Agents         []ABTestShadowAgent `json:"agents"`
}

// ABTestShadowAgent is one agent's aggregated learning
// across the test window. Lessons / Adjustments / Summaries
// are deduped and capped server-side so the response stays
// bounded for long-running tests.
type ABTestShadowAgent struct {
	AgentID                string                  `json:"agentId"`
	AgentName              string                  `json:"agentName,omitempty"`
	Role                   string                  `json:"role,omitempty"`
	EventCount             int                     `json:"eventCount"`
	LatestTradingDate      string                  `json:"latestTradingDate,omitempty"`
	Lessons                []string                `json:"lessons,omitempty"`
	Adjustments            []string                `json:"adjustments,omitempty"`
	Summaries              []string                `json:"summaries,omitempty"`
	SpecializationLearning []map[string]any        `json:"specializationLearning,omitempty"`
	ProposedEvolutionDiff  *ABEvolutionConfigDiff  `json:"proposedEvolutionDiff,omitempty"`
	Memories               []ABTestShadowMemory    `json:"memories,omitempty"`
	Timeline               []ABTestShadowAgentDay  `json:"timeline,omitempty"`
}

// ABEvolutionConfigDiff is the projected delta between
// the agent's CURRENT live evolution_config and what the
// shadow run is proposing. Empty maps/slices are omitted
// so the UI can hide the "diff" section when there's
// nothing to highlight.
//
// Changed values are tuples [previous, proposed]; the
// JSON shape is a 2-element array.
type ABEvolutionConfigDiff struct {
	Added   map[string]any   `json:"added,omitempty"`
	Changed map[string][2]any `json:"changed,omitempty"`
	Removed []string          `json:"removed,omitempty"`
}

// ABTestShadowAgentDay is one day's collapsed summary on
// the per-agent timeline. The UI uses this to draw a
// compact "what did A learn on 2026-05-26 vs what did B
// learn on 2026-05-26" timeline view.
type ABTestShadowAgentDay struct {
	TradingDate string   `json:"tradingDate"`
	Summary     string   `json:"summary,omitempty"`
	Lessons     []string `json:"lessons,omitempty"`
	Adjustments []string `json:"adjustments,omitempty"`
}

// ABTestShadowMemory is a flattened ab_test_variant_memory
// row. Layer is e.g. "shadow" / "long_term"; Content is
// the raw JSON payload passed through unchanged so the
// UI can render whatever shape the writer used.
type ABTestShadowMemory struct {
	MemoryKey   string         `json:"memoryKey"`
	Layer       string         `json:"layer"`
	TradingDate string         `json:"tradingDate,omitempty"`
	Content     map[string]any `json:"content,omitempty"`
}

// WithABShadowAgentService injects the shadow agent
// service. Idempotent. Safe to call with nil to disable
// the endpoint (handler returns 503).
func (h *FundHandler) WithABShadowAgentService(svc ABShadowAgentService) *FundHandler {
	if h != nil {
		h.abShadowAgents = svc
	}
	return h
}

// GetABShadowAgents implements
//
//	GET /api/abtests/{testId}/shadow-agents
//
// Behaviour:
//   - Unauthenticated → 401.
//   - testId missing → 400.
//   - Service unwired → 503 (deployments without AB
//     analysis pipeline stay healthy).
//   - User not a member of either side's fund → 403 via
//     handleServiceError.
//   - Test not found → 404 via handleServiceError.
//   - Happy path → 200 with the comparison envelope.
//
// The endpoint is read-only and idempotent; it does NOT
// trigger a re-analysis. Callers that want fresh data
// must POST /analyze first.
func (h *FundHandler) GetABShadowAgents(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	testID := pathValue(r, "testId")
	if !requireNonEmpty(w, testID, "testId") {
		return
	}
	if h.abShadowAgents == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "ab shadow agent service unavailable",
		})
		return
	}

	resp, err := h.abShadowAgents.ShadowAgents(r.Context(), userID, testID)
	if err != nil {
		handleServiceError(w, err, "A/B shadow agents")
		return
	}
	if resp.Variants == nil {
		resp.Variants = []ABTestShadowAgentVariant{}
	}
	for i := range resp.Variants {
		if resp.Variants[i].Agents == nil {
			resp.Variants[i].Agents = []ABTestShadowAgent{}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
