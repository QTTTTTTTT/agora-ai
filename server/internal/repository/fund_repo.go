package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// ErrNotFound is returned when a query yields no rows.
var ErrNotFound = errors.New("repository: record not found")

// ---------------------------------------------------------------------------
// Domain structs — mirror the database schema
// ---------------------------------------------------------------------------

type FundCompany struct {
	ID          string         `json:"id"`
	OwnerUserID string         `json:"ownerUserId"`
	Name        string         `json:"name"`
	Description sql.NullString `json:"description"`
	CreatedAt   time.Time      `json:"createdAt"`
	UpdatedAt   time.Time      `json:"updatedAt"`
}

type Fund struct {
	ID             string          `json:"id"`
	CompanyID      string          `json:"companyId"`
	Name           string          `json:"name"`
	Description    sql.NullString  `json:"description"`
	TradingMode    string          `json:"tradingMode"`
	InitialCapital float64         `json:"initialCapital"`
	CurrentCapital float64         `json:"currentCapital"`
	TotalAssets    float64         `json:"totalAssets"`
	NAV            float64         `json:"nav"`
	Status         string          `json:"status"`
	Config         json.RawMessage `json:"config"`
	// BaseCurrency is the reporting currency for NAV and the
	// cash-ledger summary. P1-4. Defaults to "USD" via the DB
	// migration; old rows continue to behave 1:1 USD until
	// explicitly changed.
	BaseCurrency   string          `json:"baseCurrency"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
}

type InvestmentPlan struct {
	ID                 string          `json:"id"`
	FundID             string          `json:"fundId"`
	TradingDate        time.Time       `json:"tradingDate"`
	Status             string          `json:"status"`
	Reasoning          sql.NullString  `json:"reasoning"`
	RiskScore          sql.NullFloat64 `json:"riskScore"`
	ExpectedReturn     sql.NullFloat64 `json:"expectedReturn"`
	RiskReview         json.RawMessage `json:"riskReview"`
	DiscussionSnapshot json.RawMessage `json:"discussionSnapshot"`
	RoundtableID       sql.NullString  `json:"roundtableId"`
	PMAgentID          sql.NullString  `json:"pmAgentId"`
	// Confidence is the plan-level confidence produced by the LLM
	// decision engine (Phase 2A). NULL when no decision engine ran
	// (fallback heuristic or legacy plan). The auto-execute gate
	// prefers the typed column over the JSON blob in risk_review.
	Confidence sql.NullFloat64 `json:"confidence"`
	// ClientIdempotencyKey (F16) is optional. When set, the Create path
	// performs ON CONFLICT (client_idempotency_key) DO NOTHING + RETURNING
	// id so a retried write returns the original row instead of inserting
	// a duplicate plan for the same workflow step.
	ClientIdempotencyKey sql.NullString `json:"clientIdempotencyKey,omitempty"`
	// BlockContributions (G1 #2) carries the per-plan
	// decision-block attribution: which signal blocks were
	// present in the DecisionInput, which the PM cited in its
	// Reasoning, plus per-block counts and the trace
	// fingerprint signature. Written by the wiring layer via
	// SetBlockContributions after the plan is created. Empty
	// JSON object ('{}') means "writer never ran" (legacy /
	// fallback plans). The shape is intentionally loose so the
	// writer can evolve without another migration.
	BlockContributions json.RawMessage `json:"blockContributions,omitempty"`
	// DecisionSource (Sprint 11.1) is the provenance tag for this
	// plan: llm_pm / llm_three_stage / fallback_no_llm /
	// fallback_after_llm_error / fallback_empty_plan / legacy. NULL
	// in the DB collapses to "legacy" for any plan written before
	// migration 077. Read separately via GetDecisionSource so the
	// existing GetByID SELECT stays unchanged and its test surface
	// is preserved.
	DecisionSource string `json:"decisionSource,omitempty"`
	// FallbackReason (Sprint 11.1) is the JSONB
	// errorclass.Detail payload — populated only when
	// DecisionSource starts with "fallback_". The Summary key is
	// the raw provider message and MUST be stripped from
	// non-admin API responses by the caller.
	FallbackReason json.RawMessage `json:"fallbackReason,omitempty"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
}

type PlanAction struct {
	ID                 string          `json:"id"`
	PlanID             string          `json:"planId"`
	InstrumentKey      string          `json:"instrumentKey"`
	Symbol             string          `json:"symbol"`
	Market             sql.NullString  `json:"market"`
	Exchange           sql.NullString  `json:"exchange"`
	AssetClass         sql.NullString  `json:"assetClass"`
	InstrumentType     sql.NullString  `json:"instrumentType"`
	Action             string          `json:"action"`
	PositionSide       sql.NullString  `json:"positionSide"`
	OpenClose          sql.NullString  `json:"openClose"`
	Quantity           sql.NullFloat64 `json:"quantity"`
	Price              sql.NullFloat64 `json:"price"`
	Amount             sql.NullFloat64 `json:"amount"`
	StopLoss           sql.NullFloat64 `json:"stopLoss"`
	TakeProfit         sql.NullFloat64 `json:"takeProfit"`
	Reasoning          sql.NullString  `json:"reasoning"`
	Confidence         sql.NullFloat64 `json:"confidence"`
	SupportedBy        []string        `json:"supportedBy"`
	OpposedBy          []string        `json:"opposedBy"`
	ExecutionStatus    string          `json:"executionStatus"`
	SortOrder          int             `json:"sortOrder"`
	QuoteCurrency      sql.NullString  `json:"quoteCurrency"`
	SettlementCurrency sql.NullString  `json:"settlementCurrency"`
	MarginMode         sql.NullString  `json:"marginMode"`
	Leverage           sql.NullFloat64 `json:"leverage"`
	ContractMultiplier sql.NullFloat64 `json:"contractMultiplier"`
	ExpiryDate         sql.NullTime    `json:"expiryDate"`
	ReduceOnly         sql.NullBool    `json:"reduceOnly"`
	// QuoteRefreshedAt is the timestamp of the most recent quote
	// refresh applied to this action via POST /api/plans/{id}/refresh-
	// quote. NULL means the price still reflects the original plan-
	// generation quote.
	QuoteRefreshedAt sql.NullTime `json:"quoteRefreshedAt"`
	// AutoExecutedAt is set by the runtimeApprovalGateway when the
	// per-fund auto-execute toggle approves the plan without human
	// intervention. NULL means the action was human-approved (or has
	// not yet been approved). Powers the daily-cumulative gate and the
	// audit/replay distinction between manual and autonomous approvals.
	AutoExecutedAt sql.NullTime `json:"autoExecutedAt"`

	// ------------------------------------------------------------------
	// Phase 3A-1 attribution columns. These are stamped onto the
	// plan_action at decision time and propagated into the lot
	// ledger (position_lots + closed_lots) when the fill records.
	// Together they let the strategy attribution agent answer
	// questions like "which sleeve made money last 30 days in
	// trend_up regimes". They are nullable so legacy rows and the
	// in-progress LLM-only path keep working.
	// ------------------------------------------------------------------
	// Sleeve groups actions by the family of strategy that
	// originated them: "llm_pm", "trend", "mean_reversion",
	// "momentum", "manual". The PerformanceAnalyst groups
	// realised P&L by this column.
	Sleeve sql.NullString `json:"sleeve"`
	// RegimeTag is the market-regime classification at decision
	// time ("trend_up", "trend_down", "range", "chop"). Populated
	// from the indicator snapshot in Phase 3A-3.
	RegimeTag sql.NullString `json:"regimeTag"`
	// SignalSource is the concrete signal name within the sleeve
	// — e.g. "donchian_20", "dual_ma_50_200", "bb_reversion".
	// "llm_pm" is the default when the LLMDecisionEngine generated
	// the action without a deterministic strategy second-opinion.
	SignalSource sql.NullString `json:"signalSource"`
	// ExitReason records WHY a sell/reduce was generated. Free-form
	// at first; values used by the exit manager (Phase 3A-2) and
	// the LLM PM:
	//   - "stop_loss" / "take_profit" / "trailing" / "time_stop"
	//   - "rebalance"   - portfolio drift trim
	//   - "llm_decision" - the LLM PM proposed the exit
	//   - "manual"      - human-initiated override
	// NULL for buy/add actions.
	ExitReason sql.NullString `json:"exitReason"`

	// Sprint 1 / S6 execution-strategy hint. NULL = "immediate"
	// (legacy single-shot fill at the live quote). Other valid
	// values: "twap", "vwap", "limit" — see migration 041 and
	// runtimeTradingEngine.executeStrategyDispatch.
	Strategy sql.NullString `json:"strategy"`
}

type TradeExecution struct {
	ID                 string          `json:"id"`
	FundID             string          `json:"fundId"`
	PlanID             sql.NullString  `json:"planId"`
	PlanActionID       sql.NullString  `json:"planActionId"`
	InstrumentKey      string          `json:"instrumentKey"`
	Symbol             string          `json:"symbol"`
	Market             sql.NullString  `json:"market"`
	Exchange           sql.NullString  `json:"exchange"`
	AssetClass         sql.NullString  `json:"assetClass"`
	InstrumentType     sql.NullString  `json:"instrumentType"`
	Side               string          `json:"side"`
	PositionSide       sql.NullString  `json:"positionSide"`
	OpenClose          sql.NullString  `json:"openClose"`
	OrderType          string          `json:"orderType"`
	Quantity           float64         `json:"quantity"`
	Price              sql.NullFloat64 `json:"price"`
	Amount             sql.NullFloat64 `json:"amount"`
	FilledQty          float64         `json:"filledQty"`
	FilledPrice        sql.NullFloat64 `json:"filledPrice"`
	FeeCommission      float64         `json:"feeCommission"`
	FeeStampTax        float64         `json:"feeStampTax"`
	FeeTransfer        float64         `json:"feeTransfer"`
	TradingMode        string          `json:"tradingMode"`
	BrokerOrderID      sql.NullString  `json:"brokerOrderId"`
	MCPServerID        sql.NullString  `json:"mcpServerId"`
	Status             string          `json:"status"`
	ExecutedAt         sql.NullTime    `json:"executedAt"`
	QuoteCurrency      sql.NullString  `json:"quoteCurrency"`
	SettlementCurrency sql.NullString  `json:"settlementCurrency"`
	MarginMode         sql.NullString  `json:"marginMode"`
	Leverage           sql.NullFloat64 `json:"leverage"`
	ContractMultiplier sql.NullFloat64 `json:"contractMultiplier"`
	ExpiryDate         sql.NullTime    `json:"expiryDate"`
	ReduceOnly         sql.NullBool    `json:"reduceOnly"`
	// SlippagePct is the signed fractional drift from the plan
	// reference price (Price) to the actual fill price (FilledPrice):
	// (FilledPrice - Price) / Price. NULL for executions predating the
	// SlippageGuard rollout, sells (exempt by design), or non-priced
	// fills (e.g. sell-all of an odd lot).
	SlippagePct sql.NullFloat64 `json:"slippagePct"`

	// P0-2 broker fields. All nullable so legacy rows written before
	// migration 051 keep working unchanged.
	//
	// StopPrice is the trigger price for stop / stop_limit orders.
	StopPrice sql.NullFloat64 `json:"stopPrice"`
	// TrailAmount / TrailPercent describe a trailing-stop in absolute
	// or fractional terms (mutually exclusive: only one is set).
	TrailAmount  sql.NullFloat64 `json:"trailAmount"`
	TrailPercent sql.NullFloat64 `json:"trailPercent"`
	// DisplayQty is the visible portion of an iceberg order.
	DisplayQty sql.NullFloat64 `json:"displayQty"`
	// TimeInForce is one of broker.TimeInForce: day / gtc / ioc / fok
	// / gtd / opg. NULL falls back to the engine default ("day").
	TimeInForce sql.NullString `json:"timeInForce"`
	// GoodTillDate is the expiry timestamp for time_in_force = gtd.
	GoodTillDate sql.NullTime `json:"goodTillDate"`
	// ParentTradeID points at the bracket parent — set on stop-loss /
	// take-profit / OCO child orders so the engine can de-activate
	// siblings when one fills.
	ParentTradeID sql.NullString `json:"parentTradeId"`
	// Strategy is the execution strategy that produced this fill
	// (one of broker.ExecutionStrategy: immediate / limit / twap /
	// vwap / iceberg / pov). NULL for legacy rows; new fills
	// always populate it via TradeRepo.Create.
	Strategy sql.NullString `json:"strategy"`
	// StrategyParentTradeID points at the parent of a sliced
	// execution (TWAP / VWAP / iceberg / POV). NULL means
	// "standalone parent" (or pre-088 legacy row). DISTINCT from
	// ParentTradeID — that one is the bracket / OCO parent, this
	// one is the execution-strategy slice parent. The two
	// relationships are orthogonal and both can be set on a
	// child slice that's also the entry leg of a bracket.
	StrategyParentTradeID sql.NullString `json:"strategyParentTradeId"`
	// ClientIdempotencyKey is the caller-minted idempotency key used
	// by the broker layer (broker.PlaceOrderRequest.ClientOrderID maps
	// to this column). The Create function performs ON CONFLICT DO
	// NOTHING + RETURNING so a duplicate submission collapses to the
	// existing row instead of double-booking.
	ClientIdempotencyKey sql.NullString `json:"clientIdempotencyKey"`
	CreatedAt            time.Time      `json:"createdAt"`

	// P0-5 cancel / replace tracking. CancelledAt is set the moment
	// status transitions to 'cancelled'; CancelReason is a short tag
	// (user_requested / superseded_by_replace / ttl / risk_breach /
	// system) so an aggregator can group reasons without parsing
	// free text. ReplacedAt is bumped on every successful Replace
	// call; ReplaceCount is the cumulative count and is bounded at
	// 32 in the runtime to prevent runaway modify loops.
	CancelledAt   sql.NullTime   `json:"cancelledAt"`
	CancelReason  sql.NullString `json:"cancelReason"`
	ReplacedAt    sql.NullTime   `json:"replacedAt"`
	ReplaceCount  int            `json:"replaceCount"`
}

type HoldingPosition struct {
	ID                 string          `json:"id"`
	FundID             string          `json:"fundId"`
	InstrumentKey      string          `json:"instrumentKey"`
	Symbol             string          `json:"symbol"`
	Name               sql.NullString  `json:"name"`
	Market             sql.NullString  `json:"market"`
	Exchange           sql.NullString  `json:"exchange"`
	AssetClass         sql.NullString  `json:"assetClass"`
	InstrumentType     sql.NullString  `json:"instrumentType"`
	PositionSide       sql.NullString  `json:"positionSide"`
	QuoteCurrency      sql.NullString  `json:"quoteCurrency"`
	SettlementCurrency sql.NullString  `json:"settlementCurrency"`
	MarginMode         sql.NullString  `json:"marginMode"`
	Quantity           float64         `json:"quantity"`
	AvailableQty       float64         `json:"availableQty"`
	CostPrice          float64         `json:"costPrice"`
	CurrentPrice       float64         `json:"currentPrice"`
	MarketValue        float64         `json:"marketValue"`
	Weight             float64         `json:"weight"`
	Leverage           sql.NullFloat64 `json:"leverage"`
	ContractMultiplier sql.NullFloat64 `json:"contractMultiplier"`
	ExpiryDate         sql.NullTime    `json:"expiryDate"`
	UnrealizedPnL      sql.NullFloat64 `json:"unrealizedPnL"`
	MarginUsed         sql.NullFloat64 `json:"marginUsed"`
	UpdatedAt          time.Time       `json:"updatedAt"`
}

type NavSnapshot struct {
	ID                string          `json:"id"`
	FundID            string          `json:"fundId"`
	TradingDate       time.Time       `json:"tradingDate"`
	NAV               float64         `json:"nav"`
	TotalAssets       float64         `json:"totalAssets"`
	TotalMarketValue  float64         `json:"totalMarketValue"`
	AvailableCash     float64         `json:"availableCash"`
	DailyReturn       float64         `json:"dailyReturn"`
	TotalReturn       float64         `json:"totalReturn"`
	PositionsSnapshot json.RawMessage `json:"positionsSnapshot"`
	CreatedAt         time.Time       `json:"createdAt"`
}

