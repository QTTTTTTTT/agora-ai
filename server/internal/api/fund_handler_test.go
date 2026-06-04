package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type stubFundService struct {
	createCompanyFn        func(CreateCompanyInput) (*Company, error)
	listCompaniesFn        func(string) ([]Company, error)
	listCompanyOverviewsFn func(string) ([]CompanyOverview, error)
	createFundFn           func(string, CreateFundInput) (*Fund, error)
	listFundsFn            func(string, string) ([]Fund, error)
	getFundFn              func(string, string) (*Fund, error)
	getForwardGateFn       func(string, string) (*ForwardGateStatus, error)
	updateFundFn           func(string, string, FundConfig) (*Fund, error)
	deleteFundFn           func(string, string) error
}

func (s stubFundService) CreateCompany(input CreateCompanyInput) (*Company, error) {
	if s.createCompanyFn != nil {
		return s.createCompanyFn(input)
	}
	return nil, errors.New("unexpected CreateCompany call")
}

func (s stubFundService) ListCompanies(ownerUserID string) ([]Company, error) {
	if s.listCompaniesFn != nil {
		return s.listCompaniesFn(ownerUserID)
	}
	return nil, errors.New("unexpected ListCompanies call")
}

func (s stubFundService) ListCompanyOverviews(ownerUserID string) ([]CompanyOverview, error) {
	if s.listCompanyOverviewsFn != nil {
		return s.listCompanyOverviewsFn(ownerUserID)
	}
	return nil, errors.New("unexpected ListCompanyOverviews call")
}

func (s stubFundService) CreateFund(userID string, input CreateFundInput) (*Fund, error) {
	if s.createFundFn != nil {
		return s.createFundFn(userID, input)
	}
	return nil, errors.New("unexpected CreateFund call")
}

func (s stubFundService) ListFunds(userID, companyID string) ([]Fund, error) {
	if s.listFundsFn != nil {
		return s.listFundsFn(userID, companyID)
	}
	return nil, errors.New("unexpected ListFunds call")
}

func (s stubFundService) GetFund(userID, fundID string) (*Fund, error) {
	if s.getFundFn != nil {
		return s.getFundFn(userID, fundID)
	}
	return nil, errors.New("unexpected GetFund call")
}

func (s stubFundService) GetForwardGate(userID, fundID string) (*ForwardGateStatus, error) {
	if s.getForwardGateFn != nil {
		return s.getForwardGateFn(userID, fundID)
	}
	return nil, errors.New("unexpected GetForwardGate call")
}

func (s stubFundService) UpdateFund(userID, fundID string, cfg FundConfig) (*Fund, error) {
	if s.updateFundFn != nil {
		return s.updateFundFn(userID, fundID, cfg)
	}
	return nil, errors.New("unexpected UpdateFund call")
}

func (s stubFundService) DeleteFund(userID, fundID string) error {
	if s.deleteFundFn != nil {
		return s.deleteFundFn(userID, fundID)
	}
	return errors.New("unexpected DeleteFund call")
}

type stubTeamService struct {
	addAgentFn          func(string, string, string, string) (*Agent, error)
	bindAgentFn         func(string, string, string) (*Agent, error)
	listOwnedAgentsFn   func(string, string) ([]Agent, error)
	removeAgentFn       func(string, string, string) error
	updateAgentFn       func(string, string, string, AgentConfig) (*Agent, error)
	listAgentsFn        func(string, string) ([]Agent, error)
	getLearningFn       func(string, string) (*AgentLearningStatus, error)
	enableLearningFn    func(string, string, AgentLearningConfigInput) (*AgentLearningStatus, error)
	disableLearningFn   func(string, string) (*AgentLearningStatus, error)
	updateScopeFn       func(string, string, AgentLearningScope) (*AgentLearningStatus, error)
	revokeLearningFn    func(string, string, RevokeAgentLearningInput) (*AgentLearningStatus, error)
	getLineageFn        func(string, string) (*AgentLineageTree, error)
	getLLMUsageFn       func(string, string, time.Time, time.Time) (*LLMUsageVisibility, error)
	listAuditLogsFn     func(string, string, int) (*AuditLogResponse, error)
	exportAuditLogsFn            func(string, string, int) (*AuditLogResponse, error)
	listActivityFn               func(string, string, int, uint64) ([]TeamActivityItem, error)
	pageActivityFn               func(string, string, time.Time, int) ([]TeamActivityItem, error)
	subscribeActivityFn          func(string, string) (*TeamActivityStream, error)
	getAgentSpecializationFn     func(string, string, string) (*AgentSpecialization, error)
	updateAgentSpecializationFn  func(string, string, string, AgentSpecialization) (*AgentSpecialization, error)
}

func (s stubTeamService) AddAgent(userID, fundID, role, focus string) (*Agent, error) {
	if s.addAgentFn != nil {
		return s.addAgentFn(userID, fundID, role, focus)
	}
	return nil, errors.New("unexpected AddAgent call")
}
func (s stubTeamService) BindAgent(userID, fundID, agentID string) (*Agent, error) {
	if s.bindAgentFn != nil {
		return s.bindAgentFn(userID, fundID, agentID)
	}
	return nil, errors.New("unexpected BindAgent call")
}
func (s stubTeamService) ListOwnedAgents(userID, bindStatus string) ([]Agent, error) {
	if s.listOwnedAgentsFn != nil {
		return s.listOwnedAgentsFn(userID, bindStatus)
	}
	return nil, errors.New("unexpected ListOwnedAgents call")
}
func (s stubTeamService) RemoveAgent(userID, fundID, agentID string) error {
	if s.removeAgentFn != nil {
		return s.removeAgentFn(userID, fundID, agentID)
	}
	return errors.New("unexpected RemoveAgent call")
}
func (s stubTeamService) UpdateAgent(userID, fundID, agentID string, cfg AgentConfig) (*Agent, error) {
	if s.updateAgentFn != nil {
		return s.updateAgentFn(userID, fundID, agentID, cfg)
	}
	return nil, errors.New("unexpected UpdateAgent call")
}
func (s stubTeamService) ListAgents(userID, fundID string) ([]Agent, error) {
	if s.listAgentsFn != nil {
		return s.listAgentsFn(userID, fundID)
	}
	return nil, errors.New("unexpected ListAgents call")
}
func (s stubTeamService) GetAgentLearning(userID, agentID string) (*AgentLearningStatus, error) {
	if s.getLearningFn != nil {
		return s.getLearningFn(userID, agentID)
	}
	return nil, errors.New("unexpected GetAgentLearning call")
}
func (s stubTeamService) EnableAgentLearning(userID, agentID string, input AgentLearningConfigInput) (*AgentLearningStatus, error) {
	if s.enableLearningFn != nil {
		return s.enableLearningFn(userID, agentID, input)
	}
	return nil, errors.New("unexpected EnableAgentLearning call")
}
func (s stubTeamService) DisableAgentLearning(userID, agentID string) (*AgentLearningStatus, error) {
	if s.disableLearningFn != nil {
		return s.disableLearningFn(userID, agentID)
	}
	return nil, errors.New("unexpected DisableAgentLearning call")
}
func (s stubTeamService) UpdateAgentLearningScope(userID, agentID string, scope AgentLearningScope) (*AgentLearningStatus, error) {
	if s.updateScopeFn != nil {
		return s.updateScopeFn(userID, agentID, scope)
	}
	return nil, errors.New("unexpected UpdateAgentLearningScope call")
}
func (s stubTeamService) RevokeAgentLearning(userID, agentID string, input RevokeAgentLearningInput) (*AgentLearningStatus, error) {
	if s.revokeLearningFn != nil {
		return s.revokeLearningFn(userID, agentID, input)
	}
	return nil, errors.New("unexpected RevokeAgentLearning call")
}
func (s stubTeamService) GetAgentLineage(userID, agentID string) (*AgentLineageTree, error) {
	if s.getLineageFn != nil {
		return s.getLineageFn(userID, agentID)
	}
	return nil, errors.New("unexpected GetAgentLineage call")
}
func (s stubTeamService) GetLLMUsageVisibility(userID, fundID string, from, to time.Time) (*LLMUsageVisibility, error) {
	if s.getLLMUsageFn != nil {
		return s.getLLMUsageFn(userID, fundID, from, to)
	}
	return nil, errors.New("unexpected GetLLMUsageVisibility call")
}
func (s stubTeamService) ListAuditLogs(userID, fundID string, limit int) (*AuditLogResponse, error) {
	if s.listAuditLogsFn != nil {
		return s.listAuditLogsFn(userID, fundID, limit)
	}
	return nil, errors.New("unexpected ListAuditLogs call")
}
func (s stubTeamService) ExportAuditLogs(userID, fundID string, limit int) (*AuditLogResponse, error) {
	if s.exportAuditLogsFn != nil {
		return s.exportAuditLogsFn(userID, fundID, limit)
	}
	return nil, errors.New("unexpected ExportAuditLogs call")
}
func (s stubTeamService) ListTeamActivity(userID, fundID string, limit int, sinceSeq uint64) ([]TeamActivityItem, error) {
	if s.listActivityFn != nil {
		return s.listActivityFn(userID, fundID, limit, sinceSeq)
	}
	return nil, errors.New("unexpected ListTeamActivity call")
}
func (s stubTeamService) SubscribeTeamActivity(userID, fundID string) (*TeamActivityStream, error) {
	if s.subscribeActivityFn != nil {
		return s.subscribeActivityFn(userID, fundID)
	}
	return nil, errors.New("unexpected SubscribeTeamActivity call")
}
func (s stubTeamService) PageTeamActivity(userID, fundID string, before time.Time, limit int) ([]TeamActivityItem, error) {
	if s.pageActivityFn != nil {
		return s.pageActivityFn(userID, fundID, before, limit)
	}
	return nil, errors.New("unexpected PageTeamActivity call")
}
func (s stubTeamService) GetAgentSpecialization(userID, fundID, agentID string) (*AgentSpecialization, error) {
	if s.getAgentSpecializationFn != nil {
		return s.getAgentSpecializationFn(userID, fundID, agentID)
	}
	return nil, errors.New("unexpected GetAgentSpecialization call")
}
func (s stubTeamService) UpdateAgentSpecialization(userID, fundID, agentID string, body AgentSpecialization) (*AgentSpecialization, error) {
	if s.updateAgentSpecializationFn != nil {
		return s.updateAgentSpecializationFn(userID, fundID, agentID, body)
	}
	return nil, errors.New("unexpected UpdateAgentSpecialization call")
}

