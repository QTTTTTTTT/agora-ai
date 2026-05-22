package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// PromotionService is the HTTP-facing seam to the promotion
// lifecycle. Implementation lives in cmd/server so the api
// package stays free of repository / promotion package imports.
type PromotionService interface {
	// Propose creates a pending_review promotion using the basis
	// backtest as the seed. The basis job MUST belong to the
	// fund and be in 'completed' status; the implementation may
	// additionally require walk-forward validation.
	Propose(userID string, in ProposeInput) (*Promotion, error)
	// Approve moves a pending_review promotion to approved (and
	// optionally straight into shadow / active based on
	// ShadowDays). Approver MUST differ from proposer when the
	// implementation enforces dual-control.
	Approve(userID, fundID, promotionID string) (*Promotion, error)
	// Reject is the terminal path: pending_review or shadow →
	// rejected, with an optional reason.
	Reject(userID, fundID, promotionID, reason string) (*Promotion, error)
	// Activate the shadow → active flip. Supersedes any prior
	// active promotion for the same fund.
	Activate(userID, fundID, promotionID string) (*Promotion, error)
	// Rollback an active promotion. Moves it to rolled_back and
	// returns control to whatever default engine the fund had
	// before.
	Rollback(userID, fundID, promotionID, reason string) (*Promotion, error)
	// List returns recent promotions for a fund, newest first.
	List(userID, fundID string, limit int) ([]*Promotion, error)
	// Get returns one promotion + its audit log + recent shadow
	// diffs + health snapshots so the detail page can render
	// without N round-trips.
	Get(userID, fundID, promotionID string) (*PromotionDetail, error)
}

// ProposeInput mirrors the request body. UserID + FundID come
// from the URL / auth context so they're not in the JSON.
type ProposeInput struct {
	FundID       string         `json:"-"`
	ProposedBy   string         `json:"-"`
	BasisJobID   string         `json:"basisJobId"`
	EngineParams map[string]any `json:"engineParams,omitempty"`
	ShadowDays   *int           `json:"shadowDays,omitempty"`
	DecayRatio   *float64       `json:"decayRatio,omitempty"`
	Notes        string         `json:"notes,omitempty"`
}

// Promotion is the JSON projection of the domain object. We
// flatten BaselineMetrics inline so the UI doesn't have to walk
// a nested object for the common case.
type Promotion struct {
	ID                string                 `json:"id"`
	FundID            string                 `json:"fundId"`
	ProposedBy        string                 `json:"proposedBy"`
	BasisJobID        string                 `json:"basisJobId"`
	EngineKind        string                 `json:"engineKind"`
	EngineParams      map[string]any         `json:"engineParams"`
	BaselineMetrics   PromotionBaseline      `json:"baselineMetrics"`
	Status            string                 `json:"status"`
	ShadowDays        int                    `json:"shadowDays"`
	DecayRatio        float64                `json:"decayRatio"`
	ApprovedBy        string                 `json:"approvedBy,omitempty"`
	ApprovedAt        *time.Time             `json:"approvedAt,omitempty"`
	RejectedBy        string                 `json:"rejectedBy,omitempty"`
	RejectedAt        *time.Time             `json:"rejectedAt,omitempty"`
	RejectedReason    string                 `json:"rejectedReason,omitempty"`
	ShadowStartedAt   *time.Time             `json:"shadowStartedAt,omitempty"`
	ShadowCompletedAt *time.Time             `json:"shadowCompletedAt,omitempty"`
	ActivatedAt       *time.Time             `json:"activatedAt,omitempty"`
	DeactivatedAt     *time.Time             `json:"deactivatedAt,omitempty"`
	DeactivatedReason string                 `json:"deactivatedReason,omitempty"`
	Notes             string                 `json:"notes,omitempty"`
	CreatedAt         time.Time              `json:"createdAt"`
	UpdatedAt         time.Time              `json:"updatedAt"`
}

