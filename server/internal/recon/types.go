// Package recon implements P1-3 — daily broker-statement reconciliation.
//
// What this package owns
//
//   - Domain types: Statement, StatementPosition, StatementCash,
//     StatementTrade, Run, Break.
//   - The Repo wrapping the broker_statements / reconciliation_runs /
//     reconciliation_breaks tables.
//   - The Engine that takes (statement, internal-state-snapshot) and
//     emits a list of Breaks.
//   - A MockProvider that fabricates a statement from an
//     InternalSnapshot — useful for fixtures and the day-zero
//     "statement is exactly our internal state, no breaks" test.
//
// What this package does NOT own
//
//   - Internal-state extraction. We define the InternalSnapshot
//     interface and let cmd/server adapt holding_positions +
//     cash_ledger + trade_executions into it. That keeps repository/
//     and recon/ decoupled.
//   - Scheduling. The daily runner lives in cmd/server/recon_loop.go
//     so it can hook into the same in-process scheduler as the rest
//     of the platform.
//
// Tolerances
//
// The diff engine accepts per-field tolerances (a quantity drift of
// 0.0001 lots on T-1 settlement is normal noise; flagging it would
// drown the real signal). The defaults are chosen to be tight enough
// that an actually-broken settlement is caught and loose enough that
// rounding noise doesn't fire. Callers can override per Engine
// instance so a strict month-end run can demand 1e-9 alignment.

package recon

import (
	"errors"
	"strings"
	"time"
)

// BreakType is the closed vocabulary the engine emits.
type BreakType string

const (
	BreakPositionQuantityMismatch BreakType = "position_quantity_mismatch"
	BreakPositionAvgCostMismatch  BreakType = "position_avg_cost_mismatch"
	BreakPositionMissingInternal  BreakType = "position_missing_internal"
	BreakPositionMissingBroker    BreakType = "position_missing_broker"

	BreakCashBalanceMismatch         BreakType = "cash_balance_mismatch"
	BreakCashCurrencyMissingInternal BreakType = "cash_currency_missing_internal"
	BreakCashCurrencyMissingBroker   BreakType = "cash_currency_missing_broker"

	BreakTradeMissingInternal  BreakType = "trade_missing_internal"
	BreakTradeMissingBroker    BreakType = "trade_missing_broker"
	BreakTradeQuantityMismatch BreakType = "trade_quantity_mismatch"
	BreakTradePriceMismatch    BreakType = "trade_price_mismatch"
	BreakTradeSideMismatch     BreakType = "trade_side_mismatch"
)

// Severity is how loud the break wants to be.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// RunStatus mirrors reconciliation_runs.status.
type RunStatus string

const (
	RunPending   RunStatus = "pending"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
)

// BreakStatus mirrors reconciliation_breaks.status.
type BreakStatus string

const (
	BreakOpen         BreakStatus = "open"
	BreakAcknowledged BreakStatus = "acknowledged"
	BreakResolved     BreakStatus = "resolved"
	BreakIgnored      BreakStatus = "ignored"
)

// StatementSource mirrors broker_statements.source.
type StatementSource string

const (
	SourceMock   StatementSource = "mock"
	SourceCSV    StatementSource = "csv_upload"
	SourceAPI    StatementSource = "api"
	SourceFIX    StatementSource = "fix"
)

// ----- Domain types -----

// Statement is the full statement value object. The Repo persists
// it transactionally so a partially-ingested statement never lands.
type Statement struct {
	ID            string
	FundID        string
	BrokerLinkID  string
	StatementDate time.Time
	Source        StatementSource
	PayloadHash   string
	Positions     []StatementPosition
	Cash          []StatementCash
	Trades        []StatementTrade
	IngestedBy    string
	IngestedAt    time.Time
	Status        string
	RawPayload    map[string]any
}

