// User-facing broker_link handler tests (P1-6).
//
// Covers create/list/revoke happy paths plus the auth/path
// guards. Uses sqlmock so we don't depend on a live DB.

package main

import (
	"bytes"
	"database/sql"
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

func newBrokerLinkHandlerEnv(t *testing.T) (*brokerLinkHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock := newMockDB(t)
	cleanup := func() { _ = db.Close() }
	h := newBrokerLinkHandler(&Services{DB: db})
	if h == nil {
		t.Fatal("newBrokerLinkHandler returned nil")
	}
	return h, mock, cleanup
}

// expectFundOwnership programs the SELECT funds → SELECT
// fund_companies pair that authorizeFundAccess fires.
func expectFundOwnershipBL(mock sqlmock.Sqlmock, fundID, companyID, userID string) {
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "company_id", "name", "description", "trading_mode",
			"initial_capital", "current_capital", "total_assets", "nav", "status",
			"config", "created_at", "updated_at",
		}).AddRow(fundID, companyID, "Fund", "", "live",
			100000.0, 100000.0, 100000.0, 1.0, "active",
			[]byte("{}"), now, now,
		))
	mock.ExpectQuery("FROM fund_companies").
		WithArgs(companyID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "owner_user_id", "name", "description", "created_at", "updated_at",
		}).AddRow(companyID, userID, "Co", "", now, now))
}