type PromotionBaseline struct {
	CumulativeReturn float64  `json:"cumulativeReturn"`
	AnnualizedReturn float64  `json:"annualizedReturn"`
	SharpeRatio      float64  `json:"sharpeRatio"`
	Volatility       float64  `json:"volatility"`
	MaxDrawdown      float64  `json:"maxDrawdown"`
	WinRate          float64  `json:"winRate"`
	TradeCount       int      `json:"tradeCount"`
	OOSReturn        *float64 `json:"oosReturn,omitempty"`
	OOSSharpe        *float64 `json:"oosSharpe,omitempty"`
}

// PromotionEvent is the audit-log row in JSON form. CreatedAt is
// the timeline pivot.
type PromotionEvent struct {
	ID          string                 `json:"id"`
	EventType   string                 `json:"eventType"`
	ActorUserID string                 `json:"actorUserId,omitempty"`
	Payload     map[string]any         `json:"payload,omitempty"`
	CreatedAt   time.Time              `json:"createdAt"`
}

// PromotionShadowDiff is one daily comparison row in JSON form.
type PromotionShadowDiff struct {
	ID             string         `json:"id"`
	TradingDate    time.Time      `json:"tradingDate"`
	ShadowDecision map[string]any `json:"shadowDecision"`
	ActiveDecision map[string]any `json:"activeDecision"`
	Agreement      bool           `json:"agreement"`
	CreatedAt      time.Time      `json:"createdAt"`
}

// PromotionHealth is one decay-monitor snapshot in JSON form.
type PromotionHealth struct {
	ID                string     `json:"id"`
	SnapshotAt        time.Time  `json:"snapshotAt"`
	WindowDays        int        `json:"windowDays"`
	ActualSharpe      *float64   `json:"actualSharpe,omitempty"`
	ActualReturn      *float64   `json:"actualReturn,omitempty"`
	ActualMaxDrawdown *float64   `json:"actualMaxDrawdown,omitempty"`
	ActualTradeCount  int        `json:"actualTradeCount"`
	SharpeDecayRatio  *float64   `json:"sharpeDecayRatio,omitempty"`
	DecayFlag         bool       `json:"decayFlag"`
	Notes             string     `json:"notes,omitempty"`
}

// PromotionDetail is the composite detail view: header + events
// + recent shadow comparisons + recent health snapshots + a
// derived agreement ratio. Returned by GET so the UI renders the
// full detail page in one round-trip.
type PromotionDetail struct {
	Promotion        Promotion              `json:"promotion"`
	Events           []*PromotionEvent      `json:"events"`
	ShadowDiffs      []*PromotionShadowDiff `json:"shadowDiffs"`
	Health           []*PromotionHealth     `json:"health"`
	AgreementRatio   float64                `json:"agreementRatio"`
	AgreementSamples int                    `json:"agreementSamples"`
}

// Sentinel errors translated by handleServiceError + by some
// path-specific 400 branches below.
var (
	ErrPromotionNotFound          = errors.New("promotion not found")
	ErrPromotionInvalid           = errors.New("invalid promotion input")
	ErrPromotionBasisIneligible   = errors.New("basis backtest is not eligible for promotion")
	ErrPromotionIllegalTransition = errors.New("illegal promotion status transition")
	ErrPromotionDualControl       = errors.New("dual control: approver must differ from proposer")
)

// WithPromotionService wires the Phase 2J/K/L service. nil keeps
// the endpoints in a 503 mode so legacy deployments stay green.
func (h *FundHandler) WithPromotionService(svc PromotionService) *FundHandler {
	if h != nil {
		h.promotions = svc
	}
	return h
}

