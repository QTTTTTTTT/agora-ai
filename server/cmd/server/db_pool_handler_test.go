package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/api"
)

// TestDBPoolHandlerStatusReturnsLiveStats opens a real (in-memory)
// sql.DB-style stub via DATA-DOG/go-sqlmock to avoid needing Postgres
// in-process. The test asserts that the handler emits the expected
// JSON shape with computed utilization + wait-avg fields.
func TestDBPoolHandlerStatusReturnsLiveStats(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(2 * time.Minute)

	cfg := &Config{DBMaxOpenConns: 20, DBMaxIdleConns: 5, DBConnMaxLife: 2 * time.Minute}
	h := newDBPoolHandler(&Services{DB: db}, cfg)
	if h == nil {
		t.Fatal("expected non-nil handler when svc.DB and cfg are populated")
	}

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/db-pool/status", nil)
	// Stamp a super-admin context so requireSuperAdmin lets the
	// request through. We bypass the live auth middleware here
	// because the handler is the unit under test.
	ctx := api.WithAuthenticatedUserRole(req.Context(), userRoleSuperAdmin)
	ctx = api.WithAuthenticatedUserID(ctx, "admin-test-id")
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}

	var status dbPoolStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, rr.Body.String())
	}

	if status.MaxOpenConnections != 20 {
		t.Errorf("expected MaxOpenConnections=20, got %d", status.MaxOpenConnections)
	}
	if status.MaxIdleConnsConfig != 5 {
		t.Errorf("expected MaxIdleConnsConfig=5, got %d", status.MaxIdleConnsConfig)
	}
	if status.ConnMaxLifetimeHuman != (2 * time.Minute).String() {
		t.Errorf("expected ConnMaxLifetimeHuman=%q, got %q", (2 * time.Minute).String(), status.ConnMaxLifetimeHuman)
	}
	// With no waits + no traffic UtilizationPct should be 0%
	// (InUse=0, MaxOpen=20). Allow ε for fp noise.
	if status.UtilizationPct < -1 || status.UtilizationPct > 1 {
		t.Errorf("expected UtilizationPct≈0 with no in-use conns, got %.4f", status.UtilizationPct)
	}
	// WaitAvgSeconds is -1 when WaitCount=0 (sentinel for "undefined").
	if status.WaitAvgSeconds != -1 {
		t.Errorf("expected WaitAvgSeconds=-1 with no waits, got %v", status.WaitAvgSeconds)
	}
	if status.ObservedAt.IsZero() {
		t.Error("expected non-zero ObservedAt")
	}
}

// TestDBPoolHandlerStatusRejectsNonAdmin verifies the super-admin gate
// returns 403 / 401 for unauthenticated or non-admin callers — i.e.,
// pool stats are NOT a publicly-readable secret leak.
func TestDBPoolHandlerStatusRejectsNonAdmin(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock open: %v", err)
	}
	defer db.Close()

	cfg := &Config{DBMaxOpenConns: 20, DBMaxIdleConns: 5, DBConnMaxLife: 2 * time.Minute}
	h := newDBPoolHandler(&Services{DB: db}, cfg)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	cases := []struct {
		name string
		ctx  context.Context
		want int
	}{
		{name: "unauthenticated", ctx: context.Background(), want: http.StatusUnauthorized},
		{name: "regular user", ctx: api.WithAuthenticatedUserRole(api.WithAuthenticatedUserID(context.Background(), "u1"), userRoleUser), want: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/admin/db-pool/status", nil).WithContext(tc.ctx)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("expected %d, got %d (body=%s)", tc.want, rr.Code, rr.Body.String())
			}
		})
	}
}

// TestDBPoolHandlerNilSafe asserts that newDBPoolHandler returns nil
// when its dependencies are missing — matching the rest of the
// codebase's "feature unavailable rather than panic" pattern. The
// router uses this to skip route registration cleanly.
func TestDBPoolHandlerNilSafe(t *testing.T) {
	cfg := &Config{}
	if h := newDBPoolHandler(nil, cfg); h != nil {
		t.Errorf("expected nil handler when svc is nil")
	}
	if h := newDBPoolHandler(&Services{}, cfg); h != nil {
		t.Errorf("expected nil handler when svc.DB is nil")
	}
	if h := newDBPoolHandler(&Services{DB: nil}, nil); h != nil {
		t.Errorf("expected nil handler when cfg is nil")
	}
}
