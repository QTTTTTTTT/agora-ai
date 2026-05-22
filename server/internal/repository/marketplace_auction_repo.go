package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Auction-specific accessors live in their own file so the buyout-only
// MarketplaceRepo methods stay byte-for-byte unchanged. The two surfaces
// share the underlying `agent_market_listings` and `agent_market_bids`
// tables but expose different field sets — buyout callers never need to
// know about end_times or anti-snipe windows, and vice-versa.

// AgentMarketAuctionListing is the auction view of a listing row. The
// embedded AgentMarketListing carries the columns that are also relevant
// to buyout/subscribe (id, seller, snapshot, etc.); the additional fields
// capture the auction lifecycle metadata added in migration 024.
type AgentMarketAuctionListing struct {
	AgentMarketListing

	Mode                       string
	AuctionStartedAt           sql.NullTime
	AuctionEndsAt              sql.NullTime
	AuctionReserveMinor        sql.NullInt64
	AuctionMinIncrementMinor   int64
	AuctionAntiSnipeSeconds    int
	AuctionCurrentBidMinor     sql.NullInt64
	AuctionCurrentBidderUserID sql.NullString
	AuctionCurrentBidID        sql.NullString
	AuctionSettledAt           sql.NullTime
	AuctionWinningBidID        sql.NullString
}

// AgentMarketAuctionBid is the bid representation for auctions. The
// embedded AgentMarketBid carries the buyout-shared fields and HoldID
// links the bid to the wallet hold backing it. HoldID is nullable so
// legacy buyout bids (pre-024) still scan cleanly.
type AgentMarketAuctionBid struct {
	AgentMarketBid

	HoldID sql.NullString
}

// CreateAgentMarketAuctionParams is the on-the-wire shape for opening a
// new auction listing. It is a superset of CreateAgentMarketListingParams
// — the listing portion is reused so all auction listings carry the same
// seller / snapshot semantics as buyouts, with the auction columns
// layered on top.
type CreateAgentMarketAuctionParams struct {
	CreateAgentMarketListingParams

	StartingPriceMinor      int64
	StartsAt                time.Time
	EndsAt                  time.Time
	ReserveMinor            int64
	MinIncrementMinor       int64
	AntiSnipeSeconds        int
}

type CreateAuctionBidWithTxParams struct {
	ListingID      string
	BidderUserID   string
	BidPriceMinor  int64
	Currency       string
	HoldID         string
}

// CreateAuctionListing inserts a listing row with mode='auction' and the
// auction columns populated. The buyout-only CreateListing path leaves
// these columns NULL/default; we keep them in a separate INSERT statement
// so the auction CHECK constraint added in migration 024 is satisfied.
func (r *MarketplaceRepo) CreateAuctionListing(ctx context.Context, params CreateAgentMarketAuctionParams) (*AgentMarketAuctionListing, error) {
	if params.StartingPriceMinor <= 0 {
		return nil, fmt.Errorf("marketplace_repo: starting price must be positive")
	}
	if !params.EndsAt.After(params.StartsAt) {
		return nil, fmt.Errorf("marketplace_repo: auction ends_at must be after starts_at")
	}
	payload, err := marshalMarketplaceJSON(params.SnapshotPayload)
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: marshal snapshot payload: %w", err)
	}

	minIncrement := params.MinIncrementMinor
	if minIncrement <= 0 {
		minIncrement = 1
	}
	antiSnipe := params.AntiSnipeSeconds
	if antiSnipe < 0 {
		antiSnipe = 0
	}
	reserve := sql.NullInt64{}
	if params.ReserveMinor > 0 {
		reserve = sql.NullInt64{Int64: params.ReserveMinor, Valid: true}
	}

	listing := &AgentMarketAuctionListing{}
	err = r.db.QueryRowContext(ctx, `
		INSERT INTO agent_market_listings (
			seller_user_id, source_fund_id, source_agent_id,
			agent_name, agent_role, agent_focus, latest_learning_summary,
			ask_price_minor, currency, snapshot_payload,
			mode, auction_started_at, auction_ends_at, auction_reserve_minor,
			auction_min_increment_minor, auction_anti_snipe_seconds
		)
		VALUES (
			$1, $2, $3,
			$4, $5, NULLIF($6, ''), NULLIF($7, ''),
			$8, $9, $10,
			'auction', $11, $12, $13,
			$14, $15
		)
		RETURNING id, seller_user_id, source_fund_id, source_agent_id, agent_name, agent_role, agent_focus,
		          latest_learning_summary, ask_price_minor, currency, status, snapshot_payload,
		          sold_to_user_id, sold_at, created_at, updated_at,
		          mode, auction_started_at, auction_ends_at, auction_reserve_minor,
		          auction_min_increment_minor, auction_anti_snipe_seconds,
		          auction_current_bid_minor, auction_current_bidder_user_id, auction_current_bid_id,
		          auction_settled_at, auction_winning_bid_id
	`, strings.TrimSpace(params.SellerUserID), strings.TrimSpace(params.SourceFundID), strings.TrimSpace(params.SourceAgentID),
		strings.TrimSpace(params.AgentName), strings.TrimSpace(params.AgentRole), strings.TrimSpace(params.AgentFocus), strings.TrimSpace(params.LatestLearningSummary),
		params.StartingPriceMinor, normalizeMarketCurrency(params.Currency), payload,
		params.StartsAt, params.EndsAt, reserve,
		minIncrement, antiSnipe).Scan(scanAuctionListingArgs(listing)...)
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: create auction listing: %w", err)
	}
	return listing, nil
}

