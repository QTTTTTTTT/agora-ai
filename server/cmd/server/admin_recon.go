// admin_recon.go — admin REST surface for reconciliation (P1-3).
//
// Endpoints
//
//   GET  /api/admin/reconciliation/runs                       list recent runs (?fund_id=..., ?limit=)
//   GET  /api/admin/reconciliation/runs/{id}                  one run + its breaks
//   GET  /api/admin/reconciliation/breaks                     list breaks (?fund_id, ?status, ?severity)
//   POST /api/admin/reconciliation/breaks/{id}/resolve        flip status (acknowledged/resolved/ignored/open)
//   POST /api/admin/reconciliation/runs                       trigger an on-demand run (mock provider)
//
// AuthZ: same admin gate the rest of /api/admin/* uses (requireAdmin).
//
// Audit
//
// Every operator-driven action — manual run trigger, break
// resolution — emits an audit.MutationEvent so the hash chain
// captures who did what when. The break.before/after snapshots
// preserve the resolution_note so a later auditor can see WHY
// the break was waived.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/recon"
)

// reconRunWire is the on-wire shape for one reconciliation run.
type reconRunWire struct {
	ID                 string         `json:"id"`
	FundID             string         `json:"fund_id"`
	StatementID        string         `json:"statement_id"`
	RunDate            string         `json:"run_date"`
	TriggeredBy        string         `json:"triggered_by,omitempty"`
	TriggerSource      string         `json:"trigger_source"`
	Status             string         `json:"status"`
	BreakCountTotal    int            `json:"break_count_total"`
	BreakCountCritical int            `json:"break_count_critical"`
	BreakCountWarning  int            `json:"break_count_warning"`
	BreakCountInfo     int            `json:"break_count_info"`
	Summary            map[string]any `json:"summary,omitempty"`
	StartedAt          string         `json:"started_at"`
	CompletedAt        string         `json:"completed_at,omitempty"`
	ErrorMessage       string         `json:"error_message,omitempty"`
}