type WorkflowRun struct {
	ID          string          `json:"id"`
	FundID      string          `json:"fundId"`
	TradingDate time.Time       `json:"tradingDate"`
	Status      string          `json:"status"`
	CurrentStep sql.NullString  `json:"currentStep"`
	StepResults json.RawMessage `json:"stepResults"`
	StartedAt   sql.NullTime    `json:"startedAt"`
	CompletedAt sql.NullTime    `json:"completedAt"`
	CreatedAt   time.Time       `json:"createdAt"`
}

type Memory struct {
	ID              string         `json:"id"`
	FundID          string         `json:"fundId"`
	AgentID         sql.NullString `json:"agentId"`
	OwnerUserID     sql.NullString `json:"ownerUserId"`
	Visibility      string         `json:"visibility"`
	Sensitivity     string         `json:"sensitivity"`
	OriginKind      string         `json:"originKind"`
	SourceListingID sql.NullString `json:"sourceListingId"`
	Layer           string         `json:"layer"`
	Title           sql.NullString `json:"title"`
	Content         string         `json:"content"`
	TradingDate     sql.NullTime   `json:"tradingDate"`
	Tags            []string       `json:"tags"`
	CreatedAt       time.Time      `json:"createdAt"`
	UpdatedAt       time.Time      `json:"updatedAt"`

	// TemplateKey + Payload power the i18n render path (migration 085).
	// Both are NULL/empty for legacy or non-AI-generated rows — callers
	// fall back to Content in that case. When set, the UI looks up
	// messages[locale][TemplateKey] and interpolates Payload with
	// locale-aware number formatting. See docs/I18N_TEMPLATE_VERSIONING.md.
	TemplateKey sql.NullString `json:"templateKey"`
	// Payload is the raw jsonb bytes. We deliberately keep it as
	// json.RawMessage rather than decoding here so the API layer can
	// pass it through to the client without re-marshalling — fewer
	// allocations on the hot list path, and the client decides the
	// shape based on TemplateKey.
	Payload json.RawMessage `json:"payload,omitempty"`
}

type ABTest struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	ControlFundID   string          `json:"controlFundId"`
	TreatmentFundID string          `json:"treatmentFundId"`
	VariableType    string          `json:"variableType"`
	VariableConfig  json.RawMessage `json:"variableConfig"`
	Status          string          `json:"status"`
	StartDate       sql.NullTime    `json:"startDate"`
	EndDate         sql.NullTime    `json:"endDate"`
	Results         json.RawMessage `json:"results"`
	CreatedAt       time.Time       `json:"createdAt"`
	UpdatedAt       time.Time       `json:"updatedAt"`
}

type TeamMember struct {
	ID        string         `json:"id"`
	FundID    string         `json:"fundId"`
	AgentID   string         `json:"agentId"`
	Role      string         `json:"role"`
	Focus     sql.NullString `json:"focus"`
	JoinedAt  time.Time      `json:"joinedAt"`
	Status    string         `json:"status"`
	UpdatedAt time.Time      `json:"updatedAt"`
}

type Agent struct {
	ID                            string          `json:"id"`
	UserID                        string          `json:"userId"`
	Name                          string          `json:"name"`
	Role                          string          `json:"role"`
	Focus                         sql.NullString  `json:"focus"`
	LLMModel                      sql.NullString  `json:"llmModel"`
	ModelProvider                 sql.NullString  `json:"modelProvider"`
	ModelName                     sql.NullString  `json:"modelName"`
	SystemPrompt                  sql.NullString  `json:"systemPrompt"`
	SkillConfig                   json.RawMessage `json:"skillConfig"`
	DomainConfig                  json.RawMessage `json:"domainConfig"`
	EvolutionConfig               json.RawMessage `json:"evolutionConfig"`
	PendingMarketplaceSnapshot    json.RawMessage `json:"pendingMarketplaceSnapshot"`
	MarketplaceSnapshotImportedAt sql.NullTime    `json:"marketplaceSnapshotImportedAt"`
	Status                        string          `json:"status"`
	CreatedAt                     time.Time       `json:"createdAt"`
	UpdatedAt                     time.Time       `json:"updatedAt"`
}

// ---------------------------------------------------------------------------
// FundCompanyRepo
// ---------------------------------------------------------------------------

type FundCompanyRepo struct {
	db *sql.DB
}

func NewFundCompanyRepo(db *sql.DB) *FundCompanyRepo {
	return &FundCompanyRepo{db: db}
}

func (r *FundCompanyRepo) Create(ctx context.Context, c *FundCompany) (string, error) {
	var id string
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO fund_companies (owner_user_id, name, description)
		 VALUES ($1, $2, $3)
		 RETURNING id`,
		c.OwnerUserID, c.Name, c.Description,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("fund_company_repo: create: %w", err)
	}
	return id, nil
}

func (r *FundCompanyRepo) GetByID(ctx context.Context, id string) (*FundCompany, error) {
	c := &FundCompany{}
	err := r.db.QueryRowContext(ctx,
		`SELECT id, owner_user_id, name, description, created_at, updated_at
		 FROM fund_companies WHERE id = $1`, id,
	).Scan(&c.ID, &c.OwnerUserID, &c.Name, &c.Description, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("fund_company_repo: get by id: %w", err)
	}
	return c, nil
}

func (r *FundCompanyRepo) ListByOwner(ctx context.Context, ownerUserID string) ([]FundCompany, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, owner_user_id, name, description, created_at, updated_at
		 FROM fund_companies WHERE owner_user_id = $1 ORDER BY created_at DESC`, ownerUserID,
	)
	if err != nil {
		return nil, fmt.Errorf("fund_company_repo: list by owner: %w", err)
	}
	defer rows.Close()

	var companies []FundCompany
	for rows.Next() {
		var c FundCompany
		if err := rows.Scan(&c.ID, &c.OwnerUserID, &c.Name, &c.Description, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("fund_company_repo: scan row: %w", err)
		}
		companies = append(companies, c)
	}
	return companies, rows.Err()
}

func (r *FundCompanyRepo) Update(ctx context.Context, c *FundCompany) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE fund_companies
		 SET name = $1, description = $2, updated_at = NOW()
		 WHERE id = $3`,
		c.Name, c.Description, c.ID,
	)
	if err != nil {
		return fmt.Errorf("fund_company_repo: update: %w", err)
	}
	return checkRowsAffected(res, "fund_company_repo: update")
}

func (r *FundCompanyRepo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM fund_companies WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("fund_company_repo: delete: %w", err)
	}
	return checkRowsAffected(res, "fund_company_repo: delete")
}

// ---------------------------------------------------------------------------
// FundRepo
// ---------------------------------------------------------------------------

type FundRepo struct {
	db *sql.DB
}

func NewFundRepo(db *sql.DB) *FundRepo {
	return &FundRepo{db: db}
}

func (r *FundRepo) Create(ctx context.Context, f *Fund) (string, error) {
	cfgJSON, err := marshalJSON(f.Config)
	if err != nil {
		return "", fmt.Errorf("fund_repo: marshal config: %w", err)
	}

	var id string
	err = r.db.QueryRowContext(ctx,
		`INSERT INTO funds (company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 RETURNING id`,
		f.CompanyID, f.Name, f.Description, f.TradingMode, f.InitialCapital, f.CurrentCapital, f.TotalAssets, f.NAV, f.Status, cfgJSON,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("fund_repo: create: %w", err)
	}
	return id, nil
}

func (r *FundRepo) GetByID(ctx context.Context, id string) (*Fund, error) {
	f := &Fund{}
	err := r.db.QueryRowContext(ctx,
		`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`, id,
	).Scan(&f.ID, &f.CompanyID, &f.Name, &f.Description, &f.TradingMode, &f.InitialCapital, &f.CurrentCapital, &f.TotalAssets, &f.NAV, &f.Status, &f.Config, &f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("fund_repo: get by id: %w", err)
	}
	// Default base currency to USD until the dedicated lookup
	// runs. Callers that care about cross-currency math (NAV
	// aggregator, cash_ledger summary) call GetBaseCurrency
	// explicitly so the existing GetByID call surface stays
	// backwards compatible — the fund-detail JSON shape doesn't
	// flip on a single migration.
	if f.BaseCurrency == "" {
		f.BaseCurrency = "USD"
	}
	return f, nil
}

// GetBaseCurrency returns the per-fund reporting currency
// (P1-4). Read separately from GetByID to keep the legacy SELECT
// shape stable for the dozens of test fixtures that already mock
// it. Falls back to "USD" if the column is NULL or empty.
//
// Callers: navcalc, cash_ledger summary, the funds-settings
// fetcher, the FX-aware NAV history rebuild loop.
func (r *FundRepo) GetBaseCurrency(ctx context.Context, fundID string) (string, error) {
	if r == nil || r.db == nil {
		return "", fmt.Errorf("fund_repo: nil db")
	}
	var cur sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT base_currency FROM funds WHERE id = $1`, fundID,
	).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("fund_repo: get base currency: %w", err)
	}
	if !cur.Valid || strings.TrimSpace(cur.String) == "" {
		return "USD", nil
	}
	return strings.ToUpper(strings.TrimSpace(cur.String)), nil
}

// SetBaseCurrency persists a new reporting currency for the fund
// (P1-4). Callers should pre-validate against fx.IsSupported.
// We deliberately don't merge this into Update() because changes
// to base_currency need their own audit log / 4-eye gate, not
// the generic fund-update path.
func (r *FundRepo) SetBaseCurrency(ctx context.Context, fundID, currency string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("fund_repo: nil db")
	}
	cur := strings.ToUpper(strings.TrimSpace(currency))
	if cur == "" {
		return fmt.Errorf("fund_repo: empty currency")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE funds
		    SET base_currency = $2,
		        updated_at = NOW()
		  WHERE id = $1`,
		fundID, cur,
	)
	if err != nil {
		return fmt.Errorf("fund_repo: set base currency: %w", err)
	}
	return checkRowsAffected(res, "fund_repo: set base currency")
}

// GetByIDForUpdateTx locks the fund row inside the caller's transaction
// using `SELECT ... FOR UPDATE`, serialising concurrent writers on the
// same fund. This is the read leg of an UpdateFund flow that wants to
// avoid lost-update races (a concurrent writer reading the stale
// pre-merge snapshot and then writing back over our pending changes).
//
// The caller MUST hold a tx and MUST issue the UPDATE on the same tx
// before committing — the row lock is released at commit/rollback time.
func (r *FundRepo) GetByIDForUpdateTx(ctx context.Context, tx *sql.Tx, id string) (*Fund, error) {
	if tx == nil {
		return nil, fmt.Errorf("fund_repo: GetByIDForUpdateTx requires a non-nil transaction")
	}
	f := &Fund{}
	err := tx.QueryRowContext(ctx,
		`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1
		 FOR UPDATE`, id,
	).Scan(&f.ID, &f.CompanyID, &f.Name, &f.Description, &f.TradingMode, &f.InitialCapital, &f.CurrentCapital, &f.TotalAssets, &f.NAV, &f.Status, &f.Config, &f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("fund_repo: get by id for update: %w", err)
	}
	return f, nil
}

func (r *FundRepo) ListByCompany(ctx context.Context, companyID string) ([]Fund, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE company_id = $1 ORDER BY created_at DESC`, companyID,
	)
	if err != nil {
		return nil, fmt.Errorf("fund_repo: list by company: %w", err)
	}
	defer rows.Close()

	return scanFunds(rows)
}

func (r *FundRepo) ListByCompanyIDs(ctx context.Context, companyIDs []string) ([]Fund, error) {
	if len(companyIDs) == 0 {
		return []Fund{}, nil
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE company_id = ANY($1)
		 ORDER BY company_id, created_at DESC`, pq.Array(companyIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("fund_repo: list by company ids: %w", err)
	}
	defer rows.Close()

	return scanFunds(rows)
}

func (r *FundRepo) ListActive(ctx context.Context) ([]Fund, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE status = 'active'
		 ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("fund_repo: list active: %w", err)
	}
	defer rows.Close()

	return scanFunds(rows)
}

func (r *FundRepo) Update(ctx context.Context, f *Fund) error {
	cfgJSON, err := marshalJSON(f.Config)
	if err != nil {
		return fmt.Errorf("fund_repo: marshal config: %w", err)
	}

	res, err := r.db.ExecContext(ctx,
		`UPDATE funds
		 SET name = $1, description = $2, trading_mode = $3, status = $4, config = $5, updated_at = NOW()
		 WHERE id = $6`,
		f.Name, f.Description, f.TradingMode, f.Status, cfgJSON, f.ID,
	)
	if err != nil {
		return fmt.Errorf("fund_repo: update: %w", err)
	}
	return checkRowsAffected(res, "fund_repo: update")
}

func (r *FundRepo) UpdateCapital(ctx context.Context, fundID string, currentCapital, totalAssets, nav float64) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE funds
		 SET current_capital = $1, total_assets = $2, nav = $3, updated_at = NOW()
		 WHERE id = $4`,
		currentCapital, totalAssets, nav, fundID,
	)
	if err != nil {
		return fmt.Errorf("fund_repo: update capital: %w", err)
	}
	return checkRowsAffected(res, "fund_repo: update capital")
}

func (r *FundRepo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM funds WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("fund_repo: delete: %w", err)
	}
	return checkRowsAffected(res, "fund_repo: delete")
}

// ---------------------------------------------------------------------------
// PlanRepo
// ---------------------------------------------------------------------------

type PlanRepo struct {
	db *sql.DB
}

type queryRowContextExecutor interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func NewPlanRepo(db *sql.DB) *PlanRepo {
	return &PlanRepo{db: db}
}

func (r *PlanRepo) DB() *sql.DB {
	return r.db
}

func (r *PlanRepo) Create(ctx context.Context, p *InvestmentPlan) (string, error) {
	return r.createTx(ctx, r.db, p)
}

func (r *PlanRepo) CreateWithActions(ctx context.Context, p *InvestmentPlan, actions []PlanAction) (string, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("plan_repo: begin tx for plan: %w", err)
	}
	defer tx.Rollback()
	id, err := r.createTx(ctx, tx, p)
	if err != nil {
		return "", err
	}
	if err := r.createActionsTx(ctx, tx, id, actions); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("plan_repo: commit plan with actions: %w", err)
	}
	return id, nil
}

func (r *PlanRepo) createTx(ctx context.Context, queryer queryRowContextExecutor, p *InvestmentPlan) (string, error) {
	riskReviewJSON, err := marshalJSON(p.RiskReview)
	if err != nil {
		return "", fmt.Errorf("plan_repo: marshal risk review: %w", err)
	}

	discussionSnapshotJSON, err := marshalJSONObject(p.DiscussionSnapshot)
	if err != nil {
		return "", fmt.Errorf("plan_repo: marshal discussion snapshot: %w", err)
	}

	// F16: when ClientIdempotencyKey is supplied, INSERT-OR-RETURN the
	// existing row via a CTE so a retried PM-plan step (e.g. F12
	// recovery or admin manual trigger) returns the original plan ID
	// instead of creating a duplicate. The CTE pattern is preferred over
	// ON CONFLICT DO NOTHING + a separate SELECT because it stays atomic
	// without a follow-up read.
	if p.ClientIdempotencyKey.Valid && strings.TrimSpace(p.ClientIdempotencyKey.String) != "" {
		var id string
		err = queryer.QueryRowContext(ctx,
			`WITH ins AS (
			    INSERT INTO investment_plans (fund_id, trading_date, status, reasoning, risk_score, expected_return, roundtable_id, pm_agent_id, risk_review, discussion_snapshot, confidence, client_idempotency_key)
			    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			    ON CONFLICT (client_idempotency_key) WHERE client_idempotency_key IS NOT NULL DO NOTHING
			    RETURNING id
			 )
			 SELECT id FROM ins
			 UNION ALL
			 SELECT id FROM investment_plans WHERE client_idempotency_key = $12
			 LIMIT 1`,
			p.FundID, p.TradingDate, p.Status, p.Reasoning, p.RiskScore, p.ExpectedReturn, p.RoundtableID, p.PMAgentID, riskReviewJSON, discussionSnapshotJSON, p.Confidence, p.ClientIdempotencyKey.String,
		).Scan(&id)
		if err != nil {
			return "", fmt.Errorf("plan_repo: create (idempotent): %w", err)
		}
		return id, nil
	}

	var id string
	err = queryer.QueryRowContext(ctx,
		`INSERT INTO investment_plans (fund_id, trading_date, status, reasoning, risk_score, expected_return, roundtable_id, pm_agent_id, risk_review, discussion_snapshot, confidence)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 RETURNING id`,
		p.FundID, p.TradingDate, p.Status, p.Reasoning, p.RiskScore, p.ExpectedReturn, p.RoundtableID, p.PMAgentID, riskReviewJSON, discussionSnapshotJSON, p.Confidence,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("plan_repo: create: %w", err)
	}
	return id, nil
}

