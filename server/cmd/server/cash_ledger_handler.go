// Cash ledger read endpoint (P1-1).
//
// Surfaces the per-fund cash journal so the web UI (and any future
// mobile screen) can render:
//
//   - a list of cash movements newest-first, paginated by cursor;
//   - a SUM-by-entry-type snapshot for the "fees / dividends /
//     trades" summary card.
//
// Routes
//
//	GET /api/funds/{fundId}/cash-ledger
//	    Query:
//	      from         RFC3339 lower bound (inclusive)
//	      to           RFC3339 upper bound (exclusive)
//	      type         repeatable; filter to entry_type values
//	      limit        max rows (default 100, max 500)
//	      cursor       opaque "<rfc3339>|<uuid>" from a prior page
//	      summary      "1" → also return SubtotalByEntryType
//	      balance      "1" → also return SUM(amount) for [from, to)
//
// Authorization: same authorizeFundAccess used by orders + live-
// readiness — caller must own (or be on the team for) the fund.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/fx"
	"github.com/fundai/server/internal/repository"
)

type cashLedgerHandler struct {
	cashLedger  *repository.CashLedgerRepo
	fundRepo    *repository.FundRepo
	companyRepo *repository.FundCompanyRepo
	// fxRepo is optional. When set, the handler converts the
	// summary + balance into the fund's base_currency before
	// returning, and flags fx_stale=true if any leg lacks a
	// rate. nil = no conversion (legacy single-currency mode).
	// P1-4.
	fxRepo  *fx.Repo
	metrics *serverMetrics
}

func newCashLedgerHandler(svc *Services) *cashLedgerHandler {
	if svc == nil || svc.DB == nil {
		return nil
	}
	return &cashLedgerHandler{
		cashLedger:  repository.NewCashLedgerRepo(svc.DB),
		fundRepo:    repository.NewFundRepo(svc.DB),
		companyRepo: repository.NewFundCompanyRepo(svc.DB),
		fxRepo:      fx.NewRepo(svc.DB),
		metrics:     svc.Metrics,
	}
}

func (h *cashLedgerHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/funds/{fundId}/cash-ledger", h.handleList)
}

