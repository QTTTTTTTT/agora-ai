package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// PaperTradingService is the api-package façade for the
// internal/papertrading package. Same nil-safety contract as the
// other services on FundHandler.
type PaperTradingService interface {
	CreatePortfolio(userID string, input PaperPortfolioInput) (*PaperPortfolioView, error)
	ListPortfolios(userID string) ([]*PaperPortfolioView, error)
	GetPortfolio(userID, portfolioID string) (*PaperPortfolioView, error)

	ProposeOrder(userID string, input ProposeOrderAPIInput) (*PaperOrderView, error)
	ListOrders(userID, portfolioID string, limit int) ([]*PaperOrderView, error)

	NavHistory(userID, portfolioID string) ([]PaperNavPointView, error)
	SnapshotNAV(userID string, input SnapshotNAVAPIInput) error
}

// ErrPaperTradingUnconfigured signals the service wasn't wired.
var ErrPaperTradingUnconfigured = errors.New("paper trading service not configured")

// Input / view types ----------------------------------------------------------

type PaperPortfolioInput struct {
	Name            string  `json:"name"`
	Strategy        string  `json:"strategy"`
	Market          string  `json:"market"`
	BenchmarkSymbol string  `json:"benchmarkSymbol,omitempty"`
	InitialCapital  float64 `json:"initialCapital"`
}

type PaperPortfolioView struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Strategy        string     `json:"strategy"`
	Market          string     `json:"market"`
	BenchmarkSymbol string     `json:"benchmarkSymbol,omitempty"`
	InitialCapital  float64    `json:"initialCapital"`
	CurrentNav      float64    `json:"currentNav"`
	CashBalance     float64    `json:"cashBalance"`
	CreatedAt       time.Time  `json:"createdAt"`
	LastRebalanceAt *time.Time `json:"lastRebalanceAt,omitempty"`
}

type ProposeOrderAPIInput struct {
	PortfolioID  string                 `json:"portfolioId"`
	Symbol       string                 `json:"symbol"`
	Action       string                 `json:"action"`
	TargetWeight *float64               `json:"targetWeight,omitempty"`
	SharesChange *float64               `json:"sharesChange,omitempty"`
	DecidedPrice *float64               `json:"decidedPrice,omitempty"`
	AIReasoning  map[string]interface{} `json:"aiReasoning,omitempty"`
}

type PaperOrderView struct {
	ID               string          `json:"id"`
	PortfolioID      string          `json:"portfolioId"`
	Symbol           string          `json:"symbol"`
	Action           string          `json:"action"`
	TargetWeight     *float64        `json:"targetWeight,omitempty"`
	SharesChange     *float64        `json:"sharesChange,omitempty"`
	DecidedAt        time.Time       `json:"decidedAt"`
	DecidedPrice     *float64        `json:"decidedPrice,omitempty"`
	ExecutedAt       *time.Time      `json:"executedAt,omitempty"`
	ExecutedPrice    *float64        `json:"executedPrice,omitempty"`
	AIReasoning      json.RawMessage `json:"aiReasoning,omitempty"`
	HashSignature    string          `json:"hashSignature"`
	CanonicalPayload string          `json:"canonicalPayload"`
	PublicProofURL   string          `json:"publicProofURL,omitempty"`
	OTSStatus        string          `json:"otsStatus"`
}

type PaperNavPointView struct {
	Date         time.Time `json:"date"`
	Nav          float64   `json:"nav"`
	DailyReturn  *float64  `json:"dailyReturn,omitempty"`
	BenchmarkNav *float64  `json:"benchmarkNav,omitempty"`
}

type SnapshotNAVAPIInput struct {
	PortfolioID  string                              `json:"portfolioId"`
	SnapshotDate string                              `json:"snapshotDate"`
	Nav          float64                             `json:"nav"`
	DailyReturn  *float64                            `json:"dailyReturn,omitempty"`
	BenchmarkNav *float64                            `json:"benchmarkNav,omitempty"`
	CashBalance  float64                             `json:"cashBalance"`
	Holdings     map[string]PaperHoldingPositionView `json:"holdings,omitempty"`
}

type PaperHoldingPositionView struct {
	Shares      float64 `json:"shares"`
	MarketValue float64 `json:"marketValue"`
	Weight      float64 `json:"weight"`
}

// Handlers --------------------------------------------------------------------

