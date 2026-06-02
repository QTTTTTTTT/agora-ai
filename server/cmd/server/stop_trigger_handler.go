package main

import (
	"encoding/json"
	"net/http"

	"github.com/fundai/server/internal/broker"
)

// stopTriggerStatusHandler exposes admin-only observability for the
// P0-3 stop-trigger pipeline. Two endpoints:
//
//	GET /api/admin/stop-trigger/status   — poller counters + a list
//	                                       of all currently pending
//	                                       stops (across all funds).
//	POST /api/admin/stop-trigger/tick    — manually run one trigger
//	                                       pass. Useful when an
//	                                       operator wants to verify
//	                                       a fix without waiting for
//	                                       the next interval.
//
// Both endpoints are super-admin gated.
type stopTriggerStatusHandler struct {
	poller *stopTriggerPoller
	sim    *broker.Simulator
}

func newStopTriggerStatusHandler(svc *Services) *stopTriggerStatusHandler {
	if svc == nil {
		return nil
	}
	return &stopTriggerStatusHandler{
		poller: svc.StopTriggerPoller,
		sim:    svc.BrokerSimulator,
	}
}

func (h *stopTriggerStatusHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/stop-trigger/status", h.handleStatus)
	mux.HandleFunc("POST /api/admin/stop-trigger/tick", h.handleTick)
}

// pendingStopView is the JSON shape we emit per pending stop. We
// deliberately pull a slim subset of broker.Order so we don't expose
// internal IDs and request metadata that admins don't need to see.
type pendingStopView struct {
	BrokerOrderID     string  `json:"brokerOrderId"`
	ClientOrderID     string  `json:"clientOrderId"`
	FundID            string  `json:"fundId"`
	Symbol            string  `json:"symbol"`
	InstrumentKey     string  `json:"instrumentKey"`
	Side              string  `json:"side"`
	OrderType         string  `json:"orderType"`
	Quantity          float64 `json:"quantity"`
	StopPrice         float64 `json:"stopPrice"`
	CurrentStopPrice  float64 `json:"currentStopPrice"`
	TrailingHighWater float64 `json:"trailingHighWater,omitempty"`
	TrailingLowWater  float64 `json:"trailingLowWater,omitempty"`
	TrailAmount       float64 `json:"trailAmount,omitempty"`
	TrailPercent      float64 `json:"trailPercent,omitempty"`
	PlacedAt          string  `json:"placedAt"`
}

func (h *stopTriggerStatusHandler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	resp := map[string]any{
		"enabled": h.poller != nil,
	}
	if h.poller != nil {
		resp["poller"] = h.poller.Snapshot()
	}
	if h.sim != nil {
		pendings := h.sim.AllPendingStops()
		views := make([]pendingStopView, 0, len(pendings))
		for _, o := range pendings {
			views = append(views, pendingStopView{
				BrokerOrderID:     o.BrokerOrderID,
				ClientOrderID:     o.ClientOrderID,
				FundID:            o.Request.FundID,
				Symbol:            o.Request.Symbol,
				InstrumentKey:     o.Request.InstrumentKey,
				Side:              string(o.Request.Side),
				OrderType:         string(o.Request.OrderType),
				Quantity:          o.Request.Quantity,
				StopPrice:         o.Request.StopPrice,
				CurrentStopPrice:  o.CurrentStopPrice,
				TrailingHighWater: o.TrailingHighWater,
				TrailingLowWater:  o.TrailingLowWater,
				TrailAmount:       o.Request.TrailAmount,
				TrailPercent:      o.Request.TrailPercent,
				PlacedAt:          o.PlacedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			})
		}
		resp["pendingStops"] = views
		resp["pendingCount"] = len(views)
	}
	writeStopTriggerJSON(w, http.StatusOK, resp)
}

func (h *stopTriggerStatusHandler) handleTick(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.poller == nil {
		writeStopTriggerJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":  "stop_trigger_disabled",
			"detail": "stop trigger poller not wired (missing market data or simulator)",
		})
		return
	}
	h.poller.Tick(r.Context())
	writeStopTriggerJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"poller": h.poller.Snapshot(),
	})
}

// writeStopTriggerJSON keeps the encoder local so this file doesn't
// reach for the package-wide writeJSON helper, which lives in
// main.go and changes signature when other unrelated handlers move.
func writeStopTriggerJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
