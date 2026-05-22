package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// stubMarketplaceAuctionService is the test-double used in handler tests.
// Each field is an optional override; calls without a matching override
// fail loudly so accidental over-broad stubbing doesn't hide regressions.
type stubMarketplaceAuctionService struct {
	listFn        func(userID string, limit, offset int) ([]AuctionListing, error)
	getFn         func(userID, listingID string) (*AuctionListing, error)
	createFn      func(userID string, input CreateAuctionListingInput) (*AuctionListing, error)
	bidFn         func(userID string, input PlaceAuctionBidInput) (*AuctionBid, *AuctionListing, error)
	settleFn      func(userID, listingID string) (*AuctionSettlementResult, error)
	settleDueFn   func(ctx context.Context, now time.Time, limit int) ([]AuctionSettlementResult, error)
}

func (s stubMarketplaceAuctionService) ListAuctions(userID string, limit, offset int) ([]AuctionListing, error) {
	if s.listFn != nil {
		return s.listFn(userID, limit, offset)
	}
	return nil, errors.New("unexpected ListAuctions call")
}
func (s stubMarketplaceAuctionService) GetAuction(userID, listingID string) (*AuctionListing, error) {
	if s.getFn != nil {
		return s.getFn(userID, listingID)
	}
	return nil, errors.New("unexpected GetAuction call")
}
func (s stubMarketplaceAuctionService) CreateAuction(userID string, input CreateAuctionListingInput) (*AuctionListing, error) {
	if s.createFn != nil {
		return s.createFn(userID, input)
	}
	return nil, errors.New("unexpected CreateAuction call")
}
func (s stubMarketplaceAuctionService) PlaceBid(userID string, input PlaceAuctionBidInput) (*AuctionBid, *AuctionListing, error) {
	if s.bidFn != nil {
		return s.bidFn(userID, input)
	}
	return nil, nil, errors.New("unexpected PlaceBid call")
}
func (s stubMarketplaceAuctionService) SettleAuction(userID, listingID string) (*AuctionSettlementResult, error) {
	if s.settleFn != nil {
		return s.settleFn(userID, listingID)
	}
	return nil, errors.New("unexpected SettleAuction call")
}
func (s stubMarketplaceAuctionService) SettleDueAuctions(ctx context.Context, now time.Time, limit int) ([]AuctionSettlementResult, error) {
	if s.settleDueFn != nil {
		return s.settleDueFn(ctx, now, limit)
	}
	return nil, errors.New("unexpected SettleDueAuctions call")
}

// newAuctionTestHandler builds a FundHandler whose only meaningful
// dependency is the auction service. Every other surface is stubbed so
// the test only exercises the auction routes.
func newAuctionTestHandler(svc MarketplaceAuctionService) *FundHandler {
	h := NewFundHandler(
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
	)
	if svc != nil {
		h.WithAuctionService(svc)
	}
	return h
}

// authedKYCRequest hangs both the authenticated user id and a passing KYC
// status on the request context so the CreateAuction / PlaceAuctionBid
// guards don't short-circuit with 403. The status string must match what
// RequireKYC checks for ("verified").
func authedKYCRequest(req *http.Request, userID string) *http.Request {
	ctx := WithAuthenticatedUserID(req.Context(), userID)
	ctx = WithAuthenticatedUserKYC(ctx, "verified", "tier2_advanced")
	return req.WithContext(ctx)
}

// ---------------------------------------------------------------------------
// Service-unavailable behaviour (matches reflections + skills pattern)
// ---------------------------------------------------------------------------

