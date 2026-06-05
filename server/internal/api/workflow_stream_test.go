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
