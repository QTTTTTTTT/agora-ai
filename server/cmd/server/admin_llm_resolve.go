// admin_llm_resolve.go — super-admin dry-run for the LLM resolver.
//
// The companion to internal/llm/resolve_trace.go on the HTTP side.
// Lets an operator answer "if I submit a chat for user X / agent Y
// / step Z, which layer of the 9-layer chain will pick the model,
// and what model is that?" — WITHOUT actually firing a real LLM
// call. No tokens are spent, no DB rows are written.
//
// Endpoint
//   GET /api/admin/llm-resolve
//     ?user_id=<uuid>
//     &agent_id=<string>           (optional)
//     &step=<string>                (optional, e.g. "roundtable_summary")
//     &model=<string>               (optional, forces explicit_model lookup)
//     &tier=<critical|standard|simple>  (optional, only used when step is empty)
//     &fund_id=<uuid>               (optional, drives fundOverrideHook)
//
// Auth
//   super_admin only — leaks model name + provider + base URL,
//   which is operationally sensitive. The handler scrubs API keys
//   from the response before serializing so even a misconfigured
//   audit log can't accidentally exfiltrate a key.
//
// Why dry-run (no actual chat)
//
//   1. Cheap — no upstream HTTP call.
//   2. Safe — won't trigger A/B exposures that affect downstream
//      attribution data. (The A/B hook IS invoked because the
//      resolution chain runs it, but the upstream call that would
//      record the exposure does not happen.)
//   3. Idempotent — operators can replay the same query while
//      iterating on overrides.

package main

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/fundai/server/internal/llm"
)

// registerLLMResolveAdminRoute mounts the dry-run endpoint. nil
// handler / router = no-op so a degraded boot (no LLM runtime) does
// not register a route that would 500.
func (h *adminHandler) registerLLMResolveAdminRoute(mux *http.ServeMux) {
	if h == nil || mux == nil || h.modelRouter == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/llm-resolve", h.handleLLMResolveDryRun)
}

// handleLLMResolveDryRun is the HTTP entry point. Returns 200 with
// `{resolved: {...}, trace: [...]}`. On a resolution error (e.g.
// the tier-guard rejecting the user) still returns 200 with the
// trace so the operator can see "we got to layer N before the
// guard fired".
func (h *adminHandler) handleLLMResolveDryRun(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.modelRouter == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "model router not configured"))
		return
	}

	req := buildDryRunChatRequest(r)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	cfg, trace, err := h.modelRouter.ResolveModelWithTrace(ctx, req)

	resp := map[string]any{
		"request": map[string]any{
			"user_id":  req.UserID,
			"owner_id": req.OwnerID,
			"agent_id": req.AgentID,
			"fund_id":  req.FundID,
			"step":     req.StepName,
			"model":    req.Model,
			"tier":     string(req.ModelTier),
		},
		"trace": trace,
	}
	if err != nil {
		resp["error"] = err.Error()
		resp["resolved"] = nil
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["resolved"] = scrubModelConfig(cfg)
	writeJSON(w, http.StatusOK, resp)
}

// buildDryRunChatRequest assembles the synthetic ChatRequest from
// query parameters. Missing params are left as the zero value —
// the resolver treats those as "not specified" and chooses the
// appropriate fallback in the priority chain.
func buildDryRunChatRequest(r *http.Request) *llm.ChatRequest {
	q := r.URL.Query()
	req := &llm.ChatRequest{
		UserID:   strings.TrimSpace(q.Get("user_id")),
		OwnerID:  strings.TrimSpace(q.Get("owner_id")),
		AgentID:  strings.TrimSpace(q.Get("agent_id")),
		FundID:   strings.TrimSpace(q.Get("fund_id")),
		StepName: strings.TrimSpace(q.Get("step")),
		Model:    strings.TrimSpace(q.Get("model")),
	}
	if tier := strings.TrimSpace(q.Get("tier")); tier != "" {
		req.ModelTier = llm.ModelTier(tier)
	}
	return req
}

// scrubModelConfig returns a JSON-safe view of the resolved model.
// CRITICAL: never include the raw APIKey in the response. The
// dry-run endpoint is operator-facing but the response could end
// up in browser dev-tools, screenshots, support tickets, etc.
//
// We DO expose the base URL + provider + model name + whether the
// resolved config is a BYOK key (UsesCustomKey) because those are
// what the operator actually wants to verify ("did we end up on
// the user's BYOK or on the platform pool?").
func scrubModelConfig(cfg *llm.ModelConfig) map[string]any {
	if cfg == nil {
		return nil
	}
	apiKeyHint := ""
	if cfg.APIKey != "" {
		if len(cfg.APIKey) >= 8 {
			apiKeyHint = cfg.APIKey[:4] + "..." + cfg.APIKey[len(cfg.APIKey)-4:]
		} else {
			apiKeyHint = "set"
		}
	}
	return map[string]any{
		"provider":        string(cfg.Provider),
		"model_name":      cfg.ModelName,
		"base_url":        cfg.BaseURL,
		"resolved_tier":   string(cfg.ResolvedTier),
		"uses_custom_key": cfg.UsesCustomKey,
		"api_key_hint":    apiKeyHint,
		"max_tokens":      cfg.MaxTokens,
		"temperature":     cfg.Temperature,
	}
}