// SetBlockContributions stamps the G1 #2 attribution JSON onto an
// existing plan. Called by the wiring layer after Create returns
// successfully, in a separate UPDATE so the INSERT path (and its
// extensive test coverage) stays unchanged. payload is the
// already-marshalled JSONB blob — the caller owns the schema.
//
// Soft-fail contract: a nil / empty payload is a no-op. A DB
// error is logged-and-ignored upstream; attribution is a soft
// observability signal, not a correctness one. We never want
// the decision-write path to fail because the attribution
// writer couldn't persist (e.g. transient connection drop).
func (r *PlanRepo) SetBlockContributions(ctx context.Context, planID string, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	if strings.TrimSpace(planID) == "" {
		return fmt.Errorf("plan_repo: SetBlockContributions: empty plan id")
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE investment_plans
		    SET block_contributions = $2,
		        updated_at = NOW()
		  WHERE id = $1`,
		planID, payload,
	)
	if err != nil {
		return fmt.Errorf("plan_repo: SetBlockContributions: %w", err)
	}
	return nil
}

// SetDecisionSource stamps the Sprint 11.1 LLM-vs-fallback provenance
// onto an existing plan, mirroring the SetBlockContributions soft-fail
// contract. source MUST be one of the errorclass / decision_source
// values (llm_pm, llm_three_stage, fallback_no_llm,
// fallback_after_llm_error, fallback_empty_plan); reason is the
// pre-marshalled errorclass.Detail JSONB for fallback rows and nil
// for successful LLM rows.
//
// The DB column has a NOT NULL DEFAULT 'legacy', so empty source is
// rejected as a programmer error (we never want to silently downgrade
// a successful LLM run to "legacy" because the wiring forgot to set
// the field). reason may be nil; the column is nullable.
func (r *PlanRepo) SetDecisionSource(ctx context.Context, planID, source string, reason []byte) error {
	if strings.TrimSpace(planID) == "" {
		return fmt.Errorf("plan_repo: SetDecisionSource: empty plan id")
	}
	if strings.TrimSpace(source) == "" {
		return fmt.Errorf("plan_repo: SetDecisionSource: empty source")
	}
	var reasonArg any
	if len(reason) > 0 {
		reasonArg = reason
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE investment_plans
		    SET decision_source = $2,
		        fallback_reason = $3,
		        updated_at = NOW()
		  WHERE id = $1`,
		planID, source, reasonArg,
	)
	if err != nil {
		return fmt.Errorf("plan_repo: SetDecisionSource: %w", err)
	}
	return nil
}

// GetDecisionSource is the focused read counterpart to
// SetDecisionSource. Returns the source tag and the raw JSONB
// fallback_reason blob (nil when the column is NULL). Kept narrow on
// purpose so the existing PlanRepo.GetByID SELECT — and its dense
// sqlmock test surface — stays unchanged.
func (r *PlanRepo) GetDecisionSource(ctx context.Context, planID string) (string, []byte, error) {
	if strings.TrimSpace(planID) == "" {
		return "", nil, fmt.Errorf("plan_repo: GetDecisionSource: empty plan id")
	}
	var (
		source string
		reason sql.NullString
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT COALESCE(decision_source, 'legacy'),
		        fallback_reason::text
		   FROM investment_plans
		  WHERE id = $1`,
		planID,
	).Scan(&source, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	if err != nil {
		return "", nil, fmt.Errorf("plan_repo: GetDecisionSource: %w", err)
	}
	if reason.Valid && reason.String != "" {
		return source, []byte(reason.String), nil
	}
	return source, nil, nil
}

func (r *PlanRepo) GetByID(ctx context.Context, id string) (*InvestmentPlan, error) {
	p := &InvestmentPlan{}
	err := r.db.QueryRowContext(ctx,
		`SELECT id, fund_id, trading_date, status, reasoning, risk_score, expected_return, risk_review, discussion_snapshot, roundtable_id, pm_agent_id, confidence, created_at, updated_at
		 FROM investment_plans WHERE id = $1`, id,
	).Scan(&p.ID, &p.FundID, &p.TradingDate, &p.Status, &p.Reasoning, &p.RiskScore, &p.ExpectedReturn, &p.RiskReview, &p.DiscussionSnapshot, &p.RoundtableID, &p.PMAgentID, &p.Confidence, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("plan_repo: get by id: %w", err)
	}
	return p, nil
}

func (r *PlanRepo) GetLatestByFundAndDate(ctx context.Context, fundID string, tradingDate time.Time) (*InvestmentPlan, error) {
	p := &InvestmentPlan{}
	err := r.db.QueryRowContext(ctx,
		`SELECT id, fund_id, trading_date, status, reasoning, risk_score, expected_return, risk_review, discussion_snapshot, roundtable_id, pm_agent_id, confidence, created_at, updated_at
		 FROM investment_plans
		 WHERE fund_id = $1 AND trading_date = $2
		 ORDER BY created_at DESC
		 LIMIT 1`,
		fundID, tradingDate,
	).Scan(&p.ID, &p.FundID, &p.TradingDate, &p.Status, &p.Reasoning, &p.RiskScore, &p.ExpectedReturn, &p.RiskReview, &p.DiscussionSnapshot, &p.RoundtableID, &p.PMAgentID, &p.Confidence, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("plan_repo: get latest by fund and date: %w", err)
	}
	return p, nil
}

func (r *PlanRepo) ListByFund(ctx context.Context, fundID string, limit int) ([]InvestmentPlan, error) {
	return r.ListByFundPage(ctx, fundID, limit, 0)
}

func (r *PlanRepo) ListByFundPage(ctx context.Context, fundID string, limit, offset int) ([]InvestmentPlan, error) {
	return r.ListByFundPageFiltered(ctx, fundID, PlanListFilter{Limit: limit, Offset: offset})
}

// PlanListFilter narrows a paged list of investment plans. Zero value
// for any field means "no filter on this dimension". Status is matched
// exactly. From/To filter by trading_date inclusively (so callers can
// ask for "everything on 2026-05-22" with From=To=that day).
type PlanListFilter struct {
	Limit  int
	Offset int
	Status string
	From   *time.Time
	To     *time.Time
}

// ListByFundPageFiltered is the filter-aware paged list. We build the
// WHERE clause dynamically so unused filters don't pin a query plan to
// a useless trading_date index lookup; the (fund_id, trading_date,
// created_at) composite index covers the always-present fund_id leg.
func (r *PlanRepo) ListByFundPageFiltered(ctx context.Context, fundID string, filter PlanListFilter) ([]InvestmentPlan, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	query := `SELECT id, fund_id, trading_date, status, reasoning, risk_score, expected_return, risk_review, discussion_snapshot, roundtable_id, pm_agent_id, confidence, created_at, updated_at
		 FROM investment_plans WHERE fund_id = $1`
	args := []interface{}{fundID}
	if status := strings.TrimSpace(filter.Status); status != "" {
		args = append(args, status)
		query += fmt.Sprintf(" AND status = $%d", len(args))
	}
	if filter.From != nil {
		args = append(args, filter.From.UTC())
		query += fmt.Sprintf(" AND trading_date >= $%d", len(args))
	}
	if filter.To != nil {
		args = append(args, filter.To.UTC())
		query += fmt.Sprintf(" AND trading_date <= $%d", len(args))
	}
	args = append(args, limit, offset)
	query += fmt.Sprintf(" ORDER BY trading_date DESC, created_at DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("plan_repo: list by fund: %w", err)
	}
	defer rows.Close()

	var plans []InvestmentPlan
	for rows.Next() {
		var p InvestmentPlan
		if err := rows.Scan(&p.ID, &p.FundID, &p.TradingDate, &p.Status, &p.Reasoning, &p.RiskScore, &p.ExpectedReturn, &p.RiskReview, &p.DiscussionSnapshot, &p.RoundtableID, &p.PMAgentID, &p.Confidence, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("plan_repo: scan row: %w", err)
		}
		plans = append(plans, p)
	}
	return plans, rows.Err()
}

func (r *PlanRepo) UpdateStatus(ctx context.Context, planID, status string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE investment_plans SET status = $1, updated_at = NOW() WHERE id = $2`,
		status, planID,
	)
	if err != nil {
		return fmt.Errorf("plan_repo: update status: %w", err)
	}
	return checkRowsAffected(res, "plan_repo: update status")
}

func (r *PlanRepo) UpdateActionQuote(ctx context.Context, actionID string, price, quantity, amount float64, reasoning string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE plan_actions
		 SET price = $1, quantity = $2, amount = $3, reasoning = $4, quote_refreshed_at = NOW()
		 WHERE id = $5`,
		price, quantity, amount, reasoning, actionID,
	)
	if err != nil {
		return fmt.Errorf("plan_repo: update action quote: %w", err)
	}
	return checkRowsAffected(res, "plan_repo: update action quote")
}

func (r *PlanRepo) CreateActions(ctx context.Context, planID string, actions []PlanAction) error {
	if len(actions) == 0 {
		return nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("plan_repo: begin tx for actions: %w", err)
	}
	defer tx.Rollback()
	if err := r.createActionsTx(ctx, tx, planID, actions); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("plan_repo: commit actions: %w", err)
	}
	return nil
}

func (r *PlanRepo) createActionsTx(ctx context.Context, tx *sql.Tx, planID string, actions []PlanAction) error {
	if len(actions) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO plan_actions (plan_id, instrument_key, symbol, market, exchange, asset_class, instrument_type, action, position_side, open_close, quantity, price, amount, stop_loss, take_profit, reasoning, confidence, supported_by, opposed_by, execution_status, sort_order, quote_currency, settlement_currency, margin_mode, leverage, contract_multiplier, expiry_date, reduce_only, sleeve, regime_tag, signal_source, exit_reason, strategy)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33)`,
	)
	if err != nil {
		return fmt.Errorf("plan_repo: prepare action insert: %w", err)
	}
	defer stmt.Close()

	for i := range actions {
		if _, err := stmt.ExecContext(ctx,
			planID,
			actions[i].InstrumentKey,
			actions[i].Symbol,
			actions[i].Market,
			actions[i].Exchange,
			actions[i].AssetClass,
			actions[i].InstrumentType,
			actions[i].Action,
			actions[i].PositionSide,
			actions[i].OpenClose,
			actions[i].Quantity,
			actions[i].Price,
			actions[i].Amount,
			actions[i].StopLoss,
			actions[i].TakeProfit,
			actions[i].Reasoning,
			actions[i].Confidence,
			pq.Array(actions[i].SupportedBy),
			pq.Array(actions[i].OpposedBy),
			actions[i].ExecutionStatus,
			actions[i].SortOrder,
			actions[i].QuoteCurrency,
			actions[i].SettlementCurrency,
			actions[i].MarginMode,
			actions[i].Leverage,
			actions[i].ContractMultiplier,
			actions[i].ExpiryDate,
			actions[i].ReduceOnly,
			actions[i].Sleeve,
			actions[i].RegimeTag,
			actions[i].SignalSource,
			actions[i].ExitReason,
			actions[i].Strategy,
		); err != nil {
			return fmt.Errorf("plan_repo: insert action [%d]: %w", i, err)
		}
	}
	return nil
}

func (r *PlanRepo) GetActions(ctx context.Context, planID string) ([]PlanAction, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, plan_id, instrument_key, symbol, market, exchange, asset_class, instrument_type, action, position_side, open_close, quantity, price, amount, stop_loss, take_profit, reasoning, confidence, supported_by, opposed_by, execution_status, sort_order, quote_currency, settlement_currency, margin_mode, leverage, contract_multiplier, expiry_date, reduce_only, quote_refreshed_at, auto_executed_at, sleeve, regime_tag, signal_source, exit_reason, strategy
		 FROM plan_actions WHERE plan_id = $1 ORDER BY sort_order, id`, planID,
	)
	if err != nil {
		return nil, fmt.Errorf("plan_repo: get actions: %w", err)
	}
	defer rows.Close()

	var actions []PlanAction
	for rows.Next() {
		var a PlanAction
		if err := rows.Scan(&a.ID, &a.PlanID, &a.InstrumentKey, &a.Symbol, &a.Market, &a.Exchange, &a.AssetClass, &a.InstrumentType, &a.Action, &a.PositionSide, &a.OpenClose, &a.Quantity, &a.Price, &a.Amount, &a.StopLoss, &a.TakeProfit, &a.Reasoning, &a.Confidence, pq.Array(&a.SupportedBy), pq.Array(&a.OpposedBy), &a.ExecutionStatus, &a.SortOrder, &a.QuoteCurrency, &a.SettlementCurrency, &a.MarginMode, &a.Leverage, &a.ContractMultiplier, &a.ExpiryDate, &a.ReduceOnly, &a.QuoteRefreshedAt, &a.AutoExecutedAt, &a.Sleeve, &a.RegimeTag, &a.SignalSource, &a.ExitReason, &a.Strategy); err != nil {
			return nil, fmt.Errorf("plan_repo: scan action: %w", err)
		}
		actions = append(actions, a)
	}
	return actions, rows.Err()
}

// UpdateConfidence sets the plan-level confidence column. Called by
// the PMAgent after the DecisionEngine returns. Callers may pass a
// sql.NullFloat64{Valid:false} to clear the column (e.g. when the
// fallback engine ran), in which case the auto-execute gate treats
// the plan as unconfident regardless of any value lurking in
// risk_review JSON.
func (r *PlanRepo) UpdateConfidence(ctx context.Context, planID string, confidence sql.NullFloat64) error {
	if planID == "" {
		return fmt.Errorf("plan_repo: update confidence: empty plan id")
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE investment_plans SET confidence = $1, updated_at = NOW() WHERE id = $2`,
		confidence, planID,
	)
	if err != nil {
		return fmt.Errorf("plan_repo: update confidence: %w", err)
	}
	return nil
}

// UpdateRiskReview overwrites the investment_plans.risk_review JSON
// column for a single plan. Used by the auto-execute gate to append
// its audit payload (the existing RiskAgent output is merged into the
// new doc by the caller before this UPDATE runs). We deliberately
// don't gate on the previous risk_review value because the caller is
// responsible for the read-modify-write cycle.
func (r *PlanRepo) UpdateRiskReview(ctx context.Context, planID string, review json.RawMessage) error {
	if planID == "" {
		return fmt.Errorf("plan_repo: update risk review: empty plan id")
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE investment_plans SET risk_review = $1, updated_at = NOW() WHERE id = $2`,
		review, planID,
	)
	if err != nil {
		return fmt.Errorf("plan_repo: update risk review: %w", err)
	}
	return nil
}

// StampAutoExecuted sets plan_actions.auto_executed_at = now() for
// every action under the given plan. Idempotent (a second call with
// the same plan just rewrites the timestamps; we deliberately don't
// gate on IS NULL because re-running the gate after a reject->approve
// cycle is rare enough to ignore). The caller passes "now" so tests
// can freeze time.
func (r *PlanRepo) StampAutoExecuted(ctx context.Context, planID string, now time.Time) error {
	if planID == "" {
		return fmt.Errorf("plan_repo: stamp auto executed: empty plan id")
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE plan_actions SET auto_executed_at = $1 WHERE plan_id = $2`,
		now.UTC(), planID,
	)
	if err != nil {
		return fmt.Errorf("plan_repo: stamp auto executed: %w", err)
	}
	return nil
}

