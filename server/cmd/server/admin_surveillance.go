// admin_surveillance.go — admin REST surface for trade surveillance
// (P1-7).
//
// Endpoints
//
//   GET  /api/admin/surveillance/events                       list events (?fund_id, ?status, ?severity, ?rule_code, ?limit, ?offset)
//   GET  /api/admin/surveillance/events/{id}                  one event with full metadata
//   POST /api/admin/surveillance/events/{id}/review           flip status (open / reviewing / cleared / escalated) + note
//   GET  /api/admin/surveillance/runs                         list scan runs (?fund_id, ?limit)
//   POST /api/admin/surveillance/scan                         on-demand scan for a fund + window
//
// AuthZ: requireAdmin gate; review/scan actions are emitted to the
// audit hash chain via audit.MutationEvent.

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
	"github.com/fundai/server/internal/surveillance"
)

// surveillanceEventWire is the on-wire shape for a surveillance
// event. Mirrors the DB row but with timestamps stringified and
// the metadata map left untyped because the schema is rule-specific.
type surveillanceEventWire struct {
	ID              string         `json:"id"`
	FundID          string         `json:"fund_id"`
	RuleCode        string         `json:"rule_code"`
	Severity        string         `json:"severity"`
	Symbol          string         `json:"symbol,omitempty"`
	InstrumentKey   string         `json:"instrument_key,omitempty"`
	WindowStart     string         `json:"window_start"`
	WindowEnd       string         `json:"window_end"`
	TradeIDs        []string       `json:"trade_ids"`
	Summary         string         `json:"summary"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	Status          string         `json:"status"`
	ReviewNote      string         `json:"review_note,omitempty"`
	ReviewedBy      string         `json:"reviewed_by,omitempty"`
	ReviewedAt      string         `json:"reviewed_at,omitempty"`
	DetectedAt      string         `json:"detected_at"`
	DetectorVersion string         `json:"detector_version,omitempty"`
	Fingerprint     string         `json:"fingerprint"`
}

// surveillanceRunWire is the on-wire shape for a scan run row.
type surveillanceRunWire struct {
	ID                 string         `json:"id"`
	FundID             string         `json:"fund_id,omitempty"`
	TriggeredBy        string         `json:"triggered_by,omitempty"`
	TriggerSource      string         `json:"trigger_source"`
	WindowStart        string         `json:"window_start"`
	WindowEnd          string         `json:"window_end"`
	TradeCount         int            `json:"trade_count"`
	EventCountTotal    int            `json:"event_count_total"`
	EventCountCritical int            `json:"event_count_critical"`
	EventCountWarning  int            `json:"event_count_warning"`
	EventCountInfo     int            `json:"event_count_info"`
	DurationMS         int            `json:"duration_ms"`
	Status             string         `json:"status"`
	ErrorMessage       string         `json:"error_message,omitempty"`
	Summary            map[string]any `json:"summary,omitempty"`
	StartedAt          string         `json:"started_at"`
	CompletedAt        string         `json:"completed_at,omitempty"`
}

func projectSurveillanceEvent(ev surveillance.Event) surveillanceEventWire {
	out := surveillanceEventWire{
		ID:              ev.ID,
		FundID:          ev.FundID,
		RuleCode:        string(ev.RuleCode),
		Severity:        string(ev.Severity),
		Symbol:          ev.Symbol,
		InstrumentKey:   ev.InstrumentKey,
		WindowStart:     ev.WindowStart.UTC().Format(time.RFC3339Nano),
		WindowEnd:       ev.WindowEnd.UTC().Format(time.RFC3339Nano),
		TradeIDs:        append([]string(nil), ev.TradeIDs...),
		Summary:         ev.Summary,
		Metadata:        ev.Metadata,
		Status:          string(ev.Status),
		DetectedAt:      ev.DetectedAt.UTC().Format(time.RFC3339Nano),
		DetectorVersion: ev.DetectorVersion,
		Fingerprint:     ev.Fingerprint,
	}
	return out
}

func projectSurveillanceEventDetail(d surveillance.EventDetail) surveillanceEventWire {
	out := projectSurveillanceEvent(d.Event)
	out.ReviewNote = d.ReviewNote
	out.ReviewedBy = d.ReviewedBy
	if d.ReviewedAt != nil {
		out.ReviewedAt = d.ReviewedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}

func projectSurveillanceRun(r surveillance.Run) surveillanceRunWire {
	out := surveillanceRunWire{
		ID:                 r.ID,
		FundID:             r.FundID,
		TriggeredBy:        r.TriggeredBy,
		TriggerSource:      r.TriggerSource,
		WindowStart:        r.WindowStart.UTC().Format(time.RFC3339Nano),
		WindowEnd:          r.WindowEnd.UTC().Format(time.RFC3339Nano),
		TradeCount:         r.TradeCount,
		EventCountTotal:    r.EventCountTotal,
		EventCountCritical: r.EventCountCritical,
		EventCountWarning:  r.EventCountWarning,
		EventCountInfo:     r.EventCountInfo,
		DurationMS:         r.DurationMS,
		Status:             r.Status,
		ErrorMessage:       r.ErrorMessage,
		Summary:            r.Summary,
		StartedAt:          r.StartedAt.UTC().Format(time.RFC3339Nano),
	}
	if r.CompletedAt != nil {
		out.CompletedAt = r.CompletedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}

// registerSurveillanceAdminRoutes wires the admin endpoints.
func (h *adminHandler) registerSurveillanceAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil || h.db == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/surveillance/events", h.handleListSurveillanceEvents)
	mux.HandleFunc("GET /api/admin/surveillance/events/{id}", h.handleGetSurveillanceEvent)
	mux.HandleFunc("POST /api/admin/surveillance/events/{id}/review", h.handleReviewSurveillanceEvent)
	mux.HandleFunc("GET /api/admin/surveillance/runs", h.handleListSurveillanceRuns)
	mux.HandleFunc("POST /api/admin/surveillance/scan", h.handleTriggerSurveillanceScan)
}

// ----- list events -----

func (h *adminHandler) handleListSurveillanceEvents(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(strings.TrimSpace(q.Get("limit")))
	offset, _ := strconv.Atoi(strings.TrimSpace(q.Get("offset")))
	repo := surveillance.NewRepo(h.db)
	events, err := repo.ListEvents(r.Context(), surveillance.ListEventsParams{
		FundID:   strings.TrimSpace(q.Get("fund_id")),
		RuleCode: surveillance.RuleCode(strings.TrimSpace(q.Get("rule_code"))),
		Status:   surveillance.EventStatus(strings.TrimSpace(q.Get("status"))),
		Severity: surveillance.Severity(strings.TrimSpace(q.Get("severity"))),
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]surveillanceEventWire, 0, len(events))
	for _, ev := range events {
		out = append(out, projectSurveillanceEvent(ev))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"events": out})
}

// ----- get event -----

func (h *adminHandler) handleGetSurveillanceEvent(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "id required"))
		return
	}
	repo := surveillance.NewRepo(h.db)
	d, err := repo.GetEvent(r.Context(), id)
	if err != nil {
		if errors.Is(err, surveillance.ErrEventNotFound) {
			writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "event not found"))
			return
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"event": projectSurveillanceEventDetail(*d)})
}

// ----- review event -----

type reviewSurveillanceEventRequest struct {
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

func (h *adminHandler) handleReviewSurveillanceEvent(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "id required"))
		return
	}
	var req reviewSurveillanceEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	status := strings.TrimSpace(strings.ToLower(req.Status))
	switch surveillance.EventStatus(status) {
	case surveillance.StatusOpen, surveillance.StatusReviewing, surveillance.StatusCleared, surveillance.StatusEscalated:
	default:
		writeOrderActionJSON(w, http.StatusBadRequest,
			errorPayload("invalid_status", "status must be open|reviewing|cleared|escalated"))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	repo := surveillance.NewRepo(h.db)
	if err := repo.UpdateStatus(r.Context(), surveillance.UpdateStatusParams{
		ID:         id,
		NewStatus:  surveillance.EventStatus(status),
		Note:       req.Note,
		ReviewedBy: userID,
	}); err != nil {
		if errors.Is(err, surveillance.ErrEventNotFound) {
			writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "event not found"))
			return
		}
		if errors.Is(err, surveillance.ErrInvalidStatus) {
			writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_status", err.Error()))
			return
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	// Re-read so the response reflects the fresh row including
	// the resolution timestamps the DB just stamped.
	updated, err := repo.GetEvent(r.Context(), id)
	if err != nil {
		// The update succeeded; we just can't render the detail.
		writeOrderActionJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "surveillance_event.review",
			TargetType:  "surveillance_event",
			TargetID:    id,
			After: map[string]any{
				"status":    status,
				"note":      req.Note,
				"event_id":  id,
				"fund_id":   updated.FundID,
				"rule_code": string(updated.RuleCode),
			},
		})
	}
	if h.metrics != nil {
		h.metrics.RecordSurveillanceEvent("review_" + status)
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"event": projectSurveillanceEventDetail(*updated)})
}

// ----- list runs -----

func (h *adminHandler) handleListSurveillanceRuns(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(strings.TrimSpace(q.Get("limit")))
	offset, _ := strconv.Atoi(strings.TrimSpace(q.Get("offset")))
	repo := surveillance.NewRepo(h.db)
	runs, err := repo.ListRuns(r.Context(), surveillance.ListRunsParams{
		FundID: strings.TrimSpace(q.Get("fund_id")),
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]surveillanceRunWire, 0, len(runs))
	for _, run := range runs {
		out = append(out, projectSurveillanceRun(run))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"runs": out})
}

// ----- trigger scan -----

type triggerSurveillanceScanRequest struct {
	FundID string `json:"fund_id"`
	// AsOfDate is the trading day to scan; defaults to today UTC.
	AsOfDate string `json:"as_of_date,omitempty"`
	// SessionCloseUTC, when set, lets the operator override the
	// session close used for marking-close detection. Empty falls
	// back to "20:00 UTC" — a passable proxy for US market 4PM ET.
	SessionCloseUTC string `json:"session_close_utc,omitempty"`
}

func (h *adminHandler) handleTriggerSurveillanceScan(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	var req triggerSurveillanceScanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	if strings.TrimSpace(req.FundID) == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("fund_id_required", "fund_id required"))
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
	winStart, winEnd := startOfDay(asOf), endOfDay(asOf)

	close := time.Date(asOf.Year(), asOf.Month(), asOf.Day(), 20, 0, 0, 0, time.UTC)
	if s := strings.TrimSpace(req.SessionCloseUTC); s != "" {
		t, err := time.Parse("15:04", s)
		if err != nil {
			writeOrderActionJSON(w, http.StatusBadRequest,
				errorPayload("invalid_session_close", "session_close_utc must be HH:MM"))
			return
		}
		close = time.Date(asOf.Year(), asOf.Month(), asOf.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
	}

	builder := newSurveillanceSnapshotBuilder(h.db)
	if builder == nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", "snapshot builder unavailable"))
		return
	}
	snap, err := builder.Load(r.Context(), surveillanceLoadParams{
		FundID:      req.FundID,
		WindowStart: winStart,
		WindowEnd:   winEnd,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("snapshot_failed", err.Error()))
		return
	}

	started := time.Now()
	engine := surveillance.NewEngine(surveillance.DefaultRules()...)
	res := engine.Run(snap, defaultMarketContext(close))

	repo := surveillance.NewRepo(h.db)
	persisted := 0
	deduped := 0
	for _, ev := range res.Events {
		ir, ierr := repo.InsertEvent(r.Context(), ev)
		if ierr != nil {
			if h.metrics != nil {
				h.metrics.RecordSurveillanceEvent("insert_error")
			}
			continue
		}
		if ir.Inserted {
			persisted++
			if h.metrics != nil {
				h.metrics.RecordSurveillanceEvent(fmt.Sprintf("event_%s", ev.RuleCode))
				h.metrics.RecordSurveillanceEvent(fmt.Sprintf("severity_%s", ev.Severity))
			}
		} else {
			deduped++
		}
	}
	durationMS := int(time.Since(started) / time.Millisecond)

	run, err := repo.CreateRun(r.Context(), surveillance.CreateRunParams{
		FundID:        req.FundID,
		TriggeredBy:   userID,
		TriggerSource: "manual",
		WindowStart:   winStart,
		WindowEnd:     winEnd,
		TradeCount:    len(snap),
		Result:        res,
		DurationMS:    durationMS,
		Status:        "completed",
		Summary: map[string]any{
			"as_of":     asOf.Format("2006-01-02"),
			"persisted": persisted,
			"deduped":   deduped,
		},
	})
	if err != nil {
		if h.metrics != nil {
			h.metrics.RecordSurveillanceEvent("run_failed")
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("run_failed", err.Error()))
		return
	}
	if h.metrics != nil {
		h.metrics.RecordSurveillanceEvent("run_ok")
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "surveillance_scan.trigger",
			TargetType:  "surveillance_run",
			TargetID:    run.ID,
			After: map[string]any{
				"fund_id":     req.FundID,
				"as_of":       asOf.Format("2006-01-02"),
				"trade_count": len(snap),
				"events":      run.EventCountTotal,
				"crit_events": run.EventCountCritical,
			},
		})
	}

	// Surface the events (with persisted IDs) for the UI to render
	// the post-scan dialog without a second round trip.
	out := make([]surveillanceEventWire, 0, len(res.Events))
	for _, ev := range res.Events {
		out = append(out, projectSurveillanceEvent(ev))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"run":    projectSurveillanceRun(*run),
		"events": out,
	})
}