// expectAuditMutationGenesis primes a single hash-chained
// admin_change_log INSERT (no prior row → genesis hash).
func expectAuditMutationGenesisBL(mock sqlmock.Sqlmock, actor, action, targetType, targetID string) {
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT row_hash
		FROM admin_change_log
		WHERE row_hash IS NOT NULL
		ORDER BY created_at DESC, id DESC
		LIMIT 1`)).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("INSERT INTO admin_change_log").
		WithArgs(
			sqlmock.AnyArg(),
			actor, action, targetType, targetID,
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			nil,
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
		).WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestBrokerLink_Create_Unauthenticated(t *testing.T) {
	h, _, cleanup := newBrokerLinkHandlerEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodPost, "/api/funds/f1/broker-links",
		strings.NewReader(`{"brokerId":"ibkr","accountId":"U1234567"}`))
	req.SetPathValue("fundId", "f1")
	rr := httptest.NewRecorder()
	h.handleCreate(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestBrokerLink_Create_RejectsUnknownBroker(t *testing.T) {
	h, _, cleanup := newBrokerLinkHandlerEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodPost, "/api/funds/f1/broker-links",
		strings.NewReader(`{"brokerId":"madeup","accountId":"X"}`))
	req.SetPathValue("fundId", "f1")
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), "11111111-1111-1111-1111-111111111111"))
	rr := httptest.NewRecorder()
	h.handleCreate(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "invalid_broker") {
		t.Errorf("body = %s", rr.Body.String())
	}
}

func TestBrokerLink_Create_HappyPath(t *testing.T) {
	h, mock, cleanup := newBrokerLinkHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"

	expectFundOwnershipBL(mock, fundID, companyID, userID)
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO broker_links`)).
		WithArgs(fundID, userID, "ibkr", "U1234567", []byte(nil), []byte(`{}`)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("link-1"))
	expectAuditMutationGenesisBL(mock, userID, "broker_link.request", "broker_link", "link-1")

	body := `{"brokerId":"ibkr","accountId":"U1234567"}`
	req := httptest.NewRequest(http.MethodPost, "/api/funds/"+fundID+"/broker-links", strings.NewReader(body))
	req.ContentLength = int64(len(body))
	req.SetPathValue("fundId", fundID)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()
	h.handleCreate(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(bytes.NewReader(rr.Body.Bytes())).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["link_id"] != "link-1" {
		t.Errorf("link_id = %q, want link-1", resp["link_id"])
	}
	if resp["status"] != "pending" {
		t.Errorf("status = %q, want pending", resp["status"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestBrokerLink_List_RedactsAccountID(t *testing.T) {
	h, mock, cleanup := newBrokerLinkHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	now := time.Now()

	expectFundOwnershipBL(mock, fundID, companyID, userID)
	mock.ExpectQuery(regexp.QuoteMeta(`FROM broker_links`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows(brokerLinkColumns).AddRow(
			"link-1", fundID, userID, "ibkr", "U1234567", "active",
			"approver-1", now, []byte("ct"), []byte(`{}`),
			now, now,
		))

	req := httptest.NewRequest(http.MethodGet, "/api/funds/"+fundID+"/broker-links", nil)
	req.SetPathValue("fundId", fundID)
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()
	h.handleList(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	// We expect "•••4567" in the response — redact retains last 4 chars.
	if !strings.Contains(rr.Body.String(), "4567") {
		t.Errorf("body missing last4 marker: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "U1234567") {
		t.Errorf("body leaked full account id: %s", rr.Body.String())
	}
}

func TestBrokerLink_Revoke_RejectsCrossUser(t *testing.T) {
	h, mock, cleanup := newBrokerLinkHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const otherUser = "44444444-4444-4444-4444-444444444444"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	now := time.Now()

	expectFundOwnershipBL(mock, fundID, companyID, userID)
	// Link belongs to a DIFFERENT user — handler must 403 even
	// though the requester owns the fund.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM broker_links`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows(brokerLinkColumns).AddRow(
			"link-1", fundID, otherUser, "ibkr", "U1234567", "active",
			nil, nil, nil, []byte(`{}`), now, now,
		))

	req := httptest.NewRequest(http.MethodPost, "/api/funds/"+fundID+"/broker-links/link-1/revoke", nil)
	req.SetPathValue("fundId", fundID)
	req.SetPathValue("linkId", "link-1")
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()
	h.handleRevoke(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestBrokerLink_Revoke_HappyPath(t *testing.T) {
	h, mock, cleanup := newBrokerLinkHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	now := time.Now()

	expectFundOwnershipBL(mock, fundID, companyID, userID)
	mock.ExpectQuery(regexp.QuoteMeta(`FROM broker_links`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows(brokerLinkColumns).AddRow(
			"link-1", fundID, userID, "ibkr", "U1234567", "active",
			nil, nil, nil, []byte(`{}`), now, now,
		))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE broker_links`)).
		WithArgs("link-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectAuditMutationGenesisBL(mock, userID, "broker_link.revoke", "broker_link", "link-1")

	req := httptest.NewRequest(http.MethodPost, "/api/funds/"+fundID+"/broker-links/link-1/revoke", nil)
	req.SetPathValue("fundId", fundID)
	req.SetPathValue("linkId", "link-1")
	req = req.WithContext(api.WithAuthenticatedUserID(req.Context(), userID))
	rr := httptest.NewRecorder()
	h.handleRevoke(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestRedactAccountID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"U1234567", "••••4567"},   // 8 chars: 4 dots + last 4
		{"ABC", "•••"},             // ≤4 chars: full mask
		{"", ""},                   // empty stays empty
		{"  PADDED  ", "••DDED"},   // trims to "PADDED" (6) → 2 dots + last 4
		{"AB123", "•B123"},          // 5 chars → 1 dot + last 4
	}
	for i, c := range cases {
		got := redactAccountID(c.in)
		if got != c.want {
			t.Errorf("case %d: redactAccountID(%q) = %q, want %q", i, c.in, got, c.want)
		}
	}
}

// brokerLinkColumns mirrors the SELECT in scanBrokerLink — we
// duplicate it here (vs importing) to avoid a tighter coupling
// between the cmd/server and internal/repository tests.
var brokerLinkColumns = []string{
	"id", "fund_id", "user_id", "broker_id", "account_id", "status",
	"approved_by", "approved_at", "credentials_encrypted", "metadata",
	"created_at", "updated_at",
}
