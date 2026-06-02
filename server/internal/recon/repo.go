// repo.go — DB-backed reconciliation store (P1-3).
//
// One Repo, three logical surfaces
//
//   - Statements (CRUD on broker_statements + the three child tables).
//     IngestStatement is transactional so a half-loaded statement
//     never lands.
//   - Runs (CRUD on reconciliation_runs).
//   - Breaks (CRUD on reconciliation_breaks; ResolveBreak handles
//     the operator workflow).
//
// We keep them on one Repo because the call sites (recon_loop, the
// admin handler) typically need all three at once; passing three
// separate repos creates more wiring than encapsulation pays back.

package recon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Repo wraps the reconciliation tables. Stateless aside from the
// *sql.DB so a singleton is fine to share across handlers.
type Repo struct {
	db *sql.DB
}

// NewRepo constructs a Repo. nil db is rejected at first use rather
// than at construction so test wiring can defer DB attachment.
func NewRepo(db *sql.DB) *Repo {
	return &Repo{db: db}
}

// ----- Statement ingestion -----

// IngestParams carries the fields the caller has already parsed
// from a CSV / API payload. The Repo computes payload_hash
// deterministically from the canonicalised content so two ingestions
// of the same content collapse to one row.
type IngestParams struct {
	FundID        string
	BrokerLinkID  string
	StatementDate time.Time
	Source        StatementSource
	Positions     []StatementPosition
	Cash          []StatementCash
	Trades        []StatementTrade
	IngestedBy    string
	RawPayload    map[string]any
}

// IngestStatement is the transactional bulk-insert. The hash is
// computed BEFORE inserting; if a duplicate hash exists for the
// same (fund, date, source) we return ErrAlreadyIngested with the
// existing statement id so callers can decide whether to surface
// 409 or just no-op.
func (r *Repo) IngestStatement(ctx context.Context, p IngestParams) (*Statement, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("recon: nil db")
	}
	if strings.TrimSpace(p.FundID) == "" {
		return nil, errors.New("recon: fund_id required")
	}
	if string(p.Source) == "" {
		p.Source = SourceMock
	}
	hash := computePayloadHash(p)

	// Pre-check: does the same (fund, date, source, hash) exist?
	if existing, err := r.findStatementByHash(ctx, p.FundID, p.StatementDate, string(p.Source), hash); err == nil && existing != nil {
		return existing, ErrAlreadyIngested
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("recon: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	rawJSON, _ := json.Marshal(p.RawPayload)
	var (
		brokerLinkArg any = nil
		ingestedByArg any = nil
	)
	if strings.TrimSpace(p.BrokerLinkID) != "" {
		brokerLinkArg = p.BrokerLinkID
	}
	if strings.TrimSpace(p.IngestedBy) != "" {
		ingestedByArg = p.IngestedBy
	}

	stmtID := ""
	now := time.Now().UTC()
	err = tx.QueryRowContext(ctx, `
		INSERT INTO broker_statements
		    (fund_id, broker_link_id, statement_date, source, payload_hash,
		     raw_payload, ingested_at, ingested_by, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending')
		RETURNING id::text
	`,
		p.FundID, brokerLinkArg, p.StatementDate.UTC(), string(p.Source), hash,
		string(rawJSON), now, ingestedByArg,
	).Scan(&stmtID)
	if err != nil {
		// The unique index makes a race-with-itself a defensive
		// check; surface as ErrAlreadyIngested so callers don't
		// trip on raw pq error codes.
		if isUniqueViolation(err) {
			existing, lookupErr := r.findStatementByHash(ctx, p.FundID, p.StatementDate, string(p.Source), hash)
			if lookupErr == nil && existing != nil {
				return existing, ErrAlreadyIngested
			}
		}
		return nil, fmt.Errorf("recon: insert statement: %w", err)
	}

	if err := r.insertStatementChildren(ctx, tx, stmtID, p); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("recon: commit: %w", err)
	}

	return &Statement{
		ID:            stmtID,
		FundID:        p.FundID,
		BrokerLinkID:  p.BrokerLinkID,
		StatementDate: p.StatementDate.UTC(),
		Source:        p.Source,
		PayloadHash:   hash,
		Positions:     p.Positions,
		Cash:          p.Cash,
		Trades:        p.Trades,
		IngestedBy:    p.IngestedBy,
		IngestedAt:    now,
		Status:        "pending",
		RawPayload:    p.RawPayload,
	}, nil
}

