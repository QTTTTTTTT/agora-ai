package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type AgentMarketListing struct {
	ID                    string          `json:"id"`
	SellerUserID          string          `json:"seller_user_id"`
	SourceFundID          string          `json:"source_fund_id"`
	SourceAgentID         string          `json:"source_agent_id"`
	AgentName             string          `json:"agent_name"`
	AgentRole             string          `json:"agent_role"`
	AgentFocus            sql.NullString  `json:"agent_focus"`
	LatestLearningSummary sql.NullString  `json:"latest_learning_summary"`
	AskPriceMinor         int64           `json:"ask_price_minor"`
	Currency              string          `json:"currency"`
	Status                string          `json:"status"`
	SnapshotPayload       json.RawMessage `json:"snapshot_payload"`
	SoldToUserID          sql.NullString  `json:"sold_to_user_id"`
	SoldAt                sql.NullTime    `json:"sold_at"`
	CreatedAt             time.Time       `json:"created_at"`
	UpdatedAt             time.Time       `json:"updated_at"`
}

type AgentMarketBid struct {
	ID            string    `json:"id"`
	ListingID     string    `json:"listing_id"`
	BidderUserID  string    `json:"bidder_user_id"`
	BidPriceMinor int64     `json:"bid_price_minor"`
	Currency      string    `json:"currency"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type AgentMarketOrder struct {
	ID               string         `json:"id"`
	ListingID        string         `json:"listing_id"`
	SellerUserID     string         `json:"seller_user_id"`
	BuyerUserID      string         `json:"buyer_user_id"`
	BuyerFundID      sql.NullString `json:"buyer_fund_id"`
	SourceAgentID    string         `json:"source_agent_id"`
	DeliveredAgentID string         `json:"delivered_agent_id"`
	AmountMinor      int64          `json:"amount_minor"`
	Currency         string         `json:"currency"`
	Status           string         `json:"status"`
	CreatedAt        time.Time      `json:"created_at"`
}

type CreateAgentMarketListingParams struct {
	SellerUserID          string
	SourceFundID          string
	SourceAgentID         string
	AgentName             string
	AgentRole             string
	AgentFocus            string
	LatestLearningSummary string
	AskPriceMinor         int64
	Currency              string
	SnapshotPayload       json.RawMessage
}

type CreateAgentMarketBidParams struct {
	ListingID     string
	BidderUserID  string
	BidPriceMinor int64
	Currency      string
}

type CompleteAgentMarketOrderParams struct {
	ListingID        string
	SellerUserID     string
	BuyerUserID      string
	BuyerFundID      string
	SourceAgentID    string
	DeliveredAgentID string
	AmountMinor      int64
	Currency         string
	IdempotencyKey   string
}

// CreateAgentMarketOrderParams is used when reserving a `pending` order row
// inside the same transaction that performs the wallet transfer. The row is
// later promoted to `completed` via CompleteOrderWithTx once the agent has
// been cloned and bound to the buyer's fund.
type CreateAgentMarketOrderParams struct {
	ListingID      string
	SellerUserID   string
	BuyerUserID    string
	BuyerFundID    string
	SourceAgentID  string
	AmountMinor    int64
	Currency       string
	IdempotencyKey string
}

type MarketplaceRepo struct {
	db *sql.DB
}

func NewMarketplaceRepo(db *sql.DB) *MarketplaceRepo {
	return &MarketplaceRepo{db: db}
}

func (r *MarketplaceRepo) CreateListing(ctx context.Context, params CreateAgentMarketListingParams) (*AgentMarketListing, error) {
	if params.AskPriceMinor <= 0 {
		return nil, fmt.Errorf("marketplace_repo: ask price must be positive")
	}
	payload, err := marshalMarketplaceJSON(params.SnapshotPayload)
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: marshal snapshot payload: %w", err)
	}
	listing := &AgentMarketListing{}
	err = r.db.QueryRowContext(ctx, `
		INSERT INTO agent_market_listings (
			seller_user_id,
			source_fund_id,
			source_agent_id,
			agent_name,
			agent_role,
			agent_focus,
			latest_learning_summary,
			ask_price_minor,
			currency,
			snapshot_payload
		)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), $8, $9, $10)
		RETURNING id, seller_user_id, source_fund_id, source_agent_id, agent_name, agent_role, agent_focus,
		          latest_learning_summary, ask_price_minor, currency, status, snapshot_payload,
		          sold_to_user_id, sold_at, created_at, updated_at
	`, strings.TrimSpace(params.SellerUserID), strings.TrimSpace(params.SourceFundID), strings.TrimSpace(params.SourceAgentID), strings.TrimSpace(params.AgentName), strings.TrimSpace(params.AgentRole), strings.TrimSpace(params.AgentFocus), strings.TrimSpace(params.LatestLearningSummary), params.AskPriceMinor, normalizeMarketCurrency(params.Currency), payload).Scan(
		&listing.ID,
		&listing.SellerUserID,
		&listing.SourceFundID,
		&listing.SourceAgentID,
		&listing.AgentName,
		&listing.AgentRole,
		&listing.AgentFocus,
		&listing.LatestLearningSummary,
		&listing.AskPriceMinor,
		&listing.Currency,
		&listing.Status,
		&listing.SnapshotPayload,
		&listing.SoldToUserID,
		&listing.SoldAt,
		&listing.CreatedAt,
		&listing.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: create listing: %w", err)
	}
	return listing, nil
}

func (r *MarketplaceRepo) ListActiveListings(ctx context.Context, limit, offset int) ([]AgentMarketListing, error) {
	limit, offset = normalizeMarketplaceLimitOffset(limit, offset)
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, seller_user_id, source_fund_id, source_agent_id, agent_name, agent_role, agent_focus,
		       latest_learning_summary, ask_price_minor, currency, status, snapshot_payload,
		       sold_to_user_id, sold_at, created_at, updated_at
		FROM agent_market_listings
		WHERE status = 'active'
		ORDER BY created_at DESC, id DESC
		LIMIT $1 OFFSET $2
	`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: list active listings: %w", err)
	}
	defer rows.Close()
	return scanAgentMarketListings(rows)
}

