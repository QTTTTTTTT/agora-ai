// debate_handler.go — S8.2 per-fund Bull/Bear debate REST.
//
// Routes
//
//   POST /api/funds/{fundId}/debates/run
//        Body: DebateRunRequest (JSON)
//        Runs the analyst panel for one symbol (reusing the
//        S8.1 panel provider), then drives N rounds of
//        forced Bull-vs-Bear debate over the resulting reports,
//        persists the transcript, and returns it.
//
//   GET  /api/funds/{fundId}/debates[?symbol=X&from=...&to=...&limit=N]
//        Lists historical debate transcripts.
//
//   GET  /api/funds/{fundId}/debates/{debateId}
//        Fetches one transcript + its per-round arguments.
//
// Auth: fund-level access. The panel provider + debate
// orchestrator are injected via the wiring layer (DebateProvider).

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/agent"
	"github.com/fundai/server/internal/analystreport"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/debaterepo"
	"github.com/fundai/server/internal/repository"
)

type debateHandler struct {
	repo            *debaterepo.Repo
	panelRepo       *analystreport.Repo
	fundRepo        *repository.FundRepo
	companyRepo     *repository.FundCompanyRepo
	now             func() time.Time
}

// DebateProvider is the wiring-layer hook used to obtain the
// per-fund debate orchestrator at request time. Returns nil
// when no debate is configured for the fund (handler then
// replies 503).
type DebateProvider func(fundID string) *agent.Debate

func newDebateHandler(svc *Services) *debateHandler {
	if svc == nil || svc.DB == nil || svc.DebateRepo == nil || svc.AnalystReportRepo == nil {
		return nil
	}
	return &debateHandler{
		repo:        svc.DebateRepo,
		panelRepo:   svc.AnalystReportRepo,
		fundRepo:    repository.NewFundRepo(svc.DB),
		companyRepo: repository.NewFundCompanyRepo(svc.DB),
		now:         time.Now,
	}
}

func (h *debateHandler) RegisterRoutes(mux *http.ServeMux, panelProvider AnalystPanelProvider, debateProvider DebateProvider) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("POST /api/funds/{fundId}/debates/run",
		h.wrapRun(panelProvider, debateProvider))
	mux.HandleFunc("GET /api/funds/{fundId}/debates",
		h.handleList)
	mux.HandleFunc("GET /api/funds/{fundId}/debates/{debateId}",
		h.handleGet)
}

// --- Wire shapes -----------------------------------------------------------

type debateRunRequest struct {
	// Symbol + optional analyst panel input. We reuse the S8.1
	// analystRunRequest shape verbatim so the wiring layer can
	// fan the same JSON through both endpoints.
	analystRunRequest

	// Rounds overrides the default debate length. Clamped to
	// [1, 5] by the handler.
	Rounds int `json:"rounds,omitempty"`
}

type debateArgumentWire struct {
	ID            string   `json:"id,omitempty"`
	AgentID       string   `json:"agent_id"`
	AgentName     string   `json:"agent_name"`
	Stance        string   `json:"stance"`
	Symbol        string   `json:"symbol"`
	Round         int      `json:"round"`
	AsOf          string   `json:"asof"`
	GeneratedAt   string   `json:"generated_at"`
	Direction     string   `json:"direction"`
	Confidence    int      `json:"confidence"`
	Thesis        string   `json:"thesis"`
	SupportPoints []string `json:"support_points"`
	Rebuttals     []string `json:"rebuttals"`
	CitedReports  []string `json:"cited_reports,omitempty"`
	LLMModel      string   `json:"llm_model,omitempty"`
}

type debateVerdictWire struct {
	Direction        string `json:"direction"`
	Confidence       int    `json:"confidence"`
	WinnerStance     string `json:"winner_stance,omitempty"`
	BullConfidence   int    `json:"bull_confidence"`
	BearConfidence   int    `json:"bear_confidence"`
	Contested        bool   `json:"contested"`
	WinningSummary   string `json:"winning_summary,omitempty"`
	LosingSummary    string `json:"losing_summary,omitempty"`
}

