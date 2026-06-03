package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/agentreputation"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/brinson"
	"github.com/fundai/server/internal/factorexposure"
	"github.com/fundai/server/internal/stress"
	"github.com/fundai/server/internal/marketdata"
	"github.com/fundai/server/internal/marketimpact"
	"github.com/fundai/server/internal/lockup"
	"github.com/fundai/server/internal/modelab"
	"github.com/fundai/server/internal/securitiesborrow"
	"github.com/fundai/server/internal/quota"
	"github.com/fundai/server/internal/quotecache"
	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/subscription"
	"github.com/fundai/server/internal/wsfeed"
)

const adminRoleSuperAdmin = "super_admin"

type adminHandler struct {
	db                  *sql.DB
	subscriptionService *subscription.SubscriptionService
	budgetService       *subscription.BudgetService
	walletRepo          *repository.WalletRepo
	auditLogger         audit.Logger
	marketDataService   *marketdata.Service

	// scheduler exposes the in-process fund workflow scheduler snapshot
	// and a force-trigger entry point. May be nil in tests that don't
	// wire workflow services.
	scheduler schedulerInspector
	// workflowService backs the manual-trigger endpoint. Decoupled from
	// scheduler so unit tests can stub each independently.
	workflowService workflowAdminTrigger

	// dualControl coordinates super_admin × 2 approval flow (F27).
	// Nil only in skeletal test wiring; production always sets it.
	dualControl *audit.DualControlService

	// quotaService enforces per-fund resource caps (F28). Nil-safe;
	// when absent, /api/admin/quotas/... endpoints return 503.
	quotaService *quota.Service

	// skillInbox 是 Sprint 3 / M5 跨基金技能审批 inbox 的 backend。
	// nil 时对应端点 503。
	skillInbox *skillInbox

	// corpActionRepo backs POST /api/admin/corp-actions and the
	// per-fund timeline endpoint. Nil → corp-action endpoints return
	// 503; tests that don't need them can leave it unset.
	corpActionRepo *repository.CorpActionRepo

	// metrics surfaces lifecycle counters for admin-driven flows
	// (funding-request approve/reject, broker-link approve, etc.).
	// Nil-safe in tests; production wires it via newAdminHandler.
	metrics *serverMetrics

	// marketImpact* are the S6.2 admin shims. Repo is the DB
	// surface; cache is what the admin upsert / delete handlers
	// invalidate so the simulator picks up changes immediately;
	// adapter is the EstimateForProbe source the preview
	// endpoint calls. All three may be nil in tests, in which
	// case the corresponding endpoints return 503.
	marketImpactRepo    *marketimpact.Repo
	marketImpactCache   *marketimpact.Cache
	marketImpactAdapter *marketimpact.SlippageAdapter

	// lockupRepo backs the S6.3 IPO / private-placement lock-up
	// admin endpoints. nil → endpoints return 503.
	lockupRepo *lockup.Repo

	// borrowRepo / borrowCache back the S6.4 securities-borrow
	// admin endpoints (rate CRUD, locate audit, accrual ledger).
	// nil → endpoints return 503.
	borrowRepo  *securitiesborrow.Repo
	borrowCache *securitiesborrow.Cache

	// factorExposureRepo backs the S7 / P3-1 instrument factor
	// loading admin endpoints (calibration CRUD). nil → endpoints
	// return 503 via the registration short-circuit.
	factorExposureRepo *factorexposure.Repo

	// stressRepo backs the S7 / P3-3 admin-managed stress
	// scenarios library. nil → registration short-circuits and
	// the admin / per-fund stress endpoints respond 503.
	stressRepo *stress.Repo

	// brinsonRepo backs the S7 / P3-4 Brinson attribution
	// composition library. nil → registration short-circuits and
	// the admin / per-fund Brinson endpoints respond 503.
	brinsonRepo *brinson.Repo

	// agentReputationRepo backs the S8.4 per-agent reputation
	// ledger. nil → admin endpoints short-circuit and the
	// per-fund handler returns 503.
	agentReputationRepo *agentreputation.Repo

	// agentReputationRebuildSink is the rebuild trigger the
	// admin POST endpoint calls. Typically *agentReputationLoop;
	// tests can plug a stub. nil → rebuild returns 503.
	agentReputationRebuildSink agentReputationRebuildSink

	// workflowCheckpointRepo + workflowCheckpointResumeSink back
	// the S9.2 per-step admin view + resume endpoints. Both nil
	// → endpoints stay unregistered (production routing simply
	// 404s when the feature isn't wired).
	workflowCheckpointRepo       *repository.WorkflowCheckpointRepo
	workflowCheckpointResumeSink workflowCheckpointResumeSink

	// wsFeedManager / wsFeedCache / wsFeedBridge back the S6.5
	// WebSocket-real-time market-data admin endpoints
	// (connection status, current subscriptions, cache stats,
	// force reconnect, manual subscribe). nil → endpoints
	// return 503.
	wsFeedManager *wsfeed.Manager
	wsFeedCache   *quotecache.Cache
	wsFeedBridge  *wsFeedSubscriptionBridge

	// modelABRepo + modelABReporter + modelABResolver back the
	// Sprint 10.3 / 10.4 admin endpoints (list, get, report,
	// create, set-status). All nil-safe; when unwired the
	// modelab routes simply stay unregistered, mirroring the
	// pattern used by other admin sub-modules.
	modelABRepo     *modelab.Repo
	modelABReporter *modelab.Reporter
	modelABResolver *modelab.Resolver

	// llmHealthRepo backs the Sprint 11.4 LLM-health admin
	// dashboard (decision_source / fallback_reason aggregates).
	// Nil-safe; when unwired the llm-health routes stay
	// unregistered.
	llmHealthRepo *repository.LLMHealthRepo

	// alertEventRepo backs the Sprint 12.2 alertmanager webhook
	// receiver + admin acknowledgement flow. Nil-safe.
	alertEventRepo *repository.AlertEventRepo

	// modelABPromotionDraftRepo + modelABPromotionScanLoop back
	// the Sprint 13.3 promotion endpoints. Nil-safe.
	modelABPromotionDraftRepo *modelab.DraftRepo
	modelABPromotionScanLoop  *promotionScanLoop
}

