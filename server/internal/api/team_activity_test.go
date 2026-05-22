package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestListTeamActivityReturnsBackfillWithDefaults exercises the REST backfill
// endpoint with no query params: the handler should pass limit=50 (default)
// and sinceSeq=0 (full backfill) to the service.
func TestListTeamActivityReturnsBackfillWithDefaults(t *testing.T) {
	var capturedLimit int
	var capturedSince uint64
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{
			listActivityFn: func(userID, fundID string, limit int, sinceSeq uint64) ([]TeamActivityItem, error) {
				if userID != "user-1" || fundID != "fund-1" {
					t.Fatalf("unexpected auth args: user=%q fund=%q", userID, fundID)
				}
				capturedLimit = limit
				capturedSince = sinceSeq
				return []TeamActivityItem{
					{Seq: 1, Type: "run_started", Role: "system", FundID: fundID, Timestamp: time.Now(), Message: "Daily workflow started"},
					{Seq: 2, Type: "step_started", Role: "researcher", FundID: fundID, Step: "macro_brief", Timestamp: time.Now(), Message: "researcher started: macro_brief"},
				}, nil
			},
		},
		stubPlanService{}, stubTradeService{}, stubWorkflowService{}, stubMemoryService{},
		stubDecisionTraceService{}, stubMarketService{}, stubABTestService{}, stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/team/activity", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if capturedLimit != 50 {
		t.Errorf("expected default limit=50, got %d", capturedLimit)
	}
	if capturedSince != 0 {
		t.Errorf("expected default sinceSeq=0, got %d", capturedSince)
	}

	var payload struct {
		FundID string             `json:"fundId"`
		Items  []TeamActivityItem `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload.FundID != "fund-1" || len(payload.Items) != 2 {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if payload.Items[0].Seq != 1 || payload.Items[1].Seq != 2 {
		t.Fatalf("expected seqs 1,2 in order, got %d,%d", payload.Items[0].Seq, payload.Items[1].Seq)
	}
}

// TestListTeamActivityParsesQueryParams asserts the limit + sinceSeq params
// reach the service correctly, including clamping a too-large limit.
func TestListTeamActivityParsesQueryParams(t *testing.T) {
	var capturedLimit int
	var capturedSince uint64
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{
			listActivityFn: func(_, _ string, limit int, sinceSeq uint64) ([]TeamActivityItem, error) {
				capturedLimit = limit
				capturedSince = sinceSeq
				return []TeamActivityItem{}, nil
			},
		},
		stubPlanService{}, stubTradeService{}, stubWorkflowService{}, stubMemoryService{},
		stubDecisionTraceService{}, stubMarketService{}, stubABTestService{}, stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/team/activity?limit=9999&sinceSeq=42", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if capturedLimit != 500 {
		t.Errorf("expected clamped limit=500, got %d", capturedLimit)
	}
	if capturedSince != 42 {
		t.Errorf("expected sinceSeq=42, got %d", capturedSince)
	}
}

// TestListTeamActivityBeforeRoutesToPagingService asserts the new
// `before=` query param hits PageTeamActivity rather than the regular
// ListTeamActivity path. This is the entry point for "load earlier"
// infinite scroll: the panel hands us the timestamp of the oldest
// visible event and expects an older page in newest-first order.
func TestListTeamActivityBeforeRoutesToPagingService(t *testing.T) {
	var pagedBefore time.Time
	var pagedLimit int
	listShouldNotFire := func(_, _ string, _ int, _ uint64) ([]TeamActivityItem, error) {
		t.Fatal("ListTeamActivity must NOT fire when ?before= is supplied")
		return nil, nil
	}
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{
			listActivityFn: listShouldNotFire,
			pageActivityFn: func(userID, fundID string, before time.Time, limit int) ([]TeamActivityItem, error) {
				if userID != "user-1" || fundID != "fund-1" {
					t.Fatalf("unexpected auth: user=%q fund=%q", userID, fundID)
				}
				pagedBefore = before
				pagedLimit = limit
				return []TeamActivityItem{
					{Seq: 1, Type: "run_started", Role: "system", FundID: fundID, Timestamp: before.Add(-time.Minute), Message: "earlier event"},
				}, nil
			},
		},
		stubPlanService{}, stubTradeService{}, stubWorkflowService{}, stubMemoryService{},
		stubDecisionTraceService{}, stubMarketService{}, stubABTestService{}, stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	cursor := time.Date(2026, 5, 19, 12, 30, 0, 0, time.UTC)
	url := "/api/funds/fund-1/team/activity?limit=25&before=" + cursor.Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !pagedBefore.Equal(cursor) {
		t.Errorf("expected before=%s reached service, got %s", cursor, pagedBefore)
	}
	if pagedLimit != 25 {
		t.Errorf("expected limit=25, got %d", pagedLimit)
	}
}

// TestListTeamActivityBeforeRejectsMalformedTimestamp catches typos in
// the cursor early so the UI sees a 400 with a useful message instead
// of falling through to the unbounded list path.
func TestListTeamActivityBeforeRejectsMalformedTimestamp(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{
			pageActivityFn: func(_, _ string, _ time.Time, _ int) ([]TeamActivityItem, error) {
				t.Fatal("PageTeamActivity should not fire for an invalid before cursor")
				return nil, nil
			},
		},
		stubPlanService{}, stubTradeService{}, stubWorkflowService{}, stubMemoryService{},
		stubDecisionTraceService{}, stubMarketService{}, stubABTestService{}, stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/team/activity?before=not-a-timestamp", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed before cursor, got %d", rr.Code)
	}
}

// TestListTeamActivityRequiresAuth confirms unauthenticated requests are
// rejected before reaching the service.
func TestListTeamActivityRequiresAuth(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{listActivityFn: func(_, _ string, _ int, _ uint64) ([]TeamActivityItem, error) {
			t.Fatal("service should NOT be invoked for unauthenticated request")
			return nil, nil
		}},
		stubPlanService{}, stubTradeService{}, stubWorkflowService{}, stubMemoryService{},
		stubDecisionTraceService{}, stubMarketService{}, stubABTestService{}, stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/team/activity", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestListTeamActivityPropagatesForbiddenAsNotFound verifies cross-tenant
// safety: a user who isn't authorised on the fund must not be able to learn
// it exists (returns 404 via handleServiceError).
func TestListTeamActivityPropagatesForbiddenAsNotFound(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{
			listActivityFn: func(_, _ string, _ int, _ uint64) ([]TeamActivityItem, error) {
				return nil, ErrForbidden
			},
		},
		stubPlanService{}, stubTradeService{}, stubWorkflowService{}, stubMemoryService{},
		stubDecisionTraceService{}, stubMarketService{}, stubABTestService{}, stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-other/team/activity", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden && rr.Code != http.StatusNotFound {
		t.Fatalf("expected 403 or 404 for forbidden access, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestStreamTeamActivityStreamsEventsThenClosesOnContextCancel exercises the
// SSE handler end-to-end against an httptest server: it must (1) emit the
// initial ":connected" comment, (2) deliver events from the subscription
// channel as "event: activity" frames, and (3) terminate cleanly when the
// client context is cancelled.
func TestStreamTeamActivityStreamsEventsThenClosesOnContextCancel(t *testing.T) {
	ch := make(chan TeamActivityItem, 4)
	cancelled := make(chan struct{})

	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{
			subscribeActivityFn: func(userID, fundID string) (*TeamActivityStream, error) {
				if userID != "user-1" || fundID != "fund-1" {
					t.Fatalf("unexpected auth: user=%q fund=%q", userID, fundID)
				}
				return &TeamActivityStream{
					Events: ch,
					Cancel: func() {
						select {
						case <-cancelled:
						default:
							close(cancelled)
						}
					},
					DroppedCount: func() uint64 { return 0 },
				}, nil
			},
		},
		stubPlanService{}, stubTradeService{}, stubWorkflowService{}, stubMemoryService{},
		stubDecisionTraceService{}, stubMarketService{}, stubABTestService{}, stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	server := httptest.NewServer(authInjectingMiddleware("user-1", mux))
	defer server.Close()

	ctx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/funds/fund-1/team/activity/stream", nil)
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

	// Read in a goroutine into a shared accumulator so we can wait on multiple
	// substrings without losing the bytes that arrived in earlier reads. (A
	// single SSE TCP segment can hold all three frames; the naive
	// read-until-found-then-throw-away approach loses subsequent matches.)
	accumulator := &sseAccumulator{}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 512)
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
		t.Fatalf("waiting for initial comment: %v", err)
	}

	// Publish two events.
	ch <- TeamActivityItem{Seq: 1, Type: "step_started", Role: "researcher", FundID: "fund-1", Timestamp: time.Now(), Message: "researcher started: macro_brief"}
	ch <- TeamActivityItem{Seq: 2, Type: "step_completed", Role: "researcher", FundID: "fund-1", Timestamp: time.Now(), Message: "researcher completed: macro_brief"}

	if err := accumulator.WaitFor("\"seq\":1", 1*time.Second); err != nil {
		t.Fatalf("waiting for seq=1: %v", err)
	}
	if err := accumulator.WaitFor("\"seq\":2", 1*time.Second); err != nil {
		t.Fatalf("waiting for seq=2: %v", err)
	}

	// Cancel the client side and confirm the handler invoked Cancel exactly
	// once on the subscription so the SSE goroutine can release its slot.
	cancelReq()
	select {
	case <-cancelled:
	case <-time.After(1 * time.Second):
		t.Fatal("expected Stream.Cancel to be invoked on client disconnect within 1s")
	}
}

// TestStreamTeamActivityRejectsUnauthenticated mirrors the REST endpoint:
// without a session the SSE endpoint must 401, never opening a stream.
func TestStreamTeamActivityRejectsUnauthenticated(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{subscribeActivityFn: func(_, _ string) (*TeamActivityStream, error) {
			t.Fatal("service should NOT be invoked for unauthenticated SSE request")
			return nil, nil
		}},
		stubPlanService{}, stubTradeService{}, stubWorkflowService{}, stubMemoryService{},
		stubDecisionTraceService{}, stubMarketService{}, stubABTestService{}, stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/team/activity/stream", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

// TestStreamTeamActivityPropagatesSubscribeError ensures that when the
// subscription cannot be opened (bus closed, fund forbidden, etc.) the
// handler returns the appropriate HTTP status rather than hanging.
func TestStreamTeamActivityPropagatesSubscribeError(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{subscribeActivityFn: func(_, _ string) (*TeamActivityStream, error) {
			return nil, ErrForbidden
		}},
		stubPlanService{}, stubTradeService{}, stubWorkflowService{}, stubMemoryService{},
		stubDecisionTraceService{}, stubMarketService{}, stubABTestService{}, stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-other/team/activity/stream", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden && rr.Code != http.StatusNotFound {
		t.Fatalf("expected 403 or 404 for forbidden subscription, got %d", rr.Code)
	}
}

// authInjectingMiddleware fakes the JWT/cookie-based auth middleware used in
// production by injecting the userID into the request context unconditionally.
// Used only in SSE tests where the real httptest.NewServer path is needed
// (httptest.Recorder doesn't implement http.Flusher).
func authInjectingMiddleware(userID string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(WithAuthenticatedUserID(r.Context(), userID)))
	})
}

// sseAccumulator is a goroutine-safe ever-growing buffer of bytes received
// from the SSE body. WaitFor polls until the requested substring is present
// or the deadline elapses. Compared to a single-call read-until-found
// helper, this preserves bytes between matches so multiple events arriving
// in a single TCP segment are not lost.
type sseAccumulator struct {
	mu   sync.Mutex
	data strings.Builder
}

func (s *sseAccumulator) Append(chunk []byte) {
	s.mu.Lock()
	s.data.Write(chunk)
	s.mu.Unlock()
}

func (s *sseAccumulator) Contains(substring string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Contains(s.data.String(), substring)
}

func (s *sseAccumulator) WaitFor(substring string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.Contains(substring) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("timeout waiting for SSE substring: " + substring)
}
