package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubPromotionService is the in-memory PromotionService for
// handler-level tests. Each method captures its inputs so the
// tests can assert the handler forwarded fundID + userID from
// the URL / auth context correctly.
type stubPromotionService struct {
	proposeFn  func(userID string, in ProposeInput) (*Promotion, error)
	approveFn  func(userID, fundID, promotionID string) (*Promotion, error)
	rejectFn   func(userID, fundID, promotionID, reason string) (*Promotion, error)
	activateFn func(userID, fundID, promotionID string) (*Promotion, error)
	rollbackFn func(userID, fundID, promotionID, reason string) (*Promotion, error)
	listFn     func(userID, fundID string, limit int) ([]*Promotion, error)
	getFn      func(userID, fundID, promotionID string) (*PromotionDetail, error)
}

func (s stubPromotionService) Propose(u string, in ProposeInput) (*Promotion, error) {
	if s.proposeFn != nil {
		return s.proposeFn(u, in)
	}
	return &Promotion{ID: "p-new", FundID: in.FundID, Status: "pending_review"}, nil
}
func (s stubPromotionService) Approve(u, f, p string) (*Promotion, error) {
	if s.approveFn != nil {
		return s.approveFn(u, f, p)
	}
	return &Promotion{ID: p, FundID: f, Status: "shadow"}, nil
}
func (s stubPromotionService) Reject(u, f, p, r string) (*Promotion, error) {
	if s.rejectFn != nil {
		return s.rejectFn(u, f, p, r)
	}
	return &Promotion{ID: p, FundID: f, Status: "rejected", RejectedReason: r}, nil
}
func (s stubPromotionService) Activate(u, f, p string) (*Promotion, error) {
	if s.activateFn != nil {
		return s.activateFn(u, f, p)
	}
	return &Promotion{ID: p, FundID: f, Status: "active"}, nil
}
func (s stubPromotionService) Rollback(u, f, p, r string) (*Promotion, error) {
	if s.rollbackFn != nil {
		return s.rollbackFn(u, f, p, r)
	}
	return &Promotion{ID: p, FundID: f, Status: "rolled_back", DeactivatedReason: r}, nil
}
func (s stubPromotionService) List(u, f string, limit int) ([]*Promotion, error) {
	if s.listFn != nil {
		return s.listFn(u, f, limit)
	}
	return []*Promotion{{ID: "p-1", FundID: f}}, nil
}
func (s stubPromotionService) Get(u, f, p string) (*PromotionDetail, error) {
	if s.getFn != nil {
		return s.getFn(u, f, p)
	}
	return &PromotionDetail{Promotion: Promotion{ID: p, FundID: f}}, nil
}

// newPromotionHandler returns a FundHandler wired with only the
// PromotionService — everything else gets a benign stub.
func newPromotionHandler(svc PromotionService) *FundHandler {
	return NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	).WithPromotionService(svc)
}

// Propose forwards URL fundID + auth userID and accepts a 201 on
// success.
func TestProposePromotionHappyPath(t *testing.T) {
	var captured ProposeInput
	svc := stubPromotionService{
		proposeFn: func(userID string, in ProposeInput) (*Promotion, error) {
			captured = in
			if userID != "user-1" {
				t.Errorf("userID = %s, want user-1", userID)
			}
			return &Promotion{ID: "p-x", FundID: in.FundID, Status: "pending_review"}, nil
		},
	}
	mux := http.NewServeMux()
	newPromotionHandler(svc).RegisterRoutes(mux)

	body := ProposeInput{BasisJobID: "job-1", Notes: "looks fine"}
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-7/promotions", bytes.NewReader(buf))
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if captured.FundID != "fund-7" {
		t.Errorf("fundID = %s (want fund-7)", captured.FundID)
	}
	if captured.ProposedBy != "user-1" {
		t.Errorf("proposedBy = %s", captured.ProposedBy)
	}
	if captured.BasisJobID != "job-1" {
		t.Errorf("basisJobID = %s", captured.BasisJobID)
	}
}