func (r *Repo) insertStatementChildren(ctx context.Context, tx *sql.Tx, stmtID string, p IngestParams) error {
	for _, pos := range p.Positions {
		md, _ := json.Marshal(pos.Metadata)
		_, err := tx.ExecContext(ctx, `
			INSERT INTO broker_statement_positions
			    (statement_id, symbol, quantity, avg_cost, market_value, currency, metadata)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, stmtID, canonicalSymbol(pos.Symbol), pos.Quantity, pos.AvgCost,
			pos.MarketValue, canonicalCurrency(pos.Currency), string(md))
		if err != nil {
			return fmt.Errorf("recon: insert position: %w", err)
		}
	}
	for _, c := range p.Cash {
		md, _ := json.Marshal(c.Metadata)
		_, err := tx.ExecContext(ctx, `
			INSERT INTO broker_statement_cash
			    (statement_id, currency, balance, metadata)
			VALUES ($1, $2, $3, $4)
		`, stmtID, canonicalCurrency(c.Currency), c.Balance, string(md))
		if err != nil {
			return fmt.Errorf("recon: insert cash: %w", err)
		}
	}
	for _, t := range p.Trades {
		md, _ := json.Marshal(t.Metadata)
		_, err := tx.ExecContext(ctx, `
			INSERT INTO broker_statement_trades
			    (statement_id, broker_trade_id, broker_order_id, symbol, side,
			     quantity, price, fee, currency, executed_at, metadata)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, stmtID, t.BrokerTradeID, nullableString(t.BrokerOrderID),
			canonicalSymbol(t.Symbol), strings.ToLower(strings.TrimSpace(t.Side)),
			t.Quantity, t.Price, t.Fee, canonicalCurrency(t.Currency),
			t.ExecutedAt.UTC(), string(md))
		if err != nil {
			return fmt.Errorf("recon: insert trade: %w", err)
		}
	}
	return nil
}

