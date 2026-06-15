// admin_users_handler_test.go — table-driven tests for the read-only
// admin user console (admin_users_handler.go). The tests pin three
// invariants that drift the easiest as the handler grows:
//
//  1. The role gate (requireAdmin) is actually exercised — both the
//     "no auth" → 401 path and the "user role" → 403 path.
//  2. Stats happy path returns a fully-populated payload AND the
//     30-day signup slice is zero-filled (frontend-side rendering
//     assumes no holes in the X axis).
//  3. Detail endpoint surfaces non-nil empty slices for users with
//     zero usage_entries — the JSON shape contract the drawer relies
//     on to render skeletons without a null check on every field.
//
// We use DATA-DOG/go-sqlmock (already on go.mod) to keep the test
// hermetic; spinning up a real Postgres in CI is overkill for SQL
// shape verification.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fundai/server/internal/api"
)

func newAdminUsersEnv(t *testing.T) (*adminHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return &adminHandler{db: db}, mock, func() { _ = db.Close() }
}

func usersAdminRoleLookup(mock sqlmock.Sqlmock, userID, role string) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT role FROM users WHERE id = $1`)).
		WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow(role))
}

func adminReq(method, target, userID string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	if userID != "" {
		ctx := api.WithAuthenticatedUserID(req.Context(), userID)
		req = req.WithContext(ctx)
	}
	return req
}

func TestAdminUsersStats_Unauthenticated(t *testing.T) {
	h, _, cleanup := newAdminUsersEnv(t)
	defer cleanup()
	rr := httptest.NewRecorder()
	h.handleAdminUsersStats(rr, adminReq(http.MethodGet, "/api/admin/users/stats", ""))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminUsersStats_Forbidden(t *testing.T) {
	h, mock, cleanup := newAdminUsersEnv(t)
	defer cleanup()
	const userID = "user-not-admin"
	usersAdminRoleLookup(mock, userID, "user")
	rr := httptest.NewRecorder()
	h.handleAdminUsersStats(rr, adminReq(http.MethodGet, "/api/admin/users/stats", userID))
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

// TestAdminUsersStats_HappyPath_ZeroFills30Days pins the contract
// that the signup slice always has exactly 30 entries even when the
// DB only returns rows for two of them. Without this guarantee the
// frontend chart would render a jagged X axis whenever a couple of
// quiet days landed in the window.
func TestAdminUsersStats_HappyPath_ZeroFills30Days(t *testing.T) {
	h, mock, cleanup := newAdminUsersEnv(t)
	defer cleanup()
	const userID = "admin-user"
	usersAdminRoleLookup(mock, userID, "admin")

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM users\s+WHERE status <> 'deleted'\s*$`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(4))
	mock.ExpectQuery(`last_login_at IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(3))

	now := time.Now().UTC()
	twoDaysAgo := now.AddDate(0, 0, -2)
	mock.ExpectQuery("date_trunc\\('day', created_at\\)").
		WithArgs("30").
		WillReturnRows(sqlmock.NewRows([]string{"d", "c"}).
			AddRow(twoDaysAgo, 1).
			AddRow(now, 2))

	mock.ExpectQuery("WITH latest_active AS").
		WillReturnRows(sqlmock.NewRows([]string{"tier", "c"}).
			AddRow("free", 1).
			AddRow("pro", 2).
			AddRow("premium", 1))

	rr := httptest.NewRecorder()
	h.handleAdminUsersStats(rr, adminReq(http.MethodGet, "/api/admin/users/stats", userID))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}

	var got adminUsersStatsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TotalUsers != 4 {
		t.Errorf("TotalUsers = %d, want 4", got.TotalUsers)
	}
	if got.ActiveUsers7d != 3 {
		t.Errorf("ActiveUsers7d = %d, want 3", got.ActiveUsers7d)
	}
	if len(got.NewUsers30d) != 30 {
		t.Errorf("NewUsers30d len = %d, want 30 (zero-fill)", len(got.NewUsers30d))
	}
	if len(got.TierDistribution) != 3 {
		t.Errorf("TierDistribution len = %d, want 3", len(got.TierDistribution))
	}
	// MRR = 1*free(0) + 2*pro(2900) + 1*premium(9900) = 15700
	if got.MRRCents != 15700 {
		t.Errorf("MRRCents = %d, want 15700", got.MRRCents)
	}
	if got.AsOf == "" {
		t.Error("AsOf empty")
	}
}

func TestAdminUsersList_HappyPath(t *testing.T) {
	h, mock, cleanup := newAdminUsersEnv(t)
	defer cleanup()
	const userID = "admin-user"
	usersAdminRoleLookup(mock, userID, "admin")

	created := time.Now().UTC().Add(-72 * time.Hour)
	last := time.Now().UTC().Add(-2 * time.Hour)
	end := time.Now().UTC().Add(720 * time.Hour)

	cols := []string{
		"id", "username", "display_name", "email", "role", "status", "kyc_status",
		"created_at", "last_login_at",
		"current_tier", "tier_until",
		"lifetime_cost_cents", "lifetime_calls",
	}
	mock.ExpectQuery("FROM users u\\s+LEFT JOIN active_sub").
		WithArgs("", "", 50, 0).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("u-1", "alice", "Alice", "alice@example.com", "user", "active", "verified",
				created, last, "pro", end, int64(123456), int64(42)))

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM users u`).
		WithArgs("", "").
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(1))

	rr := httptest.NewRecorder()
	h.handleAdminUsersList(rr, adminReq(http.MethodGet, "/api/admin/users", userID))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}

	var got adminUsersListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 1 || got.Page != 1 || got.Size != 50 {
		t.Errorf("paging = (total=%d, page=%d, size=%d)", got.Total, got.Page, got.Size)
	}
	if len(got.Users) != 1 || got.Users[0].Username != "alice" {
		t.Fatalf("users = %+v", got.Users)
	}
	if got.Users[0].LifetimeLLMCostCents != 123456 || got.Users[0].LifetimeLLMCalls != 42 {
		t.Errorf("lifetime = %+v", got.Users[0])
	}
	if got.Users[0].CurrentTier != "pro" {
		t.Errorf("CurrentTier = %s, want pro", got.Users[0].CurrentTier)
	}
	if got.Users[0].LastLoginAt == nil {
		t.Error("expected LastLoginAt populated")
	}
	if got.Users[0].TierUntil == nil {
		t.Error("expected TierUntil populated")
	}
}

