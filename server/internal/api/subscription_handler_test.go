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

type stubSubscriptionService struct {
	listPlansFn        func() []*SubscriptionPlan
	getPlanFn          func(string) (*SubscriptionPlan, error)
	getUserSubFn       func(context.Context, string) (*Subscription, error)
	subscribeFn        func(context.Context, string, string, string) (*Subscription, error)
	cancelFn           func(context.Context, string) error
	getEffectivePlanFn func(context.Context, string) (*SubscriptionPlan, error)
	checkQuotaFn       func(context.Context, string, string, int) error
	checkModelAccessFn func(context.Context, string, string) error
	allowsCustomKeyFn  func(context.Context, string) (bool, error)
}

func (s stubSubscriptionService) ListPlans() []*SubscriptionPlan {
	if s.listPlansFn != nil {
		return s.listPlansFn()
	}
	return nil
}
func (s stubSubscriptionService) GetPlan(tier string) (*SubscriptionPlan, error) {
	if s.getPlanFn != nil {
		return s.getPlanFn(tier)
	}
	return nil, errors.New("unexpected GetPlan call")
}
func (s stubSubscriptionService) GetUserSubscription(ctx context.Context, userID string) (*Subscription, error) {
	if s.getUserSubFn != nil {
		return s.getUserSubFn(ctx, userID)
	}
	return nil, nil
}
func (s stubSubscriptionService) Subscribe(ctx context.Context, userID, tier, paymentMethod string) (*Subscription, error) {
	if s.subscribeFn != nil {
		return s.subscribeFn(ctx, userID, tier, paymentMethod)
	}
	return nil, errors.New("unexpected Subscribe call")
}
func (s stubSubscriptionService) Cancel(ctx context.Context, userID string) error {
	if s.cancelFn != nil {
		return s.cancelFn(ctx, userID)
	}
	return errors.New("unexpected Cancel call")
}
func (s stubSubscriptionService) GetEffectivePlan(ctx context.Context, userID string) (*SubscriptionPlan, error) {
	if s.getEffectivePlanFn != nil {
		return s.getEffectivePlanFn(ctx, userID)
	}
	return nil, nil
}
func (s stubSubscriptionService) CheckQuota(ctx context.Context, userID, action string, currentCount int) error {
	if s.checkQuotaFn != nil {
		return s.checkQuotaFn(ctx, userID, action, currentCount)
	}
	return nil
}
func (s stubSubscriptionService) CheckModelAccess(ctx context.Context, userID, modelTier string) error {
	if s.checkModelAccessFn != nil {
		return s.checkModelAccessFn(ctx, userID, modelTier)
	}
	return nil
}
func (s stubSubscriptionService) AllowsCustomKey(ctx context.Context, userID string) (bool, error) {
	if s.allowsCustomKeyFn != nil {
		return s.allowsCustomKeyFn(ctx, userID)
	}
	return false, nil
}

type stubUsageTracker struct {
	getDailySummaryFn   func(context.Context, string, time.Time) (*DailySummary, error)
	getMonthlySummaryFn func(context.Context, string, string) (*MonthlySummary, error)
	getUsageHistoryFn   func(context.Context, string, int, int) ([]*UsageEntry, int, error)
	getBillFn           func(context.Context, string, string) (*MonthlyBill, error)
}

func (s stubUsageTracker) GetDailySummary(ctx context.Context, userID string, date time.Time) (*DailySummary, error) {
	if s.getDailySummaryFn != nil {
		return s.getDailySummaryFn(ctx, userID, date)
	}
	return nil, nil
}
func (s stubUsageTracker) GetMonthlySummary(ctx context.Context, userID, yearMonth string) (*MonthlySummary, error) {
	if s.getMonthlySummaryFn != nil {
		return s.getMonthlySummaryFn(ctx, userID, yearMonth)
	}
	return nil, nil
}
func (s stubUsageTracker) GetUsageHistory(ctx context.Context, userID string, offset, limit int) ([]*UsageEntry, int, error) {
	if s.getUsageHistoryFn != nil {
		return s.getUsageHistoryFn(ctx, userID, offset, limit)
	}
	return nil, 0, nil
}
func (s stubUsageTracker) GetBill(ctx context.Context, userID, yearMonth string) (*MonthlyBill, error) {
	if s.getBillFn != nil {
		return s.getBillFn(ctx, userID, yearMonth)
	}
	return nil, nil
}

type stubModelConfigService struct {
	saveConfigFn     func(context.Context, *UserModelConfig) error
	getUserConfigsFn func(context.Context, string) ([]*UserModelConfig, error)
	deleteConfigFn   func(context.Context, string, string) error
	testConnectionFn func(context.Context, *UserModelConfig) (*ConnectionTestResult, error)
}

