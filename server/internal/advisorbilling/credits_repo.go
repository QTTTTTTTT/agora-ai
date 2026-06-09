// credits_repo.go — Phase C-1 persistence layer for advisor credit
// packs.
//
// CreditsRepo wraps both:
//   * user_advisor_credits         the 1-row-per-user balance table
//   * advisor_credit_orders        the append-only purchase ledger
//
// The two are co-managed in a single repo because the webhook
// handler MUST update both transactionally (insert paid order
// row + upsert balance row + capture last_purchase_at). Splitting
// them into two repos would force the handler to share a tx
// across packages.
//
// Hot read path: Balance(userID) → 1-row PK lookup; called by
// Gate.Check on every consult. Worst case is "no row" → returns
// zero balance.
//
// Hot write path: ConsumeCredits(userID, kind) → conditional
// UPDATE that decrements the right balance column and refuses
// when balance is zero (so the Gate can fall back to the next
// quota tier). Single-shot SQL — no SELECT-then-UPDATE race.

package advisorbilling

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Balance is the per-user credit balance read shape.
type Balance struct {
	UserID              string
	DeepUnitsBalance    int
	QuickUnitsBalance   int
	TotalPurchasedCents int64
	LastPurchaseAt      sql.NullTime
	LastConsumptionAt   sql.NullTime
	UpdatedAt           time.Time
}

// Order is one row in advisor_credit_orders.
type Order struct {
	ID                    string
	UserID                string
	PackSKU               string
	DeepUnitsGranted      int
	QuickUnitsGranted     int
	PriceCentsUSD         int
	Currency              string
	Status                string
	LemonSqueezyOrderID   sql.NullString
	LemonSqueezyVariantID sql.NullString
	LemonSqueezyEventID   sql.NullString
	CheckoutURL           string
	PaidAt                sql.NullTime
	RefundedAt            sql.NullTime
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// CreditsRepo is the persistence façade.
type CreditsRepo struct {
	db *sql.DB
}

// NewCreditsRepo wires the repo with the supplied DB handle.
func NewCreditsRepo(db *sql.DB) *CreditsRepo {
	return &CreditsRepo{db: db}
}

// Balance returns the current balance for the user. Returns
// a zero-valued Balance + nil error when no row exists yet —
// matches the "user hasn't purchased anything" path which is
// the common case for Free-plan users.
func (r *CreditsRepo) Balance(ctx context.Context, userID string) (*Balance, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("advisorbilling: credits repo not configured")
	}
	b := &Balance{UserID: userID}
	err := r.db.QueryRowContext(ctx, `
		SELECT deep_units_balance, quick_units_balance, total_purchased_cents,
		       last_purchase_at, last_consumption_at, updated_at
		FROM user_advisor_credits
		WHERE user_id = $1
	`, userID).Scan(
		&b.DeepUnitsBalance, &b.QuickUnitsBalance, &b.TotalPurchasedCents,
		&b.LastPurchaseAt, &b.LastConsumptionAt, &b.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return b, nil
		}
		return nil, fmt.Errorf("advisorbilling: read balance: %w", err)
	}
	return b, nil
}

