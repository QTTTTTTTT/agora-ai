package subscription

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// UsageTracker 用量追踪器
type UsageTracker struct {
	db            *sql.DB
	mu            sync.Mutex
	buffer        []*UsageEntry
	flushInterval time.Duration
	stopCh        chan struct{}
}

// UsageEntry 用量记录条目
type UsageEntry struct {
	ID            string    `json:"id"`
	UserID        string    `json:"user_id"`
	FundID        *string   `json:"fund_id,omitempty"`
	AgentID       *string   `json:"agent_id,omitempty"`
	StepName      string    `json:"step_name"`
	ModelProvider string    `json:"model_provider"`
	ModelName     string    `json:"model_name"`
	InputTokens   int       `json:"input_tokens"`
	OutputTokens  int       `json:"output_tokens"`
	CostCents     float64   `json:"cost_cents"`
	PriceCents    float64   `json:"price_cents"`
	IsCustomKey   bool      `json:"is_custom_key"`
	CreatedAt     time.Time `json:"created_at"`
}

type FundUsageVisibility struct {
	FundID         string               `json:"fund_id"`
	From           time.Time            `json:"from"`
	To             time.Time            `json:"to"`
	TotalCalls     int                  `json:"total_calls"`
	InputTokens    int64                `json:"input_tokens"`
	OutputTokens   int64                `json:"output_tokens"`
	CostCents      float64              `json:"cost_cents"`
	PriceCents     float64              `json:"price_cents"`
	CustomKeyCalls int                  `json:"custom_key_calls"`
	ByAgent        []FundUsageBreakdown `json:"by_agent"`
	ByStep         []FundUsageBreakdown `json:"by_step"`
	ByModel        []FundUsageBreakdown `json:"by_model"`
	RecentCalls    []*UsageEntry        `json:"recent_calls"`
}

type FundUsageBreakdown struct {
	Key            string  `json:"key"`
	TotalCalls     int     `json:"total_calls"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	CostCents      float64 `json:"cost_cents"`
	PriceCents     float64 `json:"price_cents"`
	CustomKeyCalls int     `json:"custom_key_calls"`
}

// DailySummary 每日用量汇总
type DailySummary struct {
	UserID         string          `json:"user_id"`
	SummaryDate    string          `json:"summary_date"`
	TotalCalls     int             `json:"total_calls"`
	InputTokens    int64           `json:"input_tokens"`
	OutputTokens   int64           `json:"output_tokens"`
	CostCents      float64         `json:"cost_cents"`
	PriceCents     float64         `json:"price_cents"`
	CustomKeyCalls int             `json:"custom_key_calls"`
	ModelBreakdown json.RawMessage `json:"model_breakdown"`
	StepBreakdown  json.RawMessage `json:"step_breakdown"`
}

// MonthlySummary 月度用量汇总
type MonthlySummary struct {
	UserID         string          `json:"user_id"`
	YearMonth      string          `json:"year_month"`
	TotalCalls     int             `json:"total_calls"`
	InputTokens    int64           `json:"input_tokens"`
	OutputTokens   int64           `json:"output_tokens"`
	CostCents      float64         `json:"cost_cents"`
	PriceCents     float64         `json:"price_cents"`
	CustomKeyCalls int             `json:"custom_key_calls"`
	ModelBreakdown json.RawMessage `json:"model_breakdown"`
}

// MonthlyBill 月度账单
type MonthlyBill struct {
	ID              string          `json:"id"`
	UserID          string          `json:"user_id"`
	YearMonth       string          `json:"year_month"`
	PlanTier        string          `json:"plan_tier"`
	SubscriptionFee int             `json:"subscription_fee"`
	ModelUsageFee   float64         `json:"model_usage_fee"`
	CustomKeyCredit float64         `json:"custom_key_credit"`
	TotalFee        float64         `json:"total_fee"`
	FinalAmount     float64         `json:"final_amount"`
	Status          string          `json:"status"`
	DetailsJSON     json.RawMessage `json:"details_json,omitempty"`
}

func NewUsageTracker(db *sql.DB) *UsageTracker {
	return &UsageTracker{
		db:            db,
		buffer:        make([]*UsageEntry, 0, 256),
		flushInterval: 10 * time.Second,
		stopCh:        make(chan struct{}),
	}
}

func (t *UsageTracker) Record(ctx context.Context, entry *UsageEntry) error {
	if entry == nil {
		return fmt.Errorf("entry cannot be nil")
	}
	if entry.UserID == "" {
		return fmt.Errorf("user_id is required")
	}
	if entry.ID == "" {
		entry.ID = uuid.New().String()
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}

	t.mu.Lock()
	t.buffer = append(t.buffer, entry)
	shouldFlush := len(t.buffer) >= 100
	t.mu.Unlock()

	if shouldFlush {
		return t.Flush(ctx)
	}
	return nil
}

func (t *UsageTracker) Flush(ctx context.Context) error {
	t.mu.Lock()
	if len(t.buffer) == 0 {
		t.mu.Unlock()
		return nil
	}
	entries := t.buffer
	t.buffer = make([]*UsageEntry, 0, 256)
	t.mu.Unlock()

	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		t.mu.Lock()
		t.buffer = append(entries, t.buffer...)
		t.mu.Unlock()
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO usage_entries (id, user_id, fund_id, agent_id, step_name, model_provider,
		                           model_name, input_tokens, output_tokens, cost_cents,
		                           price_cents, is_custom_key, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`)
	if err != nil {
		t.mu.Lock()
		t.buffer = append(entries, t.buffer...)
		t.mu.Unlock()
		return fmt.Errorf("prepare stmt: %w", err)
	}
	defer stmt.Close()

	for _, e := range entries {
		_, err := stmt.ExecContext(ctx, e.ID, e.UserID, e.FundID, e.AgentID, e.StepName,
			e.ModelProvider, e.ModelName, e.InputTokens, e.OutputTokens,
			e.CostCents, e.PriceCents, e.IsCustomKey, e.CreatedAt)
		if err != nil {
			t.mu.Lock()
			t.buffer = append(entries, t.buffer...)
			t.mu.Unlock()
			return fmt.Errorf("exec insert: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		t.mu.Lock()
		t.buffer = append(entries, t.buffer...)
		t.mu.Unlock()
		return fmt.Errorf("commit tx: %w", err)
	}

	return nil
}

func (t *UsageTracker) Start() {
	go func() {
		ticker := time.NewTicker(t.flushInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if err := t.Flush(ctx); err != nil {
					fmt.Printf("[UsageTracker] flush error: %v\n", err)
				}
				cancel()
			case <-t.stopCh:
				return
			}
		}
	}()
}

