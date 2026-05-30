package main

import (
	"context"
	"errors"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/llm"
)

// fundAssistAdapter bridges the LLM client (which speaks the rich
// llm.ChatRequest / ChatResponse protocol) into the tiny api.FundAssistService
// interface. We do this in cmd/server rather than internal/api/ for
// the same reason the other adapters live here: the llm package has
// a heavy dep graph (router, observers, owner limiters) and the api
// layer should stay swappable / testable without dragging it in.
//
// The adapter sets only what assist actually needs: a step name (so
// the LLM observability lights up "fund_assist" instead of "unknown")
// and a deliberately conservative max-token cap (1024) — assist
// returns a JSON skeleton, not an essay.
type fundAssistAdapter struct {
	client llm.LLMClient
	// tier picks "standard" by default; cheaper than "advanced"
	// and good enough for structured-JSON extraction. Centralised
	// here so any future "make assist use tier=simple" is a
	// one-line change.
	tier llm.ModelTier
}

// newFundAssistAdapter returns nil when the LLM client is missing
// (e.g. dev box without keys configured), matching the rest of the
// codebase's "nil-safe → handler returns 503" pattern.
func newFundAssistAdapter(client llm.LLMClient) *fundAssistAdapter {
	if client == nil {
		return nil
	}
	return &fundAssistAdapter{
		client: client,
		tier:   llm.TierStandard,
	}
}

// Chat satisfies api.FundAssistService. Returns the LLM's raw text;
// any structured-JSON parsing happens in the api layer (so the
// adapter doesn't have to track schema drift).
//
// userID is plumbed into ChatRequest.UserID so the per-owner billing
// + rate-limiter wiring already in MultiProviderClient applies
// correctly. We deliberately leave AgentID / FundID empty: assist
// runs BEFORE the fund exists, so charging it against an agent / fund
// that doesn't exist yet would corrupt the per-fund quota dashboard.
func (a *fundAssistAdapter) Chat(ctx context.Context, userID, system, user string) (string, error) {
	if a == nil || a.client == nil {
		return "", errors.New("fund assist adapter has no llm client")
	}
	resp, err := a.client.Chat(ctx, llm.ChatRequest{
		UserID:    userID,
		StepName:  "fund_assist",
		ModelTier: a.tier,
		MaxTokens: 1024,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	})
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", errors.New("llm returned nil response")
	}
	return resp.Content, nil
}

// Compile-time check: adapter satisfies the api interface. If this
// breaks (e.g. the api type adds a new method), CI fails loudly here
// instead of mysteriously at handler-construction time.
var _ api.FundAssistService = (*fundAssistAdapter)(nil)