// GetAuctionByID loads an auction listing without locking. Returns
// ErrNotFound if the row exists but is not an auction (callers should
// route to the buyout path in that case).
func (r *MarketplaceRepo) GetAuctionByID(ctx context.Context, listingID string) (*AgentMarketAuctionListing, error) {
	listing := &AgentMarketAuctionListing{}
	err := r.db.QueryRowContext(ctx, auctionListingSelectColumns+`
		FROM agent_market_listings
		WHERE id = $1
	`, strings.TrimSpace(listingID)).Scan(scanAuctionListingArgs(listing)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: get auction by id: %w", err)
	}
	if listing.Mode != "auction" {
		return nil, ErrNotFound
	}
	return listing, nil
}

// LockAuctionForUpdate locks the auction row inside the caller's tx so
// concurrent bidders / settlement workers serialise on the same listing.
func (r *MarketplaceRepo) LockAuctionForUpdate(ctx context.Context, tx *sql.Tx, listingID string) (*AgentMarketAuctionListing, error) {
	if tx == nil {
		return nil, ErrNoTx
	}
	listing := &AgentMarketAuctionListing{}
	err := tx.QueryRowContext(ctx, auctionListingSelectColumns+`
		FROM agent_market_listings
		WHERE id = $1
		FOR UPDATE
	`, strings.TrimSpace(listingID)).Scan(scanAuctionListingArgs(listing)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: lock auction for update: %w", err)
	}
	if listing.Mode != "auction" {
		return nil, ErrNotFound
	}
	return listing, nil
}

// CreateAuctionBidWithTx records a bid alongside its backing wallet hold.
// Returns the inserted row so the caller can use the bid id when updating
// the listing's `auction_current_bid_id`.
func (r *MarketplaceRepo) CreateAuctionBidWithTx(ctx context.Context, tx *sql.Tx, params CreateAuctionBidWithTxParams) (*AgentMarketAuctionBid, error) {
	if tx == nil {
		return nil, ErrNoTx
	}
	if params.BidPriceMinor <= 0 {
		return nil, fmt.Errorf("marketplace_repo: bid price must be positive")
	}
	bid := &AgentMarketAuctionBid{}
	err := tx.QueryRowContext(ctx, `
		INSERT INTO agent_market_bids (listing_id, bidder_user_id, bid_price_minor, currency, status, hold_id)
		VALUES ($1, $2, $3, $4, 'active', NULLIF($5, '')::uuid)
		RETURNING id, listing_id, bidder_user_id, bid_price_minor, currency, status, created_at, updated_at, hold_id
	`, strings.TrimSpace(params.ListingID), strings.TrimSpace(params.BidderUserID), params.BidPriceMinor,
		normalizeMarketCurrency(params.Currency), strings.TrimSpace(params.HoldID)).Scan(
		&bid.ID, &bid.ListingID, &bid.BidderUserID, &bid.BidPriceMinor, &bid.Currency, &bid.Status, &bid.CreatedAt, &bid.UpdatedAt, &bid.HoldID,
	)
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: create auction bid: %w", err)
	}
	return bid, nil
}