func (t *UsageTracker) Stop() {
	close(t.stopCh)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := t.Flush(ctx); err != nil {
		fmt.Printf("[UsageTracker] final flush error: %v\n", err)
	}
}

func (t *UsageTracker) GetDailySummary(ctx context.Context, userID string, date time.Time) (*DailySummary, error) {
	summary := &DailySummary{
		UserID:         userID,
		SummaryDate:    date.Format("2006-01-02"),
		ModelBreakdown: json.RawMessage(`{}`),
		StepBreakdown:  json.RawMessage(`{}`),
	}

	query := `
		SELECT user_id, summary_date, total_calls, input_tokens, output_tokens,
		       cost_cents, price_cents, custom_key_calls, model_breakdown, step_breakdown
		FROM usage_daily_summary
		WHERE user_id = $1 AND summary_date = $2
	`
	err := t.db.QueryRowContext(ctx, query, userID, date.Format("2006-01-02")).Scan(
		&summary.UserID,
		&summary.SummaryDate,
		&summary.TotalCalls,
		&summary.InputTokens,
		&summary.OutputTokens,
		&summary.CostCents,
		&summary.PriceCents,
		&summary.CustomKeyCalls,
		&summary.ModelBreakdown,
		&summary.StepBreakdown,
	)
	if err == sql.ErrNoRows {
		return summary, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query daily summary: %w", err)
	}
	return summary, nil
}