// adminSuperAdminChecker implements audit.SuperAdminChecker by reading
// the users table. We avoid caching: super_admin promotion/demotion is
// rare but security-critical, so a per-call read is the safe default.
type adminSuperAdminChecker struct {
	db *sql.DB
}

func (c *adminSuperAdminChecker) IsSuperAdmin(ctx context.Context, userID string) (bool, error) {
	if c == nil || c.db == nil || strings.TrimSpace(userID) == "" {
		return false, nil
	}
	var role string
	err := c.db.QueryRowContext(ctx,
		`SELECT role FROM users WHERE id=$1 AND deleted_at IS NULL`, userID,
	).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(role) == adminRoleSuperAdmin, nil
}

// schedulerInspector is the narrow read-only contract the admin handler
// needs from the fund workflow scheduler. fundWorkflowScheduler
// satisfies it; tests can plug in a stub.
type schedulerInspector interface {
	Snapshot() FundSchedulerSnapshot
}

// workflowAdminTrigger is the narrow contract the admin handler needs
// to manually kick a fund into running. Implemented by
// workflowServiceAdapter via adminTriggerWorkflow below.
type workflowAdminTrigger interface {
	AdminTriggerFund(ctx contextLike, fundID string, tradingDate time.Time) (*adminTriggerResult, error)
}

// contextLike is a tiny alias used only so the interface can be defined
// in this file without importing context here (the concrete impl uses
// context.Context anyway). Kept as an interface{} sink to make stubs
// trivial — the production path always passes a real context.
type contextLike = interface{}

// adminTriggerResult is the response shape for the manual-trigger
// endpoint. State / fundID are echoed back so the operator can match
// the result against the dashboard snapshot row.
type adminTriggerResult struct {
	FundID      string `json:"fundId"`
	TradingDate string `json:"tradingDate"`
	State       string `json:"state"`
	Step        string `json:"step,omitempty"`
}

type adminUserSummary struct {
	ID          string                `json:"id"`
	Email       string                `json:"email"`
	DisplayName string                `json:"display_name"`
	Role        string                `json:"role"`
	CreatedAt   time.Time             `json:"created_at"`
	Companies   []adminCompanySummary `json:"companies"`
}

type adminCompanySummary struct {
	ID          string             `json:"id"`
	OwnerUserID string             `json:"owner_user_id"`
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	CreatedAt   time.Time          `json:"created_at"`
	UpdatedAt   time.Time          `json:"updated_at"`
	Funds       []adminFundSummary `json:"funds"`
}

type adminFundSummary struct {
	ID               string                `json:"id"`
	CompanyID        string                `json:"company_id"`
	Name             string                `json:"name"`
	Description      string                `json:"description,omitempty"`
	TradingMode      string                `json:"trading_mode"`
	Status           string                `json:"status"`
	TotalAssets      float64               `json:"total_assets"`
	NAV              float64               `json:"nav"`
	Market           string                `json:"market,omitempty"`
	Exchange         string                `json:"exchange,omitempty"`
	AssetClass       string                `json:"asset_class,omitempty"`
	BaseCurrency     string                `json:"base_currency,omitempty"`
	BenchmarkSymbol  string                `json:"benchmark_symbol,omitempty"`
	PrimaryDirection string                `json:"primary_direction,omitempty"`
	CreatedAt        time.Time             `json:"created_at"`
	UpdatedAt        time.Time             `json:"updated_at"`
	Team             []adminTeamMemberInfo `json:"team"`
}

type adminTeamMemberInfo struct {
	AgentID       string    `json:"agent_id"`
	MemberID      string    `json:"member_id"`
	Name          string    `json:"name,omitempty"`
	Role          string    `json:"role"`
	Focus         string    `json:"focus,omitempty"`
	Status        string    `json:"status"`
	JoinedAt      time.Time `json:"joined_at"`
	ModelProvider string    `json:"model_provider,omitempty"`
	ModelName     string    `json:"model_name,omitempty"`
}

type adminOverviewResponse struct {
	Users []adminUserSummary `json:"users"`
}