func (s stubModelConfigService) SaveConfig(ctx context.Context, config *UserModelConfig) error {
	if s.saveConfigFn != nil {
		return s.saveConfigFn(ctx, config)
	}
	return nil
}
func (s stubModelConfigService) GetUserConfigs(ctx context.Context, userID string) ([]*UserModelConfig, error) {
	if s.getUserConfigsFn != nil {
		return s.getUserConfigsFn(ctx, userID)
	}
	return nil, nil
}
func (s stubModelConfigService) DeleteConfig(ctx context.Context, userID, configID string) error {
	if s.deleteConfigFn != nil {
		return s.deleteConfigFn(ctx, userID, configID)
	}
	return nil
}
func (s stubModelConfigService) TestConnection(ctx context.Context, config *UserModelConfig) (*ConnectionTestResult, error) {
	if s.testConnectionFn != nil {
		return s.testConnectionFn(ctx, config)
	}
	return nil, nil
}

type stubLLMClient struct {
	listModelsFn func(context.Context) ([]ModelInfo, error)
}

type stubWalletService struct {
	getWalletFn       func(context.Context, string) (*WalletAccount, error)
	listWalletLedgerFn func(context.Context, string, int, int) ([]WalletLedgerEntry, int, error)
}

func (s stubLLMClient) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if s.listModelsFn != nil {
		return s.listModelsFn(ctx)
	}
	return nil, nil
}

func (s stubWalletService) GetWallet(ctx context.Context, userID string) (*WalletAccount, error) {
	if s.getWalletFn != nil {
		return s.getWalletFn(ctx, userID)
	}
	return nil, nil
}

func (s stubWalletService) ListWalletLedger(ctx context.Context, userID string, offset, limit int) ([]WalletLedgerEntry, int, error) {
	if s.listWalletLedgerFn != nil {
		return s.listWalletLedgerFn(ctx, userID, offset, limit)
	}
	return nil, 0, nil
}

func TestSubscriptionHandlerGetSubscriptionIncludesPermissions(t *testing.T) {
	handler := NewSubscriptionHandler(
		stubSubscriptionService{
			getUserSubFn: func(context.Context, string) (*Subscription, error) {
				return &Subscription{UserID: "user-1", PlanTier: "pro"}, nil
			},
			getEffectivePlanFn: func(context.Context, string) (*SubscriptionPlan, error) {
				return &SubscriptionPlan{Tier: "pro", AllowCustomKey: true, AllowABTest: true, AllowExport: true}, nil
			},
		},
		stubUsageTracker{},
		stubModelConfigService{},
		stubLLMClient{},
		stubWalletService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/subscription", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rr.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal subscription response: %v", err)
	}
	permissions, ok := payload["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("expected permissions map, got %#v", payload["permissions"])
	}
	if permissions["allow_custom_key"] != true {
		t.Fatalf("expected allow_custom_key true, got %#v", permissions["allow_custom_key"])
	}
	if permissions["allow_ab_test"] != true {
		t.Fatalf("expected allow_ab_test true, got %#v", permissions["allow_ab_test"])
	}
	if permissions["allow_export"] != true {
		t.Fatalf("expected allow_export true, got %#v", permissions["allow_export"])
	}
}

func TestSubscriptionHandlerSaveModelConfigRejectsCustomKeyWhenDisallowed(t *testing.T) {
	handler := NewSubscriptionHandler(
		stubSubscriptionService{
			allowsCustomKeyFn: func(context.Context, string) (bool, error) {
				return false, nil
			},
		},
		stubUsageTracker{},
		stubModelConfigService{},
		stubLLMClient{},
		stubWalletService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/models/config", bytes.NewBufferString(`{"config_type":"custom_endpoint","provider":"openai","model_name":"gpt-4o","api_key":"secret"}`))
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected status %d, got %d", http.StatusForbidden, rr.Code)
	}
	var payload subErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if payload.Error != "custom model key is not allowed for current subscription" {
		t.Fatalf("unexpected error message: %q", payload.Error)
	}
}