// ProposePromotion handles POST /api/funds/{fundId}/promotions.
//
// Body: ProposeInput. The handler injects fundId from the URL +
// proposedBy from the auth context. Returns 201 with the new
// pending_review row.
func (h *FundHandler) ProposePromotion(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	if h.promotions == nil {
		writeError(w, http.StatusServiceUnavailable, "promotion service unavailable", "")
		return
	}
	var in ProposeInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	in.FundID = fundID
	in.ProposedBy = userID
	if strings.TrimSpace(in.BasisJobID) == "" {
		writeError(w, http.StatusBadRequest, "invalid request", "basisJobId required")
		return
	}
	p, err := h.promotions.Propose(userID, in)
	if err != nil {
		writePromotionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// ApprovePromotion handles POST /api/funds/{fundId}/promotions/{id}/approve.
func (h *FundHandler) ApprovePromotion(w http.ResponseWriter, r *http.Request) {
	h.handlePromotionTransition(w, r, "approve")
}

// RejectPromotion handles POST /api/funds/{fundId}/promotions/{id}/reject.
func (h *FundHandler) RejectPromotion(w http.ResponseWriter, r *http.Request) {
	h.handlePromotionTransition(w, r, "reject")
}

// ActivatePromotion handles POST /api/funds/{fundId}/promotions/{id}/activate.
func (h *FundHandler) ActivatePromotion(w http.ResponseWriter, r *http.Request) {
	h.handlePromotionTransition(w, r, "activate")
}

// RollbackPromotion handles POST /api/funds/{fundId}/promotions/{id}/rollback.
func (h *FundHandler) RollbackPromotion(w http.ResponseWriter, r *http.Request) {
	h.handlePromotionTransition(w, r, "rollback")
}

// handlePromotionTransition factors the four transition endpoints
// — they share auth + fund/id extraction + reason-body decode.
func (h *FundHandler) handlePromotionTransition(w http.ResponseWriter, r *http.Request, action string) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	promotionID := pathValue(r, "promotionId")
	if !requireNonEmpty(w, fundID, "fundId") || !requireNonEmpty(w, promotionID, "promotionId") {
		return
	}
	if h.promotions == nil {
		writeError(w, http.StatusServiceUnavailable, "promotion service unavailable", "")
		return
	}
	// reject + rollback accept an optional reason body. approve
	// + activate ignore the body — but a stray body shouldn't
	// break them.
	var body struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil {
		// Best-effort decode; ignore EOF / empty-body errors.
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	var (
		p   *Promotion
		err error
	)
	switch action {
	case "approve":
		p, err = h.promotions.Approve(userID, fundID, promotionID)
	case "reject":
		p, err = h.promotions.Reject(userID, fundID, promotionID, body.Reason)
	case "activate":
		p, err = h.promotions.Activate(userID, fundID, promotionID)
	case "rollback":
		p, err = h.promotions.Rollback(userID, fundID, promotionID, body.Reason)
	default:
		writeError(w, http.StatusInternalServerError, "bad action", action)
		return
	}
	if err != nil {
		writePromotionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ListPromotions handles GET /api/funds/{fundId}/promotions.
func (h *FundHandler) ListPromotions(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	if h.promotions == nil {
		writeJSON(w, http.StatusOK, []*Promotion{})
		return
	}
	out, err := h.promotions.List(userID, fundID, 0)
	if err != nil {
		writePromotionError(w, err)
		return
	}
	if out == nil {
		out = []*Promotion{}
	}
	writeJSON(w, http.StatusOK, out)
}

// GetPromotion handles GET /api/funds/{fundId}/promotions/{promotionId}.
func (h *FundHandler) GetPromotion(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	promotionID := pathValue(r, "promotionId")
	if !requireNonEmpty(w, fundID, "fundId") || !requireNonEmpty(w, promotionID, "promotionId") {
		return
	}
	if h.promotions == nil {
		writeError(w, http.StatusServiceUnavailable, "promotion service unavailable", "")
		return
	}
	out, err := h.promotions.Get(userID, fundID, promotionID)
	if err != nil {
		writePromotionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// writePromotionError translates the package sentinel errors into
// HTTP status codes. Unknown errors fall through to a 500.
func writePromotionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrPromotionNotFound):
		writeError(w, http.StatusNotFound, "promotion not found", err.Error())
	case errors.Is(err, ErrPromotionInvalid):
		writeError(w, http.StatusBadRequest, "invalid promotion", err.Error())
	case errors.Is(err, ErrPromotionBasisIneligible):
		writeError(w, http.StatusBadRequest, "basis backtest not eligible", err.Error())
	case errors.Is(err, ErrPromotionIllegalTransition):
		writeError(w, http.StatusConflict, "illegal status transition", err.Error())
	case errors.Is(err, ErrPromotionDualControl):
		writeError(w, http.StatusForbidden, "dual control violation", err.Error())
	default:
		handleServiceError(w, err, "promotion")
	}
}