func newAdminHandler(svc *Services) *adminHandler {
	if svc == nil || svc.DB == nil || svc.SubscriptionService == nil {
		return nil
	}
	auditLogger := audit.NewDBLogger(svc.DB)
	h := &adminHandler{
		db:                  svc.DB,
		subscriptionService: svc.SubscriptionService,
		budgetService:       subscription.NewBudgetService(svc.DB),
		walletRepo:          repository.NewWalletRepo(svc.DB),
		auditLogger:         auditLogger,
		marketDataService:   svc.MarketDataService,
		dualControl: audit.NewDualControlService(
			svc.DB,
			auditLogger,
			&adminSuperAdminChecker{db: svc.DB},
			0, // use default 24h TTL
		),
		quotaService: quota.NewService(svc.DB),
		metrics:      svc.Metrics,

		marketImpactRepo:    svc.MarketImpactRepo,
		marketImpactCache:   svc.MarketImpactCache,
		marketImpactAdapter: svc.MarketImpactAdapter,

		lockupRepo: svc.LockupRepo,

		borrowRepo:  svc.BorrowRepo,
		borrowCache: svc.BorrowCache,

		factorExposureRepo: svc.FactorExposureRepo,

		stressRepo: svc.StressRepo,

		brinsonRepo: svc.BrinsonRepo,

		agentReputationRepo: svc.AgentReputationRepo,

		workflowCheckpointRepo: svc.WorkflowCheckpointRepo,

		wsFeedManager: svc.WSFeedManager,
		wsFeedCache:   svc.WSFeedCache,
		wsFeedBridge:  svc.WSFeedBridge,

		modelABRepo:     svc.ModelABRepo,
		modelABReporter: svc.ModelABReporter,
		llmHealthRepo:   svc.LLMHealthRepo,
		alertEventRepo:  svc.AlertEventRepo,
		modelABPromotionDraftRepo: svc.ModelABPromotionDraftRepo,
		modelABPromotionScanLoop:  svc.ModelABPromotionScanLoop,
	}
	if svc.LLMRuntime != nil {
		h.modelABResolver = svc.LLMRuntime.ModelABResolver()
	}
	if svc.WorkflowService != nil {
		if svc.WorkflowService.scheduler != nil {
			h.scheduler = svc.WorkflowService.scheduler
		}
		h.workflowService = svc.WorkflowService
	}
	// S8.4 — typed-nil safe wiring for the rebuild sink. The
	// interface field stays a true Go nil when the loop is
	// absent, so the 503 fast-path in the admin handler works.
	if svc.AgentReputationLoop != nil {
		h.agentReputationRebuildSink = svc.AgentReputationLoop
	}
	// S9.2 — typed-nil safe wiring for the resume sink. The
	// adapter forwards into the workflowService's existing
	// TriggerStep path so a resume is just "re-fire the same
	// step under admin authority". Absent workflow service →
	// resume endpoint returns 503.
	if svc.WorkflowService != nil {
		h.workflowCheckpointResumeSink = newWorkflowCheckpointResumeAdapter(svc.WorkflowService)
	}
	// Wire Sprint 3 / M5 skill inbox if both DB and mailer are
	// configured. Without mailer we still expose list + shadow-eval;
	// only the auto-approve email notification degrades to a slog
	// line, which matches the rest of the platform's mailer pattern.
	h.skillInbox = newSkillInbox(svc.DB, svc.Mailer, "FundAI", "")
	// Sprint 4 / corp-action: ledger + applier shared across
	// admin + scheduled ingest. Repository is cheap to construct
	// (just wraps *sql.DB) so we always wire it.
	h.corpActionRepo = repository.NewCorpActionRepo(svc.DB)
	h.registerDualControlActions()
	return h
}

