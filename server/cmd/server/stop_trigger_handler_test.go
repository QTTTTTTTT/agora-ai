package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/broker"
	"github.com/fundai/server/internal/matching"
	"github.com/fundai/server/internal/stoptrigger"
)

// makeStopTriggerSvc returns a Services with a real simulator +
// engine + poller so the status handler exercises the full graph.
func makeStopTriggerSvc(t *testing.T) (*Services, *broker.Simulator) {
	t.Helper()
	q := func(_ context.Context, _, _, _ string) (matching.Quote, error) {
		return matching.Quote{Last: 100, Bid: 99.95, Ask: 100.05}, nil
	}
	sim := broker.NewSimulator(q,
		broker.WithIDGenerator(func() string { return "id-1" }),
		broker.WithClock(func() time.Time { return time.Date(2026, 1, 1, 9, 30, 0, 0, time.UTC) }),
	)
	eng := stoptrigger.New(sim, stoptrigger.WithLogger(silentLogger()))
	pol := newStopTriggerPoller(eng, sim, q, time.Hour, silentLogger())
	return &Services{
		BrokerSimulator:   sim,
		StopTriggerEngine: eng,
		StopTriggerPoller: pol,
	}, sim
}

func adminCtx(req *http.Request) *http.Request {
	ctx := api.WithAuthenticatedUserID(req.Context(), "admin-1")
	ctx = api.WithAuthenticatedUserRole(ctx, userRoleSuperAdmin)
	return req.WithContext(ctx)
}

func userCtx(req *http.Request) *http.Request {
	ctx := api.WithAuthenticatedUserID(req.Context(), "user-1")
	ctx = api.WithAuthenticatedUserRole(ctx, userRoleUser)
	return req.WithContext(ctx)
}

// ---------------------------------------------------------------------------
// Auth gates
// ---------------------------------------------------------------------------

func TestStopTriggerHandler_StatusRequiresSuperAdmin(t *testing.T) {
	svc, _ := makeStopTriggerSvc(t)
	h := newStopTriggerStatusHandler(svc)

	req := userCtx(httptest.NewRequest(http.MethodGet, "/api/admin/stop-trigger/status", nil))
	rr := httptest.NewRecorder()
	h.handleStatus(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rr.Code)
	}
}

func TestStopTriggerHandler_TickRequiresSuperAdmin(t *testing.T) {
	svc, _ := makeStopTriggerSvc(t)
	h := newStopTriggerStatusHandler(svc)

	req := userCtx(httptest.NewRequest(http.MethodPost, "/api/admin/stop-trigger/tick", nil))
	rr := httptest.NewRecorder()
	h.handleTick(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// Status surface
// ---------------------------------------------------------------------------

func TestStopTriggerHandler_Status_EmptyChain(t *testing.T) {
	svc, _ := makeStopTriggerSvc(t)
	h := newStopTriggerStatusHandler(svc)

	req := adminCtx(httptest.NewRequest(http.MethodGet, "/api/admin/stop-trigger/status", nil))
	rr := httptest.NewRecorder()
	h.handleStatus(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["enabled"] != true {
		t.Errorf("enabled = %v, want true", got["enabled"])
	}
	if got["pendingCount"].(float64) != 0 {
		t.Errorf("pendingCount = %v, want 0", got["pendingCount"])
	}
	if _, ok := got["poller"]; !ok {
		t.Errorf("response missing poller field: %v", got)
	}
}

func TestStopTriggerHandler_Status_SurfacesPendingStops(t *testing.T) {
	svc, sim := makeStopTriggerSvc(t)

	if _, err := sim.PlaceOrder(context.Background(), broker.PlaceOrderRequest{
		FundID: "f1", ClientOrderID: "stop-1", Symbol: "AAPL", InstrumentKey: "us:AAPL",
		Side: broker.SideSell, OrderType: broker.OrderTypeStop, Quantity: 10, StopPrice: 95,
	}); err != nil {
		t.Fatal(err)
	}

	h := newStopTriggerStatusHandler(svc)
	req := adminCtx(httptest.NewRequest(http.MethodGet, "/api/admin/stop-trigger/status", nil))
	rr := httptest.NewRecorder()
	h.handleStatus(rr, req)

	var got struct {
		PendingCount int               `json:"pendingCount"`
		PendingStops []pendingStopView `json:"pendingStops"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.PendingCount != 1 {
		t.Errorf("pendingCount = %d, want 1", got.PendingCount)
	}
	if len(got.PendingStops) != 1 {
		t.Fatalf("pendingStops len = %d, want 1", len(got.PendingStops))
	}
	v := got.PendingStops[0]
	if v.Symbol != "AAPL" || v.Side != "sell" || v.OrderType != "stop" || v.StopPrice != 95 {
		t.Errorf("pendingStop view wrong: %+v", v)
	}
}

func TestStopTriggerHandler_Tick_DispatchesToPoller(t *testing.T) {
	svc, sim := makeStopTriggerSvc(t)
	if _, err := sim.PlaceOrder(context.Background(), broker.PlaceOrderRequest{
		FundID: "f1", ClientOrderID: "stop-1", Symbol: "AAPL", InstrumentKey: "us:AAPL",
		Side: broker.SideSell, OrderType: broker.OrderTypeStop, Quantity: 10, StopPrice: 200, // never fires under quote=100
	}); err != nil {
		t.Fatal(err)
	}

	h := newStopTriggerStatusHandler(svc)
	req := adminCtx(httptest.NewRequest(http.MethodPost, "/api/admin/stop-trigger/tick", nil))
	rr := httptest.NewRecorder()
	h.handleTick(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		OK     bool                      `json:"ok"`
		Poller stopTriggerPollerSnapshot `json:"poller"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.OK {
		t.Errorf("ok = false, want true")
	}
	if got.Poller.Ticks == 0 {
		t.Errorf("Ticks = 0, expected at least 1 after manual tick")
	}
}

func TestStopTriggerHandler_Tick_503WhenPollerMissing(t *testing.T) {
	// No poller, no sim.
	h := newStopTriggerStatusHandler(&Services{})
	req := adminCtx(httptest.NewRequest(http.MethodPost, "/api/admin/stop-trigger/tick", nil))
	rr := httptest.NewRecorder()
	h.handleTick(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when poller missing, got %d", rr.Code)
	}
}

func TestNewStopTriggerStatusHandler_NilOnNilSvc(t *testing.T) {
	if newStopTriggerStatusHandler(nil) != nil {
		t.Errorf("expected nil for nil svc")
	}
}
