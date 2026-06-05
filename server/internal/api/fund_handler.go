package api

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Position struct {
	InstrumentKey      string   `json:"instrumentKey,omitempty"`
	Symbol             string   `json:"symbol"`
	Name               string   `json:"name,omitempty"`
	Market             string   `json:"market,omitempty"`
	Exchange           string   `json:"exchange,omitempty"`
	AssetClass         string   `json:"assetClass,omitempty"`
	InstrumentType     string   `json:"instrumentType,omitempty"`
	PositionSide       string   `json:"positionSide,omitempty"`
	QuoteCurrency      string   `json:"quoteCurrency,omitempty"`
	SettlementCurrency string   `json:"settlementCurrency,omitempty"`
	MarginMode         string   `json:"marginMode,omitempty"`
	Quantity           float64  `json:"quantity"`
	AvailableQty       float64  `json:"availableQty"`
	CostPrice          float64  `json:"costPrice"`
	CurrentPrice       float64  `json:"currentPrice"`
	MarketValue        float64  `json:"marketValue"`
	Weight             float64  `json:"weight"`
	Leverage           *float64 `json:"leverage,omitempty"`
	ContractMultiplier *float64 `json:"contractMultiplier,omitempty"`
	ExpiryDate         string   `json:"expiryDate,omitempty"`
	UnrealizedPnL      *float64 `json:"unrealizedPnl,omitempty"`
	MarginUsed         *float64 `json:"marginUsed,omitempty"`
	// PriceAsOf is when the displayed CurrentPrice was sampled. Populated
	// when the live-overlay layer succeeds in fetching a fresh quote;
	// otherwise it falls back to the position's last persisted refresh
	// time. Always UTC. Empty string when neither is available.
	PriceAsOf string `json:"priceAsOf,omitempty"`
	// PriceSource is the upstream provider that produced PriceAsOf
	// ("yahoo", "eastmoney", "binance", ...). Empty when the overlay
	// failed and we fell back to the DB-cached value.
	PriceSource string `json:"priceSource,omitempty"`
	// IsStale signals the displayed price is older than the configured
	// staleness threshold (default 15 minutes) and should be rendered
	// with a warning badge in the UI.
	IsStale bool `json:"isStale,omitempty"`
}

type Agent struct {
	ID                    string          `json:"id"`
	AgentID               string          `json:"agentId,omitempty"`
	Name                  string          `json:"name,omitempty"`
	Role                  string          `json:"role"`
	Focus                 string          `json:"focus,omitempty"`
	LLMModel              string          `json:"llmModel,omitempty"`
	ModelProvider         string          `json:"modelProvider,omitempty"`
	ModelName             string          `json:"modelName,omitempty"`
	ModelBaseURL          string          `json:"modelBaseURL,omitempty"`
	HasCustomModelConfig  bool            `json:"hasCustomModelConfig,omitempty"`
	SystemPrompt          string          `json:"systemPrompt,omitempty"`
	SkillConfig           json.RawMessage `json:"skillConfig,omitempty"`
	DomainConfig          json.RawMessage `json:"domainConfig,omitempty"`
	EvolutionConfig       json.RawMessage `json:"evolutionConfig,omitempty"`
	LatestLearningSummary string          `json:"latestLearningSummary,omitempty"`
	LatestLearningAt      time.Time       `json:"latestLearningAt,omitempty"`
	LatestLearningReturn  *float64        `json:"latestLearningReturn,omitempty"`
	LatestLearningTags    []string        `json:"latestLearningTags,omitempty"`
	Status                string          `json:"status,omitempty"`
	JoinedAt              time.Time       `json:"joinedAt,omitempty"`
	FundID                string          `json:"fundId,omitempty"`
	BindStatus            string          `json:"bindStatus,omitempty"`
}

type Plan struct {
	ID                 string          `json:"id"`
	FundID             string          `json:"fundId"`
	TradingDate        string          `json:"tradingDate,omitempty"`
	Status             string          `json:"status"`
	Reasoning          string          `json:"reasoning,omitempty"`
	ReasoningZh        string          `json:"reasoningZh,omitempty"`
	ReasoningEn        string          `json:"reasoningEn,omitempty"`
	RiskScore          *float64        `json:"riskScore,omitempty"`
	ExpectedReturn     *float64        `json:"expectedReturn,omitempty"`
	RiskReview         json.RawMessage `json:"riskReview,omitempty"`
	DiscussionSnapshot json.RawMessage `json:"discussionSnapshot,omitempty"`
	RoundtableID       string          `json:"roundtableId,omitempty"`
	PMAgentID          string          `json:"pmAgentId,omitempty"`
	Actions            []PlanAction    `json:"actions,omitempty"`
	CreatedAt          time.Time       `json:"createdAt"`
	UpdatedAt          time.Time       `json:"updatedAt"`
	// Sprint 11.3 — tiered LLM-failure disclosure.
	//
	// DecisionSource is the bounded enum of provenance tags
	// (llm_pm / llm_three_stage / fallback_no_llm /
	// fallback_after_llm_error / fallback_empty_plan / legacy).
	// Every authenticated user sees this field — knowing whether
	// a plan was AI-derived vs rule-derived is a baseline
	// transparency requirement on a regulated investment surface.
	DecisionSource string `json:"decisionSource,omitempty"`
	// FallbackReason carries the user-facing payload: the
	// category code + an opaque provider/model label suitable
	// for a UI chip. The technical Summary field is NEVER
	// included in this surface and is stripped by the API layer
	// before serialisation. Admins read the full Detail via the
	// separate AdminLLMHealthSection endpoint introduced in
	// Sprint 11.4.
	FallbackReason *PlanFallbackReason `json:"fallbackReason,omitempty"`
}

// PlanFallbackReason is the redacted public projection of
// errorclass.Detail. We intentionally omit the Summary field — the raw
// provider message can include API URLs, model names, or vendor
// internals that should not leak to general users. The Provider field
// is included as a coarse identifier ("openai" / "claude") so the user
// understands which path failed without exposing the specific model
// variant under contract.
type PlanFallbackReason struct {
	Category string `json:"category"`
	Provider string `json:"provider,omitempty"`
	At       string `json:"at,omitempty"`
}

type PlanAction struct {
	ID                 string   `json:"id,omitempty"`
	InstrumentKey      string   `json:"instrumentKey,omitempty"`
	Action             string   `json:"action"`
	Symbol             string   `json:"symbol"`
	Market             string   `json:"market,omitempty"`
	Exchange           string   `json:"exchange,omitempty"`
	AssetClass         string   `json:"assetClass,omitempty"`
	InstrumentType     string   `json:"instrumentType,omitempty"`
	PositionSide       string   `json:"positionSide,omitempty"`
	OpenClose          string   `json:"openClose,omitempty"`
	Quantity           *float64 `json:"quantity,omitempty"`
	Price              *float64 `json:"price,omitempty"`
	Amount             *float64 `json:"amount,omitempty"`
	StopLoss           *float64 `json:"stopLoss,omitempty"`
	TakeProfit         *float64 `json:"takeProfit,omitempty"`
	Reasoning          string   `json:"reasoning,omitempty"`
	ReasoningZh        string   `json:"reasoningZh,omitempty"`
	ReasoningEn        string   `json:"reasoningEn,omitempty"`
	Confidence         *float64 `json:"confidence,omitempty"`
	SupportedBy        []string `json:"supportedBy,omitempty"`
	OpposedBy          []string `json:"opposedBy,omitempty"`
	ExecutionStatus    string   `json:"executionStatus,omitempty"`
	SortOrder          int      `json:"sortOrder"`
	QuoteCurrency      string   `json:"quoteCurrency,omitempty"`
	SettlementCurrency string   `json:"settlementCurrency,omitempty"`
	MarginMode         string   `json:"marginMode,omitempty"`
	Leverage           *float64 `json:"leverage,omitempty"`
	ContractMultiplier *float64 `json:"contractMultiplier,omitempty"`
	ExpiryDate         string   `json:"expiryDate,omitempty"`
	ReduceOnly         *bool    `json:"reduceOnly,omitempty"`
	// QuoteRefreshedAt is RFC 3339 timestamp of the last refresh applied
	// to this action's price (via POST /api/plans/{id}/refresh-quote).
	// Empty string means the action still holds its plan-generation
	// quote; UI uses this to render a "last refreshed N min ago" badge.
	QuoteRefreshedAt string `json:"quoteRefreshedAt,omitempty"`
}

type Trade struct {
	ID                 string    `json:"id"`
	FundID             string    `json:"fundId"`
	PlanID             string    `json:"planId,omitempty"`
	PlanActionID       string    `json:"planActionId,omitempty"`
	InstrumentKey      string    `json:"instrumentKey,omitempty"`
	Symbol             string    `json:"symbol"`
	Market             string    `json:"market,omitempty"`
	Exchange           string    `json:"exchange,omitempty"`
	AssetClass         string    `json:"assetClass,omitempty"`
	InstrumentType     string    `json:"instrumentType,omitempty"`
	Side               string    `json:"side"`
	PositionSide       string    `json:"positionSide,omitempty"`
	OpenClose          string    `json:"openClose,omitempty"`
	OrderType          string    `json:"orderType"`
	Quantity           float64   `json:"quantity"`
	Price              float64   `json:"price,omitempty"`
	Amount             float64   `json:"amount,omitempty"`
	FilledQty          float64   `json:"filledQty"`
	FilledPrice        float64   `json:"filledPrice,omitempty"`
	FeeCommission      float64   `json:"feeCommission"`
	FeeStampTax        float64   `json:"feeStampTax"`
	FeeTransfer        float64   `json:"feeTransfer"`
	TradingMode        string    `json:"tradingMode"`
	BrokerOrderID      string    `json:"brokerOrderId,omitempty"`
	MCPServerID        string    `json:"mcpServerId,omitempty"`
	Status             string    `json:"status"`
	ExecutedAt         time.Time `json:"executedAt,omitempty"`
	QuoteCurrency      string    `json:"quoteCurrency,omitempty"`
	SettlementCurrency string    `json:"settlementCurrency,omitempty"`
	MarginMode         string    `json:"marginMode,omitempty"`
	Leverage           *float64  `json:"leverage,omitempty"`
	ContractMultiplier *float64  `json:"contractMultiplier,omitempty"`
	ExpiryDate         string    `json:"expiryDate,omitempty"`
	ReduceOnly         *bool     `json:"reduceOnly,omitempty"`
	// SlippagePct is the signed fractional drift between the plan
	// reference price (Price) and the actual fill price (FilledPrice).
	// nil means the trade predates the SlippageGuard rollout or was a
	// sell/non-priced fill where the metric isn't meaningful.
	SlippagePct *float64 `json:"slippagePct,omitempty"`
	// Strategy is the execution strategy the PM direct-fill path
	// selected for this row ("immediate" / "limit" / "twap" / "vwap"
	// / "iceberg" / "pov"). Empty for legacy rows (pre-088 migration).
	// All children of a TWAP parent share the parent's value.
	Strategy string `json:"strategy,omitempty"`
	// StrategyParentTradeID points back at the parent trade row
	// when this row is one slice of a multi-child execution
	// (TWAP / VWAP / iceberg / POV). Empty means "this IS the
	// parent" (or this trade pre-dates child splitting). UIs
	// listing trades should hide rows where this is non-empty
	// to show one aggregated parent row per plan_action, then
	// drill down into children on click. Distinct from the
	// existing ParentTradeID bracket-parent pointer (OCO /
	// stop-loss siblings) which is intentionally NOT surfaced
	// in this API today.
	StrategyParentTradeID string    `json:"strategyParentTradeId,omitempty"`
	CreatedAt             time.Time `json:"createdAt"`
}

type FundUniverse struct {
	Mode          string   `json:"mode,omitempty"`
	Symbols       []string `json:"symbols,omitempty"`
	Sectors       []string `json:"sectors,omitempty"`
	Themes        []string `json:"themes,omitempty"`
	CustomFilters []string `json:"customFilters,omitempty"`
}

type FundTeamIntervals struct {
	Researcher *int `json:"researcher,omitempty"`
	PM         *int `json:"pm,omitempty"`
	Risk       *int `json:"risk,omitempty"`
	Trader     *int `json:"trader,omitempty"`
}

type FundTeamSpecialization struct {
	Markets      []string `json:"markets,omitempty"`
	AssetClasses []string `json:"assetClasses,omitempty"`
	Themes       []string `json:"themes,omitempty"`
	Instruments  []string `json:"instruments,omitempty"`
	StyleHints   []string `json:"styleHints,omitempty"`
}

type FundSpecialization struct {
	Team *FundTeamSpecialization `json:"team,omitempty"`
}

type FundHardRiskConfig struct {
	DailyLossLimit        *float64 `json:"dailyLossLimit,omitempty"`
	MaxSinglePosition     *float64 `json:"maxSinglePosition,omitempty"`
	MaxSectorExposure     *float64 `json:"maxSectorExposure,omitempty"`
	MaxTotalExposure      *float64 `json:"maxTotalExposure,omitempty"`
	MaxOrderPctOfAssets   *float64 `json:"maxOrderPctOfAssets,omitempty"`
	MaxOrderAmount        *float64 `json:"maxOrderAmount,omitempty"`
	MaxTradesPerDay       *int     `json:"maxTradesPerDay,omitempty"`
	MaxTradesPerSymbolDay *int     `json:"maxTradesPerSymbolDay,omitempty"`
	// MaxQuoteAgeSeconds is the per-fund staleness threshold for the
	// StaleQuoteGuard rule (see risk.HardRiskConfig.MaxQuoteAge). Valid
	// range is 1..86400 (1 second to 24 hours). Outside that range or
	// nil falls back to the platform default (15 minutes).
	MaxQuoteAgeSeconds *int `json:"maxQuoteAgeSeconds,omitempty"`
}

// FundAutoExecuteConfig controls whether (and under what guardrails) the
// runtimeApprovalGateway is allowed to skip user approval and push a
// generated plan straight to "approved". It is intentionally orthogonal
// to FundHardRiskConfig: hard-risk rules (lot size, T+1 settlement,
// slippage, single-order NAV cap) ALWAYS run, regardless of auto-execute
// state. The guardrails below only decide whether a plan that has
// already passed the regular gates can bypass the human in the loop.
//
// All *float* fields are fractions of NAV (0.05 = 5%). MinConfidence is
// the plan-level confidence floor produced by the LLM PMAgent (Phase
// 2A). SlippageBouncePolicy decides what happens at execution time if
// the live quote drifts beyond the SlippageGuard tolerance — under
// manual mode the plan bounces back to pending_user; under auto-execute
// the user isn't watching, so the operator picks one of three
// behaviours.
type FundAutoExecuteConfig struct {
	Enabled              bool     `json:"enabled"`
	MaxOrderPctOfAssets  *float64 `json:"maxOrderPctOfAssets,omitempty"`  // default 0.05
	MaxDailyPctOfAssets  *float64 `json:"maxDailyPctOfAssets,omitempty"`  // default 0.20
	MinConfidence        *float64 `json:"minConfidence,omitempty"`        // default 0.60
	SlippageBouncePolicy string   `json:"slippageBouncePolicy,omitempty"` // "bounce_to_user" (default) | "reject" | "force_execute"
	// AllowedMarkets restricts auto-execute to a market whitelist
	// (matched against fund.config.market AND each action's market).
	// Empty means "all markets allowed".
	AllowedMarkets []string `json:"allowedMarkets,omitempty"`
	// DecisionIntervalMinutes turns the daily one-shot workflow into a
	// recurring intra-day loop. When non-nil, the scheduler emits a
	// fresh decision run every N minutes inside the fund market's
	// trading windows (auction + regular segments, merged contiguous
	// across the midday break). Example: 30 on A-share → slots at
	// 9:00, 9:30, 10:00, 10:30, 11:00 (morning) + 13:00, 13:30, 14:00,
	// 14:30 (afternoon). nil = legacy one-shot daily trigger
	// (MacroBrief = PreOpen − 30 min). Clamped to
	// [MinDecisionIntervalMinutes, MaxDecisionIntervalMinutes] at the
	// calendar layer so a fat-finger does not produce a 1-minute hot
	// loop or a 30-day silence.
	DecisionIntervalMinutes *int `json:"decisionIntervalMinutes,omitempty"`
}

type Fund struct {
	ID               string              `json:"id"`
	CompanyID        string              `json:"companyId"`
	Name             string              `json:"name"`
	Description      string              `json:"description,omitempty"`
	TradingMode      string              `json:"tradingMode"`
	InitialCapital   float64             `json:"initialCapital"`
	CurrentCapital   float64             `json:"currentCapital"`
	TotalAssets      float64             `json:"totalAssets"`
	NAV              float64             `json:"nav"`
	Status           string              `json:"status"`
	Market           string              `json:"market,omitempty"`
	Exchange         string              `json:"exchange,omitempty"`
	AssetClass       string              `json:"assetClass,omitempty"`
	BaseCurrency     string              `json:"baseCurrency,omitempty"`
	BenchmarkSymbol  string              `json:"benchmarkSymbol,omitempty"`
	PrimaryDirection string              `json:"primaryDirection,omitempty"`
	CalendarCode     string              `json:"calendarCode,omitempty"`
	TimeZone         string              `json:"timeZone,omitempty"`
	Universe         *FundUniverse       `json:"universe,omitempty"`
	TeamIntervals    *FundTeamIntervals  `json:"teamIntervals,omitempty"`
	Specialization   *FundSpecialization `json:"specialization,omitempty"`
	HardRisk         *FundHardRiskConfig `json:"hardRisk,omitempty"`
	// AutoExecute controls per-fund auto-execute behaviour (see
	// FundAutoExecuteConfig). Always populated on reads (server fills
	// in defaults if the persisted config omits it). nil on writes
	// means "leave existing setting unchanged".
	AutoExecute *FundAutoExecuteConfig `json:"autoExecute,omitempty"`
	// ResearchTier selects the roundtable implementation used by the
	// research pool. Empty / "standard" → legacy text-concat consensus
	// (cheap). "advanced" → multi-agent bull/bear/quant debate from
	// internal/debate (more LLM calls, richer dissent signals). Phase 2B.
	ResearchTier string `json:"researchTier,omitempty"`
	// ActivityRetentionDays surfaces the per-fund retention setting
	// (1..10 days). Always populated on Fund reads: a missing or
	// invalid stored value falls back to 7 in normalizeActivityRetentionDays.
	ActivityRetentionDays int `json:"activityRetentionDays,omitempty"`
	CreatedAt        time.Time           `json:"createdAt"`
	UpdatedAt        time.Time           `json:"updatedAt"`
}

// TodayPnL is the dashboard "今日盈亏" payload returned by
// /api/funds/:fundId/today-pnl. It splits today's P&L into the
// pieces a user can reason about (realised vs. unrealised delta)
// and labels the baseline NAV snapshot used for the unrealised
// delta. The labeling matters because a fund whose previous
// trading day didn't snapshot (e.g. weekend or zero-activity day)
// falls back to an older close, which makes the "today" number
// actually span multiple days — the UI should surface that.
type TodayPnL struct {
	FundID string `json:"fundId"`
	// RealisedPnL is the sum of closed_lots.realized_pnl for lots
	// closed since today's local-trading-day start. Already net of
	// entry + exit fees (lotledger nets them into realized_pnl at
	// close time).
	RealisedPnL float64 `json:"realisedPnl"`
	// CurrentUnrealisedPnL is Σ holding_positions.unrealized_pnl
	// across all current open lots, mark-to-market.
	CurrentUnrealisedPnL float64 `json:"currentUnrealisedPnl"`
	// PriorCloseUnrealisedPnL is the unrealised P&L of the position
	// book at the most recent NAV snapshot strictly before today,
	// reconstructed from nav_snapshots.positions_snapshot. Subtract
	// this from CurrentUnrealisedPnL to get today's unrealised
	// delta. Zero when no such snapshot exists.
	PriorCloseUnrealisedPnL float64 `json:"priorCloseUnrealisedPnl"`
	// PriorCloseDate is the trading_date of the NAV snapshot used
	// as the unrealised-delta baseline (empty when none found).
	PriorCloseDate string `json:"priorCloseDate,omitempty"`
	// BaselineFresh is true when PriorCloseDate is yesterday in the
	// fund's local calendar — only then is "today P&L" a strict
	// 1-day delta. False (e.g. weekend gap, missing settle) is a
	// hint the UI can surface.
	BaselineFresh bool `json:"baselineFresh"`
	// TodayPnL is the dashboard-bound number:
	//   = RealisedPnL + (CurrentUnrealisedPnL - PriorCloseUnrealisedPnL)
	// which equals "live total assets - prior close total assets"
	// when no cash deposits/withdrawals happened in between.
	TodayPnL float64 `json:"todayPnl"`
	AsOf     time.Time `json:"asOf"`
}