type stubPlanService struct {
	listPlansFn func(string, string, PlanListFilter) ([]Plan, error)
}

func (s stubPlanService) ListPlans(userID, fundID string, filter PlanListFilter) ([]Plan, error) {
	if s.listPlansFn != nil {
		return s.listPlansFn(userID, fundID, filter)
	}
	return nil, errors.New("unexpected ListPlans call")
}
func (stubPlanService) GetPlan(string, string) (*Plan, error) {
	return nil, errors.New("unexpected GetPlan call")
}
func (stubPlanService) ApprovePlan(string, string) (*Plan, error) {
	return nil, errors.New("unexpected ApprovePlan call")
}
func (stubPlanService) RejectPlan(string, string, string) (*Plan, error) {
	return nil, errors.New("unexpected RejectPlan call")
}
func (stubPlanService) RefreshPlanQuote(context.Context, string, string) (*Plan, error) {
	return nil, errors.New("unexpected RefreshPlanQuote call")
}

type stubTradeService struct {
	listTradesFn         func(string, string, *time.Time, *time.Time, int, int, bool) ([]Trade, error)
	listChildrenFn       func(string, string, string) ([]Trade, error)
	getPortfolioFn       func(string, string) ([]Position, error)
	getPortfolioQuotesFn func(string, string) ([]PortfolioQuote, error)
	getNAVHistoryFn      func(string, string, *time.Time, *time.Time) ([]NAVPoint, error)
	getPnLFn             func(string, string, *time.Time, *time.Time) (*PnLAttribution, error)
	getTodayPnLFn        func(string, string) (*TodayPnL, error)
}

func (s stubTradeService) ListTrades(userID, fundID string, from, to *time.Time, limit, offset int, excludeChildSlices bool) ([]Trade, error) {
	if s.listTradesFn != nil {
		return s.listTradesFn(userID, fundID, from, to, limit, offset, excludeChildSlices)
	}
	return nil, errors.New("unexpected ListTrades call")
}

func (s stubTradeService) ListTradeChildren(userID, fundID, parentTradeID string) ([]Trade, error) {
	if s.listChildrenFn != nil {
		return s.listChildrenFn(userID, fundID, parentTradeID)
	}
	return nil, errors.New("unexpected ListTradeChildren call")
}

func (s stubTradeService) GetPortfolio(userID, fundID string) ([]Position, error) {
	if s.getPortfolioFn != nil {
		return s.getPortfolioFn(userID, fundID)
	}
	return nil, errors.New("unexpected GetPortfolio call")
}

func (s stubTradeService) GetPortfolioQuotes(userID, fundID string) ([]PortfolioQuote, error) {
	if s.getPortfolioQuotesFn != nil {
		return s.getPortfolioQuotesFn(userID, fundID)
	}
	return nil, errors.New("unexpected GetPortfolioQuotes call")
}

func (s stubTradeService) GetNAVHistory(userID, fundID string, from, to *time.Time) ([]NAVPoint, error) {
	if s.getNAVHistoryFn != nil {
		return s.getNAVHistoryFn(userID, fundID, from, to)
	}
	return nil, errors.New("unexpected GetNAVHistory call")
}

func (s stubTradeService) GetPnLAttribution(userID, fundID string, from, to *time.Time) (*PnLAttribution, error) {
	if s.getPnLFn != nil {
		return s.getPnLFn(userID, fundID, from, to)
	}
	return nil, errors.New("unexpected GetPnLAttribution call")
}

func (s stubTradeService) GetTodayPnL(userID, fundID string) (*TodayPnL, error) {
	if s.getTodayPnLFn != nil {
		return s.getTodayPnLFn(userID, fundID)
	}
	return nil, errors.New("unexpected GetTodayPnL call")
}

type stubWorkflowService struct {
	startWorkflowFn      func(string, string) (*WorkflowStatus, error)
	triggerStepFn        func(string, string, string) (*WorkflowStatus, error)
	getStatusFn          func(string, string) (*WorkflowStatus, error)
	resumeApprovedPlanFn func(string, time.Time, string) error
	getNextRunFn         func(string, string) (*NextWorkflowRun, error)
}

func (s stubWorkflowService) StartWorkflow(userID, fundID string) (*WorkflowStatus, error) {
	if s.startWorkflowFn != nil {
		return s.startWorkflowFn(userID, fundID)
	}
	return nil, errors.New("unexpected StartWorkflow call")
}

func (s stubWorkflowService) TriggerStep(userID, fundID, step string) (*WorkflowStatus, error) {
	if s.triggerStepFn != nil {
		return s.triggerStepFn(userID, fundID, step)
	}
	return nil, errors.New("unexpected TriggerStep call")
}

func (s stubWorkflowService) GetStatus(userID, fundID string) (*WorkflowStatus, error) {
	if s.getStatusFn != nil {
		return s.getStatusFn(userID, fundID)
	}
	return nil, errors.New("unexpected GetStatus call")
}

func (s stubWorkflowService) ResumeApprovedPlan(fundID string, tradingDate time.Time, planID string) error {
	if s.resumeApprovedPlanFn != nil {
		return s.resumeApprovedPlanFn(fundID, tradingDate, planID)
	}
	return errors.New("unexpected ResumeApprovedPlan call")
}

func (s stubWorkflowService) GetNextRun(userID, fundID string) (*NextWorkflowRun, error) {
	if s.getNextRunFn != nil {
		return s.getNextRunFn(userID, fundID)
	}
	return nil, errors.New("unexpected GetNextRun call")
}