func (r *MarketplaceRepo) ListListingsBySeller(ctx context.Context, sellerUserID string, limit, offset int) ([]AgentMarketListing, error) {
	limit, offset = normalizeMarketplaceLimitOffset(limit, offset)
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, seller_user_id, source_fund_id, source_agent_id, agent_name, agent_role, agent_focus,
		       latest_learning_summary, ask_price_minor, currency, status, snapshot_payload,
		       sold_to_user_id, sold_at, created_at, updated_at
		FROM agent_market_listings
		WHERE seller_user_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2 OFFSET $3
	`, strings.TrimSpace(sellerUserID), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: list listings by seller: %w", err)
	}
	defer rows.Close()
	return scanAgentMarketListings(rows)
}

func (r *MarketplaceRepo) GetListingByID(ctx context.Context, listingID string) (*AgentMarketListing, error) {
	listing := &AgentMarketListing{}
	err := r.db.QueryRowContext(ctx, `
		SELECT id, seller_user_id, source_fund_id, source_agent_id, agent_name, agent_role, agent_focus,
		       latest_learning_summary, ask_price_minor, currency, status, snapshot_payload,
		       sold_to_user_id, sold_at, created_at, updated_at
		FROM agent_market_listings
		WHERE id = $1
	`, strings.TrimSpace(listingID)).Scan(
		&listing.ID,
		&listing.SellerUserID,
		&listing.SourceFundID,
		&listing.SourceAgentID,
		&listing.AgentName,
		&listing.AgentRole,
		&listing.AgentFocus,
		&listing.LatestLearningSummary,
		&listing.AskPriceMinor,
		&listing.Currency,
		&listing.Status,
		&listing.SnapshotPayload,
		&listing.SoldToUserID,
		&listing.SoldAt,
		&listing.CreatedAt,
		&listing.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: get listing by id: %w", err)
	}
	return listing, nil
}

func (r *MarketplaceRepo) CancelListing(ctx context.Context, listingID, sellerUserID string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE agent_market_listings
		SET status = 'cancelled', updated_at = NOW()
		WHERE id = $1 AND seller_user_id = $2 AND status = 'active'
	`, strings.TrimSpace(listingID), strings.TrimSpace(sellerUserID))
	if err != nil {
		return fmt.Errorf("marketplace_repo: cancel listing: %w", err)
	}
	return checkRowsAffected(res, "marketplace_repo: cancel listing")
}

