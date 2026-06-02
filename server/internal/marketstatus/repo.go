// repo.go — DB-backed marketstatus store.
//
// Three small surfaces:
//
//   - InstrumentStatus: GetByKey (engine input), Upsert (operator
//     halt/limit), TouchQuote (called by quote pipeline to keep
//     last_quote_at fresh), List (admin view).
//   - CalendarDay: GetForDate (engine input), Upsert (operator),
//     ListRange (admin view).
//   - Events: Insert (after a Decision != allow), List (admin
//     audit view).

package marketstatus

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Repo wraps the marketstatus tables.
type Repo struct {
	db *sql.DB
}

// NewRepo constructs a Repo. nil db is rejected at first call to
// match the FX/recon/surveillance/drawdown repo pattern.
func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

// ----- InstrumentStatus -----

// GetByKey returns the live status row for an instrument. Returns
// (nil, nil) when no row exists — callers treat this as "not
// configured" and pass nil into the engine.
func (r *Repo) GetByKey(ctx context.Context, key string) (*InstrumentStatus, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("marketstatus: nil db")
	}
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("marketstatus: instrument_key required")
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT instrument_key, COALESCE(symbol, ''), COALESCE(market, ''),
		       status, COALESCE(halt_reason, ''),
		       halt_started_at, halt_until,
		       lower_limit, upper_limit,
		       last_quote_at, last_quote_price,
		       COALESCE(asset_class, 'equity'),
		       staleness_budget_seconds,
		       COALESCE(note, ''),
		       updated_at
		  FROM instrument_market_status
		 WHERE instrument_key = $1
	`, key)
	s, err := scanStatus(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

// UpsertStatusParams is the operator-write input. Pointer
// fields => leave-alone semantics; non-pointer scalars are
// always written.
type UpsertStatusParams struct {
	InstrumentKey       string
	Symbol              string
	Market              string
	Status              string // 'trading' | 'halted' | 'suspended'
	HaltReason          string
	HaltStartedAt       *time.Time
	HaltUntil           *time.Time
	LowerLimit          *float64
	UpperLimit          *float64
	AssetClass          string
	StalenessBudgetSecs *int
	Note                string
	UpdatedBy           string
}

// UpsertStatus writes the operator-controlled fields. last_quote_at
// is NOT touched here — that's TouchQuote's job.
func (r *Repo) UpsertStatus(ctx context.Context, p UpsertStatusParams) error {
	if r == nil || r.db == nil {
		return errors.New("marketstatus: nil db")
	}
	if strings.TrimSpace(p.InstrumentKey) == "" {
		return errors.New("marketstatus: instrument_key required")
	}
	switch p.Status {
	case "trading", "halted", "suspended":
	default:
		return errors.New("marketstatus: invalid status")
	}
	if p.LowerLimit != nil && p.UpperLimit != nil && *p.LowerLimit > *p.UpperLimit {
		return errors.New("marketstatus: lower_limit > upper_limit")
	}
	if p.StalenessBudgetSecs != nil {
		if *p.StalenessBudgetSecs < 1 || *p.StalenessBudgetSecs > 3600 {
			return errors.New("marketstatus: staleness_budget_seconds must be 1..3600")
		}
	}
	if p.AssetClass == "" {
		p.AssetClass = "equity"
	}
	var updatedBy any
	if strings.TrimSpace(p.UpdatedBy) != "" {
		updatedBy = p.UpdatedBy
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO instrument_market_status
		    (instrument_key, symbol, market, status, halt_reason,
		     halt_started_at, halt_until, lower_limit, upper_limit,
		     asset_class, staleness_budget_seconds, note, updated_at, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NOW(), $13)
		ON CONFLICT (instrument_key) DO UPDATE
		   SET symbol = EXCLUDED.symbol,
		       market = EXCLUDED.market,
		       status = EXCLUDED.status,
		       halt_reason = EXCLUDED.halt_reason,
		       halt_started_at = EXCLUDED.halt_started_at,
		       halt_until = EXCLUDED.halt_until,
		       lower_limit = EXCLUDED.lower_limit,
		       upper_limit = EXCLUDED.upper_limit,
		       asset_class = EXCLUDED.asset_class,
		       staleness_budget_seconds = EXCLUDED.staleness_budget_seconds,
		       note = EXCLUDED.note,
		       updated_at = NOW(),
		       updated_by = EXCLUDED.updated_by
	`,
		p.InstrumentKey, p.Symbol, p.Market, p.Status, nullableString(p.HaltReason),
		nullableTime(p.HaltStartedAt), nullableTime(p.HaltUntil),
		nullableFloatArg(p.LowerLimit), nullableFloatArg(p.UpperLimit),
		p.AssetClass, nullableIntArg(p.StalenessBudgetSecs), nullableString(p.Note),
		updatedBy,
	)
	if err != nil {
		return fmt.Errorf("marketstatus: upsert status: %w", err)
	}
	return nil
}