// MarkBidStatusWithTx flips a bid to outbid / refunded / won inside the
// caller's tx. Returns ErrNotFound if the bid id does not exist.
func (r *MarketplaceRepo) MarkBidStatusWithTx(ctx context.Context, tx *sql.Tx, bidID, newStatus string) error {
	if tx == nil {
		return ErrNoTx
	}
	switch newStatus {
	case "active", "outbid", "won", "refunded", "rejected", "retracted":
	default:
		return fmt.Errorf("marketplace_repo: invalid bid status %q", newStatus)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE agent_market_bids
		   SET status = $2, updated_at = NOW()
		 WHERE id = $1
	`, strings.TrimSpace(bidID), newStatus)
	if err != nil {
		return fmt.Errorf("marketplace_repo: mark bid status: %w", err)
	}
	return checkRowsAffected(res, "marketplace_repo: mark bid status")
}

// GetAuctionBidByID returns a single bid row (no locking). Used to
// resolve a hold id when refunding the prior top bidder.
func (r *MarketplaceRepo) GetAuctionBidByID(ctx context.Context, bidID string) (*AgentMarketAuctionBid, error) {
	bid := &AgentMarketAuctionBid{}
	err := r.db.QueryRowContext(ctx, `
		SELECT id, listing_id, bidder_user_id, bid_price_minor, currency, status, created_at, updated_at, hold_id
		  FROM agent_market_bids
		 WHERE id = $1
	`, strings.TrimSpace(bidID)).Scan(
		&bid.ID, &bid.ListingID, &bid.BidderUserID, &bid.BidPriceMinor, &bid.Currency, &bid.Status, &bid.CreatedAt, &bid.UpdatedAt, &bid.HoldID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: get auction bid by id: %w", err)
	}
	return bid, nil
}

// GetAuctionBidByIDWithTx mirrors GetAuctionBidByID but inside a tx with
// no row lock — used by the settlement path to pull the winning bid row
// after locking the listing.
func (r *MarketplaceRepo) GetAuctionBidByIDWithTx(ctx context.Context, tx *sql.Tx, bidID string) (*AgentMarketAuctionBid, error) {
	if tx == nil {
		return nil, ErrNoTx
	}
	bid := &AgentMarketAuctionBid{}
	err := tx.QueryRowContext(ctx, `
		SELECT id, listing_id, bidder_user_id, bid_price_minor, currency, status, created_at, updated_at, hold_id
		  FROM agent_market_bids
		 WHERE id = $1
	`, strings.TrimSpace(bidID)).Scan(
		&bid.ID, &bid.ListingID, &bid.BidderUserID, &bid.BidPriceMinor, &bid.Currency, &bid.Status, &bid.CreatedAt, &bid.UpdatedAt, &bid.HoldID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: get auction bid by id (tx): %w", err)
	}
	return bid, nil
}

// ListActiveBidsForAuctionWithTx returns all currently-active bids for an
// auction. The settlement worker uses it to find every bidder whose hold
// still needs to be released when the auction closes with no winner.
func (r *MarketplaceRepo) ListActiveBidsForAuctionWithTx(ctx context.Context, tx *sql.Tx, listingID string) ([]AgentMarketAuctionBid, error) {
	if tx == nil {
		return nil, ErrNoTx
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, listing_id, bidder_user_id, bid_price_minor, currency, status, created_at, updated_at, hold_id
		  FROM agent_market_bids
		 WHERE listing_id = $1 AND status = 'active'
	`, strings.TrimSpace(listingID))
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: list active bids: %w", err)
	}
	defer rows.Close()
	out := make([]AgentMarketAuctionBid, 0)
	for rows.Next() {
		var b AgentMarketAuctionBid
		if err := rows.Scan(&b.ID, &b.ListingID, &b.BidderUserID, &b.BidPriceMinor, &b.Currency, &b.Status, &b.CreatedAt, &b.UpdatedAt, &b.HoldID); err != nil {
			return nil, fmt.Errorf("marketplace_repo: scan active bid: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// UpdateAuctionTopBidWithTx records the new winning bid on the listing
// and, if anti-sniping extended the close time, pushes auction_ends_at
// forward. Callers MUST hold the FOR UPDATE lock on the listing first.
func (r *MarketplaceRepo) UpdateAuctionTopBidWithTx(ctx context.Context, tx *sql.Tx, listingID string, bidID, bidderUserID string, bidMinor int64, newEndsAt time.Time) error {
	if tx == nil {
		return ErrNoTx
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE agent_market_listings
		   SET auction_current_bid_minor = $2,
		       auction_current_bidder_user_id = $3,
		       auction_current_bid_id = $4,
		       auction_ends_at = $5,
		       updated_at = NOW()
		 WHERE id = $1 AND mode = 'auction'
	`, strings.TrimSpace(listingID), bidMinor, strings.TrimSpace(bidderUserID), strings.TrimSpace(bidID), newEndsAt)
	if err != nil {
		return fmt.Errorf("marketplace_repo: update auction top bid: %w", err)
	}
	return checkRowsAffected(res, "marketplace_repo: update auction top bid")
}

// MarkAuctionSettledWithTx is the "sold" terminal transition: status
// flips to 'sold', sold_to_user_id is stamped with the winner, and the
// auction_settled_at / auction_winning_bid_id columns are populated.
func (r *MarketplaceRepo) MarkAuctionSettledWithTx(ctx context.Context, tx *sql.Tx, listingID, winnerUserID, winningBidID string) error {
	if tx == nil {
		return ErrNoTx
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE agent_market_listings
		   SET status = 'sold',
		       sold_to_user_id = $2,
		       sold_at = NOW(),
		       auction_settled_at = NOW(),
		       auction_winning_bid_id = $3,
		       updated_at = NOW()
		 WHERE id = $1 AND mode = 'auction' AND status = 'active'
	`, strings.TrimSpace(listingID), strings.TrimSpace(winnerUserID), strings.TrimSpace(winningBidID))
	if err != nil {
		return fmt.Errorf("marketplace_repo: mark auction settled: %w", err)
	}
	return checkRowsAffected(res, "marketplace_repo: mark auction settled")
}

// MarkAuctionClosedUnsoldWithTx is the "reserve not met" / "no bids"
// terminal transition: status flips to 'cancelled' and auction_settled_at
// is stamped so the cron does not re-process it.
func (r *MarketplaceRepo) MarkAuctionClosedUnsoldWithTx(ctx context.Context, tx *sql.Tx, listingID string) error {
	if tx == nil {
		return ErrNoTx
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE agent_market_listings
		   SET status = 'cancelled',
		       auction_settled_at = NOW(),
		       updated_at = NOW()
		 WHERE id = $1 AND mode = 'auction' AND status = 'active'
	`, strings.TrimSpace(listingID))
	if err != nil {
		return fmt.Errorf("marketplace_repo: mark auction unsold: %w", err)
	}
	return checkRowsAffected(res, "marketplace_repo: mark auction unsold")
}

// ListExpiredActiveAuctions returns auction listings whose end time is in
// the past but still carry status='active'. The settlement worker calls
// this and then settles each one inside its own transaction.
func (r *MarketplaceRepo) ListExpiredActiveAuctions(ctx context.Context, now time.Time, limit int) ([]AgentMarketAuctionListing, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, auctionListingSelectColumns+`
		FROM agent_market_listings
		WHERE mode = 'auction'
		  AND status = 'active'
		  AND auction_ends_at IS NOT NULL
		  AND auction_ends_at <= $1
		ORDER BY auction_ends_at ASC
		LIMIT $2
	`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("marketplace_repo: list expired auctions: %w", err)
	}
	defer rows.Close()
	out := make([]AgentMarketAuctionListing, 0)
	for rows.Next() {
		listing := AgentMarketAuctionListing{}
		if err := rows.Scan(scanAuctionListingArgs(&listing)...); err != nil {
			return nil, fmt.Errorf("marketplace_repo: scan expired auction: %w", err)
		}
		out = append(out, listing)
	}
	return out, rows.Err()
}

const auctionListingSelectColumns = `
SELECT id, seller_user_id, source_fund_id, source_agent_id, agent_name, agent_role, agent_focus,
       latest_learning_summary, ask_price_minor, currency, status, snapshot_payload,
       sold_to_user_id, sold_at, created_at, updated_at,
       mode, auction_started_at, auction_ends_at, auction_reserve_minor,
       auction_min_increment_minor, auction_anti_snipe_seconds,
       auction_current_bid_minor, auction_current_bidder_user_id, auction_current_bid_id,
       auction_settled_at, auction_winning_bid_id`

// scanAuctionListingArgs centralises the column-to-field binding for the
// auction listing query so additions in the future require updating a
// single function rather than every Scan call site.
func scanAuctionListingArgs(l *AgentMarketAuctionListing) []any {
	return []any{
		&l.ID, &l.SellerUserID, &l.SourceFundID, &l.SourceAgentID,
		&l.AgentName, &l.AgentRole, &l.AgentFocus, &l.LatestLearningSummary,
		&l.AskPriceMinor, &l.Currency, &l.Status, &l.SnapshotPayload,
		&l.SoldToUserID, &l.SoldAt, &l.CreatedAt, &l.UpdatedAt,
		&l.Mode, &l.AuctionStartedAt, &l.AuctionEndsAt, &l.AuctionReserveMinor,
		&l.AuctionMinIncrementMinor, &l.AuctionAntiSnipeSeconds,
		&l.AuctionCurrentBidMinor, &l.AuctionCurrentBidderUserID, &l.AuctionCurrentBidID,
		&l.AuctionSettledAt, &l.AuctionWinningBidID,
	}
}

// MarshalAuctionMetadataJSON helps callers stash an auction snapshot in
// metadata-typed columns without re-importing encoding/json everywhere.
func MarshalAuctionMetadataJSON(payload any) (json.RawMessage, error) {
	if payload == nil {
		return json.RawMessage(`{}`), nil
	}
	return json.Marshal(payload)
}