func (r *MarketplaceRepo) MarkListingSold(ctx context.Context, listingID, buyerUserID string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE agent_market_listings
		SET status = 'sold', sold_to_user_id = $2, sold_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status = 'active'
	`, strings.TrimSpace(listingID), strings.TrimSpace(buyerUserID))
	if err != nil {
		return fmt.Errorf("marketplace_repo: mark listing sold: %w", err)
	}
	return checkRowsAffected(res, "marketplace_repo: mark listing sold")
}

// LockListingForUpdate selects the listing row with FOR UPDATE so concurrent
// purchase attempts serialise on the same listing. Must be called inside a
// transaction (PR-02 single-tx purchase flow).
func (r *MarketplaceRepo) LockListingForUpdate(ctx context.Context, tx *sql.Tx, listingID string) (*AgentMarketListing, error) {
	if tx == nil {
		return nil, ErrNoTx
	}
	listing := &AgentMarketListing{}
	err := tx.QueryRowContext(ctx, `
		SELECT id, seller_user_id, source_fund_id, source_agent_id, agent_name, agent_role, agent_focus,
		       latest_learning_summary, ask_price_minor, currency, status, snapshot_payload,
		       sold_to_user_id, sold_at, created_at, updated_at
		FROM agent_market_listings
		WHERE id = $1
		FOR UPDATE
	`, strings.TrimSpace(listingID)).Scan(
		&listing.ID,
		&listing.SellerUserID,
		&listing.SourceFundID,
		&listing.SourceAgentID,
		&listing.AgentName,
		&listing.AgentRole,
		&listing.AgentFocus,
		&listing.LatestLearningSummary,
		&listing.AskPriceMinor,
		&listing.Currency,
		&listing.Status,
		&listing.SnapshotPayload,
		&listing.SoldToUserID,
		&listing.SoldAt,
		&listing.CreatedAt,
		&listing.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: lock listing for update: %w", err)
	}
	return listing, nil
}

// CreatePendingOrderWithTx reserves a `pending` order row keyed by
// idempotency_key. If a row with the same key already exists (because the
// caller retried), the existing row is returned together with a `false`
// "created" flag, allowing the outer flow to short-circuit.
func (r *MarketplaceRepo) CreatePendingOrderWithTx(ctx context.Context, tx *sql.Tx, params CreateAgentMarketOrderParams) (*AgentMarketOrder, bool, error) {
	if tx == nil {
		return nil, false, ErrNoTx
	}
	idempotencyKey := strings.TrimSpace(params.IdempotencyKey)
	if idempotencyKey == "" {
		return nil, false, fmt.Errorf("marketplace_repo: idempotency key is required")
	}
	order := &AgentMarketOrder{}
	err := tx.QueryRowContext(ctx, `
		INSERT INTO agent_market_orders (
			listing_id,
			seller_user_id,
			buyer_user_id,
			buyer_fund_id,
			source_agent_id,
			delivered_agent_id,
			amount_minor,
			currency,
			status,
			idempotency_key
		)
		VALUES ($1, $2, $3, $4, $5, NULL, $6, $7, 'pending', $8)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id, listing_id, seller_user_id, buyer_user_id, buyer_fund_id, source_agent_id, delivered_agent_id,
		          amount_minor, currency, status, created_at
	`, strings.TrimSpace(params.ListingID), strings.TrimSpace(params.SellerUserID), strings.TrimSpace(params.BuyerUserID), sql.NullString{String: strings.TrimSpace(params.BuyerFundID), Valid: strings.TrimSpace(params.BuyerFundID) != ""}, strings.TrimSpace(params.SourceAgentID), params.AmountMinor, normalizeMarketCurrency(params.Currency), idempotencyKey).Scan(
		&order.ID,
		&order.ListingID,
		&order.SellerUserID,
		&order.BuyerUserID,
		&order.BuyerFundID,
		&order.SourceAgentID,
		&order.DeliveredAgentID,
		&order.AmountMinor,
		&order.Currency,
		&order.Status,
		&order.CreatedAt,
	)
	if err == nil {
		return order, true, nil
	}
	if err != sql.ErrNoRows {
		return nil, false, fmt.Errorf("marketplace_repo: create pending order: %w", err)
	}

	// ON CONFLICT triggered — fetch the existing row so the caller can
	// inspect it and decide whether to short-circuit.
	existing, getErr := getOrderByIdempotencyKeyTx(ctx, tx, idempotencyKey)
	if getErr != nil {
		return nil, false, getErr
	}
	return existing, false, nil
}

// CompleteOrderWithTx promotes a `pending` order row to `completed` and
// stamps the delivered agent id. Must run inside the same transaction that
// inserted the pending row.
func (r *MarketplaceRepo) CompleteOrderWithTx(ctx context.Context, tx *sql.Tx, orderID, deliveredAgentID string) (*AgentMarketOrder, error) {
	if tx == nil {
		return nil, ErrNoTx
	}
	order := &AgentMarketOrder{}
	err := tx.QueryRowContext(ctx, `
		UPDATE agent_market_orders
		SET status = 'completed',
		    delivered_agent_id = $2,
		    failure_reason = NULL,
		    updated_at = NOW()
		WHERE id = $1 AND status = 'pending'
		RETURNING id, listing_id, seller_user_id, buyer_user_id, buyer_fund_id, source_agent_id, delivered_agent_id,
		          amount_minor, currency, status, created_at
	`, strings.TrimSpace(orderID), strings.TrimSpace(deliveredAgentID)).Scan(
		&order.ID,
		&order.ListingID,
		&order.SellerUserID,
		&order.BuyerUserID,
		&order.BuyerFundID,
		&order.SourceAgentID,
		&order.DeliveredAgentID,
		&order.AmountMinor,
		&order.Currency,
		&order.Status,
		&order.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: complete order: %w", err)
	}
	return order, nil
}

// MarkListingSoldWithTx is the in-transaction variant of MarkListingSold.
func (r *MarketplaceRepo) MarkListingSoldWithTx(ctx context.Context, tx *sql.Tx, listingID, buyerUserID string) error {
	if tx == nil {
		return ErrNoTx
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE agent_market_listings
		SET status = 'sold', sold_to_user_id = $2, sold_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status = 'active'
	`, strings.TrimSpace(listingID), strings.TrimSpace(buyerUserID))
	if err != nil {
		return fmt.Errorf("marketplace_repo: mark listing sold: %w", err)
	}
	return checkRowsAffected(res, "marketplace_repo: mark listing sold")
}