type ForwardGateStatus struct {
	FundID       string                   `json:"fundId"`
	Status       string                   `json:"status"`
	Eligible     bool                     `json:"eligible"`
	Mode         string                   `json:"mode,omitempty"`
	Summary      string                   `json:"summary,omitempty"`
	RequiredDays int                      `json:"requiredDays"`
	LiveDays     int                      `json:"liveDays"`
	RequiredNAVs int                      `json:"requiredNavs"`
	NAVPoints    int                      `json:"navPoints"`
	StartDate    string                   `json:"startDate,omitempty"`
	EndDate      string                   `json:"endDate,omitempty"`
	TrackRecord  *ForwardTrackRecord      `json:"trackRecord,omitempty"`
	Checks       []ForwardGateCheck       `json:"checks,omitempty"`
	Agents       []ForwardAgentGateStatus `json:"agents,omitempty"`
	GeneratedAt  time.Time                `json:"generatedAt"`
}

type ForwardTrackRecord struct {
	TotalReturn  float64 `json:"totalReturn"`
	AnnualReturn float64 `json:"annualReturn"`
	Sharpe       float64 `json:"sharpe"`
	MaxDrawdown  float64 `json:"maxDrawdown"`
	Volatility   float64 `json:"volatility"`
	WinRate      float64 `json:"winRate"`
}

type ForwardGateCheck struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Status   string `json:"status"`
	Required int    `json:"required,omitempty"`
	Current  int    `json:"current,omitempty"`
	Message  string `json:"message,omitempty"`
}

type ForwardAgentGateStatus struct {
	AgentID   string             `json:"agentId"`
	AgentName string             `json:"agentName,omitempty"`
	Role      string             `json:"role,omitempty"`
	Focus     string             `json:"focus,omitempty"`
	Status    string             `json:"status"`
	Eligible  bool               `json:"eligible"`
	JoinedAt  time.Time          `json:"joinedAt,omitempty"`
	Checks    []ForwardGateCheck `json:"checks,omitempty"`
	CanList   bool               `json:"canList"`
	Blockers  []string           `json:"blockers,omitempty"`
	Warnings  []string           `json:"warnings,omitempty"`
}

type Company struct {
	ID          string    `json:"id"`
	OwnerUserID string    `json:"ownerUserId"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type CompanyOverview struct {
	ID          string    `json:"id"`
	OwnerUserID string    `json:"ownerUserId"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Funds       []Fund    `json:"funds"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type WorkflowStepStatus struct {
	Step       string `json:"step,omitempty"`
	Label      string `json:"label,omitempty"`
	Order      int    `json:"order,omitempty"`
	Status     string `json:"status,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
	EndedAt    string `json:"endedAt,omitempty"`
	UpdatedAt  string `json:"updatedAt,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
	Error      string `json:"error,omitempty"`
}

type WorkflowStatus struct {
	FundID          string                        `json:"fundId"`
	TradingDate     string                        `json:"tradingDate,omitempty"`
	State           string                        `json:"state"`
	Step            string                        `json:"step,omitempty"`
	StartedAt       string                        `json:"startedAt,omitempty"`
	CompletedAt     string                        `json:"completedAt,omitempty"`
	RunningForMs    int64                         `json:"runningForMs,omitempty"`
	ProgressPercent int                           `json:"progressPercent,omitempty"`
	CompletedSteps  int                           `json:"completedSteps,omitempty"`
	FailedSteps     int                           `json:"failedSteps,omitempty"`
	TotalSteps      int                           `json:"totalSteps,omitempty"`
	Steps           []WorkflowStepStatus          `json:"steps,omitempty"`
	StepResults     map[string]WorkflowStepStatus `json:"stepResults,omitempty"`
}

type LLMUsageVisibility struct {
	FundID         string              `json:"fundId"`
	From           string              `json:"from"`
	To             string              `json:"to"`
	TotalCalls     int                 `json:"totalCalls"`
	InputTokens    int64               `json:"inputTokens"`
	OutputTokens   int64               `json:"outputTokens"`
	TotalTokens    int64               `json:"totalTokens"`
	CostCents      float64             `json:"costCents"`
	PriceCents     float64             `json:"priceCents"`
	CustomKeyCalls int                 `json:"customKeyCalls"`
	ByAgent        []LLMUsageBreakdown `json:"byAgent"`
	ByStep         []LLMUsageBreakdown `json:"byStep"`
	ByModel        []LLMUsageBreakdown `json:"byModel"`
	RecentCalls    []LLMUsageCall      `json:"recentCalls"`
}

type LLMUsageBreakdown struct {
	Key            string  `json:"key"`
	Label          string  `json:"label,omitempty"`
	AgentID        string  `json:"agentId,omitempty"`
	TotalCalls     int     `json:"totalCalls"`
	InputTokens    int64   `json:"inputTokens"`
	OutputTokens   int64   `json:"outputTokens"`
	TotalTokens    int64   `json:"totalTokens"`
	CostCents      float64 `json:"costCents"`
	PriceCents     float64 `json:"priceCents"`
	CustomKeyCalls int     `json:"customKeyCalls"`
}

type LLMUsageCall struct {
	ID            string    `json:"id"`
	AgentID       string    `json:"agentId,omitempty"`
	AgentName     string    `json:"agentName,omitempty"`
	StepName      string    `json:"stepName"`
	ModelProvider string    `json:"modelProvider"`
	ModelName     string    `json:"modelName"`
	InputTokens   int       `json:"inputTokens"`
	OutputTokens  int       `json:"outputTokens"`
	TotalTokens   int       `json:"totalTokens"`
	CostCents     float64   `json:"costCents"`
	PriceCents    float64   `json:"priceCents"`
	IsCustomKey   bool      `json:"isCustomKey"`
	CreatedAt     time.Time `json:"createdAt"`
}

type AuditLogEntry struct {
	ID           string          `json:"id"`
	ActorUserID  string          `json:"actorUserId,omitempty"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resourceType"`
	ResourceID   string          `json:"resourceId"`
	Details      json.RawMessage `json:"details"`
	CreatedAt    time.Time       `json:"createdAt"`
}

type AuditLogResponse struct {
	Entries []AuditLogEntry `json:"entries"`
	Limit   int             `json:"limit"`
}

type PnLAttribution struct {
	FundID          string                     `json:"fundId"`
	From            string                     `json:"from,omitempty"`
	To              string                     `json:"to,omitempty"`
	BeginningAssets float64                    `json:"beginningAssets"`
	EndingAssets    float64                    `json:"endingAssets"`
	TotalPnL        float64                    `json:"totalPnl"`
	RealizedPnL     float64                    `json:"realizedPnl"`
	UnrealizedPnL   float64                    `json:"unrealizedPnl"`
	FeeDrag         float64                    `json:"feeDrag"`
	ReturnPct       float64                    `json:"returnPct"`
	BySymbol        []PnLAttributionBucket     `json:"bySymbol"`
	ByAssetClass    []PnLAttributionBucket     `json:"byAssetClass"`
	Daily           []PnLAttributionDailyPoint `json:"daily"`
}

type PnLAttributionBucket struct {
	Key           string  `json:"key"`
	Label         string  `json:"label,omitempty"`
	RealizedPnL   float64 `json:"realizedPnl"`
	UnrealizedPnL float64 `json:"unrealizedPnl"`
	FeeDrag       float64 `json:"feeDrag"`
	TotalPnL      float64 `json:"totalPnl"`
	TradeCount    int     `json:"tradeCount"`
	Exposure      float64 `json:"exposure"`
	Weight        float64 `json:"weight"`
}

type PnLAttributionDailyPoint struct {
	Date        string  `json:"date"`
	DailyReturn float64 `json:"dailyReturn"`
	TotalAssets float64 `json:"totalAssets"`
	DailyPnL    float64 `json:"dailyPnl"`
}

type DecisionTrace struct {
	FundID      string                   `json:"fundId"`
	TradingDate string                   `json:"tradingDate,omitempty"`
	Run         *DecisionTraceRun        `json:"run,omitempty"`
	Plan        *Plan                    `json:"plan,omitempty"`
	Memo        *CommitteeMemo           `json:"memo,omitempty"`
	Risk        *RiskExplanation         `json:"risk,omitempty"`
	Discussion  *DecisionTraceDiscussion `json:"discussion,omitempty"`
	Execution   *DecisionTraceExecution  `json:"execution,omitempty"`
	Review      *DecisionTraceReview     `json:"review,omitempty"`
	Research    []MarketResearch         `json:"research,omitempty"`
}

type RiskExplanation struct {
	Verdict          string                 `json:"verdict,omitempty"`
	Severity         string                 `json:"severity,omitempty"`
	Summary          string                 `json:"summary,omitempty"`
	BlockingReasons  []string               `json:"blockingReasons,omitempty"`
	Warnings         []string               `json:"warnings,omitempty"`
	Suggestions      []string               `json:"suggestions,omitempty"`
	AdjustmentAdvice []string               `json:"adjustmentAdvice,omitempty"`
	Checks           []RiskCheckExplanation `json:"checks,omitempty"`
}

type RiskCheckExplanation struct {
	RuleCode       string   `json:"ruleCode,omitempty"`
	RuleName       string   `json:"ruleName,omitempty"`
	Status         string   `json:"status,omitempty"`
	Severity       string   `json:"severity,omitempty"`
	Current        *float64 `json:"current,omitempty"`
	Threshold      *float64 `json:"threshold,omitempty"`
	Explanation    string   `json:"explanation,omitempty"`
	UserImpact     string   `json:"userImpact,omitempty"`
	AdjustmentHint string   `json:"adjustmentHint,omitempty"`
}

type CommitteeMemo struct {
	Title             string                  `json:"title,omitempty"`
	Summary           string                  `json:"summary,omitempty"`
	MarketBackground  string                  `json:"marketBackground,omitempty"`
	Participants      []CommitteeParticipant  `json:"participants,omitempty"`
	AgentViews        []CommitteeAgentView    `json:"agentViews,omitempty"`
	Consensus         []string                `json:"consensus,omitempty"`
	Contentions       []string                `json:"contentions,omitempty"`
	FinalDecision     *CommitteeFinalDecision `json:"finalDecision,omitempty"`
	RiskOpinion       *CommitteeRiskOpinion   `json:"riskOpinion,omitempty"`
	TraderSuggestions []CommitteeTraderAction `json:"traderSuggestions,omitempty"`
	TraceLinks        []CommitteeTraceLink    `json:"traceLinks,omitempty"`
}

type CommitteeParticipant struct {
	AgentID string `json:"agentId,omitempty"`
	Role    string `json:"role,omitempty"`
	Name    string `json:"name,omitempty"`
}

type CommitteeAgentView struct {
	AgentID   string   `json:"agentId,omitempty"`
	Role      string   `json:"role,omitempty"`
	Stance    string   `json:"stance,omitempty"`
	Symbols   []string `json:"symbols,omitempty"`
	Viewpoint string   `json:"viewpoint,omitempty"`
	Evidence  []string `json:"evidence,omitempty"`
}

type CommitteeFinalDecision struct {
	Status    string   `json:"status,omitempty"`
	PM        string   `json:"pm,omitempty"`
	Reasoning string   `json:"reasoning,omitempty"`
	Actions   []string `json:"actions,omitempty"`
}

