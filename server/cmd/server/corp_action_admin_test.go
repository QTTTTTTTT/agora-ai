package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/repository"
)

// authedReq is a httptest helper that builds a request already
// carrying the (user_id, role) context the requireSuperAdmin
// middleware expects. We pass "super_admin" for the happy path and
// "user" / "" for the rejection paths.
func authedReq(t *testing.T, method, path string, body []byte, role string) *http.Request {
	t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	ctx := api.WithAuthenticatedUserID(r.Context(), "admin-user")
	ctx = api.WithAuthenticatedUserRole(ctx, role)
	return r.WithContext(ctx)
}

// TestValidateAndBuildCorpActionRow exhaustively covers the 400-
// path validation. Each row exercises one rejection so a regression
// localizes to a single failing subtest.
func TestValidateAndBuildCorpActionRow(t *testing.T) {
	good := corpActionApplyRequest{
		InstrumentKey: "NASDAQ:NVDA",
		ExDate:        "2024-06-10",
		ActionType:    "split",
		SplitRatio:    10.0,
		CashDividend:  0,
		Source:        "manual",
	}
	if _, err := validateAndBuildCorpActionRow(good); err != nil {
		t.Fatalf("happy path rejected: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*corpActionApplyRequest)
		want string
	}{
		{"empty instrument", func(r *corpActionApplyRequest) { r.InstrumentKey = "" }, "instrument_key"},
		{"bad action_type", func(r *corpActionApplyRequest) { r.ActionType = "rights_issue" }, "action_type"},
		{"bad source", func(r *corpActionApplyRequest) { r.Source = "guess" }, "source"},
		{"zero ratio", func(r *corpActionApplyRequest) { r.SplitRatio = 0 }, "split_ratio"},
		{"negative ratio", func(r *corpActionApplyRequest) { r.SplitRatio = -1 }, "split_ratio"},
		{"negative dividend", func(r *corpActionApplyRequest) { r.CashDividend = -0.5 }, "cash_dividend"},
		{"bad ex_date", func(r *corpActionApplyRequest) { r.ExDate = "2024/06/10" }, "ex_date"},
		{"bad announced_at", func(r *corpActionApplyRequest) { r.AnnouncedAt = "yesterday" }, "announced_at"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			req := good
			tc.mut(&req)
			_, err := validateAndBuildCorpActionRow(req)
			if err == nil {
				t.Fatalf("expected error mentioning %q, got nil", tc.want)
			}
			if !regexp.MustCompile("(?i)" + regexp.QuoteMeta(tc.want)).MatchString(err.Error()) {
				t.Errorf("error %q should mention %q", err.Error(), tc.want)
			}
		})
	}
}

// TestHandleApplyCorpAction_Forbidden makes sure non-super-admin
// callers can't move money around via this endpoint. The handler
// must return 403 BEFORE touching any DB state (we assert on the
// sqlmock that no expectations fired).
func TestHandleApplyCorpAction_Forbidden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	h := &adminHandler{db: db, corpActionRepo: repository.NewCorpActionRepo(db)}
	body, _ := json.Marshal(corpActionApplyRequest{
		InstrumentKey: "NASDAQ:NVDA", ExDate: "2024-06-10", ActionType: "split",
		SplitRatio: 10.0, Source: "manual",
	})
	r := authedReq(t, http.MethodPost, "/api/admin/corp-actions", body, "user")
	w := httptest.NewRecorder()
	h.handleApplyCorpAction(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB should not be touched on 403: %v", err)
	}
}

// TestHandleApplyCorpAction_BadJSON pins the 400 path for malformed
// payload — a missing ratio or unknown action_type must come back
// before any DB interaction.
func TestHandleApplyCorpAction_BadJSON(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	h := &adminHandler{db: db, corpActionRepo: repository.NewCorpActionRepo(db)}
	r := authedReq(t, http.MethodPost, "/api/admin/corp-actions",
		[]byte(`{"instrument_key":"NASDAQ:NVDA","ex_date":"2024-06-10","action_type":"split","split_ratio":0,"source":"manual"}`),
		adminRoleSuperAdmin)
	w := httptest.NewRecorder()
	h.handleApplyCorpAction(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB should not be touched on 400: %v", err)
	}
}