type stubMemoryService struct {
	getMemoryFn    func(string, string, string, string) (*MemoryContext, error)
	searchMemoryFn func(string, string, string, string) ([]MemoryEntry, error)
}

func (s stubMemoryService) GetMemory(userID, fundID, layer, agentID string) (*MemoryContext, error) {
	if s.getMemoryFn != nil {
		return s.getMemoryFn(userID, fundID, layer, agentID)
	}
	return nil, errors.New("unexpected GetMemory call")
}
func (s stubMemoryService) SearchMemory(userID, fundID, layer, query string) ([]MemoryEntry, error) {
	if s.searchMemoryFn != nil {
		return s.searchMemoryFn(userID, fundID, layer, query)
	}
	return nil, errors.New("unexpected SearchMemory call")
}

type stubDecisionTraceService struct {
	getDecisionTraceFn func(string, string, string, string) (*DecisionTrace, error)
}

func (s stubDecisionTraceService) GetDecisionTrace(userID, fundID, tradingDate, planID string) (*DecisionTrace, error) {
	if s.getDecisionTraceFn != nil {
		return s.getDecisionTraceFn(userID, fundID, tradingDate, planID)
	}
	return nil, errors.New("unexpected GetDecisionTrace call")
}

type stubMarketService struct {
	getQuotesFn     func(string, string, []string) (*FundMarketQuotes, error)
	getResearchFn   func(string, string, string, int) (*MarketResearch, error)
	getNewsFn       func(string, string, string, int) (*FundMarketNews, error)
	getNewsDigestFn func(string, string, []string, int) (*MarketNewsDigest, error)
}

func (s stubMarketService) GetQuotes(userID, fundID string, symbols []string) (*FundMarketQuotes, error) {
	if s.getQuotesFn != nil {
		return s.getQuotesFn(userID, fundID, symbols)
	}
	return nil, errors.New("unexpected GetQuotes call")
}
func (s stubMarketService) GetResearch(userID, fundID, symbol string, limit int) (*MarketResearch, error) {
	if s.getResearchFn != nil {
		return s.getResearchFn(userID, fundID, symbol, limit)
	}
	return nil, errors.New("unexpected GetResearch call")
}
func (s stubMarketService) GetNews(userID, fundID, symbol string, limit int) (*FundMarketNews, error) {
	if s.getNewsFn != nil {
		return s.getNewsFn(userID, fundID, symbol, limit)
	}
	return nil, errors.New("unexpected GetNews call")
}
func (s stubMarketService) GetNewsDigest(userID, fundID string, symbols []string, limit int) (*MarketNewsDigest, error) {
	if s.getNewsDigestFn != nil {
		return s.getNewsDigestFn(userID, fundID, symbols, limit)
	}
	return nil, errors.New("unexpected GetNewsDigest call")
}

type stubABTestService struct{}

func (stubABTestService) ListTests(string, string) ([]ABTest, error) {
	return nil, errors.New("unexpected ListTests call")
}
func (stubABTestService) CreateTest(string, CreateABTestInput) (*ABTest, error) {
	return nil, errors.New("unexpected CreateTest call")
}
func (stubABTestService) GetTest(string, string) (*ABTest, error) {
	return nil, errors.New("unexpected GetTest call")
}
func (stubABTestService) StartTest(string, string) (*ABTest, error) {
	return nil, errors.New("unexpected StartTest call")
}
func (stubABTestService) StopTest(string, string) (*ABTest, error) {
	return nil, errors.New("unexpected StopTest call")
}
func (stubABTestService) AnalyzeTest(string, string) (*ABTest, error) {
	return nil, errors.New("unexpected AnalyzeTest call")
}
func (stubABTestService) PromoteLearning(string, string, PromoteABTestLearningInput) (*ABTestLearningPromotionResult, error) {
	return nil, errors.New("unexpected PromoteLearning call")
}
func (stubABTestService) ListLearningPromotions(string, string) ([]ABTestLearningPromotion, error) {
	return nil, errors.New("unexpected ListLearningPromotions call")
}
func (stubABTestService) RollbackLearningPromotion(string, string, string) (*ABTestLearningRollbackResult, error) {
	return nil, errors.New("unexpected RollbackLearningPromotion call")
}

type stubMarketplaceService struct{}

func (stubMarketplaceService) ListListings(string, int, int) ([]MarketplaceListing, error) {
	return nil, errors.New("unexpected ListListings call")
}
func (stubMarketplaceService) ListMyListings(string, int, int) ([]MarketplaceListing, error) {
	return nil, errors.New("unexpected ListMyListings call")
}
func (stubMarketplaceService) CreateListing(string, CreateMarketplaceListingInput) (*MarketplaceListing, error) {
	return nil, errors.New("unexpected CreateListing call")
}
func (stubMarketplaceService) CancelListing(string, string) error {
	return errors.New("unexpected CancelListing call")
}
func (stubMarketplaceService) ListBids(string, string, int, int) ([]MarketplaceBid, error) {
	return nil, errors.New("unexpected ListBids call")
}
func (stubMarketplaceService) CreateBid(string, CreateMarketplaceBidInput) (*MarketplaceBid, error) {
	return nil, errors.New("unexpected CreateBid call")
}
func (stubMarketplaceService) PurchaseListing(string, PurchaseMarketplaceListingInput) (*MarketplaceOrder, error) {
	return nil, errors.New("unexpected PurchaseListing call")
}

func authRequest(req *http.Request, userID string) *http.Request {
	return req.WithContext(WithAuthenticatedUserID(req.Context(), userID))
}

func TestFundHandlerExportAuditLogsCSV(t *testing.T) {
	called := false
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{exportAuditLogsFn: func(userID, fundID string, limit int) (*AuditLogResponse, error) {
			called = true
			if userID != "user-1" || fundID != "fund-1" || limit != 2 {
				t.Fatalf("unexpected args: user=%s fund=%s limit=%d", userID, fundID, limit)
			}
			return &AuditLogResponse{Limit: limit, Entries: []AuditLogEntry{{
				ID:           "log-1",
				ActorUserID:  "user-1",
				Action:       "export",
				ResourceType: "audit_log",
				ResourceID:   "fund-1",
				Details:      json.RawMessage(`{"fundId":"fund-1"}`),
				CreatedAt:    time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC),
			}}}, nil
		}},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/audit?limit=2&format=csv", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rr.Code, rr.Body.String())
	}
	if !called {
		t.Fatal("expected ExportAuditLogs to be called")
	}
	if contentType := rr.Header().Get("Content-Type"); !strings.Contains(contentType, "text/csv") {
		t.Fatalf("expected csv content-type, got %q", contentType)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "id,created_at,actor_user_id,action,resource_type,resource_id,details_json") || !strings.Contains(body, "log-1") {
		t.Fatalf("unexpected csv body: %s", body)
	}
}

func TestFundHandlerCreateCompanyRoute(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{
			createCompanyFn: func(input CreateCompanyInput) (*Company, error) {
				if input.OwnerUserID != "user-1" {
					t.Fatalf("expected owner user id %q, got %q", "user-1", input.OwnerUserID)
				}
				if input.Name != "Alpha" {
					t.Fatalf("expected company name %q, got %q", "Alpha", input.Name)
				}
				return &Company{ID: "company-1", OwnerUserID: input.OwnerUserID, Name: input.Name}, nil
			},
		},
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
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/companies", bytes.NewBufferString(`{"name":"Alpha"}`))
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d", http.StatusCreated, rr.Code)
	}
	var company Company
	if err := json.Unmarshal(rr.Body.Bytes(), &company); err != nil {
		t.Fatalf("unmarshal company response: %v", err)
	}
	if company.ID != "company-1" {
		t.Fatalf("expected company id %q, got %q", "company-1", company.ID)
	}
}