// MarkOrderFailedWithTx flips an in-flight order to `failed` with a reason
// for audit. Used by the reconcile cron when wallet movement diverges from
// order state.
func (r *MarketplaceRepo) MarkOrderFailedWithTx(ctx context.Context, tx *sql.Tx, orderID, reason string) error {
	if tx == nil {
		return ErrNoTx
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE agent_market_orders
		SET status = 'failed',
		    failure_reason = NULLIF($2, ''),
		    reconciled_at = NOW(),
		    updated_at = NOW()
		WHERE id = $1 AND status IN ('pending', 'failed')
	`, strings.TrimSpace(orderID), strings.TrimSpace(reason))
	if err != nil {
		return fmt.Errorf("marketplace_repo: mark order failed: %w", err)
	}
	return checkRowsAffected(res, "marketplace_repo: mark order failed")
}

// ListPendingOrdersOlderThan returns orders that have been stuck in
// `pending` longer than `cutoff`. The reconcile cron consumes this list to
// detect orphaned orders (transfer never happened, or commit dropped).
func (r *MarketplaceRepo) ListPendingOrdersOlderThan(ctx context.Context, cutoff time.Time, limit int) ([]AgentMarketOrder, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, listing_id, seller_user_id, buyer_user_id, buyer_fund_id, source_agent_id, delivered_agent_id,
		       amount_minor, currency, status, created_at
		FROM agent_market_orders
		WHERE status = 'pending' AND created_at < $1
		ORDER BY created_at ASC
		LIMIT $2
	`, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: list pending orders: %w", err)
	}
	defer rows.Close()

	orders := make([]AgentMarketOrder, 0)
	for rows.Next() {
		var order AgentMarketOrder
		if err := rows.Scan(
			&order.ID,
			&order.ListingID,
			&order.SellerUserID,
			&order.BuyerUserID,
			&order.BuyerFundID,
			&order.SourceAgentID,
			&order.DeliveredAgentID,
			&order.AmountMinor,
			&order.Currency,
			&order.Status,
			&order.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("marketplace_repo: scan pending order: %w", err)
		}
		orders = append(orders, order)
	}
	return orders, rows.Err()
}