func (t *UsageTracker) GetMonthlySummary(ctx context.Context, userID, yearMonth string) (*MonthlySummary, error) {
	summary := &MonthlySummary{
		UserID:         userID,
		YearMonth:      yearMonth,
		ModelBreakdown: json.RawMessage(`{}`),
	}

	query := `
		SELECT user_id, year_month, total_calls, input_tokens, output_tokens,
		       cost_cents, price_cents, custom_key_calls, model_breakdown
		FROM (
			SELECT user_id,
			       TO_CHAR(summary_date, 'YYYY-MM') AS year_month,
			       SUM(total_calls) AS total_calls,
			       SUM(input_tokens) AS input_tokens,
			       SUM(output_tokens) AS output_tokens,
			       SUM(cost_cents) AS cost_cents,
			       SUM(price_cents) AS price_cents,
			       SUM(custom_key_calls) AS custom_key_calls,
			       jsonb_object_agg(model_key, model_value) FILTER (WHERE model_key IS NOT NULL) AS model_breakdown
			FROM (
				SELECT user_id,
				       summary_date,
				       total_calls,
				       input_tokens,
				       output_tokens,
				       cost_cents,
				       price_cents,
				       custom_key_calls,
				       model_entry.key AS model_key,
				       model_entry.value AS model_value
				FROM usage_daily_summary
				LEFT JOIN LATERAL jsonb_each(model_breakdown) AS model_entry(key, value) ON true
				WHERE user_id = $1 AND TO_CHAR(summary_date, 'YYYY-MM') = $2
			) monthly_rows
			GROUP BY user_id, year_month
		) aggregated
	`
	err := t.db.QueryRowContext(ctx, query, userID, yearMonth).Scan(
		&summary.UserID,
		&summary.YearMonth,
		&summary.TotalCalls,
		&summary.InputTokens,
		&summary.OutputTokens,
		&summary.CostCents,
		&summary.PriceCents,
		&summary.CustomKeyCalls,
		&summary.ModelBreakdown,
	)
	if err == sql.ErrNoRows {
		return summary, nil
	}
	if err != nil {
		fallbackQuery := `
			SELECT user_id,
			       TO_CHAR(created_at, 'YYYY-MM') AS year_month,
			       COUNT(*) AS total_calls,
			       COALESCE(SUM(input_tokens), 0) AS input_tokens,
			       COALESCE(SUM(output_tokens), 0) AS output_tokens,
			       COALESCE(SUM(cost_cents), 0) AS cost_cents,
			       COALESCE(SUM(price_cents), 0) AS price_cents,
			       COALESCE(SUM(CASE WHEN is_custom_key THEN 1 ELSE 0 END), 0) AS custom_key_calls,
			       COALESCE(jsonb_object_agg(model_name, model_totals) FILTER (WHERE model_name IS NOT NULL), '{}'::jsonb) AS model_breakdown
			FROM (
				SELECT user_id,
				       created_at,
				       model_name,
				       SUM(input_tokens) OVER (PARTITION BY user_id, TO_CHAR(created_at, 'YYYY-MM'), model_name) AS input_tokens,
				       SUM(output_tokens) OVER (PARTITION BY user_id, TO_CHAR(created_at, 'YYYY-MM'), model_name) AS output_tokens,
				       SUM(cost_cents) OVER (PARTITION BY user_id, TO_CHAR(created_at, 'YYYY-MM'), model_name) AS cost_cents,
				       SUM(price_cents) OVER (PARTITION BY user_id, TO_CHAR(created_at, 'YYYY-MM'), model_name) AS price_cents,
				       is_custom_key,
				       jsonb_build_object(
						'input_tokens', SUM(input_tokens) OVER (PARTITION BY user_id, TO_CHAR(created_at, 'YYYY-MM'), model_name),
						'output_tokens', SUM(output_tokens) OVER (PARTITION BY user_id, TO_CHAR(created_at, 'YYYY-MM'), model_name),
						'cost_cents', SUM(cost_cents) OVER (PARTITION BY user_id, TO_CHAR(created_at, 'YYYY-MM'), model_name),
						'price_cents', SUM(price_cents) OVER (PARTITION BY user_id, TO_CHAR(created_at, 'YYYY-MM'), model_name)
					) AS model_totals
				FROM usage_entries
				WHERE user_id = $1 AND TO_CHAR(created_at, 'YYYY-MM') = $2
			) usage_rows
			GROUP BY user_id, year_month
		`
		fallbackErr := t.db.QueryRowContext(ctx, fallbackQuery, userID, yearMonth).Scan(
			&summary.UserID,
			&summary.YearMonth,
			&summary.TotalCalls,
			&summary.InputTokens,
			&summary.OutputTokens,
			&summary.CostCents,
			&summary.PriceCents,
			&summary.CustomKeyCalls,
			&summary.ModelBreakdown,
		)
		if fallbackErr == sql.ErrNoRows {
			return summary, nil
		}
		if fallbackErr != nil {
			return nil, fmt.Errorf("query monthly summary: %w", err)
		}
	}
	return summary, nil
}