// SumAutoExecutedAmountForFundDay returns the absolute sum of action
// amounts the gateway has already auto-approved for a given fund
// between [from, to). Used by autoExecuteGateCheck's daily-cumulative
// guardrail. We sum ABS(amount) because both buys (positive amount
// outflow) and sells (positive amount inflow) consume the daily
// auto-approval budget — the user's intent for the cap is "how much
// money are you allowed to move without me looking".
func (r *PlanRepo) SumAutoExecutedAmountForFundDay(ctx context.Context, fundID string, from, to time.Time) (float64, error) {
	if fundID == "" {
		return 0, fmt.Errorf("plan_repo: sum auto executed: empty fund id")
	}
	row := r.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(ABS(pa.amount)), 0)
		   FROM plan_actions pa
		   JOIN investment_plans ip ON ip.id = pa.plan_id
		  WHERE ip.fund_id = $1
		    AND pa.auto_executed_at IS NOT NULL
		    AND pa.auto_executed_at >= $2
		    AND pa.auto_executed_at < $3`,
		fundID, from.UTC(), to.UTC(),
	)
	var sum sql.NullFloat64
	if err := row.Scan(&sum); err != nil {
		return 0, fmt.Errorf("plan_repo: sum auto executed: %w", err)
	}
	if !sum.Valid {
		return 0, nil
	}
	return sum.Float64, nil
}

// ---------------------------------------------------------------------------
// TradeRepo
// ---------------------------------------------------------------------------

type TradeRepo struct {
	db *sql.DB
}

func NewTradeRepo(db *sql.DB) *TradeRepo {
	return &TradeRepo{db: db}
}

// Create inserts a new trade_execution row. When ClientIdempotencyKey is
// non-NULL, the insert is idempotent on the partial UNIQUE index added
// by migration 027 — a duplicate submission with the same key collapses
// to the existing row and the original id is returned. Callers that
// want strict "I created this row" semantics should compare the
// returned id with one they minted (RETURNING xmax = 0 would also
// work but the simpler SELECT-fallback pattern matches what
// PlanRepo.Create does for investment_plans).
func (r *TradeRepo) Create(ctx context.Context, t *TradeExecution) (string, error) {
	var id string
	// ON CONFLICT path is only taken when ClientIdempotencyKey is set
	// (the unique index is partial WHERE client_idempotency_key IS
	// NOT NULL). Rows without an idempotency key always insert
	// fresh — this preserves the legacy contract where the runtime
	// engine called Create without a key for every retry.
	const sqlInsert = `
		WITH ins AS (
			INSERT INTO trade_executions (
				fund_id, plan_id, plan_action_id, instrument_key, symbol,
				market, exchange, asset_class, instrument_type, side,
				position_side, open_close, order_type, quantity, price, amount,
				trading_mode, broker_order_id, mcp_server_id, status, executed_at,
				quote_currency, settlement_currency, margin_mode, leverage,
				contract_multiplier, expiry_date, reduce_only,
				stop_price, trail_amount, trail_percent, display_qty,
				time_in_force, good_till_date, parent_trade_id,
				strategy, strategy_parent_trade_id,
				client_idempotency_key
			)
			VALUES (
				$1, $2, $3, $4, $5,
				$6, $7, $8, $9, $10,
				$11, $12, $13, $14, $15, $16,
				$17, $18, $19, $20, $21,
				$22, $23, $24, $25,
				$26, $27, $28,
				$29, $30, $31, $32,
				$33, $34, $35,
				$36, $37,
				$38
			)
			ON CONFLICT (client_idempotency_key)
				WHERE client_idempotency_key IS NOT NULL
				DO NOTHING
			RETURNING id
		)
		SELECT id FROM ins
		UNION ALL
		SELECT id FROM trade_executions
			WHERE client_idempotency_key = $38
				AND $38 IS NOT NULL
		LIMIT 1`
	err := r.db.QueryRowContext(ctx, sqlInsert,
		t.FundID, t.PlanID, t.PlanActionID, t.InstrumentKey, t.Symbol,
		t.Market, t.Exchange, t.AssetClass, t.InstrumentType, t.Side,
		t.PositionSide, t.OpenClose, t.OrderType, t.Quantity, t.Price, t.Amount,
		t.TradingMode, t.BrokerOrderID, t.MCPServerID, t.Status, t.ExecutedAt,
		t.QuoteCurrency, t.SettlementCurrency, t.MarginMode, t.Leverage,
		t.ContractMultiplier, t.ExpiryDate, t.ReduceOnly,
		t.StopPrice, t.TrailAmount, t.TrailPercent, t.DisplayQty,
		t.TimeInForce, t.GoodTillDate, t.ParentTradeID,
		t.Strategy, t.StrategyParentTradeID,
		t.ClientIdempotencyKey,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("trade_repo: create: %w", err)
	}
	return id, nil
}

// tradeExecutionColumns lists the SELECT-side projection used by every
// list / get function in this repo. Centralising it in one constant
// keeps INSERT and SELECT in sync after schema changes (e.g. P0-2's
// stop / trail / TIF / parent additions in migration 051, P0-5's
// cancel / replace tracking in 053).
const tradeExecutionColumns = `
	id, fund_id, plan_id, plan_action_id, instrument_key, symbol,
	market, exchange, asset_class, instrument_type, side, position_side,
	open_close, order_type, quantity, price, amount, filled_qty,
	filled_price, fee_commission, fee_stamp_tax, fee_transfer,
	trading_mode, broker_order_id, mcp_server_id, status, executed_at,
	quote_currency, settlement_currency, margin_mode, leverage,
	contract_multiplier, expiry_date, reduce_only, slippage_pct,
	stop_price, trail_amount, trail_percent, display_qty,
	time_in_force, good_till_date, parent_trade_id,
	strategy, strategy_parent_trade_id,
	client_idempotency_key, created_at,
	cancelled_at, cancel_reason, replaced_at, replace_count`

func (r *TradeRepo) ListByFund(ctx context.Context, fundID string, from, to time.Time, limit int) ([]TradeExecution, error) {
	return r.ListByFundPage(ctx, fundID, from, to, limit, 0)
}

func (r *TradeRepo) ListByFundPage(ctx context.Context, fundID string, from, to time.Time, limit, offset int) ([]TradeExecution, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+tradeExecutionColumns+`
		 FROM trade_executions
		 WHERE fund_id = $1 AND created_at >= $2 AND created_at <= $3
		 ORDER BY created_at DESC LIMIT $4 OFFSET $5`,
		fundID, from, to, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("trade_repo: list by fund: %w", err)
	}
	defer rows.Close()
	return scanTradeExecutions(rows)
}

func (r *TradeRepo) ListByPlan(ctx context.Context, planID string) ([]TradeExecution, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+tradeExecutionColumns+`
		 FROM trade_executions
		 WHERE plan_id = $1
		 ORDER BY created_at DESC, id DESC`,
		planID,
	)
	if err != nil {
		return nil, fmt.Errorf("trade_repo: list by plan: %w", err)
	}
	defer rows.Close()
	return scanTradeExecutions(rows)
}

// ListOpenByFund returns every non-terminal trade row for the fund.
// Used by the order-replay loop on restart (P1-5) and by the
// Cancel/Replace API (P0-5) to enumerate cancellable orders. Rows
// are returned newest-first.
func (r *TradeRepo) ListOpenByFund(ctx context.Context, fundID string) ([]TradeExecution, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+tradeExecutionColumns+`
		 FROM trade_executions
		 WHERE fund_id = $1
		   AND status IN ('pending', 'working', 'triggered', 'partial')
		 ORDER BY created_at DESC, id DESC`,
		fundID,
	)
	if err != nil {
		return nil, fmt.Errorf("trade_repo: list open by fund: %w", err)
	}
	defer rows.Close()
	return scanTradeExecutions(rows)
}

// ListOpenAcrossFunds returns every non-terminal trade row for every
// fund. The order-replay loop (P1-5) calls this exactly once at boot
// to seed the broker.Simulator's in-memory book before any new
// PlaceOrder requests are accepted. Rows are returned ordered by
// fund_id then created_at (oldest first within a fund) so the
// idempotency index is rebuilt deterministically.
//
// The query is paginated only via LIMIT to keep the boot path
// bounded; in practice we expect this to be small (a few hundred at
// most) since the runtime cancels stale GTC stops via the daily
// reconcile loop. If you find this query slow, the right fix is to
// add a partial index on (status) WHERE status IN
// ('pending','working','triggered','partial').
func (r *TradeRepo) ListOpenAcrossFunds(ctx context.Context, limit int) ([]TradeExecution, error) {
	if limit <= 0 || limit > 100000 {
		limit = 10000
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+tradeExecutionColumns+`
		 FROM trade_executions
		 WHERE status IN ('pending', 'working', 'triggered', 'partial')
		 ORDER BY fund_id, created_at, id
		 LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("trade_repo: list open across funds: %w", err)
	}
	defer rows.Close()
	return scanTradeExecutions(rows)
}

// ListActiveStopByFund returns active stop / stop_limit / trailing_stop
// trades for the fund. The stop-trigger engine (P0-3) scans these on
// every quote tick to decide whether to fire a child order.
func (r *TradeRepo) ListActiveStopByFund(ctx context.Context, fundID string) ([]TradeExecution, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+tradeExecutionColumns+`
		 FROM trade_executions
		 WHERE fund_id = $1
		   AND order_type IN ('stop', 'stop_limit', 'trailing_stop')
		   AND status IN ('pending', 'working')`,
		fundID,
	)
	if err != nil {
		return nil, fmt.Errorf("trade_repo: list active stop by fund: %w", err)
	}
	defer rows.Close()
	return scanTradeExecutions(rows)
}

// GetByClientIdempotencyKey looks up a trade by its caller-minted
// idempotency key. Used during order-replay on restart (P1-5) when we
// have the client_order_id but the broker_order_id was never
// persisted (e.g. the process died after PlaceOrder accepted the
// order but before the response was written back).
func (r *TradeRepo) GetByClientIdempotencyKey(ctx context.Context, fundID, key string) (*TradeExecution, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+tradeExecutionColumns+`
		 FROM trade_executions
		 WHERE fund_id = $1 AND client_idempotency_key = $2
		 LIMIT 1`,
		fundID, key,
	)
	if err != nil {
		return nil, fmt.Errorf("trade_repo: get by idempotency key: %w", err)
	}
	defer rows.Close()
	out, err := scanTradeExecutions(rows)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return &out[0], nil
}

