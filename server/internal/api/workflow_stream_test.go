package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestStreamWorkflowStatusPushesInitialAndDiffFrames covers the happy
// path: connect → ":connected" → first frame contains the initial
// state → on next tick, only push when fingerprint changed.
func TestStreamWorkflowStatusPushesInitialAndDiffFrames(t *testing.T) {
	var calls int64

	// Three calls expected:
	//   call 1: handler authorisation read (initial frame seed)
	//   call 2: tick 1 — same fingerprint, MUST be suppressed
	//   call 3: tick 2 — fingerprint changes (state running→completed)
	getStatus := func(_, _ string) (*WorkflowStatus, error) {
		n := atomic.AddInt64(&calls, 1)
		switch n {
		case 1, 2:
			return &WorkflowStatus{
				FundID:          "fund-1",
				State:           "running",
				Step:            "agent_round_3",
				ProgressPercent: 40,
				CompletedSteps:  4,
				FailedSteps:     0,
			}, nil
		default:
			return &WorkflowStatus{
				FundID:          "fund-1",
				State:           "completed",
				ProgressPercent: 100,
				CompletedSteps:  10,
				FailedSteps:     0,
				CompletedAt:     "2026-06-05T08:35:00Z",
			}, nil
		}
	}

	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{getStatusFn: getStatus},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	server := httptest.NewServer(authInjectingMiddleware("user-1", mux))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 500ms is the clamped-up minimum. The test would otherwise
	// wait the full 2s default to observe a second tick.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/funds/fund-1/workflow/stream?interval=500ms", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("expected text/event-stream content-type, got %q", got)
	}

	accumulator := &sseAccumulator{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				accumulator.Append(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	if err := accumulator.WaitFor(": connected", 500*time.Millisecond); err != nil {
		t.Fatalf(": connected absent: %v", err)
	}
	if err := accumulator.WaitFor(`"state":"running"`, 1*time.Second); err != nil {
		t.Fatalf("initial frame missing running state: %v", err)
	}
	if err := accumulator.WaitFor(`"state":"completed"`, 3*time.Second); err != nil {
		t.Fatalf("terminal frame missing completed state: %v", err)
	}

	// Once "completed" is seen the handler closes the stream itself
	// (see workflow_stream.go terminal-state guard). We give it a
	// generous timeout because the read goroutine has to drain the
	// last frame before it sees EOF.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("stream did not close after terminal state")
	}
}

// TestStreamWorkflowStatusRejectsUnauthenticated ensures the SSE
// endpoint denies anonymous clients before opening the stream. (Same
// safeguard the REST endpoint has.)
func TestStreamWorkflowStatusRejectsUnauthenticated(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{getStatusFn: func(_, _ string) (*WorkflowStatus, error) {
			t.Fatalf("auth bypass: handler should not invoke service")
			return nil, nil
		}},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	server := httptest.NewServer(mux) // no auth middleware
	defer server.Close()

	resp, err := server.Client().Get(server.URL + "/api/funds/fund-1/workflow/stream")
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

// TestStreamWorkflowStatusPropagatesAuthError covers the "service
// returned an error during initial read" branch: we must surface a
// non-2xx response and never write text/event-stream bytes.
func TestStreamWorkflowStatusPropagatesAuthError(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{getStatusFn: func(_, _ string) (*WorkflowStatus, error) {
			return nil, errors.New("repository: record not found")
		}},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	server := httptest.NewServer(authInjectingMiddleware("user-1", mux))
	defer server.Close()

	resp, err := server.Client().Get(server.URL + "/api/funds/fund-x/workflow/stream")
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected non-200, got 200")
	}
	if got := resp.Header.Get("Content-Type"); strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("expected non-stream content-type on error, got %q", got)
	}
}