// CreatePaperPortfolio handles POST /api/papertrading/portfolios.
func (h *FundHandler) CreatePaperPortfolio(w http.ResponseWriter, r *http.Request) {
	if h.paperTrading == nil {
		writeError(w, http.StatusServiceUnavailable, "paper trading service not configured", "")
		return
	}
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	var input PaperPortfolioInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body", err.Error())
		return
	}
	if strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.Strategy) == "" || strings.TrimSpace(input.Market) == "" {
		writeError(w, http.StatusBadRequest, "name, strategy, market are required", "")
		return
	}
	if input.InitialCapital <= 0 {
		writeError(w, http.StatusBadRequest, "initialCapital must be > 0", "")
		return
	}
	out, err := h.paperTrading.CreatePortfolio(userID, input)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create portfolio failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// ListPaperPortfolios handles GET /api/papertrading/portfolios.
func (h *FundHandler) ListPaperPortfolios(w http.ResponseWriter, r *http.Request) {
	if h.paperTrading == nil {
		writeError(w, http.StatusServiceUnavailable, "paper trading service not configured", "")
		return
	}
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	out, err := h.paperTrading.ListPortfolios(userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list portfolios failed", err.Error())
		return
	}
	if out == nil {
		out = []*PaperPortfolioView{}
	}
	writeJSON(w, http.StatusOK, out)
}

// GetPaperPortfolio handles GET /api/papertrading/portfolios/{portfolioId}.
func (h *FundHandler) GetPaperPortfolio(w http.ResponseWriter, r *http.Request) {
	if h.paperTrading == nil {
		writeError(w, http.StatusServiceUnavailable, "paper trading service not configured", "")
		return
	}
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	pid := r.PathValue("portfolioId")
	out, err := h.paperTrading.GetPortfolio(userID, pid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get portfolio failed", err.Error())
		return
	}
	if out == nil {
		writeError(w, http.StatusNotFound, "portfolio not found", "")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ProposePaperOrder handles POST /api/papertrading/orders.
func (h *FundHandler) ProposePaperOrder(w http.ResponseWriter, r *http.Request) {
	if h.paperTrading == nil {
		writeError(w, http.StatusServiceUnavailable, "paper trading service not configured", "")
		return
	}
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	var input ProposeOrderAPIInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body", err.Error())
		return
	}
	out, err := h.paperTrading.ProposeOrder(userID, input)
	if err != nil {
		writeError(w, http.StatusBadRequest, "propose order failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// ListPaperOrders handles GET /api/papertrading/portfolios/{portfolioId}/orders.
func (h *FundHandler) ListPaperOrders(w http.ResponseWriter, r *http.Request) {
	if h.paperTrading == nil {
		writeError(w, http.StatusServiceUnavailable, "paper trading service not configured", "")
		return
	}
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	pid := r.PathValue("portfolioId")
	limit := 100
	if q := strings.TrimSpace(r.URL.Query().Get("limit")); q != "" {
		if v, err := strconv.Atoi(q); err == nil && v > 0 && v <= 1000 {
			limit = v
		}
	}
	out, err := h.paperTrading.ListOrders(userID, pid, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list orders failed", err.Error())
		return
	}
	if out == nil {
		out = []*PaperOrderView{}
	}
	writeJSON(w, http.StatusOK, out)
}

// GetPaperNavHistory handles GET /api/papertrading/portfolios/{portfolioId}/nav.
func (h *FundHandler) GetPaperNavHistory(w http.ResponseWriter, r *http.Request) {
	if h.paperTrading == nil {
		writeError(w, http.StatusServiceUnavailable, "paper trading service not configured", "")
		return
	}
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	pid := r.PathValue("portfolioId")
	out, err := h.paperTrading.NavHistory(userID, pid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "nav history failed", err.Error())
		return
	}
	if out == nil {
		out = []PaperNavPointView{}
	}
	writeJSON(w, http.StatusOK, out)
}

// SnapshotPaperNav handles POST /api/papertrading/snapshots. Used by
// cron / scheduler to push daily NAV snapshots.
func (h *FundHandler) SnapshotPaperNav(w http.ResponseWriter, r *http.Request) {
	if h.paperTrading == nil {
		writeError(w, http.StatusServiceUnavailable, "paper trading service not configured", "")
		return
	}
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	var input SnapshotNAVAPIInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body", err.Error())
		return
	}
	if err := h.paperTrading.SnapshotNAV(userID, input); err != nil {
		writeError(w, http.StatusBadRequest, "snapshot nav failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