// SumFilledBuyTodayByInstrument returns, for each (instrument_key, symbol)
// pair held by `fundID`, the total quantity of *filled* buy trades that
// executed on `tradingDate` in the fund's trading session (today). The
// map's key is positionMapKey(instrumentKey, symbol) — matching the
// trading engine's positionsByKey indexing — and the value is the sum
// of FilledQty when present, falling back to Quantity for legacy rows
// that didn't populate FilledQty.
//
// Used by the PM to compute SellableQtyToday: on T+1 markets the
// freshly bought quantity is locked from being sold during the same
// trading day. This query is the upstream source of truth for that
// signal; the holding_positions.available_qty column is the downstream
// cache (kept in sync by mergeBoughtPosition + Settle), but querying
// the trades table directly insulates the PM from any drift in that
// cache (e.g. historical rows persisted before the T+1 rollout).
//
// `tradingDate` is interpreted as the start of the trading-day window;
// the upper bound is `tradingDate + 24h`. Pass a zero-value time to
// disable the date filter (used by some tests that don't care about
// the boundary).
func (r *TradeRepo) SumFilledBuyTodayByInstrument(ctx context.Context, fundID string, tradingDate time.Time) (map[string]float64, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if tradingDate.IsZero() {
		rows, err = r.db.QueryContext(ctx,
			`SELECT instrument_key, symbol, COALESCE(SUM(GREATEST(filled_qty, quantity)), 0)
			 FROM trade_executions
			 WHERE fund_id = $1
			   AND side = 'buy'
			   AND status = 'filled'
			 GROUP BY instrument_key, symbol`,
			fundID,
		)
	} else {
		// Use a 24h window from the trading-day boundary. We index
		// on (fund_id, created_at DESC) so this stays cheap even at
		// high trade volume.
		end := tradingDate.Add(24 * time.Hour)
		rows, err = r.db.QueryContext(ctx,
			`SELECT instrument_key, symbol, COALESCE(SUM(GREATEST(filled_qty, quantity)), 0)
			 FROM trade_executions
			 WHERE fund_id = $1
			   AND side = 'buy'
			   AND status = 'filled'
			   AND created_at >= $2
			   AND created_at <  $3
			 GROUP BY instrument_key, symbol`,
			fundID, tradingDate, end,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("trade_repo: sum filled buy today: %w", err)
	}
	defer rows.Close()
	out := make(map[string]float64)
	for rows.Next() {
		var (
			instrumentKey sql.NullString
			symbol        string
			sum           float64
		)
		if err := rows.Scan(&instrumentKey, &symbol, &sum); err != nil {
			return nil, fmt.Errorf("trade_repo: scan sum row: %w", err)
		}
		// Mirror cmd/server's positionMapKey: instrument key if
		// present, otherwise the bare symbol. Centralising the format
		// in cmd/server would create a dep cycle, so we replicate the
		// one-line rule here and rely on callers to know the contract.
		key := strings.TrimSpace(instrumentKey.String)
		if key == "" {
			key = symbol
		}
		out[key] = sum
	}
	return out, rows.Err()
}

func scanTradeExecutions(rows *sql.Rows) ([]TradeExecution, error) {
	var trades []TradeExecution
	for rows.Next() {
		var t TradeExecution
		if err := rows.Scan(
			&t.ID, &t.FundID, &t.PlanID, &t.PlanActionID, &t.InstrumentKey, &t.Symbol,
			&t.Market, &t.Exchange, &t.AssetClass, &t.InstrumentType, &t.Side, &t.PositionSide,
			&t.OpenClose, &t.OrderType, &t.Quantity, &t.Price, &t.Amount, &t.FilledQty,
			&t.FilledPrice, &t.FeeCommission, &t.FeeStampTax, &t.FeeTransfer,
			&t.TradingMode, &t.BrokerOrderID, &t.MCPServerID, &t.Status, &t.ExecutedAt,
			&t.QuoteCurrency, &t.SettlementCurrency, &t.MarginMode, &t.Leverage,
			&t.ContractMultiplier, &t.ExpiryDate, &t.ReduceOnly, &t.SlippagePct,
			&t.StopPrice, &t.TrailAmount, &t.TrailPercent, &t.DisplayQty,
			&t.TimeInForce, &t.GoodTillDate, &t.ParentTradeID,
			&t.Strategy, &t.StrategyParentTradeID,
			&t.ClientIdempotencyKey, &t.CreatedAt,
			&t.CancelledAt, &t.CancelReason, &t.ReplacedAt, &t.ReplaceCount,
		); err != nil {
			return nil, fmt.Errorf("trade_repo: scan row: %w", err)
		}
		trades = append(trades, t)
	}
	return trades, rows.Err()
}

// UpdateStatus marks a trade as filled (or any other terminal status)
// and records the realised execution-time slippage in slippage_pct
// (NULL for sells or pre-rollout fills). The slippagePct argument is
// the signed fractional drift from the plan reference price to the
// actual fill price; pass an invalid sql.NullFloat64 to leave the
// column NULL.
func (r *TradeRepo) UpdateStatus(ctx context.Context, tradeID, status string, filledQty float64, filledPrice sql.NullFloat64, feeCommission, feeStampTax, feeTransfer float64, slippagePct sql.NullFloat64) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE trade_executions
		 SET status = $1, filled_qty = $2, filled_price = $3, fee_commission = $4, fee_stamp_tax = $5, fee_transfer = $6, slippage_pct = $7, executed_at = NOW()
		 WHERE id = $8`,
		status, filledQty, filledPrice, feeCommission, feeStampTax, feeTransfer, slippagePct, tradeID,
	)
	if err != nil {
		return fmt.Errorf("trade_repo: update status: %w", err)
	}
	return checkRowsAffected(res, "trade_repo: update status")
}

// GetByIDForFund returns a single trade scoped to a fund. Used by
// the Cancel / Replace API (P0-5) which must enforce ownership before
// allowing modification.
//
// Returns sql.ErrNoRows when (fundID, tradeID) does not exist OR when
// it exists but belongs to a different fund — leaking "exists but
// wrong fund" would let an attacker probe the trade-id space.
func (r *TradeRepo) GetByIDForFund(ctx context.Context, fundID, tradeID string) (*TradeExecution, error) {
	if strings.TrimSpace(fundID) == "" || strings.TrimSpace(tradeID) == "" {
		return nil, sql.ErrNoRows
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+tradeExecutionColumns+" FROM trade_executions WHERE id = $1 AND fund_id = $2 LIMIT 1",
		tradeID, fundID,
	)
	if err != nil {
		return nil, fmt.Errorf("trade_repo: get by id: %w", err)
	}
	defer rows.Close()
	trades, err := scanTradeExecutions(rows)
	if err != nil {
		return nil, err
	}
	if len(trades) == 0 {
		return nil, sql.ErrNoRows
	}
	return &trades[0], nil
}

// CancelOrder transitions a trade to status='cancelled' and records
// the cancel timestamp + reason. The transition is conditional on
// the row currently sitting in a non-terminal state — pending,
// working, triggered, or partial. Terminal rows return
// ErrTradeNotCancellable so the API can surface a 409 instead of
// silently swallowing the request.
//
// Reason MUST be one of the canonical short tags ("user_requested",
// "superseded_by_replace", "ttl", "risk_breach", "system"). Free-text
// is rejected at the column-check level (length > 0).
func (r *TradeRepo) CancelOrder(ctx context.Context, fundID, tradeID, reason string) error {
	if strings.TrimSpace(fundID) == "" || strings.TrimSpace(tradeID) == "" {
		return sql.ErrNoRows
	}
	if strings.TrimSpace(reason) == "" {
		reason = "user_requested"
	}
	const q = `
		UPDATE trade_executions
		   SET status = 'cancelled',
		       cancelled_at = NOW(),
		       cancel_reason = $1
		 WHERE id = $2
		   AND fund_id = $3
		   AND status IN ('pending', 'working', 'triggered', 'partial')`
	res, err := r.db.ExecContext(ctx, q, reason, tradeID, fundID)
	if err != nil {
		return fmt.Errorf("trade_repo: cancel order: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("trade_repo: cancel order: rows affected: %w", err)
	}
	if affected == 0 {
		return ErrTradeNotCancellable
	}
	return nil
}

// ReplaceOrderFields atomically updates the modifiable fields of an
// open trade and bumps replace_count + replaced_at. Each pointer is
// nil-as-no-change. The function refuses to replace a terminal row
// or one whose replace_count would exceed maxReplaces (32) — this
// prevents an unbounded modify loop from filling the audit chain.
//
// Field validation rules:
//
//   - quantity must be > 0 and >= filled_qty (can't shrink below
//     what's already been filled).
//   - price (limit), stop_price, trail_amount, trail_percent, and
//     display_qty must each be > 0 when supplied.
//
// Errors returned:
//
//   - ErrTradeNotReplaceable when the row is terminal or above the
//     replace cap.
//   - sql.ErrNoRows when (fund_id, trade_id) is not found.
//
// Audit logging is the caller's responsibility — see audit.LogMutation.
func (r *TradeRepo) ReplaceOrderFields(ctx context.Context, fundID, tradeID string, fields ReplaceTradeFields) (*TradeExecution, error) {
	if strings.TrimSpace(fundID) == "" || strings.TrimSpace(tradeID) == "" {
		return nil, sql.ErrNoRows
	}
	if !fields.HasChanges() {
		return nil, fmt.Errorf("trade_repo: replace requires at least one field change")
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("trade_repo: begin replace tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Lock the row for the duration of the modify so a concurrent
	// status update (e.g. matching engine flipping working → filled)
	// can't race with us.
	row := tx.QueryRowContext(ctx,
		`SELECT id, status, quantity, filled_qty, replace_count
		   FROM trade_executions
		  WHERE id = $1 AND fund_id = $2
		  FOR UPDATE`,
		tradeID, fundID,
	)
	var (
		curID           string
		curStatus       string
		curQuantity     float64
		curFilledQty    float64
		curReplaceCount int
	)
	if err := row.Scan(&curID, &curStatus, &curQuantity, &curFilledQty, &curReplaceCount); err != nil {
		if err == sql.ErrNoRows {
			return nil, err
		}
		return nil, fmt.Errorf("trade_repo: lookup for replace: %w", err)
	}
	switch curStatus {
	case "pending", "working", "triggered", "partial":
		// modifiable
	default:
		return nil, ErrTradeNotReplaceable
	}
	if curReplaceCount >= maxOrderReplaces {
		return nil, ErrTradeNotReplaceable
	}
	if fields.Quantity != nil {
		if *fields.Quantity <= 0 {
			return nil, fmt.Errorf("trade_repo: replace quantity must be > 0")
		}
		if *fields.Quantity < curFilledQty {
			return nil, fmt.Errorf("trade_repo: replace quantity below filled_qty (%v < %v)", *fields.Quantity, curFilledQty)
		}
	}

	// Compose UPDATE clauses dynamically — only the supplied fields
	// move so a NOT NULL replacement of an irrelevant column never
	// happens.
	var (
		clauses []string
		args    []any
	)
	pos := 1
	add := func(clause string, value any) {
		clauses = append(clauses, fmt.Sprintf(clause, pos))
		args = append(args, value)
		pos++
	}
	if fields.Quantity != nil {
		add("quantity = $%d", *fields.Quantity)
	}
	if fields.LimitPrice != nil {
		add("price = $%d", *fields.LimitPrice)
	}
	if fields.StopPrice != nil {
		add("stop_price = $%d", *fields.StopPrice)
	}
	if fields.TrailAmount != nil {
		add("trail_amount = $%d", *fields.TrailAmount)
	}
	if fields.TrailPercent != nil {
		add("trail_percent = $%d", *fields.TrailPercent)
	}
	if fields.DisplayQty != nil {
		add("display_qty = $%d", *fields.DisplayQty)
	}
	clauses = append(clauses, "replaced_at = NOW()", "replace_count = replace_count + 1")

	q := "UPDATE trade_executions SET " + strings.Join(clauses, ", ") +
		fmt.Sprintf(" WHERE id = $%d", pos)
	args = append(args, tradeID)

	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return nil, fmt.Errorf("trade_repo: apply replace: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("trade_repo: commit replace: %w", err)
	}
	return r.GetByIDForFund(ctx, fundID, tradeID)
}

// ReplaceTradeFields holds the optional new values for a Replace
// operation. All fields are pointer-as-no-change so callers can
// supply any subset; HasChanges reports whether at least one is set.
type ReplaceTradeFields struct {
	Quantity     *float64
	LimitPrice   *float64
	StopPrice    *float64
	TrailAmount  *float64
	TrailPercent *float64
	DisplayQty   *float64
}

// HasChanges reports whether any pointer is non-nil. A no-change
// replace is rejected at the repo boundary.
func (f ReplaceTradeFields) HasChanges() bool {
	return f.Quantity != nil ||
		f.LimitPrice != nil ||
		f.StopPrice != nil ||
		f.TrailAmount != nil ||
		f.TrailPercent != nil ||
		f.DisplayQty != nil
}

// maxOrderReplaces caps the per-order modify count. 32 chosen so a
// thoughtful UI flow (price / qty / stop / trail × a few iterations)
// never trips it, but a runaway script does.
const maxOrderReplaces = 32

// ErrTradeNotCancellable is returned by CancelOrder when the trade
// is already in a terminal state.
var ErrTradeNotCancellable = errors.New("trade_repo: trade is not in a cancellable state")

// ErrTradeNotReplaceable is returned by ReplaceOrderFields when the
// trade is terminal OR has hit the replace count cap.
var ErrTradeNotReplaceable = errors.New("trade_repo: trade is not in a replaceable state")

// ---------------------------------------------------------------------------
// PositionRepo
// ---------------------------------------------------------------------------

type PositionRepo struct {
	db *sql.DB
}

func NewPositionRepo(db *sql.DB) *PositionRepo {
	return &PositionRepo{db: db}
}

// UpdatePriceMetrics writes a fresh price snapshot for an existing
// position without touching the quantity / cost basis / margin fields.
// Used by the background quote refresher (PR-3) so a long-lived position
// keeps its CurrentPrice / MarketValue / UnrealizedPnL in sync with the
// last upstream tick.
//
// Returns sql.ErrNoRows when the (fund_id, instrument_key) pair does not
// exist so the caller can downgrade the message to a debug log instead of
// emitting a noisy warning during a transient race with delete.
func (r *PositionRepo) UpdatePriceMetrics(ctx context.Context, fundID, instrumentKey string, currentPrice float64, marketValue float64, unrealizedPnL sql.NullFloat64) error {
	if strings.TrimSpace(fundID) == "" || strings.TrimSpace(instrumentKey) == "" {
		return fmt.Errorf("position_repo: update price metrics: missing fund_id or instrument_key")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE holding_positions
		   SET current_price = $3,
		       market_value = $4,
		       unrealized_pnl = $5,
		       updated_at = NOW()
		 WHERE fund_id = $1 AND instrument_key = $2`,
		fundID, instrumentKey, currentPrice, marketValue, unrealizedPnL,
	)
	if err != nil {
		return fmt.Errorf("position_repo: update price metrics: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("position_repo: update price metrics: rows affected: %w", err)
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (r *PositionRepo) Upsert(ctx context.Context, p *HoldingPosition) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO holding_positions (fund_id, instrument_key, symbol, name, market, exchange, asset_class, instrument_type, position_side, quote_currency, settlement_currency, margin_mode, quantity, available_qty, cost_price, current_price, market_value, weight, leverage, contract_multiplier, expiry_date, unrealized_pnl, margin_used)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)
		 ON CONFLICT (fund_id, instrument_key) DO UPDATE SET
		   symbol = EXCLUDED.symbol,
		   name = EXCLUDED.name,
		   market = EXCLUDED.market,
		   exchange = EXCLUDED.exchange,
		   asset_class = EXCLUDED.asset_class,
		   instrument_type = EXCLUDED.instrument_type,
		   position_side = EXCLUDED.position_side,
		   quote_currency = EXCLUDED.quote_currency,
		   settlement_currency = EXCLUDED.settlement_currency,
		   margin_mode = EXCLUDED.margin_mode,
		   quantity = EXCLUDED.quantity,
		   available_qty = EXCLUDED.available_qty,
		   cost_price = EXCLUDED.cost_price,
		   current_price = EXCLUDED.current_price,
		   market_value = EXCLUDED.market_value,
		   weight = EXCLUDED.weight,
		   leverage = EXCLUDED.leverage,
		   contract_multiplier = EXCLUDED.contract_multiplier,
		   expiry_date = EXCLUDED.expiry_date,
		   unrealized_pnl = EXCLUDED.unrealized_pnl,
		   margin_used = EXCLUDED.margin_used,
		   updated_at = NOW()`,
		p.FundID, p.InstrumentKey, p.Symbol, p.Name, p.Market, p.Exchange, p.AssetClass, p.InstrumentType, p.PositionSide, p.QuoteCurrency, p.SettlementCurrency, p.MarginMode, p.Quantity, p.AvailableQty, p.CostPrice, p.CurrentPrice, p.MarketValue, p.Weight, p.Leverage, p.ContractMultiplier, p.ExpiryDate, p.UnrealizedPnL, p.MarginUsed,
	)
	if err != nil {
		return fmt.Errorf("position_repo: upsert: %w", err)
	}
	return nil
}

func (r *PositionRepo) ListByFund(ctx context.Context, fundID string) ([]HoldingPosition, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, fund_id, instrument_key, symbol, name, market, exchange, asset_class, instrument_type, position_side, quote_currency, settlement_currency, margin_mode, quantity, available_qty, cost_price, current_price, market_value, weight, leverage, contract_multiplier, expiry_date, unrealized_pnl, margin_used, updated_at
		 FROM holding_positions WHERE fund_id = $1 ORDER BY instrument_key`, fundID,
	)
	if err != nil {
		return nil, fmt.Errorf("position_repo: list by fund: %w", err)
	}
	defer rows.Close()

	var positions []HoldingPosition
	for rows.Next() {
		var p HoldingPosition
		if err := rows.Scan(&p.ID, &p.FundID, &p.InstrumentKey, &p.Symbol, &p.Name, &p.Market, &p.Exchange, &p.AssetClass, &p.InstrumentType, &p.PositionSide, &p.QuoteCurrency, &p.SettlementCurrency, &p.MarginMode, &p.Quantity, &p.AvailableQty,
			&p.CostPrice, &p.CurrentPrice, &p.MarketValue, &p.Weight, &p.Leverage, &p.ContractMultiplier, &p.ExpiryDate, &p.UnrealizedPnL, &p.MarginUsed, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("position_repo: scan row: %w", err)
		}
		positions = append(positions, p)
	}
	return positions, rows.Err()
}