// TestStreamWorkflowStatusMultiFansOutInitialFrames verifies that
// the W4-25 multiplex endpoint, when given two readable funds,
// emits one initial `event: workflow` frame per fund — each
// envelope tagged with its own `fundId`. This is the core
// invariant: a single connection, N funds.
func TestStreamWorkflowStatusMultiFansOutInitialFrames(t *testing.T) {
	getStatus := func(_, fundID string) (*WorkflowStatus, error) {
		switch fundID {
		case "fund-a":
			return &WorkflowStatus{FundID: "fund-a", State: "running", ProgressPercent: 30}, nil
		case "fund-b":
			return &WorkflowStatus{FundID: "fund-b", State: "completed", ProgressPercent: 100, CompletedAt: "2026-06-05T08:35:00Z"}, nil
		default:
			return nil, errors.New("repository: record not found")
		}
	}
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{getStatusFn: getStatus},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	server := httptest.NewServer(authInjectingMiddleware("user-1", mux))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/funds/workflow/stream?fundIds=fund-a,fund-b&interval=500ms", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	accumulator := &sseAccumulator{}
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				accumulator.Append(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	if err := accumulator.WaitFor(": connected", 500*time.Millisecond); err != nil {
		t.Fatalf(": connected absent: %v", err)
	}
	if err := accumulator.WaitFor(`"fundId":"fund-a"`, 1*time.Second); err != nil {
		t.Fatalf("fund-a frame missing: %v", err)
	}
	if err := accumulator.WaitFor(`"fundId":"fund-b"`, 1*time.Second); err != nil {
		t.Fatalf("fund-b frame missing: %v", err)
	}
	if err := accumulator.WaitFor(`"terminal":true`, 1*time.Second); err != nil {
		t.Fatalf("terminal flag missing on fund-b: %v", err)
	}
}

// TestStreamWorkflowStatusMultiSurfacesForbiddenFunds confirms
// that funds the caller cannot read are emitted as one-shot
// `event: error` frames rather than silently dropped — the
// dashboard needs the negative signal to render a "no access"
// placeholder for the missing card.
func TestStreamWorkflowStatusMultiSurfacesForbiddenFunds(t *testing.T) {
	getStatus := func(_, fundID string) (*WorkflowStatus, error) {
		switch fundID {
		case "fund-ok":
			return &WorkflowStatus{FundID: "fund-ok", State: "completed", CompletedAt: "2026-06-05T08:35:00Z"}, nil
		default:
			return nil, errors.New("repository: forbidden")
		}
	}
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{getStatusFn: getStatus},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	server := httptest.NewServer(authInjectingMiddleware("user-1", mux))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/funds/workflow/stream?fundIds=fund-ok,fund-denied&interval=500ms", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	accumulator := &sseAccumulator{}
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				accumulator.Append(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	if err := accumulator.WaitFor(`"fundId":"fund-ok"`, 1*time.Second); err != nil {
		t.Fatalf("ok frame missing: %v", err)
	}
	if err := accumulator.WaitFor(`event: error`, 1*time.Second); err != nil {
		t.Fatalf("error frame for forbidden fund missing: %v", err)
	}
	if err := accumulator.WaitFor(`"fundId":"fund-denied"`, 1*time.Second); err != nil {
		t.Fatalf("error frame did not tag fund-denied: %v", err)
	}
}

// TestStreamWorkflowStatusMultiRejectsEmptyOrOversizedRequests
// covers the input-validation guards: empty fundIds → 400,
// >50 fundIds → 400. We check both at once because they share
// the same plumbing.
func TestStreamWorkflowStatusMultiRejectsEmptyOrOversizedRequests(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{getStatusFn: func(_, _ string) (*WorkflowStatus, error) {
			t.Fatalf("service should not be called when fundIds invalid")
			return nil, nil
		}},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	server := httptest.NewServer(authInjectingMiddleware("user-1", mux))
	defer server.Close()

	resp, err := server.Client().Get(server.URL + "/api/funds/workflow/stream")
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty fundIds expected 400, got %d", resp.StatusCode)
	}

	tooMany := make([]string, 0, 60)
	for i := 0; i < 60; i++ {
		tooMany = append(tooMany, "f"+strings.Repeat("x", i))
	}
	resp2, err := server.Client().Get(server.URL + "/api/funds/workflow/stream?fundIds=" + strings.Join(tooMany, ","))
	if err != nil {
		t.Fatalf("oversize: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversize fundIds expected 400, got %d", resp2.StatusCode)
	}
}

// TestStreamWorkflowStatusMultiTracksMetrics exercises the W6-2
// counter bumps end-to-end: a successful connect with one
// readable + one forbidden fund must move
//   * connections_total +1
//   * subscriptions_total +1 (only the readable fund counts)
//   * forbidden_frames_total +1 (denied fund yields one frame)
//   * active_connections net-zero across the request lifecycle
//
// We assert *deltas* rather than absolute values because earlier
// test cases in this file may still be tearing down handlers when
// this test starts; comparing absolute counters would race with
// the prior handlers' deferred decrements.
func TestStreamWorkflowStatusMultiTracksMetrics(t *testing.T) {
	before := SnapshotMuxStreamMetrics()

	getStatus := func(_, fundID string) (*WorkflowStatus, error) {
		switch fundID {
		case "fund-ok":
			return &WorkflowStatus{FundID: "fund-ok", State: "completed", CompletedAt: "2026-06-05T08:35:00Z"}, nil
		default:
			return nil, errors.New("repository: forbidden")
		}
	}
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{getStatusFn: getStatus},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	server := httptest.NewServer(authInjectingMiddleware("user-1", mux))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		server.URL+"/api/funds/workflow/stream?fundIds=fund-ok,fund-denied&interval=500ms", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	accumulator := &sseAccumulator{}
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				accumulator.Append(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	// Wait until we've seen the initial frame so we know the
	// counter bumps have already happened.
	if err := accumulator.WaitFor(`"fundId":"fund-ok"`, 1*time.Second); err != nil {
		t.Fatalf("ok frame missing: %v", err)
	}

	mid := SnapshotMuxStreamMetrics()
	if got := mid.ConnectionsTotal - before.ConnectionsTotal; got != 1 {
		t.Errorf("connections_total delta = %d, want 1", got)
	}
	if got := mid.SubscriptionsTotal - before.SubscriptionsTotal; got != 1 {
		t.Errorf("subscriptions_total delta = %d, want 1 (only fund-ok readable)", got)
	}
	if got := mid.ForbiddenFramesTotal - before.ForbiddenFramesTotal; got != 1 {
		t.Errorf("forbidden_frames_total delta = %d, want 1", got)
	}

	cancel()
	resp.Body.Close()

	// active_connections should net out to zero across the
	// request lifecycle. We don't pin the absolute value because
	// concurrent tests may shift the global counter; the delta
	// from "before" is the contract we care about.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if SnapshotMuxStreamMetrics().ActiveConnections == before.ActiveConnections {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := SnapshotMuxStreamMetrics().ActiveConnections; got != before.ActiveConnections {
		t.Errorf("active_connections did not drain: before=%d after=%d", before.ActiveConnections, got)
	}
}