func (h *adminHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/overview", h.handleOverview)
	mux.HandleFunc("GET /api/admin/platform-settings", h.handleGetPlatformSettings)
	mux.HandleFunc("PUT /api/admin/platform-settings", h.handleUpdatePlatformSettings)
	mux.HandleFunc("POST /api/admin/wallets/recharge", h.handleRechargeWallet)
	mux.HandleFunc("GET /api/admin/kyc-applications", h.handleListKYCApplications)
	mux.HandleFunc("POST /api/admin/kyc-applications/{id}/decision", h.handleDecideKYCApplication)
	mux.HandleFunc("GET /api/admin/marketdata/health", h.handleMarketDataHealth)
	// F7 — workflow scheduler observability + manual override.
	mux.HandleFunc("GET /api/admin/workflow/scheduler", h.handleSchedulerSnapshot)
	mux.HandleFunc("POST /api/admin/workflow/scheduler/trigger/{fundId}", h.handleSchedulerTrigger)
	// F14 — LLM dollar budget management.
	mux.HandleFunc("GET /api/admin/llm-budgets/{userId}", h.handleListLLMBudgets)
	mux.HandleFunc("PUT /api/admin/llm-budgets/{userId}", h.handleUpsertLLMBudget)
	mux.HandleFunc("DELETE /api/admin/llm-budgets/{userId}", h.handleDeleteLLMBudget)
	// Backward compatible alias retained for existing clients. New clients should use /decision.
	mux.HandleFunc("POST /api/admin/kyc-applications/{id}/approve", h.handleApproveKYCApplication)
	// F27 — super_admin dual-control + behaviour audit.
	h.registerDualControlRoutes(mux)
	// F28 — per-fund quota administration.
	h.registerQuotaRoutes(mux)
	// Sprint 3 / M5 — skill inbox endpoints.
	mux.HandleFunc("GET /api/admin/skills/proposed", h.handleListProposedSkills)
	mux.HandleFunc("POST /api/admin/skills/{fundId}/{skillKey}/shadow-evaluate", h.handleShadowEvaluateSkill)
	mux.HandleFunc("POST /api/admin/skills/{fundId}/{skillKey}/approve", h.handleManualApproveSkill)
	// Sprint 4 / corp-action — apply a split / dividend to one or
	// more fund holdings, and read back the per-fund timeline.
	mux.HandleFunc("POST /api/admin/corp-actions", h.handleApplyCorpAction)
	mux.HandleFunc("GET /api/admin/funds/{fundId}/corp-actions", h.handleListCorpActionsForFund)
	// P1-6 — broker-link 4-eye approval routes.
	h.registerBrokerLinkAdminRoutes(mux)
	h.registerFundingAdminRoutes(mux)
	h.registerFXAdminRoutes(mux)
	h.registerReconAdminRoutes(mux)
	h.registerSurveillanceAdminRoutes(mux)
	h.registerDrawdownAdminRoutes(mux)
	h.registerMarketStatusAdminRoutes(mux)
	h.registerMarketImpactAdminRoutes(mux)
	h.registerLockupAdminRoutes(mux)
	h.registerBorrowAdminRoutes(mux)
	h.registerWSFeedAdminRoutes(mux)
	// S7 / P3-1 — instrument factor-loading calibration store.
	h.registerFactorExposureAdminRoutes(mux)
	// S7 / P3-3 — admin-managed stress scenarios library.
	h.registerStressAdminRoutes(mux)
	// S7 / P3-4 — Brinson benchmark composition library.
	h.registerBrinsonAdminRoutes(mux)
	// S8.4 — per-agent reputation ledger admin view + rebuild.
	h.registerAgentReputationAdminRoutes(mux)
	// S9.2 — per-step workflow checkpoint timeline + resume.
	h.registerWorkflowCheckpointAdminRoutes(mux)
	// S10.3 / 10.4 — model A/B experiment list / report / CRUD.
	h.registerModelABAdminRoutes(mux)
	h.registerLLMHealthAdminRoutes(mux)
	h.registerAlertAdminRoutes(mux)
	h.registerModelABPromotionRoutes(mux)
}

// handleListProposedSkills implements GET /api/admin/skills/proposed.
// Optional query param ageMinHours filters out anything proposed within
// the last N hours so the operator can focus on "stuck" candidates.
func (h *adminHandler) handleListProposedSkills(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.skillInbox == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "skill inbox unavailable"})
		return
	}
	ageMinHours := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("ageMinHours")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			ageMinHours = parsed
		}
	}
	resp, err := h.skillInbox.ListProposed(r.Context(), ageMinHours)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleShadowEvaluateSkill implements POST /api/admin/skills/{fundId}/{skillKey}/shadow-evaluate.
// Runs the factorlab simulator and returns the metrics; the same call
// may auto-approve the skill when the run history clears the threshold.
func (h *adminHandler) handleShadowEvaluateSkill(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.skillInbox == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "skill inbox unavailable"})
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	skillKey := strings.TrimSpace(r.PathValue("skillKey"))
	if fundID == "" || skillKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "fundId and skillKey are required"})
		return
	}
	resp, err := h.skillInbox.ShadowEvaluate(r.Context(), fundID, skillKey)
	if err != nil {
		if errors.Is(err, api.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "skill not found in fund"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleManualApproveSkill is the operator escape-hatch: when the
// human admin is satisfied without waiting for 3 cleared shadow runs,
// they can flip the proposed skill to approved with one call. We reuse
// the same idempotent autoApprove pathway, so re-firing the call is a
// no-op.
func (h *adminHandler) handleManualApproveSkill(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.skillInbox == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "skill inbox unavailable"})
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	skillKey := strings.TrimSpace(r.PathValue("skillKey"))
	if fundID == "" || skillKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "fundId and skillKey are required"})
		return
	}
	if err := h.skillInbox.autoApprove(r.Context(), fundID, skillKey); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "fundId": fundID, "skillKey": skillKey})
}

// handleSchedulerSnapshot exposes the latest scheduler tick so operators
// can answer "is the daily workflow alive, when does the next fund fire,
// and which funds have errors right now?" without grepping pod logs.
//
// Returns 503 if the scheduler hasn't been wired (single-binary tests).
// The payload is always a JSON object — even when no poll has happened
// yet — so the frontend can render a static "scheduler not polled yet"
// state instead of branching on missing fields.
func (h *adminHandler) handleSchedulerSnapshot(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.scheduler == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "workflow scheduler unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, h.scheduler.Snapshot())
}