func (r *PositionRepo) Delete(ctx context.Context, fundID, instrumentKey string) error {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM holding_positions WHERE fund_id = $1 AND instrument_key = $2`, fundID, instrumentKey,
	)
	if err != nil {
		return fmt.Errorf("position_repo: delete: %w", err)
	}
	return checkRowsAffected(res, "position_repo: delete")
}

func (r *PositionRepo) DeleteAllByFund(ctx context.Context, fundID string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM holding_positions WHERE fund_id = $1`, fundID,
	)
	if err != nil {
		return fmt.Errorf("position_repo: delete all by fund: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// NavSnapshotRepo
// ---------------------------------------------------------------------------

type NavSnapshotRepo struct {
	db *sql.DB
}

func NewNavSnapshotRepo(db *sql.DB) *NavSnapshotRepo {
	return &NavSnapshotRepo{db: db}
}

func (r *NavSnapshotRepo) DB() *sql.DB {
	return r.db
}

func (r *NavSnapshotRepo) Create(ctx context.Context, s *NavSnapshot) error {
	posJSON, err := marshalJSON(s.PositionsSnapshot)
	if err != nil {
		return fmt.Errorf("nav_snapshot_repo: marshal positions snapshot: %w", err)
	}

	// F16: idempotent on (fund_id, trading_date). A retry of the same
	// settlement step (manual admin trigger, recovery sweep, scheduler
	// double-fire) MUST collapse to an UPDATE, otherwise daily P&L
	// reports show stacked rows and overstate returns. The unique
	// constraint added in migration 027 makes ON CONFLICT safe.
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO nav_snapshots (fund_id, trading_date, nav, total_assets, total_market_value, available_cash, daily_return, total_return, positions_snapshot)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 ON CONFLICT (fund_id, trading_date) DO UPDATE
		 SET nav = EXCLUDED.nav,
		     total_assets = EXCLUDED.total_assets,
		     total_market_value = EXCLUDED.total_market_value,
		     available_cash = EXCLUDED.available_cash,
		     daily_return = EXCLUDED.daily_return,
		     total_return = EXCLUDED.total_return,
		     positions_snapshot = EXCLUDED.positions_snapshot`,
		s.FundID, s.TradingDate, s.NAV, s.TotalAssets, s.TotalMarketValue, s.AvailableCash, s.DailyReturn, s.TotalReturn, posJSON,
	)
	if err != nil {
		return fmt.Errorf("nav_snapshot_repo: create: %w", err)
	}
	return nil
}

func (r *NavSnapshotRepo) ListByFund(ctx context.Context, fundID string, from, to time.Time) ([]NavSnapshot, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, fund_id, trading_date, nav, total_assets, total_market_value, available_cash, daily_return, total_return, positions_snapshot, created_at
		 FROM nav_snapshots
		 WHERE fund_id = $1 AND trading_date >= $2 AND trading_date <= $3
		 ORDER BY trading_date`, fundID, from, to,
	)
	if err != nil {
		return nil, fmt.Errorf("nav_snapshot_repo: list by fund: %w", err)
	}
	defer rows.Close()

	var snapshots []NavSnapshot
	for rows.Next() {
		var s NavSnapshot
		if err := rows.Scan(&s.ID, &s.FundID, &s.TradingDate, &s.NAV, &s.TotalAssets, &s.TotalMarketValue, &s.AvailableCash, &s.DailyReturn, &s.TotalReturn, &s.PositionsSnapshot, &s.CreatedAt); err != nil {
			return nil, fmt.Errorf("nav_snapshot_repo: scan row: %w", err)
		}
		snapshots = append(snapshots, s)
	}
	return snapshots, rows.Err()
}

func (r *NavSnapshotRepo) GetByFundAndDate(ctx context.Context, fundID string, tradingDate time.Time) (*NavSnapshot, error) {
	s := &NavSnapshot{}
	err := r.db.QueryRowContext(ctx,
		`SELECT id, fund_id, trading_date, nav, total_assets, total_market_value, available_cash, daily_return, total_return, positions_snapshot, created_at
		 FROM nav_snapshots WHERE fund_id = $1 AND trading_date = $2
		 LIMIT 1`, fundID, tradingDate,
	).Scan(&s.ID, &s.FundID, &s.TradingDate, &s.NAV, &s.TotalAssets, &s.TotalMarketValue, &s.AvailableCash, &s.DailyReturn, &s.TotalReturn, &s.PositionsSnapshot, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("nav_snapshot_repo: get by fund and date: %w", err)
	}
	return s, nil
}

func (r *NavSnapshotRepo) GetLatest(ctx context.Context, fundID string) (*NavSnapshot, error) {
	s := &NavSnapshot{}
	err := r.db.QueryRowContext(ctx,
		`SELECT id, fund_id, trading_date, nav, total_assets, total_market_value, available_cash, daily_return, total_return, positions_snapshot, created_at
		 FROM nav_snapshots WHERE fund_id = $1
		 ORDER BY trading_date DESC LIMIT 1`, fundID,
	).Scan(&s.ID, &s.FundID, &s.TradingDate, &s.NAV, &s.TotalAssets, &s.TotalMarketValue, &s.AvailableCash, &s.DailyReturn, &s.TotalReturn, &s.PositionsSnapshot, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("nav_snapshot_repo: get latest: %w", err)
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// WorkflowRunRepo
// ---------------------------------------------------------------------------

type WorkflowRunRepo struct {
	db *sql.DB
}

func NewWorkflowRunRepo(db *sql.DB) *WorkflowRunRepo {
	return &WorkflowRunRepo{db: db}
}

func (r *WorkflowRunRepo) Create(ctx context.Context, run *WorkflowRun) (string, error) {
	stepResultsJSON, err := workflowStepResultsJSON(run.StepResults)
	if err != nil {
		return "", fmt.Errorf("workflow_run_repo: marshal step results: %w", err)
	}

	var id string
	err = r.db.QueryRowContext(ctx,
		`INSERT INTO workflow_runs (fund_id, trading_date, status, current_step, step_results, started_at, completed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id`,
		run.FundID, run.TradingDate, run.Status, run.CurrentStep, stepResultsJSON, run.StartedAt, run.CompletedAt,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("workflow_run_repo: create: %w", err)
	}
	return id, nil
}

func (r *WorkflowRunRepo) ClaimStart(ctx context.Context, fundID string, tradingDate, startedAt time.Time, initialStep string) (*WorkflowRun, bool, error) {
	return r.claimStartWithStatuses(ctx, fundID, tradingDate, startedAt, initialStep, "'pending', 'failed', 'cancelled'")
}

func (r *WorkflowRunRepo) ClaimManualStart(ctx context.Context, fundID string, tradingDate, startedAt time.Time, initialStep string) (*WorkflowRun, bool, error) {
	return r.claimStartWithStatuses(ctx, fundID, tradingDate, startedAt, initialStep, "'pending', 'failed', 'cancelled', 'completed', 'rejected'")
}

func (r *WorkflowRunRepo) claimStartWithStatuses(ctx context.Context, fundID string, tradingDate, startedAt time.Time, initialStep string, reusableStatuses string) (*WorkflowRun, bool, error) {
	startedAt = startedAt.UTC()
	stepResultsJSON, err := workflowStepResultsJSON(workflowStepResultsWithRunningStep(initialStep, startedAt))
	if err != nil {
		return nil, false, fmt.Errorf("workflow_run_repo: marshal claim step results: %w", err)
	}
	completedAt := sql.NullTime{}
	run := &WorkflowRun{}
	query := fmt.Sprintf(`INSERT INTO workflow_runs (fund_id, trading_date, status, current_step, step_results, started_at, completed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (fund_id, trading_date) DO UPDATE
		 SET status = EXCLUDED.status,
		     current_step = EXCLUDED.current_step,
		     step_results = EXCLUDED.step_results,
		     started_at = EXCLUDED.started_at,
		     completed_at = EXCLUDED.completed_at
		 WHERE workflow_runs.status IN (%s)
		 RETURNING id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at`, reusableStatuses)
	err = r.db.QueryRowContext(ctx, query,
		fundID, tradingDate, "running", initialStep, stepResultsJSON, sql.NullTime{Time: startedAt, Valid: true}, completedAt,
	).Scan(&run.ID, &run.FundID, &run.TradingDate, &run.Status, &run.CurrentStep, &run.StepResults, &run.StartedAt, &run.CompletedAt, &run.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		existing, getErr := r.GetByFundAndDate(ctx, fundID, tradingDate)
		if getErr != nil {
			return nil, false, getErr
		}
		return existing, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("workflow_run_repo: claim start: %w", err)
	}
	return run, true, nil
}

func (r *WorkflowRunRepo) ClaimManualStep(ctx context.Context, fundID string, tradingDate time.Time, step string) (*WorkflowRun, bool, error) {
	step = strings.TrimSpace(step)
	run := &WorkflowRun{}
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO workflow_runs (fund_id, trading_date, status, current_step, step_results, started_at, completed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (fund_id, trading_date) DO UPDATE
		 SET current_step = EXCLUDED.current_step
		 WHERE workflow_runs.status NOT IN ('running', 'paused', 'rejected')
		 RETURNING id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at`,
		fundID, tradingDate, "pending", step, json.RawMessage(`{}`), sql.NullTime{}, sql.NullTime{},
	).Scan(&run.ID, &run.FundID, &run.TradingDate, &run.Status, &run.CurrentStep, &run.StepResults, &run.StartedAt, &run.CompletedAt, &run.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		existing, getErr := r.GetByFundAndDate(ctx, fundID, tradingDate)
		if getErr != nil {
			return nil, false, getErr
		}
		return existing, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("workflow_run_repo: claim manual step: %w", err)
	}
	return run, true, nil
}

// ListIncomplete returns every workflow_run whose status is still "alive"
// (running / paused / pending) — i.e. anything a process restart needs to
// either resume or mark as interrupted. Returned oldest-first by trading
// date so the caller processes the most stale runs first.
//
// This is the F12 startup recovery query: callers iterate the slice and
// triage each row (resume if cleanly paused, fail-interrupt if mid-step).
// Bounded internally to avoid runaway memory on databases with thousands
// of unfinished test runs.
func (r *WorkflowRunRepo) ListIncomplete(ctx context.Context, limit int) ([]WorkflowRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at
		 FROM workflow_runs
		 WHERE status IN ('running', 'paused', 'pending')
		 ORDER BY trading_date ASC, created_at ASC
		 LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("workflow_run_repo: list incomplete: %w", err)
	}
	defer rows.Close()

	out := make([]WorkflowRun, 0, 16)
	for rows.Next() {
		var run WorkflowRun
		if err := rows.Scan(&run.ID, &run.FundID, &run.TradingDate, &run.Status, &run.CurrentStep, &run.StepResults, &run.StartedAt, &run.CompletedAt, &run.CreatedAt); err != nil {
			return nil, fmt.Errorf("workflow_run_repo: scan incomplete row: %w", err)
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workflow_run_repo: iterate incomplete rows: %w", err)
	}
	return out, nil
}

func (r *WorkflowRunRepo) Update(ctx context.Context, run *WorkflowRun) error {
	stepResultsJSON, err := workflowStepResultsJSON(run.StepResults)
	if err != nil {
		return fmt.Errorf("workflow_run_repo: marshal step results: %w", err)
	}

	res, err := r.db.ExecContext(ctx,
		`UPDATE workflow_runs
		 SET status = $1, current_step = $2, step_results = $3, started_at = $4, completed_at = $5
		 WHERE id = $6`,
		run.Status, run.CurrentStep, stepResultsJSON, run.StartedAt, run.CompletedAt, run.ID,
	)
	if err != nil {
		return fmt.Errorf("workflow_run_repo: update: %w", err)
	}
	return checkRowsAffected(res, "workflow_run_repo: update")
}

func (r *WorkflowRunRepo) UpsertMerged(ctx context.Context, run *WorkflowRun) (*WorkflowRun, error) {
	if run == nil {
		return nil, fmt.Errorf("workflow_run_repo: upsert merged: nil run")
	}
	incoming := *run
	incoming.TradingDate = normalizeWorkflowRunDate(incoming.TradingDate)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("workflow_run_repo: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := getWorkflowRunForUpdate(ctx, tx, incoming.FundID, incoming.TradingDate)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if errors.Is(err, ErrNotFound) {
		inserted, insertErr := insertWorkflowRun(ctx, tx, &incoming)
		if insertErr == nil {
			if err := tx.Commit(); err != nil {
				return nil, fmt.Errorf("workflow_run_repo: commit insert: %w", err)
			}
			return inserted, nil
		}
		if !isWorkflowRunDuplicateError(insertErr) {
			return nil, insertErr
		}
		existing, err = getWorkflowRunForUpdate(ctx, tx, incoming.FundID, incoming.TradingDate)
		if err != nil {
			return nil, err
		}
	}

	merged, err := mergeWorkflowRun(existing, &incoming)
	if err != nil {
		return nil, err
	}
	updated, err := updateWorkflowRunReturning(ctx, tx, merged)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workflow_run_repo: commit update: %w", err)
	}
	return updated, nil
}

func (r *WorkflowRunRepo) GetByFundAndDate(ctx context.Context, fundID string, tradingDate time.Time) (*WorkflowRun, error) {
	run := &WorkflowRun{}
	err := r.db.QueryRowContext(ctx,
		`SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at
		 FROM workflow_runs
		 WHERE fund_id = $1 AND trading_date = $2
		 ORDER BY created_at DESC
		 LIMIT 1`,
		fundID, tradingDate,
	).Scan(&run.ID, &run.FundID, &run.TradingDate, &run.Status, &run.CurrentStep, &run.StepResults, &run.StartedAt, &run.CompletedAt, &run.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("workflow_run_repo: get by fund and date: %w", err)
	}
	return run, nil
}

func (r *WorkflowRunRepo) GetLatestByFund(ctx context.Context, fundID string) (*WorkflowRun, error) {
	run := &WorkflowRun{}
	err := r.db.QueryRowContext(ctx,
		`SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at
		 FROM workflow_runs
		 WHERE fund_id = $1
		 ORDER BY trading_date DESC, created_at DESC
		 LIMIT 1`,
		fundID,
	).Scan(&run.ID, &run.FundID, &run.TradingDate, &run.Status, &run.CurrentStep, &run.StepResults, &run.StartedAt, &run.CompletedAt, &run.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("workflow_run_repo: get latest by fund: %w", err)
	}
	return run, nil
}

// ---------------------------------------------------------------------------
// MemoryRepo
// ---------------------------------------------------------------------------

type MemoryRepo struct {
	db *sql.DB
}

func NewMemoryRepo(db *sql.DB) *MemoryRepo {
	return &MemoryRepo{db: db}
}

func (r *MemoryRepo) DB() *sql.DB {
	return r.db
}

func (r *MemoryRepo) Create(ctx context.Context, m *Memory) (string, error) {
	applyMemoryDefaults(m)
	var id string
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO memories (fund_id, agent_id, owner_user_id, visibility, sensitivity, origin_kind, source_listing_id, layer, title, content, trading_date, tags, template_key, payload)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		 RETURNING id`,
		m.FundID, m.AgentID, m.OwnerUserID, m.Visibility, m.Sensitivity, m.OriginKind, m.SourceListingID, m.Layer, m.Title, m.Content, m.TradingDate, pq.Array(m.Tags),
		m.TemplateKey, memoryPayloadArg(m.Payload),
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("memory_repo: create: %w", err)
	}
	return id, nil
}

// memoryPayloadArg normalises a Payload field for INSERT. An empty or
// "null" RawMessage becomes a nil interface{} so pq sends a proper SQL
// NULL — passing a zero-length []byte would trip the jsonb parser with
// "invalid input syntax for type json".
func memoryPayloadArg(p json.RawMessage) any {
	if len(p) == 0 || string(p) == "null" {
		return nil
	}
	return []byte(p)
}

// applyMemoryDefaults backfills constraint-required string fields when
// the caller forgot to set them. The DB has DEFAULT clauses for these
// columns, but because Create's INSERT lists every column explicitly,
// passing an empty string bypasses the DEFAULT and hits the CHECK
// constraint (e.g. "memories_origin_kind_check" rejecting '').
//
// This is the second-level defence behind the per-call-site fixes —
// daily_review was silently failing for every fund because
// runtimeMemorySystem.writeLearningMemory built Memory{} without
// OriginKind / Visibility / Sensitivity, all three of which have
// CHECK constraints on the table. Centralising the default keeps any
// future caller from re-introducing the same wedge.
func applyMemoryDefaults(m *Memory) {
	if m == nil {
		return
	}
	if strings.TrimSpace(m.OriginKind) == "" {
		m.OriginKind = "native"
	}
	if strings.TrimSpace(m.Visibility) == "" {
		m.Visibility = "private"
	}
	if strings.TrimSpace(m.Sensitivity) == "" {
		m.Sensitivity = "internal"
	}
}

func (r *MemoryRepo) ListByFund(ctx context.Context, fundID, layer string, limit int) ([]Memory, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, fund_id, agent_id, owner_user_id, visibility, sensitivity, origin_kind, source_listing_id, layer, title, content, trading_date, tags, created_at, updated_at, template_key, payload
		 FROM memories
		 WHERE fund_id = $1 AND layer = $2
		 ORDER BY created_at DESC LIMIT $3`, fundID, layer, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("memory_repo: list by fund: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

// ExistsByFundAgentLayerDate reports whether at least one memory row
// already covers the given (fund, agent, layer, trading_date) tuple.
// agentID may be a NULL NullString to match the fund-level summary row
// (agent_id IS NULL).
//
// Used by ConsolidateDaily to dedupe intraday re-ticks: on a fund with
// 30-min cadence the workflow's StepDailyReview fires 7-8 times per
// trading day, and without this check each tick wrote a fresh row of
// near-identical "self_learning" memory, polluting the agent learning
// UI and bloating the input that the long-term reflection job later
// distills. With the check the FIRST tick of the day writes; later
// ticks early-continue and the LLM lesson call (Step D) is skipped
// entirely. We deliberately use EXISTS instead of upsert so the
// existing-row's content stays intact — the rest of the consolidate
// pipeline (attribution, reflection) is still allowed to re-run.
func (r *MemoryRepo) ExistsByFundAgentLayerDate(ctx context.Context, fundID string, agentID sql.NullString, layer string, tradingDate time.Time) (bool, error) {
	var (
		query string
		args  []any
	)
	if agentID.Valid && strings.TrimSpace(agentID.String) != "" {
		query = `SELECT 1 FROM memories
				 WHERE fund_id = $1
				   AND agent_id = $2
				   AND layer    = $3
				   AND trading_date = $4
				 LIMIT 1`
		args = []any{fundID, agentID.String, layer, tradingDate}
	} else {
		query = `SELECT 1 FROM memories
				 WHERE fund_id = $1
				   AND agent_id IS NULL
				   AND layer    = $2
				   AND trading_date = $3
				 LIMIT 1`
		args = []any{fundID, layer, tradingDate}
	}
	var one int
	err := r.db.QueryRowContext(ctx, query, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("memory_repo: exists by fund agent layer date: %w", err)
	}
	return true, nil
}

func (r *MemoryRepo) ListByFundAndDate(ctx context.Context, fundID string, tradingDate time.Time, limit int) ([]Memory, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, fund_id, agent_id, owner_user_id, visibility, sensitivity, origin_kind, source_listing_id, layer, title, content, trading_date, tags, created_at, updated_at, template_key, payload
		 FROM memories
		 WHERE fund_id = $1 AND trading_date = $2
		 ORDER BY created_at DESC LIMIT $3`,
		fundID, tradingDate, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("memory_repo: list by fund and date: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

func (r *MemoryRepo) Search(ctx context.Context, fundID, layer, keyword string) ([]Memory, error) {
	pattern := "%" + keyword + "%"
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, fund_id, agent_id, owner_user_id, visibility, sensitivity, origin_kind, source_listing_id, layer, title, content, trading_date, tags, created_at, updated_at, template_key, payload
		 FROM memories
		 WHERE fund_id = $1 AND layer = $2 AND (content ILIKE $3 OR title ILIKE $3)
		 ORDER BY created_at DESC`, fundID, layer, pattern,
	)
	if err != nil {
		return nil, fmt.Errorf("memory_repo: search: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

func (r *MemoryRepo) GetByAgent(ctx context.Context, fundID, agentID string) ([]Memory, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, fund_id, agent_id, owner_user_id, visibility, sensitivity, origin_kind, source_listing_id, layer, title, content, trading_date, tags, created_at, updated_at, template_key, payload
		 FROM memories
		 WHERE fund_id = $1 AND agent_id = $2
		 ORDER BY created_at DESC`, fundID, agentID,
	)
	if err != nil {
		return nil, fmt.Errorf("memory_repo: get by agent: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

// GetByAgentAndLayer pulls just the memories of a single layer for one
// agent — used by the A/B promotion path to lift the treatment fund's
// `long_term` reflections into the control fund. fundID scopes the query
// to a single fund (mirrors the F3 reflection isolation invariant).
func (r *MemoryRepo) GetByAgentAndLayer(ctx context.Context, fundID, agentID, layer string) ([]Memory, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, fund_id, agent_id, owner_user_id, visibility, sensitivity, origin_kind, source_listing_id, layer, title, content, trading_date, tags, created_at, updated_at, template_key, payload
		 FROM memories
		 WHERE fund_id = $1 AND agent_id = $2 AND layer = $3
		 ORDER BY created_at DESC`, fundID, agentID, layer,
	)
	if err != nil {
		return nil, fmt.Errorf("memory_repo: get by agent and layer: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

// CreateWithTx is the in-transaction variant of Create. The A/B promotion
// flow uses it so the memory clone + evolution_config update + skill_config
// rewrite + promotion-row insert all commit or roll back together.
func (r *MemoryRepo) CreateWithTx(ctx context.Context, tx *sql.Tx, m *Memory) (string, error) {
	if tx == nil {
		return "", ErrNoTx
	}
	applyMemoryDefaults(m)
	var id string
	err := tx.QueryRowContext(ctx,
		`INSERT INTO memories (fund_id, agent_id, owner_user_id, visibility, sensitivity, origin_kind, source_listing_id, layer, title, content, trading_date, tags, template_key, payload)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		 RETURNING id`,
		m.FundID, m.AgentID, m.OwnerUserID, m.Visibility, m.Sensitivity, m.OriginKind, m.SourceListingID, m.Layer, m.Title, m.Content, m.TradingDate, pq.Array(m.Tags),
		m.TemplateKey, memoryPayloadArg(m.Payload),
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("memory_repo: create (tx): %w", err)
	}
	return id, nil
}

// DeleteByIDsWithTx removes the listed memory rows inside the caller's
// transaction. Used by RollbackLearningPromotion to undo the clone step
// without touching the rest of the control fund's memories.
//
// Returns the number of rows actually deleted (0 if all ids were already
// gone — treat as no-op, the rollback is idempotent).
func (r *MemoryRepo) DeleteByIDsWithTx(ctx context.Context, tx *sql.Tx, ids []string) (int64, error) {
	if tx == nil {
		return 0, ErrNoTx
	}
	if len(ids) == 0 {
		return 0, nil
	}
	// Filter empties so we never pass "" to the IN clause (pq is strict
	// about UUID parsing).
	cleaned := make([]string, 0, len(ids))
	for _, id := range ids {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	if len(cleaned) == 0 {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM memories WHERE id = ANY($1::uuid[])`, pq.Array(cleaned),
	)
	if err != nil {
		return 0, fmt.Errorf("memory_repo: delete by ids: %w", err)
	}
	affected, _ := res.RowsAffected()
	return affected, nil
}

// scanMemories is a shared helper that scans rows into []Memory.
func scanMemories(rows *sql.Rows) ([]Memory, error) {
	var memories []Memory
	for rows.Next() {
		var m Memory
		// payloadBytes is the raw jsonb octet stream Postgres returns;
		// we re-wrap it as json.RawMessage so the API can pass it
		// through without re-marshalling. A NULL payload column lands
		// as nil and the field stays empty — the response DTO omits
		// it via the `omitempty` tag.
		var payloadBytes []byte
		if err := rows.Scan(&m.ID, &m.FundID, &m.AgentID, &m.OwnerUserID, &m.Visibility, &m.Sensitivity, &m.OriginKind, &m.SourceListingID, &m.Layer, &m.Title,
			&m.Content, &m.TradingDate, pq.Array(&m.Tags), &m.CreatedAt, &m.UpdatedAt, &m.TemplateKey, &payloadBytes); err != nil {
			return nil, fmt.Errorf("memory_repo: scan row: %w", err)
		}
		if len(payloadBytes) > 0 {
			m.Payload = json.RawMessage(payloadBytes)
		}
		memories = append(memories, m)
	}
	return memories, rows.Err()
}

func scanFunds(rows *sql.Rows) ([]Fund, error) {
	var funds []Fund
	for rows.Next() {
		var f Fund
		if err := rows.Scan(&f.ID, &f.CompanyID, &f.Name, &f.Description, &f.TradingMode, &f.InitialCapital, &f.CurrentCapital, &f.TotalAssets, &f.NAV, &f.Status, &f.Config, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, fmt.Errorf("fund_repo: scan row: %w", err)
		}
		funds = append(funds, f)
	}
	return funds, rows.Err()
}

func workflowStepResultsJSON(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return []byte(`{}`), nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("invalid JSON payload")
	}
	if string(raw) == "null" {
		return []byte(`{}`), nil
	}
	return raw, nil
}

func workflowStepResultsWithRunningStep(step string, updatedAt time.Time) json.RawMessage {
	if strings.TrimSpace(step) == "" {
		return json.RawMessage(`{}`)
	}
	payload, err := json.Marshal(map[string]map[string]string{
		strings.TrimSpace(step): {
			"status":    "running",
			"updatedAt": updatedAt.UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(payload)
}

func normalizeWorkflowRunDate(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

func isWorkflowRunTerminalStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "rejected", "cancelled":
		return true
	default:
		return false
	}
}

func isWorkflowRunDuplicateError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate key") || strings.Contains(message, "unique constraint")
}

func mergeWorkflowStepResultsJSON(existingRaw, incomingRaw json.RawMessage) (json.RawMessage, error) {
	existing := map[string]json.RawMessage{}
	incoming := map[string]json.RawMessage{}
	if len(existingRaw) > 0 && string(existingRaw) != "null" {
		if err := json.Unmarshal(existingRaw, &existing); err != nil {
			return nil, fmt.Errorf("workflow_run_repo: decode existing step results: %w", err)
		}
	}
	if len(incomingRaw) > 0 && string(incomingRaw) != "null" {
		if err := json.Unmarshal(incomingRaw, &incoming); err != nil {
			return nil, fmt.Errorf("workflow_run_repo: decode incoming step results: %w", err)
		}
	}
	for key, value := range incoming {
		existing[key] = value
	}
	if len(existing) == 0 {
		return json.RawMessage(`{}`), nil
	}
	encoded, err := json.Marshal(existing)
	if err != nil {
		return nil, fmt.Errorf("workflow_run_repo: encode merged step results: %w", err)
	}
	return json.RawMessage(encoded), nil
}

func mergeWorkflowRun(existing, incoming *WorkflowRun) (*WorkflowRun, error) {
	if incoming == nil {
		return nil, fmt.Errorf("workflow_run_repo: merge run: nil incoming")
	}
	if existing == nil {
		stepResults, err := workflowStepResultsJSON(incoming.StepResults)
		if err != nil {
			return nil, err
		}
		merged := *incoming
		merged.TradingDate = normalizeWorkflowRunDate(merged.TradingDate)
		merged.StepResults = stepResults
		return &merged, nil
	}

	merged := *existing
	if strings.TrimSpace(incoming.FundID) != "" {
		merged.FundID = strings.TrimSpace(incoming.FundID)
	}
	if !incoming.TradingDate.IsZero() {
		merged.TradingDate = normalizeWorkflowRunDate(incoming.TradingDate)
	}
	if !(isWorkflowRunTerminalStatus(existing.Status) && !isWorkflowRunTerminalStatus(incoming.Status)) && strings.TrimSpace(incoming.Status) != "" {
		merged.Status = strings.TrimSpace(incoming.Status)
	}
	if !(isWorkflowRunTerminalStatus(existing.Status) && !isWorkflowRunTerminalStatus(incoming.Status)) && incoming.CurrentStep.Valid && strings.TrimSpace(incoming.CurrentStep.String) != "" {
		merged.CurrentStep = sql.NullString{String: strings.TrimSpace(incoming.CurrentStep.String), Valid: true}
	}
	stepResults, err := mergeWorkflowStepResultsJSON(existing.StepResults, incoming.StepResults)
	if err != nil {
		return nil, err
	}
	merged.StepResults = stepResults
	if !merged.StartedAt.Valid && incoming.StartedAt.Valid {
		merged.StartedAt = sql.NullTime{Time: incoming.StartedAt.Time.UTC(), Valid: true}
	}
	if incoming.CompletedAt.Valid {
		merged.CompletedAt = sql.NullTime{Time: incoming.CompletedAt.Time.UTC(), Valid: true}
	}
	return &merged, nil
}

func getWorkflowRunForUpdate(ctx context.Context, tx *sql.Tx, fundID string, tradingDate time.Time) (*WorkflowRun, error) {
	run := &WorkflowRun{}
	err := tx.QueryRowContext(ctx,
		`SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at
		 FROM workflow_runs
		 WHERE fund_id = $1 AND trading_date = $2
		 FOR UPDATE`,
		fundID, normalizeWorkflowRunDate(tradingDate),
	).Scan(&run.ID, &run.FundID, &run.TradingDate, &run.Status, &run.CurrentStep, &run.StepResults, &run.StartedAt, &run.CompletedAt, &run.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("workflow_run_repo: get for update: %w", err)
	}
	return run, nil
}

func insertWorkflowRun(ctx context.Context, tx *sql.Tx, run *WorkflowRun) (*WorkflowRun, error) {
	stepResultsJSON, err := workflowStepResultsJSON(run.StepResults)
	if err != nil {
		return nil, fmt.Errorf("workflow_run_repo: marshal insert step results: %w", err)
	}
	inserted := &WorkflowRun{}
	err = tx.QueryRowContext(ctx,
		`INSERT INTO workflow_runs (fund_id, trading_date, status, current_step, step_results, started_at, completed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at`,
		strings.TrimSpace(run.FundID), normalizeWorkflowRunDate(run.TradingDate), strings.TrimSpace(run.Status), run.CurrentStep, stepResultsJSON, run.StartedAt, run.CompletedAt,
	).Scan(&inserted.ID, &inserted.FundID, &inserted.TradingDate, &inserted.Status, &inserted.CurrentStep, &inserted.StepResults, &inserted.StartedAt, &inserted.CompletedAt, &inserted.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("workflow_run_repo: insert: %w", err)
	}
	return inserted, nil
}

func updateWorkflowRunReturning(ctx context.Context, tx *sql.Tx, run *WorkflowRun) (*WorkflowRun, error) {
	stepResultsJSON, err := workflowStepResultsJSON(run.StepResults)
	if err != nil {
		return nil, fmt.Errorf("workflow_run_repo: marshal update step results: %w", err)
	}
	updated := &WorkflowRun{}
	err = tx.QueryRowContext(ctx,
		`UPDATE workflow_runs
		 SET status = $1, current_step = $2, step_results = $3, started_at = $4, completed_at = $5
		 WHERE id = $6
		 RETURNING id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at`,
		strings.TrimSpace(run.Status), run.CurrentStep, stepResultsJSON, run.StartedAt, run.CompletedAt, run.ID,
	).Scan(&updated.ID, &updated.FundID, &updated.TradingDate, &updated.Status, &updated.CurrentStep, &updated.StepResults, &updated.StartedAt, &updated.CompletedAt, &updated.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("workflow_run_repo: update returning: %w", err)
	}
	return updated, nil
}

// ---------------------------------------------------------------------------
// ABTestRepo
// ---------------------------------------------------------------------------

type ABTestRepo struct {
	db *sql.DB
}

func NewABTestRepo(db *sql.DB) *ABTestRepo {
	return &ABTestRepo{db: db}
}

func (r *ABTestRepo) Create(ctx context.Context, test *ABTest) (string, error) {
	variableConfigJSON, err := marshalJSON(test.VariableConfig)
	if err != nil {
		return "", fmt.Errorf("ab_test_repo: marshal variable config: %w", err)
	}
	resultsJSON, err := marshalJSON(test.Results)
	if err != nil {
		return "", fmt.Errorf("ab_test_repo: marshal results: %w", err)
	}

	var id string
	err = r.db.QueryRowContext(ctx,
		`INSERT INTO ab_tests (name, control_fund_id, treatment_fund_id, variable_type, variable_config, status, start_date, end_date, results)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING id`,
		test.Name, test.ControlFundID, test.TreatmentFundID, test.VariableType, variableConfigJSON, test.Status, test.StartDate, test.EndDate, resultsJSON,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("ab_test_repo: create: %w", err)
	}
	return id, nil
}

func (r *ABTestRepo) GetByID(ctx context.Context, id string) (*ABTest, error) {
	test := &ABTest{}
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, control_fund_id, treatment_fund_id, variable_type, variable_config, status, start_date, end_date, results, created_at, updated_at
		 FROM ab_tests WHERE id = $1`, id,
	).Scan(&test.ID, &test.Name, &test.ControlFundID, &test.TreatmentFundID, &test.VariableType, &test.VariableConfig, &test.Status, &test.StartDate, &test.EndDate, &test.Results, &test.CreatedAt, &test.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("ab_test_repo: get by id: %w", err)
	}
	return test, nil
}

func (r *ABTestRepo) ListByFund(ctx context.Context, fundID string, limit int) ([]ABTest, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, name, control_fund_id, treatment_fund_id, variable_type, variable_config, status, start_date, end_date, results, created_at, updated_at
		 FROM ab_tests
		 WHERE control_fund_id = $1 OR treatment_fund_id = $1
		 ORDER BY created_at DESC LIMIT $2`, fundID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("ab_test_repo: list by fund: %w", err)
	}
	defer rows.Close()

	var tests []ABTest
	for rows.Next() {
		var test ABTest
		if err := rows.Scan(&test.ID, &test.Name, &test.ControlFundID, &test.TreatmentFundID, &test.VariableType, &test.VariableConfig, &test.Status, &test.StartDate, &test.EndDate, &test.Results, &test.CreatedAt, &test.UpdatedAt); err != nil {
			return nil, fmt.Errorf("ab_test_repo: scan row: %w", err)
		}
		tests = append(tests, test)
	}
	return tests, rows.Err()
}

func (r *ABTestRepo) UpdateStatus(ctx context.Context, id, status string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE ab_tests SET status = $1, updated_at = NOW() WHERE id = $2`,
		status, id,
	)
	if err != nil {
		return fmt.Errorf("ab_test_repo: update status: %w", err)
	}
	return checkRowsAffected(res, "ab_test_repo: update status")
}

// ---------------------------------------------------------------------------
// TeamRepo
// ---------------------------------------------------------------------------

type TeamRepo struct {
	db *sql.DB
}

type AgentRepo struct {
	db *sql.DB
}

func NewTeamRepo(db *sql.DB) *TeamRepo {
	return &TeamRepo{db: db}
}

func NewAgentRepo(db *sql.DB) *AgentRepo {
	return &AgentRepo{db: db}
}

func (r *TeamRepo) AddMember(ctx context.Context, m *TeamMember) (string, error) {
	var id string
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO fund_team_members (fund_id, agent_id, role, focus, status)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id`,
		m.FundID, m.AgentID, m.Role, m.Focus, m.Status,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("team_repo: add member: %w", err)
	}
	return id, nil
}

func (r *TeamRepo) RemoveMember(ctx context.Context, fundID, agentID string) error {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM fund_team_members WHERE fund_id = $1 AND agent_id = $2`, fundID, agentID,
	)
	if err != nil {
		return fmt.Errorf("team_repo: remove member: %w", err)
	}
	return checkRowsAffected(res, "team_repo: remove member")
}

func (r *TeamRepo) ListByFund(ctx context.Context, fundID string) ([]TeamMember, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, fund_id, agent_id, role, focus, joined_at, status, updated_at
		 FROM fund_team_members WHERE fund_id = $1 ORDER BY joined_at`, fundID,
	)
	if err != nil {
		return nil, fmt.Errorf("team_repo: list by fund: %w", err)
	}
	defer rows.Close()

	var members []TeamMember
	for rows.Next() {
		var m TeamMember
		if err := rows.Scan(&m.ID, &m.FundID, &m.AgentID, &m.Role, &m.Focus, &m.JoinedAt, &m.Status, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("team_repo: scan row: %w", err)
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

func (r *TeamRepo) UpdateMember(ctx context.Context, m *TeamMember) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE fund_team_members
		 SET role = $1, focus = $2, status = $3, updated_at = NOW()
		 WHERE id = $4`,
		m.Role, m.Focus, m.Status, m.ID,
	)
	if err != nil {
		return fmt.Errorf("team_repo: update member: %w", err)
	}
	return checkRowsAffected(res, "team_repo: update member")
}

func (r *TeamRepo) GetMember(ctx context.Context, fundID, agentID string) (*TeamMember, error) {
	member := &TeamMember{}
	err := r.db.QueryRowContext(ctx,
		`SELECT id, fund_id, agent_id, role, focus, joined_at, status, updated_at
		 FROM fund_team_members
		 WHERE fund_id = $1 AND agent_id = $2`,
		fundID, agentID,
	).Scan(&member.ID, &member.FundID, &member.AgentID, &member.Role, &member.Focus, &member.JoinedAt, &member.Status, &member.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("team_repo: get member: %w", err)
	}
	return member, nil
}

func (r *TeamRepo) ListByAgent(ctx context.Context, agentID string) ([]TeamMember, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, fund_id, agent_id, role, focus, joined_at, status, updated_at
		 FROM fund_team_members WHERE agent_id = $1 ORDER BY joined_at`, agentID,
	)
	if err != nil {
		return nil, fmt.Errorf("team_repo: list by agent: %w", err)
	}
	defer rows.Close()

	var members []TeamMember
	for rows.Next() {
		var m TeamMember
		if err := rows.Scan(&m.ID, &m.FundID, &m.AgentID, &m.Role, &m.Focus, &m.JoinedAt, &m.Status, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("team_repo: scan row: %w", err)
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

func (r *TeamRepo) CountByFund(ctx context.Context, fundID string) (int, error) {
	var count int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM fund_team_members WHERE fund_id = $1 AND status = 'active'`, fundID,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("team_repo: count by fund: %w", err)
	}
	return count, nil
}

// ---------------------------------------------------------------------------
// Specialization (migration 087)
// ---------------------------------------------------------------------------

// TeamMemberSpecialization is the structured replacement for the
// legacy free-form `fund_team_members.focus` string when answering
// "which instruments does this researcher cover?". A row is
// OPTIONAL: a member without a row is treated as "no specialization
// set" and consumers fall back to the focus-string heuristic in
// agent_self_learning_prompts.go.
//
// Arrays are stored exactly as the caller writes them. The
// admin handler normalizes them to lower-case before persistence
// so prompt-time comparisons stay symmetric — see
// adminTeamMemberSpecializationHandler.
type TeamMemberSpecialization struct {
	MemberID    string    `json:"memberId"`
	Instruments []string  `json:"instruments"`
	Themes      []string  `json:"themes"`
	Markets     []string  `json:"markets"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// GetSpecialization fetches the structured coverage for one
// member, returning (nil, nil) when no row exists — that's a
// legitimate state, not an error. Callers MUST handle the nil
// case by falling back to the legacy focus column.
func (r *TeamRepo) GetSpecialization(ctx context.Context, memberID string) (*TeamMemberSpecialization, error) {
	spec := &TeamMemberSpecialization{}
	err := r.db.QueryRowContext(ctx,
		`SELECT member_id, instruments, themes, markets, updated_at
		 FROM fund_team_member_specialization
		 WHERE member_id = $1`,
		memberID,
	).Scan(
		&spec.MemberID,
		pq.Array(&spec.Instruments),
		pq.Array(&spec.Themes),
		pq.Array(&spec.Markets),
		&spec.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("team_repo: get specialization: %w", err)
	}
	return spec, nil
}

// UpsertSpecialization replaces the entire row (PUT semantics).
// Partial updates would require us to teach the API a separate
// PATCH grammar; the simpler "client sends the full target
// state" matches what the team-members admin UI does for the
// rest of the member fields.
//
// Returns the persisted row including the server-set updated_at,
// so the frontend can show "updated 2s ago" without a follow-up GET.
func (r *TeamRepo) UpsertSpecialization(ctx context.Context, spec *TeamMemberSpecialization) (*TeamMemberSpecialization, error) {
	if spec == nil || strings.TrimSpace(spec.MemberID) == "" {
		return nil, fmt.Errorf("team_repo: upsert specialization: member_id required")
	}
	out := &TeamMemberSpecialization{}
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO fund_team_member_specialization (member_id, instruments, themes, markets, updated_at)
		 VALUES ($1, $2, $3, $4, NOW())
		 ON CONFLICT (member_id) DO UPDATE
		   SET instruments = EXCLUDED.instruments,
		       themes      = EXCLUDED.themes,
		       markets     = EXCLUDED.markets,
		       updated_at  = NOW()
		 RETURNING member_id, instruments, themes, markets, updated_at`,
		spec.MemberID,
		pq.Array(spec.Instruments),
		pq.Array(spec.Themes),
		pq.Array(spec.Markets),
	).Scan(
		&out.MemberID,
		pq.Array(&out.Instruments),
		pq.Array(&out.Themes),
		pq.Array(&out.Markets),
		&out.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("team_repo: upsert specialization: %w", err)
	}
	return out, nil
}

// DeleteSpecialization removes the row so the consumer falls
// back to the legacy focus-string heuristic. Idempotent — no
// error when the row didn't exist.
func (r *TeamRepo) DeleteSpecialization(ctx context.Context, memberID string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM fund_team_member_specialization WHERE member_id = $1`, memberID,
	)
	if err != nil {
		return fmt.Errorf("team_repo: delete specialization: %w", err)
	}
	return nil
}

// ListSpecializationsByFund returns every member's specialization
// row for the given fund as a map keyed by member_id. Used by the
// learningContext builder to bulk-load all coverage data for the
// fund's team in one query instead of N+1 hitting the DB per
// member during prompt construction.
func (r *TeamRepo) ListSpecializationsByFund(ctx context.Context, fundID string) (map[string]*TeamMemberSpecialization, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT s.member_id, s.instruments, s.themes, s.markets, s.updated_at
		 FROM fund_team_member_specialization s
		 JOIN fund_team_members m ON m.id = s.member_id
		 WHERE m.fund_id = $1`,
		fundID,
	)
	if err != nil {
		return nil, fmt.Errorf("team_repo: list specializations by fund: %w", err)
	}
	defer rows.Close()
	out := map[string]*TeamMemberSpecialization{}
	for rows.Next() {
		spec := &TeamMemberSpecialization{}
		if err := rows.Scan(
			&spec.MemberID,
			pq.Array(&spec.Instruments),
			pq.Array(&spec.Themes),
			pq.Array(&spec.Markets),
			&spec.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("team_repo: scan specialization: %w", err)
		}
		out[spec.MemberID] = spec
	}
	return out, rows.Err()
}

func (r *AgentRepo) Create(ctx context.Context, agent *Agent) (string, error) {
	return createAgentOn(ctx, r.db, agent)
}

// CreateWithTx is the in-transaction variant used by the marketplace
// purchase flow so that the cloned agent and the wallet transfer are
// committed (or rolled back) together.
func (r *AgentRepo) CreateWithTx(ctx context.Context, tx *sql.Tx, agent *Agent) (string, error) {
	if tx == nil {
		return "", ErrNoTx
	}
	return createAgentOn(ctx, tx, agent)
}

func createAgentOn(ctx context.Context, q DBTX, agent *Agent) (string, error) {
	skillConfigJSON, err := marshalJSON(agent.SkillConfig)
	if err != nil {
		return "", fmt.Errorf("agent_repo: marshal skill config: %w", err)
	}
	domainConfigJSON, err := marshalJSON(agent.DomainConfig)
	if err != nil {
		return "", fmt.Errorf("agent_repo: marshal domain config: %w", err)
	}
	evolutionConfigJSON, err := marshalJSON(agent.EvolutionConfig)
	if err != nil {
		return "", fmt.Errorf("agent_repo: marshal evolution config: %w", err)
	}
	pendingSnapshotJSON, err := marshalJSONObject(agent.PendingMarketplaceSnapshot)
	if err != nil {
		return "", fmt.Errorf("agent_repo: marshal pending marketplace snapshot: %w", err)
	}

	var id string
	err = q.QueryRowContext(ctx,
		`INSERT INTO agents (user_id, name, role, focus, llm_model, model_provider, model_name, system_prompt, skill_config, domain_config, evolution_config, pending_marketplace_snapshot, marketplace_snapshot_imported_at, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		 RETURNING id`,
		agent.UserID, agent.Name, agent.Role, agent.Focus, agent.LLMModel, agent.ModelProvider, agent.ModelName, agent.SystemPrompt, skillConfigJSON, domainConfigJSON, evolutionConfigJSON, pendingSnapshotJSON, agent.MarketplaceSnapshotImportedAt, agent.Status,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("agent_repo: create: %w", err)
	}
	return id, nil
}

func (r *AgentRepo) GetByID(ctx context.Context, id string) (*Agent, error) {
	agent := &Agent{}
	// Historically this query omitted user_id and the marketplace
	// snapshot columns, so every caller got an Agent struct with
	// UserID="". That was harmless until P2 — when the LLM decision
	// engine started using pmAgent.UserID to drive ModelRouter's
	// agent-default lookup, a blank UserID meant the router skipped
	// the override path entirely and fell back to the platform
	// default provider (gemini), even though the agent was configured
	// to use claude. The query now mirrors GetOwnedByID's projection
	// so every Agent loaded through this repo has full identity.
	err := r.db.QueryRowContext(ctx,
		`SELECT id, user_id, name, role, focus, llm_model, model_provider, model_name, system_prompt, skill_config, domain_config, evolution_config, pending_marketplace_snapshot, marketplace_snapshot_imported_at, status, created_at, updated_at
		 FROM agents WHERE id = $1`,
		id,
	).Scan(&agent.ID, &agent.UserID, &agent.Name, &agent.Role, &agent.Focus, &agent.LLMModel, &agent.ModelProvider, &agent.ModelName, &agent.SystemPrompt, &agent.SkillConfig, &agent.DomainConfig, &agent.EvolutionConfig, &agent.PendingMarketplaceSnapshot, &agent.MarketplaceSnapshotImportedAt, &agent.Status, &agent.CreatedAt, &agent.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("agent_repo: get by id: %w", err)
	}
	return agent, nil
}

func (r *AgentRepo) Update(ctx context.Context, agent *Agent) error {
	skillConfigJSON, err := marshalJSON(agent.SkillConfig)
	if err != nil {
		return fmt.Errorf("agent_repo: marshal skill config: %w", err)
	}
	domainConfigJSON, err := marshalJSON(agent.DomainConfig)
	if err != nil {
		return fmt.Errorf("agent_repo: marshal domain config: %w", err)
	}
	evolutionConfigJSON, err := marshalJSON(agent.EvolutionConfig)
	if err != nil {
		return fmt.Errorf("agent_repo: marshal evolution config: %w", err)
	}

	res, err := r.db.ExecContext(ctx,
		`UPDATE agents
		 SET name = $1, role = $2, focus = $3, llm_model = $4, model_provider = $5, model_name = $6, system_prompt = $7,
		     skill_config = $8, domain_config = $9, evolution_config = $10, status = $11, updated_at = NOW()
		 WHERE id = $12`,
		agent.Name, agent.Role, agent.Focus, agent.LLMModel, agent.ModelProvider, agent.ModelName, agent.SystemPrompt, skillConfigJSON, domainConfigJSON, evolutionConfigJSON, agent.Status, agent.ID,
	)
	if err != nil {
		return fmt.Errorf("agent_repo: update: %w", err)
	}
	return checkRowsAffected(res, "agent_repo: update")
}

func (r *AgentRepo) GetOwnedByID(ctx context.Context, userID, id string) (*Agent, error) {
	agent := &Agent{}
	err := r.db.QueryRowContext(ctx,
		`SELECT id, user_id, name, role, focus, llm_model, model_provider, model_name, system_prompt, skill_config, domain_config, evolution_config, pending_marketplace_snapshot, marketplace_snapshot_imported_at, status, created_at, updated_at
		 FROM agents WHERE id = $1 AND user_id = $2`,
		id, userID,
	).Scan(&agent.ID, &agent.UserID, &agent.Name, &agent.Role, &agent.Focus, &agent.LLMModel, &agent.ModelProvider, &agent.ModelName, &agent.SystemPrompt, &agent.SkillConfig, &agent.DomainConfig, &agent.EvolutionConfig, &agent.PendingMarketplaceSnapshot, &agent.MarketplaceSnapshotImportedAt, &agent.Status, &agent.CreatedAt, &agent.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("agent_repo: get owned by id: %w", err)
	}
	return agent, nil
}

func (r *AgentRepo) ListByUser(ctx context.Context, userID string) ([]Agent, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, user_id, name, role, focus, llm_model, model_provider, model_name, system_prompt, skill_config, domain_config, evolution_config, pending_marketplace_snapshot, marketplace_snapshot_imported_at, status, created_at, updated_at
		 FROM agents WHERE user_id = $1 ORDER BY created_at DESC`, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("agent_repo: list by user: %w", err)
	}
	defer rows.Close()

	var agents []Agent
	for rows.Next() {
		var agent Agent
		if err := rows.Scan(&agent.ID, &agent.UserID, &agent.Name, &agent.Role, &agent.Focus, &agent.LLMModel, &agent.ModelProvider, &agent.ModelName, &agent.SystemPrompt, &agent.SkillConfig, &agent.DomainConfig, &agent.EvolutionConfig, &agent.PendingMarketplaceSnapshot, &agent.MarketplaceSnapshotImportedAt, &agent.Status, &agent.CreatedAt, &agent.UpdatedAt); err != nil {
			return nil, fmt.Errorf("agent_repo: scan user row: %w", err)
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

// ListDistinctOwners returns every user_id that owns at least one
// agent with both model_provider and model_name populated. Used by
// llmRuntime.SyncAll so the router's agent-default fallback fires
// for users who configured their PM/researchers through the agent
// editor (which writes the agents row) without ever creating a
// user_model_configs row. Returns a sorted slice for stable iteration.
func (r *AgentRepo) ListDistinctOwners(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT user_id
		  FROM agents
		 WHERE COALESCE(NULLIF(TRIM(model_provider), ''), '') <> ''
		   AND COALESCE(NULLIF(TRIM(model_name),     ''), '') <> ''
		 ORDER BY user_id`)
	if err != nil {
		return nil, fmt.Errorf("agent_repo: list distinct owners: %w", err)
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, fmt.Errorf("agent_repo: scan distinct owner: %w", err)
		}
		owners = append(owners, owner)
	}
	return owners, rows.Err()
}

func (r *AgentRepo) MarkMarketplaceSnapshotImported(ctx context.Context, agentID string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE agents SET marketplace_snapshot_imported_at = NOW(), updated_at = NOW() WHERE id = $1`, agentID,
	)
	if err != nil {
		return fmt.Errorf("agent_repo: mark marketplace snapshot imported: %w", err)
	}
	return checkRowsAffected(res, "agent_repo: mark marketplace snapshot imported")
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// marshalJSON returns a JSON byte slice suitable for JSONB columns.
// If the input is nil or empty, it returns a JSON null literal.
func marshalJSON(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return []byte("null"), nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("invalid JSON payload")
	}
	return raw, nil
}

func marshalJSONObject(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return []byte(`{}`), nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("invalid JSON payload")
	}
	return raw, nil
}

// checkRowsAffected verifies at least one row was affected; otherwise
// returns ErrNotFound so callers get a consistent "not found" signal.
func checkRowsAffected(res sql.Result, ctx string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: rows affected: %w", ctx, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