// reconBreakWire is the on-wire shape for one break.
type reconBreakWire struct {
	ID             string         `json:"id"`
	RunID          string         `json:"run_id"`
	FundID         string         `json:"fund_id"`
	Type           string         `json:"break_type"`
	Severity       string         `json:"severity"`
	Symbol         string         `json:"symbol,omitempty"`
	Currency       string         `json:"currency,omitempty"`
	InternalValue  *float64       `json:"internal_value,omitempty"`
	BrokerValue    *float64       `json:"broker_value,omitempty"`
	DiffValue      *float64       `json:"diff_value,omitempty"`
	DiffPercent    *float64       `json:"diff_percent,omitempty"`
	Description    string         `json:"description,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	Status         string         `json:"status"`
	ResolutionNote string         `json:"resolution_note,omitempty"`
	ResolvedBy     string         `json:"resolved_by,omitempty"`
	ResolvedAt     string         `json:"resolved_at,omitempty"`
	CreatedAt      string         `json:"created_at"`
}

func projectReconRun(r recon.Run) reconRunWire {
	out := reconRunWire{
		ID:                 r.ID,
		FundID:             r.FundID,
		StatementID:        r.StatementID,
		RunDate:            r.RunDate.UTC().Format("2006-01-02"),
		TriggeredBy:        r.TriggeredBy,
		TriggerSource:      r.TriggerSource,
		Status:             string(r.Status),
		BreakCountTotal:    r.BreakCountTotal,
		BreakCountCritical: r.BreakCountCritical,
		BreakCountWarning:  r.BreakCountWarning,
		BreakCountInfo:     r.BreakCountInfo,
		Summary:            r.Summary,
		StartedAt:          r.StartedAt.UTC().Format(time.RFC3339Nano),
		ErrorMessage:       r.ErrorMessage,
	}
	if r.CompletedAt != nil {
		out.CompletedAt = r.CompletedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}

func projectReconBreak(b recon.Break) reconBreakWire {
	out := reconBreakWire{
		ID:             b.ID,
		RunID:          b.RunID,
		FundID:         b.FundID,
		Type:           string(b.Type),
		Severity:       string(b.Severity),
		Symbol:         b.Symbol,
		Currency:       b.Currency,
		InternalValue:  b.InternalValue,
		BrokerValue:    b.BrokerValue,
		DiffValue:      b.DiffValue,
		DiffPercent:    b.DiffPercent,
		Description:    b.Description,
		Metadata:       b.Metadata,
		Status:         string(b.Status),
		ResolutionNote: b.ResolutionNote,
		ResolvedBy:     b.ResolvedBy,
		CreatedAt:      b.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if b.ResolvedAt != nil {
		out.ResolvedAt = b.ResolvedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}

// registerReconAdminRoutes wires the admin reconciliation
// endpoints. Called from registerAdminRoutes so route ordering
// stays deterministic w.r.t. the rest of /api/admin/*.
func (h *adminHandler) registerReconAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil || h.db == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/reconciliation/runs", h.handleListReconRuns)
	mux.HandleFunc("POST /api/admin/reconciliation/runs", h.handleTriggerReconRun)
	mux.HandleFunc("GET /api/admin/reconciliation/runs/{id}", h.handleGetReconRun)
	mux.HandleFunc("GET /api/admin/reconciliation/breaks", h.handleListReconBreaks)
	mux.HandleFunc("POST /api/admin/reconciliation/breaks/{id}/resolve", h.handleResolveReconBreak)
}

// ----- list runs -----

func (h *adminHandler) handleListReconRuns(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(strings.TrimSpace(q.Get("limit")))
	offset, _ := strconv.Atoi(strings.TrimSpace(q.Get("offset")))

	repo := recon.NewRepo(h.db)
	runs, err := repo.ListRuns(r.Context(), recon.ListRunsParams{
		FundID: strings.TrimSpace(q.Get("fund_id")),
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]reconRunWire, 0, len(runs))
	for _, run := range runs {
		out = append(out, projectReconRun(run))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"runs": out})
}

// ----- get one run + its breaks -----

func (h *adminHandler) handleGetReconRun(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "id required"))
		return
	}
	repo := recon.NewRepo(h.db)
	run, breaks, err := repo.GetRun(r.Context(), id)
	if err != nil {
		if errors.Is(err, recon.ErrRunNotFound) {
			writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "run not found"))
			return
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	bs := make([]reconBreakWire, 0, len(breaks))
	for _, b := range breaks {
		bs = append(bs, projectReconBreak(b))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"run":    projectReconRun(*run),
		"breaks": bs,
	})
}

// ----- list breaks -----

func (h *adminHandler) handleListReconBreaks(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(strings.TrimSpace(q.Get("limit")))
	offset, _ := strconv.Atoi(strings.TrimSpace(q.Get("offset")))

	repo := recon.NewRepo(h.db)
	breaks, err := repo.ListBreaks(r.Context(), recon.ListBreaksParams{
		RunID:    strings.TrimSpace(q.Get("run_id")),
		FundID:   strings.TrimSpace(q.Get("fund_id")),
		Status:   strings.TrimSpace(q.Get("status")),
		Severity: strings.TrimSpace(q.Get("severity")),
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]reconBreakWire, 0, len(breaks))
	for _, b := range breaks {
		out = append(out, projectReconBreak(b))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"breaks": out})
}

// ----- resolve break -----

type resolveBreakRequest struct {
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

func (h *adminHandler) handleResolveReconBreak(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "id required"))
		return
	}
	var req resolveBreakRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	status := strings.TrimSpace(strings.ToLower(req.Status))
	switch status {
	case string(recon.BreakOpen), string(recon.BreakAcknowledged), string(recon.BreakResolved), string(recon.BreakIgnored):
	default:
		writeOrderActionJSON(w, http.StatusBadRequest,
			errorPayload("invalid_status", "status must be open|acknowledged|resolved|ignored"))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	repo := recon.NewRepo(h.db)
	updated, err := repo.ResolveBreak(r.Context(), recon.ResolveBreakParams{
		ID:         id,
		NewStatus:  status,
		Note:       req.Note,
		ResolvedBy: userID,
	})
	if err != nil {
		if errors.Is(err, recon.ErrBreakNotFound) {
			writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "break not found"))
			return
		}
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("resolve_failed", err.Error()))
		return
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "recon_break.resolve",
			TargetType:  "reconciliation_break",
			TargetID:    id,
			After: map[string]any{
				"status":    status,
				"note":      req.Note,
				"break_id":  id,
				"fund_id":   updated.FundID,
				"break_type": string(updated.Type),
			},
		})
	}
	if h.metrics != nil {
		h.metrics.RecordReconEvent("resolve_" + status)
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"break": projectReconBreak(*updated)})
}

// ----- trigger run -----

type triggerReconRunRequest struct {
	FundID string `json:"fund_id"`
	// AsOfDate is optional; defaults to today UTC.
	AsOfDate string `json:"as_of_date,omitempty"`
	// UseMockProvider, when true, fabricates a perfect-mirror
	// statement from the internal snapshot. This is the ONLY
	// supported path until a real broker statement loader lands;
	// we still gate it explicitly so the flag is captured in audit.
	UseMockProvider bool `json:"use_mock_provider"`
	// MockDriftQty / MockDriftCash / MockDriftPrice perturb the
	// synthetic statement so the operator can see what a real
	// break flow looks like end-to-end. 0 = no perturbation.
	MockDriftQty   float64 `json:"mock_drift_qty,omitempty"`
	MockDriftCash  float64 `json:"mock_drift_cash,omitempty"`
	MockDriftPrice float64 `json:"mock_drift_price,omitempty"`
}

func (h *adminHandler) handleTriggerReconRun(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	var req triggerReconRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	if strings.TrimSpace(req.FundID) == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("fund_id_required", "fund_id required"))
		return
	}
	if !req.UseMockProvider {
		// Real-broker statements arrive via a CSV upload (future
		// work). Until then, the only sane way to trigger a run
		// is mock; reject the request loudly so a misclick doesn't
		// silently no-op.
		writeOrderActionJSON(w, http.StatusBadRequest,
			errorPayload("provider_required", "use_mock_provider must be true (real-broker ingest is not yet wired)"))
		return
	}
	asOf := time.Now().UTC()
	if s := strings.TrimSpace(req.AsOfDate); s != "" {
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			writeOrderActionJSON(w, http.StatusBadRequest,
				errorPayload("invalid_as_of", "as_of_date must be YYYY-MM-DD"))
			return
		}
		asOf = t.UTC()
	}

	// Build snapshot.
	builder := newReconSnapshotBuilder(h.db)
	if builder == nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", "snapshot builder unavailable"))
		return
	}
	snap, err := builder.Build(r.Context(), req.FundID, asOf)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("snapshot_failed", err.Error()))
		return
	}

	// Build mock statement (with optional drift) + ingest it.
	provider := recon.NewMockProvider(recon.MockProviderOptions{
		IncludeDrift:  req.MockDriftQty != 0 || req.MockDriftCash != 0 || req.MockDriftPrice != 0,
		DriftQuantity: req.MockDriftQty,
		DriftCash:     req.MockDriftCash,
		DriftPrice:    req.MockDriftPrice,
		Source:        recon.SourceMock,
		Seed:          asOf.Unix(),
	})
	stmt := provider.Build(snap)
	repo := recon.NewRepo(h.db)
	persisted, err := repo.IngestStatement(r.Context(), recon.IngestParamsFromBuild(stmt, userID))
	if err != nil && !errors.Is(err, recon.ErrAlreadyIngested) {
		if h.metrics != nil {
			h.metrics.RecordReconEvent("ingest_error")
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("ingest_failed", err.Error()))
		return
	}
	if errors.Is(err, recon.ErrAlreadyIngested) {
		if h.metrics != nil {
			h.metrics.RecordReconEvent("ingest_duplicate")
		}
	} else if h.metrics != nil {
		h.metrics.RecordReconEvent("ingest_ok")
	}

	// Diff + persist run.
	engine := recon.NewEngine(recon.DefaultTolerances)
	diff := engine.Diff(stmt, snap)
	run, err := repo.CreateRun(r.Context(), recon.CreateRunParams{
		FundID:        req.FundID,
		StatementID:   persisted.ID,
		RunDate:       asOf,
		TriggeredBy:   userID,
		TriggerSource: "manual",
		Status:        recon.RunCompleted,
		Result:        diff,
		Summary: map[string]any{
			"provider":         string(stmt.Source),
			"as_of":            asOf.Format("2006-01-02"),
			"mock_drift_qty":   req.MockDriftQty,
			"mock_drift_cash":  req.MockDriftCash,
			"mock_drift_price": req.MockDriftPrice,
		},
	})
	if err != nil {
		if h.metrics != nil {
			h.metrics.RecordReconEvent("run_failed")
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("run_failed", err.Error()))
		return
	}
	if h.metrics != nil {
		h.metrics.RecordReconEvent("run_ok")
		for _, b := range diff.Breaks {
			h.metrics.RecordReconEvent(fmt.Sprintf("break_%s", b.Type))
		}
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "recon_run.trigger",
			TargetType:  "reconciliation_run",
			TargetID:    run.ID,
			After: map[string]any{
				"fund_id":          req.FundID,
				"as_of":            asOf.Format("2006-01-02"),
				"provider":         string(stmt.Source),
				"break_count":      run.BreakCountTotal,
				"break_count_crit": run.BreakCountCritical,
			},
		})
	}

	bs := make([]reconBreakWire, 0, len(diff.Breaks))
	for _, b := range diff.Breaks {
		// `diff.Breaks` aren't persisted with IDs at this point;
		// we surface the in-memory shape so the UI can render the
		// preview without a second round trip. The admin can still
		// fetch the persisted versions via /runs/{id}.
		bs = append(bs, projectReconBreak(b))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"run":    projectReconRun(*run),
		"breaks": bs,
	})
}