func (r *Repo) findStatementByHash(ctx context.Context, fundID string, date time.Time, source, hash string) (*Statement, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id::text, fund_id::text, COALESCE(broker_link_id::text, ''),
		       statement_date, source, payload_hash,
		       ingested_at, COALESCE(ingested_by::text, ''), status
		  FROM broker_statements
		 WHERE fund_id = $1 AND statement_date = $2 AND source = $3 AND payload_hash = $4
	`, fundID, date.UTC(), source, hash)
	st := &Statement{}
	if err := row.Scan(&st.ID, &st.FundID, &st.BrokerLinkID,
		&st.StatementDate, (*string)(&st.Source), &st.PayloadHash,
		&st.IngestedAt, &st.IngestedBy, &st.Status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return st, nil
}

// GetStatement returns a fully-hydrated Statement (with children).
func (r *Repo) GetStatement(ctx context.Context, id string) (*Statement, error) {
	st := &Statement{}
	row := r.db.QueryRowContext(ctx, `
		SELECT id::text, fund_id::text, COALESCE(broker_link_id::text, ''),
		       statement_date, source, payload_hash, raw_payload::text,
		       ingested_at, COALESCE(ingested_by::text, ''), status
		  FROM broker_statements
		 WHERE id = $1
	`, id)
	var rawText string
	if err := row.Scan(&st.ID, &st.FundID, &st.BrokerLinkID,
		&st.StatementDate, (*string)(&st.Source), &st.PayloadHash, &rawText,
		&st.IngestedAt, &st.IngestedBy, &st.Status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrStatementNotFound
		}
		return nil, fmt.Errorf("recon: get statement: %w", err)
	}
	if rawText != "" {
		_ = json.Unmarshal([]byte(rawText), &st.RawPayload)
	}
	if err := r.loadStatementChildren(ctx, st); err != nil {
		return nil, err
	}
	return st, nil
}

func (r *Repo) loadStatementChildren(ctx context.Context, st *Statement) error {
	posRows, err := r.db.QueryContext(ctx, `
		SELECT symbol, quantity, avg_cost, market_value, currency, COALESCE(metadata::text, '{}')
		  FROM broker_statement_positions
		 WHERE statement_id = $1
		 ORDER BY symbol
	`, st.ID)
	if err != nil {
		return fmt.Errorf("recon: load positions: %w", err)
	}
	defer posRows.Close()
	for posRows.Next() {
		var p StatementPosition
		var md string
		if err := posRows.Scan(&p.Symbol, &p.Quantity, &p.AvgCost, &p.MarketValue, &p.Currency, &md); err != nil {
			return err
		}
		_ = json.Unmarshal([]byte(md), &p.Metadata)
		st.Positions = append(st.Positions, p)
	}

	cashRows, err := r.db.QueryContext(ctx, `
		SELECT currency, balance, COALESCE(metadata::text, '{}')
		  FROM broker_statement_cash
		 WHERE statement_id = $1
		 ORDER BY currency
	`, st.ID)
	if err != nil {
		return fmt.Errorf("recon: load cash: %w", err)
	}
	defer cashRows.Close()
	for cashRows.Next() {
		var c StatementCash
		var md string
		if err := cashRows.Scan(&c.Currency, &c.Balance, &md); err != nil {
			return err
		}
		_ = json.Unmarshal([]byte(md), &c.Metadata)
		st.Cash = append(st.Cash, c)
	}

	tradeRows, err := r.db.QueryContext(ctx, `
		SELECT broker_trade_id, COALESCE(broker_order_id, ''), symbol, side,
		       quantity, price, fee, currency, executed_at, COALESCE(metadata::text, '{}')
		  FROM broker_statement_trades
		 WHERE statement_id = $1
		 ORDER BY executed_at, broker_trade_id
	`, st.ID)
	if err != nil {
		return fmt.Errorf("recon: load trades: %w", err)
	}
	defer tradeRows.Close()
	for tradeRows.Next() {
		var t StatementTrade
		var md string
		if err := tradeRows.Scan(&t.BrokerTradeID, &t.BrokerOrderID, &t.Symbol, &t.Side,
			&t.Quantity, &t.Price, &t.Fee, &t.Currency, &t.ExecutedAt, &md); err != nil {
			return err
		}
		_ = json.Unmarshal([]byte(md), &t.Metadata)
		st.Trades = append(st.Trades, t)
	}
	return nil
}

// MarkStatementStatus updates broker_statements.status. Used after
// a run completes (transition to 'reconciled' or 'failed').
func (r *Repo) MarkStatementStatus(ctx context.Context, id, status string) error {
	if r == nil || r.db == nil {
		return errors.New("recon: nil db")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE broker_statements SET status = $2 WHERE id = $1
	`, id, status)
	return err
}

// ----- Run lifecycle -----

// CreateRunParams are the inputs to start a reconciliation run.
// `Result` is the engine output; the Repo persists run + breaks
// in one tx so a partial run never leaves dangling breaks.
type CreateRunParams struct {
	FundID        string
	StatementID   string
	RunDate       time.Time
	TriggeredBy   string
	TriggerSource string
	Status        RunStatus
	Result        Result
	Summary       map[string]any
	ErrorMessage  string
}