// TestFundHandlerGetWorkflowNextRunRoute verifies the next-run banner
// endpoint plumbs userID + fundID into the service, surfaces the
// service's DTO as JSON, and is gated by the same auth middleware as
// the rest of the workflow endpoints. The DTO shape is the contract
// the Decision Center / Agent Learning banners depend on, so this
// test also acts as a frontend-contract guard.
func TestFundHandlerGetWorkflowNextRunRoute(t *testing.T) {
	trigger := time.Date(2026, 5, 22, 13, 30, 0, 0, time.UTC)
	macro := time.Date(2026, 5, 22, 13, 0, 0, 0, time.UTC)
	dailyReview := time.Date(2026, 5, 22, 20, 0, 0, 0, time.UTC)
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{
			getNextRunFn: func(userID, fundID string) (*NextWorkflowRun, error) {
				if userID != "user-1" {
					t.Fatalf("expected user id %q, got %q", "user-1", userID)
				}
				if fundID != "fund-1" {
					t.Fatalf("expected fund id %q, got %q", "fund-1", fundID)
				}
				return &NextWorkflowRun{
					FundID:            fundID,
					TradingDate:       "2026-05-22",
					Timezone:          "America/New_York",
					NextTriggerAt:     trigger,
					CurrentlyInWindow: false,
					Steps: &WorkflowStepSchedule{
						MacroBrief:  macro,
						DailyReview: dailyReview,
					},
				}, nil
			},
		},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	req := authRequest(httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/workflow/next-run", nil), "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var payload NextWorkflowRun
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rr.Body.String())
	}
	if payload.FundID != "fund-1" || payload.TradingDate != "2026-05-22" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if !payload.NextTriggerAt.Equal(trigger) {
		t.Fatalf("expected next trigger %s, got %s", trigger, payload.NextTriggerAt)
	}
	if payload.Steps == nil || !payload.Steps.MacroBrief.Equal(macro) || !payload.Steps.DailyReview.Equal(dailyReview) {
		t.Fatalf("expected step schedule with macroBrief=%s dailyReview=%s, got %+v", macro, dailyReview, payload.Steps)
	}
}

// TestFundHandlerGetWorkflowNextRunUnavailable confirms calendar-disabled
// installs surface a 503 rather than a misleading 200 with empty fields.
// The frontend keys "schedule unavailable" off the status code, so this
// is part of the contract.
func TestFundHandlerGetWorkflowNextRunUnavailable(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{
			getNextRunFn: func(string, string) (*NextWorkflowRun, error) {
				return nil, ErrUpstreamUnavailable
			},
		},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	req := authRequest(httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/workflow/next-run", nil), "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestFundHandlerGetWorkflowNextRunRequiresAuth pins the auth gate.
func TestFundHandlerGetWorkflowNextRunRequiresAuth(t *testing.T) {
	handler := NewFundHandler(stubFundService{}, stubTeamService{}, stubPlanService{}, stubTradeService{}, stubWorkflowService{}, stubMemoryService{}, stubDecisionTraceService{}, stubMarketService{}, stubABTestService{}, stubMarketplaceService{})
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/workflow/next-run", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestFundHandlerCreateCompanyRequiresAuth(t *testing.T) {
	handler := NewFundHandler(stubFundService{}, stubTeamService{}, stubPlanService{}, stubTradeService{}, stubWorkflowService{}, stubMemoryService{}, stubDecisionTraceService{}, stubMarketService{}, stubABTestService{}, stubMarketplaceService{})
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/companies", bytes.NewBufferString(`{"name":"Alpha"}`))
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, rr.Code)
	}
	var payload apiError
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal unauthorized response: %v", err)
	}
	if payload.Message != "unauthorized" {
		t.Fatalf("expected unauthorized message, got %q", payload.Message)
	}
}

func TestFundHandlerGetDashboardRoute(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{
			getFundFn: func(userID, fundID string) (*Fund, error) {
				if userID != "user-1" {
					t.Fatalf("expected user id %q, got %q", "user-1", userID)
				}
				if fundID != "fund-1" {
					t.Fatalf("expected fund id %q, got %q", "fund-1", fundID)
				}
				return &Fund{ID: fundID, Name: "Growth Fund"}, nil
			},
		},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{
			getNAVHistoryFn: func(userID, fundID string, _ *time.Time, _ *time.Time) ([]NAVPoint, error) {
				if userID != "user-1" || fundID != "fund-1" {
					t.Fatalf("unexpected nav access user=%q fund=%q", userID, fundID)
				}
				return []NAVPoint{{Date: "2026-05-11", NAV: 1.02}}, nil
			},
			getPortfolioFn: func(userID, fundID string) ([]Position, error) {
				if userID != "user-1" || fundID != "fund-1" {
					t.Fatalf("unexpected portfolio access user=%q fund=%q", userID, fundID)
				}
				return []Position{{Symbol: "AAPL", Quantity: 10}}, nil
			},
			listTradesFn: func(userID, fundID string, _ *time.Time, _ *time.Time, limit, offset int, _ bool) ([]Trade, error) {
				if userID != "user-1" || fundID != "fund-1" {
					t.Fatalf("unexpected trade access user=%q fund=%q", userID, fundID)
				}
				if limit != 10 || offset != 0 {
					t.Fatalf("unexpected trade pagination limit=%d offset=%d", limit, offset)
				}
				return []Trade{{ID: "trade-1", Symbol: "AAPL", Quantity: 10}}, nil
			},
		},
		stubWorkflowService{
			getStatusFn: func(userID, fundID string) (*WorkflowStatus, error) {
				if userID != "user-1" || fundID != "fund-1" {
					t.Fatalf("unexpected workflow access user=%q fund=%q", userID, fundID)
				}
				return &WorkflowStatus{FundID: "fund-1", State: "running", Step: "macro_brief"}, nil
			},
		},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/dashboard", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rr.Code)
	}
	var dashboard FundDashboard
	if err := json.Unmarshal(rr.Body.Bytes(), &dashboard); err != nil {
		t.Fatalf("unmarshal dashboard response: %v", err)
	}
	if dashboard.Fund == nil || dashboard.Fund.ID != "fund-1" {
		t.Fatalf("expected dashboard fund %q, got %#v", "fund-1", dashboard.Fund)
	}
	if dashboard.Workflow == nil || dashboard.Workflow.Step != "macro_brief" {
		t.Fatalf("expected workflow step %q, got %#v", "macro_brief", dashboard.Workflow)
	}
	if len(dashboard.Trades) != 1 || dashboard.Trades[0].ID != "trade-1" {
		t.Fatalf("unexpected trades payload: %#v", dashboard.Trades)
	}
}

func TestFundHandlerGetForwardGateRoute(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{
			getForwardGateFn: func(userID, fundID string) (*ForwardGateStatus, error) {
				if userID != "user-1" {
					t.Fatalf("expected user id %q, got %q", "user-1", userID)
				}
				if fundID != "fund-1" {
					t.Fatalf("expected fund id %q, got %q", "fund-1", fundID)
				}
				return &ForwardGateStatus{FundID: fundID, Status: "eligible", Eligible: true, RequiredDays: 30, LiveDays: 35, RequiredNAVs: 10, NAVPoints: 20}, nil
			},
		},
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
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/forward-gate", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	var gate ForwardGateStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &gate); err != nil {
		t.Fatalf("unmarshal forward gate response: %v", err)
	}
	if !gate.Eligible || gate.Status != "eligible" || gate.LiveDays != 35 {
		t.Fatalf("unexpected gate response: %#v", gate)
	}
}

func TestFundHandlerTriggerStepValidation(t *testing.T) {
	handler := NewFundHandler(stubFundService{}, stubTeamService{}, stubPlanService{}, stubTradeService{}, stubWorkflowService{}, stubMemoryService{}, stubDecisionTraceService{}, stubMarketService{}, stubABTestService{}, stubMarketplaceService{})
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-1/workflow/step", bytes.NewBufferString(`{}`))
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rr.Code)
	}
	var payload apiError
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal validation response: %v", err)
	}
	if payload.Detail != "step is required" {
		t.Fatalf("expected validation detail %q, got %q", "step is required", payload.Detail)
	}
}