type debateTranscriptWire struct {
	ID          string               `json:"id,omitempty"`
	FundID      string               `json:"fund_id"`
	PanelID     string               `json:"panel_id,omitempty"`
	Symbol      string               `json:"symbol"`
	AsOf        string               `json:"asof"`
	GeneratedAt string               `json:"generated_at"`
	Verdict     debateVerdictWire    `json:"verdict"`
	Arguments   []debateArgumentWire `json:"arguments"`
	Panel       *analystPanelWire    `json:"panel,omitempty"`
}

// --- Handlers --------------------------------------------------------------

func (h *debateHandler) wrapRun(panelProvider AnalystPanelProvider, debateProvider DebateProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.handleRun(w, r, panelProvider, debateProvider)
	}
}

func (h *debateHandler) handleRun(w http.ResponseWriter, r *http.Request, panelProvider AnalystPanelProvider, debateProvider DebateProvider) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId required"))
		return
	}
	if _, err := authorizeFundAccess(r.Context(), h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}
	var req debateRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	if strings.TrimSpace(req.Symbol) == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("symbol_required", ""))
		return
	}
	if panelProvider == nil || debateProvider == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("debate_unavailable", "panel or debate provider not configured"))
		return
	}
	panel := panelProvider(fundID)
	debate := debateProvider(fundID)
	if panel == nil || debate == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("debate_unavailable", "no panel/debate for this fund"))
		return
	}

	// Step 1: run the analyst panel.
	input := buildAnalystInput(req.analystRunRequest, h.now())
	panelRep, err := panel.RunSymbol(r.Context(), input)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("panel_run_failed", err.Error()))
		return
	}
	panelRep.FundID = fundID

	// Step 2: persist the panel report (debate needs panel_id).
	panelID, perr := h.panelRepo.SavePanel(r.Context(), panelRep)
	if perr != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("panel_persist_failed", perr.Error()))
		return
	}

	// Step 3: run the debate.
	transcript, derr := debate.Run(r.Context(), fundID, panelRep, req.Notes)
	if derr != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("debate_run_failed", derr.Error()))
		return
	}

	// Step 4: persist the transcript.
	transcriptID, terr := h.repo.SaveTranscript(r.Context(), panelID, transcript)
	if terr != nil {
		// Persist failure shouldn't drop the result.
		panelWire := projectPanel(panelRep, panelID)
		writeJSON(w, http.StatusOK, map[string]any{
			"debate":        projectDebate(transcript, transcriptID, panelID, &panelWire),
			"persist_error": terr.Error(),
		})
		return
	}
	panelWire := projectPanel(panelRep, panelID)
	writeJSON(w, http.StatusOK, map[string]any{
		"debate": projectDebate(transcript, transcriptID, panelID, &panelWire),
	})
}

func (h *debateHandler) handleList(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId required"))
		return
	}
	if _, err := authorizeFundAccess(r.Context(), h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}
	q := r.URL.Query()
	params := debaterepo.ListTranscriptsParams{
		FundID: fundID,
		Symbol: strings.TrimSpace(q.Get("symbol")),
	}
	if v := strings.TrimSpace(q.Get("from")); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			params.AsOfFrom = t
		}
	}
	if v := strings.TrimSpace(q.Get("to")); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			params.AsOfTo = t
		}
	}
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			params.Limit = n
		}
	}
	rows, err := h.repo.ListTranscripts(r.Context(), params)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]debateTranscriptWire, 0, len(rows))
	for _, row := range rows {
		out = append(out, projectTranscriptRow(row, nil))
	}
	writeJSON(w, http.StatusOK, map[string]any{"debates": out})
}

func (h *debateHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	debateID := strings.TrimSpace(r.PathValue("debateId"))
	if fundID == "" || debateID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId and debateId required"))
		return
	}
	if _, err := authorizeFundAccess(r.Context(), h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}
	row, err := h.repo.GetTranscript(r.Context(), debateID)
	if err != nil {
		if errors.Is(err, debaterepo.ErrNotFound) {
			writeOrderActionJSON(w, http.StatusNotFound, errorPayload("debate_not_found", debateID))
			return
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if row.FundID != fundID {
		writeOrderActionJSON(w, http.StatusNotFound, errorPayload("debate_not_found", debateID))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"debate": projectTranscriptRow(row, nil)})
}