// handleSchedulerTrigger lets an operator force a fund's daily workflow
// to start right now, even when the calendar wouldn't have triggered it
// yet (e.g. after fixing a config bug mid-day). Internally this is the
// same code-path the scheduler uses, with forceImmediate=true, so the
// workflow_run row is still claimed transactionally and double-fires
// are impossible.
//
// Optional body: { "tradingDate": "2026-05-19" }. When omitted the
// handler defaults to today in UTC (matches the scheduler's behaviour
// when it ticks).
func (h *adminHandler) handleSchedulerTrigger(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.workflowService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "workflow service unavailable"})
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "fundId is required"})
		return
	}
	defer r.Body.Close()
	var payload struct {
		TradingDate string `json:"tradingDate"`
	}
	// Empty body is fine — decode errors are tolerated so callers can
	// trigger with no body at all.
	_ = json.NewDecoder(r.Body).Decode(&payload)
	tradingDate := time.Now().UTC()
	if trimmed := strings.TrimSpace(payload.TradingDate); trimmed != "" {
		parsed, err := time.Parse("2006-01-02", trimmed)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid tradingDate", "detail": "must be YYYY-MM-DD"})
			return
		}
		tradingDate = parsed
	}
	result, err := h.workflowService.AdminTriggerFund(r.Context(), fundID, tradingDate)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to trigger workflow", "detail": err.Error()})
		return
	}
	adminUserID, _ := api.AuthenticatedUserID(r)
	safeAuditLogAccess(r.Context(), h.auditLogger, adminUserID, "trigger", "workflow", fundID, map[string]any{
		"trading_date": result.TradingDate,
		"state":        result.State,
	})
	writeJSON(w, http.StatusOK, result)
}

// handleMarketDataHealth returns the current per-provider counters tracked by
// the marketdata service. Used by the platform ops team to spot outages
// before users open tickets. Limited to super-admins because the payload
// reveals provider names and last-error strings.
func (h *adminHandler) handleMarketDataHealth(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.marketDataService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "market data service unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": h.marketDataService.ProviderHealth(),
	})
}

func (h *adminHandler) handleOverview(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}

	users, err := h.loadOverview(r)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to load admin overview", "detail": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, adminOverviewResponse{Users: users})
}

func (h *adminHandler) handleGetPlatformSettings(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	settings, err := h.subscriptionService.GetPlatformSettings(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to load platform settings", "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (h *adminHandler) handleUpdatePlatformSettings(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	defer r.Body.Close()

	var payload struct {
		AccessMode                 string `json:"access_mode"`
		DefaultTeamIntervalMinutes int    `json:"default_team_interval_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body", "detail": "请求体必须是合法 JSON。"})
		return
	}

	settings, err := h.subscriptionService.UpdatePlatformSettings(r.Context(), &subscription.PlatformSettings{
		AccessMode:                 payload.AccessMode,
		DefaultTeamIntervalMinutes: payload.DefaultTeamIntervalMinutes,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to update platform settings", "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (h *adminHandler) handleRechargeWallet(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.walletRepo == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "wallet service unavailable"})
		return
	}
	defer r.Body.Close()

	adminUserID, _ := api.AuthenticatedUserID(r)
	var payload struct {
		UserID      string         `json:"user_id"`
		AmountMinor int64          `json:"amount_minor"`
		Currency    string         `json:"currency"`
		ReferenceID string         `json:"reference_id"`
		Note        string         `json:"note"`
		Metadata    map[string]any `json:"metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body", "detail": "请求体必须是合法 JSON。"})
		return
	}
	if strings.TrimSpace(payload.UserID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "user_id is required"})
		return
	}
	if payload.AmountMinor <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "amount_minor must be positive"})
		return
	}
	currency := strings.ToUpper(strings.TrimSpace(payload.Currency))
	if currency == "" {
		currency = "USD"
	}
	if currency != "USD" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "currency must be USD"})
		return
	}

	// Check user's KYC status before allowing recharge
	targetUser, err := loadActiveUserByID(r.Context(), h.db, payload.UserID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid user_id", "detail": "User not found or inactive"})
		return
	}
	if targetUser.KYCStatus != "verified" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "kyc_required", "detail": "目标用户尚未通过实名认证 (KYC)，无法充值。"})
		return
	}

	metadata, err := buildRechargeMetadata(payload.Note, payload.Metadata)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid metadata", "detail": err.Error()})
		return
	}
	account, entry, err := h.walletRepo.Credit(r.Context(), repository.WalletCreditParams{
		UserID:          strings.TrimSpace(payload.UserID),
		AmountMinor:     payload.AmountMinor,
		Currency:        currency,
		ReferenceType:   "admin_recharge",
		ReferenceID:     strings.TrimSpace(payload.ReferenceID),
		CreatedByUserID: adminUserID,
		Metadata:        metadata,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to recharge wallet", "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"wallet":  convertWalletAccount(account),
		"entry":   convertWalletLedgerEntry(entry),
		"message": "wallet recharged",
	})
}

func requireSuperAdmin(w http.ResponseWriter, r *http.Request) bool {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized", "detail": "missing or invalid bearer token"})
		return false
	}
	role, ok := api.AuthenticatedUserRole(r)
	if !ok || strings.TrimSpace(role) != adminRoleSuperAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden", "detail": "super admin access required"})
		return false
	}
	return true
}