type CommitteeRiskOpinion struct {
	Verdict     string   `json:"verdict,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
	Rejections  []string `json:"rejections,omitempty"`
	Suggestions []string `json:"suggestions,omitempty"`
}

type CommitteeTraderAction struct {
	PlanActionID string   `json:"planActionId,omitempty"`
	Symbol       string   `json:"symbol,omitempty"`
	Action       string   `json:"action,omitempty"`
	Instruction  string   `json:"instruction,omitempty"`
	SupportedBy  []string `json:"supportedBy,omitempty"`
	OpposedBy    []string `json:"opposedBy,omitempty"`
}

type CommitteeTraceLink struct {
	Label  string `json:"label,omitempty"`
	Target string `json:"target,omitempty"`
}

type DecisionTraceRun struct {
	State       string              `json:"state,omitempty"`
	Step        string              `json:"step,omitempty"`
	StartedAt   string              `json:"startedAt,omitempty"`
	CompletedAt string              `json:"completedAt,omitempty"`
	Steps       []DecisionTraceStep `json:"steps,omitempty"`
	RunID       string              `json:"runId,omitempty"`
}

type DecisionTraceStep struct {
	Step      string `json:"step,omitempty"`
	Status    string `json:"status,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
	EndedAt   string `json:"endedAt,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
	Error     string `json:"error,omitempty"`
}

type DecisionTraceDiscussion struct {
	Reasoning   string          `json:"reasoning,omitempty"`
	ReasoningZh string          `json:"reasoningZh,omitempty"`
	ReasoningEn string          `json:"reasoningEn,omitempty"`
	Summary     string          `json:"summary,omitempty"`
	SummaryZh   string          `json:"summaryZh,omitempty"`
	SummaryEn   string          `json:"summaryEn,omitempty"`
	Consensus   []string        `json:"consensus,omitempty"`
	ConsensusZh []string        `json:"consensusZh,omitempty"`
	ConsensusEn []string        `json:"consensusEn,omitempty"`
	Snapshot    json.RawMessage `json:"snapshot,omitempty"`
	HasSnapshot bool            `json:"hasSnapshot"`
}

type DecisionTraceExecution struct {
	Status           string                         `json:"status,omitempty"`
	ActionExecutions []DecisionTraceActionExecution `json:"actionExecutions,omitempty"`
	Trades           []Trade                        `json:"trades,omitempty"`
}

type DecisionTraceActionExecution struct {
	PlanActionID    string  `json:"planActionId,omitempty"`
	Symbol          string  `json:"symbol,omitempty"`
	Action          string  `json:"action,omitempty"`
	ExecutionStatus string  `json:"executionStatus,omitempty"`
	Trades          []Trade `json:"trades,omitempty"`
}

type DecisionTraceReview struct {
	Entries []MemoryEntry `json:"entries,omitempty"`
}

type MemoryEntry struct {
	ID          string    `json:"id"`
	AgentID     string    `json:"agentId,omitempty"`
	Title       string    `json:"title,omitempty"`
	Content     string    `json:"content"`
	Layer       string    `json:"layer"`
	TradingDate string    `json:"tradingDate,omitempty"`
	Tags        []string  `json:"tags,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`

	// TemplateKey + Payload power the i18n render path (migration
	// 085). When set, the UI renders the localised message via
	// shared/api-client/src/i18n.ts and falls back to Content
	// otherwise. Both are omitted from the wire format when unset
	// so legacy clients see the same shape they always saw.
	TemplateKey string          `json:"templateKey,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
}

type MemoryContext struct {
	FundID  string        `json:"fundId"`
	AgentID string        `json:"agentId,omitempty"`
	Layer   string        `json:"layer"`
	Entries []MemoryEntry `json:"entries"`
}

type AgentLearningScope struct {
	FundIDs      []string `json:"fundIds,omitempty"`
	Markets      []string `json:"markets,omitempty"`
	AssetClasses []string `json:"assetClasses,omitempty"`
	Themes       []string `json:"themes,omitempty"`
	Instruments  []string `json:"instruments,omitempty"`
	StyleHints   []string `json:"styleHints,omitempty"`
	MemoryScope  string   `json:"memoryScope,omitempty"`
}

type AgentLearningRecord struct {
	ID            string    `json:"id"`
	FundID        string    `json:"fundId,omitempty"`
	TradingDate   string    `json:"tradingDate,omitempty"`
	Title         string    `json:"title,omitempty"`
	Summary       string    `json:"summary,omitempty"`
	Hits          []string  `json:"hits,omitempty"`
	Misses        []string  `json:"misses,omitempty"`
	Lessons       []string  `json:"lessons,omitempty"`
	Adjustments   []string  `json:"adjustments,omitempty"`
	Tags          []string  `json:"tags,omitempty"`
	DailyReturn   *float64  `json:"dailyReturn,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	Revoked       bool      `json:"revoked,omitempty"`
	RevokedReason string    `json:"revokedReason,omitempty"`
	RevokedAt     string    `json:"revokedAt,omitempty"`

	// i18n contract (migration 085): only present when the lesson
	// was emitted by the structured pipeline. Frontend prefers this
	// over Title/Summary when rendering and falls back when missing.
	TemplateKey string          `json:"templateKey,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
}

type AgentLearningStatus struct {
	AgentID              string                `json:"agentId"`
	AgentName            string                `json:"agentName,omitempty"`
	Role                 string                `json:"role,omitempty"`
	Focus                string                `json:"focus,omitempty"`
	Enabled              bool                  `json:"enabled"`
	AutoApplyAdjustments bool                  `json:"autoApplyAdjustments"`
	MaxLessonsPerDay     int                   `json:"maxLessonsPerDay"`
	Scope                *AgentLearningScope   `json:"scope,omitempty"`
	RecentLessons        []string              `json:"recentLessons,omitempty"`
	LastLearningSummary  string                `json:"lastLearningSummary,omitempty"`
	LastLearningDate     string                `json:"lastLearningDate,omitempty"`
	LastLearningTags     []string              `json:"lastLearningTags,omitempty"`
	LastAdjustments      []string              `json:"lastAdjustments,omitempty"`
	LastDailyReturn      *float64              `json:"lastDailyReturn,omitempty"`
	LearningUpdatedAt    string                `json:"learningUpdatedAt,omitempty"`
	RevokedAt            string                `json:"revokedAt,omitempty"`
	RevokedReason        string                `json:"revokedReason,omitempty"`
	Records              []AgentLearningRecord `json:"records,omitempty"`
}

type AgentLearningConfigInput struct {
	AutoApplyAdjustments *bool               `json:"autoApplyAdjustments,omitempty"`
	MaxLessonsPerDay     *int                `json:"maxLessonsPerDay,omitempty"`
	Scope                *AgentLearningScope `json:"scope,omitempty"`
}

type RevokeAgentLearningInput struct {
	Reason string `json:"reason,omitempty"`
}

type AgentLineageNode struct {
	AgentID         string             `json:"agentId"`
	AgentName       string             `json:"agentName,omitempty"`
	Role            string             `json:"role,omitempty"`
	Focus           string             `json:"focus,omitempty"`
	OwnerUserID     string             `json:"ownerUserId,omitempty"`
	DerivedVia      string             `json:"derivedVia,omitempty"`
	SourceListingID string             `json:"sourceListingId,omitempty"`
	CreatedAt       time.Time          `json:"createdAt,omitempty"`
	Ancestors       []AgentLineageNode `json:"ancestors,omitempty"`
}

type AgentLineageTree struct {
	AgentID         string           `json:"agentId"`
	Root            AgentLineageNode `json:"root"`
	AncestorCount   int              `json:"ancestorCount"`
	MaxDepth        int              `json:"maxDepth"`
	MatryoshkaRisk  bool             `json:"matryoshkaRisk"`
	RiskExplanation string           `json:"riskExplanation,omitempty"`
}

type MarketInstrument struct {
	InstrumentKey      string  `json:"instrumentKey,omitempty"`
	Symbol             string  `json:"symbol"`
	Market             string  `json:"market,omitempty"`
	Exchange           string  `json:"exchange,omitempty"`
	AssetClass         string  `json:"assetClass,omitempty"`
	InstrumentType     string  `json:"instrumentType,omitempty"`
	QuoteCurrency      string  `json:"quoteCurrency,omitempty"`
	SettlementCurrency string  `json:"settlementCurrency,omitempty"`
	ContractMultiplier float64 `json:"contractMultiplier,omitempty"`
	ExpiryDate         string  `json:"expiryDate,omitempty"`
}

type MarketQuote struct {
	Symbol        string    `json:"symbol"`
	InstrumentKey string    `json:"instrumentKey,omitempty"`
	Market        string    `json:"market,omitempty"`
	Exchange      string    `json:"exchange,omitempty"`
	AssetClass    string    `json:"assetClass,omitempty"`
	Price         float64   `json:"price"`
	Bid           float64   `json:"bid,omitempty"`
	Ask           float64   `json:"ask,omitempty"`
	Volume        int64     `json:"volume,omitempty"`
	QuoteCurrency string    `json:"quoteCurrency,omitempty"`
	AsOf          time.Time `json:"asOf"`
	Source        string    `json:"source"`
	IsStale       bool      `json:"isStale,omitempty"`
}

type MarketNewsItem struct {
	Title       string    `json:"title"`
	TitleZh     string    `json:"titleZh,omitempty"`
	TitleEn     string    `json:"titleEn,omitempty"`
	Summary     string    `json:"summary,omitempty"`
	SummaryZh   string    `json:"summaryZh,omitempty"`
	SummaryEn   string    `json:"summaryEn,omitempty"`
	Language    string    `json:"language,omitempty"`
	URL         string    `json:"url,omitempty"`
	Source      string    `json:"source,omitempty"`
	PublishedAt time.Time `json:"publishedAt,omitempty"`
	Symbols     []string  `json:"symbols,omitempty"`
}

type MarketResearch struct {
	Instrument     MarketInstrument `json:"instrument"`
	Quote          *MarketQuote     `json:"quote,omitempty"`
	News           []MarketNewsItem `json:"news,omitempty"`
	BenchmarkQuote *MarketQuote     `json:"benchmarkQuote,omitempty"`
	Signals        []string         `json:"signals,omitempty"`
	Summary        string           `json:"summary,omitempty"`
	ProviderNotes  []string         `json:"providerNotes,omitempty"`
	GeneratedAt    time.Time        `json:"generatedAt"`
}

type FundMarketQuotes struct {
	FundID string        `json:"fundId"`
	Quotes []MarketQuote `json:"quotes"`
}

type FundMarketNews struct {
	FundID string           `json:"fundId"`
	Symbol string           `json:"symbol"`
	Items  []MarketNewsItem `json:"items"`
}

type MarketNewsDigest struct {
	FundID        string           `json:"fundId"`
	Symbols       []string         `json:"symbols,omitempty"`
	Items         []MarketNewsItem `json:"items"`
	ProviderNotes []string         `json:"providerNotes,omitempty"`
	GeneratedAt   time.Time        `json:"generatedAt"`
}

type ABTest struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	ControlFundID   string          `json:"controlFundId"`
	TreatmentFundID string          `json:"treatmentFundId"`
	VariableType    string          `json:"variableType"`
	VariableConfig  json.RawMessage `json:"variableConfig"`
	Status          string          `json:"status"`
	StartDate       string          `json:"startDate,omitempty"`
	EndDate         string          `json:"endDate,omitempty"`
	Results         *ABTestResults  `json:"results,omitempty"`
	CreatedAt       time.Time       `json:"createdAt"`
	UpdatedAt       time.Time       `json:"updatedAt"`
}

type ABTestResults struct {
	VariantA       map[string]float64       `json:"variantA"`
	VariantB       map[string]float64       `json:"variantB"`
	Winner         string                   `json:"winner"`
	NavSeries      []ABTestNAVPoint         `json:"navSeries,omitempty"`
	DecisionDiffs  []ABTestDecisionDiff     `json:"decisionDiffs,omitempty"`
	VariantATrades []ABTestVariantTrade     `json:"variantATrades,omitempty"`
	VariantBTrades []ABTestVariantTrade     `json:"variantBTrades,omitempty"`
	Confidence     *ABTestConfidenceSummary `json:"confidence,omitempty"`
	Scorecard      *ABTestScorecard         `json:"scorecard,omitempty"`
}

type ABTestNAVPoint struct {
	Date           string   `json:"date"`
	VariantA       *float64 `json:"variantA,omitempty"`
	VariantB       *float64 `json:"variantB,omitempty"`
	VariantAReturn *float64 `json:"variantAReturn,omitempty"`
	VariantBReturn *float64 `json:"variantBReturn,omitempty"`
	ExcessReturn   *float64 `json:"excessReturn,omitempty"`
}

type ABTestVariantTrade struct {
	Date        string  `json:"date"`
	VariantKey  string  `json:"variantKey"`
	Symbol      string  `json:"symbol"`
	Side        string  `json:"side"`
	Quantity    float64 `json:"quantity"`
	Price       float64 `json:"price"`
	Notional    float64 `json:"notional"`
	RealizedPnL float64 `json:"realizedPnL"`
	Reasoning   string  `json:"reasoning,omitempty"`
}

type ABTestDecisionDiff struct {
	Date           string  `json:"date"`
	Symbol         string  `json:"symbol"`
	VariantAAction string  `json:"variantAAction,omitempty"`
	VariantBAction string  `json:"variantBAction,omitempty"`
	ReturnImpact   float64 `json:"returnImpact"`
	Explanation    string  `json:"explanation,omitempty"`
}

type ABTestConfidenceSummary struct {
	Level          string   `json:"level"`
	Score          float64  `json:"score"`
	SampleDays     int      `json:"sampleDays"`
	TradeCount     int      `json:"tradeCount"`
	Warnings       []string `json:"warnings,omitempty"`
	Recommendation string   `json:"recommendation,omitempty"`
}

type ABTestScorecard struct {
	RecommendedVariant string                 `json:"recommendedVariant"`
	VariantAScore      float64                `json:"variantAScore"`
	VariantBScore      float64                `json:"variantBScore"`
	ScoreGap           float64                `json:"scoreGap"`
	Components         []ABTestScoreComponent `json:"components"`
	RiskNotes          []string               `json:"riskNotes,omitempty"`
	CostNotes          []string               `json:"costNotes,omitempty"`
	Verdict            string                 `json:"verdict,omitempty"`
}

type ABTestScoreComponent struct {
	Key          string  `json:"key"`
	Label        string  `json:"label"`
	VariantA     float64 `json:"variantA"`
	VariantB     float64 `json:"variantB"`
	Contribution float64 `json:"contribution"`
	Direction    string  `json:"direction"`
	Explanation  string  `json:"explanation,omitempty"`
}

type PromoteABTestLearningInput struct {
	VariantKey      string   `json:"variantKey,omitempty"`
	Mode            string   `json:"mode,omitempty"`
	AgentIDs        []string `json:"agentIds,omitempty"`
	DryRun          bool     `json:"dryRun,omitempty"`
	RequireAnalyzed *bool    `json:"requireAnalyzed,omitempty"`
}

type ABTestLearningPromotionResult struct {
	TestID        string                `json:"testId"`
	VariantKey    string                `json:"variantKey"`
	Mode          string                `json:"mode"`
	DryRun        bool                  `json:"dryRun"`
	UpdatedAgents []ABTestPromotedAgent `json:"updatedAgents"`
	SkippedAgents []ABTestPromotionSkip `json:"skippedAgents,omitempty"`
	Warnings      []string              `json:"warnings,omitempty"`
}

type ABTestPromotedAgent struct {
	AgentID            string   `json:"agentId"`
	AgentName          string   `json:"agentName,omitempty"`
	Role               string   `json:"role,omitempty"`
	AppliedMode        string   `json:"appliedMode"`
	LessonCount        int      `json:"lessonCount"`
	LearningEventCount int      `json:"learningEventCount"`
	LatestTradingDate  string   `json:"latestTradingDate,omitempty"`
	Lessons            []string `json:"lessons,omitempty"`
	Adjustments        []string `json:"adjustments,omitempty"`

	// F6 layered promotion targets. PromotedReflectionCount counts the
	// long-term `memories` rows that were cloned from the treatment fund
	// into the control fund for this agent. PromotedSkillKeys lists the
	// candidate skills (status=proposed) that were inserted into the
	// control agent's skill_config. Both are 0 / empty for a dry run.
	PromotedReflectionIDs []string `json:"promotedReflectionIds,omitempty"`
	PromotedSkillKeys     []string `json:"promotedSkillKeys,omitempty"`
}

type ABTestPromotionSkip struct {
	AgentID string `json:"agentId,omitempty"`
	Reason  string `json:"reason"`
}

type ABTestLearningPromotion struct {
	ID             string          `json:"id"`
	TestID         string          `json:"testId"`
	VariantKey     string          `json:"variantKey"`
	VariantName    string          `json:"variantName,omitempty"`
	AgentID        string          `json:"agentId"`
	AgentName      string          `json:"agentName,omitempty"`
	Mode           string          `json:"mode"`
	PreviousConfig json.RawMessage `json:"previousConfig,omitempty"`
	PromotedConfig json.RawMessage `json:"promotedConfig,omitempty"`
	PromotedBy     string          `json:"promotedBy,omitempty"`
	PromotedAt     time.Time       `json:"promotedAt"`
}

type ABTestLearningRollbackResult struct {
	PromotionID string `json:"promotionId"`
	TestID      string `json:"testId"`
	AgentID     string `json:"agentId"`
	AgentName   string `json:"agentName,omitempty"`
	RolledBack  bool   `json:"rolledBack"`

	// F6 rollback details. RolledBackReflectionIDs records the memory rows
	// that were deleted from the control fund; SkillKeysReverted lists the
	// skill_config keys that no longer appear after restoration. The
	// frontend uses both for an audit-log-style confirmation toast.
	RolledBackReflectionIDs []string `json:"rolledBackReflectionIds,omitempty"`
	SkillKeysReverted       []string `json:"skillKeysReverted,omitempty"`
}

type MarketplaceListing struct {
	ID                    string                   `json:"id"`
	SellerUserID          string                   `json:"sellerUserId,omitempty"`
	SourceFundID          string                   `json:"sourceFundId"`
	SourceAgentID         string                   `json:"sourceAgentId"`
	AgentName             string                   `json:"agentName"`
	AgentRole             string                   `json:"agentRole"`
	AgentFocus            string                   `json:"agentFocus,omitempty"`
	LatestLearningSummary string                   `json:"latestLearningSummary,omitempty"`
	AskPriceMinor         int64                    `json:"askPriceMinor"`
	Currency              string                   `json:"currency"`
	Status                string                   `json:"status"`
	SnapshotPayload       json.RawMessage          `json:"snapshotPayload,omitempty"`
	Trust                 *MarketplaceTrustSignals `json:"trust,omitempty"`
	SoldToUserID          string                   `json:"soldToUserId,omitempty"`
	SoldAt                *time.Time               `json:"soldAt,omitempty"`
	CreatedAt             time.Time                `json:"createdAt"`
	UpdatedAt             time.Time                `json:"updatedAt"`
}

type MarketplaceTrustSignals struct {
	Score               int       `json:"score"`
	Level               string    `json:"level"`
	Badges              []string  `json:"badges,omitempty"`
	Evidence            []string  `json:"evidence,omitempty"`
	LearningRecords     int       `json:"learningRecords"`
	PublicMemoryRecords int       `json:"publicMemoryRecords"`
	LastLearningAt      time.Time `json:"lastLearningAt,omitempty"`
	LastDailyReturn     *float64  `json:"lastDailyReturn,omitempty"`
	ModelConfigured     bool      `json:"modelConfigured"`
	ProfileCompleteness float64   `json:"profileCompleteness"`
	ListingAgeDays      int       `json:"listingAgeDays"`
}

type MarketplaceBid struct {
	ID            string    `json:"id"`
	ListingID     string    `json:"listingId"`
	BidderUserID  string    `json:"bidderUserId,omitempty"`
	BidPriceMinor int64     `json:"bidPriceMinor"`
	Currency      string    `json:"currency"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type MarketplaceOrder struct {
	ID               string    `json:"id"`
	ListingID        string    `json:"listingId"`
	SellerUserID     string    `json:"sellerUserId,omitempty"`
	BuyerUserID      string    `json:"buyerUserId,omitempty"`
	BuyerFundID      string    `json:"buyerFundId,omitempty"`
	SourceAgentID    string    `json:"sourceAgentId"`
	DeliveredAgentID string    `json:"deliveredAgentId"`
	AmountMinor      int64     `json:"amountMinor"`
	Currency         string    `json:"currency"`
	Status           string    `json:"status"`
	CreatedAt        time.Time `json:"createdAt"`
}

// ---------------------------------------------------------------------------
// Service interfaces — each backed by a concrete implementation elsewhere
// ---------------------------------------------------------------------------

type FundService interface {
	CreateCompany(input CreateCompanyInput) (*Company, error)
	ListCompanies(ownerUserID string) ([]Company, error)
	ListCompanyOverviews(ownerUserID string) ([]CompanyOverview, error)
	CreateFund(userID string, input CreateFundInput) (*Fund, error)
	ListFunds(userID, companyID string) ([]Fund, error)
	GetFund(userID, fundID string) (*Fund, error)
	GetForwardGate(userID, fundID string) (*ForwardGateStatus, error)
	UpdateFund(userID, fundID string, cfg FundConfig) (*Fund, error)
	DeleteFund(userID, fundID string) error
}

type CreateCompanyInput struct {
	OwnerUserID string `json:"ownerUserId"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type CreateFundInput struct {
	CompanyID        string              `json:"companyId"`
	Name             string              `json:"name"`
	Description      string              `json:"description,omitempty"`
	TradingMode      string              `json:"tradingMode"`
	InitialCapital   float64             `json:"initialCapital,omitempty"`
	Market           string              `json:"market,omitempty"`
	Exchange         string              `json:"exchange,omitempty"`
	AssetClass       string              `json:"assetClass,omitempty"`
	BaseCurrency     string              `json:"baseCurrency,omitempty"`
	BenchmarkSymbol  string              `json:"benchmarkSymbol,omitempty"`
	PrimaryDirection string              `json:"primaryDirection,omitempty"`
	CalendarCode     string              `json:"calendarCode,omitempty"`
	TimeZone         string              `json:"timeZone,omitempty"`
	Universe         *FundUniverse       `json:"universe,omitempty"`
	TeamIntervals    *FundTeamIntervals  `json:"teamIntervals,omitempty"`
	Specialization   *FundSpecialization `json:"specialization,omitempty"`
	HardRisk         *FundHardRiskConfig `json:"hardRisk,omitempty"`
	// ActivityRetentionDays controls how long Team Live Activity events
	// are persisted (and thus how far back the panel can scroll).
	// Valid range 1..10 days, defaults to 7 when omitted or out of
	// range. Validation lives in normalizeActivityRetentionDays.
	ActivityRetentionDays *int `json:"activityRetentionDays,omitempty"`
}

type FundConfig struct {
	Name             *string             `json:"name,omitempty"`
	Description      *string             `json:"description,omitempty"`
	TradingMode      *string             `json:"tradingMode,omitempty"`
	Status           *string             `json:"status,omitempty"`
	InitialCapital   *float64            `json:"initialCapital,omitempty"`
	Market           *string             `json:"market,omitempty"`
	Exchange         *string             `json:"exchange,omitempty"`
	AssetClass       *string             `json:"assetClass,omitempty"`
	BaseCurrency     *string             `json:"baseCurrency,omitempty"`
	BenchmarkSymbol  *string             `json:"benchmarkSymbol,omitempty"`
	PrimaryDirection *string             `json:"primaryDirection,omitempty"`
	CalendarCode     *string             `json:"calendarCode,omitempty"`
	TimeZone         *string             `json:"timeZone,omitempty"`
	Universe         *FundUniverse       `json:"universe,omitempty"`
	TeamIntervals    *FundTeamIntervals  `json:"teamIntervals,omitempty"`
	Specialization   *FundSpecialization `json:"specialization,omitempty"`
	HardRisk         *FundHardRiskConfig `json:"hardRisk,omitempty"`
	// AutoExecute toggles per-fund auto-execute guardrails (see
	// FundAutoExecuteConfig). Nil leaves the existing setting unchanged
	// on PATCH; a non-nil pointer overwrites the persisted config in
	// full (the server still backfills omitted guardrail fields with
	// defaults during normalization).
	AutoExecute *FundAutoExecuteConfig `json:"autoExecute,omitempty"`
	// ResearchTier toggles between the legacy text-concat roundtable
	// ("standard"/empty) and the Phase 2B multi-agent debate
	// roundtable ("advanced"). Nil leaves the existing setting
	// unchanged on PATCH.
	ResearchTier *string `json:"researchTier,omitempty"`
	// ActivityRetentionDays controls how long Team Live Activity events
	// are persisted. Valid range 1..10 days. Nil leaves the existing
	// setting unchanged on PATCH.
	ActivityRetentionDays *int `json:"activityRetentionDays,omitempty"`
}

type TeamService interface {
	AddAgent(userID, fundID, role, focus string) (*Agent, error)
	BindAgent(userID, fundID, agentID string) (*Agent, error)
	ListOwnedAgents(userID, bindStatus string) ([]Agent, error)
	RemoveAgent(userID, fundID, agentID string) error
	UpdateAgent(userID, fundID, agentID string, cfg AgentConfig) (*Agent, error)
	ListAgents(userID, fundID string) ([]Agent, error)
	GetAgentLearning(userID, agentID string) (*AgentLearningStatus, error)
	EnableAgentLearning(userID, agentID string, input AgentLearningConfigInput) (*AgentLearningStatus, error)
	DisableAgentLearning(userID, agentID string) (*AgentLearningStatus, error)
	UpdateAgentLearningScope(userID, agentID string, scope AgentLearningScope) (*AgentLearningStatus, error)
	RevokeAgentLearning(userID, agentID string, input RevokeAgentLearningInput) (*AgentLearningStatus, error)
	GetAgentLineage(userID, agentID string) (*AgentLineageTree, error)
	GetLLMUsageVisibility(userID, fundID string, from, to time.Time) (*LLMUsageVisibility, error)
	ListAuditLogs(userID, fundID string, limit int) (*AuditLogResponse, error)
	ExportAuditLogs(userID, fundID string, limit int) (*AuditLogResponse, error)
	// ListTeamActivity returns the most recent workflow activity events for the
	// fund (Team Live Activity timeline). sinceSeq=0 returns the newest `limit`
	// events; a positive sinceSeq returns only events with Seq > sinceSeq so
	// the UI can backfill after a transient disconnect. The implementation
	// must enforce that userID owns or has access to fundID.
	ListTeamActivity(userID, fundID string, limit int, sinceSeq uint64) ([]TeamActivityItem, error)
	// PageTeamActivity returns up to `limit` events strictly older than
	// `before`, newest-first. Used by the "load earlier" infinite-scroll
	// path in the Team Live Activity panel: the UI passes the timestamp
	// of the oldest visible item to fetch the next historical page.
	//
	// Falls back to the in-memory ring buffer when the persistence
	// store isn't wired (tests, bootstrap) — same auth contract as
	// ListTeamActivity.
	PageTeamActivity(userID, fundID string, before time.Time, limit int) ([]TeamActivityItem, error)
	// SubscribeTeamActivity opens a per-fund SSE stream. The returned channel
	// emits new activity events; the caller is responsible for invoking
	// Cancel exactly once (typically via defer in the SSE handler) so the
	// per-subscriber goroutine is released.
	SubscribeTeamActivity(userID, fundID string) (*TeamActivityStream, error)

	// GetAgentSpecialization returns the structured coverage record
	// for the (fund, agent) pair. Returns (nil, nil) when the agent
	// has no row — that's the "no specialization configured" state
	// and consumers fall back to the legacy focus string. Auth: the
	// caller must own the fund (same contract as ListAgents).
	GetAgentSpecialization(userID, fundID, agentID string) (*AgentSpecialization, error)
	// UpdateAgentSpecialization upserts the row. PUT semantics —
	// arrays passed in fully replace the persisted set; passing
	// empty arrays clears coverage and falls back to the legacy
	// heuristic. Returns the persisted row (including updated_at)
	// so the UI can show "saved Xs ago" without a follow-up GET.
	UpdateAgentSpecialization(userID, fundID, agentID string, spec AgentSpecialization) (*AgentSpecialization, error)
}

// AgentSpecialization is the JSON projection of the structured
// coverage record introduced by migration 087. Used by the
// /api/funds/{fundId}/team/{agentId}/specialization endpoints.
//
// Arrays are normalized to lower-case by the service adapter
// before persistence so the prompt builder can match against
// position symbols without a separate case-fold pass. The UI
// can show them however it likes — uppercase tickers, etc. —
// because the lower-cased version on disk is purely an internal
// matching index.
//
// FundID is included in the response (not the request body) so
// the UI can confirm the row belongs to the fund it currently
// has in scope; passing a member from a different fund is a
// service-layer auth failure, not a JSON-shape validation.
type AgentSpecialization struct {
	FundID      string    `json:"fundId"`
	AgentID     string    `json:"agentId"`
	Instruments []string  `json:"instruments"`
	Themes      []string  `json:"themes"`
	Markets     []string  `json:"markets"`
	UpdatedAt   time.Time `json:"updatedAt,omitempty"`
}

// TeamActivityItem is the JSON-friendly projection of a workflow.ActivityEvent
// exposed via /api/funds/{fundId}/team/activity{,/stream}.
type TeamActivityItem struct {
	Seq         uint64    `json:"seq"`
	Type        string    `json:"type"`
	Role        string    `json:"role"`
	Step        string    `json:"step,omitempty"`
	FundID      string    `json:"fundId"`
	RunID       string    `json:"runId,omitempty"`
	TradingDate string    `json:"tradingDate,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
	Message     string    `json:"message"`
	Error       string    `json:"error,omitempty"`
}

// TeamActivityStream is the SSE-friendly handle returned by
// SubscribeTeamActivity. Cancel must always be called exactly once (defer).
// DroppedCount reports how many events were skipped because the consumer
// channel was full (slow client) so the handler can surface a "reconnect to
// catch up" warning to the user.
type TeamActivityStream struct {
	Events       <-chan TeamActivityItem
	Cancel       func()
	DroppedCount func() uint64
}

type AgentModelConfig struct {
	Provider  string  `json:"provider"`
	ModelName string  `json:"modelName"`
	BaseURL   *string `json:"baseUrl,omitempty"`
	APIKey    *string `json:"apiKey,omitempty"`
}

type AgentConfig struct {
	Role            *string           `json:"role,omitempty"`
	Focus           *string           `json:"focus,omitempty"`
	SystemPrompt    *string           `json:"systemPrompt,omitempty"`
	SkillConfig     *json.RawMessage  `json:"skillConfig,omitempty"`
	DomainConfig    *json.RawMessage  `json:"domainConfig,omitempty"`
	EvolutionConfig *json.RawMessage  `json:"evolutionConfig,omitempty"`
	ModelConfig     *AgentModelConfig `json:"modelConfig,omitempty"`
}

// PlanListFilter narrows the result set of PlanService.ListPlans.
// All fields are optional; the zero value lists everything within the
// limit/offset window. Status is matched exactly (e.g. "approved",
// "rejected", "pending_user", "completed"). From/To filter by
// trading_date inclusively. Unknown statuses cause a 400 at the
// handler layer rather than being silently ignored downstream.
type PlanListFilter struct {
	Limit  int
	Offset int
	Status string
	From   *time.Time
	To     *time.Time
}

type PlanService interface {
	ListPlans(userID, fundID string, filter PlanListFilter) ([]Plan, error)
	GetPlan(userID, planID string) (*Plan, error)
	ApprovePlan(userID, planID string) (*Plan, error)
	RejectPlan(userID, planID, reason string) (*Plan, error)
	RefreshPlanQuote(ctx context.Context, userID, planID string) (*Plan, error)
}

type TradeService interface {
	// ListTrades returns the paged trade window for a fund.
	// excludeChildSlices=true filters out rows that are children
	// of a multi-slice parent (i.e. strategy_parent_trade_id IS
	// NOT NULL), so the UI list view shows one aggregated parent
	// per plan_action; the per-slice drilldown is served by
	// ListTradeChildren below. excludeChildSlices=false preserves
	// pre-T4 behaviour (every row, including children, in
	// created_at DESC).
	ListTrades(userID, fundID string, from, to *time.Time, limit, offset int, excludeChildSlices bool) ([]Trade, error)
	// ListTradeChildren returns every child slice of the given
	// parent trade ID — ordered created_at ASC so the operator
	// reads TWAP slices in the natural "first to last" order on
	// the drilldown panel. Returns empty slice (no error) when
	// the parent has no children (legacy / non-split rows). Authz
	// rules match ListTrades.
	ListTradeChildren(userID, fundID, parentTradeID string) ([]Trade, error)
	GetPortfolio(userID, fundID string) ([]Position, error)
	GetNAVHistory(userID, fundID string, from, to *time.Time) ([]NAVPoint, error)
	GetPnLAttribution(userID, fundID string, from, to *time.Time) (*PnLAttribution, error)
	// GetTodayPnL returns the dashboard "今日盈亏" payload:
	// today's realised + (current unrealised − prior-close
	// unrealised). See TodayPnL for the per-field semantics. The
	// frontend uses this instead of (live - latest NAV row) so the
	// number is correct even when today's intra-day NAV snapshot
	// has been rewritten by a settle/PM-plan run.
	GetTodayPnL(userID, fundID string) (*TodayPnL, error)
	// GetPortfolioQuotes returns the latest live quote snapshot for each
	// instrument currently held by the fund. Powers the PR-4 SSE quote
	// stream. Implementations must enforce fund-access authorisation
	// (same rules as GetPortfolio) and may return an empty slice when
	// no holdings exist. Errors should be plain repository / service
	// errors; the handler translates them to HTTP status codes.
	GetPortfolioQuotes(userID, fundID string) ([]PortfolioQuote, error)
}

// PortfolioQuote is the row pushed over the SSE quote stream. It mirrors
// the relevant subset of api.Position so the frontend can patch a single
// row in the holdings table without re-fetching the entire portfolio.
type PortfolioQuote struct {
	InstrumentKey string  `json:"instrumentKey"`
	Symbol        string  `json:"symbol"`
	Market        string  `json:"market,omitempty"`
	AssetClass    string  `json:"assetClass,omitempty"`
	CurrentPrice  float64 `json:"currentPrice"`
	MarketValue   float64 `json:"marketValue,omitempty"`
	// PriceAsOf / PriceSource / IsStale match Position.* so the
	// frontend can re-use its existing freshness badge component.
	PriceAsOf   string `json:"priceAsOf,omitempty"`
	PriceSource string `json:"priceSource,omitempty"`
	IsStale     bool   `json:"isStale,omitempty"`
}

type WorkflowService interface {
	StartWorkflow(userID, fundID string) (*WorkflowStatus, error)
	TriggerStep(userID, fundID, step string) (*WorkflowStatus, error)
	GetStatus(userID, fundID string) (*WorkflowStatus, error)
	ResumeApprovedPlan(fundID string, tradingDate time.Time, planID string) error

	// GetNextRun returns when the scheduler will next wake up for this
	// fund and the wall-clock anchor for every workflow step on that
	// trading day. Surfaced on the Decision Center / Agent Learning UI
	// so operators can see "still alive, will run at HH:MM" instead of
	// guessing whether anything is broken when the most recent
	// daily run has long since completed. Returns ErrUpstreamUnavailable
	// when the calendar service isn't wired (single-binary smoke runs).
	GetNextRun(userID, fundID string) (*NextWorkflowRun, error)
}

// NextWorkflowRun is the DTO the UI consumes to render the scheduler
// preview. All timestamps are RFC3339 UTC; the frontend re-renders
// them in the user's locale. `currentlyInWindow` is true when the
// scheduler considers `now` inside the active trading window for
// this fund — useful for badges like "running now" vs "next at 9:30".
type NextWorkflowRun struct {
	FundID            string                `json:"fundId"`
	TradingDate       string                `json:"tradingDate"`
	Timezone          string                `json:"timezone"`
	NextTriggerAt     time.Time             `json:"nextTriggerAt"`
	CurrentlyInWindow bool                  `json:"currentlyInWindow"`
	// Steps lists the legacy single-shot 10-step schedule (one full
	// workflow per trading day). Set only when DecisionIntervalMinutes
	// is nil; when interval mode is active the per-day schedule is a
	// repeating slot list, so Steps would be misleading and we surface
	// Slots instead.
	Steps *WorkflowStepSchedule `json:"steps,omitempty"`
	// Slots is the full list of decision triggers for the trading
	// date when the fund runs in interval mode (e.g. every 30 min).
	// Each entry is one full mini-workflow start time; the banner
	// uses this list to show "today's decision cadence" without
	// implying a single daily run. Empty when interval mode is off.
	Slots []time.Time `json:"slots,omitempty"`
	// IntervalMinutes echoes the active per-fund decision interval
	// so the frontend can render "每 30 分钟一次" labels without a
	// second round-trip into the fund settings endpoint.
	IntervalMinutes *int `json:"intervalMinutes,omitempty"`
}

// WorkflowStepSchedule mirrors marketcalendar.StepSchedule but as a
// stable JSON DTO so the frontend doesn't depend on internal layout.
type WorkflowStepSchedule struct {
	MacroBrief       time.Time `json:"macroBrief"`
	ResearchParallel time.Time `json:"researchParallel"`
	QuantSignals     time.Time `json:"quantSignals"`
	Roundtable       time.Time `json:"roundtable"`
	PMPlan           time.Time `json:"pmPlan"`
	RiskReview       time.Time `json:"riskReview"`
	UserApproval     time.Time `json:"userApproval"`
	TradeExecution   time.Time `json:"tradeExecution"`
	Settlement       time.Time `json:"settlement"`
	DailyReview      time.Time `json:"dailyReview"`
}

type MemoryService interface {
	GetMemory(userID, fundID, layer, agentID string) (*MemoryContext, error)
	SearchMemory(userID, fundID, layer, query string) ([]MemoryEntry, error)
}

// ReflectionService surfaces long-term reflections (distilled lessons
// promoted from daily learnings by the memory reflexion engine). It is
// intentionally separate from MemoryService so the read path can evolve
// independently — reflections have richer per-item metadata (theme,
// importance, source range) that we may want to expose later without
// breaking the generic memory contract.
type ReflectionService interface {
	ListReflections(userID, fundID string, limit int) (*ReflectionList, error)
}

// AttributionService is the API surface for Phase 3A-5 strategy
// attribution. Implementations wrap the attribution.Service and
// translate its domain types into JSON-friendly DTOs.
//
// The single read method gives the dashboard everything it needs:
//
//   - the cross-tab of (sleeve × regime) closed-lot stats
//   - the most recent batch of lessons the lesson generator wrote
//     to the memory store (so the dashboard can render the
//     "actions to take" rail without a second round-trip)
//
// userID is included so the service can apply the same per-user
// authorisation as the rest of the fund handlers.
//
// RefreshAttribution is the operator-driven "run it now" path.
// It pulls the latest closed_lots stats, generates lessons, and
// persists any new ones to the memory store — then returns the
// same shape as GetAttribution so the UI can re-render without a
// second round-trip. Implementations are expected to enforce the
// same per-fund authorisation as GetAttribution.
type AttributionService interface {
	GetAttribution(userID, fundID string, days int) (*AttributionResponse, error)
	RefreshAttribution(userID, fundID string, days int) (*AttributionResponse, error)
}

// AttributionResponse is the DTO the HTTP layer renders. Mirrors
// attribution.AttributionReport but flattens the repository
// stat structs into plain JSON keys so frontends don't have to
// reach into sql.NullString etc.
type AttributionResponse struct {
	FundID         string                  `json:"fundId"`
	WindowDays     int                     `json:"windowDays"`
	Since          time.Time               `json:"since"`
	GeneratedAt    time.Time               `json:"generatedAt"`
	BySleeve       []SleeveStatDTO         `json:"bySleeve"`
	ByRegime       []RegimeStatDTO         `json:"byRegime"`
	BySleeveRegime []SleeveRegimeStatDTO   `json:"bySleeveRegime"`
	Lessons        []AttributionLessonDTO  `json:"lessons"`
}

// SleeveStatDTO is the per-sleeve aggregate row. Win rate is
// pre-computed by the service so dashboards don't redo it.
type SleeveStatDTO struct {
	Sleeve         string  `json:"sleeve"`
	TradeCount     int     `json:"tradeCount"`
	WinCount       int     `json:"winCount"`
	LossCount      int     `json:"lossCount"`
	TotalPnL       float64 `json:"totalPnl"`
	AvgPnLPct      float64 `json:"avgPnlPct"`
	WinRate        float64 `json:"winRate"`
	MedianHoldDays float64 `json:"medianHoldDays"`
}

// RegimeStatDTO is the per-regime variant. Same fields minus the
// median holding days (regimes don't have a meaningful "typical
// hold" because they straddle many sleeves).
type RegimeStatDTO struct {
	Regime     string  `json:"regime"`
	TradeCount int     `json:"tradeCount"`
	WinCount   int     `json:"winCount"`
	LossCount  int     `json:"lossCount"`
	TotalPnL   float64 `json:"totalPnl"`
	AvgPnLPct  float64 `json:"avgPnlPct"`
	WinRate    float64 `json:"winRate"`
}

// SleeveRegimeStatDTO is the cross-tab cell — the one frontends
// will surface most prominently. Both labels are normalised
// (lower-case, "(unspecified)" for NULLs) by the service.
type SleeveRegimeStatDTO struct {
	Sleeve         string  `json:"sleeve"`
	Regime         string  `json:"regime"`
	TradeCount     int     `json:"tradeCount"`
	WinCount       int     `json:"winCount"`
	LossCount      int     `json:"lossCount"`
	TotalPnL       float64 `json:"totalPnl"`
	AvgPnLPct      float64 `json:"avgPnlPct"`
	WinRate        float64 `json:"winRate"`
	AvgHoldingDays float64 `json:"avgHoldingDays"`
}

// AttributionLessonDTO is the persisted-lesson view. Title +
// body are the human-readable strings the lesson generator
// produced; tags + severity drive UI filtering.
type AttributionLessonDTO struct {
	Kind      string    `json:"kind"`
	Severity  string    `json:"severity"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Tags      []string  `json:"tags"`
	CreatedAt time.Time `json:"createdAt"`

	// i18n contract (server migration 085, S15): when set, the
	// frontend renders the localised title/body via lessonRenderer.ts
	// against the user's locale and only falls back to Title/Body when
	// missing. Both fields are emitted via omitempty so legacy clients
	// see the same shape they always saw.
	//
	// The data is the *same* memories.template_key + memories.payload
	// row that MemoryEntry / AgentLearningRecord surface — we
	// deliberately surface it through this DTO too so the
	// StrategyAttributionPanel and the Memory Center render the same
	// lesson in the same language for the same user. See
	// attribution_wiring.go::memoryRowsToLessonDTO for the mapping.
	TemplateKey string          `json:"templateKey,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
}

// AgentSkillService is the read+approval surface for an agent's skill
// library (F4). The library lives inside the agent record (SkillConfig
// JSON) but is exposed as a first-class concept here so the UI can
// approve or reject candidate skills that the reflection engine produced.
//
// The contract is deliberately small:
//
//   - ListSkills returns every skill in the agent's library (approved +
//     proposed) so the UI can render a single timeline.
//   - ApproveSkill flips a proposed skill to status=approved, enabled=true
//     and stamps ApprovedAt. It is a no-op (and not an error) if the
//     skill is already approved — the UI can re-fire safely.
//   - RejectSkill removes the skill from the library. The deterministic
//     reflection key means a follow-up reflection over the same theme
//     could regenerate the proposal; that's intentional, so users who
//     change their mind don't get stuck.
type AgentSkillService interface {
	ListSkills(userID, agentID string) (*AgentSkillList, error)
	ApproveSkill(userID, agentID, skillKey string) (*AgentSkillEntry, error)
	RejectSkill(userID, agentID, skillKey string) error
}

// AgentSkillEntry is the JSON projection of a parsedSkillEntry tailored
// for the management UI. We omit raw match heuristics the user cannot
// usefully edit (workflow steps, scenario keywords) but keep Roles and
// Focuses so an admin can see which scenarios will pick the skill up
// once approved.
type AgentSkillEntry struct {
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Content     string   `json:"content,omitempty"`
	Status      string   `json:"status"`
	Source      string   `json:"source,omitempty"`
	Enabled     bool     `json:"enabled"`
	Priority    int      `json:"priority"`
	Roles       []string `json:"roles,omitempty"`
	Focuses     []string `json:"focuses,omitempty"`
	ProposedAt  string   `json:"proposedAt,omitempty"`
	ApprovedAt  string   `json:"approvedAt,omitempty"`
}

// AgentSkillList wraps a skill collection so the response shape is
// forward-compatible with pagination/filtering — same convention used by
// ReflectionList.
type AgentSkillList struct {
	AgentID string            `json:"agentId"`
	Skills  []AgentSkillEntry `json:"skills"`
}

// ReflectionItem is a single long-term reflection emitted by the memory
// reflexion engine. The Theme is parsed off the deterministic title so the
// frontend can group reflections without re-parsing the lesson body.
type ReflectionItem struct {
	ID          string    `json:"id"`
	FundID      string    `json:"fundId"`
	Theme       string    `json:"theme"`
	Title       string    `json:"title"`
	Content     string    `json:"content"`
	Tags        []string  `json:"tags,omitempty"`
	TradingDate string    `json:"tradingDate,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// ReflectionList wraps a chronologically-ordered slice of reflections so
// the response shape is forward-compatible with pagination/filters.
type ReflectionList struct {
	FundID      string           `json:"fundId"`
	Items       []ReflectionItem `json:"items"`
	GeneratedAt time.Time        `json:"generatedAt"`
}

type DecisionTraceService interface {
	GetDecisionTrace(userID, fundID, tradingDate, planID string) (*DecisionTrace, error)
}

type MarketService interface {
	GetQuotes(userID, fundID string, symbols []string) (*FundMarketQuotes, error)
	GetResearch(userID, fundID, symbol string, limit int) (*MarketResearch, error)
	GetNews(userID, fundID, symbol string, limit int) (*FundMarketNews, error)
	GetNewsDigest(userID, fundID string, symbols []string, limit int) (*MarketNewsDigest, error)
}

type CreateABTestInput struct {
	Name            string          `json:"name"`
	ControlFundID   string          `json:"controlFundId"`
	TreatmentFundID string          `json:"treatmentFundId"`
	VariableType    string          `json:"variableType"`
	VariableConfig  json.RawMessage `json:"variableConfig"`
	StartDate       string          `json:"startDate,omitempty"`
	EndDate         string          `json:"endDate,omitempty"`
}

type ABTestService interface {
	ListTests(userID, fundID string) ([]ABTest, error)
	CreateTest(userID string, input CreateABTestInput) (*ABTest, error)
	GetTest(userID, testID string) (*ABTest, error)
	StartTest(userID, testID string) (*ABTest, error)
	StopTest(userID, testID string) (*ABTest, error)
	AnalyzeTest(userID, testID string) (*ABTest, error)
	PromoteLearning(userID, testID string, input PromoteABTestLearningInput) (*ABTestLearningPromotionResult, error)
	ListLearningPromotions(userID, testID string) ([]ABTestLearningPromotion, error)
	RollbackLearningPromotion(userID, testID, promotionID string) (*ABTestLearningRollbackResult, error)
}

type CreateMarketplaceListingInput struct {
	FundID        string `json:"fundId"`
	AgentID       string `json:"agentId"`
	AskPriceMinor int64  `json:"askPriceMinor"`
	Currency      string `json:"currency,omitempty"`
}

type CreateMarketplaceBidInput struct {
	ListingID     string `json:"listingId"`
	BidPriceMinor int64  `json:"bidPriceMinor"`
	Currency      string `json:"currency,omitempty"`
}

type PurchaseMarketplaceListingInput struct {
	ListingID   string `json:"listingId"`
	BuyerFundID string `json:"buyerFundId,omitempty"`
}

type MarketplaceService interface {
	ListListings(userID string, limit, offset int) ([]MarketplaceListing, error)
	ListMyListings(userID string, limit, offset int) ([]MarketplaceListing, error)
	CreateListing(userID string, input CreateMarketplaceListingInput) (*MarketplaceListing, error)
	CancelListing(userID, listingID string) error
	ListBids(userID, listingID string, limit, offset int) ([]MarketplaceBid, error)
	CreateBid(userID string, input CreateMarketplaceBidInput) (*MarketplaceBid, error)
	PurchaseListing(userID string, input PurchaseMarketplaceListingInput) (*MarketplaceOrder, error)
}

// ---------------------------------------------------------------------------
// Marketplace auctions (English ascending + anti-sniping + wallet hold)
// ---------------------------------------------------------------------------

// AuctionListing is the public-facing projection of an `agent_market_listings`
// row with mode='auction'. Pricing/hold details that only matter to the
// seller or admin are intentionally omitted from the JSON shape so we can
// keep the same view object for both list and detail endpoints.
type AuctionListing struct {
	ID                    string                   `json:"id"`
	SellerUserID          string                   `json:"sellerUserId,omitempty"`
	SourceFundID          string                   `json:"sourceFundId"`
	SourceAgentID         string                   `json:"sourceAgentId"`
	AgentName             string                   `json:"agentName"`
	AgentRole             string                   `json:"agentRole"`
	AgentFocus            string                   `json:"agentFocus,omitempty"`
	LatestLearningSummary string                   `json:"latestLearningSummary,omitempty"`
	Mode                  string                   `json:"mode"`
	Status                string                   `json:"status"`
	Currency              string                   `json:"currency"`
	StartingPriceMinor    int64                    `json:"startingPriceMinor"`
	ReserveMinor          *int64                   `json:"reserveMinor,omitempty"`
	MinIncrementMinor     int64                    `json:"minIncrementMinor"`
	AntiSnipeSeconds      int                      `json:"antiSnipeSeconds"`
	CurrentBidMinor       *int64                   `json:"currentBidMinor,omitempty"`
	CurrentBidderUserID   string                   `json:"currentBidderUserId,omitempty"`
	CurrentBidID          string                   `json:"currentBidId,omitempty"`
	MinNextBidMinor       int64                    `json:"minNextBidMinor"`
	StartsAt              *time.Time               `json:"startsAt,omitempty"`
	EndsAt                *time.Time               `json:"endsAt,omitempty"`
	SettledAt             *time.Time               `json:"settledAt,omitempty"`
	WinningBidID          string                   `json:"winningBidId,omitempty"`
	WinnerUserID          string                   `json:"winnerUserId,omitempty"`
	SnapshotPayload       json.RawMessage          `json:"snapshotPayload,omitempty"`
	Trust                 *MarketplaceTrustSignals `json:"trust,omitempty"`
	CreatedAt             time.Time                `json:"createdAt"`
	UpdatedAt             time.Time                `json:"updatedAt"`
}

type AuctionBid struct {
	ID            string    `json:"id"`
	ListingID     string    `json:"listingId"`
	BidderUserID  string    `json:"bidderUserId,omitempty"`
	BidPriceMinor int64     `json:"bidPriceMinor"`
	Currency      string    `json:"currency"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type CreateAuctionListingInput struct {
	FundID             string    `json:"fundId"`
	AgentID            string    `json:"agentId"`
	StartingPriceMinor int64     `json:"startingPriceMinor"`
	ReserveMinor       int64     `json:"reserveMinor,omitempty"`
	MinIncrementMinor  int64     `json:"minIncrementMinor,omitempty"`
	AntiSnipeSeconds   int       `json:"antiSnipeSeconds,omitempty"`
	Currency           string    `json:"currency,omitempty"`
	StartsAt           time.Time `json:"startsAt"`
	EndsAt             time.Time `json:"endsAt"`
}

type PlaceAuctionBidInput struct {
	ListingID     string `json:"listingId"`
	BidPriceMinor int64  `json:"bidPriceMinor"`
	Currency      string `json:"currency,omitempty"`
}

type AuctionSettlementResult struct {
	ListingID     string            `json:"listingId"`
	Outcome       string            `json:"outcome"` // sold | reserve_not_met | no_bids
	WinningBidID  string            `json:"winningBidId,omitempty"`
	WinnerUserID  string            `json:"winnerUserId,omitempty"`
	FinalBidMinor int64             `json:"finalBidMinor,omitempty"`
	Order         *MarketplaceOrder `json:"order,omitempty"`
}

type MarketplaceAuctionService interface {
	ListAuctions(userID string, limit, offset int) ([]AuctionListing, error)
	GetAuction(userID, listingID string) (*AuctionListing, error)
	CreateAuction(userID string, input CreateAuctionListingInput) (*AuctionListing, error)
	PlaceBid(userID string, input PlaceAuctionBidInput) (*AuctionBid, *AuctionListing, error)
	SettleAuction(userID, listingID string) (*AuctionSettlementResult, error)
	SettleDueAuctions(ctx context.Context, now time.Time, limit int) ([]AuctionSettlementResult, error)
}

// ---------------------------------------------------------------------------
// FundHandler — aggregates all service dependencies
// ---------------------------------------------------------------------------

type FundHandler struct {
	funds            FundService
	teams            TeamService
	plans            PlanService
	trades           TradeService
	workflow         WorkflowService
	memory           MemoryService
	reflections      ReflectionService
	agentSkills      AgentSkillService
	decisionTrace    DecisionTraceService
	market           MarketService
	abtests          ABTestService
	marketplace      MarketplaceService
	auctions         MarketplaceAuctionService
	// attribution surfaces the Phase 3A-5 cross-tab + lesson
	// stream for a fund. Optional: when nil the handler responds
	// 503 and the dashboard renders an empty state.
	attribution AttributionService
	// backtests is the Phase 2E backtest service. nil-safe: the
	// handlers return 503 / empty list when this isn't wired so
	// the rest of the API works unchanged on deployments without
	// OHLC providers.
	backtests        BacktestService
	// promotions is the Phase 2J/K/L strategy promotion +
	// shadow + decay-monitor service. nil disables the
	// /promotions endpoints with a 503 / empty list.
	promotions       PromotionService
	// corpActions is the Sprint 4 corp-action read service.
	// nil-safe: GetCorpActions returns 503 when unset.
	corpActions      CorpActionService
	// benchmarks powers the benchmark-history overlay on the fund
	// dashboard. nil-safe: GetBenchmarkHistory returns 503 when
	// unwired so deployments without ohlc providers stay healthy.
	benchmarks       BenchmarkService
	// holdingsSeries powers the per-holding mini-chart grid (P1-2).
	// Same nil-safety contract as benchmarks; both lean on the
	// shared ohlc.Fetcher so they share the same kill-switch.
	holdingsSeries   HoldingsSeriesService
	// abShadowAgents surfaces the per-variant shadow agent
	// learning timeline (Card D). nil-safe: the handler returns
	// 503 when unwired so AB analyses still work without it.
	abShadowAgents   ABShadowAgentService
	// abAttribution surfaces the per-symbol A vs B operational
	// attribution table (Card D). Same nil-safety contract.
	abAttribution    ABOperationalAttributionService
	// fundAssist is the LLM-backed "describe a fund + team in
	// natural language and we'll create them" feature. nil-safe:
	// the endpoint returns 503 when unwired so deployments without
	// LLM keys still work.
	fundAssist       FundAssistService
}

// WithBacktestService wires the Phase 2E backtest service. nil
// disables the endpoints (handlers return 503 / empty list).
// Idempotent.
func (h *FundHandler) WithBacktestService(svc BacktestService) *FundHandler {
	if h != nil {
		h.backtests = svc
	}
	return h
}

// WithFundAssistService wires the LLM-backed fund-creation assistant
// onto the handler. nil disables /api/companies/{companyId}/funds:assist
// with a 503, matching the rest of the nil-safe pattern. Idempotent.
func (h *FundHandler) WithFundAssistService(svc FundAssistService) *FundHandler {
	if h != nil {
		h.fundAssist = svc
	}
	return h
}

func NewFundHandler(
	funds FundService,
	teams TeamService,
	plans PlanService,
	trades TradeService,
	workflow WorkflowService,
	memory MemoryService,
	decisionTrace DecisionTraceService,
	market MarketService,
	abtests ABTestService,
	marketplace MarketplaceService,
) *FundHandler {
	return &FundHandler{
		funds:         funds,
		teams:         teams,
		plans:         plans,
		trades:        trades,
		workflow:      workflow,
		memory:        memory,
		decisionTrace: decisionTrace,
		market:        market,
		abtests:       abtests,
		marketplace:   marketplace,
	}
}

// WithReflectionService injects the long-term reflection read service. Kept
// as an optional setter so callers that don't ship reflections (legacy
// tests, minimal handlers) can continue to construct FundHandler without
// providing one — the GET /reflections endpoint will then 404 cleanly.
func (h *FundHandler) WithReflectionService(svc ReflectionService) *FundHandler {
	if h != nil {
		h.reflections = svc
	}
	return h
}

// WithAgentSkillService injects the per-agent skill management service.
// Same optional pattern as WithReflectionService — when unset the skill
// endpoints respond 503 so frontends can degrade cleanly instead of
// hanging on a 404.
func (h *FundHandler) WithAgentSkillService(svc AgentSkillService) *FundHandler {
	if h != nil {
		h.agentSkills = svc
	}
	return h
}

// WithAuctionService injects the marketplace auction service. The auction
// surface is layered on top of the buyout/subscribe surface and is
// intentionally optional so legacy deployments can keep running.
func (h *FundHandler) WithAuctionService(svc MarketplaceAuctionService) *FundHandler {
	if h != nil {
		h.auctions = svc
	}
	return h
}

// WithAttributionService injects the Phase 3A-5 strategy
// attribution service. Optional: nil makes the related endpoint
// respond 503 instead of crashing — single-binary smoke / legacy
// tests don't need the dep wired.
func (h *FundHandler) WithAttributionService(svc AttributionService) *FundHandler {
	if h != nil {
		h.attribution = svc
	}
	return h
}

// ---------------------------------------------------------------------------
// Route registration — compatible with Go 1.22+ ServeMux patterns and chi
// ---------------------------------------------------------------------------

func (h *FundHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/companies", h.CreateCompany)
	mux.HandleFunc("GET /api/companies", h.ListCompanies)
	mux.HandleFunc("GET /api/companies/overview", h.ListCompanyOverviews)
	mux.HandleFunc("POST /api/companies/{companyId}/funds", h.CreateFund)
	mux.HandleFunc("POST /api/companies/{companyId}/funds:assist", h.AssistCreateFund)
	mux.HandleFunc("GET /api/companies/{companyId}/funds", h.ListFunds)
	mux.HandleFunc("GET /api/funds/{fundId}", h.GetFund)
	mux.HandleFunc("GET /api/funds/{fundId}/dashboard", h.GetDashboard)
	mux.HandleFunc("GET /api/funds/{fundId}/forward-gate", h.GetForwardGate)
	mux.HandleFunc("GET /api/funds/{fundId}/corp-actions", h.GetCorpActions)
	mux.HandleFunc("GET /api/funds/{fundId}/benchmark-history", h.GetBenchmarkHistory)
	mux.HandleFunc("GET /api/funds/{fundId}/holdings/series", h.GetHoldingsSeries)
	mux.HandleFunc("PUT /api/funds/{fundId}", h.UpdateFund)
	mux.HandleFunc("DELETE /api/funds/{fundId}", h.DeleteFund)
	mux.HandleFunc("POST /api/funds/{fundId}/backtests", h.SubmitBacktest)
	mux.HandleFunc("GET /api/funds/{fundId}/backtests", h.ListBacktests)
	mux.HandleFunc("GET /api/funds/{fundId}/backtests/compare", h.CompareBacktests)
	mux.HandleFunc("POST /api/funds/{fundId}/backtests/sweeps", h.SubmitSweep)
	mux.HandleFunc("GET /api/funds/{fundId}/backtests/sweeps", h.ListSweeps)
	mux.HandleFunc("GET /api/funds/{fundId}/backtests/sweeps/{sweepId}", h.GetSweep)
	mux.HandleFunc("GET /api/backtests/sweeps/axes", h.SweepAxisCatalog)
	mux.HandleFunc("GET /api/funds/{fundId}/backtests/{jobId}", h.GetBacktest)
	mux.HandleFunc("POST /api/funds/{fundId}/backtests/{jobId}/cancel", h.CancelBacktest)
	mux.HandleFunc("POST /api/funds/{fundId}/promotions", h.ProposePromotion)
	mux.HandleFunc("GET /api/funds/{fundId}/promotions", h.ListPromotions)
	mux.HandleFunc("GET /api/funds/{fundId}/promotions/{promotionId}", h.GetPromotion)
	mux.HandleFunc("POST /api/funds/{fundId}/promotions/{promotionId}/approve", h.ApprovePromotion)
	mux.HandleFunc("POST /api/funds/{fundId}/promotions/{promotionId}/reject", h.RejectPromotion)
	mux.HandleFunc("POST /api/funds/{fundId}/promotions/{promotionId}/activate", h.ActivatePromotion)
	mux.HandleFunc("POST /api/funds/{fundId}/promotions/{promotionId}/rollback", h.RollbackPromotion)

	mux.HandleFunc("GET /api/agents", h.ListOwnedAgents)
	mux.HandleFunc("GET /api/agents/{agentId}/learning", h.GetAgentLearning)
	mux.HandleFunc("PUT /api/agents/{agentId}/learning/enable", h.EnableAgentLearning)
	mux.HandleFunc("PUT /api/agents/{agentId}/learning/disable", h.DisableAgentLearning)
	mux.HandleFunc("PUT /api/agents/{agentId}/learning/scope", h.UpdateAgentLearningScope)
	mux.HandleFunc("POST /api/agents/{agentId}/learning/revoke", h.RevokeAgentLearning)
	mux.HandleFunc("GET /api/agents/{agentId}/skills", h.ListAgentSkills)
	mux.HandleFunc("POST /api/agents/{agentId}/skills/{skillKey}/approve", h.ApproveAgentSkill)
	mux.HandleFunc("DELETE /api/agents/{agentId}/skills/{skillKey}", h.RejectAgentSkill)
	mux.HandleFunc("GET /api/agents/{agentId}/lineage", h.GetAgentLineage)
	mux.HandleFunc("POST /api/funds/{fundId}/team", h.AddAgent)
	mux.HandleFunc("POST /api/funds/{fundId}/team/bind", h.BindAgent)
	mux.HandleFunc("DELETE /api/funds/{fundId}/team/{agentId}", h.RemoveAgent)
	mux.HandleFunc("PUT /api/funds/{fundId}/team/{agentId}", h.UpdateAgent)
	mux.HandleFunc("GET /api/funds/{fundId}/team/{agentId}/specialization", h.GetAgentSpecialization)
	mux.HandleFunc("PUT /api/funds/{fundId}/team/{agentId}/specialization", h.UpdateAgentSpecialization)
	mux.HandleFunc("GET /api/funds/{fundId}/team", h.ListTeam)
	mux.HandleFunc("GET /api/funds/{fundId}/team/activity", h.ListTeamActivity)
	mux.HandleFunc("GET /api/funds/{fundId}/team/activity/stream", h.StreamTeamActivity)

	mux.HandleFunc("GET /api/funds/{fundId}/plans", h.ListPlans)
	mux.HandleFunc("GET /api/funds/{fundId}/decision-trace", h.GetDecisionTrace)
	mux.HandleFunc("GET /api/plans/{planId}", h.GetPlan)
	mux.HandleFunc("POST /api/plans/{planId}/approve", h.ApprovePlan)
	mux.HandleFunc("POST /api/plans/{planId}/reject", h.RejectPlan)
	mux.HandleFunc("POST /api/plans/{planId}/refresh-quote", h.RefreshPlanQuote)

	mux.HandleFunc("GET /api/funds/{fundId}/trades", h.ListTrades)
	mux.HandleFunc("GET /api/funds/{fundId}/trades/{tradeId}/children", h.ListTradeChildren)
	mux.HandleFunc("GET /api/funds/{fundId}/portfolio", h.GetPortfolio)
	mux.HandleFunc("GET /api/funds/{fundId}/quotes/stream", h.StreamPortfolioQuotes)
	mux.HandleFunc("GET /api/funds/{fundId}/nav", h.GetNAVHistory)
	mux.HandleFunc("GET /api/funds/{fundId}/pnl-attribution", h.GetPnLAttribution)
	mux.HandleFunc("GET /api/funds/{fundId}/today-pnl", h.GetTodayPnL)
	// Phase 3A-5: strategy attribution cross-tab + lesson stream.
	// Optional service: when not wired the handler returns 503.
	mux.HandleFunc("GET /api/funds/{fundId}/strategy-attribution", h.GetStrategyAttribution)
	mux.HandleFunc("POST /api/funds/{fundId}/strategy-attribution/refresh", h.RefreshStrategyAttribution)

	mux.HandleFunc("POST /api/funds/{fundId}/workflow/start", h.StartWorkflow)
	mux.HandleFunc("POST /api/funds/{fundId}/workflow/step", h.TriggerStep)
	mux.HandleFunc("GET /api/funds/{fundId}/workflow/status", h.GetWorkflowStatus)
	mux.HandleFunc("GET /api/funds/{fundId}/workflow/next-run", h.GetWorkflowNextRun)
	// U4 step-1 — SSE companion to /workflow/status. Reuses the same
	// service-side method so authorization and data shape stay
	// identical; pushes diff-only frames on a 2s tick. Implementation
	// lives in workflow_stream.go.
	mux.HandleFunc("GET /api/funds/{fundId}/workflow/stream", h.StreamWorkflowStatus)
	mux.HandleFunc("GET /api/funds/{fundId}/llm-usage", h.GetLLMUsageVisibility)
	mux.HandleFunc("GET /api/funds/{fundId}/audit", h.ListAuditLogs)

	mux.HandleFunc("GET /api/funds/{fundId}/memory", h.GetMemory)
	mux.HandleFunc("GET /api/funds/{fundId}/memory/search", h.SearchMemory)
	mux.HandleFunc("GET /api/funds/{fundId}/reflections", h.ListReflections)

	mux.HandleFunc("GET /api/funds/{fundId}/market/quotes", h.GetMarketQuotes)
	mux.HandleFunc("GET /api/funds/{fundId}/market/research", h.GetMarketResearch)
	mux.HandleFunc("GET /api/funds/{fundId}/market/news", h.GetMarketNews)
	mux.HandleFunc("GET /api/funds/{fundId}/market/news/digest", h.GetMarketNewsDigest)

	mux.HandleFunc("GET /api/funds/{fundId}/abtests", h.ListABTests)
	mux.HandleFunc("POST /api/abtests", h.CreateABTest)
	mux.HandleFunc("GET /api/abtests/{testId}", h.GetABTest)
	mux.HandleFunc("POST /api/abtests/{testId}/start", h.StartABTest)
	mux.HandleFunc("POST /api/abtests/{testId}/stop", h.StopABTest)
	mux.HandleFunc("POST /api/abtests/{testId}/analyze", h.AnalyzeABTest)
	mux.HandleFunc("POST /api/abtests/{testId}/promote-learning", h.PromoteABTestLearning)
	mux.HandleFunc("GET /api/abtests/{testId}/learning-promotions", h.ListABTestLearningPromotions)
	mux.HandleFunc("POST /api/abtests/{testId}/learning-promotions/{promotionId}/rollback", h.RollbackABTestLearningPromotion)
	mux.HandleFunc("GET /api/abtests/{testId}/shadow-agents", h.GetABShadowAgents)
	mux.HandleFunc("GET /api/abtests/{testId}/operational-attribution", h.GetABOperationalAttribution)

	mux.HandleFunc("GET /api/marketplace/listings", h.ListMarketplaceListings)
	mux.HandleFunc("GET /api/marketplace/my-listings", h.ListMyMarketplaceListings)
	mux.HandleFunc("POST /api/marketplace/listings", h.CreateMarketplaceListing)
	mux.HandleFunc("DELETE /api/marketplace/listings/{listingId}", h.CancelMarketplaceListing)
	mux.HandleFunc("GET /api/marketplace/listings/{listingId}/bids", h.ListMarketplaceBids)
	mux.HandleFunc("POST /api/marketplace/bids", h.CreateMarketplaceBid)
	mux.HandleFunc("POST /api/marketplace/purchase", h.PurchaseMarketplaceListing)

	mux.HandleFunc("GET /api/marketplace/auctions", h.ListAuctions)
	mux.HandleFunc("GET /api/marketplace/auctions/{listingId}", h.GetAuction)
	mux.HandleFunc("POST /api/marketplace/auctions", h.CreateAuction)
	mux.HandleFunc("POST /api/marketplace/auctions/{listingId}/bids", h.PlaceAuctionBid)
	mux.HandleFunc("POST /api/marketplace/auctions/{listingId}/settle", h.SettleAuction)
}

// ---------------------------------------------------------------------------
// JSON helpers & standard error envelope
// ---------------------------------------------------------------------------

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"message":"failed to encode response"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func writeError(w http.ResponseWriter, status int, msg string, detail string) {
	writeJSON(w, status, apiError{Code: status, Message: msg, Detail: detail})
}

func decodeBody(r *http.Request, dst any) error {
	if r.Body == nil {
		return errors.New("request body is empty")
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

func pathValue(r *http.Request, name string) string {
	if v := r.PathValue(name); v != "" {
		return v
	}
	return ""
}

type contextKey string

const (
	authenticatedUserIDKey        contextKey = "authenticatedUserID"
	authenticatedUserRoleKey      contextKey = "authenticatedUserRole"
	authenticatedUserKYCStatusKey contextKey = "authenticatedUserKYCStatus"
	authenticatedUserKYCLevelKey  contextKey = "authenticatedUserKYCLevel"
)

func WithAuthenticatedUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, authenticatedUserIDKey, strings.TrimSpace(userID))
}

func WithAuthenticatedUserRole(ctx context.Context, role string) context.Context {
	return context.WithValue(ctx, authenticatedUserRoleKey, strings.TrimSpace(role))
}

func WithAuthenticatedUserKYC(ctx context.Context, status, level string) context.Context {
	ctx = context.WithValue(ctx, authenticatedUserKYCStatusKey, strings.TrimSpace(status))
	ctx = context.WithValue(ctx, authenticatedUserKYCLevelKey, strings.TrimSpace(level))
	return ctx
}

func AuthenticatedUserID(r *http.Request) (string, bool) {
	userID, ok := r.Context().Value(authenticatedUserIDKey).(string)
	if !ok {
		return "", false
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return "", false
	}
	return userID, true
}

func AuthenticatedUserRole(r *http.Request) (string, bool) {
	role, ok := r.Context().Value(authenticatedUserRoleKey).(string)
	if !ok {
		return "", false
	}
	role = strings.TrimSpace(role)
	if role == "" {
		return "", false
	}
	return role, true
}

func AuthenticatedUserKYC(r *http.Request) (status string, level string, ok bool) {
	status, okStatus := r.Context().Value(authenticatedUserKYCStatusKey).(string)
	level, okLevel := r.Context().Value(authenticatedUserKYCLevelKey).(string)
	if !okStatus || !okLevel {
		return "", "", false
	}
	return strings.TrimSpace(status), strings.TrimSpace(level), true
}

func RequireKYC(w http.ResponseWriter, r *http.Request, minLevel string) bool {
	status, level, ok := AuthenticatedUserKYC(r)
	if !ok || status != "verified" {
		writeError(w, http.StatusForbidden, "kyc_required", "该操作需要完成实名认证 (KYC)。")
		return false
	}

	levelWeight := func(l string) int {
		switch l {
		case "tier1_basic":
			return 1
		case "tier2_advanced":
			return 2
		case "tier3_enterprise":
			return 3
		default:
			return 0
		}
	}

	if levelWeight(level) < levelWeight(minLevel) {
		writeError(w, http.StatusForbidden, "kyc_level_insufficient", fmt.Sprintf("该操作需要更高级别的 KYC 认证 (%s)，当前级别为 %s。", minLevel, level))
		return false
	}

	return true
}

func requireAuthenticatedUserID(w http.ResponseWriter, r *http.Request) (string, bool) {
	userID, ok := AuthenticatedUserID(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
		return "", false
	}
	return userID, true
}

func parseOptionalTime(r *http.Request, key string) (*time.Time, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func parseIntDefault(r *http.Request, key string, def int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return def
	}
	return v
}

func parseListLimitOffset(r *http.Request, defaultLimit, maxLimit int) (int, int) {
	limit := parseIntDefault(r, "limit", defaultLimit)
	offset := parseIntDefault(r, "offset", 0)
	if limit <= 0 {
		limit = defaultLimit
	}
	if maxLimit > 0 && limit > maxLimit {
		limit = maxLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func requireNonEmpty(w http.ResponseWriter, value, field string) bool {
	if strings.TrimSpace(value) == "" {
		writeError(w, http.StatusBadRequest, "validation error", field+" is required")
		return false
	}
	return true
}

// ===========================================================================
// 1. Fund Company & Fund Management
// ===========================================================================

type createCompanyRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

func (h *FundHandler) CreateCompany(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}

	var req createCompanyRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if !requireNonEmpty(w, req.Name, "name") {
		return
	}

	company, err := h.funds.CreateCompany(CreateCompanyInput{
		OwnerUserID: userID,
		Name:        req.Name,
		Description: req.Description,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create company", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, company)
}

func (h *FundHandler) ListCompanies(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}

	companies, err := h.funds.ListCompanies(userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list companies", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, companies)
}

func (h *FundHandler) ListCompanyOverviews(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}

	overviews, err := h.funds.ListCompanyOverviews(userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list company overviews", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, overviews)
}

type createFundRequest struct {
	Name             string              `json:"name"`
	Description      string              `json:"description,omitempty"`
	TradingMode      string              `json:"tradingMode"`
	InitialCapital   float64             `json:"initialCapital,omitempty"`
	Market           string              `json:"market,omitempty"`
	Exchange         string              `json:"exchange,omitempty"`
	AssetClass       string              `json:"assetClass,omitempty"`
	BaseCurrency     string              `json:"baseCurrency,omitempty"`
	BenchmarkSymbol  string              `json:"benchmarkSymbol,omitempty"`
	PrimaryDirection string              `json:"primaryDirection,omitempty"`
	CalendarCode     string              `json:"calendarCode,omitempty"`
	TimeZone         string              `json:"timeZone,omitempty"`
	Universe         *FundUniverse       `json:"universe,omitempty"`
	TeamIntervals    *FundTeamIntervals  `json:"teamIntervals,omitempty"`
	Specialization   *FundSpecialization `json:"specialization,omitempty"`
	// ActivityRetentionDays mirrors FundConfig.ActivityRetentionDays
	// (1..10, default 7). Threaded through to CreateFundInput so the
	// create-vs-update surface is symmetric — previously POST with
	// this field returned 400 "unknown field" because decodeBody uses
	// strict decoding, even though the same field worked on PATCH.
	ActivityRetentionDays *int `json:"activityRetentionDays,omitempty"`
}

func (h *FundHandler) CreateFund(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	companyID := pathValue(r, "companyId")
	if !requireNonEmpty(w, companyID, "companyId") {
		return
	}

	var req createFundRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if !requireNonEmpty(w, req.Name, "name") {
		return
	}
	if !requireNonEmpty(w, req.TradingMode, "tradingMode") {
		return
	}

	if req.TradingMode == "live" {
		if !RequireKYC(w, r, "tier2_advanced") {
			return
		}
	}

	fund, err := h.funds.CreateFund(userID, CreateFundInput{
		CompanyID:             companyID,
		Name:                  req.Name,
		Description:           req.Description,
		TradingMode:           req.TradingMode,
		InitialCapital:        req.InitialCapital,
		Market:                req.Market,
		Exchange:              req.Exchange,
		AssetClass:            req.AssetClass,
		BaseCurrency:          req.BaseCurrency,
		BenchmarkSymbol:       req.BenchmarkSymbol,
		PrimaryDirection:      req.PrimaryDirection,
		CalendarCode:          req.CalendarCode,
		TimeZone:              req.TimeZone,
		Universe:              req.Universe,
		TeamIntervals:         req.TeamIntervals,
		Specialization:        req.Specialization,
		ActivityRetentionDays: req.ActivityRetentionDays,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create fund", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, fund)
}

// AssistCreateFund is the LLM-backed "describe the fund + team you
// want, we'll create it for you" entry point.
//
// Lifecycle:
//   - 503 when no FundAssistService is wired (e.g. dev box without
//     LLM keys).
//   - 400 on missing prompt.
//   - 502 when the LLM produced unusable output (no JSON, decode
//     fail). Distinct from 422 so the frontend can prompt "we
//     couldn't parse the model's response, please retry" vs
//     "the plan was valid JSON but failed validation".
//   - 422 with FundAssistError.Issues + the offending plan when
//     server-side validation rejects (cross-market team, missing
//     PM, unsupported market). The UI shows the issues and the
//     original plan so the user can see what went wrong.
//   - 200 OK on dryRun=true with the validated + defaulted plan +
//     any warnings — the user must POST again with dryRun=false to
//     actually create.
//   - 201 Created on dryRun=false with the created fund and full
//     agent list. Partial-failure semantics: if fund creation
//     succeeds but a later agent insertion fails, we DO leave the
//     fund (the user can fix up via the team UI) and return 207-
//     style mixed status — the response includes the fund + the
//     agents that did succeed, plus a warning describing what fell
//     through.
func (h *FundHandler) AssistCreateFund(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	companyID := pathValue(r, "companyId")
	if !requireNonEmpty(w, companyID, "companyId") {
		return
	}

	if h.fundAssist == nil {
		writeError(w, http.StatusServiceUnavailable, "assist not configured", "fund assist service is not wired on this deployment")
		return
	}

	var req FundAssistRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if !requireNonEmpty(w, req.Prompt, "prompt") {
		return
	}

	plan, warnings, err := computeAssistPlan(r.Context(), h.fundAssist, userID, req)
	if err != nil {
		// Distinguish empty-plan (LLM gave us garbage we couldn't
		// even parse) from validation-failed (plan parsed but had
		// semantic issues we caught). Both are user-recoverable
		// but the UI affordance is different.
		if errors.Is(err, ErrFundAssistEmptyPlan) {
			writeError(w, http.StatusBadGateway, "llm produced unusable output", "model didn't return a valid JSON plan; please refine your prompt and retry")
			return
		}
		var assistErr *FundAssistError
		if errors.As(err, &assistErr) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]interface{}{
				"error":    "plan_rejected",
				"detail":   "LLM 输出的方案未通过校验，请按 issues 修正提示词后重试",
				"issues":   assistErr.Issues,
				"plan":     assistErr.Plan,
				"warnings": warnings,
			})
			return
		}
		// Anything else (LLM transport error, prompt empty after
		// trim, service nil) → 500 with the underlying message.
		writeError(w, http.StatusInternalServerError, "assist failed", err.Error())
		return
	}

	plan = applyAssistDefaults(plan)

	if req.DryRun {
		writeJSON(w, http.StatusOK, FundAssistResponse{Plan: plan, Warnings: warnings})
		return
	}

	// Real execution path: create fund, then iterate agents.
	fund, err := h.funds.CreateFund(userID, planToCreateInput(companyID, plan))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create fund", err.Error())
		return
	}

	createdAgents := make([]Agent, 0, len(plan.Agents))
	for i, ag := range plan.Agents {
		role := strings.ToLower(strings.TrimSpace(ag.Role))
		focus := strings.TrimSpace(ag.Focus)
		agent, err := h.teams.AddAgent(userID, fund.ID, role, focus)
		if err != nil {
			// Fund is already created — surface the partial state
			// rather than silently swallowing, and tell the UI
			// which agent fell through. The user can finish setup
			// in the team editor.
			warnings = append(warnings, fmt.Sprintf("agents[%d] (%s) 创建失败：%s — 请在团队页面手动补齐", i, role, err.Error()))
			break
		}
		// Best-effort: push systemPrompt + name onto the agent if
		// the LLM produced one. We don't fail the call if the
		// update fails — the agent is bound and functional with
		// its default system prompt; the polish is non-critical.
		if name := strings.TrimSpace(ag.Name); name != "" || strings.TrimSpace(ag.SystemPrompt) != "" {
			cfg := AgentConfig{}
			if sp := strings.TrimSpace(ag.SystemPrompt); sp != "" {
				cfg.SystemPrompt = &sp
			}
			// Skip Name updates: AgentConfig has no Name field,
			// the seed scripts model "name" via the agent's role
			// + focus combo. We surface the LLM-written name in
			// the systemPrompt's first line instead so the user
			// sees it in the UI.
			if cfg.SystemPrompt != nil {
				updated, upErr := h.teams.UpdateAgent(userID, fund.ID, agent.ID, cfg)
				if upErr != nil {
					warnings = append(warnings, fmt.Sprintf("agents[%d] systemPrompt 写入失败：%s", i, upErr.Error()))
				} else {
					agent = updated
				}
			}
		}
		createdAgents = append(createdAgents, *agent)
	}

	writeJSON(w, http.StatusCreated, FundAssistResponse{
		FundID:   fund.ID,
		Fund:     fund,
		Agents:   createdAgents,
		Plan:     plan,
		Warnings: warnings,
	})
}

func (h *FundHandler) ListFunds(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	companyID := pathValue(r, "companyId")
	if !requireNonEmpty(w, companyID, "companyId") {
		return
	}

	funds, err := h.funds.ListFunds(userID, companyID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list funds", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, funds)
}

func (h *FundHandler) GetFund(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}

	fund, err := h.funds.GetFund(userID, fundID)
	if err != nil {
		handleServiceError(w, err, "fund")
		return
	}
	writeJSON(w, http.StatusOK, fund)
}

func (h *FundHandler) GetForwardGate(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	status, err := h.funds.GetForwardGate(userID, fundID)
	if err != nil {
		handleServiceError(w, err, "forward gate")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (h *FundHandler) GetDashboard(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}

	fund, err := h.funds.GetFund(userID, fundID)
	if err != nil {
		handleServiceError(w, err, "fund")
		return
	}

	navHistory, err := h.trades.GetNAVHistory(userID, fundID, nil, nil)
	if err != nil {
		handleServiceError(w, err, "nav history")
		return
	}

	positions, err := h.trades.GetPortfolio(userID, fundID)
	if err != nil {
		handleServiceError(w, err, "portfolio")
		return
	}

	// Dashboard "10 most-recent trades" preview: hide child
	// slices so the preview shows one parent row per
	// plan_action when child-splitting is on. The drilldown
	// lives on the dedicated Trade History page.
	trades, err := h.trades.ListTrades(userID, fundID, nil, nil, 10, 0, true)
	if err != nil {
		handleServiceError(w, err, "trades")
		return
	}
	if len(trades) > 10 {
		trades = trades[:10]
	}

	workflow, err := h.workflow.GetStatus(userID, fundID)
	if err != nil {
		handleServiceError(w, err, "workflow")
		return
	}

	writeJSON(w, http.StatusOK, FundDashboard{
		Fund:       fund,
		NavHistory: navHistory,
		Positions:  positions,
		Trades:     trades,
		Workflow:   workflow,
	})
}

func (h *FundHandler) UpdateFund(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}

	var cfg FundConfig
	if err := decodeBody(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if cfg.Name == nil && cfg.Description == nil && cfg.TradingMode == nil && cfg.Status == nil && cfg.InitialCapital == nil && cfg.Market == nil && cfg.Exchange == nil && cfg.AssetClass == nil && cfg.BaseCurrency == nil && cfg.BenchmarkSymbol == nil && cfg.PrimaryDirection == nil && cfg.CalendarCode == nil && cfg.TimeZone == nil && cfg.Universe == nil && cfg.TeamIntervals == nil && cfg.Specialization == nil && cfg.HardRisk == nil && cfg.AutoExecute == nil && cfg.ResearchTier == nil && cfg.ActivityRetentionDays == nil {
		writeError(w, http.StatusBadRequest, "validation error", "at least one field (name, description, tradingMode, status, initialCapital, market, exchange, assetClass, baseCurrency, benchmarkSymbol, primaryDirection, calendarCode, timeZone, universe, teamIntervals, specialization, hardRisk, autoExecute, researchTier, activityRetentionDays) must be provided")
		return
	}

	fund, err := h.funds.UpdateFund(userID, fundID, cfg)
	if err != nil {
		handleServiceError(w, err, "fund")
		return
	}
	writeJSON(w, http.StatusOK, fund)
}

func (h *FundHandler) DeleteFund(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}

	if err := h.funds.DeleteFund(userID, fundID); err != nil {
		handleServiceError(w, err, "fund")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ===========================================================================
// 2. Team Management
// ===========================================================================

type addAgentRequest struct {
	Role  string `json:"role"`
	Focus string `json:"focus"`
}

type bindAgentRequest struct {
	AgentID string `json:"agentId"`
}

func (h *FundHandler) ListOwnedAgents(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	bindStatus := strings.TrimSpace(r.URL.Query().Get("bindStatus"))
	agents, err := h.teams.ListOwnedAgents(userID, bindStatus)
	if err != nil {
		handleServiceError(w, err, "agent")
		return
	}
	writeJSON(w, http.StatusOK, agents)
}

func (h *FundHandler) AddAgent(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}

	var req addAgentRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if !requireNonEmpty(w, req.Role, "role") {
		return
	}

	agent, err := h.teams.AddAgent(userID, fundID, req.Role, req.Focus)
	if err != nil {
		handleServiceError(w, err, "agent")
		return
	}
	writeJSON(w, http.StatusCreated, agent)
}

func (h *FundHandler) BindAgent(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}

	var req bindAgentRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if !requireNonEmpty(w, req.AgentID, "agentId") {
		return
	}

	agent, err := h.teams.BindAgent(userID, fundID, req.AgentID)
	if err != nil {
		handleServiceError(w, err, "agent")
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func (h *FundHandler) RemoveAgent(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	agentID := pathValue(r, "agentId")
	if !requireNonEmpty(w, fundID, "fundId") || !requireNonEmpty(w, agentID, "agentId") {
		return
	}

	if err := h.teams.RemoveAgent(userID, fundID, agentID); err != nil {
		handleServiceError(w, err, "agent")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *FundHandler) UpdateAgent(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	agentID := pathValue(r, "agentId")
	if !requireNonEmpty(w, fundID, "fundId") || !requireNonEmpty(w, agentID, "agentId") {
		return
	}

	var cfg AgentConfig
	if err := decodeBody(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if cfg.Role == nil && cfg.Focus == nil && cfg.SystemPrompt == nil && cfg.SkillConfig == nil && cfg.DomainConfig == nil && cfg.EvolutionConfig == nil && cfg.ModelConfig == nil {
		writeError(w, http.StatusBadRequest, "validation error", "at least one configurable field must be provided")
		return
	}

	agent, err := h.teams.UpdateAgent(userID, fundID, agentID, cfg)
	if err != nil {
		handleServiceError(w, err, "agent")
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

// GetAgentSpecialization handles GET /api/funds/{fundId}/team/{agentId}/specialization.
// Returns 200 with the persisted row when the agent has structured
// coverage configured, or 200 with empty arrays when no row exists
// (the "no specialization set, fall back to focus string" state).
// We deliberately return 200 + empty arrays rather than 404 so the
// frontend can treat "missing" and "explicitly empty" identically.
func (h *FundHandler) GetAgentSpecialization(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	agentID := pathValue(r, "agentId")
	if !requireNonEmpty(w, fundID, "fundId") || !requireNonEmpty(w, agentID, "agentId") {
		return
	}
	spec, err := h.teams.GetAgentSpecialization(userID, fundID, agentID)
	if err != nil {
		handleServiceError(w, err, "specialization")
		return
	}
	if spec == nil {
		// Empty-state response — the UI gets an editable "no
		// coverage configured yet" shell instead of a 404 it has
		// to special-case.
		spec = &AgentSpecialization{
			FundID:      fundID,
			AgentID:     agentID,
			Instruments: []string{},
			Themes:      []string{},
			Markets:     []string{},
		}
	}
	writeJSON(w, http.StatusOK, spec)
}

// UpdateAgentSpecialization handles PUT /api/funds/{fundId}/team/{agentId}/specialization.
// Body shape: { instruments: string[], themes: string[], markets: string[] }.
// Passing empty arrays clears coverage and falls back to focus-string
// behaviour — that's the intended way to "unset" specialization.
func (h *FundHandler) UpdateAgentSpecialization(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	agentID := pathValue(r, "agentId")
	if !requireNonEmpty(w, fundID, "fundId") || !requireNonEmpty(w, agentID, "agentId") {
		return
	}
	var body AgentSpecialization
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	// Defensive nil → empty so callers don't have to pre-fill;
	// pgx will reject a NULL TEXT[] write either way.
	if body.Instruments == nil {
		body.Instruments = []string{}
	}
	if body.Themes == nil {
		body.Themes = []string{}
	}
	if body.Markets == nil {
		body.Markets = []string{}
	}
	spec, err := h.teams.UpdateAgentSpecialization(userID, fundID, agentID, body)
	if err != nil {
		handleServiceError(w, err, "specialization")
		return
	}
	writeJSON(w, http.StatusOK, spec)
}

func (h *FundHandler) ListTeam(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}

	agents, err := h.teams.ListAgents(userID, fundID)
	if err != nil {
		handleServiceError(w, err, "team")
		return
	}
	writeJSON(w, http.StatusOK, agents)
}

// ListTeamActivity returns workflow activity events for the fund's team.
// Backfill endpoint for the Team Live Activity timeline so the UI can
// render an initial state before the SSE stream connects.
//
// Query params:
//
//	limit    – max events to return (default 50, hard ceiling 500)
//	sinceSeq – only return events with seq > sinceSeq (incremental backfill
//	           after a brief SSE disconnect; defaults to 0 = return newest N)
//	before   – RFC3339 timestamp; return events strictly older than this
//	           (newest-first), powering the "load earlier" infinite scroll.
//	           Mutually exclusive with sinceSeq — when both are supplied,
//	           `before` wins because it's the more specific cursor.
func (h *FundHandler) ListTeamActivity(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}

	limit := parseLimitParam(r, "limit", 50, 500)

	// Paging via `before` cursor: take the historical path if the client
	// asked for "events older than X". The handler does not require the
	// cursor to be a known event time — any RFC3339 timestamp works,
	// matching the semantics "show me everything before this moment".
	if raw := r.URL.Query().Get("before"); raw != "" {
		before, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid before", "before must be RFC3339 (e.g. 2026-05-19T13:30:00Z)")
			return
		}
		items, err := h.teams.PageTeamActivity(userID, fundID, before, limit)
		if err != nil {
			handleServiceError(w, err, "team activity page")
			return
		}
		if items == nil {
			items = []TeamActivityItem{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"fundId": fundID,
			"items":  items,
		})
		return
	}

	sinceSeq := parseUint64Param(r, "sinceSeq")
	items, err := h.teams.ListTeamActivity(userID, fundID, limit, sinceSeq)
	if err != nil {
		handleServiceError(w, err, "team activity")
		return
	}
	if items == nil {
		items = []TeamActivityItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"fundId": fundID,
		"items":  items,
	})
}

// StreamPortfolioQuotes opens a Server-Sent Events stream that pushes a
// fresh quote snapshot for every position in the fund every ~2 seconds.
// Designed for the holdings table on the Dashboard + the live "current
// price" column in the Decision Center.
//
// Event protocol:
//
//	event: quotes
//	data: {"quotes":[{"symbol":"MU","currentPrice":98.42,"priceAsOf":"...","isStale":false}, ...]}
//
//	event: heartbeat
//	data: 2026-05-20T01:30:00Z
//
// The tick cadence is set via `MARKETDATA_QUOTE_SSE_INTERVAL` (default 2s);
// 15s heartbeats keep proxies from closing idle connections. Auth is the
// same as StreamTeamActivity — the `fundai_session` cookie is required
// because EventSource cannot set Authorization headers.
//
// The handler reads from the marketdata cache via the TradeService, which
// in turn uses PR-1's singleflight + per-provider rate limit + adaptive
// TTL, so a 100-viewer fan-out doesn't multiply upstream load.
func (h *FundHandler) StreamPortfolioQuotes(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}

	flusher, isFlusher := w.(http.Flusher)
	if !isFlusher {
		writeError(w, http.StatusInternalServerError, "sse unsupported", "response writer does not implement http.Flusher")
		return
	}

	// Authorize once up front by calling the same service the REST
	// portfolio endpoint uses. If the user can't read the portfolio they
	// shouldn't be able to subscribe to its quotes either. We immediately
	// discard the slice — we only want the access check.
	if _, err := h.trades.GetPortfolioQuotes(userID, fundID); err != nil {
		handleServiceError(w, err, "portfolio quotes stream")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if _, err := io.WriteString(w, ": connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	ctx := r.Context()
	interval := parseStreamInterval(r.URL.Query().Get("interval"), 2*time.Second)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	// Track the last frame we pushed per instrument key so we can skip
	// rows whose price hasn't changed. This keeps the per-fund payload
	// small even when the cache hasn't moved for a tick.
	type frameKey struct {
		price float64
		stale bool
	}
	last := make(map[string]frameKey)

	encoder := json.NewEncoder(w)
	pushFrame := func() bool {
		quotes, err := h.trades.GetPortfolioQuotes(userID, fundID)
		if err != nil {
			// Soft-fail: log nothing (we already logged at the auth
			// step) and try again next tick. A transient marketdata
			// outage shouldn't kill the SSE connection.
			return true
		}
		changed := make([]PortfolioQuote, 0, len(quotes))
		for _, q := range quotes {
			prev, seen := last[q.InstrumentKey]
			if seen && prev.price == q.CurrentPrice && prev.stale == q.IsStale {
				continue
			}
			last[q.InstrumentKey] = frameKey{price: q.CurrentPrice, stale: q.IsStale}
			changed = append(changed, q)
		}
		if len(changed) == 0 {
			return true
		}
		if _, err := io.WriteString(w, "event: quotes\ndata: "); err != nil {
			return false
		}
		if err := encoder.Encode(map[string]any{"quotes": changed}); err != nil {
			return false
		}
		// json.Encoder appends a trailing \n; SSE needs an extra blank
		// line as the message terminator.
		if _, err := io.WriteString(w, "\n"); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Push an immediate first frame so the client doesn't wait a full
	// tick to see live data. We deliberately ignore the early-exit hint
	// for the first frame because the connection has only just opened.
	pushFrame()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, "event: heartbeat\ndata: "+time.Now().UTC().Format(time.RFC3339)+"\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-tick.C:
			if !pushFrame() {
				return
			}
		}
	}
}

// parseStreamInterval reads the SSE tick interval from the request query
// string and clamps it to [500ms, 30s]. Empty / malformed input falls
// back to `fallback` so URLs without the param keep the previous default.
// Tight intervals are clamped so a misbehaving client can't ask the
// server to push 100 frames/sec.
func parseStreamInterval(raw string, fallback time.Duration) time.Duration {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(trimmed)
	if err != nil || parsed <= 0 {
		return fallback
	}
	const minInterval = 500 * time.Millisecond
	const maxInterval = 30 * time.Second
	if parsed < minInterval {
		return minInterval
	}
	if parsed > maxInterval {
		return maxInterval
	}
	return parsed
}

// StreamTeamActivity opens a Server-Sent Events stream of workflow events for
// the fund. Designed for the Team Live Activity panel in the web UI.
//
// Event protocol:
//
//	event: activity
//	id: <seq>
//	data: {"seq":...,"type":"step_started",...}
//
//	event: heartbeat
//	data: 2026-05-19T01:30:00Z
//
// Heartbeats are sent every 15s so intermediate proxies don't time out idle
// connections. Clients should reconnect on close and pass the last seen Seq
// as ?sinceSeq=N to the REST backfill endpoint to catch missed events.
//
// SSE compatibility note: EventSource cannot set the Authorization header, so
// authentication on this endpoint relies on the `fundai_session` cookie set
// by the login flow (the auth middleware accepts both). Cross-origin EventSource
// callers must therefore include cookies (withCredentials=true) and the server
// CORS layer must echo back Access-Control-Allow-Credentials.
func (h *FundHandler) StreamTeamActivity(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}

	flusher, isFlusher := w.(http.Flusher)
	if !isFlusher {
		writeError(w, http.StatusInternalServerError, "sse unsupported", "response writer does not implement http.Flusher")
		return
	}

	stream, err := h.teams.SubscribeTeamActivity(userID, fundID)
	if err != nil {
		handleServiceError(w, err, "team activity stream")
		return
	}
	defer stream.Cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	// Hint to nginx/cloudflare to disable buffering for this stream so events
	// arrive immediately. Harmless when proxies don't recognise the header.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Send an initial comment so EventSource clients fire "open" immediately
	// (without waiting for the first real event). The leading ":" makes this
	// an SSE comment per the spec, ignored by clients.
	if _, err := io.WriteString(w, ": connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	ctx := r.Context()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	encoder := json.NewEncoder(w)
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, "event: heartbeat\ndata: "+time.Now().UTC().Format(time.RFC3339)+"\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case evt, alive := <-stream.Events:
			if !alive {
				return
			}
			if _, err := io.WriteString(w, "event: activity\nid: "); err != nil {
				return
			}
			if _, err := io.WriteString(w, strconv.FormatUint(evt.Seq, 10)); err != nil {
				return
			}
			if _, err := io.WriteString(w, "\ndata: "); err != nil {
				return
			}
			if err := encoder.Encode(evt); err != nil {
				return
			}
			// json.Encoder appends a trailing \n; SSE needs an extra blank
			// line as the message terminator.
			if _, err := io.WriteString(w, "\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// parseUint64Param parses a numeric query parameter, returning 0 on absence
// or any parsing failure. Used by activity endpoints where invalid input
// shouldn't 400 (the worst case is the caller gets a full backfill).
func parseUint64Param(r *http.Request, key string) uint64 {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// parseLimitParam parses a positive-int query parameter, clamps it to
// [1, maxValue], and falls back to `fallback` when missing or invalid.
func parseLimitParam(r *http.Request, key string, fallback, maxValue int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	if n > maxValue {
		return maxValue
	}
	return n
}

func (h *FundHandler) GetAgentLearning(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	agentID := pathValue(r, "agentId")
	if !requireNonEmpty(w, agentID, "agentId") {
		return
	}

	status, err := h.teams.GetAgentLearning(userID, agentID)
	if err != nil {
		handleServiceError(w, err, "agent learning")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (h *FundHandler) EnableAgentLearning(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	agentID := pathValue(r, "agentId")
	if !requireNonEmpty(w, agentID, "agentId") {
		return
	}

	var input AgentLearningConfigInput
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeBody(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
			return
		}
	}
	status, err := h.teams.EnableAgentLearning(userID, agentID, input)
	if err != nil {
		handleServiceError(w, err, "agent learning")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (h *FundHandler) DisableAgentLearning(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	agentID := pathValue(r, "agentId")
	if !requireNonEmpty(w, agentID, "agentId") {
		return
	}

	status, err := h.teams.DisableAgentLearning(userID, agentID)
	if err != nil {
		handleServiceError(w, err, "agent learning")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (h *FundHandler) UpdateAgentLearningScope(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	agentID := pathValue(r, "agentId")
	if !requireNonEmpty(w, agentID, "agentId") {
		return
	}

	var scope AgentLearningScope
	if err := decodeBody(r, &scope); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	status, err := h.teams.UpdateAgentLearningScope(userID, agentID, scope)
	if err != nil {
		handleServiceError(w, err, "agent learning")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (h *FundHandler) RevokeAgentLearning(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	agentID := pathValue(r, "agentId")
	if !requireNonEmpty(w, agentID, "agentId") {
		return
	}

	var input RevokeAgentLearningInput
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeBody(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
			return
		}
	}
	status, err := h.teams.RevokeAgentLearning(userID, agentID, input)
	if err != nil {
		handleServiceError(w, err, "agent learning")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (h *FundHandler) GetAgentLineage(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	agentID := pathValue(r, "agentId")
	if !requireNonEmpty(w, agentID, "agentId") {
		return
	}

	tree, err := h.teams.GetAgentLineage(userID, agentID)
	if err != nil {
		handleServiceError(w, err, "agent lineage")
		return
	}
	writeJSON(w, http.StatusOK, tree)
}

// ===========================================================================
// 3. Investment Plans & Approval
// ===========================================================================

func (h *FundHandler) ListPlans(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}

	limit, offset := parseListLimitOffset(r, 50, 200)
	filter := PlanListFilter{Limit: limit, Offset: offset}

	// Validate optional ?status=... against the canonical plan-status
	// vocabulary. Silently ignoring an unknown status (the prior
	// behaviour) caused the client to receive ALL plans when it
	// thought it had filtered to "approved" — that was P3 sweep
	// Test 10.
	if status := strings.TrimSpace(r.URL.Query().Get("status")); status != "" {
		switch status {
		case "draft", "pending_user", "approved", "rejected", "executing", "completed", "failed":
			filter.Status = status
		default:
			writeError(w, http.StatusBadRequest, "invalid status", "status must be one of draft, pending_user, approved, rejected, executing, completed, failed")
			return
		}
	}

	from, err := parseOptionalTime(r, "from")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid 'from' parameter", "expected RFC 3339 format: "+err.Error())
		return
	}
	to, err := parseOptionalTime(r, "to")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid 'to' parameter", "expected RFC 3339 format: "+err.Error())
		return
	}
	if from != nil && to != nil && to.Before(*from) {
		writeError(w, http.StatusBadRequest, "invalid range", "'to' must be greater than or equal to 'from'")
		return
	}
	filter.From = from
	filter.To = to

	plans, err := h.plans.ListPlans(userID, fundID, filter)
	if err != nil {
		handleServiceError(w, err, "plan")
		return
	}
	writeJSON(w, http.StatusOK, plans)
}

func (h *FundHandler) GetPlan(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	planID := pathValue(r, "planId")
	if !requireNonEmpty(w, planID, "planId") {
		return
	}

	plan, err := h.plans.GetPlan(userID, planID)
	if err != nil {
		handleServiceError(w, err, "plan")
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (h *FundHandler) GetDecisionTrace(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	if h.decisionTrace == nil {
		handleServiceError(w, ErrNotImplemented, "decision trace")
		return
	}
	tradingDate := strings.TrimSpace(r.URL.Query().Get("date"))
	planID := strings.TrimSpace(r.URL.Query().Get("planId"))
	trace, err := h.decisionTrace.GetDecisionTrace(userID, fundID, tradingDate, planID)
	if err != nil {
		handleServiceError(w, err, "decision trace")
		return
	}
	writeJSON(w, http.StatusOK, trace)
}

func (h *FundHandler) ApprovePlan(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	planID := pathValue(r, "planId")
	if !requireNonEmpty(w, planID, "planId") {
		return
	}

	plan, err := h.plans.ApprovePlan(userID, planID)
	if err != nil {
		handleServiceError(w, err, "plan")
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

type rejectPlanRequest struct {
	Reason string `json:"reason"`
}

func (h *FundHandler) RejectPlan(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	planID := pathValue(r, "planId")
	if !requireNonEmpty(w, planID, "planId") {
		return
	}

	var req rejectPlanRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if !requireNonEmpty(w, req.Reason, "reason") {
		return
	}

	plan, err := h.plans.RejectPlan(userID, planID, req.Reason)
	if err != nil {
		handleServiceError(w, err, "plan")
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (h *FundHandler) RefreshPlanQuote(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	planID := pathValue(r, "planId")
	if !requireNonEmpty(w, planID, "planId") {
		return
	}

	plan, err := h.plans.RefreshPlanQuote(r.Context(), userID, planID)
	if err != nil {
		handleServiceError(w, err, "plan")
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// ===========================================================================
// 4. Trading & Portfolio
// ===========================================================================

func (h *FundHandler) ListTrades(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}

	from, err := parseOptionalTime(r, "from")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid 'from' parameter", "expected RFC 3339 format: "+err.Error())
		return
	}
	to, err := parseOptionalTime(r, "to")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid 'to' parameter", "expected RFC 3339 format: "+err.Error())
		return
	}
	if from != nil && to != nil && to.Before(*from) {
		writeError(w, http.StatusBadRequest, "invalid range", "'to' must be greater than or equal to 'from'")
		return
	}

	limit, offset := parseListLimitOffset(r, 100, 1000)
	// exclude_child_slices=true asks the service to hide rows
	// that are children of a multi-slice parent so the UI list
	// view stays at "one row per plan_action" when child-splitting
	// is on. Defaults to false to preserve the pre-T4 contract for
	// any legacy client that doesn't pass the flag (analytics
	// dumps, backups, etc.). Accepted truthy values follow Go's
	// strconv.ParseBool ("1", "true", "t", "TRUE", ...).
	excludeChildSlices := false
	if raw := r.URL.Query().Get("exclude_child_slices"); raw != "" {
		if parsed, perr := strconv.ParseBool(raw); perr == nil {
			excludeChildSlices = parsed
		}
	}
	trades, err := h.trades.ListTrades(userID, fundID, from, to, limit, offset, excludeChildSlices)
	if err != nil {
		handleServiceError(w, err, "trade")
		return
	}
	writeJSON(w, http.StatusOK, trades)
}

// ListTradeChildren serves the drilldown panel: given a parent
// trade ID, return its child slices in created_at ASC order so
// the operator reads TWAP slices in chronological execution
// order. Authz is enforced by the underlying service via the
// same fund-access rules as ListTrades. A parent with no
// children (legacy / non-split row) returns 200 OK with an
// empty array.
func (h *FundHandler) ListTradeChildren(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	tradeID := pathValue(r, "tradeId")
	if !requireNonEmpty(w, tradeID, "tradeId") {
		return
	}

	children, err := h.trades.ListTradeChildren(userID, fundID, tradeID)
	if err != nil {
		handleServiceError(w, err, "trade children")
		return
	}
	if children == nil {
		// Avoid leaking the Go nil → "null" JSON ambiguity. The
		// frontend renders [].length so this matters.
		children = []Trade{}
	}
	writeJSON(w, http.StatusOK, children)
}

func (h *FundHandler) GetPortfolio(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}

	positions, err := h.trades.GetPortfolio(userID, fundID)
	if err != nil {
		handleServiceError(w, err, "portfolio")
		return
	}
	writeJSON(w, http.StatusOK, positions)
}

func (h *FundHandler) GetNAVHistory(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}

	from, err := parseOptionalTime(r, "from")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid 'from' parameter", "expected RFC 3339 format: "+err.Error())
		return
	}
	to, err := parseOptionalTime(r, "to")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid 'to' parameter", "expected RFC 3339 format: "+err.Error())
		return
	}
	if from != nil && to != nil && to.Before(*from) {
		writeError(w, http.StatusBadRequest, "invalid range", "'to' must be greater than or equal to 'from'")
		return
	}

	history, err := h.trades.GetNAVHistory(userID, fundID, from, to)
	if err != nil {
		handleServiceError(w, err, "nav history")
		return
	}
	writeJSON(w, http.StatusOK, history)
}

func (h *FundHandler) GetPnLAttribution(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}

	from, err := parseOptionalTime(r, "from")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid 'from' parameter", "expected RFC 3339 format: "+err.Error())
		return
	}
	to, err := parseOptionalTime(r, "to")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid 'to' parameter", "expected RFC 3339 format: "+err.Error())
		return
	}
	// Reject inverted ranges up front. Without this guard the underlying
	// SQL just returns an empty set or — worse — a non-zero number when
	// the attribution math sums across "no rows" plus an unrealized delta
	// that straddles the boundary, which silently gives the user a
	// believable-looking but meaningless figure.
	if from != nil && to != nil && to.Before(*from) {
		writeError(w, http.StatusBadRequest, "invalid range", "'to' must be greater than or equal to 'from'")
		return
	}

	attribution, err := h.trades.GetPnLAttribution(userID, fundID, from, to)
	if err != nil {
		handleServiceError(w, err, "pnl attribution")
		return
	}
	writeJSON(w, http.StatusOK, attribution)
}

// GetTodayPnL serves the dashboard "今日盈亏" tile. See TodayPnL for
// the per-field semantics and why a dedicated endpoint exists
// (NAV-diff computed on the frontend silently breaks when today's
// intra-day NAV snapshot is rewritten by a settle/PM-plan run, or
// when yesterday's snapshot is missing because no trade activity
// happened that day).
func (h *FundHandler) GetTodayPnL(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	today, err := h.trades.GetTodayPnL(userID, fundID)
	if err != nil {
		handleServiceError(w, err, "today pnl")
		return
	}
	writeJSON(w, http.StatusOK, today)
}

// GetStrategyAttribution surfaces the Phase 3A-5 cross-tab of
// closed-lot statistics together with the most-recent batch of
// derived lessons. Returns 503 when the AttributionService dep
// hasn't been wired (smoke / legacy deployments).
func (h *FundHandler) GetStrategyAttribution(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	if h.attribution == nil {
		writeError(w, http.StatusServiceUnavailable, "attribution_unavailable", "strategy attribution is not configured on this server")
		return
	}
	days := parseLimitParam(r, "days", 30, 365)
	resp, err := h.attribution.GetAttribution(userID, fundID, days)
	if err != nil {
		handleServiceError(w, err, "strategy attribution")
		return
	}
	if resp == nil {
		resp = &AttributionResponse{
			FundID:         fundID,
			BySleeve:       []SleeveStatDTO{},
			ByRegime:       []RegimeStatDTO{},
			BySleeveRegime: []SleeveRegimeStatDTO{},
			Lessons:        []AttributionLessonDTO{},
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// RefreshStrategyAttribution is the operator-triggered companion
// to GetStrategyAttribution. It forces the attribution service
// to rebuild the report AND persist any new lessons immediately
// rather than waiting for the daily review hook. Useful right
// after a backfill, or whenever the operator wants to verify the
// closed-loop learning pipeline is producing memories.
//
// Returns the same shape as GetStrategyAttribution so the UI can
// drop-in render the response, plus the lessons that were freshly
// persisted in this run.
func (h *FundHandler) RefreshStrategyAttribution(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	if h.attribution == nil {
		writeError(w, http.StatusServiceUnavailable, "attribution_unavailable", "strategy attribution is not configured on this server")
		return
	}
	days := parseLimitParam(r, "days", 30, 365)
	resp, err := h.attribution.RefreshAttribution(userID, fundID, days)
	if err != nil {
		handleServiceError(w, err, "strategy attribution refresh")
		return
	}
	if resp == nil {
		resp = &AttributionResponse{
			FundID:         fundID,
			BySleeve:       []SleeveStatDTO{},
			ByRegime:       []RegimeStatDTO{},
			BySleeveRegime: []SleeveRegimeStatDTO{},
			Lessons:        []AttributionLessonDTO{},
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ===========================================================================
// 5. Workflow Control
// ===========================================================================

func (h *FundHandler) StartWorkflow(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	status, err := h.workflow.StartWorkflow(userID, fundID)
	if err != nil {
		handleServiceError(w, err, "workflow")
		return
	}
	writeJSON(w, http.StatusAccepted, status)
}

func (h *FundHandler) TriggerStep(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	step := r.URL.Query().Get("step")
	if !requireNonEmpty(w, step, "step") {
		return
	}
	status, err := h.workflow.TriggerStep(userID, fundID, step)
	if err != nil {
		handleServiceError(w, err, "workflow")
		return
	}
	writeJSON(w, http.StatusAccepted, status)
}

func (h *FundHandler) GetWorkflowStatus(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	status, err := h.workflow.GetStatus(userID, fundID)
	if err != nil {
		handleServiceError(w, err, "workflow")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// GetWorkflowNextRun returns the next-scheduler-wake info for the fund.
// Behind /api/funds/:fundId/workflow/next-run; consumed by the
// Decision Center + Agent Learning header banners so users can see
// when the next automated decision will run without reading logs.
func (h *FundHandler) GetWorkflowNextRun(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	info, err := h.workflow.GetNextRun(userID, fundID)
	if err != nil {
		handleServiceError(w, err, "workflow next-run")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (h *FundHandler) GetLLMUsageVisibility(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}

	now := time.Now().UTC()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	to := now.Add(time.Second)
	if value := strings.TrimSpace(r.URL.Query().Get("from")); value != "" {
		parsed, err := parseDateOrDateTime(value)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'from' parameter", "expected YYYY-MM-DD or RFC3339: "+err.Error())
			return
		}
		from = parsed
	}
	if value := strings.TrimSpace(r.URL.Query().Get("to")); value != "" {
		parsed, err := parseDateOrDateTime(value)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'to' parameter", "expected YYYY-MM-DD or RFC3339: "+err.Error())
			return
		}
		to = parsed
		if len(value) == len("2006-01-02") {
			to = to.AddDate(0, 0, 1)
		}
	}
	if !to.After(from) {
		writeError(w, http.StatusBadRequest, "invalid time range", "'to' must be after 'from'")
		return
	}

	visibility, err := h.teams.GetLLMUsageVisibility(userID, fundID, from, to)
	if err != nil {
		handleServiceError(w, err, "llm usage")
		return
	}
	writeJSON(w, http.StatusOK, visibility)
}

func (h *FundHandler) ListAuditLogs(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	limit := 50
	if value := strings.TrimSpace(r.URL.Query().Get("limit")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 || parsed > 200 {
			writeError(w, http.StatusBadRequest, "invalid 'limit' parameter", "expected integer in range 1-200")
			return
		}
		limit = parsed
	}
	if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("format")), "csv") {
		logs, err := h.teams.ExportAuditLogs(userID, fundID, limit)
		if err != nil {
			handleServiceError(w, err, "audit logs")
			return
		}
		writeAuditLogsCSV(w, logs)
		return
	}
	logs, err := h.teams.ListAuditLogs(userID, fundID, limit)
	if err != nil {
		handleServiceError(w, err, "audit logs")
		return
	}
	writeJSON(w, http.StatusOK, logs)
}

func writeAuditLogsCSV(w http.ResponseWriter, logs *AuditLogResponse) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="audit-log.csv"`)
	w.WriteHeader(http.StatusOK)
	writer := csv.NewWriter(w)
	defer writer.Flush()
	_ = writer.Write([]string{"id", "created_at", "actor_user_id", "action", "resource_type", "resource_id", "details_json"})
	if logs == nil {
		return
	}
	for _, entry := range logs.Entries {
		_ = writer.Write([]string{
			entry.ID,
			entry.CreatedAt.Format(time.RFC3339),
			entry.ActorUserID,
			entry.Action,
			entry.ResourceType,
			entry.ResourceID,
			string(entry.Details),
		})
	}
}

func parseDateOrDateTime(value string) (time.Time, error) {
	if parsed, err := time.Parse("2006-01-02", value); err == nil {
		return parsed.UTC(), nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

// ===========================================================================
// 6. Memory
// ===========================================================================

func (h *FundHandler) GetMemory(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	layer := strings.TrimSpace(r.URL.Query().Get("layer"))
	if !requireNonEmpty(w, layer, "layer") {
		return
	}
	agentID := strings.TrimSpace(r.URL.Query().Get("agentId"))
	ctx, err := h.memory.GetMemory(userID, fundID, layer, agentID)
	if err != nil {
		handleServiceError(w, err, "memory")
		return
	}
	writeJSON(w, http.StatusOK, ctx)
}

// ListAgentSkills returns the candidate + approved skill library for an
// agent. The list combines reflection-derived proposals (status="proposed")
// with manually authored entries (status="approved" / empty) so the UI can
// render them as a single timeline grouped by status.
func (h *FundHandler) ListAgentSkills(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	agentID := pathValue(r, "agentId")
	if !requireNonEmpty(w, agentID, "agentId") {
		return
	}
	if h.agentSkills == nil {
		writeError(w, http.StatusServiceUnavailable, "agent_skills_unavailable", "agent skill service is not configured on this server")
		return
	}
	list, err := h.agentSkills.ListSkills(userID, agentID)
	if err != nil {
		handleServiceError(w, err, "agent_skills")
		return
	}
	if list == nil {
		list = &AgentSkillList{AgentID: agentID, Skills: []AgentSkillEntry{}}
	}
	if list.Skills == nil {
		list.Skills = []AgentSkillEntry{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ApproveAgentSkill flips a proposed skill into the active library. It is
// idempotent at the service layer: approving an already-approved skill
// just returns the entry unchanged. The route is POST (not PUT) because
// the user-visible action is "approve this proposal" rather than "replace
// the entry payload".
func (h *FundHandler) ApproveAgentSkill(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	agentID := pathValue(r, "agentId")
	skillKey := pathValue(r, "skillKey")
	if !requireNonEmpty(w, agentID, "agentId") || !requireNonEmpty(w, skillKey, "skillKey") {
		return
	}
	if h.agentSkills == nil {
		writeError(w, http.StatusServiceUnavailable, "agent_skills_unavailable", "agent skill service is not configured on this server")
		return
	}
	entry, err := h.agentSkills.ApproveSkill(userID, agentID, skillKey)
	if err != nil {
		handleServiceError(w, err, "agent_skills")
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// RejectAgentSkill removes the skill from the agent's library. We respond
// 204 No Content on success — the UI already has the candidate row from
// the prior list call and just needs an ack.
func (h *FundHandler) RejectAgentSkill(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	agentID := pathValue(r, "agentId")
	skillKey := pathValue(r, "skillKey")
	if !requireNonEmpty(w, agentID, "agentId") || !requireNonEmpty(w, skillKey, "skillKey") {
		return
	}
	if h.agentSkills == nil {
		writeError(w, http.StatusServiceUnavailable, "agent_skills_unavailable", "agent skill service is not configured on this server")
		return
	}
	if err := h.agentSkills.RejectSkill(userID, agentID, skillKey); err != nil {
		handleServiceError(w, err, "agent_skills")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListReflections returns the most recent long-term reflections for a fund.
// This is a thin convenience wrapper over the memory layer that:
//
//   - Defaults limit to 50 (caller may override via ?limit=N; max 200).
//   - Returns 503 Service Unavailable when no ReflectionService is wired,
//     so the frontend can degrade gracefully (most likely a config issue).
//   - Maps the underlying repository's "not found" to an empty list, since
//     a fund with no reflections is a normal day-one state, not an error.
func (h *FundHandler) ListReflections(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	if h.reflections == nil {
		writeError(w, http.StatusServiceUnavailable, "reflection_unavailable", "long-term reflections are not configured on this server")
		return
	}
	limit := parseLimitParam(r, "limit", 50, 200)
	list, err := h.reflections.ListReflections(userID, fundID, limit)
	if err != nil {
		handleServiceError(w, err, "reflections")
		return
	}
	if list == nil {
		list = &ReflectionList{FundID: fundID, Items: []ReflectionItem{}}
	}
	if list.Items == nil {
		list.Items = []ReflectionItem{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *FundHandler) SearchMemory(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	layer := strings.TrimSpace(r.URL.Query().Get("layer"))
	if !requireNonEmpty(w, layer, "layer") {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if !requireNonEmpty(w, query, "q") {
		return
	}
	entries, err := h.memory.SearchMemory(userID, fundID, layer, query)
	if err != nil {
		handleServiceError(w, err, "memory")
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// ===========================================================================
// 7. Market Data
// ===========================================================================

func (h *FundHandler) GetMarketQuotes(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	symbols := splitCSVQuery(r.URL.Query().Get("symbols"))
	payload, err := h.market.GetQuotes(userID, fundID, symbols)
	if err != nil {
		handleServiceError(w, err, "market quotes")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (h *FundHandler) GetMarketResearch(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	symbol := strings.TrimSpace(r.URL.Query().Get("symbol"))
	if !requireNonEmpty(w, symbol, "symbol") {
		return
	}
	limit := parseIntDefault(r, "limit", 5)
	payload, err := h.market.GetResearch(userID, fundID, symbol, limit)
	if err != nil {
		handleServiceError(w, err, "market research")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (h *FundHandler) GetMarketNews(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	symbol := strings.TrimSpace(r.URL.Query().Get("symbol"))
	if !requireNonEmpty(w, symbol, "symbol") {
		return
	}
	limit := parseIntDefault(r, "limit", 5)
	payload, err := h.market.GetNews(userID, fundID, symbol, limit)
	if err != nil {
		handleServiceError(w, err, "market news")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (h *FundHandler) GetMarketNewsDigest(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	symbols := splitCSVQuery(r.URL.Query().Get("symbols"))
	limit := parseIntDefault(r, "limit", 10)
	payload, err := h.market.GetNewsDigest(userID, fundID, symbols, limit)
	if err != nil {
		handleServiceError(w, err, "market news digest")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func splitCSVQuery(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		key := strings.ToUpper(trimmed)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

// ===========================================================================
// 8. A/B Tests
// ===========================================================================

func (h *FundHandler) ListABTests(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	fundID := pathValue(r, "fundId")
	if !requireNonEmpty(w, fundID, "fundId") {
		return
	}
	tests, err := h.abtests.ListTests(userID, fundID)
	if err != nil {
		handleServiceError(w, err, "A/B test")
		return
	}
	writeJSON(w, http.StatusOK, tests)
}

func (h *FundHandler) CreateABTest(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	var input CreateABTestInput
	if err := decodeBody(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	test, err := h.abtests.CreateTest(userID, input)
	if err != nil {
		handleServiceError(w, err, "A/B test")
		return
	}
	writeJSON(w, http.StatusCreated, test)
}

func (h *FundHandler) GetABTest(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	testID := pathValue(r, "testId")
	if !requireNonEmpty(w, testID, "testId") {
		return
	}
	test, err := h.abtests.GetTest(userID, testID)
	if err != nil {
		handleServiceError(w, err, "A/B test")
		return
	}
	writeJSON(w, http.StatusOK, test)
}

func (h *FundHandler) StartABTest(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	testID := pathValue(r, "testId")
	if !requireNonEmpty(w, testID, "testId") {
		return
	}
	test, err := h.abtests.StartTest(userID, testID)
	if err != nil {
		handleServiceError(w, err, "A/B test")
		return
	}
	writeJSON(w, http.StatusOK, test)
}

func (h *FundHandler) StopABTest(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	testID := pathValue(r, "testId")
	if !requireNonEmpty(w, testID, "testId") {
		return
	}
	test, err := h.abtests.StopTest(userID, testID)
	if err != nil {
		handleServiceError(w, err, "A/B test")
		return
	}
	writeJSON(w, http.StatusOK, test)
}

func (h *FundHandler) AnalyzeABTest(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	testID := pathValue(r, "testId")
	if !requireNonEmpty(w, testID, "testId") {
		return
	}
	test, err := h.abtests.AnalyzeTest(userID, testID)
	if err != nil {
		handleServiceError(w, err, "A/B test")
		return
	}
	writeJSON(w, http.StatusOK, test)
}

func (h *FundHandler) PromoteABTestLearning(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	testID := pathValue(r, "testId")
	if !requireNonEmpty(w, testID, "testId") {
		return
	}
	var input PromoteABTestLearningInput
	if err := decodeBody(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	result, err := h.abtests.PromoteLearning(userID, testID, input)
	if err != nil {
		handleServiceError(w, err, "A/B learning promotion")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *FundHandler) ListABTestLearningPromotions(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	testID := pathValue(r, "testId")
	if !requireNonEmpty(w, testID, "testId") {
		return
	}
	promotions, err := h.abtests.ListLearningPromotions(userID, testID)
	if err != nil {
		handleServiceError(w, err, "A/B learning promotions")
		return
	}
	writeJSON(w, http.StatusOK, promotions)
}

func (h *FundHandler) RollbackABTestLearningPromotion(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	testID := pathValue(r, "testId")
	promotionID := pathValue(r, "promotionId")
	if !requireNonEmpty(w, testID, "testId") || !requireNonEmpty(w, promotionID, "promotionId") {
		return
	}
	result, err := h.abtests.RollbackLearningPromotion(userID, testID, promotionID)
	if err != nil {
		handleServiceError(w, err, "A/B learning promotion rollback")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// ===========================================================================
// 8. Marketplace
// ===========================================================================

type createMarketplaceListingRequest struct {
	FundID        string `json:"fundId"`
	AgentID       string `json:"agentId"`
	AskPriceMinor int64  `json:"askPriceMinor"`
	Currency      string `json:"currency,omitempty"`
}

type createMarketplaceBidRequest struct {
	ListingID     string `json:"listingId"`
	BidPriceMinor int64  `json:"bidPriceMinor"`
	Currency      string `json:"currency,omitempty"`
}

type purchaseMarketplaceListingRequest struct {
	ListingID   string `json:"listingId"`
	BuyerFundID string `json:"buyerFundId,omitempty"`
}

func (h *FundHandler) ListMarketplaceListings(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	limit, offset := parseListLimitOffset(r, 50, 500)
	listings, err := h.marketplace.ListListings(userID, limit, offset)
	if err != nil {
		handleServiceError(w, err, "marketplace listing")
		return
	}
	writeJSON(w, http.StatusOK, listings)
}

func (h *FundHandler) ListMyMarketplaceListings(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	limit, offset := parseListLimitOffset(r, 50, 500)
	listings, err := h.marketplace.ListMyListings(userID, limit, offset)
	if err != nil {
		handleServiceError(w, err, "marketplace listing")
		return
	}
	writeJSON(w, http.StatusOK, listings)
}

func (h *FundHandler) CreateMarketplaceListing(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}

	if !RequireKYC(w, r, "tier1_basic") {
		return
	}

	var req createMarketplaceListingRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if !requireNonEmpty(w, req.FundID, "fundId") || !requireNonEmpty(w, req.AgentID, "agentId") {
		return
	}
	if req.AskPriceMinor <= 0 {
		writeError(w, http.StatusBadRequest, "validation error", "askPriceMinor must be positive")
		return
	}
	listing, err := h.marketplace.CreateListing(userID, CreateMarketplaceListingInput{
		FundID:        req.FundID,
		AgentID:       req.AgentID,
		AskPriceMinor: req.AskPriceMinor,
		Currency:      req.Currency,
	})
	if err != nil {
		handleServiceError(w, err, "marketplace listing")
		return
	}
	writeJSON(w, http.StatusCreated, listing)
}

func (h *FundHandler) CancelMarketplaceListing(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	listingID := pathValue(r, "listingId")
	if !requireNonEmpty(w, listingID, "listingId") {
		return
	}
	if err := h.marketplace.CancelListing(userID, listingID); err != nil {
		handleServiceError(w, err, "marketplace listing")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *FundHandler) ListMarketplaceBids(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	listingID := pathValue(r, "listingId")
	if !requireNonEmpty(w, listingID, "listingId") {
		return
	}
	limit, offset := parseListLimitOffset(r, 50, 500)
	bids, err := h.marketplace.ListBids(userID, listingID, limit, offset)
	if err != nil {
		handleServiceError(w, err, "marketplace bid")
		return
	}
	writeJSON(w, http.StatusOK, bids)
}

func (h *FundHandler) CreateMarketplaceBid(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	var req createMarketplaceBidRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if !requireNonEmpty(w, req.ListingID, "listingId") {
		return
	}
	if req.BidPriceMinor <= 0 {
		writeError(w, http.StatusBadRequest, "validation error", "bidPriceMinor must be positive")
		return
	}
	bid, err := h.marketplace.CreateBid(userID, CreateMarketplaceBidInput{
		ListingID:     req.ListingID,
		BidPriceMinor: req.BidPriceMinor,
		Currency:      req.Currency,
	})
	if err != nil {
		handleServiceError(w, err, "marketplace bid")
		return
	}
	writeJSON(w, http.StatusCreated, bid)
}

func (h *FundHandler) PurchaseMarketplaceListing(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}

	if !RequireKYC(w, r, "tier1_basic") {
		return
	}

	var req purchaseMarketplaceListingRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if !requireNonEmpty(w, req.ListingID, "listingId") {
		return
	}
	order, err := h.marketplace.PurchaseListing(userID, PurchaseMarketplaceListingInput{
		ListingID:   req.ListingID,
		BuyerFundID: req.BuyerFundID,
	})
	if err != nil {
		handleServiceError(w, err, "marketplace order")
		return
	}
	writeJSON(w, http.StatusCreated, order)
}

// ---------------------------------------------------------------------------
// Marketplace auctions (F5) — handlers
// ---------------------------------------------------------------------------

type createAuctionListingRequest struct {
	FundID             string    `json:"fundId"`
	AgentID            string    `json:"agentId"`
	StartingPriceMinor int64     `json:"startingPriceMinor"`
	ReserveMinor       int64     `json:"reserveMinor,omitempty"`
	MinIncrementMinor  int64     `json:"minIncrementMinor,omitempty"`
	AntiSnipeSeconds   int       `json:"antiSnipeSeconds,omitempty"`
	Currency           string    `json:"currency,omitempty"`
	StartsAt           time.Time `json:"startsAt"`
	EndsAt             time.Time `json:"endsAt"`
}

type placeAuctionBidRequest struct {
	BidPriceMinor int64  `json:"bidPriceMinor"`
	Currency      string `json:"currency,omitempty"`
}

// auctionUnavailable returns true after writing a 503 when the auction
// service has not been wired (kept consistent with reflections/skills).
func (h *FundHandler) auctionUnavailable(w http.ResponseWriter) bool {
	if h.auctions == nil {
		writeError(w, http.StatusServiceUnavailable, "auctions unavailable", "marketplace auction service is not enabled")
		return true
	}
	return false
}

func (h *FundHandler) ListAuctions(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	if h.auctionUnavailable(w) {
		return
	}
	limit, offset := parseListLimitOffset(r, 50, 500)
	auctions, err := h.auctions.ListAuctions(userID, limit, offset)
	if err != nil {
		handleServiceError(w, err, "auction listing")
		return
	}
	writeJSON(w, http.StatusOK, auctions)
}

func (h *FundHandler) GetAuction(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	if h.auctionUnavailable(w) {
		return
	}
	listingID := pathValue(r, "listingId")
	if !requireNonEmpty(w, listingID, "listingId") {
		return
	}
	auction, err := h.auctions.GetAuction(userID, listingID)
	if err != nil {
		handleServiceError(w, err, "auction listing")
		return
	}
	writeJSON(w, http.StatusOK, auction)
}

func (h *FundHandler) CreateAuction(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	if h.auctionUnavailable(w) {
		return
	}
	if !RequireKYC(w, r, "tier1_basic") {
		return
	}
	var req createAuctionListingRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if !requireNonEmpty(w, req.FundID, "fundId") || !requireNonEmpty(w, req.AgentID, "agentId") {
		return
	}
	if req.StartingPriceMinor <= 0 {
		writeError(w, http.StatusBadRequest, "validation error", "startingPriceMinor must be positive")
		return
	}
	if req.EndsAt.IsZero() {
		writeError(w, http.StatusBadRequest, "validation error", "endsAt is required")
		return
	}
	listing, err := h.auctions.CreateAuction(userID, CreateAuctionListingInput{
		FundID:             req.FundID,
		AgentID:            req.AgentID,
		StartingPriceMinor: req.StartingPriceMinor,
		ReserveMinor:       req.ReserveMinor,
		MinIncrementMinor:  req.MinIncrementMinor,
		AntiSnipeSeconds:   req.AntiSnipeSeconds,
		Currency:           req.Currency,
		StartsAt:           req.StartsAt,
		EndsAt:             req.EndsAt,
	})
	if err != nil {
		handleServiceError(w, err, "auction listing")
		return
	}
	writeJSON(w, http.StatusCreated, listing)
}

func (h *FundHandler) PlaceAuctionBid(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	if h.auctionUnavailable(w) {
		return
	}
	if !RequireKYC(w, r, "tier1_basic") {
		return
	}
	listingID := pathValue(r, "listingId")
	if !requireNonEmpty(w, listingID, "listingId") {
		return
	}
	var req placeAuctionBidRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if req.BidPriceMinor <= 0 {
		writeError(w, http.StatusBadRequest, "validation error", "bidPriceMinor must be positive")
		return
	}
	bid, auction, err := h.auctions.PlaceBid(userID, PlaceAuctionBidInput{
		ListingID:     listingID,
		BidPriceMinor: req.BidPriceMinor,
		Currency:      req.Currency,
	})
	if err != nil {
		handleServiceError(w, err, "auction bid")
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		Bid     *AuctionBid     `json:"bid"`
		Auction *AuctionListing `json:"auction,omitempty"`
	}{Bid: bid, Auction: auction})
}

func (h *FundHandler) SettleAuction(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireAuthenticatedUserID(w, r)
	if !ok {
		return
	}
	if h.auctionUnavailable(w) {
		return
	}
	listingID := pathValue(r, "listingId")
	if !requireNonEmpty(w, listingID, "listingId") {
		return
	}
	result, err := h.auctions.SettleAuction(userID, listingID)
	if err != nil {
		handleServiceError(w, err, "auction settlement")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// ===========================================================================
// Sentinel errors for service layer → HTTP status mapping
// ===========================================================================

var (
	ErrNotFound            = errors.New("not found")
	ErrConflict            = errors.New("conflict")
	ErrForbidden           = errors.New("forbidden")
	ErrBadInput            = errors.New("bad input")
	ErrNotImplemented      = errors.New("not implemented")
	ErrUpstreamUnavailable = errors.New("upstream unavailable")
)

func handleServiceError(w http.ResponseWriter, err error, resource string) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, resource+" not found", err.Error())
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, resource+" conflict", err.Error())
	case errors.Is(err, ErrForbidden):
		writeError(w, http.StatusForbidden, resource+" access denied", err.Error())
	case errors.Is(err, ErrBadInput):
		writeError(w, http.StatusBadRequest, "invalid input for "+resource, err.Error())
	case errors.Is(err, ErrNotImplemented):
		writeError(w, http.StatusNotImplemented, resource+" not implemented", err.Error())
	case errors.Is(err, ErrUpstreamUnavailable):
		writeError(w, http.StatusServiceUnavailable, resource+" unavailable", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal error", err.Error())
	}
}

type NAVPoint struct {
	Date             string  `json:"date"`
	NAV              float64 `json:"nav"`
	TotalAssets      float64 `json:"totalAssets"`
	DailyReturn      float64 `json:"dailyReturn"`
	TotalReturn      float64 `json:"totalReturn"`
	AvailableCash    float64 `json:"availableCash"`
	TotalMarketValue float64 `json:"totalMarketValue"`
}

type FundDashboard struct {
	Fund       *Fund           `json:"fund"`
	NavHistory []NAVPoint      `json:"navHistory"`
	Positions  []Position      `json:"positions"`
	Trades     []Trade         `json:"trades"`
	Workflow   *WorkflowStatus `json:"workflow"`
}

var _ = context.Background