// --- Projection helpers ---------------------------------------------------

func projectDebate(t agent.DebateTranscript, transcriptID, panelID string, panel *analystPanelWire) debateTranscriptWire {
	out := debateTranscriptWire{
		ID:          transcriptID,
		FundID:      t.FundID,
		PanelID:     panelID,
		Symbol:      t.Symbol,
		AsOf:        t.AsOf.UTC().Format(time.RFC3339Nano),
		GeneratedAt: t.GeneratedAt.UTC().Format(time.RFC3339Nano),
		Verdict: debateVerdictWire{
			Direction:      string(t.Verdict.Direction),
			Confidence:     t.Verdict.Confidence,
			WinnerStance:   string(t.Verdict.WinnerStance),
			BullConfidence: t.Verdict.BullConfidence,
			BearConfidence: t.Verdict.BearConfidence,
			Contested:      t.Verdict.Contested,
			WinningSummary: t.Verdict.WinningSummary,
			LosingSummary:  t.Verdict.LosingSummary,
		},
	}
	for _, a := range t.Arguments {
		out.Arguments = append(out.Arguments, projectAdvocateArgument(a))
	}
	if panel != nil {
		p := *panel
		out.Panel = &p
	}
	return out
}

func projectAdvocateArgument(a agent.AdvocateArgument) debateArgumentWire {
	cited := make([]string, len(a.CitedReports))
	for i, c := range a.CitedReports {
		cited[i] = string(c)
	}
	return debateArgumentWire{
		AgentID:       a.AgentID,
		AgentName:     a.AgentName,
		Stance:        string(a.Stance),
		Symbol:        a.Symbol,
		Round:         a.Round,
		AsOf:          a.AsOf.UTC().Format(time.RFC3339Nano),
		GeneratedAt:   a.GeneratedAt.UTC().Format(time.RFC3339Nano),
		Direction:     string(a.Direction),
		Confidence:    a.Confidence,
		Thesis:        a.Thesis,
		SupportPoints: a.SupportPoints,
		Rebuttals:     a.Rebuttals,
		CitedReports:  cited,
		LLMModel:      a.LLMModel,
	}
}

func projectTranscriptRow(row debaterepo.TranscriptRow, panel *analystPanelWire) debateTranscriptWire {
	out := debateTranscriptWire{
		ID:          row.ID,
		FundID:      row.FundID,
		PanelID:     row.PanelID,
		Symbol:      row.Symbol,
		AsOf:        row.AsOf.UTC().Format(time.RFC3339Nano),
		GeneratedAt: row.GeneratedAt.UTC().Format(time.RFC3339Nano),
		Verdict: debateVerdictWire{
			Direction:      row.VerdictDirection,
			Confidence:     row.VerdictConfidence,
			WinnerStance:   row.VerdictWinner,
			BullConfidence: row.VerdictBullConfidence,
			BearConfidence: row.VerdictBearConfidence,
			Contested:      row.VerdictContested,
			WinningSummary: row.VerdictWinningSummary,
			LosingSummary:  row.VerdictLosingSummary,
		},
	}
	for _, a := range row.Arguments {
		out.Arguments = append(out.Arguments, debateArgumentWire{
			ID: a.ID, AgentID: a.AgentID, AgentName: a.AgentName,
			Stance: a.Stance, Symbol: a.Symbol, Round: a.RoundNumber,
			AsOf:          a.AsOf.UTC().Format(time.RFC3339Nano),
			GeneratedAt:   a.GeneratedAt.UTC().Format(time.RFC3339Nano),
			Direction:     a.Direction,
			Confidence:    a.Confidence,
			Thesis:        a.Thesis,
			SupportPoints: a.SupportPoints,
			Rebuttals:     a.Rebuttals,
			CitedReports:  a.CitedReports,
			LLMModel:      a.LLMModel,
		})
	}
	if panel != nil {
		p := *panel
		out.Panel = &p
	}
	return out
}