// CreatePendingOrder writes a 'pending' row into advisor_credit_orders
// at /checkout time. Returns the new order id which the handler
// passes through to LemonSqueezy as the merchant-side reference
// (so the webhook can correlate).
func (r *CreditsRepo) CreatePendingOrder(ctx context.Context, userID string, pack *CreditPack, checkoutURL string) (string, error) {
	if r == nil || r.db == nil {
		return "", errors.New("advisorbilling: credits repo not configured")
	}
	if pack == nil {
		return "", ErrUnknownPack
	}
	var id string
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO advisor_credit_orders
		    (user_id, pack_sku, deep_units_granted, quick_units_granted,
		     price_cents_usd, currency, status, checkout_url)
		VALUES ($1, $2, $3, $4, $5, 'USD', 'pending', $6)
		RETURNING id
	`, userID, pack.SKU, pack.DeepUnits, pack.QuickUnits, pack.PriceCentsUSD, strings.TrimSpace(checkoutURL)).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("advisorbilling: insert pending order: %w", err)
	}
	return id, nil
}

// MarkOrderPaid is the webhook handler's primary write — runs in
// a single transaction:
//
//   1. UPDATE the order row to status='paid' (if not already).
//      ON CONFLICT DO NOTHING via the unique index on
//      lemonsqueezy_event_id makes the second copy of the same
//      webhook a no-op.
//   2. UPSERT the balance row, adding the granted units.
//   3. Stamp last_purchase_at, increment total_purchased_cents.
//
// Returns (alreadyApplied, error). alreadyApplied=true when this
// webhook event was processed before (the unique-index conflict
// triggered) — the caller should still return 200 to LemonSqueezy
// so the webhook is acked.
func (r *CreditsRepo) MarkOrderPaid(ctx context.Context, orderID, lsOrderID, lsEventID string, paidAt time.Time, payload []byte) (bool, error) {
	if r == nil || r.db == nil {
		return false, errors.New("advisorbilling: credits repo not configured")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("advisorbilling: begin paid tx: %w", err)
	}
	defer tx.Rollback()

	// Idempotency check first — if the LS event id is already
	// recorded against an order, we've processed this exact webhook.
	if strings.TrimSpace(lsEventID) != "" {
		var existingID sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT id FROM advisor_credit_orders WHERE lemonsqueezy_event_id = $1
		`, lsEventID).Scan(&existingID)
		if err == nil && existingID.Valid {
			return true, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("advisorbilling: dedup probe: %w", err)
		}
	}

	// Stamp the order row + lookup which user + how many units to grant.
	var (
		userID      string
		deepUnits   int
		quickUnits  int
		priceCents  int
		alreadyPaid bool
	)
	payloadJSON := []byte("{}")
	if len(payload) > 0 {
		// Validate JSON before storing so a malformed payload
		// can't break the trigger.
		if !json.Valid(payload) {
			payloadJSON = []byte("{}")
		} else {
			payloadJSON = payload
		}
	}
	err = tx.QueryRowContext(ctx, `
		UPDATE advisor_credit_orders
		SET status = 'paid',
		    lemonsqueezy_order_id = COALESCE($2, lemonsqueezy_order_id),
		    lemonsqueezy_event_id = COALESCE($3, lemonsqueezy_event_id),
		    paid_at = $4,
		    raw_webhook_payload = $5::jsonb,
		    updated_at = NOW()
		WHERE id = $1
		RETURNING user_id, deep_units_granted, quick_units_granted, price_cents_usd, status = 'paid' AND paid_at IS NOT NULL
	`, orderID, nullableString(lsOrderID), nullableString(lsEventID), paidAt, string(payloadJSON)).Scan(
		&userID, &deepUnits, &quickUnits, &priceCents, &alreadyPaid,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("advisorbilling: order %q not found", orderID)
		}
		return false, fmt.Errorf("advisorbilling: mark order paid: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_advisor_credits
		    (user_id, deep_units_balance, quick_units_balance,
		     total_purchased_cents, last_purchase_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id) DO UPDATE SET
		    deep_units_balance    = user_advisor_credits.deep_units_balance + EXCLUDED.deep_units_balance,
		    quick_units_balance   = user_advisor_credits.quick_units_balance + EXCLUDED.quick_units_balance,
		    total_purchased_cents = user_advisor_credits.total_purchased_cents + EXCLUDED.total_purchased_cents,
		    last_purchase_at      = EXCLUDED.last_purchase_at,
		    updated_at            = NOW()
	`, userID, deepUnits, quickUnits, priceCents, paidAt); err != nil {
		return false, fmt.Errorf("advisorbilling: credit balance: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("advisorbilling: commit paid tx: %w", err)
	}
	return false, nil
}

// ConsumeCredits atomically decrements 1 unit from the user's
// balance for the given kind. Returns (success, error):
//   - success=true  → balance decremented, the gate should
//     record the consult as 'credit'-source.
//   - success=false, error=nil → balance is zero for this kind,
//     the gate should fall through to the next quota tier.
//   - success=false, error=non-nil → DB error.
//
// The conditional UPDATE makes this race-free: the WHERE clause
// only matches when balance > 0, and the row lock serialises
// concurrent consumes from the same user (rare).
func (r *CreditsRepo) ConsumeCredits(ctx context.Context, userID string, kind ConsultKind) (bool, error) {
	if r == nil || r.db == nil {
		return false, errors.New("advisorbilling: credits repo not configured")
	}
	column, err := columnForCredits(kind)
	if err != nil {
		return false, err
	}
	q := fmt.Sprintf(`
		UPDATE user_advisor_credits
		SET %s = %s - 1, last_consumption_at = NOW(), updated_at = NOW()
		WHERE user_id = $1 AND %s > 0
	`, column, column, column)
	res, err := r.db.ExecContext(ctx, q, userID)
	if err != nil {
		return false, fmt.Errorf("advisorbilling: consume credit: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("advisorbilling: rows affected: %w", err)
	}
	return n > 0, nil
}

// ListOrders returns the user's order history, newest first.
// Powers the /api/advisor/billing/orders endpoint.
func (r *CreditsRepo) ListOrders(ctx context.Context, userID string, limit int) ([]*Order, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("advisorbilling: credits repo not configured")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, user_id, pack_sku, deep_units_granted, quick_units_granted,
		       price_cents_usd, currency, status,
		       lemonsqueezy_order_id, lemonsqueezy_variant_id, lemonsqueezy_event_id,
		       checkout_url, paid_at, refunded_at, created_at, updated_at
		FROM advisor_credit_orders
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("advisorbilling: list orders: %w", err)
	}
	defer rows.Close()

	out := make([]*Order, 0, limit)
	for rows.Next() {
		o := &Order{}
		if err := rows.Scan(
			&o.ID, &o.UserID, &o.PackSKU, &o.DeepUnitsGranted, &o.QuickUnitsGranted,
			&o.PriceCentsUSD, &o.Currency, &o.Status,
			&o.LemonSqueezyOrderID, &o.LemonSqueezyVariantID, &o.LemonSqueezyEventID,
			&o.CheckoutURL, &o.PaidAt, &o.RefundedAt, &o.CreatedAt, &o.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("advisorbilling: scan order: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// MarkOrderRefunded transitions a paid order to 'refunded'. The
// balance is NOT auto-decremented because the user may have
// already spent the units; reconciliation is an ops decision
// surfaced in the admin panel. We just stamp the timestamp +
// status here.
func (r *CreditsRepo) MarkOrderRefunded(ctx context.Context, orderID, lsEventID string, refundedAt time.Time, payload []byte) error {
	if r == nil || r.db == nil {
		return errors.New("advisorbilling: credits repo not configured")
	}
	payloadJSON := []byte("{}")
	if len(payload) > 0 && json.Valid(payload) {
		payloadJSON = payload
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE advisor_credit_orders
		SET status = 'refunded',
		    refunded_at = $2,
		    lemonsqueezy_event_id = COALESCE(NULLIF($3, ''), lemonsqueezy_event_id),
		    raw_webhook_payload = $4::jsonb,
		    updated_at = NOW()
		WHERE id = $1 AND status = 'paid'
	`, orderID, refundedAt, lsEventID, string(payloadJSON))
	if err != nil {
		return fmt.Errorf("advisorbilling: mark order refunded: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("advisorbilling: order %q not in paid state", orderID)
	}
	return nil
}

// --- internals -------------------------------------------------------------

func columnForCredits(kind ConsultKind) (string, error) {
	switch kind {
	case KindDeep:
		return "deep_units_balance", nil
	case KindQuick:
		return "quick_units_balance", nil
	default:
		return "", fmt.Errorf("advisorbilling: unknown ConsultKind %q", kind)
	}
}

func nullableString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