func (h *adminHandler) loadOverview(r *http.Request) ([]adminUserSummary, error) {
	users, err := h.listUsers(r)
	if err != nil {
		return nil, err
	}
	companyRepo := repository.NewFundCompanyRepo(h.db)
	fundRepo := repository.NewFundRepo(h.db)
	teamRepo := repository.NewTeamRepo(h.db)
	agentRepo := repository.NewAgentRepo(h.db)

	result := make([]adminUserSummary, 0, len(users))
	for _, user := range users {
		companies, err := companyRepo.ListByOwner(r.Context(), user.ID)
		if err != nil {
			return nil, err
		}
		companySummaries := make([]adminCompanySummary, 0, len(companies))
		for _, company := range companies {
			funds, err := fundRepo.ListByCompany(r.Context(), company.ID)
			if err != nil {
				return nil, err
			}
			fundSummaries := make([]adminFundSummary, 0, len(funds))
			for _, fund := range funds {
				teamMembers, err := teamRepo.ListByFund(r.Context(), fund.ID)
				if err != nil {
					return nil, err
				}
				team := make([]adminTeamMemberInfo, 0, len(teamMembers))
				for _, member := range teamMembers {
					agent, err := agentRepo.GetByID(r.Context(), member.AgentID)
					if err != nil && !errors.Is(err, repository.ErrNotFound) {
						return nil, err
					}
					team = append(team, adminTeamMemberInfo{
						AgentID:       member.AgentID,
						MemberID:      member.ID,
						Name:          agentName(agent),
						Role:          member.Role,
						Focus:         member.Focus.String,
						Status:        member.Status,
						JoinedAt:      member.JoinedAt,
						ModelProvider: agentNullableString(agent, func(a *repository.Agent) sql.NullString { return a.ModelProvider }),
						ModelName:     agentNullableString(agent, func(a *repository.Agent) sql.NullString { return a.ModelName }),
					})
				}
				sort.Slice(team, func(i, j int) bool {
					if team[i].Role == team[j].Role {
						return team[i].JoinedAt.Before(team[j].JoinedAt)
					}
					return team[i].Role < team[j].Role
				})
				profile := decodeFundMarketProfile(fund.Config)
				fundSummaries = append(fundSummaries, adminFundSummary{
					ID:               fund.ID,
					CompanyID:        fund.CompanyID,
					Name:             fund.Name,
					Description:      fund.Description.String,
					TradingMode:      fund.TradingMode,
					Status:           fund.Status,
					TotalAssets:      fund.TotalAssets,
					NAV:              fund.NAV,
					Market:           profile.Market,
					Exchange:         profile.Exchange,
					AssetClass:       profile.AssetClass,
					BaseCurrency:     profile.BaseCurrency,
					BenchmarkSymbol:  profile.BenchmarkSymbol,
					PrimaryDirection: profile.PrimaryDirection,
					CreatedAt:        fund.CreatedAt,
					UpdatedAt:        fund.UpdatedAt,
					Team:             team,
				})
			}
			companySummaries = append(companySummaries, adminCompanySummary{
				ID:          company.ID,
				OwnerUserID: company.OwnerUserID,
				Name:        company.Name,
				Description: company.Description.String,
				CreatedAt:   company.CreatedAt,
				UpdatedAt:   company.UpdatedAt,
				Funds:       fundSummaries,
			})
		}
		result = append(result, adminUserSummary{
			ID:          user.ID,
			Email:       user.Email,
			DisplayName: user.DisplayName,
			Role:        user.Role,
			CreatedAt:   user.CreatedAt,
			Companies:   companySummaries,
		})
	}
	return result, nil
}

type adminUserRecord struct {
	ID          string
	Email       string
	DisplayName string
	Role        string
	CreatedAt   time.Time
}

