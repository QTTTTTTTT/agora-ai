package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// CNIntradayService is the api-package façade for the
// internal/cnintraday package. Stage-5 exposes a single
// "dry-run" endpoint: the operator submits a per-symbol snapshot
// (manually crafted or replayed from a fixture), the engine
// computes the 5 factors + runs the rule engine, and the
// resulting TradeSignal (or null) plus the Feishu render are
// returned.
//
// Critically, we do NOT expose a "start the live engine" endpoint
// here — that's a separate cmd/cnintraday-runner binary that
// runs in the operator's home network. The reason: the live
// engine needs minute-data credentials (baostock / akshare auth)
// and a Feishu webhook URL that we DON'T want shipped via the
// public HTTP surface.
type CNIntradayService interface {
	DryRunSignal(userID string, input CNIntradayDryRunInput) (*CNIntradayDryRunResult, error)
}

// ErrCNIntradayUnconfigured is the canonical sentinel.
var ErrCNIntradayUnconfigured = errors.New("cnintraday service not configured")

// CNIntradayDryRunInput is the dry-run request body.
type CNIntradayDryRunInput struct {
	Symbol     string             `json:"symbol"`
	Name       string             `json:"name,omitempty"`
	Market     string             `json:"market"` // main_board / chinext / star / st / bse
	PrevClose  float64            `json:"prevClose"`
	Bars       []CNIntradayBarView `json:"bars"` // ascending minute series, last bar = "now"
	NowBeijing string             `json:"nowBeijing,omitempty"` // YYYY-MM-DDTHH:MM (Beijing); empty = last bar timestamp
	SectorRank float64            `json:"sectorRank,omitempty"` // 0..1; 0 = bottom of sector
	RuleSet    string             `json:"ruleSet,omitempty"` // "conservative" | "aggressive" (default conservative)
}

type CNIntradayBarView struct {
	Timestamp      string  `json:"timestamp"` // RFC3339 or "YYYY-MM-DD HH:MM"
	Open           float64 `json:"open"`
	High           float64 `json:"high"`
	Low            float64 `json:"low"`
	Close          float64 `json:"close"`
	Volume         float64 `json:"volume"`
	Amount         float64 `json:"amount,omitempty"`
	BidAskRatio    float64 `json:"bidAskRatio,omitempty"`
	BigOrderNet    float64 `json:"bigOrderNet,omitempty"`
}

// CNIntradayDryRunResult is the response body. Signal may be nil
// (no trade triggered); FactorScores is always populated so the
// operator can see why no signal fired.
type CNIntradayDryRunResult struct {
	Signal       *CNIntradaySignalView    `json:"signal,omitempty"`
	FactorScores CNIntradayFactorTuple    `json:"factorScores"`
	Feishu       *CNIntradayFeishuPreview `json:"feishu,omitempty"`
}

type CNIntradaySignalView struct {
	Timestamp         time.Time           `json:"timestamp"`
	Symbol            string              `json:"symbol"`
	Name              string              `json:"name"`
	Type              string              `json:"type"`
	Price             float64             `json:"price"`
	Confidence        float64             `json:"confidence"`
	SuggestedPosition float64             `json:"suggestedPosition"`
	TargetPrice       float64             `json:"targetPrice"`
	StopLoss          float64             `json:"stopLoss"`
	Reasons           []string            `json:"reasons"`
	RiskWarnings      []string            `json:"riskWarnings"`
}

type CNIntradayFactorTuple struct {
	Breakout       float64 `json:"breakout"`
	VolumeSurge    float64 `json:"volumeSurge"`
	BigInflow      float64 `json:"bigInflow"`
	OrderImbalance float64 `json:"orderImbalance"`
	SectorRank     float64 `json:"sectorRank"`
}

// CNIntradayFeishuPreview is the rendered Feishu card the operator
// can paste-test. The frontend renders this as a faux mobile card.
type CNIntradayFeishuPreview struct {
	Title string   `json:"title"`
	Lines []string `json:"lines"`
}

// DryRunCNIntradaySignal handles POST /api/cnintraday/signals/dry-run.
// Auth: any logged-in user. No persistence — purely stateless.
func (h *FundHandler) DryRunCNIntradaySignal(w http.ResponseWriter, r *http.Request) {
	if h.cnIntraday == nil {
		writeError(w, http.StatusServiceUnavailable, "cnintraday service not configured", "")
		return
	}
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	var input CNIntradayDryRunInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body", err.Error())
		return
	}
	out, err := h.cnIntraday.DryRunSignal(userID, input)
	if err != nil {
		if errors.Is(err, ErrCNIntradayUnconfigured) {
			writeError(w, http.StatusServiceUnavailable, err.Error(), "")
			return
		}
		writeError(w, http.StatusBadRequest, "dry-run failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