// RecordReconcileFinding appends a row to the marketplace_reconcile_log
// audit table. Resolved=false marks issues that still need attention.
func (r *MarketplaceRepo) RecordReconcileFinding(ctx context.Context, orderID, listingID, finding string, detail json.RawMessage, resolved bool) error {
	payload, err := marshalMarketplaceJSON(detail)
	if err != nil {
		return fmt.Errorf("marketplace_repo: marshal reconcile detail: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO marketplace_reconcile_log (order_id, listing_id, finding, detail, resolved)
		VALUES (NULLIF($1, '')::uuid, NULLIF($2, '')::uuid, $3, $4, $5)
	`, strings.TrimSpace(orderID), strings.TrimSpace(listingID), strings.TrimSpace(finding), payload, resolved)
	if err != nil {
		return fmt.Errorf("marketplace_repo: record reconcile finding: %w", err)
	}
	return nil
}

func getOrderByIdempotencyKeyTx(ctx context.Context, tx *sql.Tx, key string) (*AgentMarketOrder, error) {
	order := &AgentMarketOrder{}
	err := tx.QueryRowContext(ctx, `
		SELECT id, listing_id, seller_user_id, buyer_user_id, buyer_fund_id, source_agent_id, delivered_agent_id,
		       amount_minor, currency, status, created_at
		FROM agent_market_orders
		WHERE idempotency_key = $1
	`, key).Scan(
		&order.ID,
		&order.ListingID,
		&order.SellerUserID,
		&order.BuyerUserID,
		&order.BuyerFundID,
		&order.SourceAgentID,
		&order.DeliveredAgentID,
		&order.AmountMinor,
		&order.Currency,
		&order.Status,
		&order.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: get order by idempotency: %w", err)
	}
	return order, nil
}

func (r *MarketplaceRepo) CreateBid(ctx context.Context, params CreateAgentMarketBidParams) (*AgentMarketBid, error) {
	if params.BidPriceMinor <= 0 {
		return nil, fmt.Errorf("marketplace_repo: bid price must be positive")
	}
	bid := &AgentMarketBid{}
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO agent_market_bids (listing_id, bidder_user_id, bid_price_minor, currency)
		VALUES ($1, $2, $3, $4)
		RETURNING id, listing_id, bidder_user_id, bid_price_minor, currency, status, created_at, updated_at
	`, strings.TrimSpace(params.ListingID), strings.TrimSpace(params.BidderUserID), params.BidPriceMinor, normalizeMarketCurrency(params.Currency)).Scan(
		&bid.ID,
		&bid.ListingID,
		&bid.BidderUserID,
		&bid.BidPriceMinor,
		&bid.Currency,
		&bid.Status,
		&bid.CreatedAt,
		&bid.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: create bid: %w", err)
	}
	return bid, nil
}

func (r *MarketplaceRepo) ListBidsByListing(ctx context.Context, listingID string, limit, offset int) ([]AgentMarketBid, error) {
	limit, offset = normalizeMarketplaceLimitOffset(limit, offset)
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, listing_id, bidder_user_id, bid_price_minor, currency, status, created_at, updated_at
		FROM agent_market_bids
		WHERE listing_id = $1
		ORDER BY bid_price_minor DESC, created_at ASC
		LIMIT $2 OFFSET $3
	`, strings.TrimSpace(listingID), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: list bids by listing: %w", err)
	}
	defer rows.Close()
	bids := make([]AgentMarketBid, 0)
	for rows.Next() {
		var bid AgentMarketBid
		if err := rows.Scan(&bid.ID, &bid.ListingID, &bid.BidderUserID, &bid.BidPriceMinor, &bid.Currency, &bid.Status, &bid.CreatedAt, &bid.UpdatedAt); err != nil {
			return nil, fmt.Errorf("marketplace_repo: scan bid: %w", err)
		}
		bids = append(bids, bid)
	}
	return bids, rows.Err()
}

func normalizeMarketplaceLimitOffset(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func (r *MarketplaceRepo) CompleteOrder(ctx context.Context, params CompleteAgentMarketOrderParams) (*AgentMarketOrder, error) {
	order := &AgentMarketOrder{}
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO agent_market_orders (
			listing_id,
			seller_user_id,
			buyer_user_id,
			buyer_fund_id,
			source_agent_id,
			delivered_agent_id,
			amount_minor,
			currency,
			status
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'completed')
		RETURNING id, listing_id, seller_user_id, buyer_user_id, buyer_fund_id, source_agent_id, delivered_agent_id,
		          amount_minor, currency, status, created_at
	`, strings.TrimSpace(params.ListingID), strings.TrimSpace(params.SellerUserID), strings.TrimSpace(params.BuyerUserID), sql.NullString{String: strings.TrimSpace(params.BuyerFundID), Valid: strings.TrimSpace(params.BuyerFundID) != ""}, strings.TrimSpace(params.SourceAgentID), strings.TrimSpace(params.DeliveredAgentID), params.AmountMinor, normalizeMarketCurrency(params.Currency)).Scan(
		&order.ID,
		&order.ListingID,
		&order.SellerUserID,
		&order.BuyerUserID,
		&order.BuyerFundID,
		&order.SourceAgentID,
		&order.DeliveredAgentID,
		&order.AmountMinor,
		&order.Currency,
		&order.Status,
		&order.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: complete order: %w", err)
	}
	return order, nil
}

func marshalMarketplaceJSON(raw json.RawMessage) ([]byte, error) {
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

func normalizeMarketCurrency(value string) string {
	trimmed := strings.ToUpper(strings.TrimSpace(value))
	if trimmed == "" {
		return "USD"
	}
	return trimmed
}

func scanAgentMarketListings(rows *sql.Rows) ([]AgentMarketListing, error) {
	listings := make([]AgentMarketListing, 0)
	for rows.Next() {
		var listing AgentMarketListing
		if err := rows.Scan(
			&listing.ID,
			&listing.SellerUserID,
			&listing.SourceFundID,
			&listing.SourceAgentID,
			&listing.AgentName,
			&listing.AgentRole,
			&listing.AgentFocus,
			&listing.LatestLearningSummary,
			&listing.AskPriceMinor,
			&listing.Currency,
			&listing.Status,
			&listing.SnapshotPayload,
			&listing.SoldToUserID,
			&listing.SoldAt,
			&listing.CreatedAt,
			&listing.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("marketplace_repo: scan listing: %w", err)
		}
		listings = append(listings, listing)
	}
	return listings, rows.Err()
}