func TestAuctionEndpointsReturn503WhenServiceUnwired(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	newAuctionTestHandler(nil).RegisterRoutes(mux)

	cases := []struct {
		name string
		req  *http.Request
	}{
		{"list", authedKYCRequest(httptest.NewRequest(http.MethodGet, "/api/marketplace/auctions", nil), "u-1")},
		{"get", authedKYCRequest(httptest.NewRequest(http.MethodGet, "/api/marketplace/auctions/x", nil), "u-1")},
		{"create", authedKYCRequest(httptest.NewRequest(http.MethodPost, "/api/marketplace/auctions", bytes.NewBufferString(`{}`)), "u-1")},
		{"bid", authedKYCRequest(httptest.NewRequest(http.MethodPost, "/api/marketplace/auctions/x/bids", bytes.NewBufferString(`{}`)), "u-1")},
		{"settle", authedKYCRequest(httptest.NewRequest(http.MethodPost, "/api/marketplace/auctions/x/settle", nil), "u-1")},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, tc.req)
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ListAuctions
// ---------------------------------------------------------------------------

func TestListAuctionsReturnsPayload(t *testing.T) {
	t.Parallel()
	stub := stubMarketplaceAuctionService{
		listFn: func(userID string, limit, offset int) ([]AuctionListing, error) {
			if userID != "u-1" {
				t.Fatalf("unexpected user: %s", userID)
			}
			return []AuctionListing{{ID: "auc-1", AgentName: "Researcher", Status: "active"}}, nil
		},
	}
	mux := http.NewServeMux()
	newAuctionTestHandler(stub).RegisterRoutes(mux)
	req := authedKYCRequest(httptest.NewRequest(http.MethodGet, "/api/marketplace/auctions?limit=10", nil), "u-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var out []AuctionListing
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 || out[0].ID != "auc-1" {
		t.Fatalf("unexpected payload: %+v", out)
	}
}

// ---------------------------------------------------------------------------
// CreateAuction
// ---------------------------------------------------------------------------

func TestCreateAuctionValidatesRequest(t *testing.T) {
	t.Parallel()
	stub := stubMarketplaceAuctionService{
		createFn: func(userID string, input CreateAuctionListingInput) (*AuctionListing, error) {
			t.Fatalf("service must not be called when validation fails")
			return nil, nil
		},
	}
	mux := http.NewServeMux()
	newAuctionTestHandler(stub).RegisterRoutes(mux)

	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"missing fundId", `{"agentId":"a-1","startingPriceMinor":100,"endsAt":"2027-01-01T00:00:00Z"}`, http.StatusBadRequest},
		{"missing agentId", `{"fundId":"f-1","startingPriceMinor":100,"endsAt":"2027-01-01T00:00:00Z"}`, http.StatusBadRequest},
		{"zero starting price", `{"fundId":"f-1","agentId":"a-1","startingPriceMinor":0,"endsAt":"2027-01-01T00:00:00Z"}`, http.StatusBadRequest},
		{"missing endsAt", `{"fundId":"f-1","agentId":"a-1","startingPriceMinor":100}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			req := authedKYCRequest(httptest.NewRequest(http.MethodPost, "/api/marketplace/auctions", bytes.NewBufferString(tc.body)), "u-1")
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			if rr.Code != tc.status {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestCreateAuctionForwardsInputAndReturns201(t *testing.T) {
	t.Parallel()
	captured := CreateAuctionListingInput{}
	stub := stubMarketplaceAuctionService{
		createFn: func(userID string, input CreateAuctionListingInput) (*AuctionListing, error) {
			captured = input
			if userID != "u-seller" {
				t.Fatalf("unexpected user: %s", userID)
			}
			return &AuctionListing{
				ID:                 "auc-1",
				Mode:               "auction",
				Status:             "active",
				StartingPriceMinor: input.StartingPriceMinor,
				MinIncrementMinor:  input.MinIncrementMinor,
				AntiSnipeSeconds:   input.AntiSnipeSeconds,
				EndsAt:             &input.EndsAt,
			}, nil
		},
	}
	mux := http.NewServeMux()
	newAuctionTestHandler(stub).RegisterRoutes(mux)
	body := `{
		"fundId":"f-1","agentId":"a-1",
		"startingPriceMinor":1000,
		"minIncrementMinor":50,
		"antiSnipeSeconds":30,
		"currency":"USD",
		"endsAt":"2027-01-01T00:00:00Z"
	}`
	req := authedKYCRequest(httptest.NewRequest(http.MethodPost, "/api/marketplace/auctions", bytes.NewBufferString(body)), "u-seller")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	if captured.FundID != "f-1" || captured.AgentID != "a-1" || captured.StartingPriceMinor != 1000 {
		t.Fatalf("input not forwarded: %+v", captured)
	}
	if captured.MinIncrementMinor != 50 || captured.AntiSnipeSeconds != 30 {
		t.Fatalf("auction config not forwarded: %+v", captured)
	}
}

// ---------------------------------------------------------------------------
// PlaceAuctionBid
// ---------------------------------------------------------------------------

func TestPlaceAuctionBidValidatesAmount(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	newAuctionTestHandler(stubMarketplaceAuctionService{
		bidFn: func(userID string, input PlaceAuctionBidInput) (*AuctionBid, *AuctionListing, error) {
			t.Fatalf("service must not be called for zero bid")
			return nil, nil, nil
		},
	}).RegisterRoutes(mux)
	req := authedKYCRequest(httptest.NewRequest(http.MethodPost, "/api/marketplace/auctions/auc-1/bids", bytes.NewBufferString(`{"bidPriceMinor":0}`)), "u-bidder")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestPlaceAuctionBidReturnsBidAndAuction(t *testing.T) {
	t.Parallel()
	stub := stubMarketplaceAuctionService{
		bidFn: func(userID string, input PlaceAuctionBidInput) (*AuctionBid, *AuctionListing, error) {
			if userID != "u-bidder" || input.ListingID != "auc-1" || input.BidPriceMinor != 500 {
				t.Fatalf("unexpected args: %s %+v", userID, input)
			}
			now := time.Now()
			return &AuctionBid{
					ID:            "bid-1",
					ListingID:     "auc-1",
					BidPriceMinor: 500,
					Status:        "active",
				}, &AuctionListing{
					ID:              "auc-1",
					Status:          "active",
					CurrentBidMinor: intPtr(500),
					CurrentBidID:    "bid-1",
					EndsAt:          &now,
				}, nil
		},
	}
	mux := http.NewServeMux()
	newAuctionTestHandler(stub).RegisterRoutes(mux)
	req := authedKYCRequest(httptest.NewRequest(http.MethodPost, "/api/marketplace/auctions/auc-1/bids", bytes.NewBufferString(`{"bidPriceMinor":500}`)), "u-bidder")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Bid     *AuctionBid     `json:"bid"`
		Auction *AuctionListing `json:"auction"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Bid == nil || resp.Bid.ID != "bid-1" {
		t.Fatalf("missing bid: %+v", resp)
	}
	if resp.Auction == nil || resp.Auction.CurrentBidID != "bid-1" {
		t.Fatalf("missing auction snapshot: %+v", resp.Auction)
	}
}

func TestPlaceAuctionBidMapsConflictTo409(t *testing.T) {
	t.Parallel()
	stub := stubMarketplaceAuctionService{
		bidFn: func(userID string, input PlaceAuctionBidInput) (*AuctionBid, *AuctionListing, error) {
			return nil, nil, ErrConflict
		},
	}
	mux := http.NewServeMux()
	newAuctionTestHandler(stub).RegisterRoutes(mux)
	req := authedKYCRequest(httptest.NewRequest(http.MethodPost, "/api/marketplace/auctions/auc-1/bids", bytes.NewBufferString(`{"bidPriceMinor":1000}`)), "u-bidder")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// SettleAuction
// ---------------------------------------------------------------------------

func TestSettleAuctionReturnsResult(t *testing.T) {
	t.Parallel()
	stub := stubMarketplaceAuctionService{
		settleFn: func(userID, listingID string) (*AuctionSettlementResult, error) {
			if userID != "u-1" || listingID != "auc-1" {
				t.Fatalf("unexpected: %s %s", userID, listingID)
			}
			return &AuctionSettlementResult{
				ListingID:     listingID,
				Outcome:       "sold",
				WinningBidID:  "bid-1",
				WinnerUserID:  "u-winner",
				FinalBidMinor: 1500,
			}, nil
		},
	}
	mux := http.NewServeMux()
	newAuctionTestHandler(stub).RegisterRoutes(mux)
	req := authedKYCRequest(httptest.NewRequest(http.MethodPost, "/api/marketplace/auctions/auc-1/settle", nil), "u-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var out AuctionSettlementResult
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Outcome != "sold" || out.FinalBidMinor != 1500 {
		t.Fatalf("unexpected payload: %+v", out)
	}
}

func TestSettleAuctionMapsForbidden(t *testing.T) {
	t.Parallel()
	stub := stubMarketplaceAuctionService{
		settleFn: func(userID, listingID string) (*AuctionSettlementResult, error) {
			return nil, ErrForbidden
		},
	}
	mux := http.NewServeMux()
	newAuctionTestHandler(stub).RegisterRoutes(mux)
	req := authedKYCRequest(httptest.NewRequest(http.MethodPost, "/api/marketplace/auctions/auc-1/settle", nil), "u-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Authentication guard
// ---------------------------------------------------------------------------

func TestAuctionEndpointsRequireAuthentication(t *testing.T) {
	t.Parallel()
	stub := stubMarketplaceAuctionService{
		listFn:   func(string, int, int) ([]AuctionListing, error) { return nil, nil },
		getFn:    func(string, string) (*AuctionListing, error) { return nil, nil },
		createFn: func(string, CreateAuctionListingInput) (*AuctionListing, error) { return nil, nil },
		bidFn:    func(string, PlaceAuctionBidInput) (*AuctionBid, *AuctionListing, error) { return nil, nil, nil },
		settleFn: func(string, string) (*AuctionSettlementResult, error) { return nil, nil },
	}
	mux := http.NewServeMux()
	newAuctionTestHandler(stub).RegisterRoutes(mux)

	endpoints := []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/marketplace/auctions", nil),
		httptest.NewRequest(http.MethodGet, "/api/marketplace/auctions/x", nil),
		httptest.NewRequest(http.MethodPost, "/api/marketplace/auctions", bytes.NewBufferString(`{}`)),
		httptest.NewRequest(http.MethodPost, "/api/marketplace/auctions/x/bids", bytes.NewBufferString(`{}`)),
		httptest.NewRequest(http.MethodPost, "/api/marketplace/auctions/x/settle", nil),
	}
	for _, req := range endpoints {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for %s, got %d body=%s", req.URL.Path, rr.Code, rr.Body.String())
		}
	}
}

// intPtr is a tiny helper to take the address of an integer literal in
// struct literals (Go doesn't allow &500 syntactically).
func intPtr(v int64) *int64 { return &v }
