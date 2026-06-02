package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/factorexposure"
)

func newAdminFactorExposureEnv(t *testing.T) (*adminHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	h := &adminHandler{
		db:                 db,
		metrics:            newServerMetrics(),
		factorExposureRepo: factorexposure.NewRepo(db),
	}
	return h, mock, func() { _ = db.Close() }
}

func factorLoadingRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"instrument_key", "factor", "asof", "loading", "source", "note", "updated_at",
	})
}

func TestAdminFactorExposure_List_Unauthenticated(t *testing.T) {
	h, _, cleanup := newAdminFactorExposureEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/factor-loadings", nil)
	rr := httptest.NewRecorder()
	h.handleListFactorLoadings(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminFactorExposure_List_Forbidden_NonAdmin(t *testing.T) {
	h, mock, cleanup := newAdminFactorExposureEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "user")
	req := authReq(http.MethodGet, "/api/admin/factor-loadings", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListFactorLoadings(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminFactorExposure_List_Happy(t *testing.T) {
	h, mock, cleanup := newAdminFactorExposureEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	asof := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM instrument_factor_loadings").
		WillReturnRows(factorLoadingRows().
			AddRow("US:AAPL", "momentum", asof, 1.2, "manual", "test note", asof),
		)
	req := authReq(http.MethodGet, "/api/admin/factor-loadings", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListFactorLoadings(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Loadings []factorLoadingWire `json:"loadings"`
		RowCount int                 `json:"row_count"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.RowCount != 1 || body.Loadings[0].Factor != "momentum" || body.Loadings[0].Loading != 1.2 {
		t.Errorf("got %+v", body)
	}
	if body.Loadings[0].AsOf != "2026-06-01" {
		t.Errorf("asof = %q", body.Loadings[0].AsOf)
	}
}

func TestAdminFactorExposure_List_RejectsInvalidFactor(t *testing.T) {
	h, mock, cleanup := newAdminFactorExposureEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodGet, "/api/admin/factor-loadings?factor=sector", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleListFactorLoadings(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminFactorExposure_Upsert_Happy(t *testing.T) {
	h, mock, cleanup := newAdminFactorExposureEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO instrument_factor_loadings")).
		WithArgs("US:AAPL", "momentum", sqlmock.AnyArg(), 1.2, "manual", "calibrated by quant lab").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := strings.NewReader(`{"instrument_key":"US:AAPL","factor":"momentum","asof":"2026-06-01","loading":1.2,"source":"manual","note":"calibrated by quant lab"}`)
	req := authReq(http.MethodPost, "/api/admin/factor-loadings", "", "u-1")
	req.Body = http.NoBody
	req.Body = nopCloser{body}
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.handleUpsertFactorLoading(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Loading factorLoadingWire `json:"loading"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Loading.Factor != "momentum" || out.Loading.Loading != 1.2 {
		t.Errorf("got %+v", out)
	}
}

func TestAdminFactorExposure_Upsert_RejectsInvalidFactor(t *testing.T) {
	h, mock, cleanup := newAdminFactorExposureEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	body := strings.NewReader(`{"instrument_key":"US:AAPL","factor":"sector","asof":"2026-06-01","loading":1.2}`)
	req := authReq(http.MethodPost, "/api/admin/factor-loadings", "", "u-1")
	req.Body = nopCloser{body}
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.handleUpsertFactorLoading(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminFactorExposure_Upsert_RejectsOutOfRange(t *testing.T) {
	h, mock, cleanup := newAdminFactorExposureEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	body := strings.NewReader(`{"instrument_key":"X","factor":"momentum","asof":"2026-06-01","loading":99}`)
	req := authReq(http.MethodPost, "/api/admin/factor-loadings", "", "u-1")
	req.Body = nopCloser{body}
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.handleUpsertFactorLoading(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminFactorExposure_Upsert_RejectsBadAsof(t *testing.T) {
	h, mock, cleanup := newAdminFactorExposureEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	body := strings.NewReader(`{"instrument_key":"US:AAPL","factor":"momentum","asof":"2026/06/01","loading":1.2}`)
	req := authReq(http.MethodPost, "/api/admin/factor-loadings", "", "u-1")
	req.Body = nopCloser{body}
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.handleUpsertFactorLoading(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminFactorExposure_Delete_Happy(t *testing.T) {
	h, mock, cleanup := newAdminFactorExposureEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM instrument_factor_loadings")).
		WithArgs("US:AAPL", "momentum", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	req := authReq(http.MethodDelete, "/api/admin/factor-loadings?instrument_key=US:AAPL&factor=momentum&asof=2026-06-01", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleDeleteFactorLoading(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

// nopCloser exposes a reader as an io.ReadCloser without bringing
// in bytes.NewBuffer just for the Close() side. Mirrors io.NopCloser
// but uses an explicit type so we can stash a strings.Reader.
type nopCloser struct {
	r *strings.Reader
}

func (n nopCloser) Read(p []byte) (int, error) { return n.r.Read(p) }
func (n nopCloser) Close() error               { return nil }

// jsonBody is unused in this file but pinned to silence the "bytes"
// import lint when the file evolves.
var _ = bytes.NewReader