// TouchQuote refreshes last_quote_at + last_quote_price. Called
// by the market-data ingest path; keep it cheap.
func (r *Repo) TouchQuote(ctx context.Context, key, symbol, market, assetClass string, price float64, at time.Time) error {
	if r == nil || r.db == nil {
		return errors.New("marketstatus: nil db")
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("marketstatus: instrument_key required")
	}
	if assetClass == "" {
		assetClass = "equity"
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO instrument_market_status
		    (instrument_key, symbol, market, status, asset_class,
		     last_quote_at, last_quote_price, updated_at)
		VALUES ($1, $2, $3, 'trading', $4, $5, $6, NOW())
		ON CONFLICT (instrument_key) DO UPDATE
		   SET last_quote_at = EXCLUDED.last_quote_at,
		       last_quote_price = EXCLUDED.last_quote_price,
		       updated_at = NOW()
	`,
		key, symbol, market, assetClass, at.UTC(), price,
	)
	if err != nil {
		return fmt.Errorf("marketstatus: touch quote: %w", err)
	}
	return nil
}

// ListStatusParams filters the instrument_market_status list
// view used by the admin UI.
type ListStatusParams struct {
	Market string
	Status string
	Symbol string
	Limit  int
	Offset int
}

// ListStatus returns instrument rows matching the filters,
// ordered by symbol ASC. Default limit 200.
func (r *Repo) ListStatus(ctx context.Context, p ListStatusParams) ([]InstrumentStatus, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("marketstatus: nil db")
	}
	if p.Limit <= 0 || p.Limit > 1000 {
		p.Limit = 200
	}
	args := []any{p.Limit, p.Offset}
	conds := []string{}
	if strings.TrimSpace(p.Market) != "" {
		args = append(args, p.Market)
		conds = append(conds, fmt.Sprintf("market = $%d", len(args)))
	}
	if strings.TrimSpace(p.Status) != "" {
		args = append(args, p.Status)
		conds = append(conds, fmt.Sprintf("status = $%d", len(args)))
	}
	if strings.TrimSpace(p.Symbol) != "" {
		args = append(args, "%"+p.Symbol+"%")
		conds = append(conds, fmt.Sprintf("symbol ILIKE $%d", len(args)))
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	q := fmt.Sprintf(`
		SELECT instrument_key, COALESCE(symbol, ''), COALESCE(market, ''),
		       status, COALESCE(halt_reason, ''),
		       halt_started_at, halt_until,
		       lower_limit, upper_limit,
		       last_quote_at, last_quote_price,
		       COALESCE(asset_class, 'equity'),
		       staleness_budget_seconds,
		       COALESCE(note, ''),
		       updated_at
		  FROM instrument_market_status
		  %s
		 ORDER BY symbol ASC
		 LIMIT $1 OFFSET $2
	`, where)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("marketstatus: list status: %w", err)
	}
	defer rows.Close()
	var out []InstrumentStatus
	for rows.Next() {
		s, err := scanStatus(rows)
		if err != nil {
			return nil, err
		}
		if s != nil {
			out = append(out, *s)
		}
	}
	return out, rows.Err()
}

// ----- Calendar -----

// GetCalendarDay returns the row for (market, date). Returns
// (nil, nil) when the calendar has nothing for the day; the
// engine treats nil as "open by default" so a missing calendar
// doesn't block trading.
func (r *Repo) GetCalendarDay(ctx context.Context, market string, date time.Time) (*CalendarDay, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("marketstatus: nil db")
	}
	if strings.TrimSpace(market) == "" {
		return nil, errors.New("marketstatus: market required")
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT market, trading_date,
		       is_open, COALESCE(open_local, '09:30:00'), COALESCE(close_local, '15:00:00'),
		       COALESCE(market_tz, 'Asia/Shanghai'), half_day, COALESCE(note, '')
		  FROM trading_calendar
		 WHERE market = $1 AND trading_date = $2::date
	`, market, date.Format("2006-01-02"))
	d, err := scanCalendarDay(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

// UpsertCalendarDayParams is the operator-write input.
type UpsertCalendarDayParams struct {
	Market      string
	TradingDate time.Time
	IsOpen      bool
	OpenLocal   string
	CloseLocal  string
	MarketTZ    string
	HalfDay     bool
	Note        string
}

// UpsertCalendarDay writes one calendar row.
func (r *Repo) UpsertCalendarDay(ctx context.Context, p UpsertCalendarDayParams) error {
	if r == nil || r.db == nil {
		return errors.New("marketstatus: nil db")
	}
	if strings.TrimSpace(p.Market) == "" {
		return errors.New("marketstatus: market required")
	}
	if p.OpenLocal == "" {
		p.OpenLocal = "09:30:00"
	}
	if p.CloseLocal == "" {
		p.CloseLocal = "15:00:00"
	}
	if p.MarketTZ == "" {
		p.MarketTZ = "Asia/Shanghai"
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO trading_calendar
		    (market, trading_date, is_open, open_local, close_local, market_tz, half_day, note)
		VALUES ($1, $2::date, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (market, trading_date) DO UPDATE
		   SET is_open = EXCLUDED.is_open,
		       open_local = EXCLUDED.open_local,
		       close_local = EXCLUDED.close_local,
		       market_tz = EXCLUDED.market_tz,
		       half_day = EXCLUDED.half_day,
		       note = EXCLUDED.note
	`,
		p.Market, p.TradingDate.Format("2006-01-02"), p.IsOpen,
		p.OpenLocal, p.CloseLocal, p.MarketTZ, p.HalfDay, nullableString(p.Note),
	)
	if err != nil {
		return fmt.Errorf("marketstatus: upsert calendar: %w", err)
	}
	return nil
}

// ListCalendarDays returns rows in [from, to] for one market.
func (r *Repo) ListCalendarDays(ctx context.Context, market string, from, to time.Time) ([]CalendarDay, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("marketstatus: nil db")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT market, trading_date,
		       is_open, COALESCE(open_local, '09:30:00'), COALESCE(close_local, '15:00:00'),
		       COALESCE(market_tz, 'Asia/Shanghai'), half_day, COALESCE(note, '')
		  FROM trading_calendar
		 WHERE market = $1
		   AND trading_date >= $2::date
		   AND trading_date <= $3::date
		 ORDER BY trading_date ASC
	`, market, from.Format("2006-01-02"), to.Format("2006-01-02"))
	if err != nil {
		return nil, fmt.Errorf("marketstatus: list calendar: %w", err)
	}
	defer rows.Close()
	var out []CalendarDay
	for rows.Next() {
		d, err := scanCalendarDay(rows)
		if err != nil {
			return nil, err
		}
		if d != nil {
			out = append(out, *d)
		}
	}
	return out, rows.Err()
}

// ----- Events -----

// EventDetail is the on-wire shape returned by the admin event
// list. Fund + client-order-id are nullable since not every
// rule fires inside an order context.
type EventDetail struct {
	ID            string
	FundID        string
	InstrumentKey string
	Symbol        string
	Decision      Decision
	RuleCode      RuleCode
	Summary       string
	Metadata      map[string]any
	ClientOrderID string
	DetectedAt    time.Time
}

// InsertEvent persists one rule firing. Should NOT be called for
// allow decisions (no point — the event log is for explainability
// of rejects/warns).
func (r *Repo) InsertEvent(ctx context.Context, fundID, instrumentKey, symbol, clientOrderID string, ev Event) (string, error) {
	if r == nil || r.db == nil {
		return "", errors.New("marketstatus: nil db")
	}
	if ev.Decision == DecisionAllow {
		return "", errors.New("marketstatus: cannot persist 'allow' decision")
	}
	metaJSON, _ := json.Marshal(ev.Metadata)
	if len(metaJSON) == 0 || string(metaJSON) == "null" {
		metaJSON = []byte("{}")
	}
	var (
		fundArg any
		coIDArg any
	)
	if strings.TrimSpace(fundID) != "" {
		fundArg = fundID
	}
	if strings.TrimSpace(clientOrderID) != "" {
		coIDArg = clientOrderID
	}
	detected := ev.DetectedAt
	if detected.IsZero() {
		detected = time.Now().UTC()
	}
	var id string
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO marketstatus_events
		    (fund_id, instrument_key, symbol, decision, rule_code,
		     summary, metadata, client_order_id, detected_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9)
		RETURNING id::text
	`,
		fundArg, instrumentKey, nullableString(symbol),
		string(ev.Decision), string(ev.RuleCode),
		nullableString(ev.Summary), string(metaJSON), coIDArg, detected.UTC(),
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("marketstatus: insert event: %w", err)
	}
	return id, nil
}

// ListEventsParams filters the admin event list.
type ListEventsParams struct {
	FundID        string
	InstrumentKey string
	RuleCode      string
	Decision      string
	From          time.Time
	To            time.Time
	Limit         int
	Offset        int
}

// ListEvents returns events matching the filters, newest first.
func (r *Repo) ListEvents(ctx context.Context, p ListEventsParams) ([]EventDetail, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("marketstatus: nil db")
	}
	if p.Limit <= 0 || p.Limit > 500 {
		p.Limit = 100
	}
	args := []any{p.Limit, p.Offset}
	conds := []string{}
	if strings.TrimSpace(p.FundID) != "" {
		args = append(args, p.FundID)
		conds = append(conds, fmt.Sprintf("fund_id = $%d", len(args)))
	}
	if strings.TrimSpace(p.InstrumentKey) != "" {
		args = append(args, p.InstrumentKey)
		conds = append(conds, fmt.Sprintf("instrument_key = $%d", len(args)))
	}
	if strings.TrimSpace(p.RuleCode) != "" {
		args = append(args, p.RuleCode)
		conds = append(conds, fmt.Sprintf("rule_code = $%d", len(args)))
	}
	if strings.TrimSpace(p.Decision) != "" {
		args = append(args, p.Decision)
		conds = append(conds, fmt.Sprintf("decision = $%d", len(args)))
	}
	if !p.From.IsZero() {
		args = append(args, p.From.UTC())
		conds = append(conds, fmt.Sprintf("detected_at >= $%d", len(args)))
	}
	if !p.To.IsZero() {
		args = append(args, p.To.UTC())
		conds = append(conds, fmt.Sprintf("detected_at <= $%d", len(args)))
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	q := fmt.Sprintf(`
		SELECT id::text,
		       COALESCE(fund_id::text, ''), instrument_key, COALESCE(symbol, ''),
		       decision, rule_code, COALESCE(summary, ''),
		       COALESCE(metadata::text, '{}'),
		       COALESCE(client_order_id, ''),
		       detected_at
		  FROM marketstatus_events
		  %s
		 ORDER BY detected_at DESC
		 LIMIT $1 OFFSET $2
	`, where)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("marketstatus: list events: %w", err)
	}
	defer rows.Close()
	var out []EventDetail
	for rows.Next() {
		d := EventDetail{}
		var (
			decisionStr string
			ruleCodeStr string
			metaRaw     string
		)
		if err := rows.Scan(&d.ID, &d.FundID, &d.InstrumentKey, &d.Symbol,
			&decisionStr, &ruleCodeStr, &d.Summary,
			&metaRaw, &d.ClientOrderID, &d.DetectedAt); err != nil {
			return nil, err
		}
		d.Decision = Decision(decisionStr)
		d.RuleCode = RuleCode(ruleCodeStr)
		_ = json.Unmarshal([]byte(metaRaw), &d.Metadata)
		out = append(out, d)
	}
	return out, rows.Err()
}

// ----- scan helpers -----

// rowScanner is the smallest interface that *sql.Row and *sql.Rows both satisfy.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanStatus(row rowScanner) (*InstrumentStatus, error) {
	s := &InstrumentStatus{}
	var (
		haltStarted     sql.NullTime
		haltUntil       sql.NullTime
		lowerLimit      sql.NullFloat64
		upperLimit      sql.NullFloat64
		lastQuoteAt     sql.NullTime
		lastQuotePrice  sql.NullFloat64
		stalenessSecs   sql.NullInt64
	)
	if err := row.Scan(&s.InstrumentKey, &s.Symbol, &s.Market, &s.Status, &s.HaltReason,
		&haltStarted, &haltUntil, &lowerLimit, &upperLimit,
		&lastQuoteAt, &lastQuotePrice,
		&s.AssetClass, &stalenessSecs,
		&s.Note, &s.UpdatedAt); err != nil {
		return nil, err
	}
	if haltStarted.Valid {
		t := haltStarted.Time
		s.HaltStartedAt = &t
	}
	if haltUntil.Valid {
		t := haltUntil.Time
		s.HaltUntil = &t
	}
	if lowerLimit.Valid {
		f := lowerLimit.Float64
		s.LowerLimit = &f
	}
	if upperLimit.Valid {
		f := upperLimit.Float64
		s.UpperLimit = &f
	}
	if lastQuoteAt.Valid {
		t := lastQuoteAt.Time
		s.LastQuoteAt = &t
	}
	if lastQuotePrice.Valid {
		f := lastQuotePrice.Float64
		s.LastQuotePrice = &f
	}
	if stalenessSecs.Valid {
		d := time.Duration(stalenessSecs.Int64) * time.Second
		s.StalenessBudget = &d
	}
	return s, nil
}

func scanCalendarDay(row rowScanner) (*CalendarDay, error) {
	d := &CalendarDay{}
	if err := row.Scan(&d.Market, &d.TradingDate, &d.IsOpen,
		&d.OpenLocal, &d.CloseLocal, &d.MarketTZ, &d.HalfDay, &d.Note); err != nil {
		return nil, err
	}
	return d, nil
}

// ----- helpers -----

func nullableString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func nullableTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC()
}

func nullableFloatArg(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableIntArg(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}