func TestSubscriptionHandlerListModelsIncludesCustomModels(t *testing.T) {
	handler := NewSubscriptionHandler(
		stubSubscriptionService{
			allowsCustomKeyFn: func(context.Context, string) (bool, error) {
				return true, nil
			},
		},
		stubUsageTracker{},
		stubModelConfigService{
			getUserConfigsFn: func(context.Context, string) ([]*UserModelConfig, error) {
				baseURL := "https://example.com/v1"
				return []*UserModelConfig{{
					UserID:     "user-1",
					ConfigType: "custom_endpoint",
					Provider:   "openai",
					ModelName:  "gpt-4.1",
					BaseURL:    &baseURL,
					IsActive:   true,
				}}, nil
			},
		},
		stubLLMClient{
			listModelsFn: func(context.Context) ([]ModelInfo, error) {
				return []ModelInfo{{Provider: "openai", ModelName: "gpt-4o", DisplayName: "GPT-4o", Tier: "critical", Available: true}}, nil
			},
		},
		stubWalletService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rr.Code)
	}
	var payload struct {
		PlatformModels []ModelInfo `json:"platform_models"`
		CustomModels   []ModelInfo `json:"custom_models"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal models response: %v", err)
	}
	if len(payload.PlatformModels) != 1 || payload.PlatformModels[0].ModelName != "gpt-4o" {
		t.Fatalf("unexpected platform models: %#v", payload.PlatformModels)
	}
	if len(payload.CustomModels) != 1 || payload.CustomModels[0].ModelName != "gpt-4.1" {
		t.Fatalf("unexpected custom models: %#v", payload.CustomModels)
	}
	if !payload.CustomModels[0].UsesCustomKey || !payload.CustomModels[0].CustomKeyReady {
		t.Fatalf("expected custom model flags to be true, got %#v", payload.CustomModels[0])
	}
}

func TestSubscriptionHandlerUsageHistoryRejectsInvalidLimit(t *testing.T) {
	handler := NewSubscriptionHandler(stubSubscriptionService{}, stubUsageTracker{}, stubModelConfigService{}, stubLLMClient{}, stubWalletService{})
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/usage/history?limit=101", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rr.Code)
	}
	var payload subErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if payload.Error != "invalid limit parameter (must be 1-100)" {
		t.Fatalf("unexpected error message: %q", payload.Error)
	}
}

func TestSubscriptionHandlerEstimateUsesUsageProjection(t *testing.T) {
	handler := NewSubscriptionHandler(
		stubSubscriptionService{
			getEffectivePlanFn: func(context.Context, string) (*SubscriptionPlan, error) {
				return &SubscriptionPlan{Tier: "pro", PriceCentsMonth: 19900}, nil
			},
		},
		stubUsageTracker{
			getMonthlySummaryFn: func(context.Context, string, string) (*MonthlySummary, error) {
				return &MonthlySummary{YearMonth: time.Now().Format("2006-01"), PriceCents: 3100}, nil
			},
		},
		stubModelConfigService{},
		stubLLMClient{},
		stubWalletService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/usage/estimate", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rr.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal estimate response: %v", err)
	}
	if payload["plan_tier"] != "pro" {
		t.Fatalf("expected plan tier %q, got %#v", "pro", payload["plan_tier"])
	}
	if payload["subscription_fee"] != float64(19900) {
		t.Fatalf("expected subscription fee %#v, got %#v", float64(19900), payload["subscription_fee"])
	}
	if payload["estimated_total"] == nil {
		t.Fatal("expected estimated_total field")
	}
}

func TestSubscriptionHandlerGetWallet(t *testing.T) {
	handler := NewSubscriptionHandler(
		stubSubscriptionService{},
		stubUsageTracker{},
		stubModelConfigService{},
		stubLLMClient{},
		stubWalletService{
			getWalletFn: func(context.Context, string) (*WalletAccount, error) {
				return &WalletAccount{UserID: "user-1", BaseCurrency: "USD", BalanceMinor: 12345}, nil
			},
		},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/wallet", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rr.Code)
	}
	var payload struct {
		Wallet WalletAccount `json:"wallet"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal wallet response: %v", err)
	}
	if payload.Wallet.BalanceMinor != 12345 {
		t.Fatalf("expected balance 12345, got %d", payload.Wallet.BalanceMinor)
	}
}

func TestSubscriptionHandlerWalletLedgerRejectsInvalidLimit(t *testing.T) {
	handler := NewSubscriptionHandler(
		stubSubscriptionService{},
		stubUsageTracker{},
		stubModelConfigService{},
		stubLLMClient{},
		stubWalletService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/wallet/ledger?limit=101", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rr.Code)
	}
}

func TestSubscriptionHandlerWalletLedgerReturnsEntries(t *testing.T) {
	handler := NewSubscriptionHandler(
		stubSubscriptionService{},
		stubUsageTracker{},
		stubModelConfigService{},
		stubLLMClient{},
		stubWalletService{
			listWalletLedgerFn: func(context.Context, string, int, int) ([]WalletLedgerEntry, int, error) {
				refType := "admin_recharge"
				return []WalletLedgerEntry{{EntryType: "recharge", AmountMinor: 5000, Currency: "USD", ReferenceType: &refType}}, 1, nil
			},
		},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/wallet/ledger?limit=20", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rr.Code)
	}
	var payload struct {
		Entries []WalletLedgerEntry `json:"entries"`
		Total   int                 `json:"total"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal ledger response: %v", err)
	}
	if payload.Total != 1 || len(payload.Entries) != 1 {
		t.Fatalf("unexpected ledger payload: %#v", payload)
	}
	if payload.Entries[0].EntryType != "recharge" {
		t.Fatalf("expected recharge entry, got %#v", payload.Entries[0])
	}
}
