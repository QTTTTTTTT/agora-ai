package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/memreembed"
)

// TestMemReembedHandlerStatusReturnsLiveStats exercises the happy path:
// a queue with two pending requests must round-trip through the JSON
// shape with `enabled=true` and matching counters.
func TestMemReembedHandlerStatusReturnsLiveStats(t *testing.T) {
	queue := memreembed.NewQueue(memreembed.DefaultConfig())
	if err := queue.Enqueue(memreembed.Request{MemoryID: "mem-1", Content: "alpha"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := queue.Enqueue(memreembed.Request{MemoryID: "mem-2", Content: "beta"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	h := newMemReembedHandler(&Services{MemReembedQueue: queue})
	if h == nil {
		t.Fatal("expected non-nil handler when svc is populated")
	}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/memreembed/status", nil)
	// Stamp a super-admin context so requireSuperAdmin lets the
	// request through. Same trick the db-pool handler test uses
	// to bypass the auth middleware in unit-mode.
	ctx := api.WithAuthenticatedUserRole(req.Context(), userRoleSuperAdmin)
	ctx = api.WithAuthenticatedUserID(ctx, "admin-test-id")
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}

	var status memReembedStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, rr.Body.String())
	}
	if !status.Enabled {
		t.Error("expected Enabled=true when queue is non-nil")
	}
	if status.Pending != 2 {
		t.Errorf("Pending: got %d, want 2", status.Pending)
	}
	if status.EmbeddedTotal != 0 || status.RetriedTotal != 0 || status.DeadLetterTotal != 0 {
		t.Errorf("totals should be 0 on a fresh queue, got embedded=%d retried=%d dead=%d",
			status.EmbeddedTotal, status.RetriedTotal, status.DeadLetterTotal)
	}
	// LastErrorUnix omitted when there has never been an error.
	if status.LastErrorUnix != 0 {
		t.Errorf("LastErrorUnix on fresh queue should be 0, got %d", status.LastErrorUnix)
	}
	if status.ObservedAt.IsZero() {
		t.Error("expected non-zero ObservedAt")
	}
}

// TestMemReembedHandlerStatusReportsDisabledWhenQueueNil pins the
// "feature off" contract: the route registers and answers 200 with
// `enabled=false` so the Admin UI panel can render a disabled state
// without special-casing a 404.
func TestMemReembedHandlerStatusReportsDisabledWhenQueueNil(t *testing.T) {
	h := newMemReembedHandler(&Services{MemReembedQueue: nil})
	if h == nil {
		t.Fatal("handler must register even with nil queue")
	}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/memreembed/status", nil)
	ctx := api.WithAuthenticatedUserRole(req.Context(), userRoleSuperAdmin)
	ctx = api.WithAuthenticatedUserID(ctx, "admin-test-id")
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	var status memReembedStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.Enabled {
		t.Error("expected Enabled=false with nil queue")
	}
	// Counters MUST be zero when disabled (otherwise the panel
	// shows misleading data).
	if status.Pending != 0 || status.EmbeddedTotal != 0 {
		t.Errorf("disabled queue should report zeroed counters, got %+v", status)
	}
}

// TestMemReembedHandlerStatusRejectsNonAdmin verifies the super-admin
// gate returns 401 / 403 for unauthenticated or non-admin callers —
// queue depth is NOT a publicly-readable detail.
func TestMemReembedHandlerStatusRejectsNonAdmin(t *testing.T) {
	queue := memreembed.NewQueue(memreembed.DefaultConfig())
	h := newMemReembedHandler(&Services{MemReembedQueue: queue})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	cases := []struct {
		name string
		ctx  context.Context
		want int
	}{
		{name: "unauthenticated", ctx: context.Background(), want: http.StatusUnauthorized},
		{
			name: "regular user",
			ctx: api.WithAuthenticatedUserRole(
				api.WithAuthenticatedUserID(context.Background(), "u1"),
				userRoleUser,
			),
			want: http.StatusForbidden,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/admin/memreembed/status", nil).
				WithContext(tc.ctx)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("expected %d, got %d (body=%s)", tc.want, rr.Code, rr.Body.String())
			}
		})
	}
}

// TestMemReembedHandlerNilSafe matches the codebase convention:
// constructor returns nil when its core dep (Services) is missing
// so the router skips route registration cleanly.
func TestMemReembedHandlerNilSafe(t *testing.T) {
	if h := newMemReembedHandler(nil); h != nil {
		t.Errorf("expected nil handler when svc is nil")
	}
}