func (t *UsageTracker) GetUsageHistory(ctx context.Context, userID string, offset, limit int) ([]*UsageEntry, int, error) {
	countQuery := `SELECT COUNT(*) FROM usage_entries WHERE user_id = $1`
	var total int
	if err := t.db.QueryRowContext(ctx, countQuery, userID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count usage: %w", err)
	}
	if total == 0 {
		return []*UsageEntry{}, 0, nil
	}

	query := `
		SELECT id, user_id, fund_id, agent_id, step_name, model_provider, model_name,
		       input_tokens, output_tokens, cost_cents, price_cents,
		       is_custom_key, created_at
		FROM usage_entries
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`
	rows, err := t.db.QueryContext(ctx, query, userID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("query usage history: %w", err)
	}
	defer rows.Close()

	entries := make([]*UsageEntry, 0, limit)
	for rows.Next() {
		e := &UsageEntry{}
		if err := rows.Scan(
			&e.ID, &e.UserID, &e.FundID, &e.AgentID, &e.StepName,
			&e.ModelProvider, &e.ModelName, &e.InputTokens, &e.OutputTokens,
			&e.CostCents, &e.PriceCents, &e.IsCustomKey, &e.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("scan usage entry: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate usage rows: %w", err)
	}

	return entries, total, nil
}

func (t *UsageTracker) GetFundVisibility(ctx context.Context, userID, fundID string, from, to time.Time, recentLimit int) (*FundUsageVisibility, error) {
	if recentLimit <= 0 || recentLimit > 100 {
		recentLimit = 20
	}
	visibility := &FundUsageVisibility{FundID: fundID, From: from, To: to}

	summaryQuery := `
		SELECT COUNT(*) AS total_calls,
		       COALESCE(SUM(input_tokens), 0) AS input_tokens,
		       COALESCE(SUM(output_tokens), 0) AS output_tokens,
		       COALESCE(SUM(cost_cents), 0) AS cost_cents,
		       COALESCE(SUM(price_cents), 0) AS price_cents,
		       COALESCE(SUM(CASE WHEN is_custom_key THEN 1 ELSE 0 END), 0) AS custom_key_calls
		FROM usage_entries
		WHERE user_id = $1 AND fund_id = $2 AND created_at >= $3 AND created_at < $4
	`
	if err := t.db.QueryRowContext(ctx, summaryQuery, userID, fundID, from, to).Scan(
		&visibility.TotalCalls,
		&visibility.InputTokens,
		&visibility.OutputTokens,
		&visibility.CostCents,
		&visibility.PriceCents,
		&visibility.CustomKeyCalls,
	); err != nil {
		return nil, fmt.Errorf("query fund usage summary: %w", err)
	}

	var err error
	visibility.ByAgent, err = t.getFundUsageBreakdown(ctx, userID, fundID, from, to, "COALESCE(agent_id::text, 'unassigned')")
	if err != nil {
		return nil, err
	}
	visibility.ByStep, err = t.getFundUsageBreakdown(ctx, userID, fundID, from, to, "COALESCE(NULLIF(step_name, ''), 'unknown')")
	if err != nil {
		return nil, err
	}
	visibility.ByModel, err = t.getFundUsageBreakdown(ctx, userID, fundID, from, to, "model_provider || '/' || model_name")
	if err != nil {
		return nil, err
	}

	recentQuery := `
		SELECT id, user_id, fund_id, agent_id, step_name, model_provider, model_name,
		       input_tokens, output_tokens, cost_cents, price_cents,
		       is_custom_key, created_at
		FROM usage_entries
		WHERE user_id = $1 AND fund_id = $2 AND created_at >= $3 AND created_at < $4
		ORDER BY created_at DESC
		LIMIT $5
	`
	rows, err := t.db.QueryContext(ctx, recentQuery, userID, fundID, from, to, recentLimit)
	if err != nil {
		return nil, fmt.Errorf("query recent fund usage: %w", err)
	}
	defer rows.Close()
	visibility.RecentCalls = make([]*UsageEntry, 0, recentLimit)
	for rows.Next() {
		e := &UsageEntry{}
		if err := rows.Scan(
			&e.ID, &e.UserID, &e.FundID, &e.AgentID, &e.StepName,
			&e.ModelProvider, &e.ModelName, &e.InputTokens, &e.OutputTokens,
			&e.CostCents, &e.PriceCents, &e.IsCustomKey, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan recent fund usage: %w", err)
		}
		visibility.RecentCalls = append(visibility.RecentCalls, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent fund usage: %w", err)
	}
	return visibility, nil
}

func (t *UsageTracker) getFundUsageBreakdown(ctx context.Context, userID, fundID string, from, to time.Time, keyExpr string) ([]FundUsageBreakdown, error) {
	query := fmt.Sprintf(`
		SELECT %s AS key,
		       COUNT(*) AS total_calls,
		       COALESCE(SUM(input_tokens), 0) AS input_tokens,
		       COALESCE(SUM(output_tokens), 0) AS output_tokens,
		       COALESCE(SUM(cost_cents), 0) AS cost_cents,
		       COALESCE(SUM(price_cents), 0) AS price_cents,
		       COALESCE(SUM(CASE WHEN is_custom_key THEN 1 ELSE 0 END), 0) AS custom_key_calls
		FROM usage_entries
		WHERE user_id = $1 AND fund_id = $2 AND created_at >= $3 AND created_at < $4
		GROUP BY key
		ORDER BY price_cents DESC, total_calls DESC
		LIMIT 20
	`, keyExpr)
	rows, err := t.db.QueryContext(ctx, query, userID, fundID, from, to)
	if err != nil {
		return nil, fmt.Errorf("query fund usage breakdown: %w", err)
	}
	defer rows.Close()
	items := make([]FundUsageBreakdown, 0)
	for rows.Next() {
		var item FundUsageBreakdown
		if err := rows.Scan(&item.Key, &item.TotalCalls, &item.InputTokens, &item.OutputTokens, &item.CostCents, &item.PriceCents, &item.CustomKeyCalls); err != nil {
			return nil, fmt.Errorf("scan fund usage breakdown: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate fund usage breakdown: %w", err)
	}
	return items, nil
}

func (t *UsageTracker) GetBill(ctx context.Context, userID, yearMonth string) (*MonthlyBill, error) {
	bill := &MonthlyBill{
		UserID:      userID,
		YearMonth:   yearMonth,
		PlanTier:    string(PlanFree),
		Status:      "pending",
		DetailsJSON: json.RawMessage(`{}`),
	}

	storedQuery := `
		SELECT id, user_id, year_month, plan_tier, subscription_fee, model_usage_fee,
		       custom_key_credit, total_fee, final_amount, status, details_json
		FROM monthly_bills
		WHERE user_id = $1 AND year_month = $2
	`
	err := t.db.QueryRowContext(ctx, storedQuery, userID, yearMonth).Scan(
		&bill.ID,
		&bill.UserID,
		&bill.YearMonth,
		&bill.PlanTier,
		&bill.SubscriptionFee,
		&bill.ModelUsageFee,
		&bill.CustomKeyCredit,
		&bill.TotalFee,
		&bill.FinalAmount,
		&bill.Status,
		&bill.DetailsJSON,
	)
	if err == nil {
		return bill, nil
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("query monthly bill: %w", err)
	}

	monthlySummary, err := t.GetMonthlySummary(ctx, userID, yearMonth)
	if err != nil {
		return nil, fmt.Errorf("get monthly summary: %w", err)
	}
	planTier := string(PlanFree)
	subQuery := `
		SELECT plan_tier
		FROM subscriptions
		WHERE user_id = $1
		  AND status IN ('active', 'expired', 'cancelled')
		  AND TO_CHAR(start_date, 'YYYY-MM') <= $2
		  AND TO_CHAR(end_date, 'YYYY-MM') >= $2
		ORDER BY created_at DESC
		LIMIT 1
	`
	if subErr := t.db.QueryRowContext(ctx, subQuery, userID, yearMonth).Scan(&planTier); subErr != nil && subErr != sql.ErrNoRows {
		return nil, fmt.Errorf("query subscription for bill: %w", subErr)
	}

	plan, ok := Plans[PlanTier(planTier)]
	if !ok {
		plan = Plans[PlanFree]
		planTier = string(PlanFree)
	}

	customKeyQuery := `
		SELECT COALESCE(SUM(price_cents), 0)
		FROM usage_entries
		WHERE user_id = $1
		  AND TO_CHAR(created_at, 'YYYY-MM') = $2
		  AND is_custom_key = true
	`
	var customKeyCredit float64
	if err := t.db.QueryRowContext(ctx, customKeyQuery, userID, yearMonth).Scan(&customKeyCredit); err != nil {
		return nil, fmt.Errorf("query custom key credit: %w", err)
	}

	detailsJSON, err := json.Marshal(monthlySummary)
	if err != nil {
		return nil, fmt.Errorf("marshal monthly summary: %w", err)
	}

	bill.PlanTier = planTier
	bill.SubscriptionFee = plan.PriceCentsMonth
	bill.ModelUsageFee = monthlySummary.PriceCents
	bill.CustomKeyCredit = customKeyCredit
	bill.TotalFee = float64(plan.PriceCentsMonth) + monthlySummary.PriceCents
	bill.FinalAmount = bill.TotalFee - customKeyCredit
	bill.Status = "pending"
	bill.DetailsJSON = detailsJSON
	if bill.FinalAmount < 0 {
		bill.FinalAmount = 0
	}

	return bill, nil
}