// StatementPosition is one symbol's quantity + cost from the broker.
type StatementPosition struct {
	Symbol      string
	Quantity    float64
	AvgCost     float64
	MarketValue float64
	Currency    string
	Metadata    map[string]any
}

// StatementCash is one currency's balance from the broker.
type StatementCash struct {
	Currency string
	Balance  float64
	Metadata map[string]any
}

// StatementTrade is one trade reported by the broker for the period.
type StatementTrade struct {
	BrokerTradeID string
	BrokerOrderID string
	Symbol        string
	Side          string
	Quantity      float64
	Price         float64
	Fee           float64
	Currency      string
	ExecutedAt    time.Time
	Metadata      map[string]any
}

// Run mirrors reconciliation_runs.
type Run struct {
	ID                  string
	FundID              string
	StatementID         string
	RunDate             time.Time
	TriggeredBy         string
	TriggerSource       string
	Status              RunStatus
	BreakCountTotal     int
	BreakCountCritical  int
	BreakCountWarning   int
	BreakCountInfo      int
	Summary             map[string]any
	StartedAt           time.Time
	CompletedAt         *time.Time
	ErrorMessage        string
}

// Break mirrors reconciliation_breaks.
type Break struct {
	ID             string
	RunID          string
	FundID         string
	Type           BreakType
	Severity       Severity
	Symbol         string
	Currency       string
	InternalValue  *float64
	BrokerValue    *float64
	DiffValue      *float64
	DiffPercent    *float64
	Description    string
	Metadata       map[string]any
	Status         BreakStatus
	ResolutionNote string
	ResolvedBy     string
	ResolvedAt     *time.Time
	CreatedAt      time.Time
}

// ----- Internal-state snapshot interface -----

// InternalPosition is what the engine compares to the broker's
// position line. Adapters in cmd/server populate it from
// holding_positions.
type InternalPosition struct {
	Symbol   string
	Quantity float64
	AvgCost  float64
	Currency string
}

// InternalCash is one currency's internal balance, summed from
// cash_ledger.
type InternalCash struct {
	Currency string
	Balance  float64
}

// InternalTrade is one of our recorded executions for the day.
// `ExternalRef` should be the broker_order_id we sent (or got back
// on fill); we use it to match against the broker's reported
// trades.
type InternalTrade struct {
	ID            string
	ExternalRef   string
	Symbol        string
	Side          string
	Quantity      float64
	Price         float64
	Fee           float64
	Currency      string
	ExecutedAt    time.Time
}

// InternalSnapshot is the lightweight value object the engine
// reads. Adapters build it from repository reads BEFORE handing
// it off — the engine itself is pure.
type InternalSnapshot struct {
	FundID    string
	AsOfDate  time.Time
	Positions []InternalPosition
	Cash      []InternalCash
	Trades    []InternalTrade
}

// ----- Errors -----

// ErrAlreadyIngested signals a duplicate statement (same
// (fund, date, source, hash)). The CSV-upload handler converts
// this to a 409 Conflict; the daily loop treats it as "yesterday's
// statement is already there, skip".
var ErrAlreadyIngested = errors.New("recon: statement already ingested")

// ErrStatementNotFound signals the row isn't in broker_statements.
var ErrStatementNotFound = errors.New("recon: statement not found")

// ErrRunNotFound signals reconciliation_runs.id missing.
var ErrRunNotFound = errors.New("recon: run not found")

// ErrBreakNotFound signals reconciliation_breaks.id missing.
var ErrBreakNotFound = errors.New("recon: break not found")

// canonicalSymbol normalises ticker case + whitespace so a
// HoldingPosition row keyed "AAPL" matches a broker line "  aapl  ".
// The platform's general convention is uppercase tickers; we apply
// it consistently here so misaligned casing doesn't masquerade as
// a missing-position break.
func canonicalSymbol(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// canonicalCurrency mirrors fx.canonicalCurrency. Duplicated
// (instead of imported) to avoid a circular dependency between
// recon → fx → recon.
func canonicalCurrency(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}