// CreateRun writes the run + its breaks transactionally.
func (r *Repo) CreateRun(ctx context.Context, p CreateRunParams) (*Run, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("recon: nil db")
	}
	if strings.TrimSpace(p.FundID) == "" || strings.TrimSpace(p.StatementID) == "" {
		return nil, errors.New("recon: fund_id and statement_id required")
	}
	if p.TriggerSource == "" {
		p.TriggerSource = "manual"
	}
	if p.Status == "" {
		p.Status = RunCompleted
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("recon: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	now := time.Now().UTC()
	var triggeredByArg any = nil
	if strings.TrimSpace(p.TriggeredBy) != "" {
		triggeredByArg = p.TriggeredBy
	}
	summaryJSON, _ := json.Marshal(p.Summary)
	if len(summaryJSON) == 0 || string(summaryJSON) == "null" {
		summaryJSON = []byte("{}")
	}

	cnt := p.Result.Counts
	total := cnt[SeverityCritical] + cnt[SeverityWarning] + cnt[SeverityInfo]

	var runID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO reconciliation_runs
		    (fund_id, statement_id, run_date, triggered_by, trigger_source, status,
		     break_count_total, break_count_critical, break_count_warning, break_count_info,
		     summary, started_at, completed_at, error_message)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING id::text
	`,
		p.FundID, p.StatementID, p.RunDate.UTC(), triggeredByArg, p.TriggerSource, string(p.Status),
		total, cnt[SeverityCritical], cnt[SeverityWarning], cnt[SeverityInfo],
		string(summaryJSON), now, now, p.ErrorMessage,
	).Scan(&runID)
	if err != nil {
		return nil, fmt.Errorf("recon: insert run: %w", err)
	}

	// Persist breaks.
	for _, b := range p.Result.Breaks {
		md, _ := json.Marshal(b.Metadata)
		if len(md) == 0 || string(md) == "" || string(md) == "null" {
			md = []byte("{}")
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO reconciliation_breaks
			    (run_id, fund_id, break_type, severity, symbol, currency,
			     internal_value, broker_value, diff_value, diff_percent,
			     description, metadata, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'open')
		`,
			runID, p.FundID, string(b.Type), string(b.Severity),
			nullableString(b.Symbol), nullableString(b.Currency),
			b.InternalValue, b.BrokerValue, b.DiffValue, b.DiffPercent,
			b.Description, string(md),
		)
		if err != nil {
			return nil, fmt.Errorf("recon: insert break: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("recon: commit: %w", err)
	}

	completed := now
	return &Run{
		ID:                 runID,
		FundID:             p.FundID,
		StatementID:        p.StatementID,
		RunDate:            p.RunDate.UTC(),
		TriggeredBy:        p.TriggeredBy,
		TriggerSource:      p.TriggerSource,
		Status:             p.Status,
		BreakCountTotal:    total,
		BreakCountCritical: cnt[SeverityCritical],
		BreakCountWarning:  cnt[SeverityWarning],
		BreakCountInfo:     cnt[SeverityInfo],
		Summary:            p.Summary,
		StartedAt:          now,
		CompletedAt:        &completed,
		ErrorMessage:       p.ErrorMessage,
	}, nil
}

// ListRunsParams filters the run list. Empty fields apply no filter.
type ListRunsParams struct {
	FundID string
	Limit  int
	Offset int
}

// ListRuns returns recent runs (no breaks attached). Use GetRun
// to fetch a single run with its full break list.
func (r *Repo) ListRuns(ctx context.Context, p ListRunsParams) ([]Run, error) {
	if p.Limit <= 0 || p.Limit > 200 {
		p.Limit = 50
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	args := []any{p.Limit, p.Offset}
	where := ""
	if strings.TrimSpace(p.FundID) != "" {
		where = "WHERE fund_id = $3"
		args = append(args, p.FundID)
	}
	q := fmt.Sprintf(`
		SELECT id::text, fund_id::text, statement_id::text, run_date,
		       COALESCE(triggered_by::text, ''), trigger_source, status,
		       break_count_total, break_count_critical, break_count_warning, break_count_info,
		       COALESCE(summary::text, '{}'),
		       started_at, completed_at, COALESCE(error_message, '')
		  FROM reconciliation_runs
		  %s
		 ORDER BY run_date DESC, started_at DESC
		 LIMIT $1 OFFSET $2
	`, where)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("recon: list runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var (
			run         Run
			summary     string
			completedAt sql.NullTime
		)
		if err := rows.Scan(&run.ID, &run.FundID, &run.StatementID, &run.RunDate,
			&run.TriggeredBy, &run.TriggerSource, (*string)(&run.Status),
			&run.BreakCountTotal, &run.BreakCountCritical, &run.BreakCountWarning, &run.BreakCountInfo,
			&summary, &run.StartedAt, &completedAt, &run.ErrorMessage); err != nil {
			return nil, err
		}
		if completedAt.Valid {
			t := completedAt.Time
			run.CompletedAt = &t
		}
		_ = json.Unmarshal([]byte(summary), &run.Summary)
		out = append(out, run)
	}
	return out, rows.Err()
}

// GetRun returns one run with its full break list.
func (r *Repo) GetRun(ctx context.Context, id string) (*Run, []Break, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id::text, fund_id::text, statement_id::text, run_date,
		       COALESCE(triggered_by::text, ''), trigger_source, status,
		       break_count_total, break_count_critical, break_count_warning, break_count_info,
		       COALESCE(summary::text, '{}'),
		       started_at, completed_at, COALESCE(error_message, '')
		  FROM reconciliation_runs
		 WHERE id = $1
	`, id)
	run := Run{}
	var (
		summary     string
		completedAt sql.NullTime
	)
	if err := row.Scan(&run.ID, &run.FundID, &run.StatementID, &run.RunDate,
		&run.TriggeredBy, &run.TriggerSource, (*string)(&run.Status),
		&run.BreakCountTotal, &run.BreakCountCritical, &run.BreakCountWarning, &run.BreakCountInfo,
		&summary, &run.StartedAt, &completedAt, &run.ErrorMessage); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrRunNotFound
		}
		return nil, nil, fmt.Errorf("recon: get run: %w", err)
	}
	if completedAt.Valid {
		t := completedAt.Time
		run.CompletedAt = &t
	}
	_ = json.Unmarshal([]byte(summary), &run.Summary)

	breaks, err := r.ListBreaks(ctx, ListBreaksParams{RunID: id, Limit: 1000})
	if err != nil {
		return nil, nil, err
	}
	return &run, breaks, nil
}

// ----- Breaks -----

// ListBreaksParams filters the break list. RunID + FundID + Status
// + Severity all narrow the result; the dashboard uses
// (FundID, Status='open', Severity='critical') for the alert
// list.
type ListBreaksParams struct {
	RunID    string
	FundID   string
	Status   string
	Severity string
	Limit    int
	Offset   int
}

// ListBreaks returns rows ordered by severity DESC, created_at DESC.
func (r *Repo) ListBreaks(ctx context.Context, p ListBreaksParams) ([]Break, error) {
	if p.Limit <= 0 || p.Limit > 1000 {
		p.Limit = 100
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	conds := []string{}
	args := []any{p.Limit, p.Offset}
	if strings.TrimSpace(p.RunID) != "" {
		args = append(args, p.RunID)
		conds = append(conds, fmt.Sprintf("run_id = $%d", len(args)))
	}
	if strings.TrimSpace(p.FundID) != "" {
		args = append(args, p.FundID)
		conds = append(conds, fmt.Sprintf("fund_id = $%d", len(args)))
	}
	if s := strings.TrimSpace(p.Status); s != "" {
		args = append(args, s)
		conds = append(conds, fmt.Sprintf("status = $%d", len(args)))
	}
	if s := strings.TrimSpace(p.Severity); s != "" {
		args = append(args, s)
		conds = append(conds, fmt.Sprintf("severity = $%d", len(args)))
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	q := fmt.Sprintf(`
		SELECT id::text, run_id::text, fund_id::text, break_type, severity,
		       COALESCE(symbol, ''), COALESCE(currency, ''),
		       internal_value, broker_value, diff_value, diff_percent,
		       COALESCE(description, ''), COALESCE(metadata::text, '{}'),
		       status, COALESCE(resolution_note, ''),
		       COALESCE(resolved_by::text, ''), resolved_at, created_at
		  FROM reconciliation_breaks
		  %s
		 ORDER BY (CASE severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END),
		          created_at DESC
		 LIMIT $1 OFFSET $2
	`, where)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("recon: list breaks: %w", err)
	}
	defer rows.Close()
	var out []Break
	for rows.Next() {
		var b Break
		var md string
		var resolvedAt sql.NullTime
		if err := rows.Scan(&b.ID, &b.RunID, &b.FundID, (*string)(&b.Type), (*string)(&b.Severity),
			&b.Symbol, &b.Currency, &b.InternalValue, &b.BrokerValue, &b.DiffValue, &b.DiffPercent,
			&b.Description, &md, (*string)(&b.Status), &b.ResolutionNote,
			&b.ResolvedBy, &resolvedAt, &b.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(md), &b.Metadata)
		if resolvedAt.Valid {
			t := resolvedAt.Time
			b.ResolvedAt = &t
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ResolveBreakParams is the input to flip a break out of 'open'.
type ResolveBreakParams struct {
	ID         string
	NewStatus  string // 'acknowledged' / 'resolved' / 'ignored'
	Note       string
	ResolvedBy string
}

// ResolveBreak transitions a break out of 'open'. We allow
// re-open (status='open' allowed) so an operator can reverse a
// premature resolution.
func (r *Repo) ResolveBreak(ctx context.Context, p ResolveBreakParams) (*Break, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("recon: nil db")
	}
	switch p.NewStatus {
	case string(BreakOpen), string(BreakAcknowledged), string(BreakResolved), string(BreakIgnored):
	default:
		return nil, fmt.Errorf("recon: invalid status %q", p.NewStatus)
	}
	now := time.Now().UTC()
	var resolvedAt any
	var resolvedBy any
	if p.NewStatus == string(BreakResolved) || p.NewStatus == string(BreakIgnored) {
		resolvedAt = now
		if strings.TrimSpace(p.ResolvedBy) != "" {
			resolvedBy = p.ResolvedBy
		}
	} else {
		// re-open / acknowledge: clear resolved_at.
		resolvedAt = nil
		resolvedBy = nil
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE reconciliation_breaks
		   SET status = $2, resolution_note = $3, resolved_by = $4, resolved_at = $5
		 WHERE id = $1
	`, p.ID, p.NewStatus, p.Note, resolvedBy, resolvedAt)
	if err != nil {
		return nil, fmt.Errorf("recon: update break: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return nil, ErrBreakNotFound
	}
	// Read back the canonical row.
	rows, err := r.ListBreaks(ctx, ListBreaksParams{Limit: 1})
	if err != nil {
		return nil, err
	}
	for _, b := range rows {
		if b.ID == p.ID {
			return &b, nil
		}
	}
	// Fallback: explicit lookup (ListBreaks ignored other filters).
	row := r.db.QueryRowContext(ctx, `
		SELECT id::text, run_id::text, fund_id::text, break_type, severity,
		       COALESCE(symbol, ''), COALESCE(currency, ''),
		       internal_value, broker_value, diff_value, diff_percent,
		       COALESCE(description, ''), COALESCE(metadata::text, '{}'),
		       status, COALESCE(resolution_note, ''),
		       COALESCE(resolved_by::text, ''), resolved_at, created_at
		  FROM reconciliation_breaks WHERE id = $1
	`, p.ID)
	var b Break
	var md string
	var ra sql.NullTime
	if err := row.Scan(&b.ID, &b.RunID, &b.FundID, (*string)(&b.Type), (*string)(&b.Severity),
		&b.Symbol, &b.Currency, &b.InternalValue, &b.BrokerValue, &b.DiffValue, &b.DiffPercent,
		&b.Description, &md, (*string)(&b.Status), &b.ResolutionNote,
		&b.ResolvedBy, &ra, &b.CreatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(md), &b.Metadata)
	if ra.Valid {
		t := ra.Time
		b.ResolvedAt = &t
	}
	return &b, nil
}

// ----- helpers -----

func nullableString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// computePayloadHash builds a deterministic SHA-256 over the
// canonicalised content of the statement so that re-ingesting the
// same content collapses on the unique index. We sort the children
// before hashing — the hash should not be sensitive to the order in
// which the parser emits rows.
func computePayloadHash(p IngestParams) string {
	type posEntry struct {
		Symbol      string
		Quantity    float64
		AvgCost     float64
		MarketValue float64
		Currency    string
	}
	type cashEntry struct {
		Currency string
		Balance  float64
	}
	type tradeEntry struct {
		BrokerTradeID string
		BrokerOrderID string
		Symbol        string
		Side          string
		Quantity      float64
		Price         float64
		Fee           float64
		Currency      string
		ExecutedAt    string
	}

	positions := make([]posEntry, 0, len(p.Positions))
	for _, x := range p.Positions {
		positions = append(positions, posEntry{
			Symbol:      canonicalSymbol(x.Symbol),
			Quantity:    x.Quantity,
			AvgCost:     x.AvgCost,
			MarketValue: x.MarketValue,
			Currency:    canonicalCurrency(x.Currency),
		})
	}
	sort.Slice(positions, func(i, j int) bool { return positions[i].Symbol < positions[j].Symbol })

	cash := make([]cashEntry, 0, len(p.Cash))
	for _, x := range p.Cash {
		cash = append(cash, cashEntry{
			Currency: canonicalCurrency(x.Currency),
			Balance:  x.Balance,
		})
	}
	sort.Slice(cash, func(i, j int) bool { return cash[i].Currency < cash[j].Currency })

	trades := make([]tradeEntry, 0, len(p.Trades))
	for _, x := range p.Trades {
		trades = append(trades, tradeEntry{
			BrokerTradeID: x.BrokerTradeID,
			BrokerOrderID: x.BrokerOrderID,
			Symbol:        canonicalSymbol(x.Symbol),
			Side:          strings.ToLower(strings.TrimSpace(x.Side)),
			Quantity:      x.Quantity,
			Price:         x.Price,
			Fee:           x.Fee,
			Currency:      canonicalCurrency(x.Currency),
			ExecutedAt:    x.ExecutedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	sort.Slice(trades, func(i, j int) bool {
		if trades[i].ExecutedAt != trades[j].ExecutedAt {
			return trades[i].ExecutedAt < trades[j].ExecutedAt
		}
		return trades[i].BrokerTradeID < trades[j].BrokerTradeID
	})

	payload := struct {
		FundID    string
		Date      string
		Positions []posEntry
		Cash      []cashEntry
		Trades    []tradeEntry
	}{
		FundID:    p.FundID,
		Date:      p.StatementDate.UTC().Format("2006-01-02"),
		Positions: positions,
		Cash:      cash,
		Trades:    trades,
	}
	bs, _ := json.Marshal(payload)
	sum := sha256.Sum256(bs)
	return hex.EncodeToString(sum[:])
}

// isUniqueViolation is a best-effort duplicate-row detector that
// avoids importing pq directly (the rest of the codebase uses
// `errors.Is` against driver-agnostic sentinels). We sniff the
// error string; the real path is the pre-check + UNIQUE INDEX, so
// this is only a defensive race fallback.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate key value") ||
		strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "23505")
}
