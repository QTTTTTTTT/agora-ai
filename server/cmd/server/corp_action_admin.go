package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/corpaction"
	"github.com/fundai/server/internal/repository"
)

// corpActionApplyRequest is the JSON body of POST /api/admin/corp-actions.
//
// All fields are required EXCEPT `fund_ids`. When `fund_ids` is empty
// the handler fans out the application to every fund that currently
// holds the instrument — the common admin path after Yahoo's daily
// chart sweep produces a bulk batch.
type corpActionApplyRequest struct {
	InstrumentKey string   `json:"instrument_key"`
	ExDate        string   `json:"ex_date"`     // ISO date YYYY-MM-DD
	ActionType    string   `json:"action_type"` // split | cash_dividend | stock_dividend | combined
	SplitRatio    float64  `json:"split_ratio"`
	CashDividend  float64  `json:"cash_dividend"`
	Source        string   `json:"source"` // manual | yahoo | tushare | sina | tencent
	Notes         string   `json:"notes,omitempty"`
	AnnouncedAt   string   `json:"announced_at,omitempty"`
	FundIDs       []string `json:"fund_ids,omitempty"` // empty = fan out
}

type corpActionApplyResponse struct {
	EventID      string                       `json:"event_id"`
	FanOut       int                          `json:"fan_out_funds"`
	Applications []corpActionApplicationEntry `json:"applications"`
	Skipped      []corpActionSkipEntry        `json:"skipped,omitempty"`
}

type corpActionApplicationEntry struct {
	FundID         string  `json:"fund_id"`
	PreQuantity    float64 `json:"pre_quantity"`
	PostQuantity   float64 `json:"post_quantity"`
	PreCostPrice   float64 `json:"pre_cost_price"`
	PostCostPrice  float64 `json:"post_cost_price"`
	PreUnrealized  float64 `json:"pre_unrealized_pnl"`
	PostUnrealized float64 `json:"post_unrealized_pnl"`
	CashCredit     float64 `json:"cash_credit"`
	AlreadyApplied bool    `json:"already_applied"`
}

type corpActionSkipEntry struct {
	FundID string `json:"fund_id"`
	Reason string `json:"reason"`
}

// handleApplyCorpAction implements POST /api/admin/corp-actions.
//
// Flow:
//   1. validate the request payload (cheap fail-fast).
//   2. upsert the corporate_actions row (dedup on natural key).
//   3. resolve the fund list — explicit `fund_ids` or fan-out via
//      holding_positions.
//   4. invoke applier.ApplyEvent per fund inside its own tx.
//      ErrPositionMissing is folded into a "skipped" entry rather
//      than failing the whole batch.
//
// The handler is super-admin only because the financial mutation
// is irreversible and audit-bearing.
func (h *adminHandler) handleApplyCorpAction(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.corpActionRepo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "corp action service unavailable"})
		return
	}

	var req corpActionApplyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_json", "detail": err.Error()})
		return
	}

	row, err := validateAndBuildCorpActionRow(req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_payload", "detail": err.Error()})
		return
	}

	ctx := r.Context()
	eventID, err := h.corpActionRepo.Upsert(ctx, row)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "upsert_failed", "detail": err.Error()})
		return
	}
	row.ID = eventID

	fundIDs := strings.TrimSpace(strings.Join(req.FundIDs, ""))
	var resolvedFunds []string
	if fundIDs == "" {
		resolvedFunds, err = h.fundsHoldingInstrument(ctx, row.InstrumentKey)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "fanout_lookup_failed", "detail": err.Error()})
			return
		}
	} else {
		resolvedFunds = req.FundIDs
	}

	resp := corpActionApplyResponse{EventID: eventID, FanOut: len(resolvedFunds)}
	evt := row.ToEvent()
	for _, fundID := range resolvedFunds {
		fundID = strings.TrimSpace(fundID)
		if fundID == "" {
			continue
		}
		res, applyErr := corpaction.ApplyEvent(ctx, h.db, evt, fundID)
		switch {
		case applyErr == nil:
			resp.Applications = append(resp.Applications, corpActionApplicationEntry{
				FundID:         res.FundID,
				PreQuantity:    res.PreQuantity,
				PostQuantity:   res.PostQuantity,
				PreCostPrice:   res.PreCostPrice,
				PostCostPrice:  res.PostCostPrice,
				PreUnrealized:  res.PreUnrealized,
				PostUnrealized: res.PostUnrealized,
				CashCredit:     res.CashCredit,
				AlreadyApplied: res.AlreadyApplied,
			})
		case errors.Is(applyErr, corpaction.ErrPositionMissing):
			resp.Skipped = append(resp.Skipped, corpActionSkipEntry{
				FundID: fundID,
				Reason: "position_missing",
			})
		default:
			// Hard failure on a single fund shouldn't pollute the
			// rest of the batch. We bag it as a skip with the raw
			// error message — the caller can re-run the explicit
			// fund_id later after triage.
			resp.Skipped = append(resp.Skipped, corpActionSkipEntry{
				FundID: fundID,
				Reason: applyErr.Error(),
			})
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleListCorpActionsForFund implements
// GET /api/admin/funds/{fundId}/corp-actions.
// Returns the applied corp-action timeline for the fund — used by
// the holding detail page so the operator can answer "what
// adjusted my cost basis on this date?".
func (h *adminHandler) handleListCorpActionsForFund(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.corpActionRepo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "corp action service unavailable"})
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing_fund_id"})
		return
	}
	rows, err := h.corpActionRepo.ApplicationsForFund(r.Context(), fundID, 200)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "query_failed", "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows})
}