// TestHandleApplyCorpAction_HappyPath_SingleFund walks the entire
// happy path with an explicit fund_ids list (the simpler control
// path that doesn't require fan-out). Asserts:
//
//   - corporate_actions is upserted (RETURNING id)
//   - applier transaction runs (begin → idempotency probe → lock
//     → updates → insert application → commit)
//   - response JSON carries the receipt with already_applied=false
func TestHandleApplyCorpAction_HappyPath_SingleFund(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	h := &adminHandler{db: db, corpActionRepo: repository.NewCorpActionRepo(db)}

	// Step 1: upsert returns the newly-minted event id.
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO corporate_actions")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("evt-99"))

	// Step 2: applier transaction.
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM corp_action_applications")).
		WillReturnError(errNoRowsForApplyTest)
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WillReturnRows(sqlmock.NewRows([]string{"quantity", "cost_price", "current_price", "market_value", "unrealized_pnl"}).
			AddRow(10.0, 100.0, 12.0, 120.0, -880.0))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE holding_positions")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE position_lots")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO corp_action_applications")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	body, _ := json.Marshal(corpActionApplyRequest{
		InstrumentKey: "NASDAQ:NVDA",
		ExDate:        "2024-06-10",
		ActionType:    "split",
		SplitRatio:    10.0,
		Source:        "manual",
		FundIDs:       []string{"fund-A"},
	})
	r := authedReq(t, http.MethodPost, "/api/admin/corp-actions", body, adminRoleSuperAdmin)
	w := httptest.NewRecorder()
	h.handleApplyCorpAction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp corpActionApplyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp.EventID != "evt-99" {
		t.Errorf("EventID = %q, want evt-99", resp.EventID)
	}
	if resp.FanOut != 1 {
		t.Errorf("FanOut = %d, want 1", resp.FanOut)
	}
	if len(resp.Applications) != 1 {
		t.Fatalf("Applications len = %d, want 1", len(resp.Applications))
	}
	app := resp.Applications[0]
	if app.FundID != "fund-A" {
		t.Errorf("FundID = %q, want fund-A", app.FundID)
	}
	if app.PostQuantity != 100.0 {
		t.Errorf("PostQuantity = %v, want 100 (10 * 10:1 split)", app.PostQuantity)
	}
	if app.PostCostPrice != 10.0 {
		t.Errorf("PostCostPrice = %v, want 10 (100 / 10)", app.PostCostPrice)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestHandleApplyCorpAction_PositionMissing_FoldsToSkip exercises
// the fan-out path where some funds in the list don't actually hold
// the instrument. The applier returns ErrPositionMissing for them
// and the handler must surface that as a `skipped` entry instead of
// erroring the whole batch.
func TestHandleApplyCorpAction_PositionMissing_FoldsToSkip(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	h := &adminHandler{db: db, corpActionRepo: repository.NewCorpActionRepo(db)}

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO corporate_actions")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("evt-skip"))

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM corp_action_applications")).
		WillReturnError(errNoRowsForApplyTest)
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WillReturnError(errNoRowsForApplyTest)
	mock.ExpectRollback()

	body, _ := json.Marshal(corpActionApplyRequest{
		InstrumentKey: "NASDAQ:NVDA",
		ExDate:        "2024-06-10",
		ActionType:    "split",
		SplitRatio:    10.0,
		Source:        "manual",
		FundIDs:       []string{"fund-no-hold"},
	})
	r := authedReq(t, http.MethodPost, "/api/admin/corp-actions", body, adminRoleSuperAdmin)
	w := httptest.NewRecorder()
	h.handleApplyCorpAction(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp corpActionApplyResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Applications) != 0 {
		t.Errorf("Applications len = %d, want 0", len(resp.Applications))
	}
	if len(resp.Skipped) != 1 || resp.Skipped[0].Reason != "position_missing" {
		t.Errorf("Skipped = %+v, want one entry with reason=position_missing", resp.Skipped)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// errNoRowsForApplyTest is sql.ErrNoRows. Splitting it out as a
// named sentinel keeps test bodies short and signals intent.
var errNoRowsForApplyTest = sql.ErrNoRows