// TestAdminUsersList_QuerySearchPropagatesNeedle confirms the search
// box wraps the user input in %…% wildcards rather than passing it
// through as a literal — without this the frontend search would
// only return rows whose email exactly equals the box content.
func TestAdminUsersList_QuerySearchPropagatesNeedle(t *testing.T) {
	h, mock, cleanup := newAdminUsersEnv(t)
	defer cleanup()
	const userID = "admin-user"
	usersAdminRoleLookup(mock, userID, "admin")

	cols := []string{
		"id", "username", "display_name", "email", "role", "status", "kyc_status",
		"created_at", "last_login_at",
		"current_tier", "tier_until",
		"lifetime_cost_cents", "lifetime_calls",
	}
	mock.ExpectQuery("FROM users u\\s+LEFT JOIN active_sub").
		WithArgs("%alice%", "pro", 25, 0).
		WillReturnRows(sqlmock.NewRows(cols))

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM users u`).
		WithArgs("%alice%", "pro").
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(0))

	rr := httptest.NewRecorder()
	h.handleAdminUsersList(rr,
		adminReq(http.MethodGet, "/api/admin/users?q=Alice&tier=pro&page=1&size=25", userID))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestAdminUserDetail_NotFound(t *testing.T) {
	h, mock, cleanup := newAdminUsersEnv(t)
	defer cleanup()
	const userID = "admin-user"
	usersAdminRoleLookup(mock, userID, "admin")

	mock.ExpectQuery(`FROM users\s+WHERE id = \$1 AND status <> 'deleted'`).
		WithArgs("missing").
		WillReturnError(asNoRows())

	req := adminReq(http.MethodGet, "/api/admin/users/missing", userID)
	req.SetPathValue("userId", "missing")
	rr := httptest.NewRecorder()
	h.handleAdminUserDetail(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

// TestAdminUserDetail_ZeroUsageReturnsEmptyArrays guarantees the JSON
// payload never contains `null` for ByStep/ByProvider/Last30d when a
// user simply has zero usage rows. The frontend depends on
// `array.map(...)` working without a guard.
func TestAdminUserDetail_ZeroUsageReturnsEmptyArrays(t *testing.T) {
	h, mock, cleanup := newAdminUsersEnv(t)
	defer cleanup()
	const adminID = "admin-user"
	const targetID = "user-zero"
	usersAdminRoleLookup(mock, adminID, "admin")

	created := time.Now().UTC().Add(-30 * 24 * time.Hour)
	mock.ExpectQuery(`FROM users\s+WHERE id = \$1 AND status <> 'deleted'`).
		WithArgs(targetID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "username", "display_name", "email", "phone",
			"role", "status", "kyc_status", "kyc_level", "email_verified",
			"created_at", "last_login_at",
		}).AddRow(targetID, "newbie", "Newbie", "n@example.com", nil,
			"user", "active", "unverified", "tier1_basic", false,
			created, nil))

	// subscriptions: empty
	mock.ExpectQuery(`FROM subscriptions\s+WHERE user_id`).
		WithArgs(targetID).
		WillReturnRows(sqlmock.NewRows([]string{
			"plan_tier", "status", "start_date", "end_date", "payment_method", "auto_renew",
		}))

	// usage lifetime totals — both zero.
	mock.ExpectQuery(`FROM usage_entries WHERE user_id = \$1`).
		WithArgs(targetID).
		WillReturnRows(sqlmock.NewRows([]string{"calls", "cost"}).AddRow(int64(0), int64(0)))
	// by step: empty
	mock.ExpectQuery(`GROUP BY step_name`).
		WithArgs(targetID).
		WillReturnRows(sqlmock.NewRows([]string{"step", "calls", "cost"}))
	// by provider: empty
	mock.ExpectQuery(`GROUP BY model_provider`).
		WithArgs(targetID).
		WillReturnRows(sqlmock.NewRows([]string{"provider", "calls", "cost"}))
	// last 30d: empty
	mock.ExpectQuery(`AND created_at >= NOW\(\) - INTERVAL '30 days'`).
		WithArgs(targetID).
		WillReturnRows(sqlmock.NewRows([]string{"d", "calls", "cost"}))

	// wallet
	mock.ExpectQuery(`FROM wallet_accounts WHERE user_id`).
		WithArgs(targetID).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(int64(0)))

	req := adminReq(http.MethodGet, "/api/admin/users/"+targetID, adminID)
	req.SetPathValue("userId", targetID)
	rr := httptest.NewRecorder()
	h.handleAdminUserDetail(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}

	// json.Unmarshal would let nil and [] both decode as nil. Check
	// the raw bytes instead so we know the wire JSON has [] tokens.
	body := rr.Body.String()
	for _, token := range []string{`"subscriptions":[]`, `"byStep":[]`, `"byProvider":[]`, `"last30d":[]`} {
		if !strings.Contains(body, token) {
			t.Errorf("body missing %s\nbody=%s", token, body)
		}
	}
}

func TestParsePageAndSize_ClampsToBounds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		rawPage    string
		rawSize    string
		wantPage   int
		wantSize   int
	}{
		{"defaults_when_blank", "", "", 1, 50},
		{"non_numeric_falls_back", "abc", "xyz", 1, 50},
		{"page_zero_clamps_to_one", "0", "10", 1, 10},
		{"negative_page_clamps_to_one", "-5", "20", 1, 20},
		{"size_negative_clamps_to_one", "1", "-1", 1, 1},
		{"size_huge_caps_at_200", "1", "9999", 1, 200},
		{"valid_passes_through", "3", "75", 3, 75},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, size := parsePageAndSize(tc.rawPage, tc.rawSize)
			if page != tc.wantPage || size != tc.wantSize {
				t.Errorf("parsePageAndSize(%q,%q) = (%d,%d), want (%d,%d)",
					tc.rawPage, tc.rawSize, page, size, tc.wantPage, tc.wantSize)
			}
		})
	}
}

// asNoRows lets us return sql.ErrNoRows to QueryRow without taking
// a direct database/sql import in the test helpers.
func asNoRows() error { return errSentinelNoRows }

var errSentinelNoRows = sentinelNoRows{}

type sentinelNoRows struct{}

func (sentinelNoRows) Error() string { return "sql: no rows in result set" }

// Is is implemented so errors.Is(err, sql.ErrNoRows) returns true
// — sqlmock allows arbitrary errors but the handler calls
// errors.Is(err, sql.ErrNoRows) to map to 404, so we have to satisfy
// that contract on the test side.
func (sentinelNoRows) Is(target error) bool {
	return target != nil && target.Error() == "sql: no rows in result set"
}