// validateAndBuildCorpActionRow centralises the JSON → CorpActionRow
// translation, returning a usable error per field rather than a
// generic 400. Splitting it out also lets the test suite assert on
// the exact rejection messages.
func validateAndBuildCorpActionRow(req corpActionApplyRequest) (repository.CorpActionRow, error) {
	row := repository.CorpActionRow{}
	row.InstrumentKey = strings.TrimSpace(req.InstrumentKey)
	if row.InstrumentKey == "" {
		return row, fmt.Errorf("instrument_key is required")
	}
	row.ActionType = strings.ToLower(strings.TrimSpace(req.ActionType))
	switch row.ActionType {
	case "split", "cash_dividend", "stock_dividend", "combined":
	default:
		return row, fmt.Errorf("action_type must be one of: split, cash_dividend, stock_dividend, combined")
	}
	row.Source = strings.ToLower(strings.TrimSpace(req.Source))
	switch row.Source {
	case "manual", "yahoo", "tushare", "sina", "tencent":
	default:
		return row, fmt.Errorf("source must be one of: manual, yahoo, tushare, sina, tencent")
	}
	if req.SplitRatio <= 0 {
		return row, fmt.Errorf("split_ratio must be > 0 (use 1.0 when there is no share change)")
	}
	if req.CashDividend < 0 {
		return row, fmt.Errorf("cash_dividend must be >= 0")
	}
	row.SplitRatio = req.SplitRatio
	row.CashDividend = req.CashDividend

	d, err := time.Parse("2006-01-02", strings.TrimSpace(req.ExDate))
	if err != nil {
		return row, fmt.Errorf("ex_date must be YYYY-MM-DD: %v", err)
	}
	row.ExDate = d
	if notes := strings.TrimSpace(req.Notes); notes != "" {
		row.Notes.Valid = true
		row.Notes.String = notes
	}
	if announced := strings.TrimSpace(req.AnnouncedAt); announced != "" {
		t, err := time.Parse(time.RFC3339, announced)
		if err != nil {
			return row, fmt.Errorf("announced_at must be RFC3339: %v", err)
		}
		row.AnnouncedAt.Valid = true
		row.AnnouncedAt.Time = t
	}
	return row, nil
}

// fundsHoldingInstrument resolves which fund_ids currently hold the
// instrument. Used when the operator omits the fund_ids field in
// the request (the typical "apply this split to everyone" path).
func (h *adminHandler) fundsHoldingInstrument(ctx context.Context, instrumentKey string) ([]string, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT DISTINCT fund_id FROM holding_positions WHERE instrument_key = $1 AND quantity > 0`,
		instrumentKey,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var fid string
		if err := rows.Scan(&fid); err != nil {
			return nil, err
		}
		out = append(out, fid)
	}
	return out, rows.Err()
}

// corpActionServiceAdapter implements api.CorpActionService.
//
// It bridges the user-scope read endpoint
// (`GET /api/funds/:fundId/corp-actions`) to the existing
// CorpActionRepo, threading user authorisation through the shared
// authorizeFundAccess helper. Layered this way so the repository
// stays auth-naïve while the HTTP layer remains repository-naïve.
type corpActionServiceAdapter struct {
	repo        *repository.CorpActionRepo
	fundRepo    *repository.FundRepo
	companyRepo *repository.FundCompanyRepo
}

// newCorpActionServiceAdapter constructs the adapter from the
// process-wide *sql.DB. Cheap — only allocates repository
// wrappers; no goroutines, no pools, no migrations.
func newCorpActionServiceAdapter(svc *Services) *corpActionServiceAdapter {
	if svc == nil || svc.DB == nil {
		return nil
	}
	return &corpActionServiceAdapter{
		repo:        repository.NewCorpActionRepo(svc.DB),
		fundRepo:    repository.NewFundRepo(svc.DB),
		companyRepo: repository.NewFundCompanyRepo(svc.DB),
	}
}

// ApplicationsForFund returns the user-visible corp-action timeline
// for a fund, after enforcing fund-membership. Non-members get the
// same sentinel the rest of the API surfaces use, so handleServiceError
// translates it to 403 uniformly.
func (s *corpActionServiceAdapter) ApplicationsForFund(ctx context.Context, userID, fundID string, limit int) ([]api.CorpActionApplicationDTO, error) {
	if s == nil {
		return nil, errors.New("corp action service: not configured")
	}
	if _, err := authorizeFundAccess(ctx, s.fundRepo, s.companyRepo, userID, fundID); err != nil {
		return nil, err
	}
	rows, err := s.repo.ApplicationsForFund(ctx, fundID, limit)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	out := make([]api.CorpActionApplicationDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, api.CorpActionApplicationDTO{
			InstrumentKey: r.InstrumentKey,
			ExDate:        r.ExDate,
			ActionType:    r.ActionType,
			SplitRatio:    r.SplitRatio,
			CashDividend:  r.CashDividend,
			AppliedAt:     r.AppliedAt,
			PreQuantity:   r.PreQuantity,
			PostQuantity:  r.PostQuantity,
			PreCostPrice:  r.PreCost,
			PostCostPrice: r.PostCost,
			CashCredit:    r.CashCredit,
		})
	}
	return out, nil
}