func (h *adminHandler) listUsers(r *http.Request) ([]adminUserRecord, error) {
	rows, err := h.db.QueryContext(r.Context(), `
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), created_at
		FROM users
		WHERE status = 'active'
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []adminUserRecord
	for rows.Next() {
		var user adminUserRecord
		if err := rows.Scan(&user.ID, &user.Email, &user.DisplayName, &user.Role, &user.CreatedAt); err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

func agentName(agent *repository.Agent) string {
	if agent == nil {
		return ""
	}
	return strings.TrimSpace(agent.Name)
}

func agentNullableString(agent *repository.Agent, pick func(*repository.Agent) sql.NullString) string {
	if agent == nil {
		return ""
	}
	return strings.TrimSpace(pick(agent).String)
}

func buildRechargeMetadata(note string, metadata map[string]any) (json.RawMessage, error) {
	payload := map[string]any{}
	for key, value := range metadata {
		payload[key] = value
	}
	if trimmed := strings.TrimSpace(note); trimmed != "" {
		payload["note"] = trimmed
	}
	if len(payload) == 0 {
		return json.RawMessage(`{}`), nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal recharge metadata: %w", err)
	}
	return json.RawMessage(encoded), nil
}

// --- KYC Admin API ---

type kycApplicationResponse struct {
	ID                string   `json:"id"`
	UserID            string   `json:"user_id"`
	UserEmail         string   `json:"user_email,omitempty"`
	UserDisplayName   string   `json:"user_display_name,omitempty"`
	KYCLevel          string   `json:"kyc_level"`
	Status            string   `json:"status"`
	FullName          string   `json:"full_name"`
	IDDocumentType    string   `json:"id_document_type"`
	IDDocumentNumber  string   `json:"id_document_number"`
	DocumentImageURLs []string `json:"document_image_urls,omitempty"`
	RejectionReason   string   `json:"rejection_reason,omitempty"`
	CreatedAt         string   `json:"created_at"`
	UpdatedAt         string   `json:"updated_at"`
}

func (h *adminHandler) handleListKYCApplications(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}

	status := r.URL.Query().Get("status")
	if status == "" {
		status = "pending"
	}
	limit, offset := parseAdminLimitOffset(r)

	rows, err := h.db.QueryContext(r.Context(), `
		SELECT r.id, r.user_id, COALESCE(u.email, ''), COALESCE(u.display_name, ''), r.kyc_level, r.status,
		       r.full_name, r.id_document_type, r.id_document_number, COALESCE(r.document_image_urls, '[]'::jsonb),
		       COALESCE(r.rejection_reason, ''), r.created_at, r.updated_at
		FROM user_kyc_records r
		LEFT JOIN users u ON u.id = r.user_id
		WHERE r.status = $1
		ORDER BY r.created_at ASC
		LIMIT $2 OFFSET $3
	`, status, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "database error", "detail": err.Error()})
		return
	}
	defer rows.Close()

	var apps []kycApplicationResponse
	for rows.Next() {
		var app kycApplicationResponse
		var ca, ua time.Time
		var documentURLs json.RawMessage
		if err := rows.Scan(&app.ID, &app.UserID, &app.UserEmail, &app.UserDisplayName, &app.KYCLevel, &app.Status, &app.FullName, &app.IDDocumentType, &app.IDDocumentNumber, &documentURLs, &app.RejectionReason, &ca, &ua); err != nil {
			continue
		}
		app.DocumentImageURLs = parseKYCImageURLs(documentURLs)
		app.CreatedAt = ca.Format(time.RFC3339)
		app.UpdatedAt = ua.Format(time.RFC3339)
		apps = append(apps, app)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "database error", "detail": err.Error()})
		return
	}

	if apps == nil {
		apps = make([]kycApplicationResponse, 0)
	}
	adminUserID, _ := api.AuthenticatedUserID(r)
	safeAuditLogAccess(r.Context(), h.auditLogger, adminUserID, "read", "kyc_applications", status, map[string]any{
		"status":       status,
		"limit":        limit,
		"offset":       offset,
		"result_count": len(apps),
	})
	writeJSON(w, http.StatusOK, apps)
}

func parseKYCImageURLs(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil
	}
	filtered := values[:0]
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			filtered = append(filtered, trimmed)
		}
	}
	return filtered
}

func parseAdminLimitOffset(r *http.Request) (int, int) {
	limit := parseAdminIntDefault(r, "limit", 100)
	offset := parseAdminIntDefault(r, "offset", 0)
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func parseAdminIntDefault(r *http.Request, key string, def int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}

func (h *adminHandler) handleApproveKYCApplication(w http.ResponseWriter, r *http.Request) {
	h.handleDecideKYCApplication(w, r)
}

func (h *adminHandler) handleDecideKYCApplication(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}

	appID := r.PathValue("id")
	if appID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id is required"})
		return
	}

	var payload struct {
		Action          string `json:"action"` // "approve" or "reject"
		RejectionReason string `json:"rejection_reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}

	if payload.Action != "approve" && payload.Action != "reject" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "action must be approve or reject"})
		return
	}
	if payload.Action == "reject" && strings.TrimSpace(payload.RejectionReason) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "rejection_reason is required when rejecting"})
		return
	}

	adminUserID, _ := api.AuthenticatedUserID(r)

	tx, err := h.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "transaction start failed"})
		return
	}
	defer tx.Rollback()

	var userID string
	var kycLevel string
	var currentStatus string
	err = tx.QueryRowContext(r.Context(), `SELECT user_id, kyc_level, status FROM user_kyc_records WHERE id = $1 FOR UPDATE`, appID).Scan(&userID, &kycLevel, &currentStatus)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "application not found"})
		return
	}

	if currentStatus != "pending" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "application is not pending"})
		return
	}

	newStatus := "rejected"
	userKYCStatus := "rejected"
	if payload.Action == "approve" {
		newStatus = "approved"
		userKYCStatus = "verified"
	}

	_, err = tx.ExecContext(r.Context(), `
		UPDATE user_kyc_records 
		SET status = $1, rejection_reason = $2, reviewed_by = $3, reviewed_at = NOW(), updated_at = NOW() 
		WHERE id = $4`,
		newStatus, payload.RejectionReason, adminUserID, appID,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to update application"})
		return
	}

	// Only update user's overall status if approved or if they don't have an existing verified status
	if payload.Action == "approve" {
		_, err = tx.ExecContext(r.Context(), `UPDATE users SET kyc_status = $1, kyc_level = $2, updated_at = NOW() WHERE id = $3`, userKYCStatus, kycLevel, userID)
	} else {
		// Only set to rejected if they aren't already verified
		_, err = tx.ExecContext(r.Context(), `UPDATE users SET kyc_status = 'rejected', updated_at = NOW() WHERE id = $1 AND kyc_status != 'verified'`, userID)
	}

	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to update user status"})
		return
	}

	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "transaction commit failed"})
		return
	}

	safeAuditLogAccess(r.Context(), h.auditLogger, adminUserID, payload.Action, "kyc_application", appID, map[string]any{
		"target_user_id":       userID,
		"kyc_level":            kycLevel,
		"previous_status":      currentStatus,
		"new_status":           newStatus,
		"has_rejection_reason": strings.TrimSpace(payload.RejectionReason) != "",
	})

	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "application_id": appID, "new_status": newStatus})
}