func TestFundHandlerTriggerStepMapsBadInput(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{
			triggerStepFn: func(string, string, string) (*WorkflowStatus, error) {
				return nil, ErrBadInput
			},
		},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-1/workflow/step?step=roundtable", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusBadRequest, rr.Code, rr.Body.String())
	}
}

func TestFundHandlerGetFundMapsNotFound(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{
			getFundFn: func(string, string) (*Fund, error) {
				return nil, ErrNotFound
			},
		},
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
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/missing", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, rr.Code)
	}
	var payload apiError
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if payload.Message != "fund not found" {
		t.Fatalf("expected message %q, got %q", "fund not found", payload.Message)
	}
}

func TestFundHandlerCreateFundAcceptsMarketProfileFields(t *testing.T) {
	var received CreateFundInput
	var receivedUserID string
	handler := NewFundHandler(
		stubFundService{
			createFundFn: func(userID string, input CreateFundInput) (*Fund, error) {
				receivedUserID = userID
				received = input
				return &Fund{
					ID:               "fund-1",
					CompanyID:        input.CompanyID,
					Name:             input.Name,
					TradingMode:      input.TradingMode,
					InitialCapital:   input.InitialCapital,
					Market:           input.Market,
					Exchange:         input.Exchange,
					AssetClass:       input.AssetClass,
					BaseCurrency:     input.BaseCurrency,
					BenchmarkSymbol:  input.BenchmarkSymbol,
					PrimaryDirection: input.PrimaryDirection,
					Universe:         input.Universe,
					TeamIntervals:    input.TeamIntervals,
					Specialization:   input.Specialization,
				}, nil
			},
		},
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
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	body := bytes.NewBufferString(`{"name":"Global Alpha","description":"multi-market","tradingMode":"simulation","initialCapital":250000,"market":"crypto","exchange":"BINANCE","assetClass":"crypto","baseCurrency":"USDT","benchmarkSymbol":"BTCUSDT","primaryDirection":"crypto","universe":{"mode":"manual","symbols":["BTCUSDT","ETHUSDT"],"themes":["CPO","光模块"],"sectors":["technology"],"customFilters":["marketCap>10B"]},"teamIntervals":{"trader":15,"researcher":20},"specialization":{"team":{"markets":["crypto"],"themes":["AI infra"],"instruments":["BTCUSDT"],"styleHints":["trend-following"]}}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/companies/company-1/funds", body)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	if receivedUserID != "user-1" {
		t.Fatalf("expected user id %q, got %q", "user-1", receivedUserID)
	}
	if received.CompanyID != "company-1" || received.Market != "crypto" || received.Exchange != "BINANCE" {
		t.Fatalf("unexpected create fund input: %#v", received)
	}
	if received.BaseCurrency != "USDT" || received.BenchmarkSymbol != "BTCUSDT" || received.PrimaryDirection != "crypto" {
		t.Fatalf("unexpected market profile input: %#v", received)
	}
	if received.Universe == nil || received.Universe.Mode != "manual" || len(received.Universe.Symbols) != 2 || received.Universe.Symbols[0] != "BTCUSDT" {
		t.Fatalf("unexpected universe input: %#v", received.Universe)
	}
	if len(received.Universe.Themes) != 2 || received.Universe.Themes[0] != "CPO" {
		t.Fatalf("unexpected universe themes input: %#v", received.Universe.Themes)
	}
	if len(received.Universe.Sectors) != 1 || received.Universe.Sectors[0] != "technology" {
		t.Fatalf("unexpected universe sectors input: %#v", received.Universe.Sectors)
	}
	if len(received.Universe.CustomFilters) != 1 || received.Universe.CustomFilters[0] != "marketCap>10B" {
		t.Fatalf("unexpected universe custom filters input: %#v", received.Universe.CustomFilters)
	}
	if received.TeamIntervals == nil || received.TeamIntervals.Trader == nil || *received.TeamIntervals.Trader != 15 || received.TeamIntervals.Researcher == nil || *received.TeamIntervals.Researcher != 20 {
		t.Fatalf("unexpected team intervals input: %#v", received.TeamIntervals)
	}
	if received.Specialization == nil || received.Specialization.Team == nil || len(received.Specialization.Team.Markets) != 1 || received.Specialization.Team.Markets[0] != "crypto" {
		t.Fatalf("unexpected specialization input: %#v", received.Specialization)
	}
	if len(received.Specialization.Team.Instruments) != 1 || received.Specialization.Team.Instruments[0] != "BTCUSDT" {
		t.Fatalf("unexpected specialization instruments input: %#v", received.Specialization.Team.Instruments)
	}

	var fund Fund
	if err := json.Unmarshal(rr.Body.Bytes(), &fund); err != nil {
		t.Fatalf("unmarshal fund response: %v", err)
	}
	if fund.Market != "crypto" || fund.BaseCurrency != "USDT" || fund.PrimaryDirection != "crypto" {
		t.Fatalf("unexpected fund response: %#v", fund)
	}
	if fund.TeamIntervals == nil || fund.TeamIntervals.Trader == nil || *fund.TeamIntervals.Trader != 15 {
		t.Fatalf("unexpected fund team intervals response: %#v", fund.TeamIntervals)
	}
	if fund.Specialization == nil || fund.Specialization.Team == nil || len(fund.Specialization.Team.Themes) != 1 || fund.Specialization.Team.Themes[0] != "AI infra" {
		t.Fatalf("unexpected fund specialization response: %#v", fund.Specialization)
	}
}

func TestWriteJSONFallsBackOnEncodeError(t *testing.T) {
	rr := httptest.NewRecorder()
	writeJSON(rr, http.StatusOK, map[string]float64{"bad": math.NaN()})

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d", http.StatusInternalServerError, rr.Code)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"code":500,"message":"failed to encode response"}` {
		t.Fatalf("unexpected body: %s", rr.Body.String())
	}
}

func TestFundHandlerUpdateFundAcceptsMarketProfilePatch(t *testing.T) {
	var received FundConfig
	handler := NewFundHandler(
		stubFundService{
			updateFundFn: func(userID, fundID string, cfg FundConfig) (*Fund, error) {
				if userID != "user-1" {
					t.Fatalf("expected user id %q, got %q", "user-1", userID)
				}
				if fundID != "fund-1" {
					t.Fatalf("expected fund id %q, got %q", "fund-1", fundID)
				}
				received = cfg
				return &Fund{
					ID:               fundID,
					Market:           valueOrEmpty(cfg.Market),
					Exchange:         valueOrEmpty(cfg.Exchange),
					AssetClass:       valueOrEmpty(cfg.AssetClass),
					BaseCurrency:     valueOrEmpty(cfg.BaseCurrency),
					BenchmarkSymbol:  valueOrEmpty(cfg.BenchmarkSymbol),
					PrimaryDirection: valueOrEmpty(cfg.PrimaryDirection),
					Universe:         cfg.Universe,
					TeamIntervals:    cfg.TeamIntervals,
					Specialization:   cfg.Specialization,
				}, nil
			},
		},
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
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	body := bytes.NewBufferString(`{"market":"us_equity","exchange":"NASDAQ","assetClass":"equity","baseCurrency":"USD","benchmarkSymbol":"QQQ","primaryDirection":"stocks","universe":{"mode":"manual","symbols":["AAPL","NVDA"],"themes":["CPO","光模块"],"sectors":["technology"],"customFilters":["marketCap>10B"]},"teamIntervals":{"trader":15,"risk":25},"specialization":{"team":{"markets":["us_equity"],"assetClasses":["equity"],"themes":["CPO"],"instruments":["NVDA"],"styleHints":["growth"]}}}`)
	req := httptest.NewRequest(http.MethodPut, "/api/funds/fund-1", body)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	if received.Market == nil || *received.Market != "us_equity" || received.Exchange == nil || *received.Exchange != "NASDAQ" {
		t.Fatalf("unexpected update config: %#v", received)
	}
	if received.BaseCurrency == nil || *received.BaseCurrency != "USD" || received.PrimaryDirection == nil || *received.PrimaryDirection != "stocks" {
		t.Fatalf("unexpected update market profile: %#v", received)
	}
	if received.Universe == nil || len(received.Universe.Symbols) != 2 || received.Universe.Symbols[1] != "NVDA" {
		t.Fatalf("unexpected update universe: %#v", received.Universe)
	}
	if len(received.Universe.Themes) != 2 || received.Universe.Themes[1] != "光模块" {
		t.Fatalf("unexpected update universe themes: %#v", received.Universe.Themes)
	}
	if len(received.Universe.Sectors) != 1 || received.Universe.Sectors[0] != "technology" {
		t.Fatalf("unexpected update universe sectors: %#v", received.Universe.Sectors)
	}
	if len(received.Universe.CustomFilters) != 1 || received.Universe.CustomFilters[0] != "marketCap>10B" {
		t.Fatalf("unexpected update universe custom filters: %#v", received.Universe.CustomFilters)
	}
	if received.TeamIntervals == nil || received.TeamIntervals.Trader == nil || *received.TeamIntervals.Trader != 15 || received.TeamIntervals.Risk == nil || *received.TeamIntervals.Risk != 25 {
		t.Fatalf("unexpected update team intervals: %#v", received.TeamIntervals)
	}
	if received.Specialization == nil || received.Specialization.Team == nil || len(received.Specialization.Team.Markets) != 1 || received.Specialization.Team.Markets[0] != "us_equity" {
		t.Fatalf("unexpected update specialization: %#v", received.Specialization)
	}
	if len(received.Specialization.Team.StyleHints) != 1 || received.Specialization.Team.StyleHints[0] != "growth" {
		t.Fatalf("unexpected update specialization style hints: %#v", received.Specialization.Team.StyleHints)
	}

	var fund Fund
	if err := json.Unmarshal(rr.Body.Bytes(), &fund); err != nil {
		t.Fatalf("unmarshal updated fund response: %v", err)
	}
	if fund.Exchange != "NASDAQ" || fund.BenchmarkSymbol != "QQQ" || fund.PrimaryDirection != "stocks" {
		t.Fatalf("unexpected updated fund response: %#v", fund)
	}
	if fund.TeamIntervals == nil || fund.TeamIntervals.Risk == nil || *fund.TeamIntervals.Risk != 25 {
		t.Fatalf("unexpected updated fund team intervals response: %#v", fund.TeamIntervals)
	}
	if fund.Specialization == nil || fund.Specialization.Team == nil || len(fund.Specialization.Team.Instruments) != 1 || fund.Specialization.Team.Instruments[0] != "NVDA" {
		t.Fatalf("unexpected updated fund specialization response: %#v", fund.Specialization)
	}
}

// TestFundHandlerUpdateFundAcceptsAutoExecuteOnlyPatch protects the
// "at least one field" guard: the auto-execute settings modal sends
// just {autoExecute, researchTier} when the user saves — the guard
// must NOT reject those bodies as empty. It also verifies that the
// new DecisionOffsetMinutes field round-trips through the handler.
func TestFundHandlerUpdateFundAcceptsAutoExecuteOnlyPatch(t *testing.T) {
	var received FundConfig
	handler := NewFundHandler(
		stubFundService{
			updateFundFn: func(userID, fundID string, cfg FundConfig) (*Fund, error) {
				received = cfg
				return &Fund{ID: fundID}, nil
			},
		},
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
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	body := bytes.NewBufferString(`{"autoExecute":{"enabled":true,"decisionIntervalMinutes":30},"researchTier":"advanced"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/funds/fund-1", body)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	if received.AutoExecute == nil || !received.AutoExecute.Enabled {
		t.Fatalf("expected auto-execute payload to be parsed, got %#v", received.AutoExecute)
	}
	if received.AutoExecute.DecisionIntervalMinutes == nil || *received.AutoExecute.DecisionIntervalMinutes != 30 {
		t.Fatalf("expected decisionIntervalMinutes=30, got %#v", received.AutoExecute.DecisionIntervalMinutes)
	}
	if received.ResearchTier == nil || *received.ResearchTier != "advanced" {
		t.Fatalf("expected researchTier=advanced, got %#v", received.ResearchTier)
	}
}

// TestFundHandlerUpdateFundRejectsEmptyBody locks the guard's negative
// path so a future regression that silently drops fields from the
// allow-list still surfaces as a test failure rather than an opaque
// 400 in production.
func TestFundHandlerUpdateFundRejectsEmptyBody(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{
			updateFundFn: func(userID, fundID string, cfg FundConfig) (*Fund, error) {
				t.Fatalf("update should not be invoked for an empty body, got cfg=%#v", cfg)
				return nil, nil
			},
		},
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
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPut, "/api/funds/fund-1", bytes.NewBufferString(`{}`))
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusBadRequest, rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "at least one field") {
		t.Fatalf("expected validation error body, got %s", rr.Body.String())
	}
}

func TestFundHandlerListTeamIncludesLatestLearningFields(t *testing.T) {
	dailyReturn := 0.0234
	skillConfig := json.RawMessage(`{"enabled":true,"skills":[{"key":"trade-checklist"}]}`)
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{
			listAgentsFn: func(userID, fundID string) ([]Agent, error) {
				if userID != "user-1" {
					t.Fatalf("expected user id %q, got %q", "user-1", userID)
				}
				if fundID != "fund-1" {
					t.Fatalf("expected fund id %q, got %q", "fund-1", fundID)
				}
				return []Agent{{
					ID:                    "agent-1",
					AgentID:               "agent-1",
					Role:                  "trader",
					SkillConfig:           skillConfig,
					LatestLearningSummary: "当日执行节奏有效，但需要减少部分成交。",
					LatestLearningAt:      time.Date(2026, 5, 11, 15, 30, 0, 0, time.UTC),
					LatestLearningReturn:  &dailyReturn,
					LatestLearningTags:    []string{"self_learning", "trader"},
				}}, nil
			},
		},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/team", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	var agents []Agent
	if err := json.Unmarshal(rr.Body.Bytes(), &agents); err != nil {
		t.Fatalf("unmarshal team response: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected one agent, got %#v", agents)
	}
	if agents[0].LatestLearningSummary != "当日执行节奏有效，但需要减少部分成交。" {
		t.Fatalf("unexpected latest learning summary: %#v", agents[0].LatestLearningSummary)
	}
	if string(agents[0].SkillConfig) != string(skillConfig) {
		t.Fatalf("unexpected skill config: %s", string(agents[0].SkillConfig))
	}
	if agents[0].LatestLearningReturn == nil || *agents[0].LatestLearningReturn != dailyReturn {
		t.Fatalf("unexpected latest learning return: %#v", agents[0].LatestLearningReturn)
	}
	if len(agents[0].LatestLearningTags) != 2 || agents[0].LatestLearningTags[0] != "self_learning" {
		t.Fatalf("unexpected latest learning tags: %#v", agents[0].LatestLearningTags)
	}
}

func TestFundHandlerAgentLearningRoutes(t *testing.T) {
	autoApply := false
	maxLessons := 5
	var receivedScope AgentLearningScope
	var receivedRevoke RevokeAgentLearningInput
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{
			getLearningFn: func(userID, agentID string) (*AgentLearningStatus, error) {
				if userID != "user-1" || agentID != "agent-1" {
					t.Fatalf("unexpected get learning access user=%q agent=%q", userID, agentID)
				}
				return &AgentLearningStatus{AgentID: agentID, Enabled: true, AutoApplyAdjustments: true, MaxLessonsPerDay: 3}, nil
			},
			enableLearningFn: func(userID, agentID string, input AgentLearningConfigInput) (*AgentLearningStatus, error) {
				if userID != "user-1" || agentID != "agent-1" {
					t.Fatalf("unexpected enable learning access user=%q agent=%q", userID, agentID)
				}
				if input.AutoApplyAdjustments == nil || *input.AutoApplyAdjustments != autoApply || input.MaxLessonsPerDay == nil || *input.MaxLessonsPerDay != maxLessons {
					t.Fatalf("unexpected enable input: %#v", input)
				}
				return &AgentLearningStatus{AgentID: agentID, Enabled: true, AutoApplyAdjustments: autoApply, MaxLessonsPerDay: maxLessons}, nil
			},
			disableLearningFn: func(userID, agentID string) (*AgentLearningStatus, error) {
				if userID != "user-1" || agentID != "agent-1" {
					t.Fatalf("unexpected disable learning access user=%q agent=%q", userID, agentID)
				}
				return &AgentLearningStatus{AgentID: agentID, Enabled: false, AutoApplyAdjustments: true, MaxLessonsPerDay: 3}, nil
			},
			updateScopeFn: func(userID, agentID string, scope AgentLearningScope) (*AgentLearningStatus, error) {
				if userID != "user-1" || agentID != "agent-1" {
					t.Fatalf("unexpected scope learning access user=%q agent=%q", userID, agentID)
				}
				receivedScope = scope
				return &AgentLearningStatus{AgentID: agentID, Enabled: true, AutoApplyAdjustments: true, MaxLessonsPerDay: 3, Scope: &scope}, nil
			},
			revokeLearningFn: func(userID, agentID string, input RevokeAgentLearningInput) (*AgentLearningStatus, error) {
				if userID != "user-1" || agentID != "agent-1" {
					t.Fatalf("unexpected revoke learning access user=%q agent=%q", userID, agentID)
				}
				receivedRevoke = input
				return &AgentLearningStatus{AgentID: agentID, Enabled: true, RevokedAt: "2026-05-17T00:00:00Z", RevokedReason: input.Reason}, nil
			},
		},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/agents/agent-1/learning", ""},
		{http.MethodPut, "/api/agents/agent-1/learning/enable", `{"autoApplyAdjustments":false,"maxLessonsPerDay":5}`},
		{http.MethodPut, "/api/agents/agent-1/learning/disable", ""},
		{http.MethodPut, "/api/agents/agent-1/learning/scope", `{"fundIds":["fund-1"],"markets":["us_equity"],"memoryScope":"fund_only"}`},
		{http.MethodPost, "/api/agents/agent-1/learning/revoke", `{"reason":"bad lesson"}`},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req = authRequest(req, "user-1")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s %s expected status %d, got %d body=%s", tc.method, tc.path, http.StatusOK, rr.Code, rr.Body.String())
		}
		var status AgentLearningStatus
		if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
			t.Fatalf("unmarshal learning response: %v", err)
		}
		if status.AgentID != "agent-1" {
			t.Fatalf("unexpected learning status: %#v", status)
		}
	}
	if len(receivedScope.FundIDs) != 1 || receivedScope.FundIDs[0] != "fund-1" || receivedScope.MemoryScope != "fund_only" {
		t.Fatalf("unexpected received scope: %#v", receivedScope)
	}
	if receivedRevoke.Reason != "bad lesson" {
		t.Fatalf("unexpected revoke input: %#v", receivedRevoke)
	}
}

func TestFundHandlerGetAgentLineageRoute(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{getLineageFn: func(userID, agentID string) (*AgentLineageTree, error) {
			if userID != "user-1" || agentID != "agent-1" {
				t.Fatalf("unexpected lineage access user=%q agent=%q", userID, agentID)
			}
			return &AgentLineageTree{
				AgentID:       agentID,
				AncestorCount: 1,
				MaxDepth:      1,
				Root: AgentLineageNode{AgentID: agentID, AgentName: "Clone", Ancestors: []AgentLineageNode{{
					AgentID:    "parent-1",
					AgentName:  "Origin",
					DerivedVia: "buyout",
				}}},
			}, nil
		}},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/agents/agent-1/lineage", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	var tree AgentLineageTree
	if err := json.Unmarshal(rr.Body.Bytes(), &tree); err != nil {
		t.Fatalf("unmarshal lineage response: %v", err)
	}
	if tree.AgentID != "agent-1" || tree.AncestorCount != 1 || len(tree.Root.Ancestors) != 1 {
		t.Fatalf("unexpected lineage response: %#v", tree)
	}
}

func TestFundHandlerUpdateAgentAcceptsSkillConfig(t *testing.T) {
	var received AgentConfig
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{
			updateAgentFn: func(userID, fundID, agentID string, cfg AgentConfig) (*Agent, error) {
				received = cfg
				return &Agent{ID: agentID, AgentID: agentID, Role: "trader", SkillConfig: *cfg.SkillConfig}, nil
			},
		},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	body := bytes.NewBufferString(`{"skillConfig":{"enabled":true,"skills":[{"key":"trade-checklist","content":"拆单执行","match":{"roles":["trader"]}}]}}`)
	req := httptest.NewRequest(http.MethodPut, "/api/funds/fund-1/team/agent-1", body)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	if received.SkillConfig == nil {
		t.Fatal("expected skillConfig to be decoded")
	}
	if string(*received.SkillConfig) != `{"enabled":true,"skills":[{"key":"trade-checklist","content":"拆单执行","match":{"roles":["trader"]}}]}` {
		t.Fatalf("unexpected decoded skillConfig: %s", string(*received.SkillConfig))
	}
}

func TestFundHandlerGetMarketQuotesRoute(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{
			getQuotesFn: func(userID, fundID string, symbols []string) (*FundMarketQuotes, error) {
				if userID != "user-1" || fundID != "fund-1" {
					t.Fatalf("unexpected market quote access user=%q fund=%q", userID, fundID)
				}
				if len(symbols) != 2 || symbols[0] != "AAPL" || symbols[1] != "MSFT" {
					t.Fatalf("unexpected symbols: %#v", symbols)
				}
				return &FundMarketQuotes{FundID: fundID, Quotes: []MarketQuote{{Symbol: "AAPL", Price: 123.45, Source: "quantdinger"}}}, nil
			},
		},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/market/quotes?symbols=AAPL,MSFT", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	var payload FundMarketQuotes
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal quotes response: %v", err)
	}
	if payload.FundID != "fund-1" || len(payload.Quotes) != 1 || payload.Quotes[0].Symbol != "AAPL" {
		t.Fatalf("unexpected quotes payload: %#v", payload)
	}
}

func TestFundHandlerGetMarketResearchRequiresSymbol(t *testing.T) {
	handler := NewFundHandler(stubFundService{}, stubTeamService{}, stubPlanService{}, stubTradeService{}, stubWorkflowService{}, stubMemoryService{}, stubDecisionTraceService{}, stubMarketService{}, stubABTestService{}, stubMarketplaceService{})
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/market/research", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rr.Code)
	}
	var payload apiError
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal validation response: %v", err)
	}
	if payload.Detail != "symbol is required" {
		t.Fatalf("expected validation detail %q, got %q", "symbol is required", payload.Detail)
	}
}

func TestFundHandlerGetMarketNewsRoute(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{
			getNewsFn: func(userID, fundID, symbol string, limit int) (*FundMarketNews, error) {
				if userID != "user-1" || fundID != "fund-1" {
					t.Fatalf("unexpected market news access user=%q fund=%q", userID, fundID)
				}
				if symbol != "AAPL" {
					t.Fatalf("expected symbol %q, got %q", "AAPL", symbol)
				}
				if limit != 4 {
					t.Fatalf("expected market news limit 4, got %d", limit)
				}
				return &FundMarketNews{FundID: fundID, Symbol: symbol, Items: []MarketNewsItem{{Title: "AAPL rally", Source: "local-search", Symbols: []string{"AAPL"}}}}, nil
			},
		},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/market/news?symbol=AAPL&limit=4", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	var payload FundMarketNews
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal market news response: %v", err)
	}
	if payload.FundID != "fund-1" || payload.Symbol != "AAPL" || len(payload.Items) != 1 || payload.Items[0].Source != "local-search" {
		t.Fatalf("unexpected market news payload: %#v", payload)
	}
}

func TestFundHandlerGetMarketNewsRequiresSymbol(t *testing.T) {
	handler := NewFundHandler(stubFundService{}, stubTeamService{}, stubPlanService{}, stubTradeService{}, stubWorkflowService{}, stubMemoryService{}, stubDecisionTraceService{}, stubMarketService{}, stubABTestService{}, stubMarketplaceService{})
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/market/news", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rr.Code)
	}
	var payload apiError
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal validation response: %v", err)
	}
	if payload.Detail != "symbol is required" {
		t.Fatalf("expected validation detail %q, got %q", "symbol is required", payload.Detail)
	}
}

func TestFundHandlerGetMarketNewsDigestRoute(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{
			getNewsDigestFn: func(userID, fundID string, symbols []string, limit int) (*MarketNewsDigest, error) {
				if userID != "user-1" || fundID != "fund-1" {
					t.Fatalf("unexpected market news digest access user=%q fund=%q", userID, fundID)
				}
				if len(symbols) != 2 || symbols[0] != "AAPL" || symbols[1] != "TSLA" {
					t.Fatalf("unexpected digest symbols: %#v", symbols)
				}
				if limit != 6 {
					t.Fatalf("expected digest limit 6, got %d", limit)
				}
				return &MarketNewsDigest{FundID: fundID, Symbols: symbols, Items: []MarketNewsItem{{Title: "AAPL rally", Symbols: []string{"AAPL"}}}}, nil
			},
		},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/market/news/digest?symbols=AAPL,TSLA&limit=6", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	var payload MarketNewsDigest
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal digest response: %v", err)
	}
	if payload.FundID != "fund-1" || len(payload.Items) != 1 || payload.Items[0].Title != "AAPL rally" {
		t.Fatalf("unexpected digest payload: %#v", payload)
	}
}

func TestFundHandlerGetMemorySupportsAnalysisLayer(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{},
		stubMemoryService{
			getMemoryFn: func(userID, fundID, layer, agentID string) (*MemoryContext, error) {
				if userID != "user-1" || fundID != "fund-1" {
					t.Fatalf("unexpected memory access user=%q fund=%q", userID, fundID)
				}
				if layer != "analysis" {
					t.Fatalf("expected analysis layer, got %q", layer)
				}
				if agentID != "" {
					t.Fatalf("expected empty agent id, got %q", agentID)
				}
				return &MemoryContext{
					FundID: fundID,
					Layer:  layer,
					Entries: []MemoryEntry{{
						ID:      "mem-1",
						Title:   "AAPL market research",
						Content: "AAPL: market research",
						Layer:   "analysis",
						Tags:    []string{"market-research", "AAPL"},
					}},
				}, nil
			},
		},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/memory?layer=analysis", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	var payload MemoryContext
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal memory response: %v", err)
	}
	if payload.Layer != "analysis" || len(payload.Entries) != 1 || payload.Entries[0].Title != "AAPL market research" {
		t.Fatalf("unexpected memory payload: %#v", payload)
	}
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// ---------------------------------------------------------------------------
// Phase 3A-5 strategy attribution refresh
// ---------------------------------------------------------------------------

type stubAttributionService struct {
	getFn     func(userID, fundID string, days int) (*AttributionResponse, error)
	refreshFn func(userID, fundID string, days int) (*AttributionResponse, error)
}

func (s stubAttributionService) GetAttribution(userID, fundID string, days int) (*AttributionResponse, error) {
	if s.getFn != nil {
		return s.getFn(userID, fundID, days)
	}
	return nil, nil
}

func (s stubAttributionService) RefreshAttribution(userID, fundID string, days int) (*AttributionResponse, error) {
	if s.refreshFn != nil {
		return s.refreshFn(userID, fundID, days)
	}
	return nil, nil
}

func newAttributionHandler(attr AttributionService) *FundHandler {
	h := NewFundHandler(stubFundService{}, stubTeamService{}, stubPlanService{}, stubTradeService{}, stubWorkflowService{}, stubMemoryService{}, stubDecisionTraceService{}, stubMarketService{}, stubABTestService{}, stubMarketplaceService{})
	if attr != nil {
		h = h.WithAttributionService(attr)
	}
	return h
}

func TestRefreshStrategyAttributionRequiresService(t *testing.T) {
	mux := http.NewServeMux()
	newAttributionHandler(nil).RegisterRoutes(mux)
	req := authRequest(httptest.NewRequest(http.MethodPost, "/api/funds/fund-1/strategy-attribution/refresh", nil), "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when adapter missing, got %d", rr.Code)
	}
}

func TestRefreshStrategyAttributionCallsService(t *testing.T) {
	var capturedUser, capturedFund string
	var capturedDays int
	stub := stubAttributionService{
		refreshFn: func(userID, fundID string, days int) (*AttributionResponse, error) {
			capturedUser, capturedFund, capturedDays = userID, fundID, days
			return &AttributionResponse{
				FundID:     fundID,
				WindowDays: days,
				Lessons: []AttributionLessonDTO{
					{Kind: "insufficient_data", Severity: "info", Title: "No closed trades"},
				},
			}, nil
		},
	}
	mux := http.NewServeMux()
	newAttributionHandler(stub).RegisterRoutes(mux)
	req := authRequest(httptest.NewRequest(http.MethodPost, "/api/funds/fund-42/strategy-attribution/refresh?days=14", nil), "user-7")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if capturedUser != "user-7" || capturedFund != "fund-42" || capturedDays != 14 {
		t.Fatalf("service called with unexpected args: user=%q fund=%q days=%d", capturedUser, capturedFund, capturedDays)
	}
	var resp AttributionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal refresh response: %v", err)
	}
	if resp.FundID != "fund-42" || resp.WindowDays != 14 || len(resp.Lessons) != 1 || resp.Lessons[0].Kind != "insufficient_data" {
		t.Fatalf("unexpected refresh payload: %#v", resp)
	}
}

func TestRefreshStrategyAttributionRequiresAuth(t *testing.T) {
	stub := stubAttributionService{
		refreshFn: func(string, string, int) (*AttributionResponse, error) {
			t.Fatal("refresh should not be invoked when auth is missing")
			return nil, nil
		},
	}
	mux := http.NewServeMux()
	newAttributionHandler(stub).RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/funds/fund-1/strategy-attribution/refresh", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d", rr.Code)
	}
}

func TestRefreshStrategyAttributionMapsServiceError(t *testing.T) {
	stub := stubAttributionService{
		refreshFn: func(string, string, int) (*AttributionResponse, error) {
			return nil, ErrNotFound
		},
	}
	mux := http.NewServeMux()
	newAttributionHandler(stub).RegisterRoutes(mux)
	req := authRequest(httptest.NewRequest(http.MethodPost, "/api/funds/fund-x/strategy-attribution/refresh", nil), "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 from mapped not-found, got %d", rr.Code)
	}
}

// TestFundHandlerInvertedRangeRejected guards the from/to ordering
// check on the trade/NAV/PnL-attribution endpoints. Without this
// guard, callers that accidentally swap the dates get a 200 with
// data computed across a backwards window — for pnl-attribution the
// numbers look plausible (the unrealized component leaks across the
// boundary) which makes the bug invisible from the UI.
func TestFundHandlerInvertedRangeRejected(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"trades", "/api/funds/fund-1/trades?from=2026-05-22T00:00:00Z&to=2026-05-20T00:00:00Z"},
		{"nav", "/api/funds/fund-1/nav?from=2026-05-22T00:00:00Z&to=2026-05-20T00:00:00Z"},
		{"pnl-attribution", "/api/funds/fund-1/pnl-attribution?from=2026-05-22T00:00:00Z&to=2026-05-20T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// All three handlers share the same stub trade service; we
			// don't install fns so a service call would panic. The
			// validation must reject the request before reaching it.
			handler := NewFundHandler(
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
			mux := http.NewServeMux()
			handler.RegisterRoutes(mux)
			req := authRequest(httptest.NewRequest(http.MethodGet, tc.path, nil), "user-1")
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("%s: expected 400, got %d body=%s", tc.name, rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "'to' must be greater than or equal to 'from'") {
				t.Fatalf("%s: expected detail explaining range, got %s", tc.name, rr.Body.String())
			}
		})
	}
}

var _ = context.Background