// Propose: missing basisJobId → 400.
func TestProposePromotionRejectsMissingBasis(t *testing.T) {
	mux := http.NewServeMux()
	newPromotionHandler(stubPromotionService{}).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-1/promotions", bytes.NewReader([]byte(`{}`)))
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// Propose: nil service → 503.
func TestProposePromotionUnavailableWhenServiceNil(t *testing.T) {
	mux := http.NewServeMux()
	newPromotionHandler(nil).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-1/promotions",
		bytes.NewReader([]byte(`{"basisJobId":"j"}`)))
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

// Approve: forwards userID + fundID + promotionID, returns 200.
func TestApprovePromotionForwards(t *testing.T) {
	var capturedUser, capturedFund, capturedID string
	svc := stubPromotionService{
		approveFn: func(u, f, p string) (*Promotion, error) {
			capturedUser, capturedFund, capturedID = u, f, p
			return &Promotion{ID: p, FundID: f, Status: "shadow"}, nil
		},
	}
	mux := http.NewServeMux()
	newPromotionHandler(svc).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-7/promotions/p-42/approve", nil)
	req = authRequest(req, "manager-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if capturedUser != "manager-1" || capturedFund != "fund-7" || capturedID != "p-42" {
		t.Errorf("forwarding wrong: user=%s fund=%s id=%s", capturedUser, capturedFund, capturedID)
	}
}

// Reject with body{"reason":"..."} passes the reason through.
func TestRejectPromotionForwardsReason(t *testing.T) {
	var capturedReason string
	svc := stubPromotionService{
		rejectFn: func(_, _, _, r string) (*Promotion, error) {
			capturedReason = r
			return &Promotion{Status: "rejected"}, nil
		},
	}
	mux := http.NewServeMux()
	newPromotionHandler(svc).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/funds/f/promotions/p/reject",
		bytes.NewReader([]byte(`{"reason":"bad metrics"}`)))
	req = authRequest(req, "u")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if capturedReason != "bad metrics" {
		t.Errorf("reason = %q", capturedReason)
	}
}

// Error mapping: dual-control → 403, illegal transition → 409,
// not-found → 404, basis-ineligible → 400.
func TestPromotionHandlerErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"not found", ErrPromotionNotFound, http.StatusNotFound},
		{"basis ineligible", ErrPromotionBasisIneligible, http.StatusBadRequest},
		{"illegal transition", ErrPromotionIllegalTransition, http.StatusConflict},
		{"dual control", ErrPromotionDualControl, http.StatusForbidden},
		{"unknown", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := stubPromotionService{
				approveFn: func(_, _, _ string) (*Promotion, error) { return nil, tc.err },
			}
			mux := http.NewServeMux()
			newPromotionHandler(svc).RegisterRoutes(mux)
			req := httptest.NewRequest(http.MethodPost, "/api/funds/f/promotions/p/approve", nil)
			req = authRequest(req, "u")
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Errorf("status = %d, want %d (err=%v)", rr.Code, tc.want, tc.err)
			}
		})
	}
}

// List: nil service returns an empty array (UI-friendly), not 503.
func TestListPromotionsNilServiceReturnsEmpty(t *testing.T) {
	mux := http.NewServeMux()
	newPromotionHandler(nil).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/funds/f/promotions", nil)
	req = authRequest(req, "u")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var out []*Promotion
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d, want 0", len(out))
	}
}

// Get returns the detail blob.
func TestGetPromotionReturnsDetail(t *testing.T) {
	svc := stubPromotionService{
		getFn: func(_, f, p string) (*PromotionDetail, error) {
			return &PromotionDetail{
				Promotion: Promotion{ID: p, FundID: f, Status: "active"},
				Events:    []*PromotionEvent{{ID: "ev-1", EventType: "approved"}},
			}, nil
		},
	}
	mux := http.NewServeMux()
	newPromotionHandler(svc).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/funds/f/promotions/p-1", nil)
	req = authRequest(req, "u")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var out PromotionDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Promotion.ID != "p-1" || len(out.Events) != 1 {
		t.Errorf("unexpected detail: %+v", out)
	}
}