// ---------------------------------------------------------------------------
// F14 — LLM dollar budget management
// ---------------------------------------------------------------------------

// llmBudgetRequest is the wire body for PUT /api/admin/llm-budgets/{userId}.
// fundId is optional — when omitted, the row is the user-wide cap that
// applies to any (user, fund) combo without a more-specific row.
//
// Both limit fields use *float64 so the caller can express:
//   - "no cap on this window": send null / omit
//   - "cap = 0": send 0.0 (rare but valid — blocks all spend)
// At least one of the two must be non-nil; otherwise the request is rejected.
type llmBudgetRequest struct {
	FundID            *string  `json:"fundId,omitempty"`
	DailyLimitCents   *float64 `json:"dailyLimitCents,omitempty"`
	MonthlyLimitCents *float64 `json:"monthlyLimitCents,omitempty"`
}

// llmBudgetResponse echoes back the stored row plus a live snapshot of
// current spend in each window. Operators bumping a budget can verify
// the cap and see how much is already consumed before a fund retries.
type llmBudgetResponse struct {
	UserID            string    `json:"userId"`
	FundID            *string   `json:"fundId,omitempty"`
	DailyLimitCents   *float64  `json:"dailyLimitCents,omitempty"`
	MonthlyLimitCents *float64  `json:"monthlyLimitCents,omitempty"`
	DailySpendCents   float64   `json:"dailySpendCents"`
	MonthlySpendCents float64   `json:"monthlySpendCents"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

func (h *adminHandler) handleListLLMBudgets(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.budgetService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "budget service unavailable"})
		return
	}
	userID := strings.TrimSpace(r.PathValue("userId"))
	if userID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing userId"})
		return
	}
	rows, err := h.budgetService.ListByUser(r.Context(), userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "list budgets", "detail": err.Error()})
		return
	}
	out := make([]llmBudgetResponse, 0, len(rows))
	for i := range rows {
		row := rows[i]
		fundID := ""
		if row.FundID != nil {
			fundID = *row.FundID
		}
		snap, err := h.budgetService.Snapshot(r.Context(), userID, fundID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "snapshot", "detail": err.Error()})
			return
		}
		resp := llmBudgetResponse{
			UserID:            row.UserID,
			FundID:            row.FundID,
			DailyLimitCents:   row.DailyLimitCents,
			MonthlyLimitCents: row.MonthlyLimitCents,
			UpdatedAt:         row.UpdatedAt,
		}
		if snap != nil {
			resp.DailySpendCents = snap.DailySpendCents
			resp.MonthlySpendCents = snap.MonthlySpendCents
		}
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, map[string]any{"budgets": out})
}

func (h *adminHandler) handleUpsertLLMBudget(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.budgetService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "budget service unavailable"})
		return
	}
	userID := strings.TrimSpace(r.PathValue("userId"))
	if userID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing userId"})
		return
	}
	var req llmBudgetRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body", "detail": err.Error()})
		return
	}
	fundID := ""
	if req.FundID != nil {
		fundID = strings.TrimSpace(*req.FundID)
	}
	row, err := h.budgetService.UpsertBudget(r.Context(), userID, fundID, req.DailyLimitCents, req.MonthlyLimitCents)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "upsert budget", "detail": err.Error()})
		return
	}
	adminUserID, _ := api.AuthenticatedUserID(r)
	safeAuditLogAccess(r.Context(), h.auditLogger, adminUserID, "llm_budget.upsert", "llm_budget", userID, map[string]any{
		"target_user_id":      userID,
		"fund_id":             fundID,
		"daily_limit_cents":   req.DailyLimitCents,
		"monthly_limit_cents": req.MonthlyLimitCents,
	})
	resp := llmBudgetResponse{
		UserID:            row.UserID,
		FundID:            row.FundID,
		DailyLimitCents:   row.DailyLimitCents,
		MonthlyLimitCents: row.MonthlyLimitCents,
		UpdatedAt:         row.UpdatedAt,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *adminHandler) handleDeleteLLMBudget(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.budgetService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "budget service unavailable"})
		return
	}
	userID := strings.TrimSpace(r.PathValue("userId"))
	if userID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing userId"})
		return
	}
	fundID := strings.TrimSpace(r.URL.Query().Get("fundId"))
	if err := h.budgetService.DeleteBudget(r.Context(), userID, fundID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "delete budget", "detail": err.Error()})
		return
	}
	adminUserID, _ := api.AuthenticatedUserID(r)
	safeAuditLogAccess(r.Context(), h.auditLogger, adminUserID, "llm_budget.delete", "llm_budget", userID, map[string]any{
		"target_user_id": userID,
		"fund_id":        fundID,
	})
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}