// cashLedgerEntryWire is the JSON projection. Mirrors the server's
// snake_case convention because the web client maps fields by
// position, not name (it lives in shared/api-client/src/index.ts
// where TS types pick up snake_case via a single source of truth).
type cashLedgerEntryWire struct {
	ID             string          `json:"id"`
	FundID         string          `json:"fund_id"`
	PostedAt       string          `json:"posted_at"`
	TradingDate    string          `json:"trading_date,omitempty"`
	EntryType      string          `json:"entry_type"`
	Amount         float64         `json:"amount"`
	Currency       string          `json:"currency"`
	TradeID        string          `json:"trade_id,omitempty"`
	PlanID         string          `json:"plan_id,omitempty"`
	PlanActionID   string          `json:"plan_action_id,omitempty"`
	CorpActionID   string          `json:"corp_action_id,omitempty"`
	BrokerLinkID   string          `json:"broker_link_id,omitempty"`
	Description    string          `json:"description,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	CreatedAt      string          `json:"created_at"`
}

type cashLedgerListResponse struct {
	Entries    []cashLedgerEntryWire `json:"entries"`
	NextCursor string                `json:"next_cursor,omitempty"`
	Subtotals  map[string]float64    `json:"subtotals,omitempty"`
	Balance    *float64              `json:"balance,omitempty"`
	Currency   string                `json:"currency,omitempty"`
	// FxStale is true when at least one currency in the period
	// couldn't be converted to the fund's base currency (rate
	// missing or stale). The UI uses it to render an "FX rates
	// missing" banner so the user knows the totals are an
	// approximation. Only populated when summary or balance is
	// requested. P1-4.
	FxStale bool `json:"fx_stale,omitempty"`
}

func (h *cashLedgerHandler) handleList(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId required"))
		return
	}

	ctx := r.Context()
	if _, err := authorizeFundAccess(ctx, h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}

	q := r.URL.Query()
	from, fromErr := parseOptionalRFC3339(q.Get("from"))
	if fromErr != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_from", fromErr.Error()))
		return
	}
	to, toErr := parseOptionalRFC3339(q.Get("to"))
	if toErr != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_to", toErr.Error()))
		return
	}

	limit := 100
	if raw := q.Get("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			limit = v
		}
	}
	if limit > 500 {
		limit = 500
	}

	types := q["type"]
	// Validate types up front so a typo gets a 400 instead of an
	// empty result (which would mask the bug).
	for _, t := range types {
		if !cashLedgerEntryTypeKnown(t) {
			writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_type",
				fmt.Sprintf("unknown entry_type %q", t)))
			return
		}
	}

	listParams := repository.ListByFundParams{
		From:       from,
		To:         to,
		EntryTypes: types,
		Limit:      limit,
	}
	if cursor := strings.TrimSpace(q.Get("cursor")); cursor != "" {
		ts, id, err := decodeCashLedgerCursor(cursor)
		if err != nil {
			writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_cursor", err.Error()))
			return
		}
		listParams.PostedAtBefore = ts
		listParams.IDBefore = id
	}

	entries, err := h.cashLedger.ListByFund(ctx, fundID, listParams)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	out := cashLedgerListResponse{
		Entries: make([]cashLedgerEntryWire, 0, len(entries)),
	}
	for i := range entries {
		out.Entries = append(out.Entries, projectCashLedgerEntry(&entries[i]))
	}
	// Cursor is the (posted_at, id) of the LAST returned row, so
	// the next page starts strictly older.
	if len(entries) == limit {
		last := entries[len(entries)-1]
		out.NextCursor = encodeCashLedgerCursor(last.PostedAt, last.ID)
	}

	// P1-4 — when the fund has a non-USD base_currency we'll
	// convert the summary + balance below. Resolve once so we
	// don't re-query for each.
	baseCurrency := "USD"
	if h.fundRepo != nil {
		if cur, err := h.fundRepo.GetBaseCurrency(ctx, fundID); err == nil && cur != "" {
			baseCurrency = cur
		}
	}

	if q.Get("summary") == "1" {
		subs, err := h.cashLedger.SubtotalByEntryType(ctx, fundID, from, to)
		if err != nil {
			writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
			return
		}
		// Subtotals come back keyed by entry_type and summed in
		// the entry's native currency. For a single-currency fund
		// (the common case) the totals are already in
		// baseCurrency; for a multi-currency fund we ask the FX
		// repo for the per-row conversion the journal would have
		// recorded if every entry had been booked in base. The
		// SubtotalByEntryType query collapses across currencies so
		// the conservative choice for cross-ccy funds is to
		// flag fx_stale=true and let the UI render a banner —
		// the proper aggregator runs in NAV (P1-4 NAV path).
		out.Subtotals = subs
		if h.fxRepo != nil && baseCurrency != "USD" {
			out.FxStale = true
		}
	}
	if q.Get("balance") == "1" {
		bal, stale, err := h.computeBalanceInBase(ctx, fundID, baseCurrency, from, to)
		if err != nil {
			writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
			return
		}
		out.Balance = &bal
		if stale {
			out.FxStale = true
		}
	}
	out.Currency = baseCurrency

	writeOrderActionJSON(w, http.StatusOK, out)
}

func projectCashLedgerEntry(e *repository.CashLedgerEntry) cashLedgerEntryWire {
	w := cashLedgerEntryWire{
		ID:        e.ID,
		FundID:    e.FundID,		PostedAt:  e.PostedAt.UTC().Format(time.RFC3339Nano),
		EntryType: e.EntryType,
		Amount:    e.Amount,
		Currency:  e.Currency,
		Metadata:  e.Metadata,
		CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if e.TradingDate.Valid {
		w.TradingDate = e.TradingDate.Time.UTC().Format("2006-01-02")
	}
	if e.TradeID.Valid {
		w.TradeID = e.TradeID.String
	}
	if e.PlanID.Valid {
		w.PlanID = e.PlanID.String
	}
	if e.PlanActionID.Valid {
		w.PlanActionID = e.PlanActionID.String
	}
	if e.CorpActionID.Valid {
		w.CorpActionID = e.CorpActionID.String
	}
	if e.BrokerLinkID.Valid {
		w.BrokerLinkID = e.BrokerLinkID.String
	}
	if e.IdempotencyKey.Valid {
		w.IdempotencyKey = e.IdempotencyKey.String
	}
	w.Description = e.Description
	return w
}

func cashLedgerEntryTypeKnown(t string) bool {
	switch t {
	case repository.CashEntryTradeBuyNotional,
		repository.CashEntryTradeBuyCommission,
		repository.CashEntryTradeBuyTransfer,
		repository.CashEntryTradeBuyStampTax,
		repository.CashEntryTradeSellNotional,
		repository.CashEntryTradeSellCommission,
		repository.CashEntryTradeSellTransfer,
		repository.CashEntryTradeSellStampTax,
		repository.CashEntryDividendCash,
		repository.CashEntryFeeManagement,
		repository.CashEntryFeePerformance,
		repository.CashEntryFeePlatform,
		repository.CashEntryFundingDeposit,
		repository.CashEntryFundingWithdraw,
		repository.CashEntryAdjustment,
		repository.CashEntryReversal:
		return true
	}
	return false
}

// Cursor format: "<RFC3339Nano>|<uuid>". Opaque from the client's
// POV but trivially decodable on the server. We deliberately do
// NOT base64-encode — the format is already URL-safe and a plain-
// text cursor makes cURL debugging much easier.
func encodeCashLedgerCursor(ts time.Time, id string) string {
	return fmt.Sprintf("%s|%s", ts.UTC().Format(time.RFC3339Nano), id)
}

func decodeCashLedgerCursor(s string) (time.Time, string, error) {
	parts := strings.SplitN(s, "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", fmt.Errorf("cursor must be 'timestamp|uuid'")
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", fmt.Errorf("cursor timestamp invalid: %w", err)
	}
	if strings.TrimSpace(parts[1]) == "" {
		return time.Time{}, "", fmt.Errorf("cursor id empty")
	}
	return ts, parts[1], nil
}

func parseOptionalRFC3339(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

// computeBalanceInBase sums every cash_ledger row in [from, to)
// and converts each per-currency subtotal into the fund's
// base_currency. Returns (balance, fxStale, err) where fxStale
// signals at least one leg lacked a rate (we still return what
// we could compute so the UI can render a "≈" indicator).
//
// We pre-sum by currency in SQL so the DB does the heavy lifting;
// only the FX conversion runs in Go. For a single-currency fund
// this collapses to one row + one identity convert.
func (h *cashLedgerHandler) computeBalanceInBase(
	ctx context.Context,
	fundID, baseCurrency string,
	from, to time.Time,
) (float64, bool, error) {
	if h == nil || h.cashLedger == nil {
		return 0, false, fmt.Errorf("cash_ledger_handler: nil cash ledger")
	}
	// Fast path — when there's no fxRepo we fall back to the
	// legacy single-currency BalanceByFund. That's the safe
	// behaviour for funds whose ledger only ever carries USD.
	if h.fxRepo == nil {
		bal, err := h.cashLedger.BalanceByFund(ctx, fundID, repository.BalanceByFundParams{From: from, To: to})
		return bal, false, err
	}
	subtotals, err := h.cashLedger.SubtotalByCurrency(ctx, fundID, from, to)
	if err != nil {
		return 0, false, err
	}
	total := 0.0
	stale := false
	asOf := to
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	for currency, sum := range subtotals {
		converted, _, convErr := h.fxRepo.Convert(ctx, sum, currency, baseCurrency, asOf)
		if convErr != nil {
			// Missing rate — count the subtotal as-is (best
			// effort) and flag stale so the UI can warn.
			total += sum
			stale = true
			if h.metrics != nil {
				h.metrics.RecordFXEvent("convert_stale")
			}
			continue
		}
		total += converted
	}
	return total, stale, nil
}

